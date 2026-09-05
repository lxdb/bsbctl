package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type foregroundState string

const (
	foregroundIdle          foregroundState = "idle"
	foregroundPending       foregroundState = "pending"
	foregroundInteractive   foregroundState = "interactive"
	foregroundExecuting     foregroundState = "executing"
	foregroundCriticalOwned foregroundState = "critical_owned"
)

type SessionCoordinator struct {
	launchMu sync.Mutex
	mu       sync.RWMutex
	change   func(SessionChange)

	foreground           string
	foregroundPlugin     string
	foregroundSession    pluginhost.SessionToken
	foregroundGeneration uint64
	foregroundSource     *observationSessionSource
	executing            bool
	pendingForeground    string
	pendingPlugin        string
	pendingGeneration    uint64
	pendingSession       pluginhost.SessionToken
	nextSession          uint64
	criticalIdentity     string
	criticalSequence     uint64
	lifecycles           map[string]pluginSessionLifecycleState
}

type pluginSessionLifecycleState struct {
	sequence              uint64
	errorCode             string
	errorAt               time.Time
	fallbackAfterSequence uint64
}

type observationSessionSource struct {
	pluginID, instanceID, channel, key string
	generation, revision               uint64
}

type sessionAdmission struct {
	pluginID   string
	instanceID string
	token      pluginhost.SessionToken
	generation uint64
}

type sessionChangeKind uint8

const (
	sessionChanged sessionChangeKind = iota
	sessionCanceled
	sessionCompleted
)

// SessionChange transfers one admitted lifecycle transition to the session
// input broker without coupling plugin callbacks to broker construction.
type SessionChange struct {
	kind     sessionChangeKind
	pluginID string
	target   protocol.InstanceRef
	token    pluginhost.SessionToken
}

func (c SessionChange) Apply(inputs SessionInputController) {
	if inputs == nil {
		return
	}
	switch c.kind {
	case sessionCanceled:
		inputs.Cancel(c.pluginID, c.target, string(c.token))
	case sessionCompleted:
		inputs.Complete(c.pluginID, c.target, string(c.token))
	}
}

func NewSessionCoordinator(change func(SessionChange)) *SessionCoordinator {
	return &SessionCoordinator{change: change, lifecycles: make(map[string]pluginSessionLifecycleState)}
}

func (o *SessionCoordinator) notifyChange(change SessionChange) {
	if o.change == nil {
		return
	}
	o.change(change)
}

func (o *SessionCoordinator) serializeLaunch() func() {
	o.launchMu.Lock()
	return o.launchMu.Unlock
}

func (o *SessionCoordinator) begin(pluginID, instanceID string, generation uint64) (sessionAdmission, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.criticalIdentity != "" || o.executing {
		return sessionAdmission{}, false
	}
	o.nextSession++
	admission := sessionAdmission{
		pluginID:   pluginID,
		instanceID: instanceID,
		token:      pluginhost.SessionToken(fmt.Sprintf("interactive-%d", o.nextSession)),
		generation: generation,
	}
	o.pendingForeground = instanceID
	o.pendingPlugin = pluginID
	o.pendingGeneration = generation
	o.pendingSession = admission.token
	return admission, true
}

func (o *SessionCoordinator) cancel(admission sessionAdmission) {
	o.mu.Lock()
	if o.pendingMatchesAdmission(admission) {
		o.clearPendingLocked()
	}
	o.mu.Unlock()
}

func (o *SessionCoordinator) promote(admission sessionAdmission, pluginID string, source *observationSessionSource) (sessionTermination, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.pendingMatchesAdmission(admission) || pluginID != admission.pluginID {
		return sessionTermination{}, false
	}
	o.clearPendingLocked()
	previous := sessionTermination{pluginID: o.foregroundPlugin, instanceID: o.foreground, generation: o.foregroundGeneration, token: o.foregroundSession}
	o.foreground = admission.instanceID
	o.foregroundPlugin = pluginID
	o.foregroundSession = admission.token
	o.foregroundGeneration = admission.generation
	o.foregroundSource = source
	o.executing = false
	return previous, true
}

func (o *SessionCoordinator) detach(app config.App) (sessionTermination, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	changed := false
	if o.pendingForeground == app.ID && o.pendingPlugin == app.PluginID {
		o.clearPendingLocked()
		changed = true
	}
	var termination sessionTermination
	if o.foreground == app.ID && o.foregroundPlugin == app.PluginID {
		termination = sessionTermination{pluginID: app.PluginID, instanceID: app.ID, generation: o.foregroundGeneration, token: o.foregroundSession}
		o.clearForegroundLocked()
		changed = true
	}
	return termination, changed
}

func (o *SessionCoordinator) clear(expected string, expectedToken pluginhost.SessionToken) (sessionTermination, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	activeMatch := (expected == "" || o.foreground == expected) && (expectedToken == "" || o.foregroundSession == expectedToken)
	pendingMatch := (expected == "" || o.pendingForeground == expected) && (expectedToken == "" || o.pendingSession == expectedToken)
	if pendingMatch && o.pendingForeground != "" {
		o.clearPendingLocked()
	}
	if !activeMatch || o.foreground == "" {
		return sessionTermination{}, false
	}
	termination := sessionTermination{pluginID: o.foregroundPlugin, instanceID: o.foreground, generation: o.foregroundGeneration, token: o.foregroundSession}
	o.clearForegroundLocked()
	return termination, true
}

func (o *SessionCoordinator) invalidate(value pluginhost.SessionInvalidation) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	pendingChanged := o.pendingPlugin == value.PluginID && o.pendingForeground == value.InstanceID && o.pendingSession == value.Token && o.pendingGeneration == value.Generation
	foregroundChanged := o.foregroundPlugin == value.PluginID && o.foreground == value.InstanceID && o.foregroundSession == value.Token && o.foregroundGeneration == value.Generation
	if pendingChanged {
		o.clearPendingLocked()
	}
	if foregroundChanged {
		o.clearForegroundLocked()
	}
	return pendingChanged || foregroundChanged
}

func (o *SessionCoordinator) complete(pluginID, instanceID string, generation uint64, token pluginhost.SessionToken, pendingEligible bool) (foregroundChanged, changed bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	foregroundChanged = o.foregroundPlugin == pluginID && o.foreground == instanceID && o.foregroundGeneration == generation && o.foregroundSession == token
	pendingChanged := pendingEligible && o.pendingPlugin == pluginID && o.pendingForeground == instanceID && o.pendingGeneration == generation && o.pendingSession == token
	if pendingChanged {
		o.clearPendingLocked()
	}
	if foregroundChanged {
		o.clearForegroundLocked()
	}
	return foregroundChanged, pendingChanged || foregroundChanged
}

func (o *SessionCoordinator) configurationChanged(document config.Document) {
	o.mu.Lock()
	app, exists := document.Apps[o.pendingForeground]
	pendingStillCurrent := exists && app.Enabled && app.PluginID == o.pendingPlugin && app.Generation == o.pendingGeneration
	if o.pendingForeground != "" && !pendingStillCurrent {
		o.clearPendingLocked()
	}
	o.mu.Unlock()
}

func (o *SessionCoordinator) foregroundSnapshot() (string, pluginhost.SessionToken) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.foreground, o.foregroundSession
}

func (o *SessionCoordinator) inputForegroundSnapshot() (string, uint64, pluginhost.SessionToken) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.executing || o.criticalIdentity != "" {
		return "", 0, ""
	}
	return o.foreground, o.foregroundGeneration, o.foregroundSession
}

func (o *SessionCoordinator) beginExecution(ctx context.Context, pluginID string, request protocol.SessionExecutionRequest) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.criticalIdentity != "" {
		return protocol.NewDomainError(protocol.ErrorSessionCanceled, errors.New("critical presentation owns the foreground"))
	}
	if o.foregroundPlugin == pluginID && o.foreground == request.Instance.ID && o.foregroundSession == pluginhost.SessionToken(request.SessionToken) && o.foregroundGeneration != request.Instance.Generation {
		return protocol.NewDomainError(protocol.ErrorSessionGenerationMismatch, errors.New("session generation is no longer current"))
	}
	if o.executing || o.foregroundPlugin != pluginID || o.foreground != request.Instance.ID ||
		o.foregroundGeneration != request.Instance.Generation || o.foregroundSession != pluginhost.SessionToken(request.SessionToken) {
		return protocol.NewDomainError(protocol.ErrorSessionNotActive, errors.New("session is not the active interactive foreground"))
	}
	if err := ctx.Err(); err != nil {
		return protocol.NewDomainError(protocol.ErrorSessionCanceled, err)
	}
	o.clearPendingLocked()
	o.executing = true
	return nil
}

func (o *SessionCoordinator) acquireCritical(identity string) (sessionTermination, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.executing {
		return sessionTermination{}, false
	}
	if o.criticalIdentity != "" {
		o.criticalSequence++
		o.criticalIdentity = identity
		return sessionTermination{}, true
	}
	o.clearPendingLocked()
	termination := sessionTermination{pluginID: o.foregroundPlugin, instanceID: o.foreground, generation: o.foregroundGeneration, token: o.foregroundSession}
	o.clearForegroundLocked()
	o.criticalSequence++
	o.criticalIdentity = identity
	return termination, true
}

func (o *SessionCoordinator) beginLauncherAdmission() (uint64, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.criticalIdentity != "" || o.executing {
		return 0, false
	}
	return o.criticalSequence, true
}

func (o *SessionCoordinator) launcherAdmissionCurrent(sequence uint64) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.criticalIdentity == "" && !o.executing && o.criticalSequence == sequence
}

func (o *SessionCoordinator) releaseCritical() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.criticalIdentity == "" {
		return false
	}
	o.criticalIdentity = ""
	return true
}

func (o *SessionCoordinator) criticalOwned() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.criticalIdentity != ""
}

func (o *SessionCoordinator) blocksCritical() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.executing
}

func (o *SessionCoordinator) attentionState(record observation.Record) (foreground, suppressed bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	foreground = o.foreground == record.Observation.Instance.ID
	suppressed = o.foregroundSource != nil &&
		o.foregroundSource.pluginID == record.PluginID &&
		o.foregroundSource.instanceID == record.Observation.Instance.ID &&
		o.foregroundSource.channel == record.Observation.Channel &&
		o.foregroundSource.key == record.Observation.Key &&
		o.foregroundSource.generation == record.Generation &&
		o.foregroundSource.revision == record.Observation.Revision
	return foreground, suppressed
}

func (o *SessionCoordinator) lifecycleBaseline(pluginID string) uint64 {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.lifecycles[pluginID].sequence
}

func (o *SessionCoordinator) recordLifecycleFallback(pluginID string, baseline uint64, at time.Time) {
	if at.IsZero() {
		at = time.Now().UTC()
	} else {
		at = at.UTC()
	}
	o.mu.Lock()
	state := o.lifecycles[pluginID]
	if state.sequence <= baseline {
		state.errorCode = SessionEndFailedCode
		state.errorAt = at
		state.fallbackAfterSequence = baseline
		o.lifecycles[pluginID] = state
	}
	o.mu.Unlock()
}

// PluginSessionCleanup records authenticated supervisor cleanup state.
func (o *SessionCoordinator) PluginSessionCleanup(value pluginhost.SessionCleanup) {
	if value.PluginID == "" || value.Sequence == 0 {
		return
	}
	at := value.At.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	state := o.lifecycles[value.PluginID]
	if value.Sequence <= state.sequence || (state.fallbackAfterSequence != 0 && value.Sequence <= state.fallbackAfterSequence) {
		return
	}
	state.sequence = value.Sequence
	state.fallbackAfterSequence = 0
	if value.PendingCount > 0 && value.ErrorCode == pluginhost.ErrorCodeEndSessionFailed {
		state.errorCode = SessionEndFailedCode
		state.errorAt = at
	} else {
		state.errorCode = ""
		state.errorAt = time.Time{}
	}
	o.lifecycles[value.PluginID] = state
}

func (o *SessionCoordinator) BeginExecution(ctx context.Context, live *LiveState, pluginID string, request protocol.SessionExecutionRequest) error {
	if err := request.Validate(); err != nil {
		return protocol.NewDomainError(protocol.ErrorInvalidArgument, err)
	}
	live.mu.RLock()
	app, exists := live.document.Apps[request.Instance.ID]
	current := live.loaded && exists && app.Enabled && app.PluginID == pluginID &&
		live.generations.matches(pluginID, request.Instance.ID, request.Instance.Generation)
	live.mu.RUnlock()
	if !current {
		return protocol.NewDomainError(protocol.ErrorSessionGenerationMismatch, errors.New("session instance generation is no longer current"))
	}
	if err := o.beginExecution(ctx, pluginID, request); err != nil {
		return err
	}
	o.notifyChange(SessionChange{kind: sessionChanged})
	return nil
}

func (o *SessionCoordinator) PluginSessionInvalidated(value pluginhost.SessionInvalidation) {
	if !o.invalidate(value) {
		return
	}
	o.notifyChange(SessionChange{
		kind: sessionCanceled, pluginID: value.PluginID,
		target: protocol.InstanceRef{ID: value.InstanceID, Generation: value.Generation}, token: value.Token,
	})
}

func (o *SessionCoordinator) PluginSessionCompleted(live *LiveState, pluginID string, request protocol.CompleteSessionRequest) {
	if pluginID == "" || request.Instance.ID == "" || request.SessionToken == "" {
		return
	}
	token := pluginhost.SessionToken(request.SessionToken)
	live.mu.RLock()
	app, appExists := live.document.Apps[request.Instance.ID]
	pendingEligible := live.loaded && appExists && app.PluginID == pluginID &&
		live.generations.matches(pluginID, request.Instance.ID, request.Instance.Generation)
	live.mu.RUnlock()
	_, changed := o.complete(pluginID, request.Instance.ID, request.Instance.Generation, token, pendingEligible)
	if changed {
		o.notifyChange(SessionChange{kind: sessionCompleted, pluginID: pluginID, target: request.Instance, token: token})
	}
}

func (o *SessionCoordinator) Diagnostics() SessionDiagnostics {
	activeInstanceID, lifecycleCode, lifecycleAt := o.diagnostics()
	return SessionDiagnostics{
		ActiveInstanceID:       activeInstanceID,
		State:                  string(o.state()),
		LastLifecycleErrorCode: lifecycleCode,
		LastLifecycleErrorAt:   lifecycleAt,
	}
}

func (o *SessionCoordinator) diagnostics() (instanceID, code string, at time.Time) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	instanceID = o.foreground
	var selectedPlugin string
	for pluginID, state := range o.lifecycles {
		if state.errorCode == "" {
			continue
		}
		if at.IsZero() || state.errorAt.After(at) || (state.errorAt.Equal(at) && pluginID > selectedPlugin) {
			code = state.errorCode
			at = state.errorAt
			selectedPlugin = pluginID
		}
	}
	return instanceID, code, at
}

func (o *SessionCoordinator) state() foregroundState {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.stateLocked()
}

func (o *SessionCoordinator) stateLocked() foregroundState {
	switch {
	case o.criticalIdentity != "":
		return foregroundCriticalOwned
	case o.executing:
		return foregroundExecuting
	case o.pendingForeground != "":
		return foregroundPending
	case o.foreground != "":
		return foregroundInteractive
	default:
		return foregroundIdle
	}
}

func (o *SessionCoordinator) clearForegroundLocked() {
	o.foreground = ""
	o.foregroundPlugin = ""
	o.foregroundSession = ""
	o.foregroundGeneration = 0
	o.foregroundSource = nil
	o.executing = false
}

func (o *SessionCoordinator) pendingMatchesAdmission(admission sessionAdmission) bool {
	return o.pendingPlugin == admission.pluginID && o.pendingForeground == admission.instanceID && o.pendingGeneration == admission.generation && o.pendingSession == admission.token
}

func (o *SessionCoordinator) clearPendingLocked() {
	o.pendingForeground = ""
	o.pendingPlugin = ""
	o.pendingGeneration = 0
	o.pendingSession = ""
}
