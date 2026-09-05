//go:build preview

package calendar

import (
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestPreviewScenesShowCalendarTimingAndEventOptions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	scenes := PreviewScenes(now)
	if len(scenes) != 5 {
		t.Fatalf("preview scenes = %d, want upcoming, active, and three event options", len(scenes))
	}

	wantFront := []string{"BSBCTL RELEASE REVIEW", "BSBCTL PREVIEW CAPTURE", "JOIN", "ATTEND", "SKIP"}
	wantIDs := []string{"front-title", "front-title", "front-choice", "front-choice", "front-choice"}
	for index, want := range wantFront {
		if got := calendarPreviewText(scenes[index], wantIDs[index]); got != want {
			t.Fatalf("scene %d %s = %q, want %q", index, wantIDs[index], got, want)
		}
	}
	if got := calendarPreviewText(scenes[0], "back-phase"); got != "STARTS IN" {
		t.Fatalf("upcoming phase = %q, want STARTS IN", got)
	}
	if got := calendarPreviewText(scenes[1], "back-phase"); got != "TIME REMAINING" {
		t.Fatalf("active phase = %q, want TIME REMAINING", got)
	}
	assertCalendarCountdown(t, scenes[0], now.Add(5*time.Minute))
	assertCalendarCountdown(t, scenes[1], now.Add(45*time.Minute))
	if got := calendarPreviewText(scenes[2], "back-title"); got != "EVENT OPTIONS" {
		t.Fatalf("event options title = %q, want production interaction", got)
	}
}

func assertCalendarCountdown(t *testing.T, scene protocol.Scene, want time.Time) {
	t.Helper()
	found := false
	for _, element := range scene.Elements {
		if element.Countdown == nil {
			continue
		}
		found = true
		if element.Countdown.EndsAtUnixSeconds != want.Unix() {
			t.Fatalf("countdown = %d, want %d", element.Countdown.EndsAtUnixSeconds, want.Unix())
		}
	}
	if !found {
		t.Fatal("preview countdown is missing")
	}
}

func calendarPreviewText(scene protocol.Scene, id string) string {
	for _, element := range scene.Elements {
		if element.ID == id && element.Text != nil {
			return element.Text.Value
		}
	}
	return ""
}
