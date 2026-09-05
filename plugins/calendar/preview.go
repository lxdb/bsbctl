//go:build preview

package calendar

import (
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

// PreviewScenes returns deterministic presentations built exclusively from
// public-safe bsbctl examples. It does not access Calendar or plugin configuration.
func PreviewScenes(now time.Time) []protocol.Scene {
	scenes := []protocol.Scene{
		calendarScene(calendarCard{
			Channel:     ChannelUpcoming,
			State:       "STARTS IN",
			Title:       "BSBCTL RELEASE REVIEW",
			CountdownAt: now.Add(5 * time.Minute),
		}),
		calendarScene(calendarCard{
			Channel:     ChannelActive,
			State:       "TIME REMAINING",
			Title:       "BSBCTL PREVIEW CAPTURE",
			CountdownAt: now.Add(45 * time.Minute),
		}),
	}
	session := &calendarInteractionSession{choices: []meetingChoice{
		meetingChoiceJoin,
		meetingChoiceAttend,
		meetingChoiceSkip,
	}}
	for index := range session.choices {
		session.index = index
		scenes = append(scenes, meetingInteractionScene(session))
	}
	return scenes
}
