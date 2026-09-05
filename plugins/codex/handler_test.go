package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/plugins/codex/internal/appserver"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/busylib-go/proto/inputpb"
)

type fakeAppServerClient struct {
	in           chan appserver.ManagerEvent
	started      chan struct{}
	stopped      chan struct{}
	responses    chan fakeAppServerResponse
	interrupts   chan [2]string
	respondErr   error
	interruptErr error
}

type fakeAppServerResponse struct {
	id     appserver.RawID
	result any
}

func newFakeAppServerClient() *fakeAppServerClient {
	return &fakeAppServerClient{
		in: make(chan appserver.ManagerEvent, 8), started: make(chan struct{}), stopped: make(chan struct{}),
		responses: make(chan fakeAppServerResponse, 8), interrupts: make(chan [2]string, 8),
	}
}

func (c *fakeAppServerClient) Run(ctx context.Context, out chan<- appserver.ManagerEvent) error {
	close(c.started)
	defer close(c.stopped)
	for {
		select {
		case event := <-c.in:
			select {
			case out <- event:
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *fakeAppServerClient) Respond(_ context.Context, id appserver.RawID, result any) error {
	if c.respondErr != nil {
		return c.respondErr
	}
	c.responses <- fakeAppServerResponse{id: id, result: result}
	return nil
}
func (c *fakeAppServerClient) Interrupt(_ context.Context, _ appserver.Connection, threadID, turnID string) error {
	if c.interruptErr != nil {
		return c.interruptErr
	}
	c.interrupts <- [2]string{threadID, turnID}
	return nil
}

type handlerRecordingHost struct {
	observations      chan protocol.Observation
	mu                sync.Mutex
	logs              []protocol.LogNotification
	completed         chan protocol.CompleteSessionRequest
	checkpoints       chan protocol.CheckpointRequest
	checkpointErr     error
	checkpointStarted chan struct{}
	checkpointRelease chan struct{}
	executions        chan protocol.SessionExecutionRequest
	executionErr      error
	executionRelease  <-chan struct{}
}

func (h *handlerRecordingHost) BeginSessionExecution(ctx context.Context, request protocol.SessionExecutionRequest) error {
	if h.executions != nil {
		h.executions <- request
	}
	if h.executionRelease != nil {
		select {
		case <-h.executionRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return h.executionErr
}

func (h *handlerRecordingHost) CompleteSession(_ context.Context, request protocol.CompleteSessionRequest) error {
	h.completed <- request
	return nil
}

func (h *handlerRecordingHost) PublishObservation(_ context.Context, observation protocol.Observation) error {
	h.observations <- observation
	return nil
}

func (h *handlerRecordingHost) Log(_ context.Context, event protocol.LogNotification) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, event)
	return nil
}

func (h *handlerRecordingHost) SaveCheckpoint(_ context.Context, request protocol.CheckpointRequest) error {
	if h.checkpoints != nil {
		h.checkpoints <- request
	}
	if h.checkpointStarted != nil {
		h.checkpointStarted <- struct{}{}
	}
	if h.checkpointRelease != nil {
		<-h.checkpointRelease
	}
	return h.checkpointErr
}

func TestDefinitionDeclaresResidentInteractiveCodexChannels(t *testing.T) {
	t.Parallel()
	definition := DefinitionForVersion(PluginVersion)
	if definition.ID != PluginID || definition.Version != PluginVersion {
		t.Fatalf("identity = %q/%q", definition.ID, definition.Version)
	}
	if protocol.Version != "1.0" {
		t.Fatalf("protocol version = %q", protocol.Version)
	}
	if len(definition.Contract.ExecutionModes) != 2 || definition.Contract.ExecutionModes[0] != protocol.ExecutionModeResident || definition.Contract.ExecutionModes[1] != protocol.ExecutionModeInteractive {
		t.Fatalf("execution modes = %v", definition.Contract.ExecutionModes)
	}
	want := []string{
		ChannelAttention, "guidance", ChannelOutcome, ChannelActivity, ChannelProgress, ChannelOverview, ChannelConnection, ChannelDetail,
		ChannelQuotaSummary, ChannelQuotaPressure,
	}
	if len(definition.Contract.Channels) != len(want) {
		t.Fatalf("channels = %v", definition.Contract.Channels)
	}
	for index, channel := range definition.Contract.Channels {
		if channel.ID != want[index] {
			t.Fatalf("channel %d = %q, want %q", index, channel.ID, want[index])
		}
	}
	if len(definition.Contract.Operations) != 3 || definition.Contract.Operations[0].ID != OperationSessions || definition.Contract.Operations[1].ID != OperationPin || definition.Contract.Operations[2].ID != OperationUnpin {
		t.Fatalf("operations = %#v", definition.Contract.Operations)
	}
}

func TestHandlerUsesStartButtonAndExactSessionTokenForConfirmedApproval(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	client := newFakeAppServerClient()
	host := &handlerRecordingHost{
		observations: make(chan protocol.Observation, 32), completed: make(chan protocol.CompleteSessionRequest, 2),
		executions: make(chan protocol.SessionExecutionRequest, 2),
	}
	handler := newHandler(host, func(string, bool) appServerClient { return client }, func() (string, error) { return "/Users/test", nil }, func() time.Time { return now })
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: "codex-main", Generation: 1, Config: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
	<-client.started
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerConnected}
	id, _ := appserver.ParseRawID(json.RawMessage(`"approval-1"`))
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: id, Method: "item/fileChange/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1}`),
	}}
	wait := awaitHandlerObservation(t, host, func(value protocol.Observation) bool { return value.ReasonCode == "codex_wait_file" })
	request := protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, Action: "open", SessionToken: "interactive-7",
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{Channel: wait.Channel, Key: wait.Key, Revision: wait.Revision}},
	}
	if err := handler.StartSession(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	awaitHandlerObservation(t, host, func(value protocol.Observation) bool {
		return value.Channel == ChannelDetail && value.Disposition != protocol.DispositionResolved
	})
	for count := 0; count < 2; count++ {
		result, err := handler.HandleSessionInput(context.Background(), sessionInputRequest(t, "codex-main", "stale-token", buttonInputEvent(inputpb.Button_OK)))
		if err != nil {
			t.Fatal(err)
		}
		if result.Disposition != protocol.SessionInputNotConsumed {
			t.Fatalf("stale input result = %#v", result)
		}
	}
	select {
	case response := <-client.responses:
		t.Fatalf("stale token produced response %#v", response)
	default:
	}
	if _, err := handler.HandleSessionInput(context.Background(), sessionInputRequest(t, "codex-main", "interactive-7", encoderInputEvent(1))); err != nil {
		t.Fatal(err)
	}
	for count := 0; count < 2; count++ {
		if _, err := handler.HandleSessionInput(context.Background(), sessionInputRequest(t, "codex-main", "interactive-7", buttonInputEvent(inputpb.Button_START))); err != nil {
			t.Fatal(err)
		}
	}
	if execution := <-host.executions; execution != (protocol.SessionExecutionRequest{Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, SessionToken: "interactive-7"}) {
		t.Fatalf("execution grant = %#v", execution)
	}
	response := <-client.responses
	encoded, _ := json.Marshal(response.result)
	if string(encoded) != `{"decision":"decline"}` {
		t.Fatalf("approval response = %s", encoded)
	}
	if completed := <-host.completed; completed != (protocol.CompleteSessionRequest{Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, SessionToken: "interactive-7"}) {
		t.Fatalf("completed session = %#v", completed)
	}
	if _, err := handler.HandleSessionInput(context.Background(), sessionInputRequest(t, "codex-main", "stale-token", buttonInputEvent(inputpb.Button_OK))); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-client.responses:
		t.Fatalf("stale token produced response %#v", response)
	default:
	}
}

func TestWorkerExecutionDenialPreventsCodexEffect(t *testing.T) {
	now := time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC)
	client := newFakeAppServerClient()
	want := errors.New("execution denied")
	host := &handlerRecordingHost{
		completed: make(chan protocol.CompleteSessionRequest, 1), executions: make(chan protocol.SessionExecutionRequest, 1),
		executionErr: want,
	}
	id, parseErr := appserver.ParseRawID(json.RawMessage(`"approval-1"`))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	session := &interactionSession{
		token: "interactive-7", staged: true, actions: []string{"decline"},
		request: &pendingRequest{ID: id, Kind: requestFile, Interactive: true},
	}
	worker := &codexWorker{
		instanceID: "codex-main", generation: 1, host: host, client: client,
		owner: &Handler{now: func() time.Time { return now }}, session: session,
	}
	_, err := worker.handleInput(t.Context(), session.token, &protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonStart, Action: protocol.ButtonPress}})
	if !errors.Is(err, want) {
		t.Fatalf("handleInput error = %v, want execution denial", err)
	}
	if execution := <-host.executions; execution != (protocol.SessionExecutionRequest{Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, SessionToken: session.token}) {
		t.Fatalf("execution grant = %#v", execution)
	}
	select {
	case response := <-client.responses:
		t.Fatalf("denied execution produced response %#v", response)
	default:
	}
	select {
	case completed := <-host.completed:
		t.Fatalf("denied execution completed session %#v", completed)
	default:
	}
	if session.processing {
		t.Fatal("execution denial left the session permanently processing")
	}
}

func TestWorkerRevalidatesProviderTargetAfterExecutionGrant(t *testing.T) {
	for _, disconnected := range []bool{true, false} {
		t.Run(map[bool]string{true: "disconnected", false: "request resolved"}[disconnected], func(t *testing.T) {
			client := newFakeAppServerClient()
			release := make(chan struct{})
			host := &handlerRecordingHost{completed: make(chan protocol.CompleteSessionRequest, 1), executions: make(chan protocol.SessionExecutionRequest, 1), executionRelease: release}
			id, err := appserver.ParseRawID(json.RawMessage(`"approval-1"`))
			if err != nil {
				t.Fatal(err)
			}
			session := &interactionSession{token: "exact-session", staged: true, actions: []string{"decline"}, requestKey: "approval-1", request: &pendingRequest{Key: "approval-1", ID: id, Kind: requestFile, Interactive: true}}
			reducer := NewReducer(time.Now)
			reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
			reducer.pending[session.requestKey] = session.request
			worker := &codexWorker{instanceID: "app", generation: 1, host: host, client: client, owner: &Handler{now: time.Now}, session: session, reducer: reducer, publisher: newCardPublisher(host, protocol.InstanceRef{ID: "app", Generation: 1}, time.Now)}
			done := make(chan error, 1)
			go func() {
				_, err := worker.handleInput(t.Context(), session.token, &protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonStart, Action: protocol.ButtonPress}})
				done <- err
			}()
			<-host.executions
			worker.stateMu.Lock()
			if disconnected {
				reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerDisconnected})
			} else {
				delete(reducer.pending, session.requestKey)
			}
			worker.stateMu.Unlock()
			close(release)
			if err := <-done; err == nil {
				t.Fatal("retired provider target was accepted after the grant")
			}
			select {
			case response := <-client.responses:
				t.Fatalf("retired target caused an external response: %#v", response)
			default:
			}
			select {
			case completion := <-host.completed:
				if completion.SessionToken != session.token {
					t.Fatalf("completed another session: %#v", completion)
				}
			default:
				t.Fatal("retired post-grant target left core executing")
			}
		})
	}
}

func TestWorkerClosesGrantedSessionWhenCodexEffectFails(t *testing.T) {
	now := time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC)
	client := newFakeAppServerClient()
	client.respondErr = errors.New("app-server unavailable")
	host := &handlerRecordingHost{
		completed: make(chan protocol.CompleteSessionRequest, 1), executions: make(chan protocol.SessionExecutionRequest, 1),
	}
	id, parseErr := appserver.ParseRawID(json.RawMessage(`"approval-1"`))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	session := &interactionSession{
		token: "interactive-7", staged: true, actions: []string{"decline"},
		requestKey: "approval-1",
		request:    &pendingRequest{Key: "approval-1", ID: id, Kind: requestFile, Interactive: true},
	}
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.pending[session.requestKey] = session.request
	worker := &codexWorker{
		instanceID: "codex-main", generation: 1, host: host, client: client,
		owner: &Handler{now: func() time.Time { return now }}, session: session, reducer: reducer,
		publisher: newCardPublisher(host, protocol.InstanceRef{ID: "codex-main", Generation: 1}, func() time.Time { return now }),
	}
	_, err := worker.handleInput(t.Context(), session.token, &protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonStart, Action: protocol.ButtonPress}})
	if err == nil || err.Error() != "Codex app-server action failed" {
		t.Fatalf("handleInput error = %v", err)
	}
	if execution := <-host.executions; execution.SessionToken != session.token {
		t.Fatalf("execution grant = %#v", execution)
	}
	select {
	case completion := <-host.completed:
		if completion.SessionToken != session.token {
			t.Fatalf("completion = %#v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("failed post-grant action left the session executing")
	}
	if worker.session != nil {
		t.Fatal("failed post-grant action retained the local session")
	}
	select {
	case response := <-client.responses:
		t.Fatalf("failed response was recorded as delivered: %#v", response)
	default:
	}
}

func TestHandlerSupportsRealCodexAskChoicesAndHandoff(t *testing.T) {
	for _, handoff := range []bool{false, true} {
		t.Run(fmt.Sprintf("handoff=%v", handoff), func(t *testing.T) {
			client := newFakeAppServerClient()
			host := &handlerRecordingHost{observations: make(chan protocol.Observation, 32), completed: make(chan protocol.CompleteSessionRequest, 1), executions: make(chan protocol.SessionExecutionRequest, 1)}
			handler := newHandler(host, func(string, bool) appServerClient { return client }, func() (string, error) { return "/Users/test", nil }, time.Now)
			if err := handler.ReplaceInstances(t.Context(), []protocol.Instance{{ID: "codex-main", Generation: 1, Config: []byte(`{}`)}}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
			<-client.started
			client.in <- appserver.ManagerEvent{Kind: appserver.ManagerConnected}
			id, _ := appserver.ParseRawID(json.RawMessage(`"real-ask"`))
			client.in <- appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
				Kind: appserver.IncomingServerRequest, ID: id, Method: "item/tool/requestUserInput",
				Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","isBlocking":false,"questions":[{"id":"drink","header":"Drink","question":"Which drink?","isOther":true,"isSecret":false,"options":[{"label":"Coffee"},{"label":"Tea"}]}]}`),
			}}
			ask := awaitHandlerObservation(t, host, func(v protocol.Observation) bool { return v.ReasonCode == "codex_wait_question" })
			if ask.Channel != ChannelAttention || ask.Disposition != protocol.DispositionActionable {
				t.Fatalf("real ASK was not actionable: %s/%s", ask.Channel, ask.Disposition)
			}
			if err := handler.StartSession(t.Context(), protocol.SessionStartRequest{Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, Action: "open", SessionToken: "ask-session", Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{Channel: ask.Channel, Key: ask.Key, Revision: ask.Revision}}}); err != nil {
				t.Fatal(err)
			}
			awaitHandlerObservation(t, host, func(v protocol.Observation) bool {
				return v.Channel == ChannelDetail && v.Disposition != protocol.DispositionResolved
			})
			delta := 1
			if handoff {
				delta = 2
			}
			if _, err := handler.HandleSessionInput(t.Context(), sessionInputRequest(t, "codex-main", "ask-session", encoderInputEvent(int32(delta)))); err != nil {
				t.Fatal(err)
			}
			detail := awaitHandlerObservation(t, host, func(v protocol.Observation) bool {
				return v.Channel == ChannelDetail && v.Disposition != protocol.DispositionResolved
			})
			wantLabel := "Tea"
			if handoff {
				wantLabel = "Answer in Codex"
			}
			if got := cardElement(t, *detail.Scene, "back-option-label").Text.Value; got != wantLabel {
				t.Fatalf("selected option = %q, want %q", got, wantLabel)
			}
			if _, err := handler.HandleSessionInput(t.Context(), sessionInputRequest(t, "codex-main", "ask-session", buttonInputEvent(inputpb.Button_OK))); err != nil {
				t.Fatal(err)
			}
			if completed := awaitHandlerCompletion(t, host); completed.SessionToken != "ask-session" {
				t.Fatalf("wrong completed session: %#v", completed)
			}
			if handoff {
				select {
				case interruption := <-client.interrupts:
					t.Fatalf("handoff interrupted the turn: %#v", interruption)
				default:
				}
				select {
				case response := <-client.responses:
					t.Fatalf("handoff submitted an answer: %#v", response)
				default:
				}
				select {
				case execution := <-host.executions:
					t.Fatalf("handoff requested execution: %#v", execution)
				default:
				}
				handler.worker.stateMu.Lock()
				_, pending := handler.worker.reducer.PendingRequest(ask.Key)
				handler.worker.stateMu.Unlock()
				if !pending {
					t.Fatal("handoff removed the unanswered request")
				}
			} else {
				select {
				case response := <-client.responses:
					encoded, _ := json.Marshal(response.result)
					if !response.id.Equal(id) || string(encoded) != `{"answers":{"drink":{"answers":["Tea"]}}}` {
						t.Fatalf("wrong explicit answer: %s", encoded)
					}
				default:
					t.Fatal("explicit choice was not submitted")
				}
			}
		})
	}
}

func TestHandlerClosesTypedQuestionWhenThreadAdvancesToDifferentTurn(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	client := newFakeAppServerClient()
	host := &handlerRecordingHost{observations: make(chan protocol.Observation, 32), completed: make(chan protocol.CompleteSessionRequest, 1)}
	handler := newHandler(host, func(string, bool) appServerClient { return client }, func() (string, error) { return "/Users/test", nil }, func() time.Time { return now })
	if err := handler.ReplaceInstances(t.Context(), []protocol.Instance{{ID: "codex-main", Generation: 1, Config: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(t.Context()) })
	<-client.started
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerConnected}
	id, _ := appserver.ParseRawID(json.RawMessage(`"question-1"`))
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: id, Method: "item/tool/requestUserInput",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","isBlocking":true,"questions":[{"id":"choice","header":"Choice","question":"Choose","options":[{"label":"A","description":"First"}]}]}`),
	}}
	wait := awaitHandlerObservation(t, host, func(value protocol.Observation) bool { return value.ReasonCode == "codex_wait_question" })
	if err := handler.StartSession(t.Context(), protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, Action: "open", SessionToken: "question-session",
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{
			Channel: wait.Channel, Key: wait.Key, Revision: wait.Revision,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	awaitHandlerObservation(t, host, func(value protocol.Observation) bool {
		return value.Channel == ChannelDetail && value.Disposition != protocol.DispositionResolved
	})

	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-2","status":"inProgress"}}`),
	}}
	if completed := awaitHandlerCompletion(t, host); completed != (protocol.CompleteSessionRequest{Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, SessionToken: "question-session"}) {
		t.Fatalf("completed session = %#v", completed)
	}
	if _, err := handler.HandleSessionInput(t.Context(), sessionInputRequest(t, "codex-main", "question-session", buttonInputEvent(inputpb.Button_OK))); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-client.responses:
		t.Fatalf("stale typed question produced response %#v", response)
	default:
	}
}

func TestHandlerClosesApprovalWhenPendingRequestIdentityChangesUnderSameKey(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	client := newFakeAppServerClient()
	host := &handlerRecordingHost{observations: make(chan protocol.Observation, 32), completed: make(chan protocol.CompleteSessionRequest, 1)}
	handler := newHandler(host, func(string, bool) appServerClient { return client }, func() (string, error) { return "/Users/test", nil }, func() time.Time { return now })
	if err := handler.ReplaceInstances(t.Context(), []protocol.Instance{{ID: "codex-main", Generation: 1, Config: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(t.Context()) })
	<-client.started
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerConnected}
	id, _ := appserver.ParseRawID(json.RawMessage(`"approval-1"`))
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: id, Method: "item/fileChange/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1}`),
	}}
	wait := awaitHandlerObservation(t, host, func(value protocol.Observation) bool { return value.ReasonCode == "codex_wait_file" })
	if err := handler.StartSession(t.Context(), protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, Action: "open", SessionToken: "approval-session",
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{
			Channel: wait.Channel, Key: wait.Key, Revision: wait.Revision,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	awaitHandlerObservation(t, host, func(value protocol.Observation) bool {
		return value.Channel == ChannelDetail && value.Disposition != protocol.DispositionResolved
	})

	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: id, Method: "item/fileChange/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-2","turnId":"turn-2","itemId":"item-2","startedAtMs":2}`),
	}}
	if completed := awaitHandlerCompletion(t, host); completed != (protocol.CompleteSessionRequest{Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, SessionToken: "approval-session"}) {
		t.Fatalf("completed session = %#v", completed)
	}
	for range 2 {
		if _, err := handler.HandleSessionInput(t.Context(), sessionInputRequest(t, "codex-main", "approval-session", buttonInputEvent(inputpb.Button_OK))); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case response := <-client.responses:
		t.Fatalf("stale approval produced response %#v", response)
	default:
	}
}

func TestHandlerLauncherOpensLiveReadOnlySessionThatSurvivesReconnect(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	client := newFakeAppServerClient()
	host := &handlerRecordingHost{observations: make(chan protocol.Observation, 32), completed: make(chan protocol.CompleteSessionRequest, 1)}
	handler := newHandler(host, func(string, bool) appServerClient { return client }, func() (string, error) { return "/Users/test", nil }, func() time.Time { return now })
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: "codex-main", Generation: 1, Config: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
	<-client.started
	_ = nextHandlerObservation(t, host)

	request := protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, Action: "open", SessionToken: "launcher-1",
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher},
	}
	if err := handler.StartSession(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	detail := awaitHandlerObservation(t, host, func(value protocol.Observation) bool {
		return value.Channel == ChannelDetail && value.Disposition != protocol.DispositionResolved
	})
	if cardElement(t, detail.Scene, "front-state").Text.Value != "CODEX ..." || cardElement(t, detail.Scene, "back-context").Text.Value != "App server" || detail.Disposition != protocol.DispositionSnapshot {
		t.Fatalf("launcher detail = %#v", detail)
	}
	for _, input := range []*inputpb.InputEvent{buttonInputEvent(inputpb.Button_OK), buttonInputEvent(inputpb.Button_START), encoderInputEvent(1)} {
		if _, err := handler.HandleSessionInput(t.Context(), sessionInputRequest(t, "codex-main", "launcher-1", input)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case completed := <-host.completed:
		t.Fatalf("read-only input completed launcher session: %#v", completed)
	default:
	}

	startedAt := now.Add(-time.Minute).Unix()
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerConnected}
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-live", Name: "Live session", CWD: "/safe/live-project", Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-live", Status: "inProgress", StartedAt: &startedAt},
	}}
	live := awaitHandlerObservation(t, host, func(value protocol.Observation) bool {
		return value.Channel == ChannelDetail && value.Disposition != protocol.DispositionResolved && cardElement(t, value.Scene, "front-state").Text.Value == "RUN"
	})
	if cardElement(t, live.Scene, "back-session").Text.Value != "Live session" || cardElement(t, live.Scene, "back-workdir").Text.Value != "live-project" {
		t.Fatalf("live launcher detail = %#v", live)
	}

	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerDisconnected, FailureStage: "read", FailureCode: "closed"}
	disconnected := awaitHandlerObservation(t, host, func(value protocol.Observation) bool {
		return value.Channel == ChannelDetail && value.Disposition != protocol.DispositionResolved && cardElement(t, value.Scene, "front-state").Text.Value == "CODEX ..."
	})
	if cardElement(t, disconnected.Scene, "back-detail").Text.Value != "Display only" {
		t.Fatalf("disconnected detail = %#v", disconnected)
	}
	select {
	case completed := <-host.completed:
		t.Fatalf("disconnect completed launcher session: %#v", completed)
	default:
	}

	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerConnected}
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-live", Name: "Live session", CWD: "/safe/live-project", Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-live", Status: "inProgress", StartedAt: &startedAt},
	}}
	_ = awaitHandlerObservation(t, host, func(value protocol.Observation) bool {
		return value.Channel == ChannelDetail && value.Disposition != protocol.DispositionResolved && cardElement(t, value.Scene, "front-state").Text.Value == "RUN"
	})
	result, err := handler.HandleSessionInput(context.Background(), sessionInputRequest(t, "codex-main", "launcher-1", buttonInputEvent(inputpb.Button_BACK)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != protocol.SessionInputConsumed {
		t.Fatalf("Back result = %#v", result)
	}
	if completed := <-host.completed; completed != (protocol.CompleteSessionRequest{Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, SessionToken: "launcher-1"}) {
		t.Fatalf("completed session = %#v", completed)
	}
}

func TestHandlerRejectsStatusOnlyWaitingOnUserInputActivation(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	client := newFakeAppServerClient()
	host := &handlerRecordingHost{observations: make(chan protocol.Observation, 16), completed: make(chan protocol.CompleteSessionRequest, 1)}
	handler := newHandler(host, func(string, bool) appServerClient { return client }, func() (string, error) { return "/Users/test", nil }, func() time.Time { return now })
	if err := handler.ReplaceInstances(t.Context(), []protocol.Instance{{ID: "codex-main", Generation: 1, Config: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(t.Context()) })
	<-client.started
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerConnected}
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe session", CWD: "/private/Safe project",
		Status: appserver.ThreadStatus{Type: "active", ActiveFlags: []string{"waitingOnUserInput"}},
	}}
	statusOnly := awaitHandlerObservation(t, host, func(value protocol.Observation) bool { return value.ReasonCode == "codex_waiting_input" })
	if statusOnly.Channel != "guidance" || statusOnly.Disposition != protocol.DispositionNotable {
		t.Fatalf("status-only guidance = %#v", statusOnly)
	}
	err := handler.StartSession(t.Context(), protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, Action: "open", SessionToken: "interactive-status",
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{
			Channel: statusOnly.Channel, Key: statusOnly.Key, Revision: statusOnly.Revision,
		}},
	})
	if err == nil {
		t.Fatal("status-only waitingOnUserInput activation was accepted")
	}
	handler.worker.stateMu.Lock()
	session := handler.worker.session
	handler.worker.stateMu.Unlock()
	if session != nil {
		t.Fatal("status-only waitingOnUserInput created a foreground session")
	}
}

func TestHandlerRejectsUnsupportedTypedQuestionGuidanceActivation(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	client := newFakeAppServerClient()
	host := &handlerRecordingHost{observations: make(chan protocol.Observation, 16), completed: make(chan protocol.CompleteSessionRequest, 1)}
	handler := newHandler(host, func(string, bool) appServerClient { return client }, func() (string, error) { return "/Users/test", nil }, func() time.Time { return now })
	if err := handler.ReplaceInstances(t.Context(), []protocol.Instance{{ID: "codex-main", Generation: 1, Config: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(t.Context()) })
	<-client.started
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerConnected}
	id, _ := appserver.ParseRawID(json.RawMessage(`"question-1"`))
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: id, Method: "item/tool/requestUserInput",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","isBlocking":true,"questions":[{"id":"choice","header":"Choice","question":"Choose","isSecret":true,"isOther":false,"options":[{"label":"A","description":"First"}]}]}`),
	}}
	guidance := awaitHandlerObservation(t, host, func(value protocol.Observation) bool { return value.ReasonCode == "codex_wait_question" })
	if guidance.Channel != ChannelGuidance || guidance.Disposition != protocol.DispositionNotable {
		t.Fatalf("unsupported typed question guidance = %#v", guidance)
	}
	err := handler.StartSession(t.Context(), protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, Action: "open", SessionToken: "interactive-guidance",
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{
			Channel: guidance.Channel, Key: guidance.Key, Revision: guidance.Revision,
		}},
	})
	if err == nil {
		t.Fatal("unsupported typed question guidance activation was accepted")
	}
	handler.worker.stateMu.Lock()
	session := handler.worker.session
	handler.worker.stateMu.Unlock()
	if session != nil {
		t.Fatal("unsupported typed question guidance created a foreground session")
	}
}

func TestHandlerOpensExactFailureObservationAsDisplayOnly(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	client := newFakeAppServerClient()
	host := &handlerRecordingHost{observations: make(chan protocol.Observation, 16), completed: make(chan protocol.CompleteSessionRequest, 1)}
	handler := newHandler(host, func(string, bool) appServerClient { return client }, func() (string, error) { return "/Users/test", nil }, func() time.Time { return now })
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: "codex-main", Generation: 1, Config: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
	<-client.started
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerConnected}
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe title", Status: appserver.ThreadStatus{Type: "systemError"},
	}}
	failure := awaitHandlerObservation(t, host, func(value protocol.Observation) bool { return value.ReasonCode == "codex_system_error" })
	request := protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: "codex-main", Generation: 1}, Action: "open", SessionToken: "failure-1",
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{
			Channel: failure.Channel, Key: failure.Key, Revision: failure.Revision,
		}},
	}
	if err := handler.StartSession(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	detail := awaitHandlerObservation(t, host, func(value protocol.Observation) bool {
		return value.Channel == ChannelDetail && value.Disposition != protocol.DispositionResolved
	})
	if detail.Disposition != protocol.DispositionSnapshot || cardElement(t, detail.Scene, "back-detail").Text.Value != "Display only" {
		t.Fatalf("failure detail = %#v", detail)
	}
}

func TestHandlerPublishesConnectionStateAndTreatsDisconnectAsHealthy(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	client := newFakeAppServerClient()
	host := &handlerRecordingHost{observations: make(chan protocol.Observation, 8)}
	var executable string
	var quotaEnabled bool
	handler := newHandler(host, func(path string, rateLimitsEnabled bool) appServerClient {
		executable = path
		quotaEnabled = rateLimitsEnabled
		return client
	}, func() (string, error) { return "/Users/test", nil }, func() time.Time { return now })
	timers := make(chan chan time.Time, 2)
	handler.after = func(time.Duration) <-chan time.Time {
		result := make(chan time.Time, 1)
		timers <- result
		return result
	}

	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{
		ID: "codex-main", Generation: 1, Config: []byte(`{"show_quota":true}`),
	}}); err != nil {
		t.Fatal(err)
	}
	<-client.started
	if executable != "/Users/test/.local/bin/codex" {
		t.Fatalf("Codex executable = %q", executable)
	}
	if !quotaEnabled {
		t.Fatal("Codex quota polling was not enabled")
	}
	<-timers
	if got := nextHandlerObservation(t, host); got.ReasonCode != "codex_reconnecting" {
		t.Fatalf("initial observation = %#v", got)
	}
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerConnected}
	if got := nextHandlerObservation(t, host); got.Instance.ID != "codex-main" || got.ReasonCode != "codex_connected" {
		t.Fatalf("connected observation = %#v", got)
	}
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerRateLimitsReadFailed, FailureStage: "rate_limits", FailureCode: "transport"}
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerRateLimitsReadFailed, FailureStage: "rate_limits", FailureCode: "transport"}
	quotaLog := nextHandlerLog(t, host, "codex_quota_unavailable")
	if quotaLog.Level != protocol.LogLevelWarn || quotaLog.Fields["stage"] != "rate_limits" || quotaLog.Fields["code"] != "transport" {
		t.Fatalf("quota failure log = %#v", quotaLog)
	}
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerRateLimitsSnapshot, RateLimits: &appserver.RateLimitSnapshot{
		LimitID: "codex", Primary: &appserver.RateLimitWindow{UsedPercent: 30, WindowDurationMinutes: 300, ResetsAt: now.Add(time.Hour).Unix()},
	}}
	nextHandlerLog(t, host, "codex_quota_recovered")
	if count := countHandlerLogs(host, "codex_quota_unavailable"); count != 1 {
		t.Fatalf("quota failure log count = %d, want 1", count)
	}
	client.in <- appserver.ManagerEvent{Kind: appserver.ManagerDisconnected, FailureStage: "connect", FailureCode: "transport"}
	reconnectTimer := <-timers
	if got := awaitHandlerObservation(t, host, func(value protocol.Observation) bool { return value.ReasonCode == "codex_reconnecting" }); got.ReasonCode != "codex_reconnecting" {
		t.Fatalf("reconnecting observation = %#v", got)
	}
	log := nextHandlerLog(t, host, "codex_app_server_disconnected")
	if log.Level != protocol.LogLevelWarn || log.Instance.ID != "codex-main" ||
		log.Fields["stage"] != "connect" || log.Fields["code"] != "transport" {
		t.Fatalf("disconnect log = %#v", log)
	}
	if health := handler.Health(context.Background()); !health.Healthy {
		t.Fatalf("disconnected health = %#v", health)
	}
	now = now.Add(reconnectGrace)
	reconnectTimer <- now
	if got := awaitHandlerObservation(t, host, func(value protocol.Observation) bool { return value.ReasonCode == "codex_disconnected" }); got.ReasonCode != "codex_disconnected" {
		t.Fatalf("disconnected observation = %#v", got)
	}
	if err := handler.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.stopped:
	default:
		t.Fatal("Shutdown returned before the app-server client stopped")
	}
}

func countHandlerLogs(host *handlerRecordingHost, event string) int {
	host.mu.Lock()
	defer host.mu.Unlock()
	count := 0
	for _, entry := range host.logs {
		if entry.Event == event {
			count++
		}
	}
	return count
}

func nextHandlerLog(t *testing.T, host *handlerRecordingHost, event string) protocol.LogNotification {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		host.mu.Lock()
		for _, entry := range host.logs {
			if entry.Event == event {
				host.mu.Unlock()
				return entry
			}
		}
		host.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("log event %q was not recorded", event)
	return protocol.LogNotification{}
}

func TestHandlerRejectsMultipleEnabledInstances(t *testing.T) {
	handler := newHandler(
		&handlerRecordingHost{observations: make(chan protocol.Observation, 1)},
		func(string, bool) appServerClient { return newFakeAppServerClient() },
		func() (string, error) { return "/Users/test", nil }, time.Now,
	)
	err := handler.ReplaceInstances(context.Background(), []protocol.Instance{
		{ID: "one", Config: []byte(`{}`)},
		{ID: "two", Config: []byte(`{}`)},
	})
	if err == nil {
		t.Fatal("multiple enabled instances were accepted")
	}
}

func TestHandlerRejectsUnavailableUserHome(t *testing.T) {
	handler := newHandler(
		&handlerRecordingHost{observations: make(chan protocol.Observation, 1)},
		func(string, bool) appServerClient { return newFakeAppServerClient() },
		func() (string, error) { return "", errors.New("unavailable") }, time.Now,
	)
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: "one", Config: []byte(`{}`)}}); err == nil {
		t.Fatal("unavailable user home was accepted")
	}
}

func nextHandlerObservation(t *testing.T, host *handlerRecordingHost) protocol.Observation {
	t.Helper()
	select {
	case observation := <-host.observations:
		return observation
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Codex observation")
		return protocol.Observation{}
	}
}

func awaitHandlerObservation(t *testing.T, host *handlerRecordingHost, accept func(protocol.Observation) bool) protocol.Observation {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case value := <-host.observations:
			if accept(value) {
				return value
			}
		case <-timer.C:
			t.Fatal("timed out waiting for matching Codex observation")
			return protocol.Observation{}
		}
	}
}

func awaitHandlerCompletion(t *testing.T, host *handlerRecordingHost) protocol.CompleteSessionRequest {
	t.Helper()
	select {
	case completed := <-host.completed:
		return completed
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Codex session completion")
		return protocol.CompleteSessionRequest{}
	}
}

func sessionInputRequest(t *testing.T, instanceID, token string, input *inputpb.InputEvent) protocol.SessionInputRequest {
	t.Helper()
	payload := protocol.SessionInput{}
	if encoder := input.GetEncoderEvent(); encoder != nil {
		payload.Encoder = &protocol.EncoderInput{Delta: encoder.GetDelta()}
	} else if button := input.GetButtonEvent(); button != nil {
		buttons := map[inputpb.Button]protocol.Button{inputpb.Button_OK: protocol.ButtonOK, inputpb.Button_BACK: protocol.ButtonBack, inputpb.Button_START: protocol.ButtonStart}
		payload.Button = &protocol.ButtonInput{Button: buttons[button.GetButton()], Action: protocol.ButtonPress}
	}
	return protocol.SessionInputRequest{Sequence: 1, OccurredAt: time.Now().UTC(), Instance: protocol.InstanceRef{ID: instanceID, Generation: 1}, SessionToken: token, Input: payload}
}

func encoderInputEvent(delta int32) *inputpb.InputEvent {
	return &inputpb.InputEvent{Event: &inputpb.InputEvent_EncoderEvent{EncoderEvent: &inputpb.EncoderEvent{Delta: delta}}}
}

func buttonInputEvent(button inputpb.Button) *inputpb.InputEvent {
	return &inputpb.InputEvent{Event: &inputpb.InputEvent_ButtonEvent{ButtonEvent: &inputpb.ButtonEvent{Button: button, Action: inputpb.ButtonAction_PRESS}}}
}
