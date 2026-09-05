package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
	"sync"
	"testing"
	"time"
)

func TestManagerInteractiveAndResidentSessionLifetimes(t *testing.T) {
	tests := []struct {
		name     string
		spec     Spec
		kind     InvocationKind
		end      bool
		wantStop bool
	}{
		{name: "interactive ends explicitly", spec: interactiveSpec("a", 1), kind: InvocationInteractive, end: true, wantStop: true},
		{name: "resident interactive stays running", spec: residentInteractiveSpec("a", 1), kind: InvocationInteractive, end: true, wantStop: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			child := newSupervisorChild(test.spec)
			manager := NewManager("test", fixedStarter(child), Callbacks{})
			if err := manager.Apply(context.Background(), []Spec{test.spec}); err != nil {
				t.Fatal(err)
			}
			if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1, Action: "go"}, test.kind, SessionToken("session")); err != nil {
				t.Fatal(err)
			}
			if test.end {
				if err := manager.EndSession(context.Background(), "a", protocol.InstanceRef{ID: "one", Generation: 1}, SessionToken("session")); err != nil {
					t.Fatal(err)
				}
			}
			if child.isStopped() != test.wantStop {
				t.Fatalf("stopped = %v, want %v", child.isStopped(), test.wantStop)
			}
			closeManager(t, manager)
		})
	}
}

func TestManagerEndsMatchingSessionOnChildAtMostOnce(t *testing.T) {
	spec := interactiveSpec("a", 1)
	child := newSupervisorChild(spec)
	manager := NewManager("test", fixedStarter(child), Callbacks{})
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, "session"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := manager.EndSession(context.Background(), "a", protocol.InstanceRef{ID: "one", Generation: 1}, "session"); err != nil {
			t.Fatal(err)
		}
	}
	child.mu.Lock()
	requests := append([]EndSessionRequest(nil), child.endSessions...)
	child.mu.Unlock()
	if len(requests) != 1 || requests[0].InstanceID != "one" || requests[0].Generation != 1 || requests[0].SessionToken != "session" {
		t.Fatalf("end-session requests = %#v", requests)
	}
	closeManager(t, manager)
}

func TestManagerBindsInteractiveInvokeRequestToOwnedSessionToken(t *testing.T) {
	spec := residentInteractiveSpec("a", 1)
	child := newSupervisorChild(spec)
	manager := NewManager("test", fixedStarter(child), Callbacks{})
	t.Cleanup(func() { closeManager(t, manager) })
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	request := InvokeRequest{InstanceID: "one", Generation: 1, Action: "open"}
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, "session-7"); err != nil {
		t.Fatal(err)
	}
	child.mu.Lock()
	got := append([]InvokeRequest(nil), child.invokeRequests...)
	child.mu.Unlock()
	if len(got) != 1 || got[0].Generation != 1 || got[0].SessionToken != "session-7" {
		t.Fatalf("invoke requests = %#v", got)
	}
	result, err := manager.SessionInputResult(context.Background(), "a", testSessionInput(1, "session-7"))
	if err != nil {
		t.Fatalf("input for child-received generation/token: %v", err)
	}
	if result.Disposition != protocol.SessionInputConsumed {
		t.Fatalf("session input result = %#v", result)
	}
	if got := child.eventCount(); got != 1 {
		t.Fatalf("session inputs = %d, want exact promoted session accepted", got)
	}
}

func TestManagerRejectsWrongGenerationInvokeBeforeCallingChild(t *testing.T) {
	spec := residentInteractiveSpec("a", 7)
	child := newSupervisorChild(spec)
	manager := NewManager("test", fixedStarter(child), Callbacks{})
	t.Cleanup(func() { closeManager(t, manager) })
	if err := manager.Apply(t.Context(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	request := InvokeRequest{InstanceID: "one", Generation: 8, Action: "open"}
	if err := manager.Invoke(t.Context(), "a", request, InvocationInteractive, "session"); !errors.Is(err, ErrPluginNotConfigured) {
		t.Fatalf("wrong-generation Invoke error = %v, want ErrPluginNotConfigured", err)
	}
	if got := child.invocationCount(); got != 0 {
		t.Fatalf("wrong-generation child invocations = %d, want 0", got)
	}
	request.Generation = 7
	if err := manager.Invoke(t.Context(), "a", request, InvocationInteractive, "session"); err != nil {
		t.Fatalf("current-generation Invoke: %v", err)
	}
	if got := child.invocationCount(); got != 1 {
		t.Fatalf("current-generation child invocations = %d, want 1", got)
	}
}

func TestManagerRejectsStaleSessionInputBeforeCallingChild(t *testing.T) {
	spec := residentInteractiveSpec("a", 1)
	child := newSupervisorChild(spec)
	manager := NewManager("test", fixedStarter(child), Callbacks{})
	t.Cleanup(func() { closeManager(t, manager) })
	if err := manager.Apply(t.Context(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Invoke(t.Context(), "a", InvokeRequest{InstanceID: "one", Generation: 1, Action: "open"}, InvocationInteractive, "current"); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*protocol.SessionInputRequest){
		func(request *protocol.SessionInputRequest) { request.Instance.ID = "other" },
		func(request *protocol.SessionInputRequest) { request.Instance.Generation = 2 },
		func(request *protocol.SessionInputRequest) { request.SessionToken = "stale" },
	} {
		request := testSessionInput(1, "current")
		mutate(&request)
		if _, err := manager.SessionInputResult(t.Context(), "a", request); !errors.Is(err, ErrPluginNotConfigured) {
			t.Fatalf("stale session input %#v error = %v, want ErrPluginNotConfigured", request, err)
		}
		if got := child.eventCount(); got != 0 {
			t.Fatalf("stale session input reached child: %d calls", got)
		}
	}
	if _, err := manager.SessionInputResult(t.Context(), "a", testSessionInput(2, "current")); err != nil {
		t.Fatalf("current session input: %v", err)
	}
	if got := child.eventCount(); got != 1 {
		t.Fatalf("current session input calls = %d, want 1", got)
	}
}

func TestManagerCompletesOnlyTheExactInteractiveSessionWithoutCallingChildCleanup(t *testing.T) {
	spec := residentInteractiveSpec("a", 1)
	child := newSupervisorChild(spec)
	manager := NewManager("test", fixedStarter(child), Callbacks{})
	t.Cleanup(func() { closeManager(t, manager) })
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, "current"); err != nil {
		t.Fatal(err)
	}
	if err := manager.CompleteSession(context.Background(), "a", protocol.CompleteSessionRequest{
		Instance: protocol.InstanceRef{ID: "one", Generation: 1}, SessionToken: "stale",
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.EndSession(context.Background(), "a", protocol.InstanceRef{ID: "one", Generation: 1}, "current"); err != nil {
		t.Fatal(err)
	}
	if got := child.endSessionCount(); got != 1 {
		t.Fatalf("stale completion removed current session; child cleanups = %d", got)
	}

	if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, "generation-current"); err != nil {
		t.Fatal(err)
	}
	if err := manager.CompleteSession(context.Background(), "a", protocol.CompleteSessionRequest{
		Instance: protocol.InstanceRef{ID: "one", Generation: 2}, SessionToken: "generation-current",
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.EndSession(context.Background(), "a", protocol.InstanceRef{ID: "one", Generation: 2}, "generation-current"); err != nil {
		t.Fatal(err)
	}
	if got := child.endSessionCount(); got != 1 {
		t.Fatalf("wrong-generation end removed current session; child cleanups = %d", got)
	}
	if err := manager.EndSession(context.Background(), "a", protocol.InstanceRef{ID: "one", Generation: 1}, "generation-current"); err != nil {
		t.Fatal(err)
	}
	if got := child.endSessionCount(); got != 2 {
		t.Fatalf("wrong-generation completion removed current session; child cleanups = %d", got)
	}

	if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := manager.CompleteSession(context.Background(), "a", protocol.CompleteSessionRequest{
		Instance: protocol.InstanceRef{ID: "one", Generation: 1}, SessionToken: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.EndSession(context.Background(), "a", protocol.InstanceRef{ID: "one", Generation: 1}, "completed"); err != nil {
		t.Fatal(err)
	}
	if got := child.endSessionCount(); got != 2 {
		t.Fatalf("completed session called child cleanup; calls = %d", got)
	}
}

func TestManagerCompletionFromSessionInputDoesNotReenterSupervisor(t *testing.T) {
	spec := residentInteractiveSpec("a", 1)
	child := newSupervisorChild(spec)
	completed := make(chan protocol.CompleteSessionRequest, 1)
	unexpectedExternalCallback := make(chan struct{}, 1)
	callbacks := Callbacks{
		CompleteSession: func(context.Context, string, protocol.CompleteSessionRequest) error {
			unexpectedExternalCallback <- struct{}{}
			return errors.New("external completion callback must not be used")
		},
		SessionCompleted: func(_ string, request protocol.CompleteSessionRequest) { completed <- request },
	}
	starter := func(_ context.Context, _ string, _ Spec, hostCallbacks Callbacks) (Child, error) {
		child.mu.Lock()
		child.sessionInputHook = func(ctx context.Context, request protocol.SessionInputRequest) error {
			return hostCallbacks.CompleteSession(ctx, "a", protocol.CompleteSessionRequest{
				Instance: request.Instance, SessionToken: request.SessionToken,
			})
		}
		child.mu.Unlock()
		return child, nil
	}
	manager := NewManager("test", starter, callbacks)
	t.Cleanup(func() { closeManager(t, manager) })
	if err := manager.Apply(t.Context(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Invoke(t.Context(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, "session"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := manager.SessionInputResult(ctx, "a", testSessionInput(1, "session")); err != nil {
		t.Fatalf("session input completion deadlocked: %v", err)
	}
	select {
	case <-unexpectedExternalCallback:
		t.Fatal("manager did not replace the caller-supplied completion callback")
	default:
	}
	select {
	case request := <-completed:
		if request.Instance != (protocol.InstanceRef{ID: "one", Generation: 1}) || request.SessionToken != "session" {
			t.Fatalf("completed request = %#v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("admitted completion was not applied by the supervisor")
	}
	if err := manager.EndSession(t.Context(), "a", protocol.InstanceRef{ID: "one", Generation: 1}, "session"); err != nil {
		t.Fatal(err)
	}
	if got := child.endSessionCount(); got != 0 {
		t.Fatalf("completed session called child cleanup %d times", got)
	}
}

func TestManagerInteractiveFailureRemovesSessionAndReplacementUsesNewGeneration(t *testing.T) {
	children := []*supervisorChild{newSupervisorChild(interactiveSpec("a", 1)), newSupervisorChild(interactiveSpec("a", 2))}
	children[0].invokeErr = errors.New("invoke failed")
	var mu sync.Mutex
	starts := 0
	manager := NewManager("test", func(context.Context, string, Spec, Callbacks) (Child, error) {
		mu.Lock()
		defer mu.Unlock()
		child := children[starts]
		starts++
		return child, nil
	}, Callbacks{})
	spec := interactiveSpec("a", 1)
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	request := InvokeRequest{InstanceID: "one", Generation: 1, Action: "go"}
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken("first")); err == nil {
		t.Fatal("Invoke succeeded, want failure")
	}
	if !children[0].isStopped() {
		t.Fatal("failed interactive invoke retained an idle child")
	}
	spec.Instances[0].Generation = 2
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	request.Generation = 2
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken("second")); err != nil {
		t.Fatal(err)
	}
	if err := manager.EndSession(context.Background(), "a", protocol.InstanceRef{ID: "one", Generation: 2}, SessionToken("second")); err != nil {
		t.Fatal(err)
	}
	if !children[1].isStopped() {
		t.Fatal("replacement session did not stop its generation child")
	}
	closeManager(t, manager)
}

func TestManagerUnknownSessionLifecycleOutcomesStopAndRebuildResidentChild(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *Manager, *supervisorChild)
		code string
	}{
		{
			name: "start",
			code: ErrorCodeInvokeFailed,
			run: func(t *testing.T, manager *Manager, child *supervisorChild) {
				if err := manager.Invoke(t.Context(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, "old"); err != nil {
					t.Fatal(err)
				}
				child.mu.Lock()
				child.invokeErr = errors.Join(rpc.ErrOutcomeUnknown, context.DeadlineExceeded)
				child.mu.Unlock()
				if err := manager.Invoke(t.Context(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, "new"); !errors.Is(err, rpc.ErrOutcomeUnknown) {
					t.Fatalf("Invoke error = %v, want unknown outcome", err)
				}
			},
		},
		{
			name: "input",
			code: ErrorCodeSessionInputFailed,
			run: func(t *testing.T, manager *Manager, child *supervisorChild) {
				if err := manager.Invoke(t.Context(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, "session"); err != nil {
					t.Fatal(err)
				}
				child.mu.Lock()
				child.eventErr = errors.Join(rpc.ErrOutcomeUnknown, context.DeadlineExceeded)
				child.mu.Unlock()
				if _, err := manager.SessionInputResult(t.Context(), "a", testSessionInput(1, "session")); !errors.Is(err, rpc.ErrOutcomeUnknown) {
					t.Fatalf("SessionInput error = %v, want unknown outcome", err)
				}
			},
		},
		{
			name: "end",
			code: ErrorCodeEndSessionFailed,
			run: func(t *testing.T, manager *Manager, child *supervisorChild) {
				if err := manager.Invoke(t.Context(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, "session"); err != nil {
					t.Fatal(err)
				}
				child.setEndSessionError(errors.Join(rpc.ErrOutcomeUnknown, context.DeadlineExceeded))
				if err := manager.EndSession(t.Context(), "a", protocol.InstanceRef{ID: "one", Generation: 1}, "session"); !errors.Is(err, rpc.ErrOutcomeUnknown) {
					t.Fatalf("EndSession error = %v, want unknown outcome", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newSupervisorClock(time.Unix(600, 0))
			first := newSupervisorChild(residentInteractiveSpec("a", 1))
			var mu sync.Mutex
			starts := 0
			var replacement *supervisorChild
			manager := NewManager("test", func(_ context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
				mu.Lock()
				defer mu.Unlock()
				starts++
				if starts == 1 {
					return first, nil
				}
				replacement = newSupervisorChild(spec)
				return replacement, nil
			}, Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			if err := manager.Apply(t.Context(), []Spec{residentInteractiveSpec("a", 1)}); err != nil {
				t.Fatal(err)
			}
			test.run(t, manager, first)
			if !first.isStopped() {
				t.Fatal("ambiguous lifecycle outcome retained the child")
			}
			if status := statusByID(manager.Status(), "a"); status.Phase != PhaseBackoff || status.LastErrorCode != test.code {
				t.Fatalf("status = %#v, want %s backoff", status, test.code)
			}
			clock.FireNext(t)
			awaitCondition(t, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return starts == 2 && replacement != nil
			}, "resident child reconstruction")
			if got := replacement.generation(); got != 1 {
				t.Fatalf("replacement generation = %d, want 1", got)
			}
			if test.name == "input" {
				awaitStatus(t, manager, "a", func(status PluginStatus) bool { return status.Phase == PhaseRunning })
				awaitSupervisorQueueDepthExact(t, manager, "a", 0)
				if got := replacement.eventCount(); got != 0 {
					t.Fatalf("replacement child received %d replayed inputs, want 0", got)
				}
			}
		})
	}
}

func TestManagerStaleSessionTokenCannotEndReplacementSession(t *testing.T) {
	child := newSupervisorChild(interactiveSpec("a", 1))
	manager := NewManager("test", fixedStarter(child), Callbacks{})
	if err := manager.Apply(context.Background(), []Spec{interactiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	request := InvokeRequest{InstanceID: "one", Generation: 1, Action: "go"}
	oldToken := SessionToken("old")
	newToken := SessionToken("new")
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, oldToken); err != nil {
		t.Fatal(err)
	}
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, newToken); err != nil {
		t.Fatal(err)
	}
	child.mu.Lock()
	endedAfterReplacement := append([]EndSessionRequest(nil), child.endSessions...)
	child.mu.Unlock()
	if len(endedAfterReplacement) != 1 || endedAfterReplacement[0].Generation != 1 || endedAfterReplacement[0].SessionToken != string(oldToken) {
		t.Fatalf("replacement end-session requests = %#v, want old token once", endedAfterReplacement)
	}
	if err := manager.EndSession(context.Background(), "a", protocol.InstanceRef{ID: "one", Generation: 1}, oldToken); err != nil {
		t.Fatal(err)
	}
	if child.isStopped() {
		t.Fatal("stale token stopped replacement session")
	}
	if err := manager.EndSession(context.Background(), "a", protocol.InstanceRef{ID: "one", Generation: 1}, newToken); err != nil {
		t.Fatal(err)
	}
	if !child.isStopped() {
		t.Fatal("current token did not end replacement session")
	}
	closeManager(t, manager)
}

func TestManagerFailedSameInstanceReplacementKeepsPriorSession(t *testing.T) {
	child := newSupervisorChild(interactiveSpec("a", 1))
	manager := NewManager("test", fixedStarter(child), Callbacks{})
	if err := manager.Apply(context.Background(), []Spec{interactiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	request := InvokeRequest{InstanceID: "one", Generation: 1, Action: "go"}
	oldToken := SessionToken("old")
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, oldToken); err != nil {
		t.Fatal(err)
	}
	child.mu.Lock()
	child.invokeErr = errors.New("replacement failed")
	child.mu.Unlock()
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken("new")); err == nil {
		t.Fatal("replacement invoke succeeded")
	}
	if child.isStopped() {
		t.Fatal("failed replacement stopped the prior interactive session")
	}
	if err := manager.EndSession(context.Background(), "a", protocol.InstanceRef{ID: "one", Generation: 1}, oldToken); err != nil {
		t.Fatal(err)
	}
	child.mu.Lock()
	ended := append([]EndSessionRequest(nil), child.endSessions...)
	child.mu.Unlock()
	if len(ended) != 1 || ended[0].Generation != 1 || ended[0].SessionToken != string(oldToken) {
		t.Fatalf("end-session requests = %#v, want retained old token once", ended)
	}
	if !child.isStopped() {
		t.Fatal("ending retained prior session did not stop interactive child")
	}
	closeManager(t, manager)
}

func TestManagerRetriesPendingSessionCleanupOnHealthAndClearsLifecycle(t *testing.T) {
	clock := newSupervisorClock(time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC))
	child := newSupervisorChild(residentInteractiveSpec("a", 1))
	manager := NewManager("test", fixedStarter(child), Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	if err := manager.Apply(context.Background(), []Spec{residentInteractiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	request := InvokeRequest{InstanceID: "one", Generation: 1, Action: "go"}
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken("old")); err != nil {
		t.Fatal(err)
	}
	child.setEndSessionError(errors.New("transient cleanup failure"))
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken("new")); err != nil {
		t.Fatal(err)
	}
	code, at := pluginSessionLifecycle(statusByID(manager.Status(), "a"))
	if code != ErrorCodeEndSessionFailed || at.IsZero() || child.endSessionCount() != 1 {
		t.Fatalf("failed cleanup status/calls = %q, %v, %d", code, at, child.endSessionCount())
	}

	child.setEndSessionError(nil)
	clock.FireNext(t)
	awaitCondition(t, func() bool { return child.endSessionCount() == 2 }, "supervised session cleanup retry")
	awaitStatus(t, manager, "a", func(status PluginStatus) bool {
		code, at := pluginSessionLifecycle(status)
		return code == "" && at.IsZero()
	})
	closeManager(t, manager)
}

func TestManagerAmbiguousCleanupDuringHealthStopsWithoutPingingRetiredChild(t *testing.T) {
	for _, unreaped := range []bool{false, true} {
		t.Run(fmt.Sprintf("unreaped=%t", unreaped), func(t *testing.T) {
			clock := newSupervisorClock(time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC))
			spec := residentInteractiveSpec("a", 1)
			child := newSupervisorChild(spec)
			if unreaped {
				child.delayDone, child.stopErr = true, context.DeadlineExceeded
			}
			manager := NewManager("test", fixedStarter(child), Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
			t.Cleanup(func() {
				if unreaped {
					child.mu.Lock()
					child.stopErr = nil
					child.mu.Unlock()
					child.crash(nil)
				}
				closeManager(t, manager)
			})
			if err := manager.Apply(t.Context(), []Spec{spec}); err != nil {
				t.Fatal(err)
			}
			request := InvokeRequest{InstanceID: "one", Generation: 1, Action: "go"}
			if err := manager.Invoke(t.Context(), "a", request, InvocationInteractive, SessionToken("old")); err != nil {
				t.Fatal(err)
			}
			child.setEndSessionError(errors.New("retryable cleanup failure"))
			if err := manager.Invoke(t.Context(), "a", request, InvocationInteractive, SessionToken("new")); err != nil {
				t.Fatal(err)
			}
			pings := child.pingCount()
			child.setEndSessionError(rpc.ErrOutcomeUnknown)
			clock.FireNext(t)
			wantPhase := PhaseBackoff
			if unreaped {
				wantPhase = PhaseStopping
			}
			awaitStatus(t, manager, "a", func(status PluginStatus) bool { return status.Phase == wantPhase })
			if unreaped {
				// A rejected actor command joins the health turn before checking its state.
				if _, err := manager.SessionInputResult(t.Context(), "a", testSessionInput(1, "new")); err == nil {
					t.Fatal("stopping child admitted input")
				}
				if got := statusByID(manager.Status(), "a").Phase; got != PhaseStopping {
					t.Fatalf("health overwrote stopping phase: %v", got)
				}
			}
			if !child.isStopped() || child.pingCount() != pings {
				t.Fatalf("ambiguous cleanup: stopped=%v, pings=%d (before=%d)", child.isStopped(), child.pingCount(), pings)
			}
		})
	}
}

func TestManagerRepeatedSessionCleanupFailureStaysPendingOneRetryPerHealthCadence(t *testing.T) {
	clock := newSupervisorClock(time.Date(2026, 8, 21, 9, 15, 0, 0, time.UTC))
	child := newSupervisorChild(residentInteractiveSpec("a", 1))
	child.setEndSessionError(errors.New("persistent cleanup failure"))
	manager := NewManager("test", fixedStarter(child), Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
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
	firstCode, firstAt := pluginSessionLifecycle(statusByID(manager.Status(), "a"))
	clock.FireNext(t)
	awaitCondition(t, func() bool { return child.endSessionCount() == 2 }, "first failed supervised cleanup retry")
	secondCode, secondAt := pluginSessionLifecycle(statusByID(manager.Status(), "a"))
	if firstCode != ErrorCodeEndSessionFailed || secondCode != ErrorCodeEndSessionFailed || firstAt.IsZero() || secondAt.Before(firstAt) {
		t.Fatalf("session lifecycle moved incorrectly: first=%q/%v second=%q/%v", firstCode, firstAt, secondCode, secondAt)
	}
	clock.FireNext(t)
	awaitCondition(t, func() bool { return child.endSessionCount() == 3 }, "second failed supervised cleanup retry")
	if code, at := pluginSessionLifecycle(statusByID(manager.Status(), "a")); code != ErrorCodeEndSessionFailed || at.IsZero() {
		t.Fatalf("repeated cleanup failure was forgotten: %q/%v", code, at)
	}
	closeManager(t, manager)
}

func TestManagerOrdinaryEndSessionFailureUsesSupervisedCleanupQueue(t *testing.T) {
	clock := newSupervisorClock(time.Date(2026, 8, 21, 9, 25, 0, 0, time.UTC))
	child := newSupervisorChild(residentInteractiveSpec("a", 1))
	manager := NewManager("test", fixedStarter(child), Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	if err := manager.Apply(context.Background(), []Spec{residentInteractiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	token := SessionToken("ordinary")
	if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1, Action: "go"}, InvocationInteractive, token); err != nil {
		t.Fatal(err)
	}
	child.setEndSessionError(errors.New("ordinary cleanup failure"))
	if err := manager.EndSession(context.Background(), "a", protocol.InstanceRef{ID: "one", Generation: 1}, token); err == nil {
		t.Fatal("ordinary EndSession unexpectedly succeeded")
	}
	if code, at := pluginSessionLifecycle(statusByID(manager.Status(), "a")); code != ErrorCodeEndSessionFailed || at.IsZero() {
		t.Fatalf("ordinary cleanup lifecycle = %q/%v", code, at)
	}
	child.setEndSessionError(nil)
	clock.FireNext(t)
	awaitCondition(t, func() bool { return child.endSessionCount() == 2 }, "ordinary supervised cleanup retry")
	awaitStatus(t, manager, "a", func(status PluginStatus) bool {
		code, at := pluginSessionLifecycle(status)
		return code == "" && at.IsZero()
	})
	closeManager(t, manager)
}

func TestManagerQueuedCanceledEndSessionTransfersExactCleanupOwnership(t *testing.T) {
	clock := newSupervisorClock(time.Date(2026, 8, 21, 9, 27, 0, 0, time.UTC))
	child := newSupervisorChild(residentInteractiveSpec("a", 1))
	eventRelease := make(chan struct{})
	child.eventStart = make(chan struct{}, 1)
	child.eventBlock = eventRelease
	manager := NewManager("test", fixedStarter(child), Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	if err := manager.Apply(context.Background(), []Spec{residentInteractiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	token := SessionToken("queued-deadline")
	if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1, Action: "go"}, InvocationInteractive, token); err != nil {
		t.Fatal(err)
	}
	eventDone := make(chan error, 1)
	go func() {
		_, err := manager.SessionInputResult(context.Background(), "a", testSessionInput(1, token))
		eventDone <- err
	}()
	awaitSignal(t, child.eventStart, "blocking event")

	ctx, cancel := context.WithCancel(context.Background())
	endDone := make(chan error, 1)
	go func() {
		endDone <- manager.EndSession(ctx, "a", protocol.InstanceRef{ID: "one", Generation: 1}, token)
	}()
	awaitSupervisorQueueDepth(t, manager, "a", 1)
	cancel()
	if err := awaitError(t, endDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued EndSession error = %v", err)
	}
	close(eventRelease)
	if err := awaitError(t, eventDone); err != nil {
		t.Fatal(err)
	}
	awaitSupervisorQueueDepthExact(t, manager, "a", 0)
	awaitStatus(t, manager, "a", func(status PluginStatus) bool {
		code, at := pluginSessionLifecycle(status)
		return code == ErrorCodeEndSessionFailed && !at.IsZero()
	})
	if got := child.endSessionCount(); got != 0 {
		t.Fatalf("expired queued EndSession made %d immediate child calls, want 0", got)
	}

	clock.FireNext(t)
	awaitCondition(t, func() bool { return child.endSessionCount() == 1 }, "supervised exact-token cleanup retry")
	child.mu.Lock()
	request := child.endSessions[0]
	child.mu.Unlock()
	if request.InstanceID != "one" || request.Generation != 1 || request.SessionToken != string(token) {
		t.Fatalf("supervised cleanup request = %#v", request)
	}
	awaitStatus(t, manager, "a", func(status PluginStatus) bool {
		code, at := pluginSessionLifecycle(status)
		return code == "" && at.IsZero()
	})
	closeManager(t, manager)
}

func TestManagerChildExitClearsPendingSessionCleanupLifecycle(t *testing.T) {
	clock := newSupervisorClock(time.Date(2026, 8, 21, 9, 30, 0, 0, time.UTC))
	child := newSupervisorChild(residentInteractiveSpec("a", 1))
	child.setEndSessionError(errors.New("cleanup failed before exit"))
	cleanupEvents := make(chan SessionCleanup, 8)
	manager := NewManager("test", fixedStarter(child), Callbacks{SessionCleanup: func(value SessionCleanup) { cleanupEvents <- value }}, WithClock(clock), WithJitter(func() float64 { return 1 }))
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
	if code, _ := pluginSessionLifecycle(statusByID(manager.Status(), "a")); code != ErrorCodeEndSessionFailed {
		t.Fatalf("cleanup lifecycle before exit = %q", code)
	}
	failure := awaitSessionCleanupCallback(t, cleanupEvents, func(value SessionCleanup) bool {
		return value.PendingCount == 1 && value.ErrorCode == ErrorCodeEndSessionFailed
	})
	child.crash(errors.New("process exited"))
	status := awaitStatus(t, manager, "a", func(status PluginStatus) bool { return !status.Running })
	if code, at := pluginSessionLifecycle(status); code != "" || !at.IsZero() {
		t.Fatalf("dead child retained impossible cleanup work: %q/%v", code, at)
	}
	recovery := awaitSessionCleanupCallback(t, cleanupEvents, func(value SessionCleanup) bool {
		return value.Sequence > failure.Sequence && value.PendingCount == 0
	})
	if recovery.PluginID != "a" || recovery.ErrorCode != "" || recovery.At.Before(failure.At) {
		t.Fatalf("child-exit cleanup callback = %#v after %#v", recovery, failure)
	}
	closeManager(t, manager)
}

func TestManagerSessionCleanupCapRestartsChildInsteadOfDroppingToken(t *testing.T) {
	first := newSupervisorChild(residentInteractiveSpec("a", 1))
	first.setEndSessionError(errors.New("persistent cleanup failure"))
	second := newSupervisorChild(residentInteractiveSpec("a", 1))
	children := []Child{first, second}
	var starts int
	invalidated := make(chan SessionInvalidation, 2)
	manager := NewManager("test", func(context.Context, string, Spec, Callbacks) (Child, error) {
		child := children[starts]
		starts++
		return child, nil
	}, Callbacks{SessionInvalidated: func(value SessionInvalidation) { invalidated <- value }})
	if err := manager.Apply(context.Background(), []Spec{residentInteractiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	request := InvokeRequest{InstanceID: "one", Generation: 1, Action: "go"}
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken("token-0")); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 256; index++ {
		if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken(fmt.Sprintf("token-%d", index))); err != nil {
			t.Fatalf("replacement %d: %v", index, err)
		}
	}
	if first.isStopped() || starts != 1 {
		t.Fatalf("child restarted before the 256-entry bound: stopped=%t starts=%d", first.isStopped(), starts)
	}
	if err := manager.Invoke(context.Background(), "a", request, InvocationInteractive, SessionToken("token-overflow")); !errors.Is(err, ErrPluginUnavailable) {
		t.Fatalf("cleanup overflow Invoke = %v, want stable unavailable failure", err)
	}
	if !first.isStopped() || starts != 2 {
		t.Fatalf("cleanup overflow did not restart child: stopped=%t starts=%d", first.isStopped(), starts)
	}
	value := awaitSessionInvalidation(t, invalidated)
	if value.Token != SessionToken("token-256") || value.Reason != SessionInvalidatedReplaced {
		t.Fatalf("cleanup overflow invalidation = %#v, want exact prior owner", value)
	}
	status := statusByID(manager.Status(), "a")
	if code, at := pluginSessionLifecycle(status); code != "" || !at.IsZero() || !status.Running {
		t.Fatalf("post-restart session lifecycle = %#v, %q/%v", status, code, at)
	}
	closeManager(t, manager)
}

func TestManagerInvokeSuppliesOwnedDeadline(t *testing.T) {
	child := newSupervisorChild(interactiveSpec("a", 1))
	child.invokeDeadline = make(chan time.Time, 1)
	manager := NewManager("test", fixedStarter(child), Callbacks{})
	if err := manager.Apply(context.Background(), []Spec{interactiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, SessionToken("session")); err != nil {
		t.Fatal(err)
	}
	deadline := <-child.invokeDeadline
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > operationTimeout {
		t.Fatalf("Invoke deadline remaining = %s, want within %s", remaining, operationTimeout)
	}
	closeManager(t, manager)
}

func TestManagerStartupChangeInvalidatesActiveInteractiveSession(t *testing.T) {
	started := make(chan *supervisorChild, 2)
	invalidated := make(chan SessionInvalidation, 1)
	manager := NewManager("test", func(_ context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
		child := newSupervisorChild(spec)
		started <- child
		return child, nil
	}, Callbacks{SessionInvalidated: func(value SessionInvalidation) { invalidated <- value }})
	spec := interactiveSpec("a", 1)
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	token := SessionToken("session")
	if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, token); err != nil {
		t.Fatal(err)
	}
	first := awaitStartedChild(t, started)
	spec.Version = "2"
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	value := awaitSessionInvalidation(t, invalidated)
	if value.Token != token || value.Reason != SessionInvalidatedReplaced || !first.isStopped() {
		t.Fatalf("startup invalidation = %#v, stopped=%v", value, first.isStopped())
	}
	status := statusByID(manager.Status(), "a")
	if status.Phase != PhaseStopped || status.Running {
		t.Fatalf("status = %#v", status)
	}
	select {
	case unexpected := <-started:
		t.Fatalf("idle interactive replacement started child %p", unexpected)
	default:
	}
	closeManager(t, manager)
}

func TestManagerResidentInteractiveExitAndRestartInvalidateSession(t *testing.T) {
	for _, test := range []struct {
		name   string
		act    func(*Manager, *supervisorChild) error
		reason SessionInvalidationReason
	}{
		{name: "exit", reason: SessionInvalidatedExit, act: func(_ *Manager, child *supervisorChild) error { child.crash(errors.New("boom")); return nil }},
		{name: "restart", reason: SessionInvalidatedReplaced, act: func(manager *Manager, _ *supervisorChild) error { return manager.Restart(context.Background(), "a") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalidated := make(chan SessionInvalidation, 1)
			child := newSupervisorChild(residentInteractiveSpec("a", 1))
			manager := NewManager("test", fixedStarter(child), Callbacks{SessionInvalidated: func(value SessionInvalidation) { invalidated <- value }})
			if err := manager.Apply(context.Background(), []Spec{residentInteractiveSpec("a", 1)}); err != nil {
				t.Fatal(err)
			}
			if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, "session"); err != nil {
				t.Fatal(err)
			}
			if err := test.act(manager, child); err != nil {
				t.Fatal(err)
			}
			if value := awaitSessionInvalidation(t, invalidated); value.Reason != test.reason || value.Token != "session" {
				t.Fatalf("invalidation = %#v", value)
			}
			closeManager(t, manager)
		})
	}
}

func TestManagerUnexpectedInteractiveExitInvalidatesExactSession(t *testing.T) {
	invalidated := make(chan SessionInvalidation, 1)
	child := newSupervisorChild(interactiveSpec("a", 1))
	manager := NewManager("test", fixedStarter(child), Callbacks{SessionInvalidated: func(value SessionInvalidation) {
		invalidated <- value
	}})
	if err := manager.Apply(context.Background(), []Spec{interactiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	token := SessionToken("session")
	if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, token); err != nil {
		t.Fatal(err)
	}
	child.crash(errors.New("boom"))
	select {
	case value := <-invalidated:
		if value.PluginID != "a" || value.InstanceID != "one" || value.Generation != 1 || value.Token != token || value.Reason != SessionInvalidatedExit {
			t.Fatalf("invalidation = %#v", value)
		}
	case <-time.After(time.Second):
		t.Fatal("session invalidation was not delivered")
	}
	closeManager(t, manager)
}

func TestManagerGenerationChangeInvalidatesInteractiveSession(t *testing.T) {
	invalidated := make(chan SessionInvalidation, 1)
	child := newSupervisorChild(interactiveSpec("a", 1))
	manager := NewManager("test", fixedStarter(child), Callbacks{SessionInvalidated: func(value SessionInvalidation) { invalidated <- value }})
	spec := interactiveSpec("a", 1)
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, "session"); err != nil {
		t.Fatal(err)
	}
	spec.Instances[0].Generation = 2
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-invalidated:
		if value.Reason != SessionInvalidatedGeneration || value.Generation != 1 {
			t.Fatalf("invalidation = %#v", value)
		}
	case <-time.After(time.Second):
		t.Fatal("generation change did not invalidate session")
	}
	closeManager(t, manager)
}

func TestManagerTerminalHealthFailureInvalidatesInteractiveSession(t *testing.T) {
	clock := newSupervisorClock(time.Unix(900, 0))
	invalidated := make(chan SessionInvalidation, 1)
	child := newSupervisorChild(interactiveSpec("a", 1))
	child.pings = []pingResponse{{err: errors.New("timeout")}, {err: errors.New("timeout")}, {err: errors.New("timeout")}}
	manager := NewManager("test", fixedStarter(child), Callbacks{SessionInvalidated: func(value SessionInvalidation) { invalidated <- value }}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	if err := manager.Apply(context.Background(), []Spec{interactiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, "session"); err != nil {
		t.Fatal(err)
	}
	for misses := 1; misses <= 3; misses++ {
		clock.FireNext(t)
		awaitStatus(t, manager, "a", func(status PluginStatus) bool { return status.HealthMisses == misses })
	}
	select {
	case value := <-invalidated:
		if value.Reason != SessionInvalidatedHealth {
			t.Fatalf("invalidation = %#v", value)
		}
	case <-time.After(time.Second):
		t.Fatal("health failure did not invalidate session")
	}
	closeManager(t, manager)
}

func awaitSessionInvalidation(t *testing.T, values <-chan SessionInvalidation) SessionInvalidation {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for session invalidation")
		return SessionInvalidation{}
	}
}

func awaitSessionCleanupCallback(t *testing.T, values <-chan SessionCleanup, match func(SessionCleanup) bool) SessionCleanup {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case value := <-values:
			if match(value) {
				return value
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for session cleanup callback")
			return SessionCleanup{}
		}
	}
}

func TestSessionInputTimeoutKeepsAtomicActionsInsideSupervisorLease(t *testing.T) {
	tests := []struct {
		name  string
		input protocol.SessionInput
		want  time.Duration
	}{
		{name: "encoder", input: protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: 1}}, want: eventTimeout},
		{name: "back", input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonBack, Action: protocol.ButtonPress}}, want: eventTimeout},
		{name: "ok release", input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonOK, Action: protocol.ButtonRelease}}, want: eventTimeout},
		{name: "ok press", input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonOK, Action: protocol.ButtonPress}}, want: sessionActionTimeout},
		{name: "start press", input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonStart, Action: protocol.ButtonPress}}, want: sessionActionTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := testSessionInput(1, "session")
			request.Input = test.input
			if got := sessionInputTimeout(request); got != test.want {
				t.Fatalf("timeout=%s, want %s", got, test.want)
			}
		})
	}
}
