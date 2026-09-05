package calendar

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const defaultEventRefresh = 15 * time.Minute

type attendanceDecision string

const (
	decisionAttending attendanceDecision = "attending"
	decisionSkipped   attendanceDecision = "skipped"
)

type calendarEvent struct {
	CalendarID   string
	EventID      string
	Title        string
	URL          string
	Start        time.Time
	End          time.Time
	OccurrenceAt time.Time
	AllDay       bool
	Cancelled    bool
}

type selectedEvents struct {
	Upcoming *calendarEvent
	Active   *calendarEvent
}

func selectEventsWithConfig(events []calendarEvent, now time.Time, config Config, decisions map[string]attendanceDecision, calendarRanks map[string]int) selectedEvents {
	var selected selectedEvents
	for _, event := range events {
		if !eligibleEvent(event, now) || decisions[observationKey(event)] == decisionSkipped {
			continue
		}
		settings := config.SettingsForKey(calendarKey(event.CalendarID))
		if !settings.Enabled {
			continue
		}
		if !event.Start.After(now) && event.End.After(now) && settings.ActiveEnabled {
			selectLaterStartRanked(&selected.Active, event, calendarRanks)
			continue
		}
		if settings.ReminderEnabled && event.Start.After(now) && !event.Start.Add(-settings.ReminderLead).After(now) {
			selectEarlierRanked(&selected.Upcoming, event, calendarRanks)
		}
	}
	return selected
}

func selectEarlierRanked(target **calendarEvent, event calendarEvent, ranks map[string]int) {
	if *target == nil || event.Start.Before((*target).Start) ||
		(event.Start.Equal((*target).Start) && eventWinsCalendarTie(event, **target, ranks)) {
		copy := event
		*target = &copy
	}
}

func selectLaterStartRanked(target **calendarEvent, event calendarEvent, ranks map[string]int) {
	if *target == nil || event.Start.After((*target).Start) ||
		(event.Start.Equal((*target).Start) && eventWinsCalendarTie(event, **target, ranks)) {
		copy := event
		*target = &copy
	}
}

func eventWinsCalendarTie(candidate, current calendarEvent, ranks map[string]int) bool {
	candidateRank := ranks[calendarKey(candidate.CalendarID)]
	currentRank := ranks[calendarKey(current.CalendarID)]
	if candidateRank != currentRank {
		return candidateRank > currentRank
	}
	return sourceIdentity(candidate) < sourceIdentity(current)
}

func eligibleEvent(event calendarEvent, now time.Time) bool {
	duration := event.End.Sub(event.Start)
	return event.CalendarID != "" && event.EventID != "" && !event.AllDay && !event.Cancelled && duration > 0 && duration < 24*time.Hour && event.End.After(now)
}

func nextEventRefreshWithConfig(events []calendarEvent, now time.Time, config Config) time.Duration {
	next := now.Add(defaultEventRefresh)
	for _, event := range events {
		if !eligibleEvent(event, now) {
			continue
		}
		settings := config.SettingsForKey(calendarKey(event.CalendarID))
		if !settings.Enabled {
			continue
		}
		boundaries := make([]time.Time, 0, 3)
		if settings.ReminderEnabled {
			boundaries = append(boundaries, event.Start.Add(-settings.ReminderLead), event.Start)
		} else if settings.ActiveEnabled {
			boundaries = append(boundaries, event.Start)
		}
		if settings.ActiveEnabled {
			boundaries = append(boundaries, event.End)
		}
		for _, boundary := range boundaries {
			if boundary.After(now) && boundary.Before(next) {
				next = boundary
			}
		}
	}
	return next.Sub(now)
}

func sourceIdentity(event calendarEvent) string {
	occurrence := event.OccurrenceAt
	if occurrence.IsZero() {
		occurrence = event.Start
	}
	return event.CalendarID + "\x00" + event.EventID + "\x00" + strconv.FormatInt(occurrence.UnixNano(), 10)
}

func observationKey(event calendarEvent) string {
	digest := sha256.Sum256([]byte(sourceIdentity(event)))
	return "event-" + hex.EncodeToString(digest[:])
}

func calendarKey(calendarID string) string {
	digest := sha256.Sum256([]byte(calendarID))
	return "calendar-" + hex.EncodeToString(digest[:])
}

func displayTitle(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "Untitled event"
	}
	const maxBytes = 96
	if len(value) <= maxBytes {
		return value
	}
	const ellipsis = "..."
	limit := maxBytes - len(ellipsis)
	end := 0
	for index := range value {
		if index > limit {
			break
		}
		end = index
	}
	if end == 0 {
		end = limit
	}
	return value[:end] + ellipsis
}
