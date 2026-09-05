package observation

import (
	"cmp"
	"errors"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

var (
	ErrStaleRevision   = errors.New("observation revision is stale")
	ErrStaleGeneration = errors.New("observation generation is stale")
	ErrNotFound        = errors.New("observation not found")
	ErrCapacity        = errors.New("observation capacity exceeded")
)

const (
	globalCapacity                = 2048
	pluginCapacity                = 512
	pluginInstanceCapacity        = 256
	pluginInstanceChannelCapacity = 128
	CapacityRejectionCode         = "observation_capacity"
)

// StoreDiagnostics is a bounded, redacted view of observation-store health.
type StoreDiagnostics struct {
	LiveCount          int       `json:"live_count"`
	CapacityRejections uint64    `json:"capacity_rejections"`
	LastRejectionAt    time.Time `json:"last_rejection_at,omitempty"`
	LastRejectionCode  string    `json:"last_rejection_code,omitempty"`
}

// Generation resolves the current core-owned generation for an app instance.
type Generation func(pluginID, instanceID string) (uint64, bool)

// Source is authenticated process identity attached by pluginhost.
type Source struct {
	PluginID   string
	Generation uint64
}

// Record is an observation plus identity that plugins cannot provide themselves.
type Record struct {
	PluginID          string
	Generation        uint64
	AdmissionSequence uint64
	Observation       protocol.Observation
}

type exclusionKey struct {
	identity   presentation.Identity
	generation uint64
}

type exclusionState struct {
	through  uint64
	reserved uint64
}

// ID returns the stable identity used for replacement and explanation.
func (r Record) ID() string {
	return r.Identity().String()
}

func (r Record) Identity() presentation.Identity {
	return presentation.Identity{PluginID: r.PluginID, InstanceID: r.Observation.Instance.ID, Channel: r.Observation.Channel, Key: r.Observation.Key}
}

// Store owns the latest unresolved observation for each stable identity.
type Store struct {
	mu         sync.RWMutex
	records    map[presentation.Identity]Record
	watermarks map[exclusionKey]exclusionState
	generation Generation
	now        func() time.Time
	changes    chan struct{}
	sequence   uint64
	rejections uint64
	rejectedAt time.Time
	rejected   string
}

func NewStore(generation Generation, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		records: make(map[presentation.Identity]Record), watermarks: make(map[exclusionKey]exclusionState),
		generation: generation, now: now, changes: make(chan struct{}, 1),
	}
}

func (s *Store) Changes() <-chan struct{} { return s.changes }

// Sequence identifies the latest accepted observation-state mutation. Signals
// and internal exact-revision exclusions do not advance it.
func (s *Store) Sequence() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sequence
}

// Signal requests reevaluation after core-owned state such as acknowledgement changes.
func (s *Store) Signal() { s.notify() }

// Publish validates identity/generation and applies a strictly newer revision.
func (s *Store) Publish(source Source, value protocol.Observation) error {
	now := s.now()
	if err := value.Validate(now); err != nil {
		return err
	}
	if source.PluginID == "" || source.Generation == 0 {
		return ErrStaleGeneration
	}
	if s.generation != nil {
		generation, exists := s.generation(source.PluginID, value.Instance.ID)
		if !exists || generation != source.Generation {
			return ErrStaleGeneration
		}
	}
	record := Record{PluginID: source.PluginID, Generation: source.Generation, Observation: cloneObservation(value)}
	id := record.Identity()
	identityKey := exclusionKey{identity: id, generation: source.Generation}
	s.mu.Lock()
	pruned := s.pruneLocked(now)
	if pruned {
		s.sequence++
	}
	current, exists := s.records[id]
	if exists && value.Revision < current.Observation.Revision {
		s.mu.Unlock()
		if pruned {
			s.notify()
		}
		return ErrStaleRevision
	}
	if exists && value.Revision == current.Observation.Revision {
		if reflect.DeepEqual(value, current.Observation) {
			s.mu.Unlock()
			if pruned {
				s.notify()
			}
			return nil
		}
		s.mu.Unlock()
		if pruned {
			s.notify()
		}
		return ErrStaleRevision
	}
	if !exists && value.Disposition != protocol.DispositionResolved && s.atCapacityLocked(identityKey) {
		s.rejections++
		s.rejectedAt = now.UTC()
		s.rejected = CapacityRejectionCode
		s.mu.Unlock()
		if pruned {
			s.notify()
		}
		return ErrCapacity
	}
	if value.Disposition == protocol.DispositionResolved {
		delete(s.records, id)
	} else {
		record.AdmissionSequence = s.sequence + 1
		s.records[id] = record
	}
	s.sequence++
	s.mu.Unlock()
	s.notify()
	return nil
}

func (s *Store) Withdraw(pluginID, instanceID, channel, key string) error {
	id := presentation.Identity{PluginID: pluginID, InstanceID: instanceID, Channel: channel, Key: key}
	s.mu.Lock()
	if _, exists := s.records[id]; !exists {
		s.mu.Unlock()
		return ErrNotFound
	}
	delete(s.records, id)
	s.sequence++
	s.mu.Unlock()
	s.notify()
	return nil
}

// WithdrawInstance removes retained observations through the last generation
// owned by a deleted app while preserving any later same-ID recreation.
func (s *Store) WithdrawInstance(pluginID, instanceID string, throughGeneration uint64) {
	removed := false
	s.mu.Lock()
	for id := range s.records {
		if id.PluginID == pluginID && id.InstanceID == instanceID && s.records[id].Generation <= throughGeneration {
			delete(s.records, id)
			removed = true
		}
	}
	for key := range s.watermarks {
		if key.identity.PluginID == pluginID && key.identity.InstanceID == instanceID && key.generation <= throughGeneration {
			delete(s.watermarks, key)
		}
	}
	if removed {
		s.sequence++
	}
	s.mu.Unlock()
	if removed {
		s.notify()
	}
}

// WithdrawGeneration removes only records emitted by one child generation.
func (s *Store) WithdrawGeneration(pluginID string, generation uint64) {
	changed := false
	s.mu.Lock()
	for id, record := range s.records {
		if record.PluginID == pluginID && record.Generation == generation {
			delete(s.records, id)
			changed = true
		}
	}
	if changed {
		s.sequence++
	}
	s.mu.Unlock()
	if changed {
		s.notify()
	}
}

func (s *Store) Snapshot() []Record {
	result, _ := s.SnapshotWithSequence()
	return result
}

// SnapshotWithSequence returns one owned snapshot and the accepted mutation
// sequence that produced it.
func (s *Store) SnapshotWithSequence() ([]Record, uint64) {
	now := s.now()
	s.mu.Lock()
	pruned := s.pruneLocked(now)
	if pruned {
		s.sequence++
	}
	result := make([]Record, 0, len(s.records))
	for id, record := range s.records {
		exclusion, excluded := s.watermarks[exclusionKey{identity: id, generation: record.Generation}]
		if excluded && record.Observation.Revision <= exclusion.through {
			continue
		}
		record.Observation = cloneObservation(record.Observation)
		result = append(result, record)
	}
	sequence := s.sequence
	s.mu.Unlock()
	if pruned {
		s.notify()
	}
	slices.SortFunc(result, func(left, right Record) int { return cmp.Compare(left.ID(), right.ID()) })
	return result, sequence
}

// ExcludeRevisionWithoutSignal excludes one deterministic revision from
// arbitration without waking the engine. Callers decide whether to continue
// local selection or stop until a later observation naturally signals work.
func (s *Store) ExcludeRevisionWithoutSignal(id presentation.Identity, generation, revision uint64) bool {
	s.mu.Lock()
	record, exists := s.records[id]
	if !exists || record.Generation != generation || record.Observation.Revision != revision {
		s.mu.Unlock()
		return false
	}
	s.excludeThroughLocked(id, generation, revision)
	s.mu.Unlock()
	return true
}

// ReserveExactRevisionWithoutSignal preserves proof that one exact live
// revision was admitted while a synchronous Back callback is in flight.
func (s *Store) ReserveExactRevisionWithoutSignal(id presentation.Identity, generation, revision uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[id]
	if !exists || record.Generation != generation || record.Observation.Revision != revision {
		return false
	}
	key := exclusionKey{identity: id, generation: generation}
	state := s.watermarks[key]
	state.reserved = revision
	s.watermarks[key] = state
	return true
}

// ReleaseExactRevisionReservationWithoutSignal removes only a matching Back
// reservation. A committed exclusion watermark remains intact.
func (s *Store) ReleaseExactRevisionReservationWithoutSignal(id presentation.Identity, generation, revision uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := exclusionKey{identity: id, generation: generation}
	state, exists := s.watermarks[key]
	if !exists || state.reserved != revision {
		return
	}
	state.reserved = 0
	if state.through == 0 {
		delete(s.watermarks, key)
		return
	}
	s.watermarks[key] = state
}

// ExcludeExactRevisionWithoutSignal advances a process-local revision
// watermark even when the producer withdrew the live record before the host
// completed a Back fallback. A later revision or generation remains eligible.
func (s *Store) ExcludeExactRevisionWithoutSignal(id presentation.Identity, generation, revision uint64) bool {
	if id.PluginID == "" || id.InstanceID == "" || id.Channel == "" || id.Key == "" || generation == 0 || revision == 0 {
		return false
	}
	if s.generation != nil {
		current, exists := s.generation(id.PluginID, id.InstanceID)
		if !exists || current != generation {
			return false
		}
	}
	s.mu.Lock()
	key := exclusionKey{identity: id, generation: generation}
	state, reserved := s.watermarks[key]
	record, live := s.records[id]
	if (!reserved || state.reserved != revision) && (!live || record.Generation != generation || record.Observation.Revision != revision) {
		s.mu.Unlock()
		return false
	}
	s.excludeThroughLocked(id, generation, revision)
	s.mu.Unlock()
	return true
}

func (s *Store) excludeThroughLocked(id presentation.Identity, generation, revision uint64) {
	key := exclusionKey{identity: id, generation: generation}
	state := s.watermarks[key]
	if revision > state.through {
		state.through = revision
	}
	if state.reserved == revision {
		state.reserved = 0
	}
	s.watermarks[key] = state
}

// Diagnostics returns only core-generated counts, timestamps, and stable codes.
func (s *Store) Diagnostics() StoreDiagnostics {
	now := s.now()
	s.mu.Lock()
	pruned := s.pruneLocked(now)
	if pruned {
		s.sequence++
	}
	result := StoreDiagnostics{
		LiveCount: len(s.records), CapacityRejections: s.rejections,
		LastRejectionAt: s.rejectedAt, LastRejectionCode: s.rejected,
	}
	s.mu.Unlock()
	if pruned {
		s.notify()
	}
	return result
}

func (s *Store) pruneLocked(now time.Time) bool {
	changed := false
	for id, record := range s.records {
		if !record.Observation.ValidUntil.After(now) {
			delete(s.records, id)
			changed = true
		}
	}
	return changed
}

func (s *Store) atCapacityLocked(candidate exclusionKey) bool {
	if _, exists := s.records[candidate.identity]; exists {
		return false
	}
	if _, exists := s.watermarks[candidate]; exists {
		return false
	}
	global, plugin, instance, channel := len(s.watermarks), 0, 0, 0
	for key := range s.watermarks {
		id := key.identity
		if id.PluginID != candidate.identity.PluginID {
			continue
		}
		plugin++
		if id.InstanceID != candidate.identity.InstanceID {
			continue
		}
		instance++
		if id.Channel == candidate.identity.Channel {
			channel++
		}
	}
	for id, record := range s.records {
		key := exclusionKey{identity: id, generation: record.Generation}
		if _, tracked := s.watermarks[key]; tracked {
			continue
		}
		global++
		if id.PluginID != candidate.identity.PluginID {
			continue
		}
		plugin++
		if id.InstanceID != candidate.identity.InstanceID {
			continue
		}
		instance++
		if id.Channel == candidate.identity.Channel {
			channel++
		}
	}
	return global >= globalCapacity || plugin >= pluginCapacity || instance >= pluginInstanceCapacity || channel >= pluginInstanceChannelCapacity
}

func (s *Store) notify() {
	select {
	case s.changes <- struct{}{}:
	default:
	}
}

func cloneObservation(value protocol.Observation) protocol.Observation {
	if value.Scene != nil {
		copy := *value.Scene
		copy.Elements = slices.Clone(value.Scene.Elements)
		for index := range copy.Elements {
			element := &copy.Elements[index]
			if element.Text != nil {
				text := *element.Text
				if text.Marquee != nil {
					text.Marquee = new(*text.Marquee)
				}
				element.Text = &text
			}
			if element.Image != nil {
				element.Image = new(*element.Image)
			}
			if element.Animation != nil {
				element.Animation = new(*element.Animation)
			}
			if element.Rectangle != nil {
				element.Rectangle = new(*element.Rectangle)
			}
			if element.Countdown != nil {
				element.Countdown = new(*element.Countdown)
			}
		}
		value.Scene = &copy
	}
	if value.BusyTimer != nil {
		copy := *value.BusyTimer
		value.BusyTimer = &copy
	}
	if value.Audio != nil {
		copy := *value.Audio
		value.Audio = &copy
	}
	return value
}
