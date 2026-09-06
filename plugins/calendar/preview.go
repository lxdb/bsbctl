//go:build preview

package calendar

import (
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

// PreviewScenes returns deterministic presentations derived from public-safe
// events through production selection, card, and interaction logic. It does
// not access Calendar or plugin configuration.
func PreviewScenes(now time.Time) []protocol.Scene {
	now = now.UTC()
	config := defaultCalendarConfig()
	events := []calendarEvent{
		{CalendarID: "preview", EventID: "release-review", Title: "BSBCTL RELEASE REVIEW", URL: "https://meet.google.com/abc-defg-hij", Start: now.Add(5 * time.Minute), End: now.Add(35 * time.Minute)},
		{CalendarID: "preview", EventID: "preview-capture", Title: "BSBCTL PREVIEW CAPTURE", Start: now.Add(-15 * time.Minute), End: now.Add(45 * time.Minute)},
	}
	selected := selectEventsWithConfig(events, now, config, map[string]attendanceDecision{}, nil)
	cards := cardsFromSelection(selected, config)
	scenes := make([]protocol.Scene, 0, len(cards)+3)
	for _, card := range cards {
		scenes = append(scenes, calendarScene(card))
	}
	state := newCalendarStateWithConfig(nil, nil, func() time.Time { return now }, config)
	state.events = events
	choices, ok := state.Choices(observationKey(events[0]))
	if !ok {
		return scenes
	}
	session := &calendarInteractionSession{choices: choices}
	for index := range session.choices {
		session.index = index
		scenes = append(scenes, meetingInteractionScene(session))
	}
	return scenes
}
