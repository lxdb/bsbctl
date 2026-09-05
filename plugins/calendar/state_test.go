package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func newCalendarState(store eventStore, opener urlOpener, now func() time.Time, reminderLead time.Duration) *calendarState {
	config := defaultCalendarConfig()
	config.ReminderLead = reminderLead
	return newCalendarStateWithConfig(store, opener, now, config)
}

func TestCalendarStateRecordsOnlyExplicitAttendanceChoices(t *testing.T) {
	now := time.Date(2026, time.August, 24, 17, 58, 0, 0, time.UTC)
	event := calendarEvent{CalendarID: "work", EventID: "meeting", URL: "https://meet.google.com/abc-defg-hij", Start: now.Add(2 * time.Minute), End: now.Add(32 * time.Minute)}
	store := &fakeEventStore{status: accessFull, events: []calendarEvent{event}}
	opener := &fakeURLOpener{}
	state := newCalendarState(store, opener, func() time.Time { return now }, 5*time.Minute)

	refresh, err := state.Refresh(context.Background())
	if err != nil || refresh.Upcoming == nil || refresh.Active != nil {
		t.Fatalf("initial selection/error = %#v / %v", refresh.selectedEvents, err)
	}
	selected, err := state.DecideWithGrant(context.Background(), observationKey(event), meetingChoiceAttend, nil)
	if err != nil || selected.Upcoming == nil || selected.Active != nil {
		t.Fatalf("attend selection/error = %#v / %v", selected, err)
	}
	if len(opener.opened) != 0 {
		t.Fatalf("ATTEND opened URLs: %v", opener.opened)
	}
	selected, err = state.DecideWithGrant(context.Background(), observationKey(event), meetingChoiceSkip, nil)
	if err != nil || selected.Upcoming != nil || selected.Active != nil {
		t.Fatalf("skip selection/error = %#v / %v", selected, err)
	}
}

func TestCalendarStateAppliesPerCalendarEnablementAndReminderLead(t *testing.T) {
	now := time.Date(2026, time.August, 27, 18, 0, 0, 0, time.UTC)
	disabled := calendarEvent{CalendarID: "disabled", EventID: "ignored", Start: now.Add(time.Minute), End: now.Add(time.Hour)}
	longLead := calendarEvent{CalendarID: "long-lead", EventID: "selected", Start: now.Add(10 * time.Minute), End: now.Add(time.Hour)}
	raw := `{"calendars":[{"key":"` + calendarKey(disabled.CalendarID) + `","enabled":false},{"key":"` + calendarKey(longLead.CalendarID) + `","reminder_lead_minutes":15}]}`
	config, err := decodeConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	state := newCalendarStateWithConfig(&fakeEventStore{status: accessFull, events: []calendarEvent{disabled, longLead}}, &fakeURLOpener{}, func() time.Time { return now }, config)
	selected, err := state.Refresh(context.Background())
	if err != nil || selected.Upcoming == nil || selected.Upcoming.EventID != "selected" {
		t.Fatalf("selection/error = %#v / %v", selected, err)
	}
	if got := state.NextRefresh(); got != 10*time.Minute {
		t.Fatalf("next refresh = %s, want event start in 10m", got)
	}
}

func TestCalendarStateCatalogUsesOpaqueFirstSeenPriorityAndPrivateCheckpoint(t *testing.T) {
	now := time.Date(2026, time.August, 27, 18, 0, 0, 0, time.UTC)
	store := &fakeEventStore{status: accessFull, calendars: []calendarInfo{
		{ID: "private-work-id", Title: "Work", Source: "Google"},
		{},
		{ID: "family-id", Title: "Family", Source: "iCloud"},
	}}
	state := newCalendarStateWithConfig(store, &fakeURLOpener{}, func() time.Time { return now }, mustCalendarConfig(t))
	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	catalog := state.CalendarCatalog()
	if len(catalog) != 2 || catalog[0].Key != calendarKey("family-id") || catalog[0].Priority != 2 || catalog[1].Key != calendarKey("private-work-id") || catalog[1].Priority != 1 {
		t.Fatalf("catalog = %#v", catalog)
	}
	checkpoint, err := state.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if containsAny(string(checkpoint), "private-work-id", "family-id", "Work", "Family", "Google", "iCloud") {
		t.Fatalf("checkpoint exposed Calendar metadata: %s", checkpoint)
	}
	if !strings.Contains(string(checkpoint), `"schema_version":1`) {
		t.Fatalf("checkpoint has no plugin schema version: %s", checkpoint)
	}
	store.calendars = append(store.calendars, calendarInfo{ID: "new-id", Title: "New", Source: "Google"})
	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := state.CalendarCatalog()[0]; got.Key != calendarKey("new-id") || got.Priority != 3 {
		t.Fatalf("new calendar priority = %#v", got)
	}
}

func TestCalendarCheckpointRequiresCurrentSchema(t *testing.T) {
	state := newCalendarStateWithConfig(&fakeEventStore{}, &fakeURLOpener{}, time.Now, mustCalendarConfig(t))
	key := observationKey(calendarEvent{CalendarID: "work", EventID: "meeting", Start: time.Date(2026, time.August, 27, 18, 0, 0, 0, time.UTC)})
	for _, checkpoint := range []json.RawMessage{
		json.RawMessage(`{"schema_version":1,"decisions":{"` + key + `":"attending"}}`),
	} {
		state.RestoreCheckpoint(checkpoint)
		if state.decisions[key] != decisionAttending {
			t.Fatalf("checkpoint %s was not restored", checkpoint)
		}
		state.decisions = make(map[string]attendanceDecision)
	}
	for _, schema := range []string{"", `"schema_version":0,`, `"schema_version":2,`} {
		state.RestoreCheckpoint(json.RawMessage(`{` + schema + `"decisions":{"` + key + `":"attending"}}`))
		if len(state.decisions) != 0 {
			t.Fatalf("unsupported checkpoint schema restored %#v", state.decisions)
		}
	}
}

func TestCalendarStateRefreshReportsCheckpointChanges(t *testing.T) {
	now := time.Date(2026, time.August, 27, 18, 0, 0, 0, time.UTC)
	event := calendarEvent{CalendarID: "work", EventID: "meeting", Start: now.Add(time.Minute), End: now.Add(time.Hour)}
	personal := calendarEvent{CalendarID: "personal", EventID: "meeting", Start: event.Start, End: event.End}
	store := &fakeEventStore{status: accessFull, events: []calendarEvent{event, personal}, calendars: []calendarInfo{{ID: "work"}, {ID: "personal"}}}
	state := newCalendarStateWithConfig(store, &fakeURLOpener{}, func() time.Time { return now }, mustCalendarConfig(t))

	first, err := state.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !first.CheckpointChanged {
		t.Fatal("initial calendar discovery did not report a checkpoint change")
	}
	checkpoint, err := state.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	reversedStore := &fakeEventStore{status: accessFull, events: []calendarEvent{event, personal}, calendars: []calendarInfo{{ID: "personal"}, {ID: "work"}}}
	restored := newCalendarStateWithConfig(reversedStore, &fakeURLOpener{}, func() time.Time { return now }, mustCalendarConfig(t))
	restored.RestoreCheckpoint(checkpoint)
	restoredRefresh, err := restored.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if restoredRefresh.CheckpointChanged || restoredRefresh.Upcoming == nil || restoredRefresh.Upcoming.CalendarID != "personal" {
		t.Fatalf("restored priority changed after reversed discovery: %#v", restoredRefresh)
	}
	second, err := state.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second.CheckpointChanged {
		t.Fatal("unchanged refresh reported a checkpoint change")
	}

	if _, err := state.DecideWithGrant(t.Context(), observationKey(event), meetingChoiceSkip, nil); err != nil {
		t.Fatal(err)
	}
	store.events = nil
	pruned, err := state.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !pruned.CheckpointChanged {
		t.Fatal("decision pruning did not report a checkpoint change")
	}
	prunedCheckpoint, err := state.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	var persisted attendanceCheckpoint
	if err := json.Unmarshal(prunedCheckpoint, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Decisions) != 0 {
		t.Fatalf("pruned decisions remained in checkpoint: %#v", persisted.Decisions)
	}
}

func TestCalendarOperationReturnsExplicitLocalCatalog(t *testing.T) {
	store := &fakeEventStore{status: accessFull, calendars: []calendarInfo{{ID: "work-id", Title: "Work", Source: "Google"}}}
	state := newCalendarStateWithConfig(store, &fakeURLOpener{}, time.Now, mustCalendarConfig(t))
	host := newHandlerHost()
	handler := &Handler{}
	handler.worker = &calendarWorker{instanceID: AppID, generation: 4, state: state, host: host, owner: handler}
	result, err := handler.InvokeOperation(context.Background(), protocol.OperationRequest{Instance: protocol.InstanceRef{ID: AppID, Generation: 4}, Operation: OperationCalendars, Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	const wantJSON = `{"calendars":[{
		"key":"calendar-b092b5692135187155da2f7706d6ad03c131ffeb96e75de40a4bdbcb2759f9b9",
		"title":"Work","source":"Google","priority":1,
		"effective":{"enabled":true,"reminder_enabled":true,"reminder_lead_minutes":5,
			"reminder_sound":true,"reminder_show_event_name":true,"active_enabled":true,
			"active_sound":true,"active_display":"event_name","active_theme":"meeting"}
	}]}`
	var got, want any
	if err := json.Unmarshal(result.Payload, &got); err != nil {
		t.Fatalf("decode catalog result: %v", err)
	}
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog payload = %s, want %s", result.Payload, wantJSON)
	}
	if store.fetches != 0 {
		t.Fatalf("catalog query fetched %d event batches, want none", store.fetches)
	}
	select {
	case checkpoint := <-host.checkpoints:
		if checkpoint.Instance != (protocol.InstanceRef{ID: AppID, Generation: 4}) {
			t.Fatalf("checkpoint instance = %#v, want Calendar generation 4", checkpoint.Instance)
		}
	case <-time.After(time.Second):
		t.Fatal("new calendar order was not checkpointed")
	}
	if _, err := handler.InvokeOperation(context.Background(), protocol.OperationRequest{Instance: protocol.InstanceRef{ID: AppID, Generation: 4}, Operation: "unsupported"}); err == nil {
		t.Fatal("Calendar catalog accepted an unsupported operation")
	}
	if _, err := handler.InvokeOperation(context.Background(), protocol.OperationRequest{Instance: protocol.InstanceRef{ID: AppID, Generation: 4}, Operation: OperationCalendars, Payload: json.RawMessage(`{"unexpected":true}`)}); err == nil {
		t.Fatal("Calendar catalog accepted a non-empty payload")
	}
}

func TestCalendarOperationReportsUnavailableAccess(t *testing.T) {
	state := newCalendarStateWithConfig(&fakeEventStore{status: accessDenied}, &fakeURLOpener{}, time.Now, mustCalendarConfig(t))
	handler := &Handler{worker: &calendarWorker{instanceID: AppID, generation: 1, state: state}}
	_, err := handler.InvokeOperation(context.Background(), protocol.OperationRequest{Instance: protocol.InstanceRef{ID: AppID, Generation: 1}, Operation: OperationCalendars})
	if !errors.Is(err, ErrCalendarAccess) {
		t.Fatalf("operation error = %v, want Calendar access error", err)
	}
}

func TestCalendarStateJoinOpensThenMarksAttendanceDuringGrace(t *testing.T) {
	now := time.Date(2026, time.August, 24, 18, 5, 0, 0, time.UTC)
	event := calendarEvent{CalendarID: "work", EventID: "meeting", URL: "https://zoom.us/j/123", Start: now.Add(-5 * time.Minute), End: now.Add(25 * time.Minute)}
	store := &fakeEventStore{status: accessFull, events: []calendarEvent{event}}
	opener := &fakeURLOpener{}
	state := newCalendarState(store, opener, func() time.Time { return now }, 5*time.Minute)

	selected, err := state.DecideWithGrant(context.Background(), observationKey(event), meetingChoiceJoin, nil)
	if err != nil || selected.Active == nil {
		t.Fatalf("late join selection/error = %#v / %v", selected, err)
	}
	if len(opener.opened) != 1 || opener.opened[0] != event.URL {
		t.Fatalf("opened URLs = %v", opener.opened)
	}
}

func TestCalendarStateExecutionDenialPreventsJoinAndDecision(t *testing.T) {
	now := time.Date(2026, time.August, 24, 18, 5, 0, 0, time.UTC)
	event := calendarEvent{CalendarID: "work", EventID: "meeting", URL: "https://zoom.us/j/123", Start: now.Add(-5 * time.Minute), End: now.Add(25 * time.Minute)}
	opener := &fakeURLOpener{}
	state := newCalendarState(&fakeEventStore{status: accessFull, events: []calendarEvent{event}}, opener, func() time.Time { return now }, 5*time.Minute)
	want := errors.New("execution denied")
	grants := 0
	if _, err := state.DecideWithGrant(t.Context(), observationKey(event), meetingChoiceJoin, func() error {
		grants++
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("DecideWithGrant error = %v, want execution denial", err)
	}
	if grants != 1 || len(opener.opened) != 0 || len(state.decisions) != 0 {
		t.Fatalf("grant/opened/decisions = %d/%v/%v", grants, opener.opened, state.decisions)
	}
}

func TestCalendarCheckpointContainsOnlyBoundedOpaqueDecisions(t *testing.T) {
	now := time.Date(2026, time.August, 24, 18, 0, 0, 0, time.UTC)
	event := calendarEvent{CalendarID: "private-work", EventID: "secret-id", Title: "Confidential", URL: "https://zoom.us/j/123?pwd=secret", Start: now.Add(time.Minute), End: now.Add(time.Hour)}
	state := newCalendarState(&fakeEventStore{status: accessFull, events: []calendarEvent{event}}, &fakeURLOpener{}, func() time.Time { return now }, 5*time.Minute)
	if _, err := state.DecideWithGrant(context.Background(), observationKey(event), meetingChoiceAttend, nil); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := state.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if string(checkpoint) == "" || containsAny(string(checkpoint), event.CalendarID, event.EventID, event.Title, event.URL) {
		t.Fatalf("unsafe checkpoint = %s", checkpoint)
	}
	restored := newCalendarState(&fakeEventStore{status: accessFull, events: []calendarEvent{event}}, &fakeURLOpener{}, func() time.Time { return now }, 5*time.Minute)
	restored.RestoreCheckpoint(checkpoint)
	selected, err := restored.Refresh(context.Background())
	if err != nil || selected.Upcoming == nil {
		t.Fatalf("restored selection/error = %#v / %v", selected, err)
	}
	restored.RestoreCheckpoint(json.RawMessage(`{"schema_version":1,"decisions":{"not-an-opaque-key":"attending"}}`))
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if candidate != "" && strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func TestCalendarStateRequestsFullAccessBeforeReadingEvents(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	store := &fakeEventStore{
		status:        accessNotDetermined,
		requestStatus: accessFull,
		events: []calendarEvent{{
			CalendarID: "work", EventID: "next", Start: now.Add(2 * time.Minute), End: now.Add(time.Hour),
		}},
	}
	state := newCalendarState(store, &fakeURLOpener{}, func() time.Time { return now }, 5*time.Minute)

	selected, err := state.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if store.requests != 1 {
		t.Fatalf("access requests = %d, want 1", store.requests)
	}
	if store.fetches != 1 {
		t.Fatalf("event fetches = %d, want 1", store.fetches)
	}
	if !store.start.Equal(now.Add(-24*time.Hour)) || !store.end.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("query window = [%s, %s]", store.start, store.end)
	}
	if selected.Upcoming == nil || selected.Upcoming.EventID != "next" {
		t.Fatalf("selected = %#v", selected)
	}
}

func TestCalendarStateDoesNotReadEventsWithoutFullAccess(t *testing.T) {
	statuses := []accessStatus{accessDenied, accessRestricted, accessWriteOnly}
	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			store := &fakeEventStore{status: status}
			state := newCalendarState(store, &fakeURLOpener{}, time.Now, 5*time.Minute)

			if _, err := state.Refresh(context.Background()); !errors.Is(err, ErrCalendarAccess) {
				t.Fatalf("Refresh error = %v, want ErrCalendarAccess", err)
			}
			if store.requests != 0 || store.fetches != 0 {
				t.Fatalf("without access: requests=%d fetches=%d", store.requests, store.fetches)
			}
		})
	}
}

func TestCalendarStateDoesNotReadWhenAccessRequestIsDenied(t *testing.T) {
	store := &fakeEventStore{status: accessNotDetermined, requestStatus: accessDenied}
	state := newCalendarState(store, &fakeURLOpener{}, time.Now, 5*time.Minute)

	if _, err := state.Refresh(context.Background()); !errors.Is(err, ErrCalendarAccess) {
		t.Fatalf("Refresh error = %v, want ErrCalendarAccess", err)
	}
	if store.requests != 1 || store.fetches != 0 {
		t.Fatalf("denied request: requests=%d fetches=%d", store.requests, store.fetches)
	}
}

func TestCalendarStateJoinRevalidatesAndOpensTheExactStructuredURL(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	rawURL := "https://team.zoom.us/j/123456789?pwd=private"
	event := calendarEvent{
		CalendarID: "work", EventID: "next", URL: rawURL,
		Start: now.Add(2 * time.Minute), End: now.Add(time.Hour),
	}
	store := &fakeEventStore{status: accessFull, events: []calendarEvent{event}}
	opener := &fakeURLOpener{}
	state := newCalendarState(store, opener, func() time.Time { return now }, 5*time.Minute)

	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := state.DecideWithGrant(context.Background(), observationKey(event), meetingChoiceJoin, nil); err != nil {
		t.Fatal(err)
	}
	if store.fetches != 2 {
		t.Fatalf("event fetches = %d, want refresh plus join revalidation", store.fetches)
	}
	if len(opener.opened) != 1 || opener.opened[0] != rawURL {
		t.Fatalf("opened URLs = %v", opener.opened)
	}
}

func TestCalendarStateJoinRejectsSupersededOccurrence(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	old := calendarEvent{
		CalendarID: "work", EventID: "old", URL: "https://meet.google.com/abc-defg-hij",
		Start: now.Add(2 * time.Minute), End: now.Add(time.Hour),
	}
	store := &fakeEventStore{status: accessFull, events: []calendarEvent{old}}
	opener := &fakeURLOpener{}
	state := newCalendarState(store, opener, func() time.Time { return now }, 5*time.Minute)
	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.events = []calendarEvent{{
		CalendarID: "work", EventID: "replacement", URL: "https://meet.google.com/new-room",
		Start: now.Add(time.Minute), End: now.Add(time.Hour),
	}}

	if _, err := state.DecideWithGrant(context.Background(), observationKey(old), meetingChoiceJoin, nil); !errors.Is(err, ErrStaleMeetingAction) {
		t.Fatalf("Join error = %v, want ErrStaleMeetingAction", err)
	}
	if len(opener.opened) != 0 {
		t.Fatalf("stale action opened %v", opener.opened)
	}
}

func TestCalendarStateJoinRejectsAtStartAndWithoutARecognizedURL(t *testing.T) {
	base := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	now := base
	event := calendarEvent{
		CalendarID: "work", EventID: "next", Start: base.Add(2 * time.Minute), End: base.Add(time.Hour),
	}
	store := &fakeEventStore{status: accessFull, events: []calendarEvent{event}}
	opener := &fakeURLOpener{}
	state := newCalendarState(store, opener, func() time.Time { return now }, 5*time.Minute)

	if _, err := state.DecideWithGrant(context.Background(), observationKey(event), meetingChoiceJoin, nil); !errors.Is(err, ErrMeetingLinkUnavailable) {
		t.Fatalf("Join without link error = %v, want ErrMeetingLinkUnavailable", err)
	}
	event.URL = "https://meet.google.com/abc-defg-hij"
	store.events = []calendarEvent{event}
	now = event.Start
	if _, err := state.DecideWithGrant(context.Background(), observationKey(event), meetingChoiceJoin, nil); err != nil {
		t.Fatalf("Join at start error = %v", err)
	}
	if len(opener.opened) != 1 || opener.opened[0] != event.URL {
		t.Fatalf("join at start opened %v", opener.opened)
	}
}

func TestCalendarStateJoinFailsClosedWhenRevalidationFails(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	event := calendarEvent{
		CalendarID: "work", EventID: "next", URL: "https://meet.google.com/abc-defg-hij",
		Start: now.Add(2 * time.Minute), End: now.Add(time.Hour),
	}
	store := &fakeEventStore{status: accessFull, events: []calendarEvent{event}, fetchError: errors.New("EventKit failed")}
	opener := &fakeURLOpener{}
	state := newCalendarState(store, opener, func() time.Time { return now }, 5*time.Minute)

	if _, err := state.DecideWithGrant(context.Background(), observationKey(event), meetingChoiceJoin, nil); err == nil || errors.Is(err, ErrStaleMeetingAction) {
		t.Fatalf("Join error = %v, want EventKit failure", err)
	}
	if len(opener.opened) != 0 {
		t.Fatalf("failed revalidation opened %v", opener.opened)
	}
}

type fakeEventStore struct {
	mu            sync.Mutex
	status        accessStatus
	requestStatus accessStatus
	requestError  error
	events        []calendarEvent
	calendars     []calendarInfo
	fetchError    error
	requests      int
	fetches       int
	start         time.Time
	end           time.Time
}

func (s *fakeEventStore) Calendars(context.Context) ([]calendarInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]calendarInfo(nil), s.calendars...), nil
}

func (s *fakeEventStore) AuthorizationStatus() accessStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *fakeEventStore) RequestFullAccess(context.Context) (accessStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	if s.requestError == nil {
		s.status = s.requestStatus
	}
	return s.requestStatus, s.requestError
}

func (s *fakeEventStore) Events(_ context.Context, start, end time.Time) ([]calendarEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetches++
	s.start, s.end = start, end
	return append([]calendarEvent(nil), s.events...), s.fetchError
}

type monitorEventStore struct {
	*fakeEventStore
	changes chan struct{}
	closes  int
}

func newMonitorEventStore(status accessStatus, events []calendarEvent) *monitorEventStore {
	return &monitorEventStore{
		fakeEventStore: &fakeEventStore{status: status, events: events},
		changes:        make(chan struct{}, 4),
	}
}

func (s *monitorEventStore) Changes() <-chan struct{} { return s.changes }
func (s *monitorEventStore) Close() error {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()
	return nil
}

func (s *monitorEventStore) setStatus(status accessStatus) {
	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
}

func (s *monitorEventStore) fetchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches
}

func (s *monitorEventStore) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

type fakeURLOpener struct {
	opened []string
	err    error
}

func (o *fakeURLOpener) Open(_ context.Context, rawURL string) error {
	if o.err != nil {
		return o.err
	}
	o.opened = append(o.opened, rawURL)
	return nil
}
