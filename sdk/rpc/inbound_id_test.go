package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
)

type postWriteBarrierConn struct {
	net.Conn
	visible chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *postWriteBarrierConn) Write(data []byte) (int, error) {
	written, err := c.Conn.Write(data)
	if err == nil && written == len(data) {
		c.once.Do(func() {
			close(c.visible)
			<-c.release
		})
	}
	return written, err
}

func TestCompletedInboundRequestIDCanBeReused(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	barrier := &postWriteBarrierConn{Conn: leftConn, visible: make(chan struct{}), release: make(chan struct{})}
	peer := NewPeer(barrier)
	var releaseOnce sync.Once
	releaseWriter := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	t.Cleanup(func() {
		releaseWriter()
		_ = peer.Close()
		_ = rightConn.Close()
	})
	var callsMu sync.Mutex
	calls := 0
	secondStarted := make(chan struct{})
	if err := peer.Handle("reuse", func(context.Context, json.RawMessage) (any, *Error) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call == 2 {
			close(secondStarted)
		}
		return map[string]int{"call": call}, nil
	}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = peer.Serve(t.Context()) }()
	reader := bufio.NewReader(rightConn)
	readCall := func(want int) {
		t.Helper()
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read response %d: %v", want, err)
		}
		msg, err := decodeMessage(line)
		if err != nil || msg.Error != nil {
			t.Fatalf("decode response %d = %#v, %v", want, msg, err)
		}
		var result struct {
			Call int `json:"call"`
		}
		if err := decodeStrict(msg.Result, &result); err != nil || result.Call != want {
			t.Fatalf("response %d result = %#v, %v", want, result, err)
		}
	}
	request := []byte(`{"jsonrpc":"2.0","id":"7","method":"reuse","params":{}}` + "\n")
	if _, err := rightConn.Write(request); err != nil {
		t.Fatal(err)
	}
	readCall(1)
	select {
	case <-barrier.visible:
	case <-time.After(time.Second):
		t.Fatal("first response did not reach the peer")
	}
	if _, err := rightConn.Write(request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondStarted:
	case <-peer.Done():
		t.Fatalf("completed request ID reuse terminated peer: %v", peer.terminalError())
	case <-time.After(time.Second):
		t.Fatal("reused request ID did not reach its handler")
	}
	releaseWriter()
	readCall(2)
	select {
	case <-peer.Done():
		t.Fatalf("peer terminated after legal request ID reuse: %v", peer.terminalError())
	default:
	}
	third := []byte(`{"jsonrpc":"2.0","id":"8","method":"reuse","params":{}}` + "\n")
	if _, err := rightConn.Write(third); err != nil {
		t.Fatal(err)
	}
	readCall(3)
}
