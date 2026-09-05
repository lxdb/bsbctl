package calendar

import (
	"strings"
	"testing"
	"time"
)

func mustCalendarConfig(t *testing.T) Config {
	t.Helper()
	config, err := decodeConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestDecodeConfigDefaultsReminderLeadToFiveMinutes(t *testing.T) {
	config, err := decodeConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.ReminderLead != 5*time.Minute || !config.ReminderEnabled || !config.ReminderSound || !config.ReminderShowEventName {
		t.Fatalf("reminder defaults = %#v", config)
	}
	if !config.ActiveEnabled || !config.ActiveSound || config.ActiveDisplay != activeDisplayEventName || config.ActiveTheme != "meeting" {
		t.Fatalf("active defaults = %#v", config)
	}
}

func TestDecodeConfigAcceptsBoundedReminderLead(t *testing.T) {
	config, err := decodeConfig([]byte(`{"reminder_lead_minutes":30}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.ReminderLead != 30*time.Minute {
		t.Fatalf("reminder lead = %s, want 30m", config.ReminderLead)
	}
}

func TestDecodeConfigResolvesPerCalendarOverridesWithoutChangingGlobalDefaults(t *testing.T) {
	raw := `{
		"reminder_lead_minutes":5,
		"calendars":[{
			"key":"calendar-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"reminder_lead_minutes":15,
			"reminder_show_event_name":false,
			"active_display":"theme",
			"active_theme":"on_air"
		}]
	}`
	config, err := decodeConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	overridden := config.SettingsForKey("calendar-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if overridden.ReminderLead != 15*time.Minute || overridden.ReminderShowEventName || overridden.ActiveDisplay != activeDisplayTheme || overridden.ActiveTheme != "on_air" {
		t.Fatalf("overridden settings = %#v", overridden)
	}
	defaults := config.SettingsForKey("calendar-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if defaults.ReminderLead != 5*time.Minute || !defaults.ReminderShowEventName || defaults.ActiveDisplay != activeDisplayEventName || defaults.ActiveTheme != "meeting" {
		t.Fatalf("default settings changed = %#v", defaults)
	}
}

func TestDecodeConfigCanDisableOneCalendar(t *testing.T) {
	config, err := decodeConfig([]byte(`{"calendars":[{"key":"calendar-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","enabled":false}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.SettingsForKey("calendar-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa").Enabled {
		t.Fatal("disabled calendar remained enabled")
	}
}

func TestDecodeConfigRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "zero", raw: `{"reminder_lead_minutes":0}`},
		{name: "above maximum", raw: `{"reminder_lead_minutes":61}`},
		{name: "fraction", raw: `{"reminder_lead_minutes":2.5}`},
		{name: "unknown", raw: `{"calendar_ids":["work"]}`},
		{name: "duplicate field", raw: `{"reminder_enabled":true,"reminder_enabled":false}`},
		{name: "duplicate calendar", raw: `{"calendars":[{"key":"calendar-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"key":"calendar-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`},
		{name: "unsafe calendar key", raw: `{"calendars":[{"key":"work"}]}`},
		{name: "unsafe theme", raw: `{"active_display":"theme","active_theme":"../meeting"}`},
		{name: "unsupported active display", raw: `{"active_display":"title"}`},
		{name: "trailing value", raw: `{}` + "\n" + `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeConfig([]byte(test.raw)); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestDecodeConfigRejectsOversizedInput(t *testing.T) {
	raw := []byte(`{"padding":"` + strings.Repeat("x", maxConfigBytes) + `"}`)
	if _, err := decodeConfig(raw); err == nil {
		t.Fatal("oversized configuration was accepted")
	}
}
