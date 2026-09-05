package pluginhost

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestManagerRuntimeStartFailureIsStatusNotApplyError(t *testing.T) {
	clock := newSupervisorClock(time.Unix(100, 0))
	manager := NewManager("test", func(context.Context, string, Spec, Callbacks) (Child, error) {
		return nil, errors.New("contains secret-ish process details")
	}, Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	if err := manager.Apply(context.Background(), []Spec{residentSpec("a", 1)}); err != nil {
		t.Fatalf("valid desired state returned runtime error: %v", err)
	}
	status := statusByID(manager.Status(), "a")
	if status.Phase != PhaseBackoff || status.LastErrorCode != ErrorCodeStartFailed || status.RetryAt != clock.Now().Add(time.Second) {
		t.Fatalf("status = %#v", status)
	}
	if _, exists := reflect.TypeOf(status).FieldByName("LastError"); exists {
		t.Fatal("PluginStatus exposes a raw error field")
	}
	closeManager(t, manager)
}

func TestManagerHealthSeparatesDeclaredHealthFromRPCMisses(t *testing.T) {
	clock := newSupervisorClock(time.Unix(200, 0))
	child := newSupervisorChild(residentSpec("a", 1))
	child.pings = []pingResponse{{healthy: false}, {healthy: true}, {err: errors.New("timeout")}, {err: errors.New("timeout")}, {err: errors.New("timeout")}}
	manager := NewManager("test", fixedStarter(child), Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	if err := manager.Apply(context.Background(), []Spec{residentSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}

	clock.FireNext(t)
	awaitStatus(t, manager, "a", func(s PluginStatus) bool {
		return s.Phase == PhaseUnhealthy && s.Running && !s.Healthy && s.HealthMisses == 0 && s.LastErrorCode == ErrorCodeHealthReported
	})
	if child.isStopped() {
		t.Fatal("valid unhealthy response restarted the child")
	}
	clock.FireNext(t)
	awaitStatus(t, manager, "a", func(s PluginStatus) bool {
		return s.Phase == PhaseRunning && s.HealthMisses == 0 && s.Healthy && s.LastErrorCode == ""
	})
	for want := 1; want <= 3; want++ {
		clock.FireNext(t)
		misses := want
		if want < 3 {
			awaitStatus(t, manager, "a", func(s PluginStatus) bool { return s.HealthMisses == misses })
		}
	}
	awaitStatus(t, manager, "a", func(s PluginStatus) bool {
		return s.Phase == PhaseBackoff && s.HealthMisses == 3 && s.LastErrorCode == ErrorCodeHealthTimeout
	})
	if !child.isStopped() {
		t.Fatal("unhealthy child was not stopped")
	}
	closeManager(t, manager)
}

func TestManagerReplacementCleanupFailureIsHealthNotInvokeFailure(t *testing.T) {
	child := newSupervisorChild(interactiveSpec("a", 1))
	manager := NewManager("test", fixedStarter(child), Callbacks{})
	if err := manager.Apply(context.Background(), []Spec{interactiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	request := InvokeRequest{InstanceID: "one", Generation: 1, Action: "go"}
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken("old")); err != nil {
		t.Fatal(err)
	}
	child.mu.Lock()
	child.endSessionErr = errors.New("token=secret replacement cleanup failed")
	child.mu.Unlock()
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken("new")); err != nil {
		t.Fatalf("successful replacement became cleanup failure: %v", err)
	}
	status := statusByID(manager.Status(), "a")
	if status.SessionLifecycleErrorCode != ErrorCodeEndSessionFailed || status.SessionLifecycleErrorAt.IsZero() || status.LastErrorCode != "" || !status.Running {
		t.Fatalf("plugin status = %#v", status)
	}
	closeManager(t, manager)
}

func TestManagerEmitsSequencedSessionCleanupFailureAndRecovery(t *testing.T) {
	events := make(chan SessionCleanup, 8)
	awaitEvent := func(match func(SessionCleanup) bool) SessionCleanup {
		t.Helper()
		deadline := time.NewTimer(time.Second)
		defer deadline.Stop()
		for {
			select {
			case value := <-events:
				if match(value) {
					return value
				}
			case <-deadline.C:
				t.Fatal("timed out waiting for session cleanup callback")
				return SessionCleanup{}
			}
		}
	}
	callbacks := Callbacks{SessionCleanup: func(value SessionCleanup) { events <- value }}

	clock := newSupervisorClock(time.Date(2026, 8, 21, 9, 28, 0, 0, time.UTC))
	child := newSupervisorChild(residentInteractiveSpec("a", 1))
	child.setEndSessionError(errors.New("cleanup failed"))
	manager := NewManager("test", fixedStarter(child), callbacks, WithClock(clock), WithJitter(func() float64 { return 1 }))
	if err := manager.Apply(context.Background(), []Spec{residentInteractiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	request := InvokeRequest{InstanceID: "one", Generation: 1, Action: "go"}
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken("old")); err != nil {
		t.Fatal(err)
	}
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken("new")); err != nil {
		t.Fatal(err)
	}
	failure := awaitEvent(func(value SessionCleanup) bool { return value.PendingCount == 1 })
	if failure.PluginID != "a" || failure.Sequence == 0 || failure.At.IsZero() || failure.ErrorCode != ErrorCodeEndSessionFailed {
		t.Fatalf("cleanup failure callback = %#v", failure)
	}

	child.setEndSessionError(nil)
	clock.FireNext(t)
	recovery := awaitEvent(func(value SessionCleanup) bool { return value.PendingCount == 0 && value.Sequence > failure.Sequence })
	if recovery.PluginID != "a" || recovery.At.Before(failure.At) || recovery.ErrorCode != "" {
		t.Fatalf("cleanup recovery callback = %#v after %#v", recovery, failure)
	}
	closeManager(t, manager)
}

func TestManagerGeneralQueuedErrorCannotOverwriteSessionLifecycleFailure(t *testing.T) {
	child := newSupervisorChild(residentInteractiveSpec("a", 1))
	child.setEndSessionError(errors.New("cleanup failure"))
	endStarted := make(chan struct{}, 1)
	endRelease := make(chan struct{})
	child.mu.Lock()
	child.eventErr = errors.New("unrelated event failure")
	child.endSessionStart = endStarted
	child.endSessionBlock = endRelease
	child.mu.Unlock()
	manager := NewManager("test", fixedStarter(child), Callbacks{})
	if err := manager.Apply(context.Background(), []Spec{residentInteractiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	request := InvokeRequest{InstanceID: "one", Generation: 1, Action: "go"}
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken("old")); err != nil {
		t.Fatal(err)
	}
	replacementDone := make(chan error, 1)
	go func() {
		replacementDone <- manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken("new"))
	}()
	awaitSignal(t, endStarted, "replacement cleanup entry")
	eventDone := make(chan error, 1)
	go func() {
		_, err := manager.SessionInputResult(context.Background(), "a", testSessionInput(1, "new"))
		eventDone <- err
	}()
	awaitSupervisorQueueDepth(t, manager, "a", 1)
	close(endRelease)
	if err := <-replacementDone; err != nil {
		t.Fatal(err)
	}
	_, lifecycleAt := pluginSessionLifecycle(statusByID(manager.Status(), "a"))
	if err := <-eventDone; err == nil {
		t.Fatal("event unexpectedly succeeded")
	}
	status := statusByID(manager.Status(), "a")
	code, at := pluginSessionLifecycle(status)
	if status.LastErrorCode != ErrorCodeSessionInputFailed || code != ErrorCodeEndSessionFailed || !at.Equal(lifecycleAt) {
		t.Fatalf("general error raced lifecycle status: %#v lifecycle=%q/%v", status, code, at)
	}
	closeManager(t, manager)
}
