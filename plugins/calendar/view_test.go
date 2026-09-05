package calendar

import (
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestCalendarReminderSceneMatchesFirmwareAssetAndMarqueeContract(t *testing.T) {
	t.Parallel()
	deadline := time.Date(2026, time.August, 27, 17, 5, 0, 0, time.UTC)
	scene := calendarScene(calendarCard{Channel: ChannelUpcoming, State: "STARTS IN", Title: "Planning review with a long descriptive title", CountdownAt: deadline})
	animation := calendarElement(t, scene, "front-calendar")
	if animation.Animation == nil || animation.Animation.Asset.PackagePath != "" || animation.Animation.Asset.StockName != calendarReminderAnimation || !animation.Animation.Loop || animation.X != 0 || animation.Y != 0 {
		t.Fatalf("front reminder animation = %#v", animation)
	}
	title := calendarElement(t, scene, "front-title")
	if title.Text == nil || title.Text.Value != "Planning review with a long descriptive title" || title.Text.Width != 54 || title.X != 18 || title.Y != 0 || title.Text.Marquee == nil || title.Text.Marquee.PixelsPerMinute != 1000 || title.Text.Marquee.StartDelayMilliseconds != 1000 || title.Text.Marquee.RepeatDelayMilliseconds != 2500 {
		t.Fatalf("front title = %#v", title)
	}
	icon := calendarElement(t, scene, "front-timer-icon")
	if icon.Image == nil || icon.Image.Asset.PackagePath != "" || icon.Image.Asset.StockName != calendarReminderIcon || icon.X != 18 || icon.Y != 10 {
		t.Fatalf("front reminder icon = %#v", icon)
	}
	countdown := calendarElement(t, scene, "front-countdown")
	if countdown.Countdown == nil || countdown.Countdown.EndsAtUnixSeconds != deadline.Unix() || countdown.Countdown.ShowHours != protocol.CountdownShowHoursWhenNonZero || countdown.X != 25 || countdown.Y != 9 {
		t.Fatalf("front countdown = %#v", countdown)
	}
	if got := calendarElement(t, scene, "back-title"); got.Text == nil || title.Text == nil || got.Text.Value != title.Text.Value || got.Text.Width != 122 || got.Text.Marquee == nil || title.Text.Marquee == nil || got.Text.Marquee.PixelsPerMinute != title.Text.Marquee.PixelsPerMinute {
		t.Fatalf("back marquee = %#v", got)
	}
	if got := calendarElement(t, scene, "back-action").Text; got == nil || got.Value != "START / OPTIONS" {
		var value string
		if got != nil {
			value = got.Value
		}
		t.Fatalf("back action = %q", value)
	}
}

func TestCalendarActiveSceneUsesEventAnimationAndClock(t *testing.T) {
	t.Parallel()
	scene := calendarScene(calendarCard{Channel: ChannelActive, State: "TIME REMAINING", Title: "Team sync", CountdownAt: time.Now().Add(time.Hour)})
	if got := calendarElement(t, scene, "front-calendar"); got.Animation == nil || got.Animation.Asset.StockName != calendarActiveAnimation {
		t.Fatalf("active animation = %#v", got)
	}
	if got := calendarElement(t, scene, "front-timer-icon"); got.Image == nil || got.Image.Asset.StockName != calendarActiveIcon {
		t.Fatalf("active icon = %#v", got)
	}
}

func TestDefinitionDeclaresAutomaticCalendarChannelsAndQuery(t *testing.T) {
	t.Parallel()
	definition := DefinitionForVersion("9.8.7")
	if definition.ID != PluginID || definition.Version != "9.8.7" {
		t.Fatalf("definition identity = %q/%q", definition.ID, definition.Version)
	}
	if len(definition.Contract.ExecutionModes) != 2 || definition.Contract.ExecutionModes[0] != protocol.ExecutionModeResident || definition.Contract.ExecutionModes[1] != protocol.ExecutionModeInteractive {
		t.Fatalf("execution modes = %v", definition.Contract.ExecutionModes)
	}
	if len(definition.Contract.Channels) != 3 || definition.Contract.Channels[0].ID != ChannelUpcoming || definition.Contract.Channels[1].ID != ChannelActive || definition.Contract.Channels[2].ID != ChannelInteraction {
		t.Fatalf("channels = %v", definition.Contract.Channels)
	}
	if len(definition.Contract.Operations) != 1 || definition.Contract.Operations[0].ID != OperationCalendars || definition.Contract.Operations[0].Kind != protocol.OperationQuery {
		t.Fatalf("operations = %v", definition.Contract.Operations)
	}
}

func calendarElement(t *testing.T, scene protocol.Scene, id string) protocol.Element {
	t.Helper()
	for _, element := range scene.Elements {
		if element.ID == id {
			return element
		}
	}
	t.Fatalf("scene has no element %q: %#v", id, scene)
	return protocol.Element{}
}
