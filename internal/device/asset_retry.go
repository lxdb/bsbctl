package device

import (
	"context"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
)

type AssetRuntime interface {
	Status() RuntimeStatus
	Changes() <-chan struct{}
}

type AssetReconciler interface {
	ReconcileAssets(context.Context) error
	AssetStatus() []assets.State
}

type AssetRetryTimer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

type AssetRetryOptions struct {
	Wake     <-chan struct{}
	Now      func() time.Time
	NewTimer func(time.Duration) AssetRetryTimer
}

// RunAssetRetry retries asset reconciliation only while the device client is
// ready. Runtime and WebSocket edges wake it immediately; RetryAt drives
// endpoint recovery without depending on status-stream traffic.
func RunAssetRetry(ctx context.Context, runtime AssetRuntime, reconciler AssetReconciler, options AssetRetryOptions) error {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NewTimer == nil {
		options.NewTimer = func(delay time.Duration) AssetRetryTimer {
			return &realAssetRetryTimer{timer: time.NewTimer(delay)}
		}
	}
	var timer AssetRetryTimer
	var timerC <-chan time.Time
	disarm := func() {
		if timer != nil {
			stopAssetRetryTimer(timer)
		}
		timerC = nil
	}
	defer disarm()
	arm := func(deadline time.Time) {
		if deadline.IsZero() {
			disarm()
			return
		}
		delay := deadline.Sub(options.Now())
		if delay < 0 {
			delay = 0
		}
		if timer == nil {
			timer = options.NewTimer(delay)
		} else {
			stopAssetRetryTimer(timer)
			timer.Reset(delay)
		}
		timerC = timer.C()
	}
	reconcile := func() {
		if runtime.Status().Phase != PhaseReady {
			disarm()
			return
		}
		// Reconciliation failures are represented by asset/app diagnostics and
		// remain local; a transient endpoint failure must not end the daemon.
		_ = reconciler.ReconcileAssets(ctx)
		arm(earliestAssetRetry(reconciler.AssetStatus()))
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-runtime.Changes():
			reconcile()
		case <-options.Wake:
			reconcile()
		case <-timerC:
			timerC = nil
			reconcile()
		}
	}
}

func earliestAssetRetry(states []assets.State) time.Time {
	var earliest time.Time
	for _, state := range states {
		if state.Phase != assets.PhasePending || state.RetryAt.IsZero() {
			continue
		}
		if earliest.IsZero() || state.RetryAt.Before(earliest) {
			earliest = state.RetryAt
		}
	}
	return earliest
}

func stopAssetRetryTimer(timer AssetRetryTimer) {
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C():
	default:
	}
}

type realAssetRetryTimer struct{ timer *time.Timer }

func (t *realAssetRetryTimer) C() <-chan time.Time            { return t.timer.C }
func (t *realAssetRetryTimer) Stop() bool                     { return t.timer.Stop() }
func (t *realAssetRetryTimer) Reset(delay time.Duration) bool { return t.timer.Reset(delay) }
