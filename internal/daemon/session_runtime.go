package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/eventbus"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type sessionPluginController interface {
	EndSession(context.Context, string, protocol.InstanceRef, pluginhost.SessionToken) error
}

type sessionContextInvalidator func(context.Context) error

// SessionRuntime couples session state to the two effects required by terminal
// transitions: retiring queued input and ending the exact plugin session.
type SessionRuntime struct {
	state      *SessionCoordinator
	plugins    sessionPluginController
	inputs     SessionInputController
	invalidate sessionContextInvalidator
}

func NewSessionRuntime(state *SessionCoordinator, plugins sessionPluginController, inputs SessionInputController, invalidate sessionContextInvalidator) (*SessionRuntime, error) {
	if state == nil {
		return nil, errors.New("session coordinator is required")
	}
	if plugins == nil {
		return nil, errors.New("session plugin controller is required")
	}
	if inputs == nil {
		return nil, errors.New("session input controller is required")
	}
	if invalidate == nil {
		return nil, errors.New("session context invalidator is required")
	}
	return &SessionRuntime{state: state, plugins: plugins, inputs: inputs, invalidate: invalidate}, nil
}

func (r *SessionRuntime) Foreground() string {
	foreground, _ := r.state.foregroundSnapshot()
	return foreground
}

func (r *SessionRuntime) ForegroundSession() (string, string) {
	foreground, _, token := r.state.inputForegroundSnapshot()
	return foreground, string(token)
}

func (r *SessionRuntime) ForegroundSessionRef() (protocol.InstanceRef, string) {
	instanceID, generation, token := r.state.inputForegroundSnapshot()
	return protocol.InstanceRef{ID: instanceID, Generation: generation}, string(token)
}

func (r *SessionRuntime) BeginLauncherAdmission() (uint64, bool) {
	return r.state.beginLauncherAdmission()
}

func (r *SessionRuntime) LauncherAdmissionCurrent(sequence uint64) bool {
	return r.state.launcherAdmissionCurrent(sequence)
}

func (r *SessionRuntime) AcquireCritical(ctx context.Context, candidate presentation.Candidate) bool {
	termination, acquired := r.state.acquireCritical(candidate.ID())
	if !acquired {
		return false
	}
	_ = r.invalidate(ctx)
	if termination.pluginID != "" {
		_ = r.endSession(ctx, termination)
	}
	return true
}

func (r *SessionRuntime) ReleaseCritical() {
	r.state.releaseCritical()
}

func (r *SessionRuntime) CriticalPresentationOwned() bool {
	return r.state.criticalOwned()
}

func (r *SessionRuntime) ClearForeground(ctx context.Context, expected string, expectedToken pluginhost.SessionToken) bool {
	termination, changed := r.state.clear(expected, expectedToken)
	if changed && termination.pluginID != "" {
		_ = r.endSession(ctx, termination)
	}
	return changed
}

func (r *SessionRuntime) ClearForegroundContext(ctx context.Context, expected string) {
	r.ClearForeground(ctx, expected, "")
}

func (r *SessionRuntime) ClearForegroundSessionContext(ctx context.Context, expected, token string) {
	if expected == "" || token == "" {
		return
	}
	r.ClearForeground(ctx, expected, pluginhost.SessionToken(token))
}

func (r *SessionRuntime) Detach(ctx context.Context, app config.App) bool {
	termination, changed := r.state.detach(app)
	if termination.pluginID != "" {
		_ = r.endSession(ctx, termination)
	}
	return changed
}

func (r *SessionRuntime) EndSession(ctx context.Context, termination sessionTermination) error {
	return r.endSession(ctx, termination)
}

func (r *SessionRuntime) endSession(ctx context.Context, termination sessionTermination) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if termination.pluginID == "" || termination.instanceID == "" || termination.generation == 0 || termination.token == "" {
		return nil
	}
	target := protocol.InstanceRef{ID: termination.instanceID, Generation: termination.generation}
	r.inputs.Cancel(termination.pluginID, target, string(termination.token))
	baseline := r.state.lifecycleBaseline(termination.pluginID)
	callCtx, cancel := context.WithTimeout(ctx, sessionEndTimeout)
	err := r.plugins.EndSession(callCtx, termination.pluginID, target, termination.token)
	cancel()
	if err != nil {
		r.state.recordLifecycleFallback(termination.pluginID, baseline, time.Now().UTC())
	}
	return err
}

func (r *SessionRuntime) ApplyConfiguration(document config.Document) {
	r.state.configurationChanged(document)
	r.inputs.Apply(sessionInputTargets(document))
}

func (r *SessionRuntime) InputStatus() []eventbus.Status {
	return r.inputs.Status()
}

func (r *SessionRuntime) Diagnostics() SessionDiagnostics {
	return r.state.Diagnostics()
}

// Launch admission stays under the same session owner as terminal effects.
func (r *SessionRuntime) serializeLaunch() func() {
	return r.state.serializeLaunch()
}

func (r *SessionRuntime) begin(pluginID, instanceID string, generation uint64) (sessionAdmission, bool) {
	return r.state.begin(pluginID, instanceID, generation)
}

func (r *SessionRuntime) cancel(admission sessionAdmission) {
	r.state.cancel(admission)
}

func (r *SessionRuntime) promote(admission sessionAdmission, pluginID string, source *observationSessionSource) (sessionTermination, bool) {
	return r.state.promote(admission, pluginID, source)
}

// Detachment returns terminal work so reconciliation can preserve its effect
// ordering without reaching into the coordinator.
func (r *SessionRuntime) detach(app config.App) (sessionTermination, bool) {
	return r.state.detach(app)
}
