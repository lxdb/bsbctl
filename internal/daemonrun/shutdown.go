package daemonrun

import (
	"context"
	"errors"
	"time"

	"github.com/lxdb/bsbctl/internal/device"
)

type shutdownBudgets struct {
	service     time.Duration
	logs        time.Duration
	deviceClear time.Duration
	outputJoin  time.Duration
	runtimeJoin time.Duration
}

func defaultShutdownBudgets() shutdownBudgets {
	return shutdownBudgets{
		service: 9 * time.Second, logs: 2 * time.Second,
		deviceClear: 4 * time.Second, outputJoin: 6 * time.Second, runtimeJoin: 2 * time.Second,
	}
}

func waitRelays(done ...<-chan error) error {
	errs := make([]error, 0, len(done))
	for _, result := range done {
		if result != nil {
			errs = append(errs, <-result)
		}
	}
	return errors.Join(errs...)
}

func runShutdownPhase(parent context.Context, budget time.Duration, close func(context.Context) error) error {
	if close == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	return close(ctx)
}

func shutdownDeviceWithBudgets(ctx context.Context, gateway *device.Gateway, output *device.Output, cancelRuntime context.CancelFunc, runtimeDone <-chan error, budgets shutdownBudgets) error {
	// Runtime remains live until bsbctl's final desired canvas has cleared and
	// every already-admitted device operation has drained.
	clearErr := runShutdownPhase(ctx, budgets.deviceClear, func(clearCtx context.Context) error {
		_, err := gateway.Render(clearCtx, nil)
		return err
	})
	outputErr := runShutdownPhase(ctx, budgets.outputJoin, output.Close)
	cancelRuntime()
	runtimeCtx, cancel := context.WithTimeout(ctx, budgets.runtimeJoin)
	defer cancel()
	select {
	case runtimeErr := <-runtimeDone:
		return errors.Join(clearErr, outputErr, runtimeErr)
	case <-runtimeCtx.Done():
		return errors.Join(clearErr, outputErr, runtimeCtx.Err())
	}
}
