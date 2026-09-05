package calendar

import (
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	PluginID           = "dev.bsbctl.calendar"
	PluginVersion      = "0.1.0"
	AppID              = "calendar"
	ChannelUpcoming    = "upcoming"
	ChannelActive      = "active"
	ChannelInteraction = "interaction"
	OperationCalendars = "calendars"
	CalendarOpenAction = "open"

	calendarReminderAnimation = "calendar_reminder_16x16.anim"
	calendarActiveAnimation   = "calendar_event_16x16.anim"
	calendarReminderIcon      = "hourglass_5x5.image"
	calendarActiveIcon        = "clock_5x5.image"
	calendarReminderSound     = "calendar_reminder_ends.snd"
	calendarActiveSound       = "calendar_event_starts.snd"
	calendarSoundWindow       = 15 * time.Second
)

type calendarCard struct {
	Channel     string
	Key         string
	State       string
	Title       string
	ObservedAt  time.Time
	CountdownAt time.Time
	ValidUntil  time.Time
	Disposition protocol.Disposition
	Impact      protocol.Impact
	ReasonCode  string
	BusyTimer   *protocol.BusyTimerPresentation
	AudioCue    *protocol.AudioCue
}

func cardsFromSelection(selected selectedEvents, config Config) []calendarCard {
	cards := make([]calendarCard, 0, 2)
	if selected.Upcoming != nil {
		settings := config.SettingsForKey(calendarKey(selected.Upcoming.CalendarID))
		if settings.Enabled && settings.ReminderEnabled {
			title := configuredTitle(*selected.Upcoming, settings.ReminderShowEventName)
			observedAt := selected.Upcoming.Start.Add(-settings.ReminderLead)
			card := calendarCard{
				Channel: ChannelUpcoming, Key: observationKey(*selected.Upcoming), State: "STARTS IN", Title: title,
				ObservedAt: observedAt, CountdownAt: selected.Upcoming.Start, ValidUntil: selected.Upcoming.Start,
				Disposition: protocol.DispositionActionable,
				Impact:      protocol.ImpactNotable, ReasonCode: "calendar_reminder",
			}
			if settings.ReminderSound {
				card.AudioCue = calendarAudioCue("calendar-reminder", *selected.Upcoming, calendarReminderSound, observedAt, card.ValidUntil)
			}
			cards = append(cards, card)
		}
	}
	if selected.Active != nil {
		settings := config.SettingsForKey(calendarKey(selected.Active.CalendarID))
		if settings.Enabled && settings.ActiveEnabled {
			card := calendarCard{
				Channel: ChannelActive, Key: observationKey(*selected.Active), State: "TIME REMAINING",
				Title:      configuredTitle(*selected.Active, settings.ReminderShowEventName),
				ObservedAt: selected.Active.Start, CountdownAt: selected.Active.End, ValidUntil: selected.Active.End,
				Disposition: protocol.DispositionActionable,
				Impact:      protocol.ImpactNotable, ReasonCode: "calendar_active",
			}
			if settings.ActiveDisplay == activeDisplayTheme {
				card.BusyTimer = &protocol.BusyTimerPresentation{Theme: settings.ActiveTheme}
			}
			if settings.ActiveSound {
				card.AudioCue = calendarAudioCue("calendar-active", *selected.Active, calendarActiveSound, selected.Active.Start, card.ValidUntil)
			}
			cards = append(cards, card)
		}
	}
	return cards
}

func configuredTitle(event calendarEvent, show bool) string {
	if !show {
		return "CALENDAR EVENT"
	}
	return displayTitle(event.Title)
}

func calendarAudioCue(prefix string, event calendarEvent, assetID string, enteredAt, validUntil time.Time) *protocol.AudioCue {
	expiresAt := enteredAt.Add(calendarSoundWindow)
	if validUntil.Before(expiresAt) {
		expiresAt = validUntil
	}
	return &protocol.AudioCue{ID: prefix + ":" + observationKey(event), Asset: protocol.AssetRef{StockName: assetID}, ExpiresAt: expiresAt}
}

func hasMeetingURL(event calendarEvent) bool {
	_, ok := meetingURL(event.URL)
	return ok
}
