package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPluginOperationRoutesOnlyDeclaredEnabledGeneration(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	plugin := document.Plugins["plugin"]
	plugin.Operations = []protocol.OperationDescriptor{{ID: "inspect", Kind: protocol.OperationQuery}}
	document.Plugins["plugin"] = plugin
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	plugins := &operationPluginController{result: protocol.OperationResult{Payload: json.RawMessage(`{"status":"ready"}`)}}
	service := newTestReconciler(t, store, nil, plugins)
	t.Cleanup(func() { _ = service.Close(t.Context()) })
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

	payload := json.RawMessage(`{"detail":true}`)
	result, err := service.PluginOperation(t.Context(), "ball8", protocol.OperationQuery, "inspect", payload)
	if err != nil || string(result.Payload) != `{"status":"ready"}` {
		t.Fatalf("PluginOperation = %s, %v", result.Payload, err)
	}
	pluginID, request := plugins.operation()
	if pluginID != "plugin" || request.Instance.ID != "ball8" || request.Instance.Generation != 1 || request.Operation != "inspect" || string(request.Payload) != string(payload) {
		t.Fatalf("routed operation = plugin %q request %#v", pluginID, request)
	}
	payload[2] = 'X'
	if string(request.Payload) != `{"detail":true}` {
		t.Fatalf("operation payload aliased caller memory: %s", request.Payload)
	}
}

func TestPluginOperationRejectsUnavailableRoutesBeforeController(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	plugin := document.Plugins["plugin"]
	plugin.Operations = []protocol.OperationDescriptor{{ID: "inspect", Kind: protocol.OperationQuery}}
	document.Plugins["plugin"] = plugin
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	plugins := &operationPluginController{}
	service := newTestReconciler(t, store, nil, plugins)
	t.Cleanup(func() { _ = service.Close(t.Context()) })
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		appID     string
		kind      protocol.OperationKind
		operation string
	}{
		{name: "missing app", appID: "missing", kind: protocol.OperationQuery, operation: "inspect"},
		{name: "undeclared operation", appID: "ball8", kind: protocol.OperationQuery, operation: "missing"},
		{name: "wrong kind", appID: "ball8", kind: protocol.OperationCommand, operation: "inspect"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.PluginOperation(t.Context(), test.appID, test.kind, test.operation, json.RawMessage(`{}`))
			if !errors.Is(err, ErrAppNotEnabled) {
				t.Fatalf("PluginOperation error = %v, want ErrAppNotEnabled", err)
			}
		})
	}
	if plugins.calls != 0 {
		t.Fatalf("controller calls = %d, want 0", plugins.calls)
	}
}

func TestPluginOperationValidatesPayloadAndResultAtServiceBoundary(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	plugin := document.Plugins["plugin"]
	plugin.Operations = []protocol.OperationDescriptor{{ID: "inspect", Kind: protocol.OperationQuery}}
	document.Plugins["plugin"] = plugin
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	plugins := &operationPluginController{result: protocol.OperationResult{Payload: json.RawMessage(`{"status":"ready"}`)}}
	service := newTestReconciler(t, store, nil, plugins)
	t.Cleanup(func() { _ = service.Close(t.Context()) })
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

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
		if _, err := service.PluginOperation(t.Context(), "ball8", protocol.OperationQuery, "inspect", payload); err == nil {
			t.Fatalf("invalid operation payload %s was accepted", payload[:min(len(payload), 64)])
		}
	}
	if plugins.calls != 0 {
		t.Fatalf("controller calls after invalid payloads = %d, want 0", plugins.calls)
	}
	if _, err := service.PluginOperation(t.Context(), "ball8", protocol.OperationQuery, "inspect", objectOfSize(protocol.MaxJSONObjectBytes)); err != nil {
		t.Fatalf("operation payload at exact limit: %v", err)
	}
	if plugins.calls != 1 {
		t.Fatalf("controller calls after valid boundary payload = %d, want 1", plugins.calls)
	}

	plugins.result = protocol.OperationResult{Payload: json.RawMessage(`null`)}
	result, err := service.PluginOperation(t.Context(), "ball8", protocol.OperationQuery, "inspect", json.RawMessage(`{}`))
	if err == nil || result.Payload != nil {
		t.Fatalf("invalid operation result = %s, %v; want rejected empty result", result.Payload, err)
	}
}

type operationPluginController struct {
	safePluginController
	muOperation sync.Mutex
	calls       int
	pluginID    string
	request     protocol.OperationRequest
	result      protocol.OperationResult
	err         error
}

func (f *operationPluginController) Operation(_ context.Context, pluginID string, request protocol.OperationRequest) (protocol.OperationResult, error) {
	f.muOperation.Lock()
	defer f.muOperation.Unlock()
	f.calls++
	f.pluginID = pluginID
	f.request = request
	return f.result, f.err
}

func (f *operationPluginController) operation() (string, protocol.OperationRequest) {
	f.muOperation.Lock()
	defer f.muOperation.Unlock()
	return f.pluginID, f.request
}
