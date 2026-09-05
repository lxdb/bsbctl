package device

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	publicstream "github.com/lxdb/busylib-go/stream"
)

func TestAssetRetryReconcilesOnWebSocketReadyTransition(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime(RuntimeConfig{
		BaseURL: "http://busybar.test",
		Factory: func(context.Context, string, string) (Client, error) {
			return &fakeRuntimeClient{statusStream: &fakeRuntimeStream{}}, nil
		},
	})
	reconciler := &fakeAssetReconciler{called: make(chan struct{}, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	runtimeDone := make(chan error, 1)
	workerDone := make(chan error, 1)
	go func() { workerDone <- RunAssetRetry(ctx, runtime, reconciler, AssetRetryOptions{}) }()
	go func() { runtimeDone <- runtime.Run(ctx) }()
	awaitRuntimePhase(t, runtime, PhaseConnecting)
	runtime.ObserveStatusStream(StreamHealth{
		Phase: StreamStatusTransition, Lifecycle: publicstream.LifecycleConnected, Access: publicstream.AccessAccepted,
	})

	select {
	case <-reconciler.called:
	case <-time.After(time.Second):
		t.Fatal("ready transition did not reconcile assets")
	}
	cancel()
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-runtimeDone; err != nil {
		t.Fatal(err)
	}
}

func TestAssetRetryUsesPendingDeadlineWithoutStatusStreamTraffic(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	timers := make(chan *fakeAssetTimer, 2)
	runtime := &fakeAssetRuntime{phase: PhaseReady, changes: make(chan struct{}, 1)}
	runtime.changes <- struct{}{}
	reconciler := &fakeAssetReconciler{called: make(chan struct{}, 3)}
	reconciler.states = []assets.State{{PluginID: "plugin", Phase: assets.PhasePending, RetryAt: now.Add(2 * time.Second)}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunAssetRetry(ctx, runtime, reconciler, AssetRetryOptions{
			Now: func() time.Time { return now },
			NewTimer: func(delay time.Duration) AssetRetryTimer {
				timer := &fakeAssetTimer{channel: make(chan time.Time, 1)}
				if delay != 2*time.Second {
					t.Errorf("timer delay = %v", delay)
				}
				timers <- timer
				return timer
			},
		})
	}()
	<-reconciler.called
	timer := <-timers
	reconciler.mu.Lock()
	reconciler.states = []assets.State{{PluginID: "plugin", Phase: assets.PhaseReady}}
	reconciler.mu.Unlock()
	timer.channel <- now.Add(2 * time.Second)
	select {
	case <-reconciler.called:
	case <-time.After(time.Second):
		t.Fatal("pending RetryAt did not trigger reconciliation")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAssetRetryDoesNotPollWhileRuntimeUnavailable(t *testing.T) {
	t.Parallel()
	runtime := &fakeAssetRuntime{phase: PhaseBackoff, changes: make(chan struct{}, 1)}
	runtime.changes <- struct{}{}
	reconciler := &fakeAssetReconciler{called: make(chan struct{}, 1)}
	timerCreated := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunAssetRetry(ctx, runtime, reconciler, AssetRetryOptions{NewTimer: func(time.Duration) AssetRetryTimer {
			timerCreated <- struct{}{}
			return &fakeAssetTimer{channel: make(chan time.Time)}
		}})
	}()
	select {
	case <-reconciler.called:
		t.Fatal("reconciled while runtime unavailable")
	case <-timerCreated:
		t.Fatal("scheduled polling while runtime unavailable")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type fakeAssetRuntime struct {
	mu      sync.Mutex
	phase   Phase
	changes chan struct{}
}

func (f *fakeAssetRuntime) Status() RuntimeStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return RuntimeStatus{Phase: f.phase}
}
func (f *fakeAssetRuntime) Changes() <-chan struct{} { return f.changes }
func (f *fakeAssetRuntime) setPhase(phase Phase) {
	f.mu.Lock()
	f.phase = phase
	f.mu.Unlock()
	select {
	case f.changes <- struct{}{}:
	default:
	}
}

type fakeAssetReconciler struct {
	mu     sync.Mutex
	states []assets.State
	called chan struct{}
}

func (f *fakeAssetReconciler) ReconcileAssets(context.Context) error {
	f.called <- struct{}{}
	return nil
}
func (f *fakeAssetReconciler) AssetStatus() []assets.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]assets.State(nil), f.states...)
}

type fakeAssetTimer struct{ channel chan time.Time }

func (f *fakeAssetTimer) C() <-chan time.Time    { return f.channel }
func (*fakeAssetTimer) Stop() bool               { return true }
func (*fakeAssetTimer) Reset(time.Duration) bool { return true }
