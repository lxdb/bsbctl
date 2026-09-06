package slack

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	maxEnvelopeBytes       = 256 * 1024
	connectionHelloTimeout = 10 * time.Second
)

type socketDialer func(context.Context, string) (*websocket.Conn, error)

type transportEventKind uint8

const (
	transportHello transportEventKind = iota
	transportRefresh
	transportEnded
)

type transportEvent struct {
	id       uint64
	kind     transportEventKind
	err      error
	accepted chan bool
}

type transportDial struct {
	conn *websocket.Conn
	err  error
}

type transportSession struct {
	id     uint64
	cancel context.CancelFunc
}

func (w *worker) runTransport() {
	transportCtx, cancelTransport := context.WithCancel(w.ctx)
	events := make(chan transportEvent, 16)
	dials := make(chan transportDial)
	var background sync.WaitGroup
	var active, candidate *transportSession
	var dialing bool
	var nextID uint64
	var attempt int
	var retryTimer, helloTimer *time.Timer
	var retryC, helloC <-chan time.Time

	stopRetry := func() {
		if retryTimer != nil {
			retryTimer.Stop()
		}
		retryTimer, retryC = nil, nil
	}
	stopHello := func() {
		if helloTimer != nil {
			helloTimer.Stop()
		}
		helloTimer, helloC = nil, nil
	}
	startDial := func() {
		if dialing || candidate != nil || transportCtx.Err() != nil {
			return
		}
		stopRetry()
		dialing = true
		background.Go(func() {
			ticket, err := w.client.openSocket(transportCtx, w.instance.Secrets["app_token"])
			var conn *websocket.Conn
			if err == nil {
				conn, err = w.dial(transportCtx, ticket)
				if err != nil {
					if _, safe := errors.AsType[*sourceError](err); !safe {
						err = &sourceError{code: "disconnected"}
					}
				}
			}
			select {
			case dials <- transportDial{conn: conn, err: err}:
			case <-transportCtx.Done():
				if conn != nil {
					_ = conn.CloseNow()
				}
			}
		})
	}
	scheduleRetry := func(retryAfter time.Duration) {
		if dialing || candidate != nil || retryC != nil || transportCtx.Err() != nil {
			return
		}
		delay := reconnectDelay(attempt, retryAfter, rand.Float64())
		attempt = min(attempt+1, 6)
		retryTimer = time.NewTimer(delay)
		retryC = retryTimer.C
	}
	startSession := func(conn *websocket.Conn) *transportSession {
		nextID++
		ctx, cancel := context.WithCancel(transportCtx)
		session := &transportSession{id: nextID, cancel: cancel}
		background.Go(func() {
			err := w.readManagedConnection(ctx, session.id, conn, events)
			select {
			case events <- transportEvent{id: session.id, kind: transportEnded, err: err}:
			case <-transportCtx.Done():
			}
		})
		return session
	}
	classify := func(err error) (string, time.Duration) {
		if source, ok := errors.AsType[*sourceError](err); ok {
			return source.code, source.retryAfter
		}
		return "disconnected", 0
	}
	terminal := func(code string) bool {
		return code == "auth_required" || code == "missing_scope"
	}
	defer func() {
		cancelTransport()
		stopRetry()
		stopHello()
		if active != nil {
			active.cancel()
		}
		if candidate != nil {
			candidate.cancel()
		}
		background.Wait()
	}()

	startDial()
	for w.ctx.Err() == nil && !w.requiresAuthentication() {
		select {
		case <-w.ctx.Done():
			return
		case <-retryC:
			stopRetry()
			startDial()
		case <-helloC:
			stopHello()
			if candidate == nil {
				continue
			}
			candidate.cancel()
			candidate = nil
			if active == nil {
				w.disconnected("disconnected")
			} else {
				w.recordDiagnostic("disconnected")
			}
			scheduleRetry(0)
		case result := <-dials:
			dialing = false
			if result.err != nil {
				code, retryAfter := classify(result.err)
				if terminal(code) {
					w.disconnected(code)
					return
				}
				if active == nil {
					w.disconnected(code)
				} else {
					w.recordDiagnostic(code)
				}
				scheduleRetry(retryAfter)
				continue
			}
			candidate = startSession(result.conn)
			helloTimer = time.NewTimer(connectionHelloTimeout)
			helloC = helloTimer.C
		case event := <-events:
			switch event.kind {
			case transportHello:
				if candidate == nil || event.id != candidate.id {
					event.accepted <- false
					continue
				}
				stopHello()
				old := active
				active, candidate = candidate, nil
				w.activateConnection(active.id)
				event.accepted <- true
				attempt = 0
				if old != nil {
					old.cancel()
				}
			case transportRefresh:
				switch {
				case active != nil && event.id == active.id:
					attempt = 0
					startDial()
				case candidate != nil && event.id == candidate.id:
					stopHello()
					candidate.cancel()
					candidate = nil
					if active == nil {
						w.disconnected("disconnected")
					} else {
						w.recordDiagnostic("disconnected")
					}
					attempt = 0
					startDial()
				}
			case transportEnded:
				code, retryAfter := classify(event.err)
				if terminal(code) {
					w.disconnected(code)
					return
				}
				switch {
				case active != nil && event.id == active.id:
					w.connectionDisconnected(active.id, code)
					active = nil
					if candidate == nil && !dialing {
						scheduleRetry(retryAfter)
					}
				case candidate != nil && event.id == candidate.id:
					stopHello()
					candidate = nil
					if active == nil {
						w.disconnected(code)
					} else {
						w.recordDiagnostic(code)
					}
					scheduleRetry(retryAfter)
				}
			}
		}
	}
}

func reconnectDelay(attempt int, retryAfter time.Duration, jitter float64) time.Duration {
	base := min(5*time.Second*time.Duration(1<<min(max(attempt, 0), 6)), 5*time.Minute)
	return max(min(base+time.Duration(float64(base)*0.2*jitter), 5*time.Minute), retryAfter)
}

type socketCallbacks struct {
	requireHello bool
	heartbeat    <-chan time.Time
	hello        func() bool
	live         func()
	gap          func(string, bool, bool)
	refresh      func() bool
	revoke       func()
}

func (w *worker) readManagedConnection(parent context.Context, id uint64, conn *websocket.Conn, events chan<- transportEvent) error {
	planned, err := w.readSocket(parent, conn, socketCallbacks{
		requireHello: true,
		hello: func() bool {
			accepted := make(chan bool, 1)
			select {
			case events <- transportEvent{id: id, kind: transportHello, accepted: accepted}:
			case <-parent.Done():
				return false
			}
			select {
			case ok := <-accepted:
				return ok
			case <-parent.Done():
				return false
			}
		},
		live: func() { w.connectionLive(id) },
		gap:  func(code string, drop, diagnostic bool) { w.connectionGap(id, code, drop, diagnostic) },
		refresh: func() bool {
			select {
			case events <- transportEvent{id: id, kind: transportRefresh}:
				return true
			case <-parent.Done():
				return false
			}
		},
		revoke: func() { w.disconnected("auth_required") },
	})
	if planned && err == nil {
		return &sourceError{code: "disconnected"}
	}
	return err
}

// readSocket exclusively owns one socket and its ACK path. Nothing in this
// reader waits for the reducer, disk checkpoint, publication, or user interaction.
func (w *worker) readSocket(parent context.Context, conn *websocket.Conn, callbacks socketCallbacks) (bool, error) {
	ctx, cancel := context.WithCancel(parent)
	established := false
	conn.SetReadLimit(maxEnvelopeBytes)
	heartbeat := callbacks.heartbeat
	var heartbeatTicker *time.Ticker
	if heartbeat == nil {
		heartbeatTicker = time.NewTicker(15 * time.Second)
		heartbeat = heartbeatTicker.C
	}
	var ping sync.WaitGroup
	ping.Go(func() {
		if heartbeatTicker != nil {
			defer heartbeatTicker.Stop()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat:
				probe, stop := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(probe)
				stop()
				if err != nil {
					cancel()
					return
				}
				callbacks.live()
			}
		}
	})
	defer func() { cancel(); _ = conn.CloseNow(); ping.Wait() }()
	for {
		kind, raw, err := conn.Read(ctx)
		if err != nil {
			callbacks.gap("disconnected", false, false)
			return false, &sourceError{code: "disconnected"}
		}
		var envelope struct {
			Type       string          `json:"type"`
			EnvelopeID string          `json:"envelope_id"`
			Reason     string          `json:"reason"`
			Payload    json.RawMessage `json:"payload"`
			Connection struct {
				AppID string `json:"app_id"`
			} `json:"connection_info"`
		}
		if kind != websocket.MessageText || json.Unmarshal(raw, &envelope) != nil || len(envelope.EnvelopeID) > 128 {
			callbacks.gap("invalid_envelope", false, false)
			return false, &sourceError{code: "invalid_envelope"}
		}
		if envelope.EnvelopeID != "" {
			ack, _ := json.Marshal(struct {
				EnvelopeID string `json:"envelope_id"`
			}{envelope.EnvelopeID})
			ackCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			err := conn.Write(ackCtx, websocket.MessageText, ack)
			stop()
			if err != nil {
				return false, &sourceError{code: "disconnected"}
			}
		}
		switch envelope.Type {
		case "hello":
			if envelope.EnvelopeID != "" || envelope.Connection.AppID != w.cfg.appID {
				return false, &sourceError{code: "auth_required"}
			}
			if !callbacks.hello() {
				return false, context.Canceled
			}
			established = true
		case "disconnect":
			if envelope.Reason == "warning" || envelope.Reason == "refresh_requested" {
				if callbacks.refresh() {
					continue
				}
				return true, nil
			}
			return false, &sourceError{code: "disconnected"}
		case "events_api":
			if callbacks.requireHello && !established {
				return false, &sourceError{code: "invalid_envelope"}
			}
			if envelope.EnvelopeID == "" || len(envelope.Payload) == 0 {
				callbacks.gap("invalid_envelope", false, true)
				continue
			}
			if w.revokesAuthorization(envelope.Payload) {
				callbacks.revoke()
				return false, &sourceError{code: "auth_required"}
			}
			callbacks.live()
			select {
			case w.queue <- envelope.Payload:
			default:
				callbacks.gap("queue_overflow", true, true)
			}
		default:
			callbacks.gap("unsupported_envelope", false, true)
		}
	}
}

// Lifecycle invalidation is app-bound transport metadata, not permission to
// retain message content. Slack may omit authorizations on tokens_revoked.
func (w *worker) revokesAuthorization(raw json.RawMessage) bool {
	if len(raw) == 0 || len(raw) > maxEnvelopeBytes {
		return false
	}
	var callback struct {
		Type     string `json:"type"`
		APIAppID string `json:"api_app_id"`
		TeamID   string `json:"team_id"`
		Event    struct {
			Type   string `json:"type"`
			Tokens struct {
				OAuth []string `json:"oauth"`
			} `json:"tokens"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &callback) != nil || callback.Type != "event_callback" || callback.APIAppID != w.cfg.appID || callback.TeamID != w.cfg.workspaceID {
		return false
	}
	switch callback.Event.Type {
	case "tokens_revoked":
		return slices.Contains(callback.Event.Tokens.OAuth, w.cfg.userID)
	case "app_uninstalled":
		return true
	default:
		return false
	}
}
