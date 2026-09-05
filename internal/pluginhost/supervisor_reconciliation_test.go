package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
	"sync"
	"testing"
	"time"
)

func TestManagerSubmitsAllPackageUpdatesBeforeWaiting(t *testing.T) {
	startedA := make(chan struct{})
	releaseA := make(chan struct{})
	startedB := make(chan struct{})
	var startedAOnce, startedBOnce sync.Once
	starter := func(ctx context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
		switch spec.ID {
		case "a":
			startedAOnce.Do(func() { close(startedA) })
			select {
			case <-releaseA:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case "b":
			startedBOnce.Do(func() { close(startedB) })
		}
		return newSupervisorChild(spec), nil
	}
	manager := NewManager("test", starter, Callbacks{})
	done := make(chan error, 1)
	go func() {
		done <- manager.Apply(context.Background(), []Spec{residentSpec("a", 1), residentSpec("b", 1)})
	}()
	awaitSignal(t, startedA, "plugin a start")
	awaitSignal(t, startedB, "plugin b start while a is blocked")
	awaitStatus(t, manager, "b", func(status PluginStatus) bool { return status.Phase == PhaseRunning })
	close(releaseA)
	if err := awaitError(t, done); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	closeManager(t, manager)
}

func TestManagerDefiniteReplacementErrorDoesNotCommitAndCanRetry(t *testing.T) {
	clock := newSupervisorClock(time.Unix(450, 0))
	child := newSupervisorChild(residentSpec("a", 1))
	child.replaceErr = errors.New("rejected")
	manager := NewManager("test", fixedStarter(child), Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	t.Cleanup(func() { _ = manager.Close(t.Context()) })
	if err := manager.Apply(t.Context(), []Spec{residentSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	updated := residentSpec("a", 2)
	if err := manager.Apply(t.Context(), []Spec{updated}); err == nil {
		t.Fatal("definite replacement error was reported as success")
	}
	if got := child.generation(); got != 1 {
		t.Fatalf("rejected replacement committed generation %d", got)
	}
	child.mu.Lock()
	child.replaceErr = nil
	child.mu.Unlock()
	if err := manager.Apply(t.Context(), []Spec{updated}); err != nil {
		t.Fatalf("retry replacement: %v", err)
	}
	if got := child.generation(); got != 2 {
		t.Fatalf("retried replacement generation = %d, want 2", got)
	}
}

func TestManagerUnknownReplacementOutcomeStopsChildAndRebuildsDesiredState(t *testing.T) {
	clock := newSupervisorClock(time.Unix(500, 0))
	first := newSupervisorChild(residentSpec("a", 1))
	first.replaceErr = errors.Join(rpc.ErrOutcomeUnknown, context.DeadlineExceeded)
	var mu sync.Mutex
	children := []*supervisorChild{first}
	manager := NewManager("test", func(_ context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(children) == 1 && children[0] == first && first.invocationCount() == 0 && !first.isStopped() {
			return first, nil
		}
		child := newSupervisorChild(spec)
		children = append(children, child)
		return child, nil
	}, Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	if err := manager.Apply(t.Context(), []Spec{residentSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	updated := residentSpec("a", 2)
	if err := manager.Apply(t.Context(), []Spec{updated}); !errors.Is(err, rpc.ErrOutcomeUnknown) {
		t.Fatalf("Apply error = %v, want unknown outcome", err)
	}
	if !first.isStopped() {
		t.Fatal("ambiguous replacement outcome retained the child")
	}
	if status := statusByID(manager.Status(), "a"); status.Phase != PhaseBackoff || status.LastErrorCode != ErrorCodeReconcileFailed {
		t.Fatalf("status = %#v, want reconcile backoff", status)
	}
	clock.FireNext(t)
	awaitCondition(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(children) == 2
	}, "desired-state child reconstruction")
	mu.Lock()
	replacement := children[1]
	mu.Unlock()
	if got := replacement.generation(); got != 2 {
		t.Fatalf("replacement generation = %d, want desired generation 2", got)
	}
}

func TestManagerReconcileWithdrawsOnlyRemovedGeneration(t *testing.T) {
	child := newSupervisorChild(residentSpec("a", 1))
	withdrawn := make(chan uint64, 4)
	manager := NewManager("test", fixedStarter(child), Callbacks{WithdrawGeneration: func(_ string, generation uint64) { withdrawn <- generation }})
	spec := residentSpec("a", 1)
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	spec.Instances[0].Generation = 2
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	if got := awaitGeneration(t, withdrawn); got != 1 {
		t.Fatalf("reconcile withdrew %d, want 1", got)
	}
	select {
	case got := <-withdrawn:
		t.Fatalf("unexpected reconcile withdrawal %d", got)
	default:
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := awaitGeneration(t, withdrawn); got != 2 {
		t.Fatalf("stop withdrew %d, want 2", got)
	}
}

func TestManagerReconcileWithdrawsOnlyChangedInstanceWhenSiblingsShareGeneration(t *testing.T) {
	spec := residentSpec("a", 1)
	spec.Instances = append(spec.Instances, Instance{ID: "two", Generation: 1, Config: json.RawMessage(`{}`)})
	child := newSupervisorChild(spec)
	withdrawn := make(chan protocol.InstanceRef, 4)
	manager := NewManager("test", fixedStarter(child), Callbacks{WithdrawInstance: func(pluginID, instanceID string, generation uint64) {
		if pluginID != "a" {
			t.Errorf("withdraw plugin = %q, want a", pluginID)
		}
		withdrawn <- protocol.InstanceRef{ID: instanceID, Generation: generation}
	}})
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	spec.Instances[0].Generation = 2
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-withdrawn:
		if got != (protocol.InstanceRef{ID: "one", Generation: 1}) {
			t.Fatalf("withdrawn instance = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for exact withdrawal")
	}
	select {
	case got := <-withdrawn:
		t.Fatalf("unexpected sibling withdrawal: %#v", got)
	default:
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
