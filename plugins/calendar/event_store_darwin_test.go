//go:build darwin && cgo

package calendar

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type pendingAuthorizationRequest struct {
	polled   chan struct{}
	released atomic.Bool
}

func (r *pendingAuthorizationRequest) Poll() (accessStatus, bool, error) {
	select {
	case r.polled <- struct{}{}:
	default:
	}
	return accessUnknown, false, nil
}

func (r *pendingAuthorizationRequest) Close() { r.released.Store(true) }

func TestWaitForAuthorizationHonorsCancellationAfterRequestStarts(t *testing.T) {
	request := &pendingAuthorizationRequest{polled: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := waitForAuthorization(ctx, request)
		done <- err
	}()
	select {
	case <-request.polled:
	case <-time.After(time.Second):
		t.Fatal("authorization request was not polled")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("authorization wait did not honor cancellation")
	}
	if !request.released.Load() {
		t.Fatal("canceled authorization request was not released")
	}
}

func TestNativeEventStoreLifecycleDoesNotRequireReadingCalendarData(t *testing.T) {
	store, err := newNativeEventStore()
	if err != nil {
		t.Fatal(err)
	}
	status := store.AuthorizationStatus()
	switch status {
	case accessNotDetermined, accessRestricted, accessDenied, accessWriteOnly, accessFull:
	default:
		t.Fatalf("authorization status = %q", status)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeEventStoreAndOpenerHonorPreCanceledContexts(t *testing.T) {
	store, err := newNativeEventStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Events(ctx, time.Now(), time.Now().Add(time.Hour)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Events error = %v, want context.Canceled", err)
	}
	if _, err := store.Calendars(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Calendars error = %v, want context.Canceled", err)
	}
	if err := (nativeURLOpener{}).Open(ctx, "https://meet.google.com/abc-defg-hij"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open error = %v, want context.Canceled", err)
	}
}
