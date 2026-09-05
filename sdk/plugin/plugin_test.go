package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

type basePlugin struct {
	mu            sync.Mutex
	replacements  [][]protocol.Instance
	replaceErr    error
	shutdownCalls int
}

func (p *basePlugin) ReplaceInstances(_ context.Context, instances []protocol.Instance) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.replacements = append(p.replacements, append([]protocol.Instance(nil), instances...))
	return p.replaceErr
}
func (p *basePlugin) Shutdown(context.Context) error { p.shutdownCalls++; return nil }

type interactivePlugin struct {
	basePlugin
	inputResult protocol.SessionInputResult
	inputErr    error
	inputs      chan protocol.SessionInputRequest
}

func (*interactivePlugin) StartSession(context.Context, protocol.SessionStartRequest) error {
	return nil
}

func (p *interactivePlugin) HandleSessionInput(_ context.Context, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	p.inputs <- request
	return p.inputResult, p.inputErr
}

func (*interactivePlugin) EndSession(context.Context, protocol.SessionEndRequest) error {
	return nil
}

func testDefinition(factory func(*Host) Plugin) Definition {
	return Definition{ID: "dev.bsbctl.test", Version: "1", Contract: Contract{ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident}, Channels: []protocol.Channel{{ID: "main"}}}, New: factory}
}

func TestInitializeUsesExactV1AndConstructsHandlerOnlyAfterValidation(t *testing.T) {
	created := 0
	definition := testDefinition(func(*Host) Plugin { created++; return &basePlugin{} })
	runtime := &runtime{definition: definition, host: &Host{}}
	bad, _ := json.Marshal(protocol.InitializeRequest{CoreVersion: "1", PluginID: definition.ID, PluginVersion: definition.Version, ProtocolVersion: "3.5"})
	if _, rpcErr := runtime.initialize(t.Context(), bad); rpcErr == nil {
		t.Fatal("incompatible protocol was accepted")
	}
	if created != 0 {
		t.Fatalf("factory calls = %d before valid initialize", created)
	}
	good, _ := json.Marshal(protocol.InitializeRequest{CoreVersion: "1", PluginID: definition.ID, PluginVersion: definition.Version, ProtocolVersion: protocol.Version})
	result, rpcErr := runtime.initialize(t.Context(), good)
	if rpcErr != nil {
		t.Fatalf("initialize: %#v", rpcErr)
	}
	if created != 1 {
		t.Fatalf("factory calls = %d, want 1", created)
	}
	initialized := result.(protocol.InitializeResult)
	if initialized.ProtocolVersion != "1.0" || initialized.PluginID != definition.ID {
		t.Fatalf("initialize result = %#v", initialized)
	}
}

func TestDefinitionContractRequiresOnlyDeclaredOptionalInterfaces(t *testing.T) {
	resident := testDefinition(func(*Host) Plugin { return &basePlugin{} })
	if err := validateDefinition(resident); err != nil {
		t.Fatal(err)
	}
	if err := validateHandlerContract(resident.Contract, resident.New(&Host{})); err != nil {
		t.Fatal(err)
	}
	unsupported := resident
	unsupported.Contract.ExecutionModes = []protocol.ExecutionMode{"scheduled"}
	if err := validateDefinition(unsupported); err == nil {
		t.Fatal("scheduled execution mode was accepted")
	}
	interactive := resident.Contract
	interactive.ExecutionModes = []protocol.ExecutionMode{protocol.ExecutionModeInteractive}
	if err := validateHandlerContract(interactive, &basePlugin{}); err == nil {
		t.Fatal("interactive contract accepted without SessionHandler")
	}
}

func TestReplaceInstancesIsSeparateFromInitializeAndUnlocksHostCalls(t *testing.T) {
	plugin := &basePlugin{}
	runtime := &runtime{definition: testDefinition(func(*Host) Plugin { return plugin }), host: &Host{}}
	initialize, _ := json.Marshal(protocol.InitializeRequest{CoreVersion: "1", PluginID: "dev.bsbctl.test", PluginVersion: "1", ProtocolVersion: "1.0"})
	if _, rpcErr := runtime.initialize(t.Context(), initialize); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if runtime.host.ready.Load() {
		t.Fatal("host was ready before desired state replacement")
	}
	replace := json.RawMessage(`{"instances":[{"id":"main","generation":7,"config":{}}]}`)
	if _, rpcErr := runtime.replaceInstances(t.Context(), replace); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !runtime.host.ready.Load() {
		t.Fatal("host remained not_ready after successful replacement")
	}
	if len(plugin.replacements) != 1 || plugin.replacements[0][0].Generation != 7 {
		t.Fatalf("replacements = %#v", plugin.replacements)
	}
}

func TestFailedReplacementDoesNotMakeHostReady(t *testing.T) {
	plugin := &basePlugin{replaceErr: errors.New("private failure")}
	runtime := &runtime{definition: testDefinition(func(*Host) Plugin { return plugin }), host: &Host{}}
	initialize, _ := json.Marshal(protocol.InitializeRequest{CoreVersion: "1", PluginID: "dev.bsbctl.test", PluginVersion: "1", ProtocolVersion: "1.0"})
	_, _ = runtime.initialize(t.Context(), initialize)
	if _, rpcErr := runtime.replaceInstances(t.Context(), json.RawMessage(`{"instances":[]}`)); rpcErr == nil || rpcErr.Code != -32603 || string(rpcErr.Data) != "" {
		t.Fatalf("replacement error leaked detail or was nil: %#v", rpcErr)
	}
	if runtime.host.ready.Load() {
		t.Fatal("failed replacement made host ready")
	}
}

func TestPermanentConfigurationReplacementReturnsOnlyInvalidArgumentKind(t *testing.T) {
	plugin := &basePlugin{replaceErr: PermanentConfiguration(errors.New("token=secret /Users/private"))}
	runtime := &runtime{definition: testDefinition(func(*Host) Plugin { return plugin }), host: &Host{}}
	initialize, _ := json.Marshal(protocol.InitializeRequest{CoreVersion: "1", PluginID: "dev.bsbctl.test", PluginVersion: "1", ProtocolVersion: "1.0"})
	_, _ = runtime.initialize(t.Context(), initialize)
	_, rpcErr := runtime.replaceInstances(t.Context(), json.RawMessage(`{"instances":[]}`))
	if rpcErr == nil || rpcErr.Code != protocol.DomainErrorCode {
		t.Fatalf("replacement error = %#v", rpcErr)
	}
	var data protocol.ErrorData
	if err := protocol.DecodeStrict(rpcErr.Data, &data); err != nil || data.Kind != protocol.ErrorInvalidArgument {
		t.Fatalf("error data = %s, %v", rpcErr.Data, err)
	}
	if strings.Contains(string(rpcErr.Data), "secret") || strings.Contains(string(rpcErr.Data), "/Users/") {
		t.Fatalf("wire error leaked private detail: %s", rpcErr.Data)
	}
}

func TestRejectSecretsRejectsEveryUndeclaredSecretWithoutDisclosingValues(t *testing.T) {
	t.Parallel()
	err := RejectSecrets("app", map[string]string{"token": "keychain://private/account"})
	if err == nil || !IsPermanentConfiguration(err) {
		t.Fatalf("RejectSecrets error = %v", err)
	}
	if strings.Contains(err.Error(), "keychain://") {
		t.Fatalf("RejectSecrets disclosed a secret reference: %v", err)
	}
	if err := RejectSecrets("app", nil); err != nil {
		t.Fatalf("RejectSecrets nil = %v", err)
	}
}

func TestReplacementSuspendsHostEffectsAndRestoresPriorReadinessOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%t", fail), func(t *testing.T) {
			host := &Host{}
			host.ready.Store(true)
			plugin := &readinessReplacementPlugin{started: make(chan struct{}), release: make(chan struct{}), fail: fail}
			runtime := &runtime{handler: plugin, initialized: true, host: host}
			done := make(chan *rpc.Error, 1)
			go func() {
				_, rpcErr := runtime.replaceInstances(t.Context(), json.RawMessage(`{"instances":[]}`))
				done <- rpcErr
			}()
			<-plugin.started
			if host.ready.Load() {
				t.Fatal("host remained ready while replacement was in progress")
			}
			err := host.BeginSessionExecution(t.Context(), protocol.SessionExecutionRequest{Instance: protocol.InstanceRef{ID: "main", Generation: 1}, SessionToken: "retiring"})
			denied, ok := errors.AsType[*protocol.DomainError](err)
			if !ok || denied.Kind() != protocol.ErrorSessionGenerationMismatch {
				t.Fatalf("replacement execution denial = %v", err)
			}
			close(plugin.release)
			rpcErr := <-done
			if fail == (rpcErr == nil) {
				t.Fatalf("replacement error = %#v, fail=%t", rpcErr, fail)
			}
			if !host.ready.Load() {
				t.Fatal("replacement did not restore the previously committed host readiness")
			}
		})
	}
}

func TestHostWithdrawRejectsInvalidIdentityBeforeTransport(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	left, right := rpc.NewPeer(leftConn), rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })
	if err := right.Handle("host.observation.withdraw", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = left.Serve(ctx) }()
	go func() { _ = right.Serve(ctx) }()
	host := &Host{peer: left}
	host.ready.Store(true)
	err := host.WithdrawObservation(ctx, protocol.WithdrawRequest{
		Instance: protocol.InstanceRef{ID: "main", Generation: 1}, Channel: "", Key: "",
	})
	domain, ok := errors.AsType[*protocol.DomainError](err)
	if !ok || domain.Kind() != protocol.ErrorInvalidArgument {
		t.Fatalf("WithdrawObservation error = %v, want invalid_argument", err)
	}
}

func TestHostMethodsValidateAndSendExactV1Effects(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	pluginPeer, corePeer := rpc.NewPeer(leftConn), rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = pluginPeer.Close(); _ = corePeer.Close() })
	type received struct {
		method string
		raw    json.RawMessage
	}
	receivedCalls := make(chan received, 7)
	for _, method := range []string{
		"host.observation.publish",
		"host.observation.withdraw",
		"host.checkpoint.save",
		"host.session.execution.begin",
		"host.session.complete",
		"host.log",
		"host.metric",
	} {
		if err := corePeer.Handle(method, func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
			receivedCalls <- received{method: method, raw: append(json.RawMessage(nil), raw...)}
			return struct{}{}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	go func() { _ = pluginPeer.Serve(t.Context()) }()
	go func() { _ = corePeer.Serve(t.Context()) }()
	host := &Host{peer: pluginPeer}
	host.ready.Store(true)
	ref := protocol.InstanceRef{ID: "main", Generation: 7}
	now := time.Now().UTC()
	observation := protocol.Observation{
		Instance: ref, Channel: "main", Key: "state", Revision: 1,
		Disposition: protocol.DispositionResolved, Impact: protocol.ImpactNormal, ReasonCode: "complete",
		ObservedAt: now, UpdatedAt: now,
	}
	withdraw := protocol.WithdrawRequest{Instance: ref, Channel: "main", Key: "state"}
	checkpoint := protocol.CheckpointRequest{Instance: ref, Data: json.RawMessage(`{"cursor":2}`)}
	execution := protocol.SessionExecutionRequest{Instance: ref, SessionToken: "session-1"}
	complete := protocol.CompleteSessionRequest{Instance: ref, SessionToken: "session-1"}
	logValue := protocol.LogNotification{Level: protocol.LogLevelInfo, Event: "refresh.complete", Instance: ref, Fields: map[string]string{"source": "test"}}
	metric := protocol.MetricNotification{Instance: ref, Name: "refresh.count", Value: 1, Unit: "items"}

	if err := host.PublishObservation(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	if err := host.WithdrawObservation(t.Context(), withdraw); err != nil {
		t.Fatal(err)
	}
	if err := host.SaveCheckpoint(t.Context(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := host.BeginSessionExecution(t.Context(), execution); err != nil {
		t.Fatal(err)
	}
	if err := host.CompleteSession(t.Context(), complete); err != nil {
		t.Fatal(err)
	}
	if err := host.Log(t.Context(), logValue); err != nil {
		t.Fatal(err)
	}
	if accepted, err := host.RecordMetric(metric); err != nil || !accepted {
		t.Fatalf("RecordMetric = %t, %v", accepted, err)
	}

	want := map[string]any{
		"host.observation.publish":     protocol.PublishRequest{Observation: observation},
		"host.observation.withdraw":    withdraw,
		"host.checkpoint.save":         checkpoint,
		"host.session.execution.begin": execution,
		"host.session.complete":        complete,
		"host.log":                     logValue,
		"host.metric":                  metric,
	}
	for range want {
		select {
		case call := <-receivedCalls:
			encoded, err := json.Marshal(want[call.method])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(call.raw, encoded) {
				t.Fatalf("%s payload = %s, want %s", call.method, call.raw, encoded)
			}
			delete(want, call.method)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for host effects; %d calls missing", len(want))
		}
	}
}

type readinessReplacementPlugin struct {
	started chan struct{}
	release chan struct{}
	fail    bool
}

func (p *readinessReplacementPlugin) ReplaceInstances(context.Context, []protocol.Instance) error {
	close(p.started)
	<-p.release
	if p.fail {
		return errors.New("replacement failed")
	}
	return nil
}

func TestStateChangingHostMethodsFailTypedNotReady(t *testing.T) {
	host := &Host{}
	err := host.SaveCheckpoint(t.Context(), protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: "main", Generation: 1}, Data: json.RawMessage(`{}`)})
	domain, matched := errors.AsType[*protocol.DomainError](err)
	if !matched || domain.Kind() != protocol.ErrorNotReady {
		t.Fatalf("error = %v, want typed not_ready", err)
	}
	err = host.BeginSessionExecution(t.Context(), protocol.SessionExecutionRequest{Instance: protocol.InstanceRef{ID: "main", Generation: 1}, SessionToken: "session"})
	domain, matched = errors.AsType[*protocol.DomainError](err)
	if !matched || domain.Kind() != protocol.ErrorSessionGenerationMismatch {
		t.Fatalf("execution denial = %v, want typed session_generation_mismatch", err)
	}
}

func TestHostRejectsAndRedactsUnknownRemoteDomainKind(t *testing.T) {
	const hostileKind = "authorization=Bearer hostile-domain-kind-secret"
	leftConn, rightConn := net.Pipe()
	pluginPeer, corePeer := rpc.NewPeer(leftConn), rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = pluginPeer.Close(); _ = corePeer.Close() })
	if err := corePeer.Handle("host.observation.publish", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		data, marshalErr := json.Marshal(map[string]string{"kind": hostileKind})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return nil, &rpc.Error{Code: protocol.DomainErrorCode, Message: hostileKind, Data: data}
	}); err != nil {
		t.Fatal(err)
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- pluginPeer.Serve(t.Context()) }()
	go func() { _ = corePeer.Serve(t.Context()) }()
	host := &Host{peer: pluginPeer}
	host.ready.Store(true)
	now := time.Now().UTC()
	err := host.PublishObservation(t.Context(), protocol.Observation{
		Instance: protocol.InstanceRef{ID: "main", Generation: 1}, Channel: "main", Key: "state", Revision: 1,
		Disposition: protocol.DispositionResolved, Impact: protocol.ImpactNormal, ReasonCode: "test_state",
		ObservedAt: now, UpdatedAt: now,
	})
	if !errors.Is(err, rpc.ErrProtocol) {
		t.Fatalf("PublishObservation error = %v, want protocol violation", err)
	}
	if strings.Contains(err.Error(), hostileKind) {
		t.Fatalf("protocol error leaked hostile domain kind: %v", err)
	}
	select {
	case <-pluginPeer.Done():
	case <-time.After(time.Second):
		t.Fatal("unknown domain kind left SDK peer usable")
	}
	select {
	case serveErr := <-serveResult:
		if !errors.Is(serveErr, rpc.ErrProtocol) || strings.Contains(serveErr.Error(), hostileKind) {
			t.Fatalf("Serve error = %v, want redacted protocol violation", serveErr)
		}
	case <-time.After(time.Second):
		t.Fatal("terminated SDK peer did not return from Serve")
	}
}

func TestRecordMetricRejectsInvalidNotificationLocally(t *testing.T) {
	left, right := net.Pipe()
	host := &Host{peer: rpc.NewPeer(left)}
	t.Cleanup(func() { _ = host.peer.Close(); _ = right.Close() })
	accepted, err := host.RecordMetric(protocol.MetricNotification{Name: "", Value: 1})
	domain, matched := errors.AsType[*protocol.DomainError](err)
	if accepted || !matched || domain.Kind() != protocol.ErrorInvalidArgument {
		t.Fatalf("RecordMetric = %t, %v; want typed invalid_argument", accepted, err)
	}
}

func TestRuntimeRegistersOnlyV1MethodNames(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	pluginPeer, corePeer := rpc.NewPeer(leftConn), rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = pluginPeer.Close(); _ = corePeer.Close() })
	runtime := &runtime{definition: testDefinition(func(*Host) Plugin { return &basePlugin{} }), host: &Host{peer: pluginPeer}}
	if err := runtime.register(pluginPeer); err != nil {
		t.Fatal(err)
	}
	go func() { _ = pluginPeer.Serve(t.Context()) }()
	go func() { _ = corePeer.Serve(t.Context()) }()
	request := protocol.InitializeRequest{CoreVersion: "1", PluginID: "dev.bsbctl.test", PluginVersion: "1", ProtocolVersion: "1.0"}
	var result protocol.InitializeResult
	if err := corePeer.Call(t.Context(), "plugin.initialize", request, &result); err != nil {
		t.Fatal(err)
	}
	if err := corePeer.Call(t.Context(), "plugin.reconcile", struct{}{}, nil); err == nil {
		t.Fatal("v3 plugin.reconcile remained registered")
	}
	err := corePeer.Call(t.Context(), "plugin.run", struct{}{}, nil)
	rpcErr, ok := errors.AsType[*rpc.Error](err)
	if !ok || rpcErr.Code != -32601 {
		t.Fatalf("plugin.run error = %v, want method not found", err)
	}
	err = corePeer.Call(t.Context(), "plugin.event.deliver", struct{}{}, nil)
	rpcErr, ok = errors.AsType[*rpc.Error](err)
	if !ok || rpcErr.Code != -32601 {
		t.Fatalf("plugin.event.deliver error = %v, want method not found", err)
	}
}

func TestHealthAndShutdownRejectInvalidParams(t *testing.T) {
	plugin := &basePlugin{}
	runtime := &runtime{handler: plugin, initialized: true, host: &Host{}}
	for _, test := range []struct {
		name string
		call func(context.Context, json.RawMessage) (any, *rpc.Error)
	}{
		{name: "health", call: runtime.health},
		{name: "shutdown", call: runtime.shutdown},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, params := range []json.RawMessage{
				json.RawMessage(`{"extra":true}`),
				json.RawMessage(`[]`),
				json.RawMessage(`null`),
				json.RawMessage(`"scalar"`),
			} {
				if _, rpcErr := test.call(t.Context(), params); rpcErr == nil || rpcErr.Code != -32602 {
					t.Fatalf("params %s error = %#v, want invalid params", params, rpcErr)
				}
			}
		})
	}
	if plugin.shutdownCalls != 0 {
		t.Fatalf("shutdown calls = %d after invalid params, want 0", plugin.shutdownCalls)
	}
}

func TestSessionInputIsAcknowledgedAndCallbackErrorReachesCaller(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	pluginPeer, corePeer := rpc.NewPeer(leftConn), rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = pluginPeer.Close(); _ = corePeer.Close() })
	handler := &interactivePlugin{inputErr: errors.New("input rejected"), inputs: make(chan protocol.SessionInputRequest, 1)}
	definition := testDefinition(func(*Host) Plugin { return handler })
	definition.Contract.ExecutionModes = []protocol.ExecutionMode{protocol.ExecutionModeInteractive}
	runtime := &runtime{definition: definition, host: &Host{peer: pluginPeer}}
	if err := runtime.register(pluginPeer); err != nil {
		t.Fatal(err)
	}
	go func() { _ = pluginPeer.Serve(t.Context()) }()
	go func() { _ = corePeer.Serve(t.Context()) }()
	initialize := protocol.InitializeRequest{CoreVersion: "1", PluginID: definition.ID, PluginVersion: definition.Version, ProtocolVersion: protocol.Version}
	var initialized protocol.InitializeResult
	if err := corePeer.Call(t.Context(), "plugin.initialize", initialize, &initialized); err != nil {
		t.Fatal(err)
	}
	if err := corePeer.Call(t.Context(), "plugin.instances.replace", protocol.ReplaceInstancesRequest{Instances: []protocol.Instance{{ID: "main", Generation: 7, Config: json.RawMessage(`{}`)}}}, nil); err != nil {
		t.Fatal(err)
	}
	request := protocol.SessionInputRequest{
		Sequence: 1, OccurredAt: time.Now().UTC(), Instance: protocol.InstanceRef{ID: "main", Generation: 7},
		SessionToken: "session", Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonOK, Action: protocol.ButtonPress}},
	}
	err := corePeer.Call(t.Context(), "plugin.session.input", request, nil)
	if err == nil {
		t.Fatal("session input callback error was not acknowledged")
	}
	if got := <-handler.inputs; got.Sequence != request.Sequence || got.Instance != request.Instance {
		t.Fatalf("session input = %#v, want %#v", got, request)
	}
}

func TestSessionInputReturnsAndValidatesDisposition(t *testing.T) {
	for _, test := range []struct {
		name        string
		disposition protocol.SessionInputDisposition
		wantError   bool
	}{
		{name: "consumed", disposition: protocol.SessionInputConsumed},
		{name: "not consumed", disposition: protocol.SessionInputNotConsumed},
		{name: "missing", wantError: true},
		{name: "unknown", disposition: "handled", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			leftConn, rightConn := net.Pipe()
			pluginPeer, corePeer := rpc.NewPeer(leftConn), rpc.NewPeer(rightConn)
			t.Cleanup(func() { _ = pluginPeer.Close(); _ = corePeer.Close() })
			handler := &interactivePlugin{
				inputResult: protocol.SessionInputResult{Disposition: test.disposition},
				inputs:      make(chan protocol.SessionInputRequest, 1),
			}
			definition := testDefinition(func(*Host) Plugin { return handler })
			definition.Contract.ExecutionModes = []protocol.ExecutionMode{protocol.ExecutionModeInteractive}
			runtime := &runtime{definition: definition, host: &Host{peer: pluginPeer}}
			if err := runtime.register(pluginPeer); err != nil {
				t.Fatal(err)
			}
			go func() { _ = pluginPeer.Serve(t.Context()) }()
			go func() { _ = corePeer.Serve(t.Context()) }()
			initialize := protocol.InitializeRequest{CoreVersion: "1", PluginID: definition.ID, PluginVersion: definition.Version, ProtocolVersion: protocol.Version}
			if err := corePeer.Call(t.Context(), "plugin.initialize", initialize, &protocol.InitializeResult{}); err != nil {
				t.Fatal(err)
			}
			if err := corePeer.Call(t.Context(), "plugin.instances.replace", protocol.ReplaceInstancesRequest{Instances: []protocol.Instance{{ID: "main", Generation: 7, Config: json.RawMessage(`{}`)}}}, nil); err != nil {
				t.Fatal(err)
			}
			request := protocol.SessionInputRequest{
				Sequence: 1, OccurredAt: time.Now().UTC(), Instance: protocol.InstanceRef{ID: "main", Generation: 7}, SessionToken: "session",
				Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonBack, Action: protocol.ButtonPress}},
			}
			var result protocol.SessionInputResult
			err := corePeer.Call(t.Context(), "plugin.session.input", request, &result)
			if test.wantError {
				if err == nil {
					t.Fatalf("invalid disposition %q was accepted", test.disposition)
				}
				return
			}
			if err != nil || result.Disposition != test.disposition {
				t.Fatalf("result = %#v, err = %v", result, err)
			}
		})
	}
}

func TestRuntimeDispatchesDeclaredSessionAndOperationCallbacks(t *testing.T) {
	handler := &dispatchPlugin{}
	runtime := &runtime{
		definition:  Definition{Contract: Contract{Operations: []protocol.OperationDescriptor{{ID: "inspect", Kind: protocol.OperationQuery}}}},
		handler:     handler,
		initialized: true,
		host:        &Host{},
	}
	ref := protocol.InstanceRef{ID: "main", Generation: 7}
	start := protocol.SessionStartRequest{Instance: ref, Action: "open", Payload: json.RawMessage(`{"source":"test"}`), SessionToken: "session-1"}
	end := protocol.SessionEndRequest{Instance: ref, SessionToken: "session-1"}
	operation := protocol.OperationRequest{Instance: ref, Operation: "inspect", Payload: json.RawMessage(`{"detail":true}`)}
	startRaw, _ := json.Marshal(start)
	endRaw, _ := json.Marshal(end)
	operationRaw, _ := json.Marshal(operation)
	if _, rpcErr := runtime.startSession(t.Context(), startRaw); rpcErr != nil {
		t.Fatalf("startSession: %#v", rpcErr)
	}
	if _, rpcErr := runtime.endSession(t.Context(), endRaw); rpcErr != nil {
		t.Fatalf("endSession: %#v", rpcErr)
	}
	result, rpcErr := runtime.invokeOperation(t.Context(), operationRaw)
	if rpcErr != nil {
		t.Fatalf("invokeOperation: %#v", rpcErr)
	}
	if handler.start.Instance != ref || handler.start.SessionToken != "session-1" || handler.end != end || handler.operation.Instance != ref || handler.operation.Operation != "inspect" {
		t.Fatalf("callbacks = start %#v end %#v operation %#v", handler.start, handler.end, handler.operation)
	}
	if got := result.(protocol.OperationResult); string(got.Payload) != `{"accepted":true}` {
		t.Fatalf("operation result = %s", got.Payload)
	}

	undeclared := operation
	undeclared.Operation = "missing"
	undeclaredRaw, _ := json.Marshal(undeclared)
	if _, rpcErr := runtime.invokeOperation(t.Context(), undeclaredRaw); rpcErr == nil || rpcErr.Code != -32602 || handler.operation.Operation != "inspect" {
		t.Fatalf("undeclared operation error = %#v; callback = %#v", rpcErr, handler.operation)
	}
}

type dispatchPlugin struct {
	basePlugin
	start     protocol.SessionStartRequest
	end       protocol.SessionEndRequest
	operation protocol.OperationRequest
}

func (p *dispatchPlugin) StartSession(_ context.Context, request protocol.SessionStartRequest) error {
	p.start = request
	return nil
}

func (*dispatchPlugin) HandleSessionInput(context.Context, protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, nil
}

func (p *dispatchPlugin) EndSession(_ context.Context, request protocol.SessionEndRequest) error {
	p.end = request
	return nil
}

func (p *dispatchPlugin) InvokeOperation(_ context.Context, request protocol.OperationRequest) (protocol.OperationResult, error) {
	p.operation = request
	return protocol.OperationResult{Payload: json.RawMessage(`{"accepted":true}`)}, nil
}

func TestRuntimeShutdownCallsOptionalHandlerExactlyOnce(t *testing.T) {
	plugin := &basePlugin{}
	runtime := &runtime{handler: plugin, initialized: true, host: &Host{}}
	if err := runtime.shutdownRuntime(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.shutdownRuntime(t.Context()); err != nil {
		t.Fatal(err)
	}
	if plugin.shutdownCalls != 1 {
		t.Fatalf("shutdown calls = %d, want 1", plugin.shutdownCalls)
	}
}

func TestEOFReturnsWithinShutdownDeadlineWhenHandlerIgnoresContext(t *testing.T) {
	block := make(chan struct{})
	plugin := &blockingShutdownPlugin{block: block}
	runtime := &runtime{handler: plugin, initialized: true, host: &Host{}}
	leftConn, rightConn := net.Pipe()
	peer := rpc.NewPeer(leftConn)
	t.Cleanup(func() { close(block); _ = peer.Close() })
	served := make(chan error, 1)
	go func() {
		serveErr, shutdownErr := serveAndShutdown(context.Background(), peer, runtime)
		served <- errors.Join(serveErr, shutdownErr)
	}()
	_ = rightConn.Close()
	select {
	case err := <-served:
		if !errors.Is(err, io.EOF) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, rpc.ErrClosed) {
			t.Fatalf("serve/shutdown error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runtime remained blocked after shutdown deadline")
	}
}

type blockingShutdownPlugin struct{ block <-chan struct{} }

func (*blockingShutdownPlugin) ReplaceInstances(context.Context, []protocol.Instance) error {
	return nil
}
func (p *blockingShutdownPlugin) Shutdown(context.Context) error { <-p.block; return nil }
