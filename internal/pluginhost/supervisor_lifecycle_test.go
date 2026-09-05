package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"sync"
	"testing"
	"time"
)

func TestManagerCanceledStopRetainsChildUntilReapAndPreventsOverlappingReplacement(t *testing.T) {
	first := newCancelingStopChild(residentSpec("a", 1))
	var mu sync.Mutex
	starts := 0
	manager := NewManager("test", func(_ context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
		mu.Lock()
		defer mu.Unlock()
		starts++
		if starts == 1 {
			return first, nil
		}
		return newSupervisorChild(spec), nil
	}, Callbacks{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	spec := residentSpec("a", 1)
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	spec.Executable = "/replacement-plugin"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := manager.Apply(ctx, []Spec{spec})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup-changing Apply error = %v, want deadline", err)
	}
	status := statusByID(manager.Status(), "a")
	if status.Phase != PhaseStopping || !status.Running {
		t.Fatalf("status before reap = %#v, want owned stopping child", status)
	}
	if got := startCount(&mu, &starts); got != 1 {
		t.Fatalf("replacement overlapped unreaped child: %d starts", got)
	}
	if _, err := manager.SessionInputResult(context.Background(), "a", testSessionInput(1, "session")); !errors.Is(err, ErrPluginUnavailable) {
		t.Fatalf("session input while stopping error = %v, want unavailable", err)
	}
	if first.eventCount() != 0 {
		t.Fatal("stopping child received work from the replacement desired state")
	}

	retryCtx, retryCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = manager.Apply(retryCtx, []Spec{spec})
	retryCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Apply while stopping error = %v, want deadline", err)
	}
	if got := startCount(&mu, &starts); got != 1 {
		t.Fatalf("retry overlapped unreaped child: %d starts", got)
	}

	first.finish()
	awaitCondition(t, func() bool { return startCount(&mu, &starts) == 2 }, "replacement after old child reap")
	awaitStatus(t, manager, "a", func(status PluginStatus) bool { return status.Phase == PhaseRunning })
}

func TestManagerStartsQueuedReplacementWhenChildReapsBeforeStopReturnsError(t *testing.T) {
	stopErr := errors.New("stop failed after child exited")
	first := newControlledErrorStopChild(residentSpec("a", 1), stopErr)
	var mu sync.Mutex
	starts := 0
	manager := NewManager("test", func(_ context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
		mu.Lock()
		defer mu.Unlock()
		starts++
		if starts == 1 {
			return first, nil
		}
		return newSupervisorChild(spec), nil
	}, Callbacks{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	spec := residentSpec("a", 1)
	if err := manager.Apply(t.Context(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	reaped := manager.supervisors["a"].childReaped
	manager.mu.Unlock()

	spec.Executable = "/replacement-plugin"
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- manager.Apply(t.Context(), []Spec{spec})
	}()
	<-first.stopStarted
	first.finish()
	<-reaped
	close(first.releaseStop)
	if err := <-applyDone; !errors.Is(err, stopErr) {
		t.Fatalf("startup-changing Apply error = %v, want %v", err, stopErr)
	}

	awaitCondition(t, func() bool { return startCount(&mu, &starts) == 2 }, "replacement after child reap and stop error")
}

func TestSupervisorPostStopRetryCannotBeDowngradedToRestart(t *testing.T) {
	supervisor := &supervisor{}
	supervisor.queuePostStop(postStopRestart)
	supervisor.queuePostStop(postStopRetry)
	supervisor.queuePostStop(postStopRestart)
	if supervisor.postStop != postStopRetry {
		t.Fatalf("post-stop action = %v, want retry", supervisor.postStop)
	}
	supervisor.clearPostStop()
	if supervisor.postStop != postStopNone {
		t.Fatalf("cleared post-stop action = %v", supervisor.postStop)
	}
}

func TestManagerRetryAfterCanceledStopRedrivesStopBeforeReplacement(t *testing.T) {
	first := newRetryableStopChild(residentSpec("a", 1))
	defer first.finish()
	var mu sync.Mutex
	starts := 0
	manager := NewManager("test", func(_ context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
		mu.Lock()
		defer mu.Unlock()
		starts++
		if starts == 1 {
			return first, nil
		}
		return newSupervisorChild(spec), nil
	}, Callbacks{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	spec := residentSpec("a", 1)
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	spec.Executable = "/replacement-plugin"
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := manager.Apply(firstCtx, []Spec{spec})
	firstCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Apply error = %v, want canceled Stop deadline", err)
	}
	if got := startCount(&mu, &starts); got != 1 {
		t.Fatalf("replacement overlapped canceled Stop: %d starts", got)
	}

	retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Second)
	err = manager.Apply(retryCtx, []Spec{spec})
	retryCancel()
	if err != nil {
		t.Fatalf("retry Apply did not redrive Stop: %v", err)
	}
	if got := startCount(&mu, &starts); got != 2 {
		t.Fatalf("replacement starts = %d, want two after reap", got)
	}
}

func TestManagerDisableDuringBackoffAndCloseNeverRestart(t *testing.T) {
	clock := newSupervisorClock(time.Unix(500, 0))
	starts := 0
	manager := NewManager("test", func(context.Context, string, Spec, Callbacks) (Child, error) {
		starts++
		return nil, errors.New("boom")
	}, Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	if err := manager.Apply(context.Background(), []Spec{residentSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	clock.FireAll()
	if starts != 1 || len(manager.Status()) != 0 {
		t.Fatalf("disable allowed restart: starts=%d statuses=%#v", starts, manager.Status())
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.FireAll()
	if starts != 1 {
		t.Fatalf("Close allowed restart: starts=%d", starts)
	}
}

func TestManagerCrashWithdrawsEachGenerationOnceAndRetriesResident(t *testing.T) {
	clock := newSupervisorClock(time.Unix(600, 0))
	first := newSupervisorChild(residentSpec("a", 7))
	second := newSupervisorChild(residentSpec("a", 7))
	children := []Child{first, second}
	var mu sync.Mutex
	managerStarts := 0
	withdrawn := make(chan uint64, 4)
	manager := NewManager("test", func(context.Context, string, Spec, Callbacks) (Child, error) {
		mu.Lock()
		defer mu.Unlock()
		child := children[managerStarts]
		managerStarts++
		return child, nil
	}, Callbacks{WithdrawGeneration: func(_ string, generation uint64) { withdrawn <- generation }}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	spec := residentSpec("a", 7)
	spec.Instances = append(spec.Instances, Instance{ID: "two", Generation: 7, Config: json.RawMessage(`{}`)})
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	first.crash(errors.New("boom"))
	awaitStatus(t, manager, "a", func(s PluginStatus) bool { return s.Phase == PhaseBackoff && s.ExitCount == 1 })
	if got := awaitGeneration(t, withdrawn); got != 7 {
		t.Fatalf("withdrawn generation = %d", got)
	}
	select {
	case duplicate := <-withdrawn:
		t.Fatalf("generation withdrawn twice: %d", duplicate)
	default:
	}
	clock.FireNext(t)
	awaitStatus(t, manager, "a", func(s PluginStatus) bool { return s.Phase == PhaseRunning })
	if managerStarts != 2 {
		t.Fatalf("starts = %d, want 2", managerStarts)
	}
	closeManager(t, manager)
}

func TestManagerRemovalReachesSupervisorWithSaturatedCommandQueue(t *testing.T) {
	child, invokeRelease := blockingInteractiveChild()
	manager := NewManager("test", fixedStarter(child), Callbacks{})
	if err := manager.Apply(context.Background(), []Spec{interactiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	invokeDone := make(chan error, 1)
	go func() {
		invokeDone <- manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, SessionToken("session"))
	}()
	awaitSignal(t, child.invokeStart, "blocked invoke")
	fillSupervisorQueueWithCanceledEvents(t, manager, "a")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := manager.Apply(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Apply error = %v, want deadline while actor is blocked", err)
	}
	close(invokeRelease)
	if err := awaitError(t, invokeDone); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	awaitCondition(t, child.isStopped, "removed saturated supervisor to stop")
	awaitCondition(t, func() bool { return len(manager.Status()) == 0 }, "removed saturated supervisor registry cleanup")
}

func TestManagerCloseIrrevocablySignalsSaturatedSupervisorAndCanBeRetried(t *testing.T) {
	child, invokeRelease := blockingInteractiveChild()
	manager := NewManager("test", fixedStarter(child), Callbacks{})
	if err := manager.Apply(context.Background(), []Spec{interactiveSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, SessionToken("session"))
	}()
	awaitSignal(t, child.invokeStart, "blocked invoke")
	fillSupervisorQueueWithCanceledEvents(t, manager, "a")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := manager.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want deadline", err)
	}
	close(invokeRelease)
	awaitCondition(t, child.isStopped, "closed saturated supervisor to stop")
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("retry Close: %v", err)
	}
}

func TestManagerDoesNotReplaceRetiringSupervisorBeforeJoin(t *testing.T) {
	spec := residentInteractiveSpec("a", 1)
	oldChild := newSupervisorChild(spec)
	invokeRelease := make(chan struct{})
	oldChild.invokeStart = make(chan struct{}, 1)
	oldChild.invokeBlock = invokeRelease
	var mu sync.Mutex
	starts := 0
	manager := NewManager("test", func(_ context.Context, _ string, current Spec, _ Callbacks) (Child, error) {
		mu.Lock()
		defer mu.Unlock()
		starts++
		if starts == 1 {
			return oldChild, nil
		}
		return newSupervisorChild(current), nil
	}, Callbacks{})
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	invokeDone := make(chan error, 1)
	go func() {
		invokeDone <- manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, SessionToken("session"))
	}()
	awaitSignal(t, oldChild.invokeStart, "blocked invoke")

	removeCtx, cancelRemove := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := manager.Apply(removeCtx, nil)
	cancelRemove()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("remove Apply error = %v, want deadline", err)
	}
	reapplyCtx, cancelReapply := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = manager.Apply(reapplyCtx, []Spec{spec})
	cancelReapply()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reapply error = %v, want deadline while prior supervisor retires", err)
	}
	mu.Lock()
	gotStarts := starts
	mu.Unlock()
	if gotStarts != 1 {
		t.Fatalf("starts before prior supervisor joined = %d, want 1", gotStarts)
	}

	close(invokeRelease)
	_ = awaitError(t, invokeDone)
	awaitCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.supervisors) == 0
	}, "retiring supervisor cleanup")
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatalf("reapply after prior supervisor joined: %v", err)
	}
	mu.Lock()
	gotStarts = starts
	mu.Unlock()
	if gotStarts != 2 {
		t.Fatalf("starts after prior supervisor joined = %d, want 2", gotStarts)
	}
	closeManager(t, manager)
}

func TestManagerAlreadyCanceledFirstApplyDoesNotCreateSupervisor(t *testing.T) {
	starts := 0
	manager := NewManager("test", func(_ context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
		starts++
		return newSupervisorChild(spec), nil
	}, Callbacks{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Apply(ctx, []Spec{residentSpec("a", 1)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply error = %v, want canceled", err)
	}
	if statuses := manager.Status(); len(statuses) != 0 {
		t.Fatalf("Status after canceled first Apply = %#v, want empty", statuses)
	}
	if starts != 0 {
		t.Fatalf("starts after canceled first Apply = %d, want 0", starts)
	}
	closeManager(t, manager)
}

func TestManagerCanceledFirstApplyEventuallyCleansUpCreatedSupervisor(t *testing.T) {
	startEntered := make(chan struct{})
	startRelease := make(chan struct{})
	child := newSupervisorChild(residentSpec("a", 1))
	manager := NewManager("test", func(context.Context, string, Spec, Callbacks) (Child, error) {
		close(startEntered)
		<-startRelease
		return child, nil
	}, Callbacks{})
	ctx, cancel := context.WithCancel(context.Background())
	applyDone := make(chan error, 1)
	go func() { applyDone <- manager.Apply(ctx, []Spec{residentSpec("a", 1)}) }()
	awaitSignal(t, startEntered, "first supervisor start")
	cancel()
	if err := awaitError(t, applyDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply error = %v, want canceled", err)
	}
	if statuses := manager.Status(); len(statuses) != 0 {
		t.Fatalf("Status after canceled admitted Apply = %#v, want empty", statuses)
	}
	close(startRelease)
	awaitCondition(t, child.isStopped, "canceled first Apply child cleanup")
	awaitCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.supervisors) == 0
	}, "canceled first Apply supervisor cleanup")
	closeManager(t, manager)
}

func TestManagerCanceledQueuedCommandsRespectLifecycleOwnership(t *testing.T) {
	t.Run("invoke", func(t *testing.T) {
		child, release := blockingInteractiveChild()
		manager := NewManager("test", fixedStarter(child), Callbacks{})
		if err := manager.Apply(context.Background(), []Spec{interactiveSpec("a", 1)}); err != nil {
			t.Fatal(err)
		}
		go func() {
			_ = manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, SessionToken("first"))
		}()
		awaitSignal(t, child.invokeStart, "first invoke")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- manager.Invoke(ctx, "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, SessionToken("second"))
		}()
		awaitSupervisorQueueDepth(t, manager, "a", 1)
		cancel()
		if err := awaitError(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("queued Invoke error = %v", err)
		}
		close(release)
		awaitCondition(t, func() bool { return child.invocationCount() >= 1 }, "first invoke completion")
		awaitSupervisorQueueDepthExact(t, manager, "a", 0)
		if got := child.invocationCount(); got != 1 {
			t.Fatalf("invocations = %d, want 1", got)
		}
		closeManager(t, manager)
	})

	t.Run("event", func(t *testing.T) {
		child, release := blockingInteractiveChild()
		manager := NewManager("test", fixedStarter(child), Callbacks{})
		if err := manager.Apply(context.Background(), []Spec{interactiveSpec("a", 1)}); err != nil {
			t.Fatal(err)
		}
		go func() {
			_ = manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, SessionToken("session"))
		}()
		awaitSignal(t, child.invokeStart, "blocking invoke")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := manager.SessionInputResult(ctx, "a", testSessionInput(1, "session")); done <- err }()
		awaitSupervisorQueueDepth(t, manager, "a", 1)
		cancel()
		if err := awaitError(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("queued Event error = %v", err)
		}
		close(release)
		awaitCondition(t, func() bool { return child.invocationCount() == 1 }, "blocking invoke completion")
		awaitSupervisorQueueDepthExact(t, manager, "a", 0)
		if got := child.eventCount(); got != 0 {
			t.Fatalf("events = %d, want 0", got)
		}
		closeManager(t, manager)
	})

	t.Run("apply", func(t *testing.T) {
		child, release := blockingInteractiveChild()
		manager := NewManager("test", fixedStarter(child), Callbacks{})
		spec := interactiveSpec("a", 1)
		if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
			t.Fatal(err)
		}
		go func() {
			_ = manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, SessionToken("session"))
		}()
		awaitSignal(t, child.invokeStart, "blocking invoke")
		spec.Instances[0].Generation = 2
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- manager.Apply(ctx, []Spec{spec}) }()
		awaitSupervisorQueueDepth(t, manager, "a", 1)
		cancel()
		if err := awaitError(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("queued Apply error = %v", err)
		}
		close(release)
		awaitCondition(t, func() bool { return child.invocationCount() == 1 }, "blocking invoke completion")
		awaitSupervisorQueueDepthExact(t, manager, "a", 0)
		if got := child.generation(); got != 1 {
			t.Fatalf("generation = %d, want 1", got)
		}
		closeManager(t, manager)
	})

	t.Run("end session", func(t *testing.T) {
		child := newSupervisorChild(interactiveSpec("a", 1))
		eventRelease := make(chan struct{})
		child.eventStart = make(chan struct{}, 1)
		child.eventBlock = eventRelease
		manager := NewManager("test", fixedStarter(child), Callbacks{})
		if err := manager.Apply(context.Background(), []Spec{interactiveSpec("a", 1)}); err != nil {
			t.Fatal(err)
		}
		if err := manager.Invoke(context.Background(), "a", InvokeRequest{InstanceID: "one", Generation: 1}, InvocationInteractive, SessionToken("session")); err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = manager.SessionInputResult(context.Background(), "a", testSessionInput(1, "session")) }()
		awaitSignal(t, child.eventStart, "blocking event")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- manager.EndSession(ctx, "a", protocol.InstanceRef{ID: "one", Generation: 1}, SessionToken("session"))
		}()
		awaitSupervisorQueueDepth(t, manager, "a", 1)
		cancel()
		if err := awaitError(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("queued EndSession error = %v", err)
		}
		close(eventRelease)
		awaitCondition(t, func() bool { return child.eventCount() == 1 }, "blocking event completion")
		awaitSupervisorQueueDepthExact(t, manager, "a", 0)
		awaitCondition(t, child.isStopped, "canceled EndSession retirement of the unowned nonresident child")
		if got := child.endSessionCount(); got != 0 {
			t.Fatalf("expired queued EndSession made %d child RPCs, want process-death cleanup", got)
		}
		closeManager(t, manager)
	})
}

func TestManagerCloseDeadlineIncludesDelayedChildDoneAndRetrySurfacesStopError(t *testing.T) {
	child := newSupervisorChild(residentSpec("a", 1))
	child.delayDone = true
	child.stopErr = errors.New("stop failed")
	manager := NewManager("test", fixedStarter(child), Callbacks{})
	if err := manager.Apply(context.Background(), []Spec{residentSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- manager.Close(ctx) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(200 * time.Millisecond):
		child.finish()
		cancel()
		_ = awaitError(t, done)
		t.Fatal("Close did not return after caller deadline while Done was delayed")
	}
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want deadline", err)
	}
	if !errors.Is(err, child.stopErr) {
		t.Fatalf("first Close error = %v, want joined stop error", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("Close blocked %s beyond caller deadline", elapsed)
	}
	child.finish()
	awaitCondition(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		return errors.Is(manager.Close(ctx), child.stopErr)
	}, "retry Close to surface terminal stop error")
}

type cancelingStopChild struct {
	*supervisorChild
}

type controlledErrorStopChild struct {
	*supervisorChild
	stopStarted chan struct{}
	releaseStop chan struct{}
	stopErr     error
}

type retryableStopChild struct {
	*supervisorChild
	stopCalls int
}

func newCancelingStopChild(spec Spec) *cancelingStopChild {
	return &cancelingStopChild{supervisorChild: newSupervisorChild(spec)}
}

func (c *cancelingStopChild) Stop(ctx context.Context) error {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func newControlledErrorStopChild(spec Spec, stopErr error) *controlledErrorStopChild {
	return &controlledErrorStopChild{
		supervisorChild: newSupervisorChild(spec),
		stopStarted:     make(chan struct{}),
		releaseStop:     make(chan struct{}),
		stopErr:         stopErr,
	}
}

func (c *controlledErrorStopChild) Stop(context.Context) error {
	close(c.stopStarted)
	<-c.releaseStop
	return c.stopErr
}

func newRetryableStopChild(spec Spec) *retryableStopChild {
	return &retryableStopChild{supervisorChild: newSupervisorChild(spec)}
}

func (c *retryableStopChild) Stop(ctx context.Context) error {
	c.mu.Lock()
	c.stopped = true
	c.stopCalls++
	call := c.stopCalls
	c.mu.Unlock()
	if call == 1 {
		<-ctx.Done()
		return ctx.Err()
	}
	c.finish()
	return nil
}

func TestManagerNotifiesEveryStartedChildIncarnation(t *testing.T) {
	started := make(chan uint64, 2)
	manager := NewManager("test", func(_ context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
		return newSupervisorChild(spec), nil
	}, Callbacks{Started: func(_ string, runID uint64) { started <- runID }})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	spec := residentSpec("a", 1)
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	first := <-started
	if err := manager.Restart(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	second := <-started
	if first == 0 || second <= first {
		t.Fatalf("run IDs = %d, %d", first, second)
	}
}

func TestManagerChildIncarnationDoesNotResetWithReplacementSupervisor(t *testing.T) {
	started := make(chan uint64, 2)
	manager := NewManager("test", func(_ context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
		return newSupervisorChild(spec), nil
	}, Callbacks{Started: func(_ string, incarnation uint64) { started <- incarnation }})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	spec := residentSpec("a", 1)
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	first := <-started
	if err := manager.Apply(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	second := <-started
	if first == 0 || second <= first {
		t.Fatalf("incarnations reset across supervisor replacement: %d, %d", first, second)
	}
}

func blockingInteractiveChild() (*supervisorChild, chan struct{}) {
	release := make(chan struct{})
	child := newSupervisorChild(interactiveSpec("a", 1))
	child.invokeStart = make(chan struct{}, 1)
	child.invokeBlock = release
	return child, release
}

func fillSupervisorQueueWithCanceledEvents(t *testing.T, manager *Manager, id string) {
	t.Helper()
	manager.mu.Lock()
	current := manager.supervisors[id]
	manager.mu.Unlock()
	if current == nil {
		t.Fatalf("supervisor %q not found", id)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < supervisorQueueCapacity; i++ {
		current.commands <- sessionInputCommand{ctx: ctx, request: testSessionInput(uint64(i+1), "session")}
	}
}
