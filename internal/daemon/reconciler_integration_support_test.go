package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/eventbus"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"sync"
	"testing"
	"time"
)

type observationDiagnostics struct {
	store    observation.StoreDiagnostics
	state    AttentionStateDiagnostics
	selected observation.Record
}

func (d *observationDiagnostics) AttentionSnapshot() (attention.Trace, bool) {
	return attention.Trace{}, false
}

func (d *observationDiagnostics) AttentionExplain(string) (attention.Evaluation, bool) {
	return attention.Evaluation{}, false
}

func (d *observationDiagnostics) AttentionHistory(int, time.Time) []attention.Trace { return nil }

func (*observationDiagnostics) AcknowledgeAttention(string) error { return nil }

func (*observationDiagnostics) Wake() {}

func (*observationDiagnostics) Reconcile(context.Context) error { return nil }

func (*observationDiagnostics) RecorderStatus() attention.RecorderStatus {
	return attention.RecorderStatus{Phase: attention.RecorderUnavailable}
}

func (d *observationDiagnostics) ObservationDiagnostics() observation.StoreDiagnostics {
	return d.store
}

func (d *observationDiagnostics) AttentionStateStatus() AttentionStateDiagnostics { return d.state }

func (*observationDiagnostics) PresentationCooldownStatus() PresentationCooldownDiagnostics {
	return PresentationCooldownDiagnostics{}
}

func (d *observationDiagnostics) SelectedObservation() (observation.Record, bool) {
	return d.selected, d.selected.PluginID != ""
}

type orderedServiceLog struct {
	mu     sync.Mutex
	values []string
}

func (l *orderedServiceLog) add(value string) {
	l.mu.Lock()
	l.values = append(l.values, value)
	l.mu.Unlock()
}

func (l *orderedServiceLog) reset() { l.mu.Lock(); l.values = nil; l.mu.Unlock() }

func (l *orderedServiceLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.values...)
}

func equalServiceOperations(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

type orderedPluginController struct {
	*fakePluginController
	log *orderedServiceLog
}

func (c *orderedPluginController) Apply(ctx context.Context, specs []pluginhost.Spec) error {
	c.log.add("plugin")
	return c.fakePluginController.Apply(ctx, specs)
}

type canceledSessionInput struct {
	pluginID string
	target   protocol.InstanceRef
	token    string
}

type recordingSessionInputController struct {
	mu        sync.Mutex
	canceled  []canceledSessionInput
	completed []canceledSessionInput
}

func (c *recordingSessionInputController) Complete(pluginID string, target protocol.InstanceRef, token string) {
	c.mu.Lock()
	c.completed = append(c.completed, canceledSessionInput{pluginID: pluginID, target: target, token: token})
	c.mu.Unlock()
}

func (*recordingSessionInputController) Apply([]eventbus.TargetSet) {}

func (*recordingSessionInputController) Status() []eventbus.Status { return nil }

func (c *recordingSessionInputController) Cancel(pluginID string, target protocol.InstanceRef, token string) {
	c.mu.Lock()
	c.canceled = append(c.canceled, canceledSessionInput{pluginID: pluginID, target: target, token: token})
	c.mu.Unlock()
}

func (c *recordingSessionInputController) completedSnapshot() []canceledSessionInput {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]canceledSessionInput(nil), c.completed...)
}

func (c *recordingSessionInputController) snapshot() []canceledSessionInput {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]canceledSessionInput(nil), c.canceled...)
}

type fakePluginController struct {
	lastSpecs     []pluginhost.Spec
	invoked       pluginhost.InvokeRequest
	invocations   int
	endedPlugin   string
	endedInstance string
	invokedToken  pluginhost.SessionToken
}

type safePluginController struct {
	mu        sync.Mutex
	lastSpecs []pluginhost.Spec
	invoked   []pluginhost.InvokeRequest
	applyWake chan struct{}
	isClosed  bool
}

type scriptedPluginController struct {
	*safePluginController
	muScript sync.Mutex
	errors   []error
}

type blockingApplyController struct {
	*safePluginController
	muCalls    sync.Mutex
	callCount  int
	oldStarted chan struct{}
	oldRelease chan struct{}
}

func newBlockingApplyController() *blockingApplyController {
	return &blockingApplyController{safePluginController: &safePluginController{}, oldStarted: make(chan struct{}, 1), oldRelease: make(chan struct{})}
}

func (f *blockingApplyController) Apply(ctx context.Context, specs []pluginhost.Spec) error {
	f.muCalls.Lock()
	f.callCount++
	call := f.callCount
	f.muCalls.Unlock()
	if call == 2 {
		f.oldStarted <- struct{}{}
		<-f.oldRelease
	}
	return f.safePluginController.Apply(ctx, specs)
}

func (f *blockingApplyController) calls() int {
	f.muCalls.Lock()
	defer f.muCalls.Unlock()
	return f.callCount
}

func newScriptedPluginController(errors ...error) *scriptedPluginController {
	return &scriptedPluginController{safePluginController: &safePluginController{}, errors: append([]error(nil), errors...)}
}

func (f *scriptedPluginController) Apply(ctx context.Context, specs []pluginhost.Spec) error {
	if err := f.safePluginController.Apply(ctx, specs); err != nil {
		return err
	}
	f.muScript.Lock()
	defer f.muScript.Unlock()
	if len(f.errors) == 0 {
		return nil
	}
	err := f.errors[0]
	f.errors = f.errors[1:]
	return err
}

func (f *safePluginController) Apply(_ context.Context, specs []pluginhost.Spec) error {
	f.mu.Lock()
	f.lastSpecs = append([]pluginhost.Spec(nil), specs...)
	f.mu.Unlock()
	if f.applyWake != nil {
		select {
		case f.applyWake <- struct{}{}:
		default:
		}
	}
	return nil
}

func (f *safePluginController) Invoke(_ context.Context, _ string, request pluginhost.InvokeRequest, _ pluginhost.InvocationKind, _ pluginhost.SessionToken) error {
	f.mu.Lock()
	f.invoked = append(f.invoked, request)
	f.mu.Unlock()
	return nil
}

func (*safePluginController) EndSession(context.Context, string, protocol.InstanceRef, pluginhost.SessionToken) error {
	return nil
}

func (*safePluginController) Status() []pluginhost.PluginStatus { return nil }

func (*safePluginController) Operation(context.Context, string, protocol.OperationRequest) (protocol.OperationResult, error) {
	return protocol.OperationResult{}, errors.New("unexpected plugin operation")
}

func (f *safePluginController) Close(context.Context) error {
	f.mu.Lock()
	f.isClosed = true
	f.mu.Unlock()
	return nil
}

func (f *safePluginController) specs() []pluginhost.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pluginhost.Spec(nil), f.lastSpecs...)
}

func (f *safePluginController) closed() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.isClosed }

func (f *safePluginController) invocationRequests() []pluginhost.InvokeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pluginhost.InvokeRequest(nil), f.invoked...)
}

func awaitCondition(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatalf("timed out waiting for %s", description)
	}
}

func (f *fakePluginController) Apply(_ context.Context, specs []pluginhost.Spec) error {
	f.lastSpecs = specs
	return nil
}

func (f *fakePluginController) Invoke(_ context.Context, _ string, request pluginhost.InvokeRequest, _ pluginhost.InvocationKind, token pluginhost.SessionToken) error {
	f.invocations++
	f.invoked = request
	f.invokedToken = token
	return nil
}

func (f *fakePluginController) EndSession(_ context.Context, pluginID string, target protocol.InstanceRef, _ pluginhost.SessionToken) error {
	f.endedPlugin = pluginID
	f.endedInstance = target.ID
	return nil
}

func (f *fakePluginController) Status() []pluginhost.PluginStatus { return nil }

func (*fakePluginController) Operation(context.Context, string, protocol.OperationRequest) (protocol.OperationResult, error) {
	return protocol.OperationResult{}, errors.New("unexpected plugin operation")
}

func (f *fakePluginController) Close(context.Context) error { return nil }

type fakeAssetController struct{ ready bool }

func (f *fakeAssetController) Reconcile(context.Context, []assets.Package) {}

func (*fakeAssetController) CollectGarbage(context.Context, []assets.Package) {}

func (f *fakeAssetController) Ready(string) bool { return f.ready }

func (f *fakeAssetController) ReadyFor(assets.Package) bool { return f.ready }

func (f *fakeAssetController) Status() []assets.State {
	return []assets.State{{PluginID: "plugin", Phase: assets.PhasePending}}
}

type lifecyclePluginController struct {
	mu                sync.Mutex
	invoked           []pluginhost.SessionToken
	ended             []pluginhost.SessionToken
	deadline          time.Time
	endErr            error
	internalCleanupAt time.Time
	pluginStatus      pluginhost.PluginStatus
}

func (*lifecyclePluginController) Apply(context.Context, []pluginhost.Spec) error { return nil }

func (f *lifecyclePluginController) Invoke(_ context.Context, _ string, _ pluginhost.InvokeRequest, _ pluginhost.InvocationKind, token pluginhost.SessionToken) error {
	f.mu.Lock()
	f.invoked = append(f.invoked, token)
	if len(f.invoked) == 2 && !f.internalCleanupAt.IsZero() {
		f.pluginStatus = pluginhost.PluginStatus{
			ID: "plugin", Running: true, SessionLifecycleErrorCode: pluginhost.ErrorCodeEndSessionFailed, SessionLifecycleErrorAt: f.internalCleanupAt,
		}
	}
	f.mu.Unlock()
	return nil
}

func (f *lifecyclePluginController) EndSession(ctx context.Context, _ string, _ protocol.InstanceRef, token pluginhost.SessionToken) error {
	deadline, _ := ctx.Deadline()
	f.mu.Lock()
	f.ended = append(f.ended, token)
	f.deadline = deadline
	err := f.endErr
	f.mu.Unlock()
	return err
}

func (f *lifecyclePluginController) Status() []pluginhost.PluginStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pluginStatus.ID == "" {
		return nil
	}
	return []pluginhost.PluginStatus{f.pluginStatus}
}

func (*lifecyclePluginController) Close(context.Context) error { return nil }

func (*lifecyclePluginController) Operation(context.Context, string, protocol.OperationRequest) (protocol.OperationResult, error) {
	return protocol.OperationResult{}, errors.New("unexpected plugin operation")
}

func (f *lifecyclePluginController) snapshot() ([]pluginhost.SessionToken, []pluginhost.SessionToken, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pluginhost.SessionToken(nil), f.invoked...), append([]pluginhost.SessionToken(nil), f.ended...), f.deadline
}

func awaitServiceSignal(t *testing.T, values <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-values:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func serviceDocument(enabled bool) config.Document {
	return config.Document{
		Version: config.CurrentVersion, Generation: 1,
		Plugins: map[string]config.Plugin{"plugin": {
			ID: "plugin", Version: "1", Executable: "/plugin",
			ProtocolVersion: protocol.Version, ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
			Channels: []protocol.Channel{{ID: "answer"}},
		}},
		Apps: map[string]config.App{"ball8": {
			ID: "ball8", PluginID: "plugin", Generation: 1, Enabled: enabled, LaunchAction: "ask", Config: json.RawMessage(`{}`),
			Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}},
		}},
	}
}
