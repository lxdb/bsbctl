package calendar

import (
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestCardsFromSelectionBuildsAutomaticReminderAndEventNameActiveContracts(t *testing.T) {
	now := time.Date(2026, time.August, 27, 17, 0, 0, 0, time.UTC)
	upcoming := calendarEvent{CalendarID: "work", EventID: "next", Title: "Planning review", URL: "https://meet.google.com/abc-defg-hij", Start: now.Add(2 * time.Minute), End: now.Add(time.Hour)}
	active := calendarEvent{CalendarID: "work", EventID: "active", Title: "Current meeting", URL: "https://zoom.us/j/123", Start: now.Add(-time.Minute), End: now.Add(20 * time.Minute)}
	config, err := decodeConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	cards := cardsFromSelection(selectedEvents{Upcoming: &upcoming, Active: &active}, config)
	if len(cards) != 2 {
		t.Fatalf("cards = %#v", cards)
	}
	reminder := cards[0]
	if reminder.Channel != ChannelUpcoming || reminder.Key != observationKey(upcoming) || reminder.State != "STARTS IN" {
		t.Fatalf("reminder card = %#v", reminder)
	}
	if reminder.ObservedAt != upcoming.Start.Add(-5*time.Minute) || reminder.ValidUntil != upcoming.Start || reminder.Disposition != protocol.DispositionActionable || reminder.ReasonCode != "calendar_reminder" {
		t.Fatalf("reminder behavior = %#v", reminder)
	}
	if reminder.AudioCue == nil || reminder.AudioCue.Asset.StockName != calendarReminderSound || reminder.AudioCue.Asset.PackagePath != "" || reminder.AudioCue.ExpiresAt != reminder.ObservedAt.Add(calendarSoundWindow) {
		t.Fatalf("reminder audio = %#v", reminder.AudioCue)
	}
	activeCard := cards[1]
	if activeCard.Channel != ChannelActive || activeCard.Key != observationKey(active) || activeCard.State != "TIME REMAINING" || activeCard.BusyTimer != nil {
		t.Fatalf("active card = %#v", activeCard)
	}
	if activeCard.ValidUntil != active.End || activeCard.Disposition != protocol.DispositionActionable || activeCard.ReasonCode != "calendar_active" {
		t.Fatalf("active behavior = %#v", activeCard)
	}
	if activeCard.AudioCue == nil || activeCard.AudioCue.Asset.StockName != calendarActiveSound || activeCard.AudioCue.Asset.PackagePath != "" || activeCard.AudioCue.ExpiresAt != active.Start.Add(calendarSoundWindow) {
		t.Fatalf("active audio = %#v", activeCard.AudioCue)
	}
}

func TestCardsFromSelectionSupportsPrivateReminderAndNativeThemeActiveMode(t *testing.T) {
	now := time.Date(2026, time.August, 27, 17, 0, 0, 0, time.UTC)
	event := calendarEvent{CalendarID: "private", EventID: "event", Title: "Confidential acquisition", Start: now.Add(-time.Minute), End: now.Add(20 * time.Minute)}
	key := calendarKey(event.CalendarID)
	raw := `{"calendars":[{"key":"` + key + `","reminder_show_event_name":false,"active_display":"theme","active_theme":"on_air","active_sound":false}]}`
	config, err := decodeConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	cards := cardsFromSelection(selectedEvents{Active: &event}, config)
	if len(cards) != 1 || cards[0].Title != "CALENDAR EVENT" || cards[0].BusyTimer == nil || cards[0].BusyTimer.Theme != "on_air" || cards[0].AudioCue != nil {
		t.Fatalf("private theme card = %#v", cards)
	}
}
