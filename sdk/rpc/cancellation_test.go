package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestCallEmptyAcceptsWhitespaceInsideEmptyObject(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	peer := NewPeer(left)
	defer peer.Close()
	go func() { _ = peer.Serve(t.Context()) }()
	done := make(chan error, 1)
	go func() {
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(right).Decode(&request); err != nil {
			done <- err
			return
		}
		_, err := fmt.Fprintf(right, "{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{ \t }}\n", request.ID)
		done <- err
	}()
	if err := peer.CallEmpty(t.Context(), "empty", nil); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCanceledCallDoesNotWaitForAnotherCallsWrite(t *testing.T) {
	left, right := net.Pipe()
	started := make(chan struct{})
	peer := NewPeer(&writeBarrierConn{Conn: left, started: started})
	defer right.Close()
	defer peer.Close()
	first := make(chan error, 1)
	go func() { first <- peer.Call(t.Context(), "blocked", nil, nil) }()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	second := make(chan error, 1)
	go func() { second <- peer.Call(ctx, "canceled", nil, nil) }()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrOutcomeUnknown) {
			t.Errorf("unwritten canceled call outcome = %v", err)
		}
	case <-time.After(time.Second):
		t.Error("canceled call waited behind an unrelated blocked write")
		peer.Close()
		<-second
	}
	peer.Close()
	<-first
}

type resetFailureConn struct {
	net.Conn
	deadlines int
	err       error
}

func (c *resetFailureConn) Write(data []byte) (int, error) { return len(data), nil }
func (c *resetFailureConn) SetWriteDeadline(time.Time) error {
	c.deadlines++
	if c.deadlines == 2 {
		return c.err
	}
	return nil
}

func TestWrittenRequestWithDeadlineResetFailureHasUnknownOutcome(t *testing.T) {
	want := errors.New("deadline reset failed")
	peer := &Peer{conn: &resetFailureConn{err: want}}
	err := peer.writeData(t.Context(), []byte("{}\n"))
	if !errors.Is(err, want) || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("fully written request outcome = %v, want unknown outcome and reset failure", err)
	}
}

func TestOversizedHandlerResultReturnsBoundedErrorAndKeepsConnection(t *testing.T) {
	left, right := net.Pipe()
	client, server := NewPeer(left), NewPeer(right)
	defer client.Close()
	defer server.Close()
	if err := server.Handle("large", func(context.Context, json.RawMessage) (any, *Error) {
		return map[string]string{"value": strings.Repeat("x", MaxMessageBytes)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.Handle("health", func(context.Context, json.RawMessage) (any, *Error) { return struct{}{}, nil }); err != nil {
		t.Fatal(err)
	}
	go func() { _ = client.Serve(t.Context()) }()
	go func() { _ = server.Serve(t.Context()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := client.Call(ctx, "large", nil, nil)
	rpcErr, ok := errors.AsType[*Error](err)
	if !ok || rpcErr.Code != -32603 || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("oversized handler result error = %v, want bounded internal error", err)
	}
	if err := client.CallEmpty(ctx, "health", nil); err != nil {
		t.Fatalf("connection unusable after oversized result: %v", err)
	}
}
