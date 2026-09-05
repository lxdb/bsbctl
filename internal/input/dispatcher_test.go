package input

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxdb/busylib-go/proto/inputpb"
)

func TestDispatcherPreservesFIFOAndContinuesAfterHandlerFailure(t *testing.T) {
	var values []int32
	dispatcher := newTestDispatcher(func(_ context.Context, event *inputpb.InputEvent) error {
		value := event.GetEncoderEvent().GetDelta()
		values = append(values, value)
		if value == 2 {
			return errors.New("sensitive handler detail")
		}
		return nil
	}, nil)
	for _, value := range []int32{1, 2, 3} {
		if !dispatcher.Submit(encoder(value)) {
			t.Fatalf("Submit(%d) rejected", value)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	awaitInput(t, func() bool { return dispatcher.Status().Handled == 3 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, want := values, []int32{1, 2, 3}; len(got) != len(want) || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("values = %v", got)
	}
	if status := dispatcher.Status(); status.LastErrorCode != "input_handler_failed" {
		t.Fatalf("status = %#v", status)
	}
}

func TestDispatcherOverflowIsNonBlockingAndCoalescesCancellation(t *testing.T) {
	var canceled atomic.Int32
	dispatcher := newTestDispatcher(func(context.Context, *inputpb.InputEvent) error { return nil }, func(context.Context) {
		canceled.Add(1)
	})
	for index := 0; index < InputQueueCapacity; index++ {
		if !dispatcher.Submit(encoder(int32(index))) {
			t.Fatalf("queue rejected item %d", index)
		}
	}
	started := time.Now()
	if dispatcher.Submit(encoder(1000)) {
		t.Fatal("overflow item was accepted")
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("overflow submissions blocked")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	awaitInput(t, func() bool { return canceled.Load() == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	status := dispatcher.Status()
	if status.Overruns != 1 || status.Discarded != InputQueueCapacity || status.QueueDepth != 0 || status.Handled != 0 || status.LastErrorCode != "input_overrun" || canceled.Load() != 1 {
		t.Fatalf("status=%#v canceled=%d", status, canceled.Load())
	}
}

func TestDispatcherAcceptsFreshInputAfterDiscardingOverflowBurst(t *testing.T) {
	handled := make(chan int32, 1)
	dispatcher := newTestDispatcher(func(_ context.Context, event *inputpb.InputEvent) error {
		handled <- event.GetEncoderEvent().GetDelta()
		return nil
	}, func(context.Context) {})
	for index := range InputQueueCapacity {
		dispatcher.Submit(encoder(int32(index + 1)))
	}
	if dispatcher.Submit(encoder(999)) {
		t.Fatal("overflow item was accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dispatcher.Run(ctx) }()
	awaitInput(t, func() bool { return dispatcher.Status().QueueDepth == 0 })
	if !dispatcher.Submit(encoder(1000)) {
		t.Fatal("fresh input was rejected after overflow recovery")
	}
	select {
	case value := <-handled:
		if value != 1000 {
			t.Fatalf("stale burst input was replayed: %d", value)
		}
	case <-time.After(time.Second):
		t.Fatal("fresh input was not handled")
	}
}

func TestDispatcherInvalidatesOldForegroundInputAndAcceptsOnlyNewContext(t *testing.T) {
	handled := make(chan int32, 2)
	dispatcher := newTestDispatcher(func(_ context.Context, event *inputpb.InputEvent) error {
		handled <- event.GetEncoderEvent().GetDelta()
		return nil
	}, func(context.Context) { t.Error("foreground transition invoked overflow cleanup") })
	if !dispatcher.Submit(encoder(1)) || !dispatcher.Submit(encoder(2)) {
		t.Fatal("old-context input was not admitted")
	}
	dispatcher.InvalidateContext()
	if !dispatcher.Submit(encoder(3)) {
		t.Fatal("new-context input was not admitted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	select {
	case got := <-handled:
		if got != 3 {
			t.Fatalf("handled stale context value %d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("new-context input was not handled")
	}
	select {
	case got := <-handled:
		t.Fatalf("unexpected additional input %d", got)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	status := dispatcher.Status()
	if status.Discarded != 2 || status.Handled != 1 || status.QueueDepth != 0 {
		t.Fatalf("status=%#v", status)
	}
}

func TestDispatcherOverflowCleanupTargetsCapturedSession(t *testing.T) {
	current := "old"
	cleared := make(chan string, 1)
	dispatcher := NewDispatcherWithClearCapture(func(context.Context, *inputpb.InputEvent) error { return nil }, func() func(context.Context) {
		captured := current
		return func(context.Context) { cleared <- captured }
	})
	for index := range InputQueueCapacity {
		dispatcher.Submit(encoder(int32(index + 1)))
	}
	if dispatcher.Submit(encoder(999)) {
		t.Fatal("overflow item was accepted")
	}
	current = "new"
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	select {
	case got := <-cleared:
		if got != "old" {
			t.Fatalf("cleared session %q, want captured old session", got)
		}
	case <-time.After(time.Second):
		t.Fatal("captured overflow cleanup did not run")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherClearsPromptlyWhileInputContinuesDuringOverflowRecovery(t *testing.T) {
	cleared := make(chan struct{}, 1)
	dispatcher := newTestDispatcher(func(context.Context, *inputpb.InputEvent) error { return nil }, func(context.Context) {
		select {
		case cleared <- struct{}{}:
		default:
		}
	})
	for index := range InputQueueCapacity {
		dispatcher.Submit(encoder(int32(index + 1)))
	}
	if dispatcher.Submit(encoder(999)) {
		t.Fatal("overflow item was accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for ctx.Err() == nil {
			dispatcher.Submit(encoder(1000))
		}
	}()
	go func() { done <- dispatcher.Run(ctx) }()
	select {
	case <-cleared:
	case <-time.After(time.Second):
		t.Fatal("continuous input prevented overflow cancellation")
	}
	cancel()
	<-producerDone
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not join after continuous input stopped")
	}
}

func TestDispatcherCancellationJoins(t *testing.T) {
	dispatcher := newTestDispatcher(func(context.Context, *inputpb.InputEvent) error { return nil }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not join")
	}
}

func encoder(delta int32) *inputpb.InputEvent {
	return &inputpb.InputEvent{Event: &inputpb.InputEvent_EncoderEvent{EncoderEvent: &inputpb.EncoderEvent{Delta: delta}}}
}

func newTestDispatcher(handle func(context.Context, *inputpb.InputEvent) error, clear func(context.Context)) *Dispatcher {
	return NewDispatcherWithClearCapture(handle, func() func(context.Context) { return clear })
}

func awaitInput(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met")
}
