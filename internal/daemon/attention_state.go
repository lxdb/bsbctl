package daemon

import (
	"cmp"
	"slices"
	"time"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/presentation"
)

// restoreAttentionState restores only bounded scheduling state for current
// app generations. Corrupt or incompatible state degrades diagnostics without
// preventing daemon startup.
func (e *Engine) restoreAttentionState(stateStore attentionStatePersistence, generation observation.Generation) {
	e.stepMu.Lock()
	defer e.stepMu.Unlock()
	e.stateStore = stateStore
	e.stateGeneration = generation
	if stateStore == nil {
		return
	}
	document, err := stateStore.Load()
	if err != nil {
		return
	}
	now := e.now().UTC()
	for _, entry := range document.LastShown {
		if !e.restoreStateIdentity(entry.Identity, entry.ShownAt, now) {
			continue
		}
		id := stateIdentityID(entry.Identity)
		e.history.LastShown[id] = entry.ShownAt.UTC()
		e.lastShownState[id] = lastShownMetadata{identity: entry.Identity, persistent: true}
		e.stateStats.RestoredEntries++
	}
	for _, entry := range document.Acknowledgements {
		if entry.ObservedAt.After(now) {
			e.stateStats.DiscardedEntries++
			continue
		}
		if !e.restoreStateIdentity(entry.Identity, entry.TouchedAt, now) {
			continue
		}
		id := stateIdentityID(entry.Identity)
		e.acknowledged[id] = ackState{
			identity: entry.Identity, generation: entry.Identity.Generation,
			observedAt: entry.ObservedAt.UTC(), touchedAt: entry.TouchedAt.UTC(),
		}
		e.stateStats.RestoredEntries++
	}
	if e.stateStats.DiscardedEntries != 0 || e.stateStats.PrunedEntries != 0 {
		_ = e.persistState(e.store.Snapshot(), e.acknowledged)
	}
}

func (e *Engine) restoreStateIdentity(identity attention.StateIdentity, at, now time.Time) bool {
	if at.After(now) {
		e.stateStats.DiscardedEntries++
		return false
	}
	if e.stateTTL <= 0 || !at.Add(e.stateTTL).After(now) {
		e.stateStats.PrunedEntries++
		return false
	}
	if e.stateGeneration == nil {
		e.stateStats.DiscardedEntries++
		return false
	}
	generation, exists := e.stateGeneration(identity.PluginID, identity.InstanceID)
	if !exists || generation != identity.Generation {
		e.stateStats.DiscardedEntries++
		return false
	}
	return true
}

type stateVictim struct {
	id string
	at time.Time
}

func (e *Engine) pruneState(records []observation.Record, now time.Time) bool {
	before := len(e.history.LastShown) + len(e.acknowledged)
	live := make(map[string]observation.Record, len(records))
	for _, record := range records {
		live[record.ID()] = record
	}
	ttlPruned := uint64(0)
	for id, shownAt := range e.history.LastShown {
		if e.stateTTL <= 0 || !shownAt.Add(e.stateTTL).After(now) {
			delete(e.history.LastShown, id)
			delete(e.lastShownState, id)
			ttlPruned++
			continue
		}
		if _, exists := live[id]; exists {
			continue
		}
	}
	capacityEvictions := pruneLastShownCapacity(e.history.LastShown, live, e.stateCapacity)
	e.syncLastShownState()
	if ttlPruned != 0 || capacityEvictions != 0 {
		e.mu.Lock()
		e.stateStats.LastShownTTLPruned += ttlPruned
		e.stateStats.LastShownCapacityEvictions += capacityEvictions
		e.stateStats.LastPrunedAt = now.UTC()
		e.mu.Unlock()
	}
	e.pruneAcknowledgementsWithLive(live, now)
	e.pruneCombinedState(live, now)
	return len(e.history.LastShown)+len(e.acknowledged) != before
}

func pruneLastShownCapacity(values map[string]time.Time, live map[string]observation.Record, capacity int) uint64 {
	if capacity <= 0 || len(values) <= capacity {
		return 0
	}
	victims := make([]stateVictim, 0, len(values))
	for id, at := range values {
		if _, exists := live[id]; !exists {
			victims = append(victims, stateVictim{id: id, at: at})
		}
	}
	sortStateVictims(victims)
	evicted := uint64(0)
	for _, victim := range victims {
		if len(values) <= capacity {
			break
		}
		delete(values, victim.id)
		evicted++
	}
	return evicted
}

func (e *Engine) pruneAcknowledgementsWithLive(live map[string]observation.Record, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ttlPruned := uint64(0)
	superseded := uint64(0)
	for id, state := range e.acknowledged {
		if e.stateTTL <= 0 || !state.touchedAt.Add(e.stateTTL).After(now) {
			delete(e.acknowledged, id)
			ttlPruned++
			continue
		}
		if record, exists := live[id]; exists {
			if state.generation != record.Generation || !state.observedAt.Equal(record.Observation.ObservedAt) {
				delete(e.acknowledged, id)
				superseded++
			}
			continue
		}
	}
	capacityEvictions := uint64(0)
	if e.stateCapacity > 0 && len(e.acknowledged) > e.stateCapacity {
		victims := make([]stateVictim, 0, len(e.acknowledged))
		for id, state := range e.acknowledged {
			if _, exists := live[id]; !exists {
				victims = append(victims, stateVictim{id: id, at: state.touchedAt})
			}
		}
		sortStateVictims(victims)
		for _, victim := range victims {
			if len(e.acknowledged) <= e.stateCapacity {
				break
			}
			delete(e.acknowledged, victim.id)
			capacityEvictions++
		}
	}
	if ttlPruned != 0 || superseded != 0 || capacityEvictions != 0 {
		e.stateStats.AcknowledgementTTLPruned += ttlPruned
		e.stateStats.SupersededAcknowledgementsPruned += superseded
		e.stateStats.AcknowledgementCapacityEvictions += capacityEvictions
		e.stateStats.LastPrunedAt = now.UTC()
	}
}

type combinedStateVictim struct {
	id     string
	at     time.Time
	ack    bool
	isLive bool
}

func (e *Engine) pruneCombinedState(live map[string]observation.Record, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	remaining := len(e.history.LastShown) + len(e.acknowledged) - e.stateCapacity
	if e.stateCapacity <= 0 || remaining <= 0 {
		return
	}
	victims := make([]combinedStateVictim, 0, len(e.history.LastShown)+len(e.acknowledged))
	for id, at := range e.history.LastShown {
		_, isLive := live[id]
		victims = append(victims, combinedStateVictim{id: id, at: at, isLive: isLive})
	}
	for id, state := range e.acknowledged {
		_, isLive := live[id]
		victims = append(victims, combinedStateVictim{id: id, at: state.touchedAt, ack: true, isLive: isLive})
	}
	sortCombinedStateVictims(victims)
	for _, victim := range victims[:remaining] {
		if victim.ack {
			delete(e.acknowledged, victim.id)
			e.stateStats.AcknowledgementCapacityEvictions++
		} else {
			delete(e.history.LastShown, victim.id)
			delete(e.lastShownState, victim.id)
			e.stateStats.LastShownCapacityEvictions++
		}
	}
	e.stateStats.LastPrunedAt = now.UTC()
}

func (e *Engine) syncLastShownState() {
	for id := range e.lastShownState {
		if _, exists := e.history.LastShown[id]; !exists {
			delete(e.lastShownState, id)
		}
	}
}

func (e *Engine) boundProposedAcknowledgements(
	proposed map[string]ackState,
	lastShown map[string]time.Time,
	lastShownState map[string]lastShownMetadata,
	records []observation.Record,
) (uint64, uint64) {
	remaining := len(lastShown) + len(proposed) - e.stateCapacity
	if e.stateCapacity <= 0 || remaining <= 0 {
		return 0, 0
	}
	live := make(map[string]observation.Record, len(records))
	for _, record := range records {
		live[record.ID()] = record
	}
	victims := make([]combinedStateVictim, 0, len(lastShown)+len(proposed))
	for key, at := range lastShown {
		_, isLive := live[key]
		victims = append(victims, combinedStateVictim{id: key, at: at, isLive: isLive})
	}
	for key, state := range proposed {
		_, isLive := live[key]
		victims = append(victims, combinedStateVictim{id: key, at: state.touchedAt, ack: true, isLive: isLive})
	}
	sortCombinedStateVictims(victims)
	acknowledgementEvictions := uint64(0)
	lastShownEvictions := uint64(0)
	for _, victim := range victims[:remaining] {
		if victim.ack {
			delete(proposed, victim.id)
			acknowledgementEvictions++
		} else {
			delete(lastShown, victim.id)
			delete(lastShownState, victim.id)
			lastShownEvictions++
		}
	}
	return acknowledgementEvictions, lastShownEvictions
}

func (e *Engine) stateDocument(records []observation.Record, acknowledgements map[string]ackState) attention.StateDocument {
	return e.stateDocumentFrom(records, acknowledgements, e.history.LastShown, e.lastShownState)
}

func (e *Engine) stateDocumentFrom(
	records []observation.Record,
	acknowledgements map[string]ackState,
	lastShown map[string]time.Time,
	lastShownState map[string]lastShownMetadata,
) attention.StateDocument {
	live := make(map[string]observation.Record, len(records))
	for _, record := range records {
		live[record.ID()] = record
	}
	document := attention.StateDocument{Version: attention.StateVersion}
	for id, shownAt := range lastShown {
		metadata, exists := lastShownState[id]
		if !exists || !metadata.persistent {
			if exists {
				continue
			}
			record, liveNow := live[id]
			if !liveNow {
				continue
			}
			rule, valid := e.resolve(record)
			if !valid || rule.Policy == presentation.PolicyInteractive {
				continue
			}
			metadata = lastShownMetadata{identity: attention.StateIdentity{
				PluginID: record.PluginID, InstanceID: record.Observation.Instance.ID, Generation: record.Generation,
				Channel: record.Observation.Channel, Key: record.Observation.Key,
			}, persistent: true}
		}
		document.LastShown = append(document.LastShown, attention.LastShownState{Identity: metadata.identity, ShownAt: shownAt.UTC()})
	}
	for id, state := range acknowledgements {
		identity := state.identity
		if identity.PluginID == "" {
			record, exists := live[id]
			if !exists {
				continue
			}
			identity = attention.StateIdentity{
				PluginID: record.PluginID, InstanceID: record.Observation.Instance.ID, Generation: state.generation,
				Channel: record.Observation.Channel, Key: record.Observation.Key,
			}
		}
		document.Acknowledgements = append(document.Acknowledgements, attention.AcknowledgementState{
			Identity: identity, ObservedAt: state.observedAt.UTC(), TouchedAt: state.touchedAt.UTC(),
		})
	}
	return document
}

func (e *Engine) persistState(records []observation.Record, acknowledgements map[string]ackState) error {
	if e.stateStore == nil {
		return nil
	}
	_, err := e.stateStore.Save(e.stateDocument(records, acknowledgements))
	return err
}

func stateIdentityID(identity attention.StateIdentity) string {
	return presentation.Identity{
		PluginID: identity.PluginID, InstanceID: identity.InstanceID, Channel: identity.Channel, Key: identity.Key,
	}.String()
}

func sortCombinedStateVictims(victims []combinedStateVictim) {
	slices.SortFunc(victims, func(left, right combinedStateVictim) int {
		if left.isLive != right.isLive {
			if !left.isLive {
				return -1
			}
			return 1
		}
		if order := left.at.Compare(right.at); order != 0 {
			return order
		}
		if left.ack != right.ack {
			if !left.ack {
				return -1
			}
			return 1
		}
		return cmp.Compare(left.id, right.id)
	})
}

func sortStateVictims(values []stateVictim) {
	slices.SortFunc(values, func(left, right stateVictim) int {
		if order := left.at.Compare(right.at); order != 0 {
			return order
		}
		return cmp.Compare(left.id, right.id)
	})
}

func (e *Engine) AttentionStateStatus() AttentionStateDiagnostics {
	e.stepMu.Lock()
	defer e.stepMu.Unlock()
	e.mu.RLock()
	result := e.stateStats
	result.AcknowledgementEntries = len(e.acknowledged)
	e.mu.RUnlock()
	result.LastShownEntries = len(e.history.LastShown)
	if e.stateStore == nil {
		result.Phase = "unavailable"
		return result
	}
	status := e.stateStore.Status()
	result.Phase = status.Phase
	result.LastErrorCode = status.LastErrorCode
	result.LastReadAt = status.LastReadAt
	result.LastWriteAt = status.LastWriteAt
	result.Failures = status.Failures
	return result
}
