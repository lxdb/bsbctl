package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestProcessSendsEndSessionRequest(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{peer: rpc.NewPeer(leftConn), instances: instanceMap([]Instance{{ID: "app", Generation: 7}})}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	received := make(chan protocol.SessionEndRequest, 1)
	if err := remote.Handle("plugin.session.end", func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request protocol.SessionEndRequest
		if err := protocol.DecodeStrict(raw, &request); err != nil {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		received <- request
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()
	caller, ok := any(process).(interface {
		EndSession(context.Context, EndSessionRequest) error
	})
	if !ok {
		t.Fatal("Process does not implement EndSession")
	}
	want := EndSessionRequest{InstanceID: "app", Generation: 7, SessionToken: "session-1"}
	callCtx, callCancel := context.WithTimeout(ctx, time.Second)
	defer callCancel()
	if err := caller.EndSession(callCtx, want); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		wantWire := protocol.SessionEndRequest{Instance: protocol.InstanceRef{ID: "app", Generation: 7}, SessionToken: "session-1"}
		if got != wantWire {
			t.Fatalf("request = %#v, want %#v", got, wantWire)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for plugin.session.end")
	}
	for _, cleanup := range []EndSessionRequest{
		{InstanceID: "app", SessionToken: "session-without-generation"},
		{InstanceID: "app", Generation: 6},
	} {
		if err := caller.EndSession(callCtx, cleanup); err != nil {
			t.Fatalf("unowned cleanup should be an idempotent no-op: %v", err)
		}
	}
	select {
	case got := <-received:
		t.Fatalf("unowned cleanup reached plugin: %#v", got)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestProcessUsesExactV1SessionAndOperationMethods(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{peer: rpc.NewPeer(leftConn), instances: instanceMap([]Instance{{ID: "app", Generation: 7}})}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	methods := make(chan string, 2)
	if err := remote.Handle("plugin.session.start", func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request protocol.SessionStartRequest
		if protocol.DecodeStrict(raw, &request) != nil || request.Instance != (protocol.InstanceRef{ID: "app", Generation: 7}) || request.SessionToken != "session-3" {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		methods <- "plugin.session.start"
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := remote.Handle("plugin.operation.invoke", func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request protocol.OperationRequest
		if protocol.DecodeStrict(raw, &request) != nil || request.Operation != "sessions" || request.Instance != (protocol.InstanceRef{ID: "app", Generation: 7}) {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		methods <- "plugin.operation.invoke"
		return protocol.OperationResult{Payload: json.RawMessage(`{"sessions":[]}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()
	if err := process.Invoke(ctx, InvokeRequest{InstanceID: "app", Generation: 7, Action: "open", SessionToken: "session-3"}); err != nil {
		t.Fatal(err)
	}
	result, err := process.Operation(ctx, protocol.OperationRequest{Instance: protocol.InstanceRef{ID: "app", Generation: 7}, Operation: "sessions", Payload: json.RawMessage(`{}`)})
	if err != nil || string(result.Payload) != `{"sessions":[]}` {
		t.Fatalf("operation = %s / %v", result.Payload, err)
	}
	for _, want := range []string{"plugin.session.start", "plugin.operation.invoke"} {
		if got := <-methods; got != want {
			t.Fatalf("method = %q, want %q", got, want)
		}
	}
}

func TestProcessRejectsInvalidSessionEndAndOperationRequestsBeforeRPC(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{peer: rpc.NewPeer(leftConn), instances: instanceMap([]Instance{{ID: "app", Generation: 7}})}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	var sessionCalls atomic.Int32
	if err := remote.Handle("plugin.session.start", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		sessionCalls.Add(1)
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var endCalls atomic.Int32
	if err := remote.Handle("plugin.session.end", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		endCalls.Add(1)
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var operationCalls atomic.Int32
	if err := remote.Handle("plugin.operation.invoke", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		operationCalls.Add(1)
		return protocol.OperationResult{Payload: json.RawMessage(`{}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()

	for _, payload := range []json.RawMessage{
		json.RawMessage(`"scalar"`),
		json.RawMessage(`[]`),
		json.RawMessage(`null`),
		pluginObjectOfSize(t, protocol.MaxJSONObjectBytes+1),
	} {
		err := process.Invoke(ctx, InvokeRequest{
			InstanceID: "app", Generation: 7, Action: "open", Payload: payload, SessionToken: "session-3",
		})
		if err == nil {
			t.Fatalf("invalid session payload %s was sent", payload[:min(len(payload), 64)])
		}
		_, err = process.Operation(ctx, protocol.OperationRequest{
			Instance: protocol.InstanceRef{ID: "app", Generation: 7}, Operation: "sessions", Payload: payload,
		})
		if err == nil {
			t.Fatalf("invalid operation payload %s was sent", payload[:min(len(payload), 64)])
		}
	}
	for _, token := range []string{"", "session\ncontrol", strings.Repeat("s", 129)} {
		err := process.EndSession(ctx, EndSessionRequest{InstanceID: "app", Generation: 7, SessionToken: token})
		if err == nil || err.Error() != "plugin_end_session_failed: plugin end session failed" {
			t.Fatalf("invalid end-session token %q error = %v", token, err)
		}
	}
	if sessionCalls.Load() != 0 || endCalls.Load() != 0 || operationCalls.Load() != 0 {
		t.Fatalf("invalid request RPC calls = session:%d end:%d operation:%d, want 0", sessionCalls.Load(), endCalls.Load(), operationCalls.Load())
	}
}

func TestProcessRejectsUnknownOperationResultFields(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{peer: rpc.NewPeer(leftConn), instances: instanceMap([]Instance{{ID: "app", Generation: 7}})}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	if err := remote.Handle("plugin.operation.invoke", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		return map[string]any{"payload": map[string]any{}, "unknown": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()
	_, err := process.Operation(ctx, protocol.OperationRequest{
		Instance: protocol.InstanceRef{ID: "app", Generation: 7}, Operation: "sessions", Payload: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("operation result with unknown field was accepted")
	}
}

func TestProcessValidatesOperationResultPayloadObjectBoundary(t *testing.T) {
	for _, test := range []struct {
		name    string
		result  any
		wantErr bool
	}{
		{name: "object at limit", result: protocol.OperationResult{Payload: pluginObjectOfSize(t, protocol.MaxJSONObjectBytes)}},
		{name: "object over limit", result: protocol.OperationResult{Payload: pluginObjectOfSize(t, protocol.MaxJSONObjectBytes+1)}, wantErr: true},
		{name: "scalar", result: protocol.OperationResult{Payload: json.RawMessage(`"value"`)}, wantErr: true},
		{name: "array", result: protocol.OperationResult{Payload: json.RawMessage(`[]`)}, wantErr: true},
		{name: "null", result: protocol.OperationResult{Payload: json.RawMessage(`null`)}, wantErr: true},
		{name: "malformed", result: json.RawMessage(`{"payload":{"broken"}`), wantErr: true},
		{name: "unknown outer field", result: map[string]any{"payload": map[string]any{}, "unknown": true}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			leftConn, rightConn := net.Pipe()
			process := &Process{peer: rpc.NewPeer(leftConn), instances: instanceMap([]Instance{{ID: "app", Generation: 7}})}
			remote := rpc.NewPeer(rightConn)
			t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
			if err := remote.Handle("plugin.operation.invoke", func(context.Context, json.RawMessage) (any, *rpc.Error) {
				return test.result, nil
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			go func() { _ = process.peer.Serve(ctx) }()
			go func() { _ = remote.Serve(ctx) }()
			_, err := process.Operation(ctx, protocol.OperationRequest{
				Instance: protocol.InstanceRef{ID: "app", Generation: 7}, Operation: "sessions", Payload: json.RawMessage(`{}`),
			})
			if (err != nil) != test.wantErr {
				t.Fatalf("Operation error = %v, wantErr=%t", err, test.wantErr)
			}
		})
	}

}

func TestProcessAuthenticatesCompleteSessionRequests(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{
		spec: Spec{ID: "dev.bsbctl.test"}, peer: rpc.NewPeer(leftConn),
		instances: map[string]Instance{"configured": {ID: "configured", Generation: 7}},
	}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	delivered := make(chan protocol.CompleteSessionRequest, 1)
	if err := process.register(Callbacks{CompleteSession: func(_ context.Context, pluginID string, request protocol.CompleteSessionRequest) error {
		if pluginID != "dev.bsbctl.test" {
			t.Errorf("plugin ID = %q", pluginID)
		}
		delivered <- request
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()
	want := protocol.CompleteSessionRequest{Instance: protocol.InstanceRef{ID: "configured", Generation: 7}, SessionToken: "interactive-7"}
	if err := remote.Call(ctx, "host.session.complete", want, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-delivered:
		if got != want {
			t.Fatalf("request = %#v, want %#v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for complete-session callback")
	}
	for _, invalid := range []protocol.CompleteSessionRequest{
		{Instance: protocol.InstanceRef{ID: "configured", Generation: 7}},
		{Instance: protocol.InstanceRef{ID: "other", Generation: 7}, SessionToken: "interactive-7"},
	} {
		if err := remote.Call(ctx, "host.session.complete", invalid, nil); err == nil {
			t.Fatalf("invalid complete-session request succeeded: %#v", invalid)
		}
	}
}

func TestProcessAuthenticatesExecutionGrantAndPreservesStableDenials(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{
		spec: Spec{ID: "dev.bsbctl.test"}, peer: rpc.NewPeer(leftConn),
		instances: map[string]Instance{"configured": {ID: "configured", Generation: 7}},
	}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	delivered := make(chan protocol.SessionExecutionRequest, 3)
	if err := process.register(Callbacks{BeginExecution: func(_ context.Context, pluginID string, request protocol.SessionExecutionRequest) error {
		if pluginID != "dev.bsbctl.test" {
			t.Errorf("plugin ID = %q", pluginID)
		}
		delivered <- request
		switch request.SessionToken {
		case "canceled":
			return protocol.NewDomainError(protocol.ErrorSessionCanceled, errors.New("private cancellation detail"))
		case "inactive":
			return protocol.NewDomainError(protocol.ErrorSessionNotActive, errors.New("private inactive detail"))
		default:
			return nil
		}
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()

	want := protocol.SessionExecutionRequest{Instance: protocol.InstanceRef{ID: "configured", Generation: 7}, SessionToken: "interactive-7"}
	if err := remote.Call(ctx, "host.session.execution.begin", want, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-delivered; got != want {
		t.Fatalf("request = %#v, want %#v", got, want)
	}

	for _, test := range []struct {
		name     string
		request  protocol.SessionExecutionRequest
		wantCode int
		wantKind protocol.ErrorKind
	}{
		{name: "canceled", request: protocol.SessionExecutionRequest{Instance: want.Instance, SessionToken: "canceled"}, wantCode: protocol.DomainErrorCode, wantKind: protocol.ErrorSessionCanceled},
		{name: "inactive", request: protocol.SessionExecutionRequest{Instance: want.Instance, SessionToken: "inactive"}, wantCode: protocol.DomainErrorCode, wantKind: protocol.ErrorSessionNotActive},
		{name: "retired generation", request: protocol.SessionExecutionRequest{Instance: protocol.InstanceRef{ID: "configured", Generation: 8}, SessionToken: "interactive-8"}, wantCode: protocol.DomainErrorCode, wantKind: protocol.ErrorSessionGenerationMismatch},
		{name: "missing token", request: protocol.SessionExecutionRequest{Instance: want.Instance}, wantCode: -32602},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := remote.Call(ctx, "host.session.execution.begin", test.request, nil)
			rpcErr, ok := errors.AsType[*rpc.Error](err)
			if !ok || rpcErr.Code != test.wantCode {
				t.Fatalf("error = %#v, want code %d", err, test.wantCode)
			}
			if test.wantKind == "" {
				return
			}
			kind, domain, decodeErr := protocol.DecodeRemoteError(rpcErr.Code, rpcErr.Data)
			if decodeErr != nil || !domain || kind != test.wantKind {
				t.Fatalf("domain error = %q/%t/%v, want %q", kind, domain, decodeErr, test.wantKind)
			}
		})
	}

	process.policyMu.Lock()
	process.pending = map[string]Instance{"configured": {ID: "configured", Generation: 8}}
	process.policyMu.Unlock()
	err := remote.Call(ctx, "host.session.execution.begin", want, nil)
	rpcErr, ok := errors.AsType[*rpc.Error](err)
	if !ok {
		t.Fatalf("replacement-pending error = %#v", err)
	}
	kind, domain, decodeErr := protocol.DecodeRemoteError(rpcErr.Code, rpcErr.Data)
	if decodeErr != nil || !domain || kind != protocol.ErrorSessionGenerationMismatch {
		t.Fatalf("replacement-pending error = %q/%t/%v, want %q", kind, domain, decodeErr, protocol.ErrorSessionGenerationMismatch)
	}
}

func TestProcessMapsHostileChildRPCErrorsToStableCoreErrors(t *testing.T) {
	tests := []struct {
		name   string
		method string
		want   string
		call   func(context.Context, *Process) error
	}{
		{name: "invoke", method: "plugin.session.start", want: "plugin_invoke_failed: plugin invocation failed", call: func(ctx context.Context, process *Process) error {
			return process.Invoke(ctx, InvokeRequest{InstanceID: "app", Generation: 1, Action: "start", SessionToken: "session"})
		}},
		{name: "end session", method: "plugin.session.end", want: "plugin_end_session_failed: plugin end session failed", call: func(ctx context.Context, process *Process) error {
			return process.EndSession(ctx, EndSessionRequest{InstanceID: "app", Generation: 1, SessionToken: "session"})
		}},
		{name: "replace instances", method: "plugin.instances.replace", want: "plugin_reconcile_failed: plugin reconciliation failed", call: func(ctx context.Context, process *Process) error {
			return process.ReplaceInstances(ctx, nil)
		}},
		{name: "ping", method: "plugin.health", want: "plugin_ping_failed: plugin health check failed", call: func(ctx context.Context, process *Process) error {
			_, err := process.Ping(ctx)
			return err
		}},
		{name: "shutdown", method: "plugin.shutdown", want: "plugin_shutdown_failed: plugin shutdown failed", call: func(ctx context.Context, process *Process) error {
			return process.Stop(ctx)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			leftConn, rightConn := net.Pipe()
			process := &Process{
				cmd: &exec.Cmd{Process: &os.Process{Pid: 4242}}, peer: rpc.NewPeer(leftConn),
				done: make(chan error, 1), reaped: make(chan struct{}), stopDone: make(chan struct{}),
				shutdownGrace: time.Second, termGrace: time.Second,
				signalGroup: func(int, syscall.Signal) error {
					t.Fatal("hostile RPC response caused a process signal before natural exit")
					return nil
				},
				instances: instanceMap([]Instance{{ID: "app", Generation: 1}}),
			}
			remote := rpc.NewPeer(rightConn)
			if err := remote.Handle(test.method, func(context.Context, json.RawMessage) (any, *rpc.Error) {
				if test.method == "plugin.shutdown" {
					close(process.reaped)
				}
				return nil, &rpc.Error{Code: -32099, Message: hostileRPCSecret, Data: json.RawMessage(`{"secret":"hostile-data-canary"}`)}
			}); err != nil {
				t.Fatal(err)
			}
			serveCtx, cancelServe := context.WithCancel(context.Background())
			defer cancelServe()
			go func() { _ = process.peer.Serve(serveCtx) }()
			go func() { _ = remote.Serve(serveCtx) }()
			t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })

			callCtx, cancelCall := context.WithTimeout(context.Background(), time.Second)
			defer cancelCall()
			err := test.call(callCtx, process)
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), hostileRPCSecret) || strings.Contains(err.Error(), "hostile-data-canary") {
				t.Fatalf("returned error retained hostile child data: %v", err)
			}
			if !errors.Is(err, rpc.ErrProtocol) {
				t.Fatalf("hostile remote error = %v, want protocol violation", err)
			}
			select {
			case <-process.peer.Done():
			default:
				t.Fatal("protocol-invalid remote error left peer usable")
			}
		})
	}
}

func TestProcessRedactsUnknownRemoteDomainKindFromImmediateAndTerminalErrors(t *testing.T) {
	const hostileKind = "authorization=Bearer hostile-core-domain-kind-canary"
	leftConn, rightConn := net.Pipe()
	process := &Process{
		peer:      rpc.NewPeer(leftConn),
		instances: instanceMap([]Instance{{ID: "app", Generation: 1}}),
	}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	if err := remote.Handle("plugin.session.start", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		data, marshalErr := json.Marshal(map[string]string{"kind": hostileKind})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return nil, &rpc.Error{Code: protocol.DomainErrorCode, Message: hostileKind, Data: data}
	}); err != nil {
		t.Fatal(err)
	}
	hostServeResult := make(chan error, 1)
	go func() { hostServeResult <- process.peer.Serve(t.Context()) }()
	go func() { _ = remote.Serve(t.Context()) }()

	err := process.Invoke(t.Context(), InvokeRequest{
		InstanceID: "app", Generation: 1, Action: "start", SessionToken: "session",
	})
	if !errors.Is(err, rpc.ErrProtocol) {
		t.Fatalf("Invoke error = %v, want protocol violation", err)
	}
	if errorGraphContains(err, hostileKind) {
		t.Fatalf("immediate error graph retained hostile domain kind: %v", err)
	}
	select {
	case <-process.peer.Done():
	case <-time.After(time.Second):
		t.Fatal("unknown domain kind left core peer usable")
	}
	select {
	case serveErr := <-hostServeResult:
		if !errors.Is(serveErr, rpc.ErrProtocol) || errorGraphContains(serveErr, hostileKind) {
			t.Fatalf("Serve error = %v, want redacted protocol violation", serveErr)
		}
	case <-time.After(time.Second):
		t.Fatal("terminated core peer did not return from Serve")
	}
}

func errorGraphContains(err error, value string) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), value) {
		return true
	}
	switch current := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range current.Unwrap() {
			if errorGraphContains(cause, value) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return errorGraphContains(current.Unwrap(), value)
	}
	return false
}

func TestProcessRPCErrorMappingPreservesLocalContextIdentity(t *testing.T) {
	for name, newContext := range map[string]func() (context.Context, context.CancelFunc){
		"canceled": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, func() {}
		},
		"deadline": func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		},
	} {
		t.Run(name, func(t *testing.T) {
			leftConn, rightConn := net.Pipe()
			process := &Process{peer: rpc.NewPeer(leftConn), instances: instanceMap([]Instance{{ID: "app", Generation: 1}})}
			t.Cleanup(func() { _ = process.peer.Close(); _ = rightConn.Close() })
			ctx, cancel := newContext()
			defer cancel()
			err := process.Invoke(ctx, InvokeRequest{InstanceID: "app", Generation: 1, Action: "start", SessionToken: "session"})
			if !errors.Is(err, ctx.Err()) {
				t.Fatalf("error = %v, want local context identity %v", err, ctx.Err())
			}
			if err == nil || err.Error() != "plugin_invoke_failed: plugin invocation failed" {
				t.Fatalf("error text = %v", err)
			}
		})
	}
}

func TestProcessRejectsInvalidHealthResult(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{peer: rpc.NewPeer(leftConn)}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	if err := remote.Handle("plugin.health", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		return json.RawMessage(`{"healthy":true,"unexpected":true}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = process.peer.Serve(t.Context()) }()
	go func() { _ = remote.Serve(t.Context()) }()
	if _, err := process.Ping(t.Context()); err == nil {
		t.Fatal("invalid health result accepted")
	}
}
