package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestDuplicateErrorNamesTerminatePendingCall(t *testing.T) {
	for name, rpcError := range map[string]string{
		"code":          `{"code":-32603,"code":-32602,"message":"failed"}`,
		"message":       `{"code":-32603,"message":"first","message":"second"}`,
		"data":          `{"code":-32603,"message":"failed","data":{},"data":{}}`,
		"nested object": `{"code":-32603,"message":"failed","data":{"cause":{"kind":"first","kind":"second"}}}`,
		"nested array":  `{"code":-32603,"message":"failed","data":{"causes":[{"kind":"first","kind":"second"}]}}`,
		"escaped name":  `{"code":-32603,"message":"failed","data":{"kind":"first","\u006bind":"second"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			left, right := net.Pipe()
			peer := NewPeer(left)
			t.Cleanup(func() { _ = right.Close(); _ = peer.Close() })
			if err := right.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			served := make(chan error, 1)
			go func() { served <- peer.Serve(t.Context()) }()
			called := make(chan error, 1)
			go func() { called <- peer.Call(t.Context(), "test", nil, nil) }()
			request, err := bufio.NewReader(right).ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(request, &envelope); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(right, "{\"jsonrpc\":\"2.0\",\"id\":%q,\"error\":%s}\n", envelope.ID, rpcError); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-called:
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("Call error = %v, want protocol termination", err)
				}
			case <-time.After(time.Second):
				t.Fatal("malformed response did not unblock pending call")
			}
			select {
			case err := <-served:
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("Serve error = %v, want protocol violation", err)
				}
			case <-time.After(time.Second):
				t.Fatal("malformed response did not terminate Serve")
			}
		})
	}
}

func TestDuplicateDeadlineRejectedBeforeDispatch(t *testing.T) {
	for name, metadata := range map[string]string{
		"expired then future": `{"deadline_unix_milliseconds":1,"deadline_unix_milliseconds":4102444800000}`,
		"future then expired": `{"deadline_unix_milliseconds":4102444800000,"deadline_unix_milliseconds":1}`,
		"same deadline":       `{"deadline_unix_milliseconds":4102444800000,"deadline_unix_milliseconds":4102444800000}`,
	} {
		t.Run(name, func(t *testing.T) {
			left, right := net.Pipe()
			peer := NewPeer(left)
			t.Cleanup(func() { _ = right.Close(); _ = peer.Close() })
			if err := right.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			called := make(chan struct{}, 1)
			if err := peer.Handle("test", func(context.Context, json.RawMessage) (any, *Error) {
				called <- struct{}{}
				return struct{}{}, nil
			}); err != nil {
				t.Fatal(err)
			}
			go func() { _ = peer.Serve(t.Context()) }()
			if _, err := fmt.Fprintf(right, "{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"method\":\"test\",\"bsbctl\":%s}\n", metadata); err != nil {
				t.Fatal(err)
			}
			line, err := bufio.NewReader(right).ReadBytes('\n')
			if err != nil {
				t.Fatalf("read invalid-request response: %v", err)
			}
			var response struct {
				ID    json.RawMessage `json:"id"`
				Error *Error          `json:"error"`
			}
			if err := json.Unmarshal(line, &response); err != nil {
				t.Fatal(err)
			}
			if string(response.ID) != "null" || response.Error == nil || response.Error.Code != -32600 {
				t.Fatalf("response = %s, want invalid request with null id", line)
			}
			select {
			case <-called:
				t.Fatal("ambiguous deadline reached handler")
			default:
			}
			select {
			case <-peer.Done():
				t.Fatal("invalid request terminated peer")
			default:
			}
		})
	}
}

func TestDecodeErrorPreservesDistinctNestedObjects(t *testing.T) {
	const raw = `{"jsonrpc":"2.0","id":"1","error":{"code":-32603,"message":"failed","data":{"causes":[{"kind":"first"},{"kind":"second"}],"large":1e1000}}}`
	msg, err := decodeMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"causes":[{"kind":"first"},{"kind":"second"}],"large":1e1000}`
	if msg.Error == nil || string(msg.Error.Data) != want {
		t.Fatalf("error = %#v, want unmodified nested error data", msg.Error)
	}
}
