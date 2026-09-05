package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lxdb/bsbctl/internal/checkpoint"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/device"
	busyinput "github.com/lxdb/bsbctl/internal/input"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/pluginlog"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRuntimeStatusIncludesObservationStoreHealth(t *testing.T) {
	want := observation.StoreDiagnostics{
		LiveCount: 17, CapacityRejections: 3,
		LastRejectionAt:   time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC),
		LastRejectionCode: observation.CapacityRejectionCode,
	}
	wantState := AttentionStateDiagnostics{
		LastShownEntries: 23, AcknowledgementEntries: 7,
		LastShownCapacityEvictions: 11, AcknowledgementCapacityEvictions: 5,
	}
	options := newTestRuntimeStatusOptions(t)
	options.Attention = &observationDiagnostics{store: want, state: wantState}
	status, err := NewRuntimeStatus(options)
	if err != nil {
		t.Fatal(err)
	}
	if got := status.RuntimeDiagnostics().Observations; got != want {
		t.Fatalf("observation diagnostics = %#v, want %#v", got, want)
	}
	if got := status.RuntimeDiagnostics().AttentionState; got != wantState {
		t.Fatalf("attention state diagnostics = %#v, want %#v", got, wantState)
	}
}

func TestRuntimeStatusKeepsOutputAndAudioOwnershipSeparate(t *testing.T) {
	options := newTestRuntimeStatusOptions(t)
	output := staticOutputDiagnostics{status: device.OutputStatus{Phase: device.OutputBusy, QueueDepth: 3}}
	audio := staticAudioDiagnostics{status: device.AudioStatus{Attempts: 4, LastErrorCode: "audio_play_failed"}}
	options.Output, options.Audio = output, audio
	status, err := NewRuntimeStatus(options)
	if err != nil {
		t.Fatal(err)
	}

	diagnostics := status.RuntimeDiagnostics()
	if diagnostics.Output != output.status || diagnostics.Audio != audio.status {
		t.Fatalf("runtime diagnostics = output %#v audio %#v", diagnostics.Output, diagnostics.Audio)
	}
}

func TestRuntimeStatusRejectsEveryMissingDependency(t *testing.T) {
	options := newTestRuntimeStatusOptions(t)
	if _, err := NewRuntimeStatus(options); err != nil {
		t.Fatalf("complete runtime diagnostic sources: %v", err)
	}
	tests := []struct {
		name   string
		remove func(*RuntimeStatusOptions)
	}{
		{name: "live", remove: func(o *RuntimeStatusOptions) { o.Live = nil }},
		{name: "plugins", remove: func(o *RuntimeStatusOptions) { o.Plugins = nil }},
		{name: "assets", remove: func(o *RuntimeStatusOptions) { o.Assets = nil }},
		{name: "sessions", remove: func(o *RuntimeStatusOptions) { o.Sessions = nil }},
		{name: "attention", remove: func(o *RuntimeStatusOptions) { o.Attention = nil }},
		{name: "configuration", remove: func(o *RuntimeStatusOptions) { o.Configuration = nil }},
		{name: "checkpoints", remove: func(o *RuntimeStatusOptions) { o.Checkpoints = nil }},
		{name: "input", remove: func(o *RuntimeStatusOptions) { o.Input = nil }},
		{name: "device", remove: func(o *RuntimeStatusOptions) { o.Device = nil }},
		{name: "output", remove: func(o *RuntimeStatusOptions) { o.Output = nil }},
		{name: "audio", remove: func(o *RuntimeStatusOptions) { o.Audio = nil }},
		{name: "logs", remove: func(o *RuntimeStatusOptions) { o.Logs = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			incomplete := options
			test.remove(&incomplete)
			if _, err := NewRuntimeStatus(incomplete); err == nil {
				t.Fatal("NewRuntimeStatus accepted a missing dependency")
			}
		})
	}
}

func newTestRuntimeStatusOptions(t *testing.T) RuntimeStatusOptions {
	t.Helper()
	live := NewLiveState()
	desired, err := NewDesiredState(config.NewStore(filepath.Join(t.TempDir(), "config.json")), nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := NewCheckpoints(checkpoint.NewStore(filepath.Join(t.TempDir(), "checkpoints")), live)
	if err != nil {
		t.Fatal(err)
	}
	plugins := &safePluginController{}
	sessions, err := NewSessionRuntime(NewSessionCoordinator(nil), plugins, &recordingSessionInputController{}, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return RuntimeStatusOptions{
		Live: live, Plugins: plugins, Assets: &fakeAssetController{ready: true}, Sessions: sessions,
		Attention: &observationDiagnostics{}, Configuration: desired, Checkpoints: checkpoints,
		Input: staticInputDiagnostics{}, Device: staticDeviceDiagnostics{}, Output: staticOutputDiagnostics{},
		Audio: staticAudioDiagnostics{}, Logs: staticLogDiagnostics{},
	}
}

func TestSessionDiagnosticsJSONOmitsUnobservedLifecycleTime(t *testing.T) {
	encoded, err := json.Marshal(SessionDiagnostics{ActiveInstanceID: "ball8"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"active_instance_id":"ball8","state":"idle"}`; got != want {
		t.Fatalf("session diagnostics JSON = %s, want %s", got, want)
	}
}

type staticOutputDiagnostics struct{ status device.OutputStatus }

func (d staticOutputDiagnostics) Status() device.OutputStatus { return d.status }

type staticAudioDiagnostics struct{ status device.AudioStatus }

func (d staticAudioDiagnostics) Status() device.AudioStatus { return d.status }

type staticInputDiagnostics struct{}

func (staticInputDiagnostics) Status() busyinput.DispatcherStatus {
	return busyinput.DispatcherStatus{}
}

type staticDeviceDiagnostics struct{}

func (staticDeviceDiagnostics) Status() device.RuntimeStatus { return device.RuntimeStatus{} }

type staticLogDiagnostics struct{}

func (staticLogDiagnostics) Status() pluginlog.Status { return pluginlog.Status{} }

func TestReconcilerEndSessionFailureIsBoundedSafeLifecycleHealth(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &lifecyclePluginController{endErr: errors.New("token=secret raw child failure")}
	service := newTestReconciler(t, store, nil, plugins)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", json.RawMessage(`{"secret":"payload"}`)); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatalf("replacement launch became cleanup failure: %v", err)
	}
	_, _, deadline := plugins.snapshot()
	if deadline.IsZero() || time.Until(deadline) <= 0 {
		t.Fatalf("end-session deadline = %v, want live bounded context", deadline)
	}
	status := service.sessions.Diagnostics()
	if status.ActiveInstanceID != "ball8" || status.LastLifecycleErrorCode != SessionEndFailedCode || status.LastLifecycleErrorAt.IsZero() {
		t.Fatalf("session diagnostics = %#v", status)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if containsAny(string(encoded), "interactive-", "secret", "payload", "child failure") {
		t.Fatalf("session diagnostics leaked lifecycle internals: %s", encoded)
	}
}

func TestReconcilerSessionCleanupCallbacksRetainOtherPluginFailures(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	service := newTestReconciler(t, store, nil, &safePluginController{})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	firstAt := time.Date(2026, 8, 21, 10, 10, 0, 0, time.UTC)
	secondAt := firstAt.Add(time.Minute)
	service.sessions.state.PluginSessionCleanup(pluginhost.SessionCleanup{PluginID: "first", Sequence: 1, At: firstAt, PendingCount: 1, ErrorCode: pluginhost.ErrorCodeEndSessionFailed})
	service.sessions.state.PluginSessionCleanup(pluginhost.SessionCleanup{PluginID: "second", Sequence: 1, At: secondAt, PendingCount: 2, ErrorCode: pluginhost.ErrorCodeEndSessionFailed})
	status := service.sessions.Diagnostics()
	if status.LastLifecycleErrorCode != SessionEndFailedCode || !status.LastLifecycleErrorAt.Equal(secondAt) {
		t.Fatalf("latest active plugin failure = %#v", status)
	}

	service.sessions.state.PluginSessionCleanup(pluginhost.SessionCleanup{PluginID: "second", Sequence: 2, At: secondAt.Add(time.Minute)})
	status = service.sessions.Diagnostics()
	if status.LastLifecycleErrorCode != SessionEndFailedCode || !status.LastLifecycleErrorAt.Equal(firstAt) {
		t.Fatalf("second recovery cleared first failure: %#v", status)
	}
	service.sessions.state.PluginSessionCleanup(pluginhost.SessionCleanup{PluginID: "first", Sequence: 1, At: secondAt.Add(2 * time.Minute)})
	status = service.sessions.Diagnostics()
	if status.LastLifecycleErrorCode != SessionEndFailedCode || !status.LastLifecycleErrorAt.Equal(firstAt) {
		t.Fatalf("stale first recovery changed lifecycle state: %#v", status)
	}
	service.sessions.state.PluginSessionCleanup(pluginhost.SessionCleanup{PluginID: "first", Sequence: 2, At: secondAt.Add(3 * time.Minute)})
	status = service.sessions.Diagnostics()
	if status.LastLifecycleErrorCode != "" || !status.LastLifecycleErrorAt.IsZero() {
		t.Fatalf("all plugin recoveries left lifecycle failure: %#v", status)
	}
}

func TestReconcilerEndSessionTimeoutFallbackRequiresLaterCleanupSequenceToClear(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &lifecyclePluginController{endErr: context.DeadlineExceeded}
	service := newTestReconciler(t, store, nil, plugins)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	clearAt := time.Date(2026, 8, 21, 10, 20, 0, 0, time.UTC)
	service.sessions.state.PluginSessionCleanup(pluginhost.SessionCleanup{PluginID: "plugin", Sequence: 10, At: clearAt})
	service.ClearForegroundContext(context.Background(), "ball8")
	status := service.sessions.Diagnostics()
	if status.LastLifecycleErrorCode != SessionEndFailedCode || status.LastLifecycleErrorAt.IsZero() {
		t.Fatalf("caller-timeout fallback = %#v", status)
	}

	service.sessions.state.PluginSessionCleanup(pluginhost.SessionCleanup{PluginID: "plugin", Sequence: 10, At: clearAt.Add(time.Minute)})
	status = service.sessions.Diagnostics()
	if status.LastLifecycleErrorCode != SessionEndFailedCode {
		t.Fatalf("same-sequence callback cleared timeout fallback: %#v", status)
	}
	service.sessions.state.PluginSessionCleanup(pluginhost.SessionCleanup{PluginID: "plugin", Sequence: 11, At: clearAt.Add(2 * time.Minute)})
	status = service.sessions.Diagnostics()
	if status.LastLifecycleErrorCode != "" || !status.LastLifecycleErrorAt.IsZero() {
		t.Fatalf("later supervisor recovery did not clear fallback: %#v", status)
	}
}

func TestReconcilerCapturesPluginhostReplacementCleanupCallback(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	failedAt := time.Date(2026, 8, 21, 8, 30, 0, 0, time.UTC)
	plugins := &lifecyclePluginController{internalCleanupAt: failedAt}
	service := newTestReconciler(t, store, nil, plugins)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatalf("replacement launch became pluginhost cleanup failure: %v", err)
	}
	service.sessions.state.PluginSessionCleanup(pluginhost.SessionCleanup{
		PluginID: "plugin", Sequence: 1, At: failedAt,
		PendingCount: 1, ErrorCode: pluginhost.ErrorCodeEndSessionFailed,
	})
	status := service.sessions.Diagnostics()
	if status.LastLifecycleErrorCode != SessionEndFailedCode || !status.LastLifecycleErrorAt.Equal(failedAt) || status.ActiveInstanceID != "ball8" {
		t.Fatalf("session diagnostics = %#v", status)
	}
}

func TestReconcilerManagerCompositionRetainsAndRetriesExactReplacementCleanup(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	clock := newServiceManagerClock(time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC))
	child := newServiceManagerChild(errors.New("replacement cleanup failed"))
	var service *Reconciler
	manager := pluginhost.NewManager("test", func(context.Context, string, pluginhost.Spec, pluginhost.Callbacks) (pluginhost.Child, error) {
		return child, nil
	}, pluginhost.Callbacks{
		SessionInvalidated: func(value pluginhost.SessionInvalidation) {
			if service != nil {
				service.sessions.state.PluginSessionInvalidated(value)
			}
		},
		SessionCleanup: func(value pluginhost.SessionCleanup) {
			if service != nil {
				service.sessions.state.PluginSessionCleanup(value)
			}
		},
	}, pluginhost.WithClock(clock), pluginhost.WithJitter(func() float64 { return 1 }), pluginhost.WithHealthInterval(time.Hour))
	service = newTestReconciler(t, store, nil, manager)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatalf("replacement launch became cleanup failure: %v", err)
	}
	ended := child.endedSessions()
	if len(ended) != 1 || ended[0].SessionToken != "interactive-1" {
		t.Fatalf("replacement cleanup calls = %#v, want exact old token once", ended)
	}
	failed := service.sessions.Diagnostics()
	if failed.LastLifecycleErrorCode != SessionEndFailedCode || failed.LastLifecycleErrorAt.IsZero() {
		t.Fatalf("pending cleanup was not stably visible: %#v", failed)
	}

	child.setEndSessionError(nil)
	clock.FireNext(t)
	awaitCondition(t, time.Second, func() bool { return len(child.endedSessions()) == 2 }, "supervised exact-token cleanup retry")
	awaitCondition(t, time.Second, func() bool {
		status := service.sessions.Diagnostics()
		return status.LastLifecycleErrorCode == "" && status.LastLifecycleErrorAt.IsZero()
	}, "daemon session lifecycle recovery")
	ended = child.endedSessions()
	if ended[1] != ended[0] {
		t.Fatalf("supervised retry changed cleanup identity: %#v", ended)
	}
}

func TestReconcilerManagerCleanupCapRejectsProposedSessionAndInvalidatesPriorOwner(t *testing.T) {
	document := serviceDocument(true)
	plugin := document.Plugins["plugin"]
	plugin.ExecutionModes = append(plugin.ExecutionModes, protocol.ExecutionModeResident)
	document.Plugins["plugin"] = plugin
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	first := newServiceManagerChild(errors.New("persistent cleanup failure"))
	second := newServiceManagerChild(nil)
	children := []pluginhost.Child{first, second}
	starts := 0
	invalidated := make(chan pluginhost.SessionInvalidation, 1)
	var service *Reconciler
	manager := pluginhost.NewManager("test", func(context.Context, string, pluginhost.Spec, pluginhost.Callbacks) (pluginhost.Child, error) {
		child := children[starts]
		starts++
		return child, nil
	}, pluginhost.Callbacks{
		SessionInvalidated: func(value pluginhost.SessionInvalidation) {
			if service != nil {
				service.sessions.state.PluginSessionInvalidated(value)
			}
			invalidated <- value
		},
		SessionCleanup: func(value pluginhost.SessionCleanup) {
			if service != nil {
				service.sessions.state.PluginSessionCleanup(value)
			}
		},
	})
	service = newTestReconciler(t, store, nil, manager)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 256; index++ {
		if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
			t.Fatalf("replacement %d: %v", index+1, err)
		}
	}
	if got := service.Foreground(); got != "ball8" {
		t.Fatalf("foreground before overflow = %q", got)
	}
	err := service.Launch(context.Background(), "ball8", "", nil)
	if !errors.Is(err, pluginhost.ErrPluginUnavailable) {
		t.Fatalf("overflow launch = %v, want stable unavailable failure", err)
	}
	service.sessions.state.mu.RLock()
	foreground, token, pending := service.sessions.state.foreground, service.sessions.state.foregroundSession, service.sessions.state.pendingSession
	service.sessions.state.mu.RUnlock()
	if foreground != "" || token != "" || pending != "" {
		t.Fatalf("overflow left ghost core ownership: foreground=%q token=%q pending=%q", foreground, token, pending)
	}
	if starts != 2 || !first.stopped() {
		t.Fatalf("overflow fail-closed restart = starts %d stopped %t", starts, first.stopped())
	}
	select {
	case value := <-invalidated:
		if value.PluginID != "plugin" || value.InstanceID != "ball8" || value.Token != "interactive-257" || value.Reason != pluginhost.SessionInvalidatedReplaced {
			t.Fatalf("overflow prior-owner invalidation = %#v", value)
		}
	case <-time.After(time.Second):
		t.Fatal("overflow did not invalidate the exact prior Core owner")
	}
}

func TestReconcilerManagerQueuedEndSessionTimeoutRemainsVisibleUntilSupervisedRetry(t *testing.T) {
	document := serviceDocument(true)
	plugin := document.Plugins["plugin"]
	plugin.ExecutionModes = append(plugin.ExecutionModes, protocol.ExecutionModeResident)
	document.Plugins["plugin"] = plugin
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	clock := newServiceManagerClock(time.Date(2026, 8, 21, 10, 5, 0, 0, time.UTC))
	child := newServiceManagerChild(nil)
	eventRelease := make(chan struct{})
	child.eventStart = make(chan struct{}, 1)
	child.eventBlock = eventRelease
	var service *Reconciler
	manager := pluginhost.NewManager("test", func(context.Context, string, pluginhost.Spec, pluginhost.Callbacks) (pluginhost.Child, error) {
		return child, nil
	}, pluginhost.Callbacks{
		SessionInvalidated: func(value pluginhost.SessionInvalidation) {
			if service != nil {
				service.sessions.state.PluginSessionInvalidated(value)
			}
		},
		SessionCleanup: func(value pluginhost.SessionCleanup) {
			if service != nil {
				service.sessions.state.PluginSessionCleanup(value)
			}
		},
	}, pluginhost.WithClock(clock), pluginhost.WithJitter(func() float64 { return 1 }), pluginhost.WithHealthInterval(time.Hour))
	service = newTestReconciler(t, store, nil, manager)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	ref, sessionToken := service.ForegroundSessionRef()
	eventDone := make(chan error, 1)
	go func() {
		_, err := manager.SessionInputResult(context.Background(), "plugin", protocol.SessionInputRequest{
			Sequence: 1, OccurredAt: time.Now().UTC(), Instance: ref, SessionToken: sessionToken,
			Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonOK, Action: protocol.ButtonPress}},
		})
		eventDone <- err
	}()
	select {
	case <-child.eventStart:
	case <-time.After(time.Second):
		t.Fatal("manager did not enter blocking child SessionInput")
	}

	deadlineCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	service.ClearForegroundContext(deadlineCtx, "ball8")
	cancel()
	fallback := service.sessions.Diagnostics()
	if fallback.ActiveInstanceID != "" || fallback.LastLifecycleErrorCode != SessionEndFailedCode || fallback.LastLifecycleErrorAt.IsZero() {
		t.Fatalf("queued caller-timeout diagnostics = %#v", fallback)
	}
	close(eventRelease)
	select {
	case err := <-eventDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocking manager Event did not finish")
	}
	awaitCondition(t, time.Second, func() bool {
		return service.sessions.Diagnostics().LastLifecycleErrorCode == SessionEndFailedCode
	}, "supervisor adoption of queued timeout cleanup")
	if got := len(child.endedSessions()); got != 0 {
		t.Fatalf("expired queued cleanup made %d immediate child calls, want 0", got)
	}

	clock.FireNext(t)
	awaitCondition(t, time.Second, func() bool { return len(child.endedSessions()) == 1 }, "supervised queued-timeout cleanup retry")
	awaitCondition(t, time.Second, func() bool {
		status := service.sessions.Diagnostics()
		return status.LastLifecycleErrorCode == "" && status.LastLifecycleErrorAt.IsZero()
	}, "supervised queued-timeout lifecycle recovery")
	ended := child.endedSessions()
	if ended[0].InstanceID != "ball8" || ended[0].SessionToken != "interactive-1" {
		t.Fatalf("supervised queued-timeout cleanup = %#v", ended)
	}
}

type serviceManagerChild struct {
	mu          sync.Mutex
	done        chan error
	stopOnce    sync.Once
	endErr      error
	endSessions []pluginhost.EndSessionRequest
	isStopped   bool
	eventStart  chan struct{}
	eventBlock  <-chan struct{}
}

func newServiceManagerChild(endErr error) *serviceManagerChild {
	return &serviceManagerChild{done: make(chan error), endErr: endErr}
}

func (*serviceManagerChild) Invoke(context.Context, pluginhost.InvokeRequest) error { return nil }

func (c *serviceManagerChild) EndSession(_ context.Context, request pluginhost.EndSessionRequest) error {
	c.mu.Lock()
	c.endSessions = append(c.endSessions, request)
	err := c.endErr
	c.mu.Unlock()
	return err
}

func (*serviceManagerChild) ReplaceInstances(context.Context, []pluginhost.Instance) error {
	return nil
}

func (c *serviceManagerChild) SessionInput(context.Context, protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	c.mu.Lock()
	started, blocked := c.eventStart, c.eventBlock
	c.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if blocked != nil {
		<-blocked
	}
	return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, nil
}

func (*serviceManagerChild) Operation(context.Context, protocol.OperationRequest) (protocol.OperationResult, error) {
	return protocol.OperationResult{Payload: json.RawMessage(`{}`)}, nil
}

func (*serviceManagerChild) Ping(context.Context) (protocol.HealthResult, error) {
	return protocol.HealthResult{Healthy: true}, nil
}

func (c *serviceManagerChild) Done() <-chan error { return c.done }

func (c *serviceManagerChild) Stop(context.Context) error {
	c.mu.Lock()
	c.isStopped = true
	c.mu.Unlock()
	c.stopOnce.Do(func() { close(c.done) })
	return nil
}

func (c *serviceManagerChild) setEndSessionError(err error) {
	c.mu.Lock()
	c.endErr = err
	c.mu.Unlock()
}

func (c *serviceManagerChild) endedSessions() []pluginhost.EndSessionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]pluginhost.EndSessionRequest(nil), c.endSessions...)
}

func (c *serviceManagerChild) stopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isStopped
}

type serviceManagerClock struct {
	mu     sync.Mutex
	now    time.Time
	timers chan *serviceManagerTimer
}

type serviceManagerTimer struct {
	mu      sync.Mutex
	active  bool
	when    time.Time
	channel chan time.Time
}

func newServiceManagerClock(now time.Time) *serviceManagerClock {
	return &serviceManagerClock{now: now, timers: make(chan *serviceManagerTimer, 1024)}
}

func (c *serviceManagerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *serviceManagerClock) NewTimer(delay time.Duration) pluginhost.Timer {
	c.mu.Lock()
	timer := &serviceManagerTimer{active: true, when: c.now.Add(delay), channel: make(chan time.Time, 1)}
	c.mu.Unlock()
	c.timers <- timer
	return timer
}

func (c *serviceManagerClock) FireNext(t *testing.T) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case timer := <-c.timers:
			timer.mu.Lock()
			if !timer.active {
				timer.mu.Unlock()
				continue
			}
			timer.active = false
			when := timer.when
			c.mu.Lock()
			c.now = when
			c.mu.Unlock()
			timer.channel <- when
			timer.mu.Unlock()
			return
		case <-deadline.C:
			t.Fatal("no active plugin supervisor timer")
		}
	}
}

func (t *serviceManagerTimer) C() <-chan time.Time { return t.channel }

func (t *serviceManagerTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := t.active
	t.active = false
	return wasActive
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
