package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestCloneInstancesCopiesCheckpointRestore(t *testing.T) {
	source := []Instance{{
		ID: "app", Generation: 7,
		Checkpoint: &CheckpointRestore{Generation: 7, Data: json.RawMessage(`{"cursor":"next"}`)},
	}}
	cloned := cloneInstances(source)
	source[0].Checkpoint.Generation = 8
	source[0].Checkpoint.Data[11] = 'X'
	if cloned[0].Checkpoint == nil || cloned[0].Checkpoint.Generation != 7 || string(cloned[0].Checkpoint.Data) != `{"cursor":"next"}` {
		t.Fatalf("cloned checkpoint changed with source: %#v", cloned[0].Checkpoint)
	}
}

func TestManagerRejectsUnknownOrDuplicateExecutionModes(t *testing.T) {
	for name, modes := range map[string][]protocol.ExecutionMode{
		"unknown":   {"network"},
		"duplicate": {protocol.ExecutionModeResident, protocol.ExecutionModeResident},
	} {
		t.Run(name, func(t *testing.T) {
			manager := NewManager("test", (&fakeStarter{}).start, Callbacks{})
			t.Cleanup(func() { _ = manager.Close(t.Context()) })
			err := manager.Apply(t.Context(), []Spec{{
				ID: "monitor", Version: "1", Executable: "/plugin", ProtocolVersion: protocol.Version, ExecutionModes: modes,
			}})
			if err == nil {
				t.Fatalf("execution modes %v were accepted", modes)
			}
		})
	}
}

func TestManagerResidencyReconcileInvokeAndDisable(t *testing.T) {
	t.Parallel()
	starter := &fakeStarter{}
	manager := NewManager("test", starter.start, Callbacks{})
	resident := Spec{
		ID: "monitor", Version: "1", Executable: "/plugin",
		ProtocolVersion: protocol.Version,
		ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeResident},
		Instances:       []Instance{{ID: "one", Generation: 1, Config: json.RawMessage(`{}`)}},
	}
	interactive := Spec{
		ID: "ball8", Version: "1", Executable: "/plugin",
		ProtocolVersion: protocol.Version,
		ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
		Instances:       []Instance{{ID: "two", Generation: 1, Config: json.RawMessage(`{}`)}},
	}

	if err := manager.Apply(context.Background(), []Spec{resident, interactive}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := starter.startedIDs(); !equalStrings(got, []string{"monitor"}) {
		t.Fatalf("started after Apply = %v, want resident only", got)
	}

	resident.Instances[0].Generation = 2
	if err := manager.Apply(context.Background(), []Spec{resident, interactive}); err != nil {
		t.Fatalf("reconcile Apply: %v", err)
	}
	if got := starter.child("monitor").reconciledGeneration(); got != 2 {
		t.Fatalf("reconciled generation = %d, want 2", got)
	}

	request := InvokeRequest{InstanceID: "two", Generation: 1, Action: "ask"}
	if err := manager.Invoke(context.Background(), "ball8", request, InvocationInteractive, SessionToken("session")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := starter.startedIDs(); !equalStrings(got, []string{"monitor", "ball8"}) {
		t.Fatalf("started after Invoke = %v", got)
	}
	if got := starter.child("ball8").invocations(); got != 1 {
		t.Fatalf("ball8 invocations = %d, want 1", got)
	}

	if err := manager.Apply(context.Background(), []Spec{interactive}); err != nil {
		t.Fatalf("disable resident: %v", err)
	}
	if !starter.child("monitor").isStopped() {
		t.Fatal("removed resident plugin was not stopped")
	}
	if statuses := manager.Status(); len(statuses) != 1 || statuses[0].ID != "ball8" || !statuses[0].Running {
		t.Fatalf("Status = %#v", statuses)
	}

	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !starter.child("ball8").isStopped() {
		t.Fatal("Close did not stop interactive child")
	}
}

func TestManagerRejectsInvokeForUndesiredPlugin(t *testing.T) {
	t.Parallel()
	manager := NewManager("test", (&fakeStarter{}).start, Callbacks{})
	if err := manager.Invoke(context.Background(), "missing", InvokeRequest{}, InvocationInteractive, SessionToken("session")); !errors.Is(err, ErrPluginNotConfigured) {
		t.Fatalf("Invoke error = %v, want ErrPluginNotConfigured", err)
	}
}

func TestManagerRunsDeclaredOperationAgainstExactInstance(t *testing.T) {
	starter := &fakeStarter{}
	manager := NewManager("test", starter.start, Callbacks{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	spec := Spec{
		ID: "monitor", Version: "1", Executable: "/plugin",
		ProtocolVersion: protocol.Version,
		ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeResident},
		Operations:      []protocol.OperationDescriptor{{ID: "sessions", Kind: protocol.OperationQuery}},
		Instances:       []Instance{{ID: "one", Generation: 1, Config: json.RawMessage(`{}`)}},
	}
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Operation(context.Background(), "monitor", protocol.OperationRequest{Instance: protocol.InstanceRef{ID: "one", Generation: 1}, Operation: "sessions", Payload: json.RawMessage(`{}`)})
	if err != nil || string(result.Payload) != `{}` {
		t.Fatalf("operation = %s / %v", result.Payload, err)
	}
	if _, err := manager.Operation(context.Background(), "monitor", protocol.OperationRequest{Instance: protocol.InstanceRef{ID: "one"}, Operation: "sessions", Payload: json.RawMessage(`{}`)}); !errors.Is(err, ErrPluginNotConfigured) {
		t.Fatalf("generation-less operation error = %v, want ErrPluginNotConfigured", err)
	}
	if _, err := manager.Operation(context.Background(), "monitor", protocol.OperationRequest{Instance: protocol.InstanceRef{ID: "other"}, Operation: "sessions"}); !errors.Is(err, ErrPluginNotConfigured) {
		t.Fatalf("unknown instance operation error = %v", err)
	}
}

func TestManagerObservesUnexpectedChildExit(t *testing.T) {
	t.Parallel()
	starter := &fakeStarter{}
	withdrawn := make(chan uint64, 1)
	manager := NewManager("test", starter.start, Callbacks{WithdrawGeneration: func(_ string, generation uint64) { withdrawn <- generation }})
	spec := Spec{
		ID: "monitor", Version: "1", Executable: "/plugin",
		ProtocolVersion: protocol.Version,
		ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeResident},
		Instances:       []Instance{{ID: "one", Generation: 7, Config: json.RawMessage(`{}`)}},
	}
	if err := manager.Apply(context.Background(), []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-manager.Changes():
	default:
	}
	starter.child("monitor").crash(errors.New("boom"))
	select {
	case <-manager.Changes():
	case <-time.After(time.Second):
		t.Fatal("manager did not observe child exit")
	}
	statuses := manager.Status()
	if len(statuses) != 1 || statuses[0].Running || statuses[0].LastErrorCode == "" {
		t.Fatalf("Status after crash = %#v", statuses)
	}
	select {
	case generation := <-withdrawn:
		if generation != 7 {
			t.Fatalf("withdrawn generation = %d", generation)
		}
	case <-time.After(time.Second):
		t.Fatal("child observations were not withdrawn")
	}
}

type fakeStarter struct {
	mu       sync.Mutex
	started  []string
	children map[string]*fakeChild
}

func (s *fakeStarter) start(_ context.Context, _ string, spec Spec, _ Callbacks) (Child, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.children == nil {
		s.children = make(map[string]*fakeChild)
	}
	child := &fakeChild{done: make(chan error), instances: spec.Instances}
	s.started = append(s.started, spec.ID)
	s.children[spec.ID] = child
	return child, nil
}

func (s *fakeStarter) startedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.started...)
}

func (s *fakeStarter) child(id string) *fakeChild {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.children[id]
}

type fakeChild struct {
	mu        sync.Mutex
	done      chan error
	instances []Instance
	invoked   int
	stopped   bool
}

func (c *fakeChild) Invoke(context.Context, InvokeRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invoked++
	return nil
}
func (*fakeChild) EndSession(context.Context, EndSessionRequest) error { return nil }
func (c *fakeChild) ReplaceInstances(_ context.Context, instances []Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.instances = instances
	return nil
}
func (c *fakeChild) SessionInput(context.Context, protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, nil
}
func (c *fakeChild) Operation(context.Context, protocol.OperationRequest) (protocol.OperationResult, error) {
	return protocol.OperationResult{Payload: json.RawMessage(`{}`)}, nil
}
func (c *fakeChild) Ping(context.Context) (protocol.HealthResult, error) {
	return protocol.HealthResult{Healthy: true}, nil
}
func (c *fakeChild) Done() <-chan error { return c.done }
func (c *fakeChild) Stop(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stopped {
		c.stopped = true
		close(c.done)
	}
	return nil
}
func (c *fakeChild) reconciledGeneration() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.instances[0].Generation
}
func (c *fakeChild) invocations() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.invoked
}
func (c *fakeChild) isStopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

func (c *fakeChild) crash(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	c.stopped = true
	c.done <- err
	close(c.done)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
