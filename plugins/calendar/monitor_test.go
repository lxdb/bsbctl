package calendar

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCalendarMonitorRefreshesAtBoundariesAndCoalescedStoreChanges(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	store := newMonitorEventStore(accessFull, []calendarEvent{{
		CalendarID: "work", EventID: "next", Start: now.Add(2 * time.Minute), End: now.Add(time.Hour),
	}})
	state := newCalendarState(store, &fakeURLOpener{}, func() time.Time { return now }, 5*time.Minute)
	boundary := newManualMonitorTimer()
	debounce := newManualMonitorTimer()
	authorization := newManualMonitorTicker()
	updates := make(chan monitorUpdate, 4)
	monitor := newCalendarMonitor(state, store, boundary, debounce, authorization, func(_ context.Context, selected calendarRefresh, err error) {
		updates <- monitorUpdate{selected: selected.selectedEvents, err: err}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- monitor.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	initial := nextMonitorUpdate(t, updates)
	if initial.err != nil || initial.selected.Upcoming == nil || initial.selected.Upcoming.EventID != "next" {
		t.Fatalf("initial update = %#v", initial)
	}
	if got := nextTimerReset(t, boundary); got != 2*time.Minute {
		t.Fatalf("boundary reset = %s, want 2m", got)
	}

	store.changes <- struct{}{}
	if got := nextTimerReset(t, debounce); got != 250*time.Millisecond {
		t.Fatalf("debounce reset = %s, want 250ms", got)
	}
	if got := store.fetchCount(); got != 1 {
		t.Fatalf("store change fetched before debounce: %d", got)
	}
	debounce.ticks <- now
	changed := nextMonitorUpdate(t, updates)
	if changed.err != nil || changed.selected.Upcoming == nil {
		t.Fatalf("changed update = %#v", changed)
	}
	if got := store.fetchCount(); got != 2 {
		t.Fatalf("store change fetches = %d, want 2", got)
	}
	_ = nextTimerReset(t, boundary)
}

func TestCalendarMonitorWithdrawsStateWhenAuthorizationIsRevoked(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	store := newMonitorEventStore(accessFull, []calendarEvent{{
		CalendarID: "work", EventID: "next", Start: now.Add(2 * time.Minute), End: now.Add(time.Hour),
	}})
	state := newCalendarState(store, &fakeURLOpener{}, func() time.Time { return now }, 5*time.Minute)
	boundary := newManualMonitorTimer()
	debounce := newManualMonitorTimer()
	authorization := newManualMonitorTicker()
	updates := make(chan monitorUpdate, 4)
	monitor := newCalendarMonitor(state, store, boundary, debounce, authorization, func(_ context.Context, selected calendarRefresh, err error) {
		updates <- monitorUpdate{selected: selected.selectedEvents, err: err}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- monitor.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	_ = nextMonitorUpdate(t, updates)
	_ = nextTimerReset(t, boundary)
	store.setStatus(accessDenied)
	authorization.ticks <- now
	revoked := nextMonitorUpdate(t, updates)
	if !errors.Is(revoked.err, ErrCalendarAccess) {
		t.Fatalf("revoked update error = %v, want ErrCalendarAccess", revoked.err)
	}
	if revoked.selected.Upcoming != nil || revoked.selected.Active != nil {
		t.Fatalf("revoked update retained events: %#v", revoked.selected)
	}
	if got := store.fetchCount(); got != 1 {
		t.Fatalf("revocation performed another event query: %d", got)
	}
}

func TestCalendarMonitorStopsAllTimersOnCancellation(t *testing.T) {
	store := newMonitorEventStore(accessDenied, nil)
	state := newCalendarState(store, &fakeURLOpener{}, time.Now, 5*time.Minute)
	boundary := newManualMonitorTimer()
	debounce := newManualMonitorTimer()
	authorization := newManualMonitorTicker()
	updates := make(chan monitorUpdate, 1)
	monitor := newCalendarMonitor(state, store, boundary, debounce, authorization, func(_ context.Context, selected calendarRefresh, err error) {
		updates <- monitorUpdate{selected: selected.selectedEvents, err: err}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- monitor.Run(ctx) }()
	_ = nextMonitorUpdate(t, updates)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !boundary.stopped || !debounce.stopped || !authorization.stopped {
		t.Fatalf("stopped: boundary=%v debounce=%v authorization=%v", boundary.stopped, debounce.stopped, authorization.stopped)
	}
}

func TestCalendarMonitorRearmsBoundaryForInteractionRetry(t *testing.T) {
	store := newMonitorEventStore(accessFull, nil)
	state := newCalendarState(store, &fakeURLOpener{}, time.Now, 5*time.Minute)
	boundary := newManualMonitorTimer()
	updates := make(chan monitorUpdate, 4)
	monitor := newCalendarMonitor(state, store, boundary, newManualMonitorTimer(), newManualMonitorTicker(), func(_ context.Context, selected calendarRefresh, err error) {
		updates <- monitorUpdate{selected: selected.selectedEvents, err: err}
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- monitor.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	_ = nextMonitorUpdate(t, updates)
	if delay := nextTimerReset(t, boundary); delay <= calendarPublicationRetry {
		t.Fatalf("test requires a later event boundary, got %s", delay)
	}
	state.RetryAfter(calendarPublicationRetry)
	if delay := nextTimerReset(t, boundary); delay != calendarPublicationRetry {
		t.Fatalf("interaction persistence retry = %s, want %s", delay, calendarPublicationRetry)
	}
	boundary.ticks <- time.Now()
	_ = nextMonitorUpdate(t, updates)
	if count := store.fetchCount(); count != 2 {
		t.Fatalf("retry timer did not refresh and republish: fetches=%d", count)
	}
}

type monitorUpdate struct {
	selected selectedEvents
	err      error
}

type manualMonitorTimer struct {
	ticks   chan time.Time
	resets  chan time.Duration
	stopped bool
}

func newManualMonitorTimer() *manualMonitorTimer {
	return &manualMonitorTimer{ticks: make(chan time.Time, 4), resets: make(chan time.Duration, 4)}
}

func (t *manualMonitorTimer) C() <-chan time.Time { return t.ticks }
func (t *manualMonitorTimer) Reset(delay time.Duration) {
	t.resets <- delay
}
func (t *manualMonitorTimer) Stop() { t.stopped = true }

type manualMonitorTicker struct {
	ticks   chan time.Time
	stopped bool
}

func newManualMonitorTicker() *manualMonitorTicker {
	return &manualMonitorTicker{ticks: make(chan time.Time, 4)}
}

func (t *manualMonitorTicker) C() <-chan time.Time { return t.ticks }
func (t *manualMonitorTicker) Stop()               { t.stopped = true }

func nextMonitorUpdate(t *testing.T, updates <-chan monitorUpdate) monitorUpdate {
	t.Helper()
	select {
	case update := <-updates:
		return update
	case <-time.After(time.Second):
		t.Fatal("Calendar monitor did not publish an update")
		return monitorUpdate{}
	}
}

func nextTimerReset(t *testing.T, timer *manualMonitorTimer) time.Duration {
	t.Helper()
	select {
	case delay := <-timer.resets:
		return delay
	case <-time.After(time.Second):
		t.Fatal("Calendar monitor did not reset its timer")
		return 0
	}
}
