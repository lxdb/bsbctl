// Package daemon composes observation state, core attention, and device output.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/identifier"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/presentation"
)

const (
	// The observation store admits at most 2,048 live identities, so this hard
	// bound can retain every live entry while evicting only stale state.
	defaultAttentionStateCapacity = 2048
	// Rotation intervals can reach 24 hours with 50 percent jitter; retain
	// absent identities beyond that maximum due window.
	defaultAttentionStateTTL    = 48 * time.Hour
	defaultPresentationCooldown = 30 * time.Second
)

// AttentionStateDiagnostics exposes bounded identity-churn counters without
// observation content or identifiers.
type AttentionStateDiagnostics struct {
	Phase                            string    `json:"phase"`
	LastErrorCode                    string    `json:"last_error_code,omitempty"`
	LastReadAt                       time.Time `json:"last_read_at,omitempty"`
	LastWriteAt                      time.Time `json:"last_write_at,omitempty"`
	Failures                         uint64    `json:"failures"`
	RestoredEntries                  uint64    `json:"restored_entries"`
	DiscardedEntries                 uint64    `json:"discarded_entries"`
	PrunedEntries                    uint64    `json:"pruned_entries"`
	LastShownEntries                 int       `json:"last_shown_entries"`
	AcknowledgementEntries           int       `json:"acknowledgement_entries"`
	LastShownTTLPruned               uint64    `json:"last_shown_ttl_pruned"`
	LastShownCapacityEvictions       uint64    `json:"last_shown_capacity_evictions"`
	AcknowledgementTTLPruned         uint64    `json:"acknowledgement_ttl_pruned"`
	AcknowledgementCapacityEvictions uint64    `json:"acknowledgement_capacity_evictions"`
	SupersededAcknowledgementsPruned uint64    `json:"superseded_acknowledgements_pruned"`
	LastPrunedAt                     time.Time `json:"last_pruned_at,omitempty"`
}

// PresentationCooldownDiagnostics reports only the process-local suppression
// gate and a stable reason. It never includes observation or provider content.
type PresentationCooldownDiagnostics struct {
	Active      bool      `json:"active"`
	Until       time.Time `json:"until,omitzero"`
	RemainingMS int64     `json:"remaining_ms,omitzero"`
	Reason      string    `json:"reason,omitempty"`
}

var (
	ErrObservationNotFound           = errors.New("observation was not found")
	ErrObservationNotAcknowledgeable = errors.New("observation does not require acknowledgement")
	errInvalidRendererResult         = errors.New("renderer returned an invalid delivery result")
)

// Renderer owns the serialized physical display boundary. Successful candidate
// renders return drawn, unchanged, or firmware suppression; successful clears
// return cleared or unchanged. Failure outcomes always accompany an error.
type Renderer interface {
	Render(context.Context, *presentation.Candidate) (attention.DeliveryOutcome, error)
}

type ForegroundCoordinator interface {
	AcquireCritical(context.Context, presentation.Candidate) bool
	ReleaseCritical()
}

type attentionStatePersistence interface {
	Load() (attention.StateDocument, error)
	Save(attention.StateDocument) (localstate.CommitOutcome, error)
	Status() attention.StateStoreStatus
}

// Engine is the small state machine between observations, core attention, and output.
type Engine struct {
	store          *observation.Store
	resolve        attention.Resolver
	renderer       Renderer
	foreground     ForegroundCoordinator
	history        presentation.History
	lastRevision   uint64
	lastGeneration uint64
	lastIdentity   presentation.Identity
	criticalOwned  bool
	retry          time.Duration
	recorder       *attention.Recorder
	mu             sync.RWMutex
	stepMu         sync.Mutex
	attentionState
	now                        func() time.Time
	presentationCooldownUntil  time.Time
	presentationCooldownReason string
	presentationCooldownKept   BackPresentation
	localSequence              atomic.Uint64
}

// attentionState owns the bounded durable scheduling state. Engine embeds it
// so selection can use acknowledgements without exposing persistence details.
type attentionState struct {
	acknowledged    map[string]ackState
	lastShownState  map[string]lastShownMetadata
	stateStore      attentionStatePersistence
	stateGeneration observation.Generation
	stateCapacity   int
	stateTTL        time.Duration
	stateStats      AttentionStateDiagnostics
}

// BackPresentation is an opaque lease for the exact presentation visible when
// a physical Back press was accepted. Callers may only return it to Engine.
type BackPresentation struct {
	identity   presentation.Identity
	id         string
	generation uint64
	revision   uint64
}

func (p BackPresentation) valid() bool {
	return p.id != "" && p.identity.PluginID != "" && p.generation != 0 && p.revision != 0
}

func (p BackPresentation) matches(id string, identity presentation.Identity, generation, revision uint64) bool {
	return p.valid() && p.id == id && p.identity == identity && p.generation == generation && p.revision == revision
}

type ackState struct {
	identity   attention.StateIdentity
	generation uint64
	observedAt time.Time
	touchedAt  time.Time
}

type lastShownMetadata struct {
	identity   attention.StateIdentity
	persistent bool
}

type EngineOptions struct {
	Store      *observation.Store
	Resolve    attention.Resolver
	Renderer   Renderer
	Foreground ForegroundCoordinator
	StateStore attentionStatePersistence
	Generation observation.Generation
	Recorder   *attention.Recorder
	Retry      time.Duration
}

func NewEngine(options EngineOptions) (*Engine, error) {
	if options.Store == nil {
		return nil, errors.New("observation store is required")
	}
	if options.Resolve == nil {
		return nil, errors.New("attention resolver is required")
	}
	if options.Renderer == nil {
		return nil, errors.New("attention renderer is required")
	}
	if options.Foreground == nil {
		return nil, errors.New("foreground coordinator is required")
	}
	if options.StateStore == nil {
		return nil, errors.New("attention state store is required")
	}
	if options.Generation == nil {
		return nil, errors.New("generation lookup is required")
	}
	retry := options.Retry
	if retry <= 0 {
		retry = 2 * time.Second
	}
	engine := newEngineWithRetry(options.Store, options.Resolve, options.Renderer, retry)
	engine.foreground = options.Foreground
	engine.recorder = options.Recorder
	engine.restoreAttentionState(options.StateStore, options.Generation)
	return engine, nil
}

func newEngineWithRetry(store *observation.Store, resolve attention.Resolver, renderer Renderer, retry time.Duration) *Engine {
	return &Engine{
		store: store, resolve: resolve, renderer: renderer,
		history: presentation.History{LastShown: make(map[string]time.Time)},
		retry:   retry,
		attentionState: attentionState{
			acknowledged: make(map[string]ackState), lastShownState: make(map[string]lastShownMetadata),
			stateCapacity: defaultAttentionStateCapacity, stateTTL: defaultAttentionStateTTL,
		},
		now: time.Now,
	}
}

// Step computes and applies one deterministic decision at now.
func (e *Engine) Step(ctx context.Context, now time.Time) error {
	_, err := e.step(ctx, now)
	return err
}

type evaluationSequence struct {
	store uint64
	local uint64
}

func (e *Engine) step(ctx context.Context, now time.Time) (evaluationSequence, error) {
	e.stepMu.Lock()
	defer e.stepMu.Unlock()
	records, storeSequence := e.store.SnapshotWithSequence()
	processed := evaluationSequence{store: storeSequence, local: e.localSequence.Load()}
	if e.pruneState(records, now) {
		_ = e.persistState(records, e.acknowledged)
	}
	for {
		decision := attention.Select(records, e.resolveRecord, e.history, now)
		if e.presentationCooldownActive(now) && (decision.Candidate == nil || decision.Candidate.Band != presentation.BandCriticalActionable) {
			if e.presentationCooldownKept.matches(e.history.CurrentID, e.lastIdentity, e.lastGeneration, e.lastRevision) &&
				containsExactRecord(records, e.presentationCooldownKept) {
				e.record(now, decision, attention.OutcomeUnchanged)
				return processed, nil
			}
			if e.history.CurrentID == "" && !e.criticalOwned {
				e.record(now, decision, attention.OutcomeUnchanged)
				return processed, nil
			}
			outcome, err := e.renderer.Render(ctx, nil)
			if contractErr := validateRenderResult(nil, outcome, err); contractErr != nil {
				e.record(now, decision, attention.OutcomeDeviceUnavailable)
				return processed, contractErr
			}
			if err != nil {
				e.record(now, decision, renderFailureOutcome(outcome))
				return processed, err
			}
			e.clearRenderedSelection()
			e.record(now, decision, outcome)
			return processed, nil
		}
		if decision.Candidate == nil {
			if e.history.CurrentID == "" && !e.criticalOwned {
				e.record(now, decision, attention.OutcomeUnchanged)
				return processed, nil
			}
			outcome, err := e.renderer.Render(ctx, nil)
			if contractErr := validateRenderResult(nil, outcome, err); contractErr != nil {
				e.record(now, decision, attention.OutcomeDeviceUnavailable)
				return processed, contractErr
			}
			if err != nil {
				e.record(now, decision, renderFailureOutcome(outcome))
				return processed, err
			}
			e.clearRenderedSelection()
			e.record(now, decision, outcome)
			return processed, nil
		}
		winner := decision.Candidate
		if !(e.criticalOwned && winner.Band != presentation.BandCriticalActionable) &&
			winner.ID() == e.history.CurrentID && winner.Generation == e.lastGeneration && winner.Revision == e.lastRevision {
			e.record(now, decision, attention.OutcomeUnchanged)
			return processed, nil
		}
		if winner.Band == presentation.BandCriticalActionable && !e.acquireCritical(ctx, *winner) {
			e.record(now, decision, attention.OutcomeUnchanged)
			return processed, nil
		}
		outcome, err := e.renderer.Render(ctx, winner)
		if contractErr := validateRenderResult(winner, outcome, err); contractErr != nil {
			e.record(now, decision, attention.OutcomeDeviceUnavailable)
			return processed, contractErr
		}
		if err != nil {
			if errors.Is(err, presentation.ErrInvalidPresentation) &&
				e.store.ExcludeRevisionWithoutSignal(winner.Identity(), winner.Generation, winner.Revision) {
				e.record(now, decision, attention.OutcomeInvalidPresentation)
				records, processed.store = e.store.SnapshotWithSequence()
				continue
			}
			e.record(now, decision, renderFailureOutcome(outcome))
			return processed, err
		}
		if outcome == attention.OutcomeFirmwareSuppressed {
			e.store.ExcludeRevisionWithoutSignal(winner.Identity(), winner.Generation, winner.Revision)
			e.clearRenderedSelection()
			e.record(now, decision, outcome)
			return processed, nil
		}
		if winner.Band != presentation.BandCriticalActionable {
			e.releaseCritical()
		}
		if winner.ID() != e.history.CurrentID {
			e.history.CurrentID = winner.ID()
			e.history.CurrentSince = now
		}
		e.history.LastShown[winner.ID()] = now
		e.lastShownState[winner.ID()] = lastShownMetadata{
			identity: attention.StateIdentity{
				PluginID: winner.PluginID, InstanceID: winner.InstanceID, Generation: winner.Generation,
				Channel: winner.Channel, Key: winner.Key,
			},
			persistent: winner.Policy != presentation.PolicyInteractive,
		}
		e.pruneState(records, now)
		_ = e.persistState(records, e.acknowledged)
		e.lastRevision = winner.Revision
		e.lastGeneration = winner.Generation
		e.lastIdentity = winner.Identity()
		e.record(now, decision, outcome)
		return processed, nil
	}
}

// CaptureBackPresentation binds a Back callback to the exact physical-delivery
// claim that existed before the plugin callback began.
func (e *Engine) CaptureBackPresentation() BackPresentation {
	e.stepMu.Lock()
	defer e.stepMu.Unlock()
	captured := BackPresentation{identity: e.lastIdentity, id: e.history.CurrentID, generation: e.lastGeneration, revision: e.lastRevision}
	if captured.valid() {
		e.store.ReserveExactRevisionWithoutSignal(captured.identity, captured.generation, captured.revision)
	}
	return captured
}

// ReconcileConsumedBack invalidates and redraws only when the captured
// presentation still owns the canvas. A newer presentation is left untouched.
func (e *Engine) ReconcileConsumedBack(ctx context.Context, captured BackPresentation, invalidate func()) error {
	e.stepMu.Lock()
	if captured.valid() {
		e.store.ReleaseExactRevisionReservationWithoutSignal(captured.identity, captured.generation, captured.revision)
	}
	if !captured.matches(e.history.CurrentID, e.lastIdentity, e.lastGeneration, e.lastRevision) {
		e.stepMu.Unlock()
		return nil
	}
	if invalidate != nil {
		invalidate()
	}
	e.lastRevision = 0
	e.lastGeneration = 0
	e.lastIdentity = presentation.Identity{}
	e.stepMu.Unlock()
	return e.Reconcile(ctx)
}

// DismissForBack starts the process-local presentation gate and tombstones the
// captured revision. It never clears a presentation that took ownership while
// the plugin callback was pending.
func (e *Engine) DismissForBack(ctx context.Context, captured BackPresentation, reason string) error {
	e.stepMu.Lock()
	defer e.stepMu.Unlock()
	now := e.now().UTC()
	e.presentationCooldownUntil = now.Add(defaultPresentationCooldown)
	e.presentationCooldownReason = presentationCooldownReason(reason)

	if captured.valid() {
		e.store.ExcludeExactRevisionWithoutSignal(captured.identity, captured.generation, captured.revision)
	}
	if !captured.matches(e.history.CurrentID, e.lastIdentity, e.lastGeneration, e.lastRevision) {
		e.presentationCooldownKept = BackPresentation{identity: e.lastIdentity, id: e.history.CurrentID, generation: e.lastGeneration, revision: e.lastRevision}
		e.signalLocalChange()
		return nil
	}
	e.presentationCooldownKept = BackPresentation{}

	outcome, err := e.renderer.Render(ctx, nil)
	if contractErr := validateRenderResult(nil, outcome, err); contractErr != nil {
		e.record(now, attention.Decision{}, attention.OutcomeDeviceUnavailable)
		e.signalLocalChange()
		return contractErr
	}
	if err != nil {
		e.record(now, attention.Decision{}, renderFailureOutcome(outcome))
		e.signalLocalChange()
		return err
	}
	e.clearRenderedSelection()
	e.record(now, attention.Decision{}, outcome)
	e.signalLocalChange()
	return nil
}

func (e *Engine) clearRenderedSelection() {
	e.history.CurrentID = ""
	e.history.CurrentSince = time.Time{}
	e.lastRevision = 0
	e.lastGeneration = 0
	e.lastIdentity = presentation.Identity{}
	e.releaseCritical()
}

func (e *Engine) signalLocalChange() {
	e.localSequence.Add(1)
	e.store.Signal()
}

func (e *Engine) presentationCooldownActive(now time.Time) bool {
	if e.presentationCooldownUntil.IsZero() {
		return false
	}
	if now.Before(e.presentationCooldownUntil) {
		return true
	}
	e.presentationCooldownUntil = time.Time{}
	e.presentationCooldownReason = ""
	e.presentationCooldownKept = BackPresentation{}
	return false
}

func containsExactRecord(records []observation.Record, captured BackPresentation) bool {
	for _, record := range records {
		if captured.matches(record.ID(), record.Identity(), record.Generation, record.Observation.Revision) {
			return true
		}
	}
	return false
}

func presentationCooldownReason(reason string) string {
	switch reason {
	case "back_not_consumed", "back_session_input_failed", "back_no_session":
		return reason
	default:
		return "back_fallback"
	}
}

func (e *Engine) acquireCritical(ctx context.Context, candidate presentation.Candidate) bool {
	e.mu.RLock()
	coordinator := e.foreground
	e.mu.RUnlock()
	if coordinator != nil && !coordinator.AcquireCritical(ctx, candidate) {
		return false
	}
	e.criticalOwned = true
	return true
}

func (e *Engine) releaseCritical() {
	if !e.criticalOwned {
		return
	}
	e.mu.RLock()
	coordinator := e.foreground
	e.mu.RUnlock()
	if coordinator != nil {
		coordinator.ReleaseCritical()
	}
	e.criticalOwned = false
}

func (e *Engine) Acknowledge(id string) error {
	e.stepMu.Lock()
	defer e.stepMu.Unlock()
	records := e.store.Snapshot()
	for _, record := range records {
		if record.ID() != id {
			continue
		}
		rule, ok := e.resolve(record)
		if !ok || !rule.RequiresAck {
			return fmt.Errorf("%w: %q", ErrObservationNotAcknowledgeable, id)
		}
		now := e.now().UTC()
		e.pruneState(records, now)
		e.mu.RLock()
		proposed := make(map[string]ackState, len(e.acknowledged)+1)
		for key, state := range e.acknowledged {
			proposed[key] = state
		}
		e.mu.RUnlock()
		identity := attention.StateIdentity{
			PluginID: record.PluginID, InstanceID: record.Observation.Instance.ID, Generation: record.Generation,
			Channel: record.Observation.Channel, Key: record.Observation.Key,
		}
		proposed[id] = ackState{identity: identity, generation: record.Generation, observedAt: record.Observation.ObservedAt, touchedAt: now}
		proposedLastShown := maps.Clone(e.history.LastShown)
		proposedLastShownState := maps.Clone(e.lastShownState)
		acknowledgementEvictions, lastShownEvictions := e.boundProposedAcknowledgements(
			proposed, proposedLastShown, proposedLastShownState, records,
		)
		if e.stateStore != nil {
			outcome, err := e.stateStore.Save(e.stateDocumentFrom(records, proposed, proposedLastShown, proposedLastShownState))
			if err != nil {
				return err
			}
			if outcome != localstate.Committed {
				return errors.New("attention acknowledgement durability was not confirmed")
			}
		}
		e.mu.Lock()
		e.acknowledged = proposed
		e.history.LastShown = proposedLastShown
		e.lastShownState = proposedLastShownState
		e.stateStats.AcknowledgementCapacityEvictions += acknowledgementEvictions
		e.stateStats.LastShownCapacityEvictions += lastShownEvictions
		if acknowledgementEvictions != 0 || lastShownEvictions != 0 {
			e.stateStats.LastPrunedAt = now
		}
		e.mu.Unlock()
		e.Wake()
		return nil
	}
	return fmt.Errorf("%w: %q", ErrObservationNotFound, id)
}

func (e *Engine) AcknowledgeAttention(id string) error { return e.Acknowledge(id) }

func (e *Engine) Wake() {
	e.signalLocalChange()
}

// InvalidateRenderedSelection forgets only the physical-delivery claim. The
// selected identity and attention history remain intact so reconciliation can
// redraw the same revision after firmware closes the HTTP canvas.
func (e *Engine) InvalidateRenderedSelection() {
	e.stepMu.Lock()
	e.lastRevision = 0
	e.lastGeneration = 0
	e.lastIdentity = presentation.Identity{}
	e.stepMu.Unlock()
	e.Wake()
}

// SelectedObservation returns only the exact observation revision that was
// successfully rendered. A newer unpublished revision fails closed until the
// renderer applies it.
func (e *Engine) SelectedObservation() (observation.Record, bool) {
	e.stepMu.Lock()
	defer e.stepMu.Unlock()
	if e.history.CurrentID == "" || e.lastGeneration == 0 || e.lastRevision == 0 {
		return observation.Record{}, false
	}
	for _, record := range e.store.Snapshot() {
		if record.ID() == e.history.CurrentID && record.Generation == e.lastGeneration && record.Observation.Revision == e.lastRevision {
			return record, true
		}
	}
	return observation.Record{}, false
}

func (e *Engine) RemoveInstance(pluginID, instanceID string, throughGeneration uint64) {
	e.store.WithdrawInstance(pluginID, instanceID, throughGeneration)
	prefix, _ := identifier.Encode(pluginID, instanceID)
	prefix += "/"
	e.stepMu.Lock()
	for id := range e.history.LastShown {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		metadata, exists := e.lastShownState[id]
		if exists && metadata.identity.Generation > throughGeneration {
			continue
		}
		delete(e.history.LastShown, id)
		delete(e.lastShownState, id)
	}
	e.mu.Lock()
	for id, state := range e.acknowledged {
		if state.generation <= throughGeneration && strings.HasPrefix(id, prefix) {
			delete(e.acknowledged, id)
		}
	}
	e.mu.Unlock()
	_ = e.persistState(e.store.Snapshot(), e.acknowledged)
	e.stepMu.Unlock()
}

// Reconcile synchronously applies the latest attention decision. Lifecycle
// transactions use it to establish display ordering before removing assets.
func (e *Engine) Reconcile(ctx context.Context) error {
	return e.Step(ctx, time.Now())
}

func (e *Engine) resolveRecord(record observation.Record) (attention.Rule, bool) {
	rule, ok := e.resolve(record)
	if !ok {
		return rule, false
	}
	e.mu.RLock()
	state, acknowledged := e.acknowledged[record.ID()]
	e.mu.RUnlock()
	rule.Acknowledged = acknowledged && state.generation == record.Generation && state.observedAt.Equal(record.Observation.ObservedAt)
	return rule, true
}

func renderFailureOutcome(outcome attention.DeliveryOutcome) attention.DeliveryOutcome {
	switch outcome {
	case attention.OutcomeDeviceUnavailable, attention.OutcomeAssetMissing, attention.OutcomeInvalidPresentation:
		return outcome
	}
	return attention.OutcomeDeviceUnavailable
}

func validateRenderResult(candidate *presentation.Candidate, outcome attention.DeliveryOutcome, err error) error {
	if err != nil {
		switch outcome {
		case attention.OutcomeDeviceUnavailable, attention.OutcomeAssetMissing, attention.OutcomeInvalidPresentation:
			return nil
		default:
			return fmt.Errorf("%w: outcome %q cannot accompany an error: %w", errInvalidRendererResult, outcome, err)
		}
	}
	if candidate == nil {
		if outcome == attention.OutcomeCleared || outcome == attention.OutcomeUnchanged {
			return nil
		}
		return fmt.Errorf("%w: clear returned outcome %q", errInvalidRendererResult, outcome)
	}
	switch outcome {
	case attention.OutcomeDrawn, attention.OutcomeUnchanged, attention.OutcomeFirmwareSuppressed:
		return nil
	default:
		return fmt.Errorf("%w: candidate render returned outcome %q", errInvalidRendererResult, outcome)
	}
}

func (e *Engine) record(now time.Time, decision attention.Decision, outcome attention.DeliveryOutcome) {
	recorder := e.attentionRecorder()
	if recorder == nil {
		return
	}
	selectedID := ""
	if decision.Candidate != nil {
		selectedID = decision.Candidate.ID()
	}
	_ = recorder.Append(attention.Trace{At: now.UTC(), SelectedID: selectedID, Outcome: outcome, Evaluations: decision.Evaluations})
}

func (e *Engine) AttentionSnapshot() (attention.Trace, bool) {
	recorder := e.attentionRecorder()
	if recorder == nil {
		return attention.Trace{}, false
	}
	return recorder.Snapshot()
}

func (e *Engine) AttentionExplain(id string) (attention.Evaluation, bool) {
	recorder := e.attentionRecorder()
	if recorder == nil {
		return attention.Evaluation{}, false
	}
	return recorder.Explain(id)
}

func (e *Engine) AttentionHistory(limit int, since time.Time) []attention.Trace {
	recorder := e.attentionRecorder()
	if recorder == nil {
		return nil
	}
	return recorder.History(limit, since)
}

func (e *Engine) RecorderStatus() attention.RecorderStatus {
	recorder := e.attentionRecorder()
	if recorder == nil {
		return attention.RecorderStatus{Phase: attention.RecorderUnavailable}
	}
	return recorder.Status()
}

func (e *Engine) ObservationDiagnostics() observation.StoreDiagnostics {
	return e.store.Diagnostics()
}

func (e *Engine) PresentationCooldownStatus() PresentationCooldownDiagnostics {
	e.stepMu.Lock()
	defer e.stepMu.Unlock()
	now := e.now().UTC()
	if !e.presentationCooldownActive(now) {
		return PresentationCooldownDiagnostics{}
	}
	remaining := e.presentationCooldownUntil.Sub(now)
	return PresentationCooldownDiagnostics{
		Active: true, Until: e.presentationCooldownUntil, RemainingMS: remaining.Milliseconds(), Reason: e.presentationCooldownReason,
	}
}

func (e *Engine) attentionRecorder() *attention.Recorder {
	e.mu.RLock()
	recorder := e.recorder
	e.mu.RUnlock()
	return recorder
}

// NextDeadline returns the earliest known time at which eligibility or a hold
// can change without a new publication.
func (e *Engine) NextDeadline(now time.Time) time.Time {
	e.stepMu.Lock()
	defer e.stepMu.Unlock()
	var next time.Time
	consider := func(value time.Time) {
		if value.IsZero() || !value.After(now) {
			return
		}
		if next.IsZero() || value.Before(next) {
			next = value
		}
	}
	records := e.store.Snapshot()
	if e.pruneState(records, now) {
		_ = e.persistState(records, e.acknowledged)
	}
	for _, record := range records {
		consider(record.Observation.ValidUntil)
	}
	if e.stateTTL > 0 {
		for _, shownAt := range e.history.LastShown {
			consider(shownAt.Add(e.stateTTL))
		}
		e.mu.RLock()
		for _, state := range e.acknowledged {
			consider(state.touchedAt.Add(e.stateTTL))
		}
		e.mu.RUnlock()
	}
	if e.presentationCooldownActive(now) {
		consider(e.presentationCooldownUntil)
	}
	decision := attention.Select(records, e.resolveRecord, e.history, now)
	for _, evaluation := range decision.Evaluations {
		consider(evaluation.CooldownUntil)
		if evaluation.Reason == attention.ReasonNotDue {
			consider(evaluation.NextDue)
		}
	}
	if decision.Candidate != nil && decision.Candidate.ID() == e.history.CurrentID {
		consider(e.history.CurrentSince.Add(decision.Candidate.Hold()))
	}
	return next
}

// Run drains coalesced candidate changes and exact state-transition deadlines.
func (e *Engine) Run(ctx context.Context) error {
	retryAt := time.Time{}
	processed, err := e.step(ctx, time.Now())
	if err != nil {
		retryAt = time.Now().Add(e.retry)
	}
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		now := time.Now()
		deadline := e.NextDeadline(now)
		if !retryAt.IsZero() && (deadline.IsZero() || retryAt.Before(deadline)) {
			deadline = retryAt
		}
		var timerC <-chan time.Time
		if !deadline.IsZero() {
			delay := time.Until(deadline)
			if timer == nil {
				timer = time.NewTimer(delay)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(delay)
			}
			timerC = timer.C
		}
		shouldStep := false
		select {
		case <-ctx.Done():
			return nil
		case <-e.store.Changes():
			shouldStep = e.store.Sequence() > processed.store || e.localSequence.Load() > processed.local
			if !shouldStep && !deadline.IsZero() && !deadline.After(time.Now()) {
				shouldStep = true
			}
		case <-timerC:
			shouldStep = true
		}
		if !shouldStep {
			continue
		}
		processed, err = e.step(ctx, time.Now())
		if err != nil && !errors.Is(err, context.Canceled) {
			retryAt = time.Now().Add(e.retry)
		} else {
			retryAt = time.Time{}
		}
	}
}
