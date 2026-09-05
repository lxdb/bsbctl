package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"
)

const maxAttendanceDecisions = 256

type meetingChoice string

const (
	meetingChoiceJoin   meetingChoice = "join"
	meetingChoiceAttend meetingChoice = "attend"
	meetingChoiceSkip   meetingChoice = "skip"
)

var opaqueEventKeyPattern = regexp.MustCompile(`^event-[0-9a-f]{64}$`)

type accessStatus string

const (
	accessUnknown       accessStatus = "unknown"
	accessNotDetermined accessStatus = "not_determined"
	accessRestricted    accessStatus = "restricted"
	accessDenied        accessStatus = "denied"
	accessWriteOnly     accessStatus = "write_only"
	accessFull          accessStatus = "full_access"
)

var (
	ErrCalendarAccess         = errors.New("full Calendar access is unavailable")
	ErrStaleMeetingAction     = errors.New("meeting action is stale")
	ErrMeetingLinkUnavailable = errors.New("meeting link is unavailable")
)

type eventStore interface {
	AuthorizationStatus() accessStatus
	RequestFullAccess(context.Context) (accessStatus, error)
	Events(context.Context, time.Time, time.Time) ([]calendarEvent, error)
	Calendars(context.Context) ([]calendarInfo, error)
}

type calendarInfo struct {
	ID     string
	Title  string
	Source string
}

type calendarCatalogEntry struct {
	Key       string                    `json:"key"`
	Title     string                    `json:"title"`
	Source    string                    `json:"source"`
	Priority  int                       `json:"priority"`
	Effective calendarEffectiveSettings `json:"effective"`
}

type calendarEffectiveSettings struct {
	Enabled               bool          `json:"enabled"`
	ReminderEnabled       bool          `json:"reminder_enabled"`
	ReminderLeadMinutes   int           `json:"reminder_lead_minutes"`
	ReminderSound         bool          `json:"reminder_sound"`
	ReminderShowEventName bool          `json:"reminder_show_event_name"`
	ActiveEnabled         bool          `json:"active_enabled"`
	ActiveSound           bool          `json:"active_sound"`
	ActiveDisplay         activeDisplay `json:"active_display"`
	ActiveTheme           string        `json:"active_theme"`
}

type urlOpener interface {
	Open(context.Context, string) error
}

type calendarState struct {
	mu            sync.Mutex
	store         eventStore
	opener        urlOpener
	now           func() time.Time
	config        Config
	selected      selectedEvents
	events        []calendarEvent
	decisions     map[string]attendanceDecision
	nextRefresh   time.Duration
	retryWake     chan struct{}
	calendarOrder []string
	calendarInfo  map[string]calendarInfo
}

type calendarRefresh struct {
	selectedEvents
	CheckpointChanged bool
}

func newCalendarStateWithConfig(store eventStore, opener urlOpener, now func() time.Time, config Config) *calendarState {
	if now == nil {
		now = time.Now
	}
	return &calendarState{
		store: store, opener: opener, now: now, config: config, decisions: make(map[string]attendanceDecision),
		calendarInfo: make(map[string]calendarInfo), retryWake: make(chan struct{}, 1),
	}
}

func (s *calendarState) Refresh(ctx context.Context) (calendarRefresh, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked(ctx)
}

func (s *calendarState) refreshLocked(ctx context.Context) (calendarRefresh, error) {
	if s.store == nil {
		s.nextRefresh = defaultEventRefresh
		return calendarRefresh{}, errors.New("Calendar event store is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return calendarRefresh{}, err
	}
	status := s.store.AuthorizationStatus()
	if status == accessNotDetermined {
		var err error
		status, err = s.store.RequestFullAccess(ctx)
		if err != nil {
			s.selected = selectedEvents{}
			s.nextRefresh = defaultEventRefresh
			return calendarRefresh{}, fmt.Errorf("request full Calendar access: %w", err)
		}
	}
	if status != accessFull {
		s.selected = selectedEvents{}
		s.nextRefresh = defaultEventRefresh
		return calendarRefresh{}, fmt.Errorf("%w: %s", ErrCalendarAccess, status)
	}
	now := s.now().UTC()
	events, err := s.store.Events(ctx, now.Add(-24*time.Hour), now.Add(24*time.Hour))
	if err != nil {
		s.selected = selectedEvents{}
		s.nextRefresh = defaultEventRefresh
		return calendarRefresh{}, fmt.Errorf("read Calendar events: %w", err)
	}
	s.events = append(s.events[:0], events...)
	catalogChanged, err := s.refreshCalendarCatalogLocked(ctx, events)
	if err != nil {
		s.selected = selectedEvents{}
		s.nextRefresh = defaultEventRefresh
		return calendarRefresh{}, fmt.Errorf("read EventKit calendars: %w", err)
	}
	decisionsChanged := s.pruneDecisionsLocked(events)
	s.selected = selectEventsWithConfig(events, now, s.config, s.decisions, s.calendarRanksLocked())
	s.nextRefresh = nextEventRefreshWithConfig(events, now, s.config)
	return calendarRefresh{selectedEvents: s.selected, CheckpointChanged: catalogChanged || decisionsChanged}, nil
}

func (s *calendarState) RefreshCatalog(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil {
		return false, errors.New("Calendar event store is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	status := s.store.AuthorizationStatus()
	if status == accessNotDetermined {
		var err error
		status, err = s.store.RequestFullAccess(ctx)
		if err != nil {
			return false, fmt.Errorf("request full Calendar access: %w", err)
		}
	}
	if status != accessFull {
		return false, fmt.Errorf("%w: %s", ErrCalendarAccess, status)
	}
	changed, err := s.refreshCalendarCatalogLocked(ctx, s.events)
	if err != nil {
		return false, fmt.Errorf("read EventKit calendars: %w", err)
	}
	return changed, nil
}

func (s *calendarState) NextRefresh() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextRefresh <= 0 {
		return defaultEventRefresh
	}
	return s.nextRefresh
}

func (s *calendarState) RetryAfter(delay time.Duration) {
	if delay <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextRefresh <= 0 || delay < s.nextRefresh {
		s.nextRefresh = delay
		select {
		case s.retryWake <- struct{}{}:
		default:
		}
	}
}

func (s *calendarState) DecideWithGrant(ctx context.Context, key string, choice meetingChoice, grant func() error) (selectedEvents, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.refreshLocked(ctx)
	if err != nil {
		return selectedEvents{}, err
	}
	var event *calendarEvent
	for index := range s.events {
		if observationKey(s.events[index]) == key {
			copy := s.events[index]
			event = &copy
			break
		}
	}
	if event == nil {
		return selectedEvents{}, ErrStaleMeetingAction
	}
	now := s.now().UTC()
	settings := s.config.SettingsForKey(calendarKey(event.CalendarID))
	actionable := settings.Enabled && ((settings.ReminderEnabled && now.Before(event.Start) && !now.Before(event.Start.Add(-settings.ReminderLead))) ||
		(settings.ActiveEnabled && !now.Before(event.Start) && now.Before(event.End)))
	if !actionable || !now.Before(event.End) {
		return selectedEvents{}, ErrStaleMeetingAction
	}
	if _, exists := s.decisions[key]; !exists && len(s.decisions) >= maxAttendanceDecisions {
		return selectedEvents{}, errors.New("Calendar attendance decision capacity exceeded")
	}
	var joinLink meetingLink
	switch choice {
	case meetingChoiceJoin:
		link, ok := meetingURL(event.URL)
		if !ok {
			return selectedEvents{}, ErrMeetingLinkUnavailable
		}
		if s.opener == nil {
			return selectedEvents{}, errors.New("meeting URL opener is unavailable")
		}
		joinLink = link
	case meetingChoiceAttend, meetingChoiceSkip:
	default:
		return selectedEvents{}, errors.New("unsupported meeting attendance choice")
	}
	if grant != nil {
		if err := grant(); err != nil {
			return selectedEvents{}, err
		}
	}
	if choice == meetingChoiceJoin {
		if err := s.opener.Open(ctx, joinLink.URL); err != nil {
			return selectedEvents{}, fmt.Errorf("open %s meeting: %w", joinLink.Provider, err)
		}
		s.decisions[key] = decisionAttending
	} else if choice == meetingChoiceAttend {
		s.decisions[key] = decisionAttending
	} else {
		s.decisions[key] = decisionSkipped
	}
	s.selected = selectEventsWithConfig(s.events, now, s.config, s.decisions, s.calendarRanksLocked())
	s.nextRefresh = nextEventRefreshWithConfig(s.events, now, s.config)
	return s.selected, nil
}

func (s *calendarState) Choices(key string) ([]meetingChoice, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.events {
		if observationKey(event) != key {
			continue
		}
		choices := make([]meetingChoice, 0, 3)
		if hasMeetingURL(event) {
			choices = append(choices, meetingChoiceJoin)
		}
		choices = append(choices, meetingChoiceAttend, meetingChoiceSkip)
		return choices, true
	}
	return nil, false
}

func (s *calendarState) LauncherCard() *calendarCard {
	s.mu.Lock()
	defer s.mu.Unlock()
	cards := cardsFromSelection(s.selected, s.config)
	for index := range cards {
		if cards[index].Channel == ChannelActive {
			card := cards[index]
			return &card
		}
	}
	if len(cards) == 0 {
		return nil
	}
	card := cards[0]
	return &card
}

type attendanceCheckpoint struct {
	SchemaVersion int                           `json:"schema_version"`
	Decisions     map[string]attendanceDecision `json:"decisions"`
	CalendarOrder []string                      `json:"calendar_order,omitempty"`
}

const attendanceCheckpointSchemaVersion = 1

func (s *calendarState) Checkpoint() (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(attendanceCheckpoint{
		SchemaVersion: attendanceCheckpointSchemaVersion,
		Decisions:     s.decisions, CalendarOrder: s.calendarOrder,
	})
	return json.RawMessage(data), err
}

// RestoreCheckpoint ignores malformed or out-of-contract data and never
// allows persisted state to smuggle event metadata back into the plugin.
func (s *calendarState) RestoreCheckpoint(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var checkpoint attendanceCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil ||
		checkpoint.SchemaVersion != attendanceCheckpointSchemaVersion ||
		len(checkpoint.Decisions) > maxAttendanceDecisions || len(checkpoint.CalendarOrder) > maxCalendarSettings {
		return
	}
	restored := make(map[string]attendanceDecision, len(checkpoint.Decisions))
	for key, decision := range checkpoint.Decisions {
		if !opaqueEventKeyPattern.MatchString(key) || (decision != decisionAttending && decision != decisionSkipped) {
			return
		}
		restored[key] = decision
	}
	seenCalendars := make(map[string]struct{}, len(checkpoint.CalendarOrder))
	for _, key := range checkpoint.CalendarOrder {
		if !calendarKeyPattern.MatchString(key) {
			return
		}
		if _, exists := seenCalendars[key]; exists {
			return
		}
		seenCalendars[key] = struct{}{}
	}
	s.mu.Lock()
	s.decisions = restored
	s.calendarOrder = slices.Clone(checkpoint.CalendarOrder)
	s.mu.Unlock()
}

func (s *calendarState) refreshCalendarCatalogLocked(ctx context.Context, events []calendarEvent) (bool, error) {
	infos := make([]calendarInfo, 0)
	var err error
	infos, err = s.store.Calendars(ctx)
	if err != nil {
		return false, err
	}
	if len(infos) == 0 {
		seen := make(map[string]struct{})
		for _, event := range events {
			if event.CalendarID == "" {
				continue
			}
			if _, exists := seen[event.CalendarID]; exists {
				continue
			}
			seen[event.CalendarID] = struct{}{}
			infos = append(infos, calendarInfo{ID: event.CalendarID})
		}
	}
	if len(infos) > maxCalendarSettings {
		return false, fmt.Errorf("Calendar catalog exceeds %d entries", maxCalendarSettings)
	}
	present := make(map[string]calendarInfo, len(infos))
	for _, info := range infos {
		if info.ID == "" {
			continue
		}
		key := calendarKey(info.ID)
		present[key] = info
	}
	order := make([]string, 0, len(present))
	known := make(map[string]struct{}, len(present))
	for _, key := range s.calendarOrder {
		if _, exists := present[key]; exists {
			order = append(order, key)
			known[key] = struct{}{}
		}
	}
	for _, info := range infos {
		if info.ID == "" {
			continue
		}
		key := calendarKey(info.ID)
		if _, exists := known[key]; exists {
			continue
		}
		order = append(order, key)
		known[key] = struct{}{}
	}
	changed := !slices.Equal(s.calendarOrder, order)
	s.calendarOrder, s.calendarInfo = order, present
	return changed, nil
}

func (s *calendarState) calendarRanksLocked() map[string]int {
	ranks := make(map[string]int, len(s.calendarOrder))
	for index, key := range s.calendarOrder {
		ranks[key] = index + 1
	}
	return ranks
}

func (s *calendarState) CalendarCatalog() []calendarCatalogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]calendarCatalogEntry, 0, len(s.calendarOrder))
	for index := len(s.calendarOrder) - 1; index >= 0; index-- {
		key := s.calendarOrder[index]
		info := s.calendarInfo[key]
		settings := s.config.SettingsForKey(key)
		result = append(result, calendarCatalogEntry{
			Key: key, Title: info.Title, Source: info.Source, Priority: index + 1,
			Effective: calendarEffectiveSettings{
				Enabled: settings.Enabled, ReminderEnabled: settings.ReminderEnabled,
				ReminderLeadMinutes: int(settings.ReminderLead / time.Minute), ReminderSound: settings.ReminderSound,
				ReminderShowEventName: settings.ReminderShowEventName, ActiveEnabled: settings.ActiveEnabled,
				ActiveSound: settings.ActiveSound, ActiveDisplay: settings.ActiveDisplay, ActiveTheme: settings.ActiveTheme,
			},
		})
	}
	return result
}

func (s *calendarState) pruneDecisionsLocked(events []calendarEvent) bool {
	present := make(map[string]struct{}, len(events))
	for _, event := range events {
		present[observationKey(event)] = struct{}{}
	}
	changed := false
	for key := range s.decisions {
		if _, ok := present[key]; !ok {
			delete(s.decisions, key)
			changed = true
		}
	}
	return changed
}
