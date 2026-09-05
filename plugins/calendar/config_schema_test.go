package calendar

import (
	"os"
	"testing"

	"github.com/lxdb/bsbctl/internal/configschema"
)

func TestConfigurationSchemaMatchesRuntimeCalendarContract(t *testing.T) {
	data, err := os.ReadFile(configschema.FileName)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := configschema.Compile(data)
	if err != nil {
		t.Fatal(err)
	}
	valid := [][]byte{
		[]byte(`{}`),
		[]byte(`{"reminder_lead_minutes":1}`),
		[]byte(`{"reminder_lead_minutes":60}`),
		[]byte(`{"reminder_enabled":false,"reminder_sound":false,"reminder_show_event_name":false,"active_enabled":false,"active_sound":false,"active_display":"theme","active_theme":"on_air"}`),
		[]byte(`{"calendars":[{"key":"calendar-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","enabled":false,"reminder_lead_minutes":15,"active_display":"theme","active_theme":"meeting"}]}`),
	}
	invalid := [][]byte{
		[]byte(`{"reminder_lead_minutes":0}`),
		[]byte(`{"reminder_lead_minutes":61}`),
		[]byte(`{"reminder_lead_minutes":2.5}`),
		[]byte(`{"calendar_ids":["work"]}`),
		[]byte(`{"active_display":"title"}`),
		[]byte(`{"active_theme":"../meeting"}`),
		[]byte(`{"calendars":[{"key":"work"}]}`),
	}
	for _, raw := range valid {
		if err := schema.Validate(raw); err != nil {
			t.Fatalf("schema rejected valid %s: %v", raw, err)
		}
		if _, err := decodeConfig(raw); err != nil {
			t.Fatalf("runtime rejected valid %s: %v", raw, err)
		}
	}
	for _, raw := range invalid {
		if err := schema.Validate(raw); err == nil {
			t.Fatalf("schema accepted invalid %s", raw)
		}
		if _, err := decodeConfig(raw); err == nil {
			t.Fatalf("runtime accepted invalid %s", raw)
		}
	}
}
