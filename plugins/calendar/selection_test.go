package calendar

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSelectEventsUsesEarliestUpcomingAndMostRecentlyStartedActive(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	events := []calendarEvent{
		{CalendarID: "work", EventID: "active-old", Title: "Old active", Start: now.Add(-45 * time.Minute), End: now.Add(15 * time.Minute)},
		{CalendarID: "work", EventID: "active-new", Title: "New active", Start: now.Add(-10 * time.Minute), End: now.Add(20 * time.Minute)},
		{CalendarID: "work", EventID: "upcoming-later", Title: "Later", Start: now.Add(4 * time.Minute), End: now.Add(34 * time.Minute)},
		{CalendarID: "work", EventID: "upcoming-next", Title: "Next", Start: now.Add(2 * time.Minute), End: now.Add(32 * time.Minute)},
	}

	selected := selectEventsWithConfig(events, now, testSelectionConfig(5*time.Minute), nil, nil)
	if selected.Upcoming == nil || selected.Upcoming.EventID != "upcoming-next" {
		t.Fatalf("upcoming = %#v, want upcoming-next", selected.Upcoming)
	}
	if selected.Active == nil || selected.Active.EventID != "active-new" {
		t.Fatalf("active = %#v, want active-new", selected.Active)
	}
}

func TestSelectEventsIsScheduleDrivenAndHonorsExplicitSkip(t *testing.T) {
	start := time.Date(2026, time.August, 24, 18, 0, 0, 0, time.UTC)
	event := calendarEvent{CalendarID: "work", EventID: "meeting", Start: start, End: start.Add(30 * time.Minute)}
	key := observationKey(event)

	config := testSelectionConfig(5 * time.Minute)
	before := selectEventsWithConfig([]calendarEvent{event}, start.Add(-2*time.Minute), config, nil, nil)
	if before.Upcoming == nil || before.Active != nil {
		t.Fatalf("undecided before start = %#v", before)
	}
	attending := selectEventsWithConfig([]calendarEvent{event}, start.Add(-2*time.Minute), config, map[string]attendanceDecision{key: decisionAttending}, nil)
	if attending.Upcoming == nil || attending.Active != nil {
		t.Fatalf("attending before start = %#v", attending)
	}
	activeWithoutAttendance := selectEventsWithConfig([]calendarEvent{event}, start.Add(20*time.Minute), config, nil, nil)
	if activeWithoutAttendance.Active == nil || activeWithoutAttendance.Upcoming != nil {
		t.Fatalf("scheduled active event = %#v", activeWithoutAttendance)
	}
	skipped := selectEventsWithConfig([]calendarEvent{event}, start.Add(5*time.Minute), config, map[string]attendanceDecision{key: decisionSkipped}, nil)
	if skipped.Upcoming != nil || skipped.Active != nil {
		t.Fatalf("skipped meeting = %#v", skipped)
	}
}

func TestSelectEventsExcludesEventsOutsideTheCalendarContract(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		event calendarEvent
	}{
		{name: "empty final event id after fallback", event: calendarEvent{CalendarID: "work", Start: now, End: now.Add(time.Hour)}},
		{name: "empty calendar id", event: calendarEvent{EventID: "event", Start: now, End: now.Add(time.Hour)}},
		{name: "all day", event: calendarEvent{CalendarID: "work", EventID: "all-day", AllDay: true, Start: now, End: now.Add(23 * time.Hour)}},
		{name: "cancelled", event: calendarEvent{CalendarID: "work", EventID: "cancelled", Cancelled: true, Start: now, End: now.Add(time.Hour)}},
		{name: "zero duration", event: calendarEvent{CalendarID: "work", EventID: "zero", Start: now, End: now}},
		{name: "negative duration", event: calendarEvent{CalendarID: "work", EventID: "negative", Start: now, End: now.Add(-time.Minute)}},
		{name: "twenty four hours", event: calendarEvent{CalendarID: "work", EventID: "long", Start: now, End: now.Add(24 * time.Hour)}},
		{name: "already ended", event: calendarEvent{CalendarID: "work", EventID: "ended", Start: now.Add(-time.Hour), End: now.Add(-time.Second)}},
		{name: "outside lead", event: calendarEvent{CalendarID: "work", EventID: "future", Start: now.Add(6 * time.Minute), End: now.Add(time.Hour)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected := selectEventsWithConfig([]calendarEvent{test.event}, now, testSelectionConfig(5*time.Minute), nil, nil)
			if selected.Upcoming != nil || selected.Active != nil {
				t.Fatalf("excluded event was selected: %#v", selected)
			}
		})
	}
}

func TestSelectEventsDoesNotPreferAJoinLinkOverAnEarlierMeeting(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	events := []calendarEvent{
		{CalendarID: "work", EventID: "without-link", Start: now.Add(time.Minute), End: now.Add(30 * time.Minute)},
		{CalendarID: "work", EventID: "with-link", URL: "https://meet.google.com/abc-defg-hij", Start: now.Add(2 * time.Minute), End: now.Add(32 * time.Minute)},
	}

	selected := selectEventsWithConfig(events, now, testSelectionConfig(5*time.Minute), nil, nil)
	if selected.Upcoming == nil || selected.Upcoming.EventID != "without-link" {
		t.Fatalf("upcoming = %#v, want event without link", selected.Upcoming)
	}
}

func TestSelectEventsUsesStableSourceIdentityForTies(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	start := now.Add(2 * time.Minute)
	events := []calendarEvent{
		{CalendarID: "z-calendar", EventID: "event", Start: start, End: start.Add(time.Hour)},
		{CalendarID: "a-calendar", EventID: "event", Start: start, End: start.Add(time.Hour)},
	}

	selected := selectEventsWithConfig(events, now, testSelectionConfig(5*time.Minute), nil, nil)
	if selected.Upcoming == nil || selected.Upcoming.CalendarID != "a-calendar" {
		t.Fatalf("upcoming = %#v, want a-calendar tie winner", selected.Upcoming)
	}
}

func TestObservationKeyIsOpaqueAndOccurrenceSpecific(t *testing.T) {
	start := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	first := calendarEvent{CalendarID: "private-work-calendar", EventID: "secret-event-id", Start: start, End: start.Add(time.Hour)}
	second := first
	second.Start = start.Add(7 * 24 * time.Hour)
	second.End = second.Start.Add(time.Hour)

	firstKey := observationKey(first)
	secondKey := observationKey(second)
	if firstKey == secondKey {
		t.Fatal("recurring occurrences shared one observation key")
	}
	if !regexp.MustCompile(`^event-[0-9a-f]{64}$`).MatchString(firstKey) {
		t.Fatalf("observation key %q is not an opaque SHA-256 key", firstKey)
	}
	if strings.Contains(firstKey, first.CalendarID) || strings.Contains(firstKey, first.EventID) {
		t.Fatalf("observation key %q exposed EventKit identity", firstKey)
	}
}

func TestDisplayTitleProducesOneBoundedSafeLine(t *testing.T) {
	if got := displayTitle("  Plan\nReview\tNow  "); got != "Plan Review Now" {
		t.Fatalf("sanitized title = %q", got)
	}
	if got := displayTitle(" \n\t "); got != "Untitled event" {
		t.Fatalf("empty title = %q", got)
	}
	long := strings.Repeat("a", 100)
	if got := displayTitle(long); got != strings.Repeat("a", 93)+"..." {
		t.Fatalf("bounded title has %d bytes, want 96", len(got))
	}
}

func TestNextEventRefreshUsesTheEarliestBoundaryWithASafetyCap(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		events []calendarEvent
		want   time.Duration
	}{
		{name: "no events", want: 15 * time.Minute},
		{name: "safety cap", events: []calendarEvent{{CalendarID: "work", EventID: "future", Start: now.Add(30 * time.Minute), End: now.Add(time.Hour)}}, want: 15 * time.Minute},
		{name: "lead boundary", events: []calendarEvent{{CalendarID: "work", EventID: "lead", Start: now.Add(10 * time.Minute), End: now.Add(time.Hour)}}, want: 5 * time.Minute},
		{name: "upcoming start", events: []calendarEvent{{CalendarID: "work", EventID: "upcoming", Start: now.Add(2 * time.Minute), End: now.Add(time.Hour)}}, want: 2 * time.Minute},
		{name: "active end", events: []calendarEvent{{CalendarID: "work", EventID: "active", Start: now.Add(-time.Minute), End: now.Add(3 * time.Minute)}}, want: 3 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := nextEventRefreshWithConfig(test.events, now, testSelectionConfig(5*time.Minute)); got != test.want {
				t.Fatalf("refresh delay = %s, want %s", got, test.want)
			}
		})
	}
}

func testSelectionConfig(reminderLead time.Duration) Config {
	config := defaultCalendarConfig()
	config.ReminderLead = reminderLead
	return config
}
