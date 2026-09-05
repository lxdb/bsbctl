package pluginhost

import (
	"context"
	"errors"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"math"
	"sync"
	"testing"
	"time"
)

func TestPluginRetryDelayIsOverflowSafeAndAbsolutelyCapped(t *testing.T) {
	t.Parallel()
	if got := backoffDelay(math.MaxInt); got != 30*time.Second {
		t.Fatalf("backoffDelay(MaxInt) = %v, want 30s", got)
	}
	if got := backoffDelay(math.MinInt); got != time.Second {
		t.Fatalf("backoffDelay(MinInt) = %v, want 1s", got)
	}
	tests := []struct {
		name   string
		delay  time.Duration
		jitter float64
		want   time.Duration
	}{
		{name: "negative infinity clamps low", delay: 16 * time.Second, jitter: math.Inf(-1), want: 12_800 * time.Millisecond},
		{name: "positive infinity clamps high", delay: 16 * time.Second, jitter: math.Inf(1), want: 19_200 * time.Millisecond},
		{name: "nan is neutral", delay: 16 * time.Second, jitter: math.NaN(), want: 16 * time.Second},
		{name: "jitter cannot exceed absolute cap", delay: 30 * time.Second, jitter: 1.2, want: 30 * time.Second},
		{name: "large input cannot overflow", delay: time.Duration(math.MaxInt64), jitter: .8, want: 30 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := jitteredDelay(test.delay, test.jitter); got != test.want {
				t.Fatalf("jitteredDelay(%v, %v) = %v, want %v", test.delay, test.jitter, got, test.want)
			}
		})
	}
}

func TestManagerPermanentUnsupportedStartDoesNotRetryUntilExecutableChanges(t *testing.T) {
	clock := newSupervisorClock(time.Unix(150, 0))
	var mu sync.Mutex
	starts := 0
	manager := NewManager("test", func(context.Context, string, Spec, Callbacks) (Child, error) {
		mu.Lock()
		starts++
		mu.Unlock()
		return nil, PermanentStart(errors.New("unsupported on this platform"))
	}, Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	spec := residentSpec("a", 1)
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatalf("valid unsupported desired state returned runtime error: %v", err)
	}
	status := statusByID(manager.Status(), "a")
	if status.Phase != PhaseQuarantined || status.LastErrorCode != ErrorCodeUnsupportedPlatform || !status.RetryAt.IsZero() {
		t.Fatalf("unsupported status = %#v", status)
	}
	if got := startCount(&mu, &starts); got != 1 {
		t.Fatalf("starts = %d, want one", got)
	}
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	if got := startCount(&mu, &starts); got != 1 {
		t.Fatalf("unchanged desired state retried permanent start: %d starts", got)
	}
	if err := manager.Restart(context.Background(), "a"); !errors.Is(err, ErrPluginUnavailable) {
		t.Fatalf("manual restart error = %v, want permanent unavailability", err)
	}
	if got := startCount(&mu, &starts); got != 1 {
		t.Fatalf("manual restart retried unchanged permanent start: %d starts", got)
	}

	spec.Executable = "/replacement-plugin"
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	if got := startCount(&mu, &starts); got != 2 {
		t.Fatalf("changed executable did not clear permanent classification: %d starts", got)
	}
}

func TestManagerQuarantinesFifthUnexpectedExit(t *testing.T) {
	clock := newSupervisorClock(time.Unix(300, 0))
	var mu sync.Mutex
	var child *supervisorChild
	manager := NewManager("test", func(context.Context, string, Spec, Callbacks) (Child, error) {
		mu.Lock()
		defer mu.Unlock()
		child = newSupervisorChild(residentSpec("a", 1))
		return child, nil
	}, Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	if err := manager.Apply(context.Background(), []Spec{residentSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	for exits := 1; exits <= 5; exits++ {
		mu.Lock()
		current := child
		mu.Unlock()
		current.crash(errors.New("boom"))
		wantPhase := PhaseBackoff
		if exits == 5 {
			wantPhase = PhaseQuarantined
		}
		awaitStatus(t, manager, "a", func(status PluginStatus) bool {
			return status.Phase == wantPhase && status.ExitCount == exits
		})
		if exits < 5 {
			clock.FireNext(t)
			awaitStatus(t, manager, "a", func(status PluginStatus) bool { return status.Phase == PhaseRunning })
		}
	}
	if err := manager.Restart(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	status := statusByID(manager.Status(), "a")
	if status.Phase != PhaseRunning || status.ExitCount != 0 {
		t.Fatalf("status after explicit restart = %#v", status)
	}
	closeManager(t, manager)
}

func TestManagerUnexpectedExitWindowPrunesAfterTenMinutes(t *testing.T) {
	clock := newSupervisorClock(time.Unix(350, 0))
	started := make(chan *supervisorChild, 5)
	manager := NewManager("test", func(context.Context, string, Spec, Callbacks) (Child, error) {
		child := newSupervisorChild(residentSpec("a", 1))
		started <- child
		return child, nil
	}, Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	if err := manager.Apply(context.Background(), []Spec{residentSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	current := awaitStartedChild(t, started)
	for exits := 1; exits <= 4; exits++ {
		current.crash(errors.New("boom"))
		awaitStatus(t, manager, "a", func(status PluginStatus) bool {
			return status.Phase == PhaseBackoff && status.ExitCount == exits
		})
		clock.FireNext(t)
		current = awaitStartedChild(t, started)
		awaitStatus(t, manager, "a", func(status PluginStatus) bool { return status.Phase == PhaseRunning })
	}
	clock.Advance(10*time.Minute + time.Nanosecond)
	current.crash(errors.New("boom"))
	status := awaitStatus(t, manager, "a", func(status PluginStatus) bool { return status.Phase == PhaseBackoff })
	if status.ExitCount != 1 {
		t.Fatalf("rolling exit count = %d, want 1", status.ExitCount)
	}
	closeManager(t, manager)
}

func TestManagerSpecChangeClearsQuarantineForOnlyAffectedPackage(t *testing.T) {
	clock := newSupervisorClock(time.Unix(400, 0))
	var mu sync.Mutex
	children := make(map[string]*supervisorChild)
	manager := NewManager("test", func(_ context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
		mu.Lock()
		defer mu.Unlock()
		child := newSupervisorChild(spec)
		children[spec.ID] = child
		return child, nil
	}, Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	a, b := residentSpec("a", 1), residentSpec("b", 1)
	if err := manager.Apply(context.Background(), []Spec{a, b}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		for exits := 1; exits <= 5; exits++ {
			mu.Lock()
			current := children[id]
			mu.Unlock()
			current.crash(errors.New("boom"))
			want := PhaseBackoff
			if exits == 5 {
				want = PhaseQuarantined
			}
			awaitStatus(t, manager, id, func(status PluginStatus) bool { return status.Phase == want })
			if exits < 5 {
				clock.FireNext(t)
				awaitStatus(t, manager, id, func(status PluginStatus) bool { return status.Phase == PhaseRunning })
			}
		}
	}
	if statusByID(manager.Status(), "a").Phase != PhaseQuarantined || statusByID(manager.Status(), "b").Phase != PhaseQuarantined {
		t.Fatalf("statuses = %#v", manager.Status())
	}
	a.Instances[0].Generation = 2
	if err := manager.Apply(context.Background(), []Spec{a, b}); err != nil {
		t.Fatal(err)
	}
	if statusByID(manager.Status(), "a").Phase != PhaseRunning {
		t.Fatalf("changed a status = %#v", statusByID(manager.Status(), "a"))
	}
	if statusByID(manager.Status(), "b").Phase != PhaseQuarantined {
		t.Fatalf("unchanged b status = %#v", statusByID(manager.Status(), "b"))
	}
	closeManager(t, manager)
}

func TestManagerPermanentReplacementFailureQuarantinesUntilSpecChanges(t *testing.T) {
	child := newSupervisorChild(residentSpec("a", 1))
	child.replaceErr = protocol.NewDomainError(protocol.ErrorInvalidArgument, errors.New("invalid desired configuration"))
	manager := NewManager("test", func(_ context.Context, _ string, _ Spec, _ Callbacks) (Child, error) {
		return child, nil
	}, Callbacks{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	if err := manager.Apply(t.Context(), []Spec{residentSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(t.Context(), []Spec{residentSpec("a", 2)}); err == nil {
		t.Fatal("permanent replacement unexpectedly succeeded")
	}
	status := statusByID(manager.Status(), "a")
	if status.Phase != PhaseQuarantined || status.LastErrorCode != ErrorCodeConfigurationRejected || !status.RetryAt.IsZero() {
		t.Fatalf("status = %#v, want permanent configuration quarantine", status)
	}

	child.mu.Lock()
	child.replaceErr = nil
	child.mu.Unlock()
	if err := manager.Apply(t.Context(), []Spec{residentSpec("a", 3)}); err != nil {
		t.Fatalf("Apply corrected spec: %v", err)
	}
	status = statusByID(manager.Status(), "a")
	if status.Phase != PhaseRunning || status.LastErrorCode != "" || child.generation() != 3 {
		t.Fatalf("corrected status = %#v, generation = %d", status, child.generation())
	}
}

func TestManagerStartFailuresUseFullDelayLadderWithoutQuarantine(t *testing.T) {
	clock := newSupervisorClock(time.Unix(700, 0))
	manager := NewManager("test", func(context.Context, string, Spec, Callbacks) (Child, error) {
		return nil, errors.New("start failed")
	}, Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	if err := manager.Apply(context.Background(), []Spec{residentSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}
	wants := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	for attempt, want := range wants {
		status := awaitStatus(t, manager, "a", func(status PluginStatus) bool { return status.Phase == PhaseBackoff })
		if got := status.RetryAt.Sub(clock.Now()); got != want {
			t.Fatalf("attempt %d delay = %s, want %s", attempt+1, got, want)
		}
		if status.Phase == PhaseQuarantined {
			t.Fatalf("start failure %d quarantined package", attempt+1)
		}
		if attempt+1 < len(wants) {
			clock.FireNext(t)
			awaitStatus(t, manager, "a", func(next PluginStatus) bool { return next.LastErrorAt.After(status.LastErrorAt) })
		}
	}
	closeManager(t, manager)
}

func TestManagerUnexpectedExitRetriesUseFullDelayLadder(t *testing.T) {
	clock := newSupervisorClock(time.Unix(750, 0))
	var mu sync.Mutex
	var child *supervisorChild
	manager := NewManager("test", func(_ context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
		mu.Lock()
		defer mu.Unlock()
		child = newSupervisorChild(spec)
		return child, nil
	}, Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	if err := manager.Apply(context.Background(), []Spec{residentSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}

	wants := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	for attempt, want := range wants {
		if attempt == 4 {
			// Keep retry-attempt history while pruning the separate rolling exit window.
			clock.Jump(10*time.Minute + time.Nanosecond)
		}
		mu.Lock()
		current := child
		mu.Unlock()
		current.crash(errors.New("boom"))
		status := awaitStatus(t, manager, "a", func(status PluginStatus) bool {
			return status.Phase == PhaseBackoff && status.RetryAt.After(clock.Now())
		})
		if got := status.RetryAt.Sub(clock.Now()); got != want {
			t.Fatalf("unexpected exit %d delay = %s, want %s", attempt+1, got, want)
		}
		if attempt+1 < len(wants) {
			clock.FireNext(t)
			awaitStatus(t, manager, "a", func(status PluginStatus) bool { return status.Phase == PhaseRunning })
		}
	}
	closeManager(t, manager)
}

func TestManagerSuccessfulHealthPingResetsRetryAttempt(t *testing.T) {
	clock := newSupervisorClock(time.Unix(775, 0))
	var mu sync.Mutex
	var child *supervisorChild
	manager := NewManager("test", func(_ context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
		mu.Lock()
		defer mu.Unlock()
		child = newSupervisorChild(spec)
		return child, nil
	}, Callbacks{}, WithClock(clock), WithJitter(func() float64 { return 1 }))
	if err := manager.Apply(context.Background(), []Spec{residentSpec("a", 1)}); err != nil {
		t.Fatal(err)
	}

	crash := func(want time.Duration) {
		t.Helper()
		mu.Lock()
		current := child
		mu.Unlock()
		current.crash(errors.New("boom"))
		status := awaitStatus(t, manager, "a", func(status PluginStatus) bool {
			return status.Phase == PhaseBackoff && status.RetryAt.After(clock.Now())
		})
		if got := status.RetryAt.Sub(clock.Now()); got != want {
			t.Fatalf("retry delay = %s, want %s", got, want)
		}
	}

	crash(time.Second)
	clock.FireNext(t)
	awaitStatus(t, manager, "a", func(status PluginStatus) bool { return status.Phase == PhaseRunning })
	crash(2 * time.Second)
	clock.FireNext(t)
	awaitStatus(t, manager, "a", func(status PluginStatus) bool { return status.Phase == PhaseRunning })
	mu.Lock()
	healthyChild := child
	mu.Unlock()
	clock.FireNext(t)
	awaitCondition(t, func() bool { return healthyChild.pingCount() == 1 }, "successful health ping")
	crash(time.Second)
	closeManager(t, manager)
}
