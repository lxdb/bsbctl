package main

import (
	"os"
	"strings"
	"testing"

	"github.com/lxdb/bsbctl/plugins/calendar"
)

func TestCalendarExecutableVersionDefaultsToPluginVersion(t *testing.T) {
	t.Parallel()
	if version != calendar.PluginVersion {
		t.Fatalf("version = %q, want %q", version, calendar.PluginVersion)
	}
}

func TestCalendarInfoPlistDeclaresFullAccessPurpose(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("Info.plist")
	if err != nil {
		t.Fatal(err)
	}
	value := string(data)
	for _, required := range []string{
		"<string>dev.bsbctl.calendar</string>",
		"<key>NSCalendarsFullAccessUsageDescription</key>",
		"<key>NSCalendarsUsageDescription</key>",
		"reads local Calendar events",
	} {
		if !strings.Contains(value, required) {
			t.Fatalf("Info.plist is missing %q", required)
		}
	}
}
