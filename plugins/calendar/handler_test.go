package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestHandlerRejectsMultipleEnabledInstancesAndPluginOwnedState(t *testing.T) {
	factoryCalls := 0
	handler := newHandler(nil, func() (managedEventStore, error) {
		factoryCalls++
		return newMonitorEventStore(accessFull, nil), nil
	}, &fakeURLOpener{}, time.Now, testCalendarScene)
	tests := []struct {
		name      string
		instances []protocol.Instance
	}{
		{name: "multiple", instances: []protocol.Instance{
			{ID: "one", Generation: 1, Config: json.RawMessage(`{}`)},
			{ID: "two", Generation: 1, Config: json.RawMessage(`{}`)},
		}},
		{name: "secrets", instances: []protocol.Instance{{
			ID: AppID, Generation: 1, Config: json.RawMessage(`{}`), Secrets: map[string]string{"token": "secret"},
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := handler.ReplaceInstances(context.Background(), test.instances); err == nil {
				t.Fatal("invalid Calendar instance configuration was accepted")
			}
		})
	}
	if factoryCalls != 0 {
		t.Fatalf("event store factory calls = %d, want 0", factoryCalls)
	}
}

func TestHandlerPublishesAndInvokesOnlyTheExactUpcomingObservation(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	rawURL := "https://meet.google.com/abc-defg-hij"
	event := calendarEvent{
		CalendarID: "work", EventID: "next", Title: "Planning",
		URL: rawURL, Start: now.Add(2 * time.Minute), End: now.Add(time.Hour),
	}
	store := newMonitorEventStore(accessFull, []calendarEvent{event})
	opener := &fakeURLOpener{}
	host := newHandlerHost()
	handler := newHandler(host, func() (managedEventStore, error) { return store, nil }, opener, func() time.Time { return now }, testCalendarScene)
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{
		ID: AppID, Generation: 7, Config: json.RawMessage(`{}`),
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
	observation := nextHandlerObservation(t, host.observations)
	request := protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: AppID, Generation: 7}, Action: CalendarOptionsAction, SessionToken: "interactive-9",
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{
			Channel: ChannelUpcoming, Key: observation.Key, Revision: observation.Revision,
		}},
	}
	if err := handler.StartSession(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	chooser := nextHandlerObservation(t, host.observations)
	if chooser.Channel != ChannelInteraction || chooser.Disposition != protocol.DispositionSnapshot {
		t.Fatalf("chooser observation = %#v", chooser)
	}
	result, err := handler.HandleSessionInput(context.Background(), calendarSessionInputRequest(t, AppID, 7, "interactive-9", calendarButtonInput(protocol.ButtonOK)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != protocol.SessionInputConsumed {
		t.Fatalf("Calendar OK result = %#v", result)
	}
	select {
	case execution := <-host.executions:
		if execution != (protocol.SessionExecutionRequest{Instance: protocol.InstanceRef{ID: AppID, Generation: 7}, SessionToken: "interactive-9"}) {
			t.Fatalf("execution grant = %#v", execution)
		}
	case <-time.After(time.Second):
		t.Fatal("Calendar did not request an execution grant")
	}
	if len(opener.opened) != 1 || opener.opened[0] != rawURL {
		t.Fatalf("opened URLs = %v", opener.opened)
	}
	select {
	case completed := <-host.completions:
		if completed.Instance != (protocol.InstanceRef{ID: AppID, Generation: 7}) || completed.SessionToken != "interactive-9" {
			t.Fatalf("completion = %#v", completed)
		}
	case <-time.After(time.Second):
		t.Fatal("Calendar action did not complete its exact session")
	}

	invalid := []protocol.SessionStartRequest{
		{Instance: protocol.InstanceRef{ID: AppID, Generation: 7}, Action: "other", SessionToken: "interactive-10", Trigger: request.Trigger},
		{Instance: protocol.InstanceRef{ID: "other", Generation: 7}, Action: CalendarOptionsAction, SessionToken: "interactive-10", Trigger: request.Trigger},
		{Instance: protocol.InstanceRef{ID: AppID, Generation: 7}, Action: CalendarOptionsAction, SessionToken: "", Trigger: request.Trigger},
		{Instance: protocol.InstanceRef{ID: AppID, Generation: 7}, Action: CalendarOptionsAction, SessionToken: "interactive-10"},
		{Instance: protocol.InstanceRef{ID: AppID, Generation: 7}, Action: CalendarOptionsAction, SessionToken: "interactive-10", Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher}},
		{Instance: protocol.InstanceRef{ID: AppID, Generation: 7}, Action: CalendarOptionsAction, SessionToken: "interactive-10", Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{Channel: ChannelActive, Key: observation.Key, Revision: observation.Revision}}},
		{Instance: protocol.InstanceRef{ID: AppID, Generation: 7}, Action: CalendarOptionsAction, SessionToken: "interactive-10", Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{Channel: ChannelUpcoming, Key: observation.Key, Revision: observation.Revision + 999}}},
	}
	for index, invalidRequest := range invalid {
		if err := handler.StartSession(context.Background(), invalidRequest); err == nil {
			t.Fatalf("invalid request %d was accepted", index)
		}
	}
	if len(opener.opened) != 1 {
		t.Fatalf("invalid actions opened URLs: %v", opener.opened)
	}
}

func TestCalendarOptionsActionRejectsLegacyName(t *testing.T) {
	t.Parallel()
	if calendarOptionsAction("options") {
		t.Fatal("legacy options action was accepted")
	}
}

func TestHandlerOpensLauncherSessionWithoutObservationTrigger(t *testing.T) {
	now := time.Date(2026, time.September, 4, 17, 0, 0, 0, time.UTC)
	host := newHandlerHost()
	handler := newHandler(host, func() (managedEventStore, error) {
		return newMonitorEventStore(accessFull, nil), nil
	}, &fakeURLOpener{}, func() time.Time { return now }, testCalendarScene)
	if err := handler.ReplaceInstances(t.Context(), []protocol.Instance{{ID: AppID, Generation: 7, Config: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })

	request := protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: AppID, Generation: 7}, Action: CalendarOpenAction, SessionToken: "launcher-calendar",
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher},
	}
	if err := handler.StartSession(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	opened := nextHandlerObservation(t, host.observations)
	if opened.Channel != ChannelInteraction || opened.Disposition != protocol.DispositionSnapshot || opened.ReasonCode != "calendar_launcher" || opened.Scene == nil {
		t.Fatalf("Calendar launcher observation = %#v", opened)
	}
	result, err := handler.HandleSessionInput(t.Context(), calendarSessionInputRequest(t, AppID, 7, request.SessionToken, calendarButtonInput(protocol.ButtonBack)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != protocol.SessionInputConsumed {
		t.Fatalf("Calendar launcher Back result = %#v", result)
	}
	if resolved := nextHandlerObservation(t, host.observations); resolved.Disposition != protocol.DispositionResolved {
		t.Fatalf("Calendar launcher resolution = %#v", resolved)
	}
	select {
	case completed := <-host.completions:
		if completed.Instance != request.Instance || completed.SessionToken != request.SessionToken {
			t.Fatalf("Calendar launcher completion = %#v", completed)
		}
	case <-time.After(time.Second):
		t.Fatal("Calendar launcher session did not complete")
	}
}

func TestHandlerWithdrawsOnRevocationAndRecoversWithoutLeakingDetails(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	event := calendarEvent{
		CalendarID: "private-work", EventID: "secret-id", Title: "Confidential acquisition",
		URL: "https://zoom.us/j/123?pwd=secret", Start: now.Add(2 * time.Minute), End: now.Add(time.Hour),
	}
	store := newMonitorEventStore(accessFull, []calendarEvent{event})
	host := newHandlerHost()
	handler := newHandler(host, func() (managedEventStore, error) { return store, nil }, &fakeURLOpener{}, func() time.Time { return now }, testCalendarScene)
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: AppID, Generation: 1, Config: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
	_ = nextHandlerObservation(t, host.observations)

	store.setStatus(accessDenied)
	store.changes <- struct{}{}
	resolved := nextHandlerObservation(t, host.observations)
	if resolved.Disposition != protocol.DispositionResolved {
		t.Fatalf("revocation observation = %#v", resolved)
	}
	log := nextHandlerLog(t, host.logs)
	if log.Event != "calendar_access_unavailable" || len(log.Fields) != 0 || log.Message == event.Title || log.Message == event.URL {
		t.Fatalf("unsafe access log = %#v", log)
	}
	if handler.Health(context.Background()).Healthy {
		t.Fatal("handler remained healthy after Calendar access revocation")
	}

	store.setStatus(accessFull)
	store.changes <- struct{}{}
	recovered := nextHandlerObservation(t, host.observations)
	if recovered.Disposition != protocol.DispositionActionable {
		t.Fatalf("recovery observation = %#v", recovered)
	}
	if log := nextHandlerLog(t, host.logs); log.Event != "calendar_recovered" {
		t.Fatalf("recovery log = %#v", log)
	}
	if !handler.Health(context.Background()).Healthy {
		t.Fatal("handler did not recover after Calendar access was restored")
	}
}

func TestHandlerChooserRoutesEncoderAttendSkipAndBack(t *testing.T) {
	tests := []struct {
		name       string
		delta      int32
		button     protocol.Button
		want       attendanceDecision
		checkpoint bool
	}{
		{name: "attend", delta: 1, button: protocol.ButtonOK, want: decisionAttending, checkpoint: true},
		{name: "skip", delta: 2, button: protocol.ButtonOK, want: decisionSkipped, checkpoint: true},
		{name: "back", button: protocol.ButtonBack},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.August, 24, 17, 58, 0, 0, time.UTC)
			event := calendarEvent{CalendarID: "work", EventID: test.name, URL: "https://meet.google.com/abc-defg-hij", Start: now.Add(2 * time.Minute), End: now.Add(32 * time.Minute)}
			store := newMonitorEventStore(accessFull, []calendarEvent{event})
			opener := &fakeURLOpener{}
			host := newHandlerHost()
			handler := newHandler(host, func() (managedEventStore, error) { return store, nil }, opener, func() time.Time { return now }, testCalendarScene)
			if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: AppID, Generation: 3, Config: json.RawMessage(`{}`)}}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
			observation := nextHandlerObservation(t, host.observations)
			<-host.checkpoints
			request := protocol.SessionStartRequest{Instance: protocol.InstanceRef{ID: AppID, Generation: 3}, Action: CalendarOptionsAction, SessionToken: "choice-" + test.name, Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{Channel: ChannelUpcoming, Key: observation.Key, Revision: observation.Revision}}}
			if err := handler.StartSession(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			_ = nextHandlerObservation(t, host.observations)
			if test.delta != 0 {
				if _, err := handler.HandleSessionInput(context.Background(), calendarSessionInputRequest(t, AppID, 3, request.SessionToken, calendarEncoderInput(test.delta))); err != nil {
					t.Fatal(err)
				}
				_ = nextHandlerObservation(t, host.observations)
			}
			if _, err := handler.HandleSessionInput(context.Background(), calendarSessionInputRequest(t, AppID, 3, request.SessionToken, calendarButtonInput(test.button))); err != nil {
				t.Fatal(err)
			}
			if len(opener.opened) != 0 {
				t.Fatalf("%s unexpectedly opened %v", test.name, opener.opened)
			}
			if test.checkpoint {
				select {
				case checkpoint := <-host.checkpoints:
					var value attendanceCheckpoint
					if err := json.Unmarshal(checkpoint.Data, &value); err != nil || value.Decisions[observation.Key] != test.want {
						t.Fatalf("checkpoint = %s / %v", checkpoint.Data, err)
					}
				case <-time.After(time.Second):
					t.Fatal("meeting choice was not checkpointed")
				}
			} else {
				select {
				case checkpoint := <-host.checkpoints:
					t.Fatalf("BACK persisted checkpoint %#v", checkpoint)
				default:
				}
			}
		})
	}
}

func TestHandlerPersistsFailedCheckpointOnRefreshWithoutOpeningJoinURLAgain(t *testing.T) {
	now := time.Date(2026, time.August, 24, 17, 58, 0, 0, time.UTC)
	event := calendarEvent{CalendarID: "work", EventID: "retry", URL: "https://meet.google.com/abc-defg-hij", Start: now.Add(2 * time.Minute), End: now.Add(32 * time.Minute)}
	store := newMonitorEventStore(accessFull, []calendarEvent{event})
	opener := &fakeURLOpener{}
	host := newHandlerHost()
	host.checkpointErr = errors.New("checkpoint storage unavailable")
	handler := newHandler(host, func() (managedEventStore, error) { return store, nil }, opener, func() time.Time { return now }, testCalendarScene)
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: AppID, Generation: 4, Config: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
	observation := nextHandlerObservation(t, host.observations)
	<-host.checkpoints
	token := "checkpoint-retry"
	request := protocol.SessionStartRequest{Instance: protocol.InstanceRef{ID: AppID, Generation: 4}, Action: CalendarOptionsAction, SessionToken: token, Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{Channel: ChannelUpcoming, Key: observation.Key, Revision: observation.Revision}}}
	if err := handler.StartSession(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	_ = nextHandlerObservation(t, host.observations)
	if _, err := handler.HandleSessionInput(t.Context(), calendarSessionInputRequest(t, AppID, 4, token, calendarButtonInput(protocol.ButtonOK))); err == nil {
		t.Fatal("checkpoint failure was acknowledged as a successful choice")
	}
	<-host.checkpoints
	if len(opener.opened) != 1 {
		t.Fatalf("initial JOIN opened %v", opener.opened)
	}
	if handler.Health(context.Background()).Healthy {
		t.Fatal("checkpoint failure did not degrade Calendar health")
	}
	select {
	case completion := <-host.completions:
		if completion != (protocol.CompleteSessionRequest{Instance: protocol.InstanceRef{ID: AppID, Generation: 4}, SessionToken: token}) {
			t.Fatalf("completed session = %#v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("checkpoint failure left the granted session executing")
	}
	if observation := nextHandlerObservation(t, host.observations); observation.Disposition != protocol.DispositionResolved {
		t.Fatalf("checkpoint cleanup observation = %#v", observation)
	}
	host.checkpointErr = nil
	handler.mu.RLock()
	worker := handler.worker
	handler.mu.RUnlock()
	if worker == nil {
		t.Fatal("Calendar worker stopped before checkpoint recovery")
	}
	refresh, refreshErr := worker.state.Refresh(t.Context())
	worker.apply(t.Context(), refresh, refreshErr)
	select {
	case <-host.checkpoints:
	case <-time.After(time.Second):
		t.Fatal("dirty attendance checkpoint was not persisted on refresh")
	}
	if len(opener.opened) != 1 {
		t.Fatalf("checkpoint refresh reopened JOIN URL: %v", opener.opened)
	}
	select {
	case completed := <-host.completions:
		t.Fatalf("checkpoint recovery completed the session twice: %#v", completed)
	default:
	}
}

func TestHandlerClosesGrantedSessionWhenJoinEffectFails(t *testing.T) {
	now := time.Date(2026, time.August, 24, 17, 58, 0, 0, time.UTC)
	event := calendarEvent{CalendarID: "work", EventID: "failed-join", URL: "https://meet.google.com/abc-defg-hij", Start: now.Add(2 * time.Minute), End: now.Add(32 * time.Minute)}
	store := newMonitorEventStore(accessFull, []calendarEvent{event})
	opener := &fakeURLOpener{err: errors.New("open failed")}
	host := newHandlerHost()
	handler := newHandler(host, func() (managedEventStore, error) { return store, nil }, opener, func() time.Time { return now }, testCalendarScene)
	if err := handler.ReplaceInstances(t.Context(), []protocol.Instance{{ID: AppID, Generation: 4, Config: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
	observation := nextHandlerObservation(t, host.observations)
	<-host.checkpoints
	token := "failed-join"
	if err := handler.StartSession(t.Context(), protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: AppID, Generation: 4}, Action: CalendarOptionsAction, SessionToken: token,
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{Channel: ChannelUpcoming, Key: observation.Key, Revision: observation.Revision}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = nextHandlerObservation(t, host.observations)
	if _, err := handler.HandleSessionInput(t.Context(), calendarSessionInputRequest(t, AppID, 4, token, calendarButtonInput(protocol.ButtonOK))); err == nil {
		t.Fatal("failed URL effect returned success")
	}
	if execution := <-host.executions; execution.SessionToken != token {
		t.Fatalf("execution grant = %#v", execution)
	}
	select {
	case completion := <-host.completions:
		if completion.SessionToken != token {
			t.Fatalf("completion = %#v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("failed post-grant effect left the session executing")
	}
	if len(opener.opened) != 0 {
		t.Fatalf("failed opener recorded successful URLs: %v", opener.opened)
	}
}

func TestHandlerClosesEventStoreAfterShutdown(t *testing.T) {
	store := newMonitorEventStore(accessDenied, nil)
	host := newHandlerHost()
	handler := newHandler(host, func() (managedEventStore, error) { return store, nil }, &fakeURLOpener{}, time.Now, testCalendarScene)
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: AppID, Generation: 1, Config: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	_ = nextHandlerLog(t, host.logs)
	if err := handler.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.closeCount() != 1 {
		t.Fatalf("event store closes = %d, want 1", store.closeCount())
	}
}

func TestHandlerRetriesSoonAfterPublicationFailure(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	store := newMonitorEventStore(accessFull, []calendarEvent{{
		CalendarID: "work", EventID: "next", Title: "Planning",
		Start: now.Add(2 * time.Minute), End: now.Add(time.Hour),
	}})
	host := newHandlerHost()
	host.observeErr = errors.New("daemon is still initializing")
	handler := newHandler(host, nil, &fakeURLOpener{}, func() time.Time { return now }, testCalendarScene)
	state := newCalendarState(store, &fakeURLOpener{}, func() time.Time { return now }, 5*time.Minute)
	selected, err := state.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	worker := &calendarWorker{
		instanceID: AppID,
		host:       host,
		state:      state,
		publisher:  newCalendarPublisher(host, protocol.InstanceRef{ID: AppID, Generation: 1}, mustCalendarConfig(t), func() time.Time { return now }, testCalendarScene),
		owner:      handler,
	}
	handler.worker = worker

	worker.apply(context.Background(), selected, nil)

	if handler.Health(context.Background()).Healthy {
		t.Fatal("handler remained healthy after publication failed")
	}
	if got, want := state.NextRefresh(), 5*time.Second; got != want {
		t.Fatalf("next refresh = %s, want publication retry %s", got, want)
	}
}

type handlerHost struct {
	observations  chan protocol.Observation
	logs          chan protocol.LogNotification
	completions   chan protocol.CompleteSessionRequest
	checkpoints   chan protocol.CheckpointRequest
	executions    chan protocol.SessionExecutionRequest
	observeErr    error
	checkpointErr error
	executionErr  error
}

func newHandlerHost() *handlerHost {
	return &handlerHost{
		observations: make(chan protocol.Observation, 16),
		logs:         make(chan protocol.LogNotification, 16),
		completions:  make(chan protocol.CompleteSessionRequest, 4),
		checkpoints:  make(chan protocol.CheckpointRequest, 4),
		executions:   make(chan protocol.SessionExecutionRequest, 4),
	}
}

func (h *handlerHost) BeginSessionExecution(_ context.Context, request protocol.SessionExecutionRequest) error {
	h.executions <- request
	return h.executionErr
}

func (h *handlerHost) SaveCheckpoint(_ context.Context, request protocol.CheckpointRequest) error {
	h.checkpoints <- request
	return h.checkpointErr
}

func (h *handlerHost) PublishObservation(_ context.Context, observation protocol.Observation) error {
	if h.observeErr != nil {
		return h.observeErr
	}
	h.observations <- observation
	return nil
}

func (h *handlerHost) Log(_ context.Context, notification protocol.LogNotification) error {
	h.logs <- notification
	return nil
}

func (h *handlerHost) CompleteSession(_ context.Context, request protocol.CompleteSessionRequest) error {
	h.completions <- request
	return nil
}

func nextHandlerObservation(t *testing.T, values <-chan protocol.Observation) protocol.Observation {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("Calendar handler did not publish an observation")
		return protocol.Observation{}
	}
}

func nextHandlerLog(t *testing.T, values <-chan protocol.LogNotification) protocol.LogNotification {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("Calendar handler did not publish a log transition")
		return protocol.LogNotification{}
	}
}

func calendarSessionInputRequest(_ *testing.T, instanceID string, generation uint64, token string, input protocol.SessionInput) protocol.SessionInputRequest {
	return protocol.SessionInputRequest{Sequence: 1, OccurredAt: time.Now().UTC(), Instance: protocol.InstanceRef{ID: instanceID, Generation: generation}, SessionToken: token, Input: input}
}

func calendarButtonInput(button protocol.Button) protocol.SessionInput {
	return protocol.SessionInput{Button: &protocol.ButtonInput{Button: button, Action: protocol.ButtonPress}}
}

func calendarEncoderInput(delta int32) protocol.SessionInput {
	return protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: delta}}
}
