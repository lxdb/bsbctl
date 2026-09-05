package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
	"net"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInitializeResultRequiresExactV1Identity(t *testing.T) {
	spec := Spec{ID: "dev.bsbctl.test", Version: "1", ProtocolVersion: protocol.Version}
	want := protocol.InitializeResult{PluginID: spec.ID, PluginVersion: spec.Version, ProtocolVersion: protocol.Version}
	if !matchesInitializeResult(spec, want) {
		t.Fatal("host rejected exact v1 identity")
	}
	for name, mutate := range map[string]func(*protocol.InitializeResult){
		"plugin id":        func(result *protocol.InitializeResult) { result.PluginID = "dev.bsbctl.other" },
		"plugin version":   func(result *protocol.InitializeResult) { result.PluginVersion = "2" },
		"protocol version": func(result *protocol.InitializeResult) { result.ProtocolVersion = "3.4" },
	} {
		t.Run(name, func(t *testing.T) {
			result := want
			mutate(&result)
			if matchesInitializeResult(spec, result) {
				t.Fatalf("host accepted mismatched initialize result %#v", result)
			}
		})
	}
}

func TestProcessRejectsInvalidReplacementBeforePendingOrRPC(t *testing.T) {
	for _, test := range invalidDesiredInstanceCases(t) {
		t.Run(test.name, func(t *testing.T) {
			leftConn, rightConn := net.Pipe()
			currentInstances := []Instance{testProcessInstance(7)}
			pendingInstance := testProcessInstance(6)
			pendingInstance.ID = "pending"
			pendingInstances := []Instance{pendingInstance}
			wantCurrent := instanceMap(cloneInstances(currentInstances))
			wantPending := instanceMap(cloneInstances(pendingInstances))
			process := &Process{
				peer:      rpc.NewPeer(leftConn),
				instances: instanceMap(cloneInstances(currentInstances)), pending: instanceMap(cloneInstances(pendingInstances)),
			}
			remote := rpc.NewPeer(rightConn)
			t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
			var replacementCalls atomic.Int32
			if err := remote.Handle("plugin.instances.replace", func(context.Context, json.RawMessage) (any, *rpc.Error) {
				replacementCalls.Add(1)
				return struct{}{}, nil
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			go func() { _ = process.peer.Serve(ctx) }()
			go func() { _ = remote.Serve(ctx) }()

			if err := process.ReplaceInstances(ctx, test.instances); err == nil {
				t.Fatal("invalid replacement was accepted")
			}
			if replacementCalls.Load() != 0 {
				t.Fatalf("replacement RPC calls = %d, want 0", replacementCalls.Load())
			}
			process.policyMu.RLock()
			gotCurrent, gotPending := process.instances, process.pending
			process.policyMu.RUnlock()
			if !reflect.DeepEqual(gotCurrent, wantCurrent) || !reflect.DeepEqual(gotPending, wantPending) {
				t.Fatalf("invalid replacement changed process state: current=%#v pending=%#v", gotCurrent, gotPending)
			}
		})
	}
}

func TestManagerRejectsInvalidDesiredInstancesBeforeStarter(t *testing.T) {
	for _, test := range invalidDesiredInstanceCases(t) {
		t.Run(test.name, func(t *testing.T) {
			var starterCalls atomic.Int32
			manager := NewManager("test", func(context.Context, string, Spec, Callbacks) (Child, error) {
				starterCalls.Add(1)
				return nil, PermanentStart(errors.New("starter must not run"))
			}, Callbacks{})
			t.Cleanup(func() { _ = manager.Close(t.Context()) })
			err := manager.Apply(t.Context(), []Spec{{
				ID: "dev.bsbctl.test", Version: "1", Executable: "/plugin", ProtocolVersion: protocol.Version,
				ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident}, Instances: test.instances,
			}})
			if err == nil {
				t.Fatal("invalid desired instances were accepted")
			}
			if starterCalls.Load() != 0 {
				t.Fatalf("starter calls = %d, want 0", starterCalls.Load())
			}
		})
	}
}

type invalidDesiredInstanceCase struct {
	name      string
	instances []Instance
}

func invalidDesiredInstanceCases(t *testing.T) []invalidDesiredInstanceCase {
	t.Helper()
	scalarConfig := testProcessInstance(8)
	scalarConfig.Config = json.RawMessage(`"value"`)
	arrayConfig := testProcessInstance(8)
	arrayConfig.Config = json.RawMessage(`[]`)
	nullConfig := testProcessInstance(8)
	nullConfig.Config = json.RawMessage(`null`)
	malformedConfig := testProcessInstance(8)
	malformedConfig.Config = json.RawMessage(`{"broken"`)
	oversizedConfig := testProcessInstance(8)
	oversizedConfig.Config = pluginObjectOfSize(t, protocol.MaxJSONObjectBytes+1)
	scalarCheckpoint := testProcessInstance(8)
	scalarCheckpoint.Checkpoint = &CheckpointRestore{Generation: 8, Data: json.RawMessage(`"value"`)}
	arrayCheckpoint := testProcessInstance(8)
	arrayCheckpoint.Checkpoint = &CheckpointRestore{Generation: 8, Data: json.RawMessage(`[]`)}
	nullCheckpoint := testProcessInstance(8)
	nullCheckpoint.Checkpoint = &CheckpointRestore{Generation: 8, Data: json.RawMessage(`null`)}
	malformedCheckpoint := testProcessInstance(8)
	malformedCheckpoint.Checkpoint = &CheckpointRestore{Generation: 8, Data: json.RawMessage(`{"broken"`)}
	oversizedCheckpoint := testProcessInstance(8)
	oversizedCheckpoint.Checkpoint = &CheckpointRestore{Generation: 8, Data: pluginObjectOfSize(t, protocol.MaxJSONObjectBytes+1)}
	emptyID := testProcessInstance(8)
	emptyID.ID = ""
	zeroGeneration := testProcessInstance(0)
	mismatchedCheckpoint := testProcessInstance(8)
	mismatchedCheckpoint.Checkpoint = &CheckpointRestore{Generation: 7, Data: json.RawMessage(`{}`)}
	duplicate := testProcessInstance(9)

	return []invalidDesiredInstanceCase{
		{name: "scalar config", instances: []Instance{scalarConfig}},
		{name: "array config", instances: []Instance{arrayConfig}},
		{name: "null config", instances: []Instance{nullConfig}},
		{name: "malformed config", instances: []Instance{malformedConfig}},
		{name: "oversized config", instances: []Instance{oversizedConfig}},
		{name: "scalar checkpoint", instances: []Instance{scalarCheckpoint}},
		{name: "array checkpoint", instances: []Instance{arrayCheckpoint}},
		{name: "null checkpoint", instances: []Instance{nullCheckpoint}},
		{name: "malformed checkpoint", instances: []Instance{malformedCheckpoint}},
		{name: "oversized checkpoint", instances: []Instance{oversizedCheckpoint}},
		{name: "empty id", instances: []Instance{emptyID}},
		{name: "zero generation", instances: []Instance{zeroGeneration}},
		{name: "mismatched checkpoint generation", instances: []Instance{mismatchedCheckpoint}},
		{name: "duplicate id", instances: []Instance{testProcessInstance(8), duplicate}},
	}
}

func TestStartMapsHostileInitializeErrorToStableCoreError(t *testing.T) {
	process, err := Start(context.Background(), "test", Spec{
		ID: "dev.bsbctl.test", Version: "test", Executable: os.Args[0],
		ProtocolVersion: protocol.Version,
		Args:            []string{"-test.run=^TestHostileInitializePluginHelperProcess$", "-test.bsbctl-hostile-initialize-helper=true"},
		ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
	}, Callbacks{})
	if process != nil {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = process.Stop(ctx)
		})
		t.Fatal("Start returned a process after hostile initialize error")
	}
	if err == nil || err.Error() != "plugin_initialize_failed: plugin initialization failed" {
		t.Fatalf("Start error = %v", err)
	}
	if strings.Contains(err.Error(), hostileRPCSecret) || strings.Contains(err.Error(), "hostile-data-canary") {
		t.Fatalf("Start retained hostile child data: %v", err)
	}
	var printed strings.Builder
	_, _ = fmt.Fprintln(&printed, "bsbctl:", err)
	if strings.Contains(printed.String(), hostileRPCSecret) || strings.Contains(printed.String(), "hostile-data-canary") {
		t.Fatalf("printed daemon-style error retained hostile child data: %s", printed.String())
	}
}

func TestStartRejectsUnknownInitializeResultFields(t *testing.T) {
	process, err := Start(t.Context(), "test", Spec{
		ID: "dev.bsbctl.test", Version: "test", Executable: os.Args[0],
		ProtocolVersion: protocol.Version,
		Args:            []string{"-test.run=^TestUnknownInitializePluginHelperProcess$", "-test.bsbctl-unknown-initialize-helper=true"},
		ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
		Channels:        []protocol.Channel{{ID: "main"}},
		Instances:       []Instance{{ID: "test", Generation: 1, Config: json.RawMessage(`{}`)}},
	}, Callbacks{})
	if process != nil {
		t.Cleanup(func() { _ = process.Stop(t.Context()) })
		t.Fatal("Start accepted an initialize result with an unknown field")
	}
	if err == nil {
		t.Fatal("Start returned no error for an initialize result with an unknown field")
	}
	if err.Error() != "plugin_initialize_failed: plugin initialization failed" {
		t.Fatalf("Start error = %v, want strict initialize rejection", err)
	}
}

func TestInitializeRPCPermanentConfigurationMapsToSupervisorClassification(t *testing.T) {
	err := mapInitializeRPCError(nil, &rpc.Error{
		Code: -32000, Message: hostileRPCSecret,
		Data: json.RawMessage(`{"kind":"invalid_argument"}`),
	})
	if !errors.Is(err, ErrPermanentStart) {
		t.Fatalf("initialize error = %v, want ErrPermanentStart", err)
	}
	if got := err.Error(); got != "plugin cannot start with the current executable and desired state\nplugin_initialize_failed: plugin initialization failed" {
		t.Fatalf("stable initialize error = %q", got)
	}
	if strings.Contains(err.Error(), hostileRPCSecret) || strings.Contains(err.Error(), "hostile-data-canary") {
		t.Fatalf("permanent initialize error retained hostile child data: %v", err)
	}
}
