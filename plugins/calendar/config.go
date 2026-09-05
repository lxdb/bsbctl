package calendar

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	protocoljson "github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	maxConfigBytes      = 64 * 1024
	maxCalendarSettings = 256
)

type activeDisplay string

const (
	activeDisplayEventName activeDisplay = "event_name"
	activeDisplayTheme     activeDisplay = "theme"
)

var calendarKeyPattern = regexp.MustCompile(`^calendar-[0-9a-f]{64}$`)
var calendarThemePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type Config struct {
	ReminderEnabled       bool
	ReminderLead          time.Duration
	ReminderSound         bool
	ReminderShowEventName bool
	ActiveEnabled         bool
	ActiveSound           bool
	ActiveDisplay         activeDisplay
	ActiveTheme           string
	CalendarOverrides     map[string]calendarOverride
}

type configJSON struct {
	ReminderEnabled       *bool                  `json:"reminder_enabled,omitempty"`
	ReminderLeadMinutes   *int                   `json:"reminder_lead_minutes,omitempty"`
	ReminderSound         *bool                  `json:"reminder_sound,omitempty"`
	ReminderShowEventName *bool                  `json:"reminder_show_event_name,omitempty"`
	ActiveEnabled         *bool                  `json:"active_enabled,omitempty"`
	ActiveSound           *bool                  `json:"active_sound,omitempty"`
	ActiveDisplay         *activeDisplay         `json:"active_display,omitempty"`
	ActiveTheme           *string                `json:"active_theme,omitempty"`
	Calendars             []calendarOverrideJSON `json:"calendars,omitempty"`
}

type calendarOverrideJSON struct {
	Key                   string         `json:"key"`
	Enabled               *bool          `json:"enabled,omitempty"`
	ReminderEnabled       *bool          `json:"reminder_enabled,omitempty"`
	ReminderLeadMinutes   *int           `json:"reminder_lead_minutes,omitempty"`
	ReminderSound         *bool          `json:"reminder_sound,omitempty"`
	ReminderShowEventName *bool          `json:"reminder_show_event_name,omitempty"`
	ActiveEnabled         *bool          `json:"active_enabled,omitempty"`
	ActiveSound           *bool          `json:"active_sound,omitempty"`
	ActiveDisplay         *activeDisplay `json:"active_display,omitempty"`
	ActiveTheme           *string        `json:"active_theme,omitempty"`
}

type calendarOverride struct {
	Enabled               *bool
	ReminderEnabled       *bool
	ReminderLead          *time.Duration
	ReminderSound         *bool
	ReminderShowEventName *bool
	ActiveEnabled         *bool
	ActiveSound           *bool
	ActiveDisplay         *activeDisplay
	ActiveTheme           *string
}

type calendarSettings struct {
	Enabled               bool
	ReminderEnabled       bool
	ReminderLead          time.Duration
	ReminderSound         bool
	ReminderShowEventName bool
	ActiveEnabled         bool
	ActiveSound           bool
	ActiveDisplay         activeDisplay
	ActiveTheme           string
}

func decodeConfig(raw json.RawMessage) (Config, error) {
	if len(raw) == 0 || len(raw) > maxConfigBytes {
		return Config{}, fmt.Errorf("Calendar configuration must contain between 1 and %d bytes", maxConfigBytes)
	}
	var value configJSON
	if err := protocoljson.DecodeStrict(raw, &value); err != nil {
		return Config{}, fmt.Errorf("decode Calendar configuration: %w", err)
	}
	config := defaultCalendarConfig()
	config.CalendarOverrides = make(map[string]calendarOverride, len(value.Calendars))
	applyBool(&config.ReminderEnabled, value.ReminderEnabled)
	applyBool(&config.ReminderSound, value.ReminderSound)
	applyBool(&config.ReminderShowEventName, value.ReminderShowEventName)
	applyBool(&config.ActiveEnabled, value.ActiveEnabled)
	applyBool(&config.ActiveSound, value.ActiveSound)
	if value.ReminderLeadMinutes != nil {
		lead, err := calendarLead(*value.ReminderLeadMinutes)
		if err != nil {
			return Config{}, err
		}
		config.ReminderLead = lead
	}
	if value.ActiveDisplay != nil {
		config.ActiveDisplay = *value.ActiveDisplay
	}
	if value.ActiveTheme != nil {
		config.ActiveTheme = *value.ActiveTheme
	}
	if err := validateActive(config.ActiveDisplay, config.ActiveTheme); err != nil {
		return Config{}, err
	}
	if len(value.Calendars) > maxCalendarSettings {
		return Config{}, fmt.Errorf("calendars must contain at most %d entries", maxCalendarSettings)
	}
	for _, item := range value.Calendars {
		if !calendarKeyPattern.MatchString(item.Key) {
			return Config{}, fmt.Errorf("calendar key %q must be an opaque calendar SHA-256 key", item.Key)
		}
		if _, exists := config.CalendarOverrides[item.Key]; exists {
			return Config{}, fmt.Errorf("calendar key %q is duplicated", item.Key)
		}
		override := calendarOverride{
			Enabled: item.Enabled, ReminderEnabled: item.ReminderEnabled, ReminderSound: item.ReminderSound,
			ReminderShowEventName: item.ReminderShowEventName, ActiveEnabled: item.ActiveEnabled,
			ActiveSound: item.ActiveSound, ActiveDisplay: item.ActiveDisplay, ActiveTheme: item.ActiveTheme,
		}
		if item.ReminderLeadMinutes != nil {
			lead, err := calendarLead(*item.ReminderLeadMinutes)
			if err != nil {
				return Config{}, fmt.Errorf("calendar %q: %w", item.Key, err)
			}
			override.ReminderLead = &lead
		}
		settings := config.settingsWithOverride(override)
		if err := validateActive(settings.ActiveDisplay, settings.ActiveTheme); err != nil {
			return Config{}, fmt.Errorf("calendar %q: %w", item.Key, err)
		}
		config.CalendarOverrides[item.Key] = override
	}
	return config, nil
}

func defaultCalendarConfig() Config {
	return Config{
		ReminderEnabled: true, ReminderLead: 5 * time.Minute, ReminderSound: true, ReminderShowEventName: true,
		ActiveEnabled: true, ActiveSound: true, ActiveDisplay: activeDisplayEventName, ActiveTheme: "meeting",
		CalendarOverrides: make(map[string]calendarOverride),
	}
}

func calendarLead(minutes int) (time.Duration, error) {
	if minutes < 1 || minutes > 60 {
		return 0, errors.New("reminder_lead_minutes must be between 1 and 60")
	}
	return time.Duration(minutes) * time.Minute, nil
}

func validateActive(display activeDisplay, theme string) error {
	if display != activeDisplayEventName && display != activeDisplayTheme {
		return fmt.Errorf("active_display must be %q or %q", activeDisplayEventName, activeDisplayTheme)
	}
	if !calendarThemePattern.MatchString(theme) {
		return errors.New("active_theme must be a safe lowercase theme identifier")
	}
	return nil
}

func applyBool(target *bool, value *bool) {
	if value != nil {
		*target = *value
	}
}

func (c Config) SettingsForKey(key string) calendarSettings {
	return c.settingsWithOverride(c.CalendarOverrides[key])
}

func (c Config) settingsWithOverride(value calendarOverride) calendarSettings {
	settings := calendarSettings{
		Enabled: true, ReminderEnabled: c.ReminderEnabled, ReminderLead: c.ReminderLead,
		ReminderSound: c.ReminderSound, ReminderShowEventName: c.ReminderShowEventName,
		ActiveEnabled: c.ActiveEnabled, ActiveSound: c.ActiveSound,
		ActiveDisplay: c.ActiveDisplay, ActiveTheme: c.ActiveTheme,
	}
	applyBool(&settings.Enabled, value.Enabled)
	applyBool(&settings.ReminderEnabled, value.ReminderEnabled)
	applyBool(&settings.ReminderSound, value.ReminderSound)
	applyBool(&settings.ReminderShowEventName, value.ReminderShowEventName)
	applyBool(&settings.ActiveEnabled, value.ActiveEnabled)
	applyBool(&settings.ActiveSound, value.ActiveSound)
	if value.ReminderLead != nil {
		settings.ReminderLead = *value.ReminderLead
	}
	if value.ActiveDisplay != nil {
		settings.ActiveDisplay = *value.ActiveDisplay
	}
	if value.ActiveTheme != nil {
		settings.ActiveTheme = *value.ActiveTheme
	}
	return settings
}
