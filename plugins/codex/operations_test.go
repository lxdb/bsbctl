package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/plugins/codex/internal/appserver"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestCodexOperationsListRedactedSessionsAndPersistPinnedFocus(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	startedAt := now.Unix()
	client := newFakeAppServerClient()
	host := &handlerRecordingHost{
		observations: make(chan protocol.Observation, 32), completed: make(chan protocol.CompleteSessionRequest, 2),
		checkpoints: make(chan protocol.CheckpointRequest, 2),
	}
	handler := newHandler(host, func(string, bool) appServerClient { return client }, func() (string, error) { return "/Users/test", nil }, func() time.Time { return now })
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: "codex-main", Generation: 7, Config: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
	<-client.started
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerConnected}
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe title", CWD: "/private/hidden/project", Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-1", Status: "inProgress", StartedAt: &startedAt},
	}}
	awaitHandlerObservation(t, host, func(value protocol.Observation) bool { return value.ReasonCode == "codex_running" })
	ref := protocol.InstanceRef{ID: "codex-main", Generation: 7}
	listed, err := handler.InvokeOperation(context.Background(), protocol.OperationRequest{Instance: ref, Operation: OperationSessions, Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(listed.Payload), "/private/") || !strings.Contains(string(listed.Payload), `"thread_id":"thread-1"`) || !strings.Contains(string(listed.Payload), `"title":"Safe title"`) {
		t.Fatalf("session listing = %s", listed.Payload)
	}
	pinned, err := handler.InvokeOperation(context.Background(), protocol.OperationRequest{Instance: ref, Operation: OperationPin, Payload: json.RawMessage(`{"thread_id":"thread-1"}`)})
	if err != nil || string(pinned.Payload) != `{"pinned_thread_id":"thread-1"}` {
		t.Fatalf("pin = %s / %v", pinned.Payload, err)
	}
	checkpoint := <-host.checkpoints
	if checkpoint.Instance != ref || string(checkpoint.Data) != `{"schema_version":1,"pinned_thread_id":"thread-1"}` {
		t.Fatalf("pin checkpoint = %#v", checkpoint)
	}
	awaitHandlerObservation(t, host, func(value protocol.Observation) bool { return value.ReasonCode == "codex_thread_pinned" })
	unpinned, err := handler.InvokeOperation(context.Background(), protocol.OperationRequest{Instance: ref, Operation: OperationUnpin, Payload: json.RawMessage(`{}`)})
	if err != nil || string(unpinned.Payload) != `{"pinned_thread_id":null}` {
		t.Fatalf("unpin = %s / %v", unpinned.Payload, err)
	}
	if checkpoint := <-host.checkpoints; string(checkpoint.Data) != `{"schema_version":1}` {
		t.Fatalf("unpin checkpoint = %s", checkpoint.Data)
	}
}

func TestCodexPinRollsBackWhenCheckpointIsNotPersisted(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	client := newFakeAppServerClient()
	host := &handlerRecordingHost{
		observations: make(chan protocol.Observation, 32), completed: make(chan protocol.CompleteSessionRequest, 2),
		checkpoints: make(chan protocol.CheckpointRequest, 1), checkpointErr: errors.New("checkpoint unavailable"),
	}
	handler := newHandler(host, func(string, bool) appServerClient { return client }, func() (string, error) { return "/Users/test", nil }, func() time.Time { return now })
	if err := handler.ReplaceInstances(t.Context(), []protocol.Instance{{ID: "codex-main", Generation: 7, Config: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
	<-client.started
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerConnected}
	startedAt := now.Unix()
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe title", Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-1", Status: "inProgress", StartedAt: &startedAt},
	}}
	awaitHandlerObservation(t, host, func(value protocol.Observation) bool { return value.ReasonCode == "codex_running" })

	ref := protocol.InstanceRef{ID: "codex-main", Generation: 7}
	_, err := handler.InvokeOperation(t.Context(), protocol.OperationRequest{
		Instance: ref, Operation: OperationPin, Payload: json.RawMessage(`{"thread_id":"thread-1"}`),
	})
	if err == nil {
		t.Fatal("pin succeeded without a durable checkpoint")
	}
	if got := handler.worker.reducer.PinnedThread(); got != "" {
		t.Fatalf("unpersisted pin remained active: %q", got)
	}
}

func TestCodexCheckpointFailureCannotRestorePinAcrossThreadReconciliation(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	client := newFakeAppServerClient()
	host := &handlerRecordingHost{
		observations: make(chan protocol.Observation, 64), completed: make(chan protocol.CompleteSessionRequest, 2),
		checkpoints: make(chan protocol.CheckpointRequest, 4),
	}
	handler := newHandler(host, func(string, bool) appServerClient { return client }, func() (string, error) { return "/Users/test", nil }, func() time.Time { return now })
	if err := handler.ReplaceInstances(t.Context(), []protocol.Instance{{ID: "codex-main", Generation: 7, Config: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
	<-client.started
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerConnected}
	startedAt := now.Unix()
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-0", Name: "Safe title", Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-thread-0", Status: "inProgress", StartedAt: &startedAt},
	}}
	awaitHandlerObservation(t, host, func(value protocol.Observation) bool { return value.ReasonCode == "codex_running" })
	ref := protocol.InstanceRef{ID: "codex-main", Generation: 7}
	if _, err := handler.InvokeOperation(t.Context(), protocol.OperationRequest{
		Instance: ref, Operation: OperationPin, Payload: json.RawMessage(`{"thread_id":"thread-0"}`),
	}); err != nil {
		t.Fatal(err)
	}
	<-host.checkpoints
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe title", Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-thread-1", Status: "inProgress", StartedAt: &startedAt},
	}}
	deadline := time.Now().Add(time.Second)
	for {
		listed, err := handler.InvokeOperation(t.Context(), protocol.OperationRequest{
			Instance: ref, Operation: OperationSessions, Payload: json.RawMessage(`{}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(listed.Payload), `"thread_id":"thread-1"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("thread-1 was not reconciled")
		}
		time.Sleep(time.Millisecond)
	}

	host.checkpointErr = errors.New("checkpoint unavailable")
	host.checkpointStarted = make(chan struct{}, 1)
	host.checkpointRelease = make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := handler.InvokeOperation(t.Context(), protocol.OperationRequest{
			Instance: ref, Operation: OperationPin, Payload: json.RawMessage(`{"thread_id":"thread-1"}`),
		})
		result <- err
	}()
	select {
	case <-host.checkpointStarted:
	case err := <-result:
		close(host.checkpointRelease)
		t.Fatalf("pin returned before checkpoint persistence: %v", err)
	case <-time.After(time.Second):
		close(host.checkpointRelease)
		t.Fatal("pin did not start checkpoint persistence")
	}
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerThreadsReconciled, ThreadIDs: []string{}}
	close(host.checkpointRelease)
	if err := <-result; err == nil {
		t.Fatal("pin succeeded without a durable checkpoint")
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		handler.worker.stateMu.Lock()
		pinned := handler.worker.reducer.PinnedThread()
		handler.worker.stateMu.Unlock()
		if pinned == "" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("checkpoint rollback restored a pin removed by thread reconciliation")
}

func TestPinnedCheckpointRejectsUnsafeThreadIdentifiers(t *testing.T) {
	for _, checkpoint := range []json.RawMessage{
		json.RawMessage(`{"schema_version":1,"pinned_thread_id":" spaced "}`),
		json.RawMessage(`{"schema_version":1,"pinned_thread_id":"` + strings.Repeat("x", 129) + `"}`),
	} {
		if got := decodePinnedCheckpoint(checkpoint); got != "" {
			t.Fatalf("unsafe checkpoint restored %q", got)
		}
	}
}

func TestPinnedCheckpointRequiresCurrentSchema(t *testing.T) {
	for _, checkpoint := range []json.RawMessage{
		json.RawMessage(`{"schema_version":1,"pinned_thread_id":"thread-1"}`),
	} {
		if got := decodePinnedCheckpoint(checkpoint); got != "thread-1" {
			t.Fatalf("checkpoint %s restored %q", checkpoint, got)
		}
	}
	for _, schema := range []string{"", `"schema_version":0,`, `"schema_version":2,`} {
		if got := decodePinnedCheckpoint(json.RawMessage(`{` + schema + `"pinned_thread_id":"thread-1"}`)); got != "" {
			t.Fatalf("unsupported checkpoint schema restored %q", got)
		}
	}
}
