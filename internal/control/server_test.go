package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/checkpoint"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/installer"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/pluginlog"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

func (s *Server) Path() string { return s.path }

func TestUnixJSONRPCControlsAppsAndReportsStatus(t *testing.T) {
	t.Parallel()
	backend := &fakeBackend{document: config.Document{
		Version: config.CurrentVersion, Generation: 4,
		Apps:    map[string]config.App{"ball8": {ID: "ball8", PluginID: "plugin", Generation: 2, Enabled: false, Config: json.RawMessage(`{}`)}},
		Plugins: map[string]config.Plugin{},
	}, deviceStatus: device.RuntimeStatus{Phase: device.PhaseBackoff, Attempt: 3, LastErrorCode: "access_token_unavailable"}, recorderStatus: attention.RecorderStatus{
		Phase: attention.RecorderDegraded, LastErrorCode: "rotation_failed", LastErrorAt: time.Now().UTC(), LastSequence: 7,
	}, logStatus: pluginlog.Status{Dropped: 4, LastErrorCode: "plugin_log_write_failed", LastErrorAt: time.Now().UTC()}}
	backend.observationStatus = observation.StoreDiagnostics{LiveCount: 128, CapacityRejections: 2, LastRejectionAt: time.Now().UTC(), LastRejectionCode: observation.CapacityRejectionCode}
	backend.attentionState = daemon.AttentionStateDiagnostics{LastShownEntries: 17, AcknowledgementEntries: 4, LastShownCapacityEvictions: 9}
	backend.configStatus = daemon.ConfigPersistenceStatus{LastErrorCode: daemon.ConfigDurabilityUncertainCode}
	backend.checkpointStatus = checkpoint.Status{Files: 3, DataBytes: 4096, Failures: 1, LastErrorCode: checkpoint.DurabilityUncertainCode}
	backend.sessionStatus = daemon.SessionDiagnostics{ActiveInstanceID: "ball8", State: "interactive", LastLifecycleErrorCode: daemon.SessionEndFailedCode, LastLifecycleErrorAt: time.Now().UTC()}
	backend.audioStatus = device.AudioStatus{Attempts: 2, LastErrorCode: "audio_play_failed"}
	socketDir, err := os.MkdirTemp("/tmp", "bctl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	server, err := Listen(filepath.Join(socketDir, "ctl.sock"), "test-version", testBackends(backend))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()

	client, err := Dial(ctx, server.Path())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	var status Status
	if err := client.Call(ctx, "daemon.status", nil, &status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Version != "test-version" || status.Generation != 4 || len(status.Apps) != 1 || status.Apps[0].Enabled || status.Apps[0].RuntimeGeneration != 2 {
		t.Fatalf("status = %#v", status)
	}
	if status.Device != backend.deviceStatus {
		t.Fatalf("device status = %#v", status.Device)
	}
	if status.Audio != backend.audioStatus {
		t.Fatalf("audio status = %#v", status.Audio)
	}
	if status.AttentionRecorder != backend.recorderStatus {
		t.Fatalf("attention recorder status = %#v", status.AttentionRecorder)
	}
	if status.PluginLogs != backend.logStatus {
		t.Fatalf("plugin log status = %#v", status.PluginLogs)
	}
	if status.Observations != backend.observationStatus {
		t.Fatalf("observation status = %#v", status.Observations)
	}
	if status.AttentionState != backend.attentionState {
		t.Fatalf("attention state status = %#v", status.AttentionState)
	}
	if status.Configuration != backend.configStatus || status.Checkpoints != backend.checkpointStatus {
		t.Fatalf("persistence status = %#v, %#v", status.Configuration, status.Checkpoints)
	}
	if status.Session != backend.sessionStatus {
		t.Fatalf("session status = %#v, want %#v", status.Session, backend.sessionStatus)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || containsAny(string(encoded), "/Users/", "disk full", "attention.jsonl") {
		t.Fatalf("status leaks recorder internals: %s", encoded)
	}
	if err := client.Call(ctx, "app.set_enabled", SetEnabledRequest{AppID: "ball8", Enabled: true}, nil); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	if !backend.document.Apps["ball8"].Enabled {
		t.Fatal("backend app was not enabled")
	}
	if err := client.Call(ctx, "app.launch", LaunchRequest{AppID: "ball8", Action: "ask"}, nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if backend.launched != "ball8/ask" {
		t.Fatalf("launched = %q", backend.launched)
	}
	var operation protocol.OperationResult
	if err := client.Call(ctx, "app.operation", PluginOperationRequest{AppID: "ball8", Operation: "sessions", Kind: protocol.OperationQuery, Payload: json.RawMessage(`{}`)}, &operation); err != nil {
		t.Fatalf("plugin operation: %v", err)
	}
	if string(operation.Payload) != `{"sessions":[]}` {
		t.Fatalf("plugin operation result = %s", operation.Payload)
	}
	var trace attention.Trace
	if err := client.Call(ctx, "attention.snapshot", nil, &trace); err != nil {
		t.Fatalf("attention snapshot: %v", err)
	}
	if trace.SelectedID != "plugin/ball8/main/state" {
		t.Fatalf("attention snapshot = %#v", trace)
	}
	var evaluation attention.Evaluation
	if err := client.Call(ctx, "attention.explain", AttentionExplainRequest{ObservationID: trace.SelectedID}, &evaluation); err != nil {
		t.Fatalf("attention explain: %v", err)
	}
	if evaluation.Reason != attention.ReasonSelected {
		t.Fatalf("attention explain = %#v", evaluation)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}

func TestControlBackendErrorsAreRedacted(t *testing.T) {
	backend := &fakeBackend{document: config.Document{Version: config.CurrentVersion, Generation: 1, Apps: map[string]config.App{}, Plugins: map[string]config.Plugin{}}, operationErr: errors.New("token=secret at /Users/private provider.invalid")}
	server, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()
	for name, call := range map[string]func() error{
		"enable": func() error {
			return client.Call(context.Background(), "app.set_enabled", SetEnabledRequest{AppID: "app", Enabled: true}, nil)
		},
		"launch": func() error { return client.Call(context.Background(), "app.launch", LaunchRequest{AppID: "app"}, nil) },
		"acknowledge": func() error {
			return client.Call(context.Background(), "attention.acknowledge", AttentionAcknowledgeRequest{ObservationID: "id"}, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil || containsAny(err.Error(), "secret", "/Users/", "provider.invalid") {
				t.Fatalf("control error was not safely translated: %v", err)
			}
		})
	}
	_ = server
}

func TestControlValidatesPluginOperationPayloadAndResult(t *testing.T) {
	backend := &fakeBackend{document: emptyControlDocument()}
	_, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()

	objectOfSize := func(size int) json.RawMessage {
		const shell = `{"x":""}`
		return json.RawMessage(`{"x":"` + strings.Repeat("x", size-len(shell)) + `"}`)
	}
	validRequest := PluginOperationRequest{
		AppID: "ball8", Operation: "sessions", Kind: protocol.OperationQuery,
	}
	for _, test := range []struct {
		name   string
		params any
	}{
		{name: "scalar payload", params: withOperationPayload(validRequest, json.RawMessage(`"value"`))},
		{name: "array payload", params: withOperationPayload(validRequest, json.RawMessage(`[]`))},
		{name: "null payload", params: withOperationPayload(validRequest, json.RawMessage(`null`))},
		{name: "oversized payload", params: withOperationPayload(validRequest, objectOfSize(protocol.MaxJSONObjectBytes+1))},
		{name: "unknown field", params: map[string]any{
			"app_id": "ball8", "operation": "sessions", "kind": protocol.OperationQuery, "unknown": true,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := client.Call(t.Context(), "app.operation", test.params, nil)
			rpcErr, ok := errors.AsType[*rpc.Error](err)
			if !ok || rpcErr.Code != -32602 {
				t.Fatalf("error = %#v, want invalid params", err)
			}
		})
	}
	if backend.operationCalls != 0 {
		t.Fatalf("backend calls after invalid requests = %d, want 0", backend.operationCalls)
	}

	atLimit := withOperationPayload(validRequest, objectOfSize(protocol.MaxJSONObjectBytes))
	if err := client.Call(t.Context(), "app.operation", atLimit, nil); err != nil {
		t.Fatalf("payload at exact limit: %v", err)
	}
	if backend.operationCalls != 1 {
		t.Fatalf("backend calls after exact-limit request = %d, want 1", backend.operationCalls)
	}

	backend.operationResult = json.RawMessage(`null`)
	var result protocol.OperationResult
	err := client.Call(t.Context(), "app.operation", validRequest, &result)
	rpcErr, ok := errors.AsType[*rpc.Error](err)
	if !ok || rpcErr.Code != -32047 || result.Payload != nil {
		t.Fatalf("invalid backend result = %s, %#v; want redacted operation failure", result.Payload, err)
	}
}

func TestControlValidatesLaunchPayloadBeforeBackend(t *testing.T) {
	backend := &fakeBackend{document: emptyControlDocument()}
	_, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()
	objectOfSize := func(size int) json.RawMessage {
		const shell = `{"x":""}`
		return json.RawMessage(`{"x":"` + strings.Repeat("x", size-len(shell)) + `"}`)
	}
	for _, payload := range []json.RawMessage{
		json.RawMessage(`"scalar"`),
		json.RawMessage(`[]`),
		json.RawMessage(`null`),
		objectOfSize(protocol.MaxJSONObjectBytes + 1),
	} {
		err := client.Call(t.Context(), "app.launch", LaunchRequest{AppID: "ball8", Action: "ask", Payload: payload}, nil)
		rpcErr, ok := errors.AsType[*rpc.Error](err)
		if !ok || rpcErr.Code != -32602 {
			t.Fatalf("payload %s error = %#v, want invalid params", payload[:min(len(payload), 64)], err)
		}
	}
	if backend.launchCalls != 0 {
		t.Fatalf("backend launch calls after invalid requests = %d, want 0", backend.launchCalls)
	}
	if err := client.Call(t.Context(), "app.launch", LaunchRequest{
		AppID: "ball8", Action: "ask", Payload: objectOfSize(protocol.MaxJSONObjectBytes),
	}, nil); err != nil {
		t.Fatalf("launch payload at exact limit: %v", err)
	}
	if backend.launchCalls != 1 {
		t.Fatalf("backend launch calls after exact-limit request = %d, want 1", backend.launchCalls)
	}
}

func TestControlValidatesCreateAndReplaceConfigObjectBoundary(t *testing.T) {
	objectOfSize := func(size int) json.RawMessage {
		const shell = `{"x":""}`
		return json.RawMessage(`{"x":"` + strings.Repeat("x", size-len(shell)) + `"}`)
	}
	for _, test := range []struct {
		name   string
		method string
		params func(json.RawMessage) any
		calls  func(*configValidationBackend) int
	}{
		{name: "create", method: "app.create", params: func(raw json.RawMessage) any {
			return CreateAppRequest{AppID: "ball8", PluginID: "plugin", Config: raw}
		}, calls: func(backend *configValidationBackend) int { return backend.createCalls }},
		{name: "replace", method: "app.replace_config", params: func(raw json.RawMessage) any {
			return ReplaceConfigRequest{AppID: "ball8", Config: raw}
		}, calls: func(backend *configValidationBackend) int { return backend.replaceCalls }},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &configValidationBackend{fakeBackend: &fakeBackend{document: emptyControlDocument()}}
			_, client, cancel := startControlTestServer(t, backend)
			defer cancel()
			defer client.Close()
			for _, invalid := range []json.RawMessage{
				json.RawMessage(`"value"`),
				json.RawMessage(`[]`),
				json.RawMessage(`null`),
				objectOfSize(protocol.MaxJSONObjectBytes + 1),
			} {
				err := client.Call(t.Context(), test.method, test.params(invalid), nil)
				rpcErr, ok := errors.AsType[*rpc.Error](err)
				if !ok || rpcErr.Code != -32602 {
					t.Fatalf("config %s error = %#v, want invalid params", invalid[:min(len(invalid), 64)], err)
				}
			}
			if got := test.calls(backend); got != 0 {
				t.Fatalf("backend calls after invalid configs = %d, want 0", got)
			}
			if err := client.Call(t.Context(), test.method, test.params(objectOfSize(protocol.MaxJSONObjectBytes)), nil); err != nil {
				t.Fatalf("config at exact limit: %v", err)
			}
			if got := test.calls(backend); got != 1 {
				t.Fatalf("backend calls after exact-limit config = %d, want 1", got)
			}
		})
	}

	malformed := json.RawMessage(`{"broken"`)
	if validReplaceConfigRequest(ReplaceConfigRequest{AppID: "ball8", Config: malformed}) ||
		validCreateAppRequest(CreateAppRequest{AppID: "ball8", PluginID: "plugin", Config: malformed}) {
		t.Fatal("config validators accepted malformed object-looking JSON")
	}
}

func TestControlNoArgumentMethodsAcceptOnlyOmittedOrEmptyObjectParams(t *testing.T) {
	backend := &fakeBackend{document: emptyControlDocument()}
	_, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()
	for _, method := range []string{"daemon.status", "attention.snapshot"} {
		t.Run(method, func(t *testing.T) {
			err := client.Call(t.Context(), method, json.RawMessage(`{"unknown":true}`), nil)
			rpcErr, ok := errors.AsType[*rpc.Error](err)
			if !ok || rpcErr.Code != -32602 {
				t.Fatalf("unknown params error = %#v, want invalid params", err)
			}
			for _, params := range []json.RawMessage{
				json.RawMessage(`[]`),
				json.RawMessage(`null`),
				json.RawMessage(`"scalar"`),
			} {
				if err := client.Call(t.Context(), method, params, nil); err == nil {
					t.Fatalf("non-object params %s were accepted", params)
				}
			}
			if err := client.Call(t.Context(), method, struct{}{}, nil); err != nil {
				t.Fatalf("empty object params: %v", err)
			}
		})
	}
}

func withOperationPayload(request PluginOperationRequest, payload json.RawMessage) PluginOperationRequest {
	request.Payload = payload
	return request
}

func TestControlAttentionAcknowledgeDistinguishesRejectionFromFailure(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		err  error
		code int
	}{
		{name: "missing", err: daemon.ErrObservationNotFound, code: -32054},
		{name: "not acknowledgeable", err: daemon.ErrObservationNotAcknowledgeable, code: -32054},
		{name: "backend failure", err: errors.New("backend unavailable"), code: -32053},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeBackend{document: emptyControlDocument(), operationErr: test.err}
			_, client, cancel := startControlTestServer(t, backend)
			defer cancel()
			defer client.Close()
			err := client.Call(context.Background(), "attention.acknowledge", AttentionAcknowledgeRequest{ObservationID: "id"}, nil)
			rpcErr, ok := errors.AsType[*rpc.Error](err)
			if !ok || rpcErr.Code != test.code {
				t.Fatalf("error = %v, want RPC code %d", err, test.code)
			}
		})
	}
}

func TestListenRejectsEveryMissingBackendBeforeSocketCreation(t *testing.T) {
	backend := &fakeBackend{document: emptyControlDocument()}
	complete := testBackends(backend)
	tests := []struct {
		name string
		want string
		drop func(*Backends)
	}{
		{name: "apps", want: "apps control backend is required", drop: func(backends *Backends) { backends.Apps = nil }},
		{name: "catalog", want: "catalog control backend is required", drop: func(backends *Backends) { backends.Catalog = nil }},
		{name: "operations", want: "operations control backend is required", drop: func(backends *Backends) { backends.Operations = nil }},
		{name: "attention", want: "attention control backend is required", drop: func(backends *Backends) { backends.Attention = nil }},
		{name: "status", want: "status control backend is required", drop: func(backends *Backends) { backends.Status = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backends := complete
			test.drop(&backends)
			path := filepath.Join(t.TempDir(), "control.sock")
			server, err := Listen(path, "test", backends)
			if server != nil || err == nil || err.Error() != test.want {
				t.Fatalf("Listen = %#v, %v", server, err)
			}
			if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("control socket was created before backend validation: %v", statErr)
			}
		})
	}
}

func TestControlServerRejectsPeerSeventeenAndJoinsActivePeers(t *testing.T) {
	listener := newQueuedListener()
	options := defaultControlOptions()
	createdPeers := make(chan struct{}, 17)
	defaultFactory := options.newPeer
	options.newPeer = func(conn net.Conn) controlPeer {
		createdPeers <- struct{}{}
		return defaultFactory(conn)
	}
	server := newServer("", "test", testBackends(&fakeBackend{document: emptyControlDocument()}), listener, nil, options)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	clients := make([]net.Conn, 0, 17)
	for range 17 {
		serverConn, clientConn := net.Pipe()
		clients = append(clients, clientConn)
		listener.enqueue(serverConn)
	}
	listener.awaitAccepted(t, 17)
	for index := 0; index < 16; index++ {
		select {
		case <-createdPeers:
		case <-time.After(time.Second):
			t.Fatalf("admitted peer %d never created a handler", index+1)
		}
	}
	select {
	case <-createdPeers:
		t.Fatal("peer 17 created a handler")
	default:
	}
	if err := requireClosedControlConn(clients[16]); err != nil {
		t.Fatalf("peer 17 remained open: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not close and join active peer goroutines")
	}
	for index, client := range clients[:16] {
		if err := requireClosedControlConn(client); err != nil {
			t.Fatalf("active peer %d remained open after shutdown: %v", index+1, err)
		}
	}
	for _, client := range clients {
		_ = client.Close()
	}
}

func requireClosedControlConn(conn net.Conn) error {
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		return errors.New("read succeeded")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		return err
	}
	return nil
}

func TestActivityConnResetsReadIdleDeadline(t *testing.T) {
	base := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	clock := &sequenceClock{values: []time.Time{base, base.Add(10 * time.Second)}}
	conn := &deadlineRecordingConn{}
	active := newActivityConn(conn, defaultControlOptions().idleTimeout, clock.now)
	_, _ = active.Read(make([]byte, 1))
	_, _ = active.Read(make([]byte, 1))
	want := []time.Time{base.Add(30 * time.Second), base.Add(40 * time.Second)}
	if len(conn.deadlines) != len(want) || !conn.deadlines[0].Equal(want[0]) || !conn.deadlines[1].Equal(want[1]) {
		t.Fatalf("deadlines = %v, want %v", conn.deadlines, want)
	}
}

func TestControlServerClosesIdlePeer(t *testing.T) {
	listener := newQueuedListener()
	server := newServer("", "test", testBackends(&fakeBackend{document: emptyControlDocument()}), listener, nil, controlOptions{
		maxPeers: 16, idleTimeout: 25 * time.Millisecond, now: time.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	listener.enqueue(serverConn)
	listener.awaitAccepted(t, 1)
	if err := clientConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := clientConn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("idle peer read = %v, want close", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestControlServerShutdownWaitsForActiveCall(t *testing.T) {
	listener := newQueuedListener()
	backend := &blockingControlBackend{
		fakeBackend: &fakeBackend{document: emptyControlDocument()},
		started:     make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}),
	}
	server := newServer("", "test", testBackends(backend), listener, nil, defaultControlOptions())
	serverCtx, cancelServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(serverCtx) }()
	serverConn, clientConn := net.Pipe()
	listener.enqueue(serverConn)
	listener.awaitAccepted(t, 1)
	clientCtx, cancelClient := context.WithCancel(context.Background())
	client := &Client{peer: rpc.NewPeer(clientConn), cancel: cancelClient}
	go func() { _ = client.peer.Serve(clientCtx) }()
	callDone := make(chan error, 1)
	go func() {
		callDone <- client.Call(context.Background(), "app.launch", LaunchRequest{AppID: "ball8"}, nil)
	}()
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("active call did not start")
	}
	cancelServer()
	select {
	case <-backend.canceled:
	case <-time.After(time.Second):
		t.Fatal("active call did not observe shutdown cancellation")
	}
	select {
	case err := <-serverDone:
		t.Fatalf("Serve returned before active call unwound: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(backend.release)
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not join active call")
	}
	_ = client.Close()
	<-callDone
}

func TestControlServerReturnsGenericListenerErrorWithoutWatcherLeak(t *testing.T) {
	listener := newQueuedListener()
	listener.fail(errors.New("token=secret listener internals"))
	server := newServer("", "test", testBackends(&fakeBackend{document: emptyControlDocument()}), listener, nil, defaultControlOptions())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := server.Serve(ctx)
	if err == nil || err.Error() != "listener_failed" {
		t.Fatalf("Serve error = %v", err)
	}
}

func TestControlServerTreatsUnexpectedNetErrClosedAsListenerFailure(t *testing.T) {
	listener := newQueuedListener()
	listener.fail(net.ErrClosed)
	server := newServer("", "test", testBackends(&fakeBackend{document: emptyControlDocument()}), listener, nil, defaultControlOptions())
	err := server.Serve(context.Background())
	if err == nil || err.Error() != "listener_failed" {
		t.Fatalf("unexpected net.ErrClosed = %v", err)
	}
}

func TestControlServerClosesAndJoinsPeerOwnedGoroutinesBeforeReturning(t *testing.T) {
	listener := newQueuedListener()
	peer := newObservableControlPeer()
	t.Cleanup(peer.unblockClose)
	options := defaultControlOptions()
	options.newPeer = func(net.Conn) controlPeer { return peer }
	server := newServer("", "test", testBackends(&fakeBackend{document: emptyControlDocument()}), listener, nil, options)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	listener.enqueue(serverConn)
	listener.awaitAccepted(t, 1)
	awaitControlSignal(t, peer.serveStarted, "peer Serve entry")
	cancel()
	awaitControlSignal(t, peer.closeStarted, "explicit peer Close")
	select {
	case err := <-done:
		t.Fatalf("Serve returned before peer Close joined owned goroutines: %v", err)
	default:
	}
	peer.unblockClose()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after peer goroutines joined")
	}
}

func TestControlServerRemovesOnlyOwnedSocketIdentity(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "control.sock")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	owned, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &Server{path: path, info: owned}
	server.removeOwnSocket()
	if contents, err := os.ReadFile(path); err != nil || string(contents) != "replacement" {
		t.Fatalf("replacement socket owner was removed: contents=%q err=%v", contents, err)
	}
}

func startControlTestServer(t *testing.T, backend completeTestBackend) (*Server, *Client, context.CancelFunc) {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "bctl-errors-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	server, err := Listen(filepath.Join(directory, "control.sock"), "test", testBackends(backend))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = server.Serve(ctx) }()
	client, err := Dial(ctx, server.Path())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return server, client, cancel
}

type fakeBackend struct {
	document          config.Document
	launched          string
	deviceStatus      device.RuntimeStatus
	audioStatus       device.AudioStatus
	recorderStatus    attention.RecorderStatus
	logStatus         pluginlog.Status
	observationStatus observation.StoreDiagnostics
	attentionState    daemon.AttentionStateDiagnostics
	configStatus      daemon.ConfigPersistenceStatus
	checkpointStatus  checkpoint.Status
	sessionStatus     daemon.SessionDiagnostics
	operationErr      error
	launchCalls       int
	operationCalls    int
	operationResult   json.RawMessage
}

type completeTestBackend interface {
	AppsBackend
	CatalogBackend
	OperationsBackend
	AttentionBackend
	StatusBackend
}

func testBackends(backend completeTestBackend) Backends {
	return Backends{
		Apps: backend, Catalog: backend, Operations: backend, Attention: backend, Status: backend,
	}
}

type configValidationBackend struct {
	*fakeBackend
	createCalls  int
	replaceCalls int
}

func (b *configValidationBackend) CreateAppInstance(_ context.Context, app config.App) (daemon.AppInstanceResult, error) {
	b.createCalls++
	return daemon.AppInstanceResult{
		Document: config.Document{Version: config.CurrentVersion, Generation: 2}, Outcome: localstate.Committed,
		AppID: app.ID, PluginID: app.PluginID, Enabled: app.Enabled,
	}, nil
}

func (*configValidationBackend) DeleteAppInstance(context.Context, string) (daemon.AppInstanceResult, error) {
	return daemon.AppInstanceResult{}, nil
}

func (b *configValidationBackend) ReplaceAppConfiguration(_ context.Context, _ string, _ daemon.AppConfiguration) (config.Document, localstate.CommitOutcome, error) {
	b.replaceCalls++
	return config.Document{Version: config.CurrentVersion, Generation: 2}, localstate.Committed, nil
}

func (f *fakeBackend) RuntimeDiagnostics() daemon.RuntimeDiagnostics {
	return daemon.RuntimeDiagnostics{
		Device: f.deviceStatus, Audio: f.audioStatus, AttentionRecorder: f.recorderStatus, PluginLogs: f.logStatus, Observations: f.observationStatus,
		AttentionState: f.attentionState, Configuration: f.configStatus, Checkpoints: f.checkpointStatus, Session: f.sessionStatus,
	}
}
func (f *fakeBackend) PresentationCooldownStatus() daemon.PresentationCooldownDiagnostics {
	return f.RuntimeDiagnostics().PresentationCooldown
}

func (f *fakeBackend) SetEnabled(_ context.Context, appID string, enabled bool) (daemon.EnableResult, error) {
	if f.operationErr != nil {
		return daemon.EnableResult{}, f.operationErr
	}
	app := f.document.Apps[appID]
	changed := app.Enabled != enabled
	app.Enabled = enabled
	f.document.Apps[appID] = app
	return daemon.EnableResult{Document: f.document, Changed: changed, Outcome: localstate.Committed}, nil
}
func (f *fakeBackend) CreateAppInstance(_ context.Context, app config.App) (daemon.AppInstanceResult, error) {
	return daemon.AppInstanceResult{
		Document: f.document, AppID: app.ID, PluginID: app.PluginID, Enabled: app.Enabled, Outcome: localstate.Committed,
	}, nil
}
func (f *fakeBackend) DeleteAppInstance(_ context.Context, appID string) (daemon.AppInstanceResult, error) {
	return daemon.AppInstanceResult{Document: f.document, AppID: appID, Outcome: localstate.Committed}, nil
}
func (f *fakeBackend) ReplaceAppConfiguration(_ context.Context, _ string, _ daemon.AppConfiguration) (config.Document, localstate.CommitOutcome, error) {
	return f.document, localstate.Committed, nil
}
func (*fakeBackend) CatalogInstall(context.Context, installer.InstallRequest, bool) (installer.Result, error) {
	return installer.Result{}, nil
}
func (*fakeBackend) CatalogRollback(context.Context, installer.RollbackRequest) (installer.Result, error) {
	return installer.Result{}, nil
}
func (*fakeBackend) CatalogStatus(context.Context, string) (installer.Snapshot, error) {
	return installer.Snapshot{}, nil
}
func (f *fakeBackend) Launch(_ context.Context, appID, action string, _ json.RawMessage) error {
	f.launchCalls++
	if f.operationErr != nil {
		return f.operationErr
	}
	f.launched = appID + "/" + action
	return nil
}
func (f *fakeBackend) PluginOperation(_ context.Context, appID string, kind protocol.OperationKind, operation string, _ json.RawMessage) (protocol.OperationResult, error) {
	f.operationCalls++
	if f.operationErr != nil {
		return protocol.OperationResult{}, f.operationErr
	}
	if appID != "ball8" || kind != protocol.OperationQuery || operation != "sessions" {
		return protocol.OperationResult{}, errors.New("unexpected operation")
	}
	if f.operationResult != nil {
		return protocol.OperationResult{Payload: f.operationResult}, nil
	}
	return protocol.OperationResult{Payload: json.RawMessage(`{"sessions":[]}`)}, nil
}
func (f *fakeBackend) Document() (config.Document, bool)        { return f.document, true }
func (f *fakeBackend) Status() []pluginhost.PluginStatus        { return nil }
func (f *fakeBackend) DeviceStatus() device.RuntimeStatus       { return f.deviceStatus }
func (f *fakeBackend) RecorderStatus() attention.RecorderStatus { return f.recorderStatus }
func (f *fakeBackend) LogStatus() pluginlog.Status              { return f.logStatus }
func (f *fakeBackend) AttentionSnapshot() (attention.Trace, bool) {
	id := "plugin/ball8/main/state"
	return attention.Trace{Sequence: 1, SelectedID: id, Evaluations: []attention.Evaluation{{ObservationID: id, Reason: attention.ReasonSelected}}}, true
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
func (f *fakeBackend) AttentionExplain(id string) (attention.Evaluation, bool) {
	if id == "plugin/ball8/main/state" {
		return attention.Evaluation{ObservationID: id, Reason: attention.ReasonSelected}, true
	}
	return attention.Evaluation{}, false
}
func (f *fakeBackend) AttentionHistory(int, time.Time) []attention.Trace {
	value, _ := f.AttentionSnapshot()
	return []attention.Trace{value}
}
func (f *fakeBackend) AcknowledgeAttention(string) error { return f.operationErr }
func (*fakeBackend) Wake()                               {}
func (*fakeBackend) Reconcile(context.Context) error     { return nil }
func (f *fakeBackend) ObservationDiagnostics() observation.StoreDiagnostics {
	return f.observationStatus
}
func (*fakeBackend) AttentionStateStatus() daemon.AttentionStateDiagnostics {
	return daemon.AttentionStateDiagnostics{}
}

func emptyControlDocument() config.Document {
	return config.Document{Version: config.CurrentVersion, Generation: 1, Apps: map[string]config.App{}, Plugins: map[string]config.Plugin{}}
}

type queuedListener struct {
	connections chan net.Conn
	closed      chan struct{}
	closeOnce   sync.Once
	mu          sync.Mutex
	accepted    int
	err         error
}

type observableControlPeer struct {
	serveStarted chan struct{}
	closeStarted chan struct{}
	closeRelease chan struct{}
	serveOnce    sync.Once
	closeOnce    sync.Once
	releaseOnce  sync.Once
}

func newObservableControlPeer() *observableControlPeer {
	return &observableControlPeer{
		serveStarted: make(chan struct{}), closeStarted: make(chan struct{}), closeRelease: make(chan struct{}),
	}
}
func (*observableControlPeer) Handle(string, rpc.Handler) error { return nil }
func (p *observableControlPeer) Serve(ctx context.Context) error {
	p.serveOnce.Do(func() { close(p.serveStarted) })
	<-ctx.Done()
	return ctx.Err()
}
func (p *observableControlPeer) Close() error {
	p.closeOnce.Do(func() { close(p.closeStarted) })
	<-p.closeRelease
	return nil
}
func (p *observableControlPeer) unblockClose() { p.releaseOnce.Do(func() { close(p.closeRelease) }) }

type blockingControlBackend struct {
	*fakeBackend
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

func (b *blockingControlBackend) Launch(ctx context.Context, _, _ string, _ json.RawMessage) error {
	close(b.started)
	<-ctx.Done()
	close(b.canceled)
	<-b.release
	return ctx.Err()
}

func newQueuedListener() *queuedListener {
	return &queuedListener{connections: make(chan net.Conn, 32), closed: make(chan struct{})}
}

func (l *queuedListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		l.mu.Lock()
		l.accepted++
		l.mu.Unlock()
		return conn, nil
	case <-l.closed:
		l.mu.Lock()
		err := l.err
		l.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, net.ErrClosed
	}
}
func (l *queuedListener) Close() error          { l.closeOnce.Do(func() { close(l.closed) }); return nil }
func (*queuedListener) Addr() net.Addr          { return testAddr("control") }
func (l *queuedListener) enqueue(conn net.Conn) { l.connections <- conn }
func (l *queuedListener) fail(err error) {
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
	_ = l.Close()
}
func (l *queuedListener) awaitAccepted(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		accepted := l.accepted
		l.mu.Unlock()
		if accepted >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("accepted peers did not reach %d", want)
}

func awaitControlSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

type testAddr string

func (a testAddr) Network() string { return string(a) }
func (a testAddr) String() string  { return string(a) }

type sequenceClock struct {
	mu     sync.Mutex
	values []time.Time
}

func (c *sequenceClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	value := c.values[0]
	c.values = c.values[1:]
	return value
}

type deadlineRecordingConn struct{ deadlines []time.Time }

func (*deadlineRecordingConn) Read([]byte) (int, error)        { return 0, io.EOF }
func (*deadlineRecordingConn) Write(value []byte) (int, error) { return len(value), nil }
func (*deadlineRecordingConn) Close() error                    { return nil }
func (*deadlineRecordingConn) LocalAddr() net.Addr             { return testAddr("local") }
func (*deadlineRecordingConn) RemoteAddr() net.Addr            { return testAddr("remote") }
func (c *deadlineRecordingConn) SetDeadline(value time.Time) error {
	c.deadlines = append(c.deadlines, value)
	return nil
}
func (c *deadlineRecordingConn) SetReadDeadline(value time.Time) error {
	c.deadlines = append(c.deadlines, value)
	return nil
}
func (*deadlineRecordingConn) SetWriteDeadline(time.Time) error { return nil }
