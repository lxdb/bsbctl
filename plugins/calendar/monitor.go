package calendar

import (
	"context"
	"time"
)

const (
	calendarChangeDebounce   = 250 * time.Millisecond
	calendarPublicationRetry = 5 * time.Second
	authorizationInterval    = time.Minute
)

type monitorTimer interface {
	C() <-chan time.Time
	Reset(time.Duration)
	Stop()
}

type monitorTicker interface {
	C() <-chan time.Time
	Stop()
}

type calendarMonitor struct {
	state         *calendarState
	store         managedEventStore
	boundary      monitorTimer
	debounce      monitorTimer
	authorization monitorTicker
	publish       func(context.Context, calendarRefresh, error)
}

func newCalendarMonitor(
	state *calendarState,
	store managedEventStore,
	boundary, debounce monitorTimer,
	authorization monitorTicker,
	publish func(context.Context, calendarRefresh, error),
) *calendarMonitor {
	if boundary == nil {
		boundary = newRealMonitorTimer()
	}
	if debounce == nil {
		debounce = newRealMonitorTimer()
	}
	if authorization == nil {
		authorization = realMonitorTicker{Ticker: time.NewTicker(authorizationInterval)}
	}
	if publish == nil {
		publish = func(context.Context, calendarRefresh, error) {}
	}
	return &calendarMonitor{
		state: state, store: store, boundary: boundary, debounce: debounce,
		authorization: authorization, publish: publish,
	}
}

func (m *calendarMonitor) Run(ctx context.Context) error {
	defer m.boundary.Stop()
	defer m.debounce.Stop()
	defer m.authorization.Stop()
	lastAccess := accessUnknown
	var boundaryAt time.Time
	resetBoundary := func(delay time.Duration) {
		boundaryAt = time.Now().Add(delay)
		m.boundary.Reset(delay)
	}
	refresh := func() {
		result, err := m.state.Refresh(ctx)
		lastAccess = m.store.AuthorizationStatus()
		m.publish(ctx, result, err)
		resetBoundary(m.state.NextRefresh())
	}
	refresh()
	changes := m.store.Changes()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-m.boundary.C():
			refresh()
		case <-m.state.retryWake:
			delay := m.state.NextRefresh()
			if time.Now().Add(delay).Before(boundaryAt) {
				resetBoundary(delay)
			}
		case _, ok := <-changes:
			if !ok {
				changes = nil
				continue
			}
			m.debounce.Reset(calendarChangeDebounce)
		case <-m.debounce.C():
			refresh()
		case <-m.authorization.C():
			if current := m.store.AuthorizationStatus(); current != lastAccess {
				refresh()
			}
		}
	}
}

type realMonitorTimer struct {
	timer *time.Timer
	armed bool
}

func newRealMonitorTimer() *realMonitorTimer {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	return &realMonitorTimer{timer: timer}
}

func (t *realMonitorTimer) C() <-chan time.Time { return t.timer.C }

func (t *realMonitorTimer) Reset(delay time.Duration) {
	if delay <= 0 {
		delay = time.Millisecond
	}
	if t.armed && !t.timer.Stop() {
		select {
		case <-t.timer.C:
		default:
		}
	}
	t.timer.Reset(delay)
	t.armed = true
}

func (t *realMonitorTimer) Stop() {
	if t.armed && !t.timer.Stop() {
		select {
		case <-t.timer.C:
		default:
		}
	}
	t.armed = false
}

type realMonitorTicker struct{ *time.Ticker }

func (t realMonitorTicker) C() <-chan time.Time { return t.Ticker.C }
