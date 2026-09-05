package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func (p *Peer) droppedInboundNotifications() uint64 { return p.droppedInbound.Load() }

func TestNotifyCancellationInterruptsBlockedWriteAndClosesPeer(t *testing.T) {
	var newPeer func(net.Conn) *Peer = NewPeer
	leftConn, rightConn := net.Pipe()
	started := make(chan struct{})
	peer := newPeer(&writeBarrierConn{Conn: leftConn, started: started})
	t.Cleanup(func() {
		_ = peer.Close()
		_ = rightConn.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- peer.Notify(ctx, "blocked", nil) }()
	<-started
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Notify error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Notify remained blocked after context cancellation")
	}
	select {
	case <-peer.Done():
	case <-time.After(time.Second):
		t.Fatal("peer remained open after blocked write cancellation")
	}
}

func TestQueuedCriticalWriteCancellationClosesPeer(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	started := make(chan struct{})
	peer := NewPeer(&writeBarrierConn{Conn: leftConn, started: started})
	t.Cleanup(func() {
		_ = peer.Close()
		_ = rightConn.Close()
	})
	firstResult := make(chan error, 1)
	go func() { firstResult <- peer.Notify(context.Background(), "first", nil) }()
	<-started

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondResult := make(chan error, 1)
	go func() { secondResult <- peer.Notify(secondCtx, "second", nil) }()
	var accepted outboundWrite
	select {
	case accepted = <-peer.writes:
	case <-time.After(time.Second):
		t.Fatal("second critical write was not accepted into the queue")
	}
	peer.writes <- accepted
	cancelSecond()

	select {
	case err := <-secondResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second Notify error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued critical write did not return after cancellation")
	}
	select {
	case <-peer.Done():
	case <-time.After(time.Second):
		t.Fatal("queued critical cancellation did not close the peer")
	}
	select {
	case <-firstResult:
	case <-time.After(time.Second):
		t.Fatal("peer termination did not unblock the active write")
	}
}

func TestTryNotifyLossyDropsWhenBoundedQueueIsFullWithoutClosingPeer(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	started := make(chan struct{})
	peer := NewPeer(&writeBarrierConn{Conn: leftConn, started: started})
	t.Cleanup(func() {
		_ = peer.Close()
		_ = rightConn.Close()
	})

	queued, err := peer.TryNotifyLossy("metric", map[string]any{"value": 0})
	if err != nil || !queued {
		t.Fatalf("first lossy notification = %v, %v, want queued", queued, err)
	}
	<-started
	for i := 0; i < outboundQueueCapacity; i++ {
		queued, err = peer.TryNotifyLossy("metric", map[string]any{"value": i + 1})
		if err != nil || !queued {
			t.Fatalf("lossy notification %d = %v, %v, want queued", i+1, queued, err)
		}
	}
	queued, err = peer.TryNotifyLossy("metric", map[string]any{"value": "dropped"})
	if err != nil || queued {
		t.Fatalf("full-queue notification = %v, %v, want observable drop", queued, err)
	}
	select {
	case <-peer.Done():
		t.Fatal("queue-full lossy drop closed the peer")
	default:
	}
}

func TestCriticalResponseQueueTimeoutClosesPeer(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	started := make(chan struct{})
	peer := NewPeer(&writeBarrierConn{Conn: leftConn, started: started})
	t.Cleanup(func() {
		_ = peer.Close()
		_ = rightConn.Close()
	})
	queued, err := peer.TryNotifyLossy("metric", map[string]any{"value": "active"})
	if err != nil || !queued {
		t.Fatalf("active write = %v, %v, want queued", queued, err)
	}
	<-started
	for i := 0; i < outboundQueueCapacity; i++ {
		queued, err = peer.TryNotifyLossy("metric", map[string]any{"value": i})
		if err != nil || !queued {
			t.Fatalf("queue fill %d = %v, %v", i, queued, err)
		}
	}

	peer.writeResponseWithin(context.Background(), message{
		JSONRPC: Version, ID: json.RawMessage(`"response"`), Result: json.RawMessage(`null`),
	}, 10*time.Millisecond)
	select {
	case <-peer.Done():
	case <-time.After(time.Second):
		t.Fatal("critical response admission timeout did not close peer")
	}
}

func TestAcceptedLossyWriteUsesBoundedDeadlineAndFailureClosesPeer(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	deadlines := make(chan time.Time, 2)
	conn := &deadlineRejectingConn{Conn: leftConn, deadlines: deadlines}
	peer := NewPeer(conn)
	t.Cleanup(func() {
		_ = peer.Close()
		_ = rightConn.Close()
	})
	queued, err := peer.TryNotifyLossy("metric", map[string]any{"value": 1})
	if err != nil || !queued {
		t.Fatalf("TryNotifyLossy = %v, %v, want queued", queued, err)
	}
	select {
	case deadline := <-deadlines:
		if deadline.IsZero() {
			t.Fatal("accepted lossy write used no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > 2*time.Second {
			t.Fatalf("lossy write deadline remaining = %v, want within two seconds", remaining)
		}
	case <-time.After(time.Second):
		t.Fatal("lossy write did not configure a deadline")
	}
	select {
	case <-peer.Done():
	case <-time.After(time.Second):
		t.Fatal("admitted lossy I/O failure did not close peer")
	}
}

func TestBlockedWriteDoesNotDelayUnrelatedPeer(t *testing.T) {
	blockedLeft, blockedRight := net.Pipe()
	blockedStarted := make(chan struct{})
	blocked := NewPeer(&writeBarrierConn{Conn: blockedLeft, started: blockedStarted})
	blockedResult := make(chan error, 1)
	blockedCtx, cancelBlocked := context.WithCancel(context.Background())
	go func() { blockedResult <- blocked.Notify(blockedCtx, "blocked", nil) }()
	<-blockedStarted

	independentLeft, independentRight := net.Pipe()
	independent := NewPeer(independentLeft)
	readDone := make(chan error, 1)
	go func() {
		_, err := bufio.NewReader(independentRight).ReadBytes('\n')
		readDone <- err
	}()
	writeDone := make(chan error, 1)
	go func() { writeDone <- independent.Notify(context.Background(), "ready", nil) }()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("independent Notify: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated peer was delayed by blocked writer")
	}
	if err := <-readDone; err != nil {
		t.Fatalf("read independent notification: %v", err)
	}

	cancelBlocked()
	_ = blocked.Close()
	_ = independent.Close()
	_ = blockedRight.Close()
	_ = independentRight.Close()
	<-blockedResult
}

func TestPeerCloseIsRepeatableAndJoinsBlockedWriter(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	started := make(chan struct{})
	peer := NewPeer(&writeBarrierConn{Conn: leftConn, started: started})
	writeDone := make(chan error, 1)
	go func() { writeDone <- peer.Notify(context.Background(), "blocked", nil) }()
	<-started

	closeDone := make(chan error, 1)
	go func() { closeDone <- peer.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("first Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not join the blocked writer")
	}
	if err := peer.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock the active writer caller")
	}
	_ = rightConn.Close()
}

func TestSaturatedInboundMethodsPreserveCriticalAndExplicitLossySemantics(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	left := NewPeer(leftConn)
	right := NewPeer(rightConn)
	release := make(chan struct{})
	started := make(chan struct{}, maxHandlers)
	t.Cleanup(func() {
		close(release)
		_ = left.Close()
		_ = right.Close()
	})
	if err := left.HandleLossyNotification("occupy", func(context.Context, json.RawMessage) (any, *Error) {
		started <- struct{}{}
		<-release
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := left.Handle("critical", func(context.Context, json.RawMessage) (any, *Error) {
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := right.Handle("trigger", func(ctx context.Context, _ json.RawMessage) (any, *Error) {
		err := right.Call(ctx, "occupy", nil, nil)
		rpcErr, ok := errors.AsType[*Error](err)
		if !ok || rpcErr.Code != -32603 {
			return nil, &Error{Code: -32603, Message: "extra request was not rejected"}
		}
		return "delivered", nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = left.Serve(ctx) }()
	go func() { _ = right.Serve(ctx) }()

	for i := 0; i < maxHandlers; i++ {
		notifyCtx, notifyCancel := context.WithTimeout(ctx, time.Second)
		err := right.Notify(notifyCtx, "occupy", map[string]any{"value": i})
		notifyCancel()
		if err != nil {
			t.Fatalf("occupy notification %d: %v", i, err)
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("handler %d did not start", i)
		}
	}
	for i := 0; i < maxHandlers+2; i++ {
		dropCtx, dropCancel := context.WithTimeout(ctx, time.Second)
		err := right.Notify(dropCtx, "occupy", map[string]any{"value": "queued-or-dropped"})
		dropCancel()
		if err != nil {
			t.Fatalf("saturated notification %d write: %v", i, err)
		}
	}

	callCtx, callCancel := context.WithTimeout(ctx, time.Second)
	defer callCancel()
	var result string
	if err := left.Call(callCtx, "trigger", nil, &result); err != nil {
		t.Fatalf("Call while handlers saturated: %v", err)
	}
	if result != "delivered" {
		t.Fatalf("result = %q, want delivered", result)
	}
	if dropped := left.droppedInboundNotifications(); dropped < 1 || dropped > 2 {
		t.Fatalf("dropped inbound notifications = %d, want bounded backlog with 1 or 2 drops", dropped)
	}

	criticalCtx, criticalCancel := context.WithTimeout(ctx, time.Second)
	criticalWriteErr := right.Notify(criticalCtx, "critical", nil)
	criticalCancel()
	select {
	case <-left.Done():
	case <-time.After(time.Second):
		t.Fatal("overflowing critical notification did not terminate receiver")
	}
	if err := left.terminalError(); !errors.Is(err, ErrInboundSaturated) {
		t.Fatalf("receiver terminal error = %v, want inbound saturation", err)
	}
	select {
	case <-right.Done():
	case <-time.After(time.Second):
		t.Fatalf("sender did not observe receiver connection termination (write error: %v)", criticalWriteErr)
	}
}

type writeBarrierConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

type cancelAfterFullWriteConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *cancelAfterFullWriteConn) Write(data []byte) (int, error) {
	c.cancel()
	return len(data), nil
}

func (c *cancelAfterFullWriteConn) SetWriteDeadline(time.Time) error {
	return nil
}

type shortWriteConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *shortWriteConn) Write(data []byte) (int, error) {
	return len(data) - 1, nil
}

func (c *shortWriteConn) SetWriteDeadline(time.Time) error {
	return nil
}

func (c *shortWriteConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

type deadlineRejectingConn struct {
	net.Conn
	mu        sync.Mutex
	deadline  time.Time
	deadlines chan time.Time
}

func (c *deadlineRejectingConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadline = deadline
	c.mu.Unlock()
	c.deadlines <- deadline
	return nil
}

func (c *deadlineRejectingConn) Write(data []byte) (int, error) {
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	if !deadline.IsZero() {
		return 0, os.ErrDeadlineExceeded
	}
	return len(data), nil
}

func (c *writeBarrierConn) Write(data []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(data)
}

func TestWriteDataTreatsFullWriteAsSentWhenContextCancelsAtReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	peer := &Peer{conn: &cancelAfterFullWriteConn{cancel: cancel}}

	if err := peer.writeData(ctx, []byte("{}\n")); err != nil {
		t.Fatalf("writeData error = %v, want full write accepted", err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("context error = %v, want canceled after full write", ctx.Err())
	}
}

func TestPartialWriteTerminatesPeerWithoutAdvancingHighWater(t *testing.T) {
	conn := &shortWriteConn{closed: make(chan struct{})}
	peer := NewPeer(conn)
	t.Cleanup(func() { _ = peer.Close() })

	err := peer.Call(t.Context(), "test", struct{}{}, nil)
	if !errors.Is(err, io.ErrShortWrite) || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("Call error = %v, want short write with unknown outcome", err)
	}
	if written := peer.highestWrittenID.Load(); written != 0 {
		t.Fatalf("highest written id = %d, want 0", written)
	}
	select {
	case <-peer.Done():
	case <-time.After(time.Second):
		t.Fatal("partial write did not terminate peer")
	}
}

func TestPeerSupportsBidirectionalNestedCalls(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	left := NewPeer(leftConn)
	right := NewPeer(rightConn)
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })

	if err := left.Handle("host.decorate", func(_ context.Context, raw json.RawMessage) (any, *Error) {
		var request struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, &Error{Code: -32602, Message: "invalid params"}
		}
		return map[string]string{"value": "[" + request.Value + "]"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := right.Handle("plugin.render", func(ctx context.Context, raw json.RawMessage) (any, *Error) {
		var request struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, &Error{Code: -32602, Message: "invalid params"}
		}
		var decorated struct {
			Value string `json:"value"`
		}
		if err := right.Call(ctx, "host.decorate", map[string]string{"value": strings.ToUpper(request.Value)}, &decorated); err != nil {
			return nil, &Error{Code: -32603, Message: err.Error()}
		}
		return decorated, nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErrs := make(chan error, 2)
	go func() { serveErrs <- left.Serve(ctx) }()
	go func() { serveErrs <- right.Serve(ctx) }()

	callCtx, callCancel := context.WithTimeout(ctx, time.Second)
	defer callCancel()
	var got struct {
		Value string `json:"value"`
	}
	if err := left.Call(callCtx, "plugin.render", map[string]string{"value": "hello"}, &got); err != nil {
		t.Fatalf("call: %v", err)
	}
	if got.Value != "[HELLO]" {
		t.Fatalf("result = %q, want [HELLO]", got.Value)
	}
}

func TestCallRejectsNonEmptyResultWhenCallerExpectsEmptyResponse(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	left := NewPeer(leftConn)
	right := NewPeer(rightConn)
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })
	if err := right.Handle("empty", func(context.Context, json.RawMessage) (any, *Error) {
		return map[string]bool{"unexpected": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := right.Handle("empty-null", func(context.Context, json.RawMessage) (any, *Error) {
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = left.Serve(ctx) }()
	go func() { _ = right.Serve(ctx) }()

	err := left.CallEmpty(t.Context(), "empty", nil)
	if err == nil || !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("Call error = %v, want invalid empty response", err)
	}
	err = left.CallEmpty(t.Context(), "empty-null", nil)
	if err == nil || !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("Call null error = %v, want invalid empty response", err)
	}
}

func TestPeerReturnsMethodErrorAndHonorsCancellation(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	left := NewPeer(leftConn)
	right := NewPeer(rightConn)
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = left.Serve(ctx) }()
	go func() { _ = right.Serve(ctx) }()

	callCtx, callCancel := context.WithTimeout(ctx, time.Second)
	defer callCancel()
	err := left.Call(callCtx, "missing", nil, nil)
	rpcErr, ok := errors.AsType[*Error](err)
	if !ok || rpcErr.Code != -32601 {
		t.Fatalf("error = %v, want method-not-found RPC error", err)
	}

	blocked := make(chan struct{})
	if err := right.Handle("blocked", func(ctx context.Context, _ json.RawMessage) (any, *Error) {
		close(blocked)
		<-ctx.Done()
		return nil, &Error{Code: -32000, Message: "cancelled"}
	}); err != nil {
		t.Fatal(err)
	}
	cancelled, stop := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() { result <- left.Call(cancelled, "blocked", nil, nil) }()
	<-blocked
	stop()
	if err := <-result; !errors.Is(err, context.Canceled) || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("cancelled call = %v, want context cancellation with unknown outcome", err)
	}
}

func TestCanceledCallAcceptsOnlyCanonicalWrittenHistoricResponse(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	left := NewPeer(leftConn)
	right := NewPeer(rightConn)
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })

	started := make(chan struct{}, 1)
	if err := right.Handle("blocked", func(ctx context.Context, _ json.RawMessage) (any, *Error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, &Error{Code: -32000, Message: "cancelled"}
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = left.Serve(ctx) }()
	go func() { _ = right.Serve(ctx) }()

	callCtx, cancelCall := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() { result <- left.Call(callCtx, "blocked", nil, nil) }()
	<-started
	cancelCall()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v, want context cancellation", err)
	}

	left.mu.Lock()
	pending := len(left.pending)
	left.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending response state = %d, want 0 after cancellation", pending)
	}
	if err := left.deliver(message{ID: json.RawMessage(`"1"`), Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("late response for issued request: %v", err)
	}
	if err := left.deliver(message{ID: json.RawMessage(`"2"`), Result: json.RawMessage(`{}`)}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("response for unissued request error = %v, want protocol violation", err)
	}
	if err := left.deliver(message{ID: json.RawMessage(`"01"`), Result: json.RawMessage(`{}`)}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("noncanonical historic response error = %v, want protocol violation", err)
	}
}

func TestResponseForAllocatedButUnwrittenIDIsRejected(t *testing.T) {
	left, right := net.Pipe()
	peer := NewPeer(left)
	t.Cleanup(func() { _ = peer.Close(); _ = right.Close() })
	peer.nextID.Store(2)
	peer.highestWrittenID.Store(1)
	if err := peer.deliver(message{ID: json.RawMessage(`"1"`), Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("written historic response: %v", err)
	}
	if err := peer.deliver(message{ID: json.RawMessage(`"2"`), Result: json.RawMessage(`{}`)}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("allocated but unwritten response error = %v, want protocol violation", err)
	}
}

func TestQueuedInboundCancellationReleasesInflightState(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	peer := NewPeer(leftConn)
	t.Cleanup(func() {
		_ = peer.Close()
		_ = rightConn.Close()
	})

	for range maxHandlers {
		peer.handlerSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range maxHandlers {
			<-peer.handlerSlots
		}
	})

	requestCtx, cancelRequest := context.WithCancel(t.Context())
	requestID := json.RawMessage(`"queued"`)
	peer.mu.Lock()
	peer.inflight["queued"] = cancelRequest
	peer.mu.Unlock()
	peer.methods <- inboundMethod{
		ctx: requestCtx, writeCtx: t.Context(), cancel: cancelRequest,
		msg: message{JSONRPC: Version, ID: requestID, Method: "blocked", hasID: true},
	}

	queuedDeadline := time.Now().Add(time.Second)
	for len(peer.methods) != 0 {
		if time.Now().After(queuedDeadline) {
			t.Fatal("dispatcher did not accept queued request")
		}
		runtime.Gosched()
	}
	peer.mu.Lock()
	_, admitted := peer.inflight["queued"]
	peer.mu.Unlock()
	if !admitted {
		t.Fatal("queued request was not admitted before cancellation")
	}

	cancelRequest()
	deadline := time.Now().Add(time.Second)
	for {
		peer.mu.Lock()
		_, retained := peer.inflight["queued"]
		peer.mu.Unlock()
		if !retained {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued cancellation retained inbound request state")
		}
		runtime.Gosched()
	}
}

func TestPreCanceledCallDoesNotWriteOrClosePeer(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	left := NewPeer(leftConn)
	right := NewPeer(rightConn)
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := right.Handle("health", func(context.Context, json.RawMessage) (any, *Error) {
		return map[string]bool{"healthy": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = left.Serve(ctx) }()
	go func() { _ = right.Serve(ctx) }()

	canceled, stop := context.WithCancel(ctx)
	stop()
	if err := left.Call(canceled, "health", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled Call error = %v", err)
	}
	var result struct {
		Healthy bool `json:"healthy"`
	}
	if err := left.Call(ctx, "health", nil, &result); err != nil || !result.Healthy {
		t.Fatalf("subsequent Call = %#v, %v", result, err)
	}
}

func TestPreCanceledCallDoesNotCreateWrittenIDGap(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	peer := NewPeer(leftConn)
	t.Cleanup(func() { _ = peer.Close(); _ = rightConn.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = peer.Serve(ctx) }()

	preCanceled, stop := context.WithCancel(ctx)
	stop()
	if err := peer.Call(preCanceled, "health", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled Call error = %v", err)
	}

	requestID := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(rightConn)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			requestID <- "read-error"
			return
		}
		request, err := decodeMessage(bytes.TrimSpace(line))
		if err != nil {
			requestID <- "decode-error"
			return
		}
		var id string
		_ = json.Unmarshal(request.ID, &id)
		requestID <- id
		response, _ := marshalMessage(message{JSONRPC: Version, ID: request.ID, Result: json.RawMessage(`{}`)})
		_, _ = rightConn.Write(response)
	}()
	if err := peer.Call(ctx, "health", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-requestID; got != "1" {
		t.Fatalf("first written request id = %q, want 1", got)
	}
}

func TestPeerRejectsOversizedOutboundMessage(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	defer rightConn.Close()
	peer := NewPeer(leftConn)
	defer peer.Close()
	err := peer.Notify(context.Background(), "large", map[string]string{"value": strings.Repeat("x", MaxMessageBytes)})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Notify error = %v, want size error", err)
	}
}

func TestDecodeMessageRejectsNonCanonicalEnvelopes(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"duplicate key":        `{"jsonrpc":"2.0","jsonrpc":"2.0","method":"test"}`,
		"unknown key":          `{"jsonrpc":"2.0","method":"test","extra":true}`,
		"numeric id":           `{"jsonrpc":"2.0","id":1,"method":"test","params":{}}`,
		"null id":              `{"jsonrpc":"2.0","id":null,"method":"test","params":{}}`,
		"empty id":             `{"jsonrpc":"2.0","id":"","method":"test","params":{}}`,
		"zero id":              `{"jsonrpc":"2.0","id":"0","method":"test","params":{}}`,
		"negative id":          `{"jsonrpc":"2.0","id":"-1","method":"test","params":{}}`,
		"noncanonical id":      `{"jsonrpc":"2.0","id":"01","method":"test","params":{}}`,
		"overflow id":          `{"jsonrpc":"2.0","id":"18446744073709551616","method":"test","params":{}}`,
		"batch":                `[{"jsonrpc":"2.0","method":"test"}]`,
		"scalar params":        `{"jsonrpc":"2.0","method":"test","params":"bad"}`,
		"request with result":  `{"jsonrpc":"2.0","id":"1","method":"test","result":{}}`,
		"response with method": `{"jsonrpc":"2.0","id":"1","method":"test","result":{}}`,
		"result and error":     `{"jsonrpc":"2.0","id":"1","result":{},"error":{"code":-32603,"message":"internal error"}}`,
		"error case alias":     `{"jsonrpc":"2.0","id":"1","error":{"Code":-32603,"message":"internal error"}}`,
		"metadata case alias":  `{"jsonrpc":"2.0","id":"1","method":"test","bsbctl":{"Deadline_unix_milliseconds":1}}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeMessage([]byte(raw)); err == nil {
				t.Fatal("non-canonical envelope was accepted")
			}
		})
	}
}

func FuzzDecodeMessage(f *testing.F) {
	for _, seed := range []string{
		`{"jsonrpc":"2.0","id":"1","method":"test","params":{}}`,
		`{"jsonrpc":"2.0","method":"event","params":{}}`,
		`{"jsonrpc":"2.0","id":"1","result":{}}`,
		`{"jsonrpc":"2.0","jsonrpc":"2.0","method":"test"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > MaxMessageBytes {
			t.Skip()
		}
		message, err := decodeMessage(raw)
		if err != nil {
			return
		}
		if !json.Valid(raw) || message.JSONRPC != Version {
			t.Fatalf("decoder accepted invalid envelope: %q", raw)
		}
		if err := validateMessage(message); err != nil {
			t.Fatalf("decoder returned invalid message: %v", err)
		}
	})
}

func TestDuplicateConcurrentInboundRequestIDTerminatesPeer(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	peer := NewPeer(leftConn)
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		_ = peer.Close()
		_ = rightConn.Close()
	})
	started := make(chan struct{})
	if err := peer.Handle("blocked", func(context.Context, json.RawMessage) (any, *Error) {
		close(started)
		<-release
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = peer.Serve(t.Context()) }()

	request := []byte(`{"jsonrpc":"2.0","id":"7","method":"blocked","params":{}}` + "\n")
	if _, err := rightConn.Write(request); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := rightConn.Write(request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-peer.Done():
		if err := peer.terminalError(); !errors.Is(err, ErrProtocol) {
			t.Fatalf("terminal error = %v, want protocol violation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate in-flight request id did not terminate peer")
	}
}

func TestCallPropagatesDeadlineAndCancelsRemoteHandler(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	left := NewPeer(leftConn)
	right := NewPeer(rightConn)
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })

	started := make(chan time.Time, 1)
	cancelled := make(chan error, 1)
	if err := right.Handle("blocked", func(ctx context.Context, _ json.RawMessage) (any, *Error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			started <- time.Time{}
		} else {
			started <- deadline
		}
		<-ctx.Done()
		cancelled <- ctx.Err()
		return nil, &Error{Code: -32000, Message: "request cancelled"}
	}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = left.Serve(t.Context()) }()
	go func() { _ = right.Serve(t.Context()) }()

	callCtx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	result := make(chan error, 1)
	go func() { result <- left.Call(callCtx, "blocked", struct{}{}, nil) }()
	deadline := <-started
	if deadline.IsZero() || time.Until(deadline) > 250*time.Millisecond {
		t.Fatalf("remote deadline = %v, want propagated caller deadline", deadline)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v, want context cancellation", err)
	}
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("remote handler error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("rpc.cancel did not cancel the remote handler")
	}
}

func TestNonCanonicalCancelRequestIDTerminatesPeer(t *testing.T) {
	for _, id := range []string{"0", "-1", "01", "18446744073709551616"} {
		t.Run(id, func(t *testing.T) {
			leftConn, rightConn := net.Pipe()
			peer := NewPeer(leftConn)
			t.Cleanup(func() { _ = peer.Close(); _ = rightConn.Close() })
			go func() { _ = peer.Serve(t.Context()) }()
			if _, err := rightConn.Write([]byte(`{"jsonrpc":"2.0","method":"rpc.cancel","params":{"id":"` + id + `"}}` + "\n")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-peer.Done():
				if !errors.Is(peer.terminalError(), ErrProtocol) {
					t.Fatalf("terminal error = %v, want protocol error", peer.terminalError())
				}
			case <-time.After(time.Second):
				t.Fatal("invalid rpc.cancel did not terminate peer")
			}
		})
	}
}

func TestUnknownResponseTerminatesPeer(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	peer := NewPeer(leftConn)
	t.Cleanup(func() { _ = peer.Close(); _ = rightConn.Close() })
	go func() { _ = peer.Serve(t.Context()) }()
	if _, err := rightConn.Write([]byte(`{"jsonrpc":"2.0","id":"999","result":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-peer.Done():
		if err := peer.terminalError(); !errors.Is(err, ErrProtocol) {
			t.Fatalf("terminal error = %v, want protocol error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unsolicited response did not terminate peer")
	}
}
