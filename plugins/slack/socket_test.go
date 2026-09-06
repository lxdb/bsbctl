package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

// live drives transport-independent worker state in focused tests. Production
// liveness is tied to an accepted managed connection.
func (w *worker) live() {
	w.transportMu.Lock()
	w.liveLocked()
	w.transportMu.Unlock()
	w.notify()
}

// readConnection exposes the socket reader without the production connection
// manager so focused tests can control a single socket directly.
func (w *worker) readConnection(parent context.Context, conn *websocket.Conn) (bool, error) {
	return w.readConnectionWithHeartbeat(parent, conn, nil)
}

func (w *worker) readConnectionWithHeartbeat(parent context.Context, conn *websocket.Conn, heartbeat <-chan time.Time) (bool, error) {
	return w.readSocket(parent, conn, socketCallbacks{
		heartbeat: heartbeat,
		hello:     func() bool { w.live(); return true },
		live:      w.live,
		gap: func(code string, drop, diagnostic bool) {
			if diagnostic {
				w.markGap(code, drop)
			} else {
				w.setGap(code, drop)
			}
		},
		refresh: func() bool { return false },
		revoke:  func() { w.disconnected("auth_required") },
	})
}

func waitSnapshot(t *testing.T, w *worker, condition func(workerSnapshot) bool) workerSnapshot {
	t.Helper()
	timeout := time.NewTimer(20 * time.Second)
	defer timeout.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		snap := w.snapshot()
		if condition(snap) {
			return snap
		}
		select {
		case <-poll.C:
		case <-timeout.C:
			snap = w.snapshot()
			if condition(snap) {
				return snap
			}
			t.Fatalf("worker transition missing: %+v", snap)
		}
	}
}

func socketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		accepted <- conn
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, strings.Replace(server.URL, "http:", "ws:", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	peer := <-accepted
	t.Cleanup(func() { _ = client.CloseNow(); _ = peer.CloseNow() })
	return client, peer
}

func TestSocketHelloBindsConfiguredSlackApp(t *testing.T) {
	for _, tc := range []struct {
		name, appID string
		accepted    bool
	}{
		{"matching app", "A123", true},
		{"different app", "A999", false},
		{"missing app", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, peer := socketPair(t)
			cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123"}`))
			w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, nil, nil, nil, time.Now)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { _, err := w.readConnection(ctx, client); done <- err }()
			raw, _ := json.Marshal(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": tc.appID}})
			if err := peer.Write(ctx, websocket.MessageText, raw); err != nil {
				t.Fatal(err)
			}
			if tc.accepted {
				waitSnapshot(t, w, func(s workerSnapshot) bool { return s.Fresh })
				cancel()
				<-done
				return
			}
			err := <-done
			source, ok := errors.AsType[*sourceError](err)
			if !ok || source.code != "auth_required" {
				t.Fatalf("wrong app hello error = %v", err)
			}
			cancel()
		})
	}
}

func TestSocketPlannedRefreshReturnsToReadyCoverage(t *testing.T) {
	first, peer1 := socketPair(t)
	second, peer2 := socketPair(t)
	var tickets atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"url":"wss://wss-primary.slack.com/socket?ticket=%d"}`, tickets.Add(1))
	})
	dials := make(chan int, 2)
	dial := func(_ context.Context, ticket string) (*websocket.Conn, error) {
		if strings.HasSuffix(ticket, "=1") {
			dials <- 1
			return first, nil
		}
		dials <- 2
		return second, nil
	}
	cfg, err := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123"}`))
	if err != nil {
		t.Fatal(err)
	}
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"app_token": "app-canary"}}, cfg, &checkpointHost{}, client, dial, time.Now)
	go w.run()
	t.Cleanup(func() { w.cancel(); <-w.done })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case got := <-dials:
		if got != 1 {
			t.Fatalf("initial dial used connection %d", got)
		}
	case <-ctx.Done():
		t.Fatal("initial connection missing")
	}
	if err := peer1.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","connection_info":{"app_id":"A123"}}`)); err != nil {
		t.Fatal(err)
	}
	initial := waitSnapshot(t, w, func(s workerSnapshot) bool { return s.Fresh && s.Phase == "ready" })
	if err := peer1.Write(ctx, websocket.MessageText, []byte(`{"type":"disconnect","reason":"refresh_requested"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-dials:
		if got != 2 {
			t.Fatalf("refresh dial used connection %d", got)
		}
	case <-ctx.Done():
		t.Fatal("refresh connection missing")
	}
	sendAuthorizationEnvelope(t, ctx, peer1, "during-refresh", callback("Ev1", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"during refresh"}`))
	waitSnapshot(t, w, func(s workerSnapshot) bool { return len(s.Items) == 1 })
	if err := peer2.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","connection_info":{"app_id":"A123"}}`)); err != nil {
		t.Fatal(err)
	}
	snap := waitSnapshot(t, w, func(s workerSnapshot) bool { return s.LastSuccess.After(initial.LastSuccess) })
	if snap.Phase != "ready" || snap.Gap || !snap.Fresh {
		t.Fatalf("successful planned refresh degraded coverage: %+v", snap)
	}
	if _, _, err := peer1.Read(ctx); err == nil {
		t.Fatal("old connection remained active after replacement hello")
	}
}

func TestSocketRefreshRejectsUnestablishedCandidateAndTracksLaterLoss(t *testing.T) {
	first, peer1 := socketPair(t)
	second, peer2 := socketPair(t)
	third, peer3 := socketPair(t)
	var tickets atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"url":"wss://wss-primary.slack.com/socket?ticket=%d"}`, tickets.Add(1))
	})
	dials := make(chan int, 3)
	dial := func(_ context.Context, ticket string) (*websocket.Conn, error) {
		n := int(ticket[len(ticket)-1] - '0')
		dials <- n
		switch n {
		case 1:
			return first, nil
		case 2:
			return second, nil
		default:
			return third, nil
		}
	}
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123"}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"app_token": "app-canary"}}, cfg, &checkpointHost{}, client, dial, time.Now)
	go w.run()
	t.Cleanup(func() { w.cancel(); <-w.done })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if got := <-dials; got != 1 {
		t.Fatalf("initial dial used connection %d", got)
	}
	if err := peer1.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","connection_info":{"app_id":"A123"}}`)); err != nil {
		t.Fatal(err)
	}
	waitSnapshot(t, w, func(s workerSnapshot) bool { return s.Phase == "ready" })
	if err := peer1.Write(ctx, websocket.MessageText, []byte(`{"type":"disconnect","reason":"refresh_requested"}`)); err != nil {
		t.Fatal(err)
	}
	if got := <-dials; got != 2 {
		t.Fatalf("first replacement used connection %d", got)
	}
	if err := peer2.Write(ctx, websocket.MessageText, []byte(`{"type":"disconnect","reason":"refresh_requested"}`)); err != nil {
		t.Fatal(err)
	}
	if got := <-dials; got != 3 {
		t.Fatalf("second replacement used connection %d", got)
	}
	sendAuthorizationEnvelope(t, ctx, peer1, "still-active", callback("Ev1", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"still active"}`))
	continuous := waitSnapshot(t, w, func(s workerSnapshot) bool { return len(s.Items) == 1 })
	if continuous.Gap || continuous.Phase != "ready" {
		t.Fatalf("failed candidate interrupted active coverage: %+v", continuous)
	}
	if err := peer1.CloseNow(); err != nil {
		t.Fatal(err)
	}
	waitSnapshot(t, w, func(s workerSnapshot) bool { return s.Gap })
	if err := peer3.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","connection_info":{"app_id":"A123"}}`)); err != nil {
		t.Fatal(err)
	}
	snap := waitSnapshot(t, w, func(s workerSnapshot) bool { return s.Fresh })
	if !snap.Gap || snap.Phase != "degraded" {
		t.Fatalf("replacement promotion erased coverage loss: %+v", snap)
	}
}

func TestSocketAcknowledgesOverflowWithoutReducerOrPersistence(t *testing.T) {
	client, peer := socketPair(t)
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123"}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, nil, nil, nil, time.Now)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := w.readConnection(ctx, client); done <- err }()
	ioctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	for i := range 257 {
		raw := fmt.Sprintf(`{"type":"events_api","envelope_id":"ack-%d","payload":{"type":"event_callback"}}`, i)
		if err := peer.Write(ioctx, websocket.MessageText, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		_, ack, err := peer.Read(ioctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(ack) != fmt.Sprintf(`{"envelope_id":"ack-%d"}`, i) {
			t.Fatalf("wrong ACK %s", ack)
		}
	}
	waitSnapshot(t, w, func(s workerSnapshot) bool { return s.Dropped == 1 })
	if len(w.queue) != 256 || !w.snapshot().Gap {
		t.Fatalf("overflow not bounded: queued=%d snapshot=%+v", len(w.queue), w.snapshot())
	}
	// Reader must service control frames even when no reducer can drain the queue.
	peerRead := make(chan struct{})
	go func() { _, _, _ = peer.Read(ioctx); close(peerRead) }()
	if err := peer.Ping(ioctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = peer.CloseNow()
	<-peerRead
	select {
	case <-done:
	case <-ioctx.Done():
		t.Fatal("reader did not join")
	}
}

func TestSocketMalformedUnsupportedAndOversizeAreBounded(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      string
	}{
		{"invalid JSON", `{"canary":`, "invalid_envelope"},
		{"unsupported envelope", `{"type":"interactive","envelope_id":"a","payload":{"private":"canary"}}`, "unsupported_envelope"},
		{"oversize", strings.Repeat("canary", 50000), "disconnected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, peer := socketPair(t)
			w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, config{configured: true}, nil, nil, nil, time.Now)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() { _, _ = w.readConnection(ctx, client); close(done) }()
			_ = peer.Write(ctx, websocket.MessageText, []byte(tc.raw))
			if tc.name == "unsupported envelope" {
				_, _, _ = peer.Read(ctx)
			} else {
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("malformed reader stuck")
				}
			}
			snap := waitSnapshot(t, w, func(s workerSnapshot) bool { return s.Gap })
			raw, _ := json.Marshal(snap)
			if !snap.Gap || strings.Contains(string(raw), "canary") {
				t.Fatalf("diagnostic leaked/missing gap: %s", raw)
			}
			cancel()
			<-done
		})
	}
}

func TestSocketAcknowledgesWhileCheckpointSaveBlocksReducer(t *testing.T) {
	socket, peer := socketPair(t)
	apiClient := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"url":"wss://wss-primary.slack.com/socket?ticket=1"}`)
	})
	saving := make(chan struct{}, 1)
	release := make(chan struct{})
	host := &checkpointHost{save: func(ctx context.Context, r protocol.CheckpointRequest) error {
		select {
		case saving <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123"}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"app_token": "canary"}}, cfg, host, apiClient, func(context.Context, string) (*websocket.Conn, error) {
		return socket, nil
	}, time.Now)
	go w.run()
	defer func() { w.cancel(); <-w.done }()
	ioctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := peer.Write(ioctx, websocket.MessageText, []byte(`{"type":"hello","connection_info":{"app_id":"A123"}}`)); err != nil {
		t.Fatal(err)
	}
	waitSnapshot(t, w, func(s workerSnapshot) bool { return s.Phase == "ready" })
	original := callback("Ev1", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"canary-body"}`)
	sendAuthorizationEnvelope(t, ioctx, peer, "first", original)
	waitSnapshot(t, w, func(s workerSnapshot) bool { return len(s.Items) == 1 })
	select {
	case <-saving:
	case <-ioctx.Done():
		t.Fatal("coalesced save not reached")
	}
	sendAuthorizationEnvelope(t, ioctx, peer, "while-saving", original)
	if snap := w.snapshot(); len(snap.Items) != 1 {
		t.Fatalf("blocked checkpoint changed cached snapshot: %+v", snap)
	}
	close(release)
}

func TestSocketIdlePongRenewsLivenessWithoutClearingCoverageGap(t *testing.T) {
	client, peer := socketPair(t)
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123"}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, nil, nil, nil, time.Now)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	done := make(chan error, 1)
	heartbeat := make(chan time.Time, 1)
	go func() { _, err := w.readConnectionWithHeartbeat(ctx, client, heartbeat); done <- err }()
	if err := peer.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","connection_info":{"app_id":"A123"}}`)); err != nil {
		t.Fatal(err)
	}
	initial := waitSnapshot(t, w, func(s workerSnapshot) bool { return s.Fresh })
	w.setGap("queue_overflow", true)
	remoteDone := make(chan struct{})
	go func() { _, _, _ = peer.Read(ctx); close(remoteDone) }()
	heartbeat <- time.Now()
	renewed := waitSnapshot(t, w, func(s workerSnapshot) bool { return s.LastSuccess.After(initial.LastSuccess) })
	if !renewed.Fresh || renewed.Phase != "degraded" || !renewed.Gap || renewed.Dropped != 1 {
		t.Fatalf("idle pong changed coverage state: %+v", renewed)
	}
	cancel()
	_ = peer.CloseNow()
	<-done
	<-remoteDone
}

// The original callback stays inside the 1024-callback window after both its
// logical message and its 128-entry recovery fingerprint have been evicted.
func seedEvictedReplay(t *testing.T, w *worker) json.RawMessage {
	t.Helper()
	original := callback("original", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"original"}`)
	w.reduce(original)
	for i := 2; i <= 130; i++ {
		w.reduce(callback(fmt.Sprintf("newer-%d", i), fmt.Sprintf(`{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"%d.000001","text":"newer"}`, i)))
	}
	w.mu.Lock()
	for _, fingerprint := range w.state.fingerprints {
		if fingerprint.Version == "1.000001" {
			w.mu.Unlock()
			t.Fatal("original fingerprint still masks FIFO loss")
		}
	}
	w.mu.Unlock()
	assertReplaySurvivors(t, w)
	return original
}

func assertReplaySurvivors(t *testing.T, w *worker) {
	t.Helper()
	items := w.snapshot().Items
	if len(items) != 128 {
		t.Fatalf("want 128 independent survivors, got %d", len(items))
	}
	survivors := make(map[string]bool)
	for _, item := range items {
		survivors[item.MessageTS] = true
	}
	for i := 3; i <= 130; i++ {
		if !survivors[fmt.Sprintf("%d.000001", i)] {
			t.Fatalf("replay displaced expected survivor %d; original present=%t", i, survivors["1.000001"])
		}
	}
}

func TestSocketReconnectPreservesCallbackFIFOAfterLogicalEviction(t *testing.T) {
	first, peer1 := socketPair(t)
	second, peer2 := socketPair(t)
	var tickets atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"url":"wss://wss-primary.slack.com/socket?ticket=%d"}`, tickets.Add(1))
	})
	dials := make(chan struct{}, 2)
	dial := func(ctx context.Context, ticket string) (*websocket.Conn, error) {
		dials <- struct{}{}
		if strings.HasSuffix(ticket, "=1") {
			return first, nil
		}
		return second, nil
	}
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123"}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, &checkpointHost{}, client, dial, time.Now)
	original := seedEvictedReplay(t, w)
	go w.run()
	defer func() { w.cancel(); <-w.done }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-dials:
	case <-ctx.Done():
		t.Fatal("initial socket missing")
	}
	if err := peer1.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","connection_info":{"app_id":"A123"}}`)); err != nil {
		t.Fatal(err)
	}
	waitSnapshot(t, w, func(s workerSnapshot) bool { return s.Phase == "ready" })
	if err := peer1.Write(ctx, websocket.MessageText, []byte(`{"type":"disconnect","reason":"refresh_requested"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dials:
	case <-ctx.Done():
		t.Fatal("reconnect missing")
	}
	if err := peer2.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","connection_info":{"app_id":"A123"}}`)); err != nil {
		t.Fatal(err)
	}
	// The second queued callback provides a reducer barrier without changing any
	// retained logical message; ACK alone cannot prove the replay was reduced.
	for i, payload := range []json.RawMessage{original, callback("barrier", `{"type":"unsupported"}`)} {
		raw, _ := json.Marshal(struct {
			Type    string          `json:"type"`
			ID      string          `json:"envelope_id"`
			Payload json.RawMessage `json:"payload"`
		}{"events_api", fmt.Sprint(i), payload})
		if err := peer2.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
		if _, _, err := peer2.Read(ctx); err != nil {
			t.Fatal(err)
		}
	}
	waitSnapshot(t, w, func(s workerSnapshot) bool { return s.ErrorCode == "unsupported_event" })
	assertReplaySurvivors(t, w)
}
