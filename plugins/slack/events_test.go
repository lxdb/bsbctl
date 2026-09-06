package slack

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventExtractionRejectsOversizeAndExcessiveRichTraversal(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(strings.Repeat(" ", 262145)), json.RawMessage(`{"type":"event_callback"`)} {
		if _, _, err := normalizeEvent(raw, "A123", "T123", "U123", false); err == nil {
			t.Fatal("malformed or oversize event accepted")
		}
	}
	node := `{"type":"user","user_id":"U123"}`
	for range 18 {
		node = `{"type":"rich_text_section","elements":[` + node + `]}`
	}
	msg := `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","blocks":[{"type":"rich_text","elements":[` + node + `]}]}`
	if _, _, err := normalizeEvent(callback("EvDeep", msg), "A123", "T123", "U123", false); err == nil {
		t.Fatal("unbounded rich traversal")
	}
}

func TestEventIgnoresUserReferencesOutsideRichText(t *testing.T) {
	s := fixtureState(t, "")
	applyFixture(t, s, "Ev1", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","blocks":[{"type":"section","elements":[{"type":"user","user_id":"U123"}]}]}`, fixtureNow)
	if got := s.items(); len(got) != 1 || got[0].Mention || got[0].Kind != "channel" {
		t.Fatal("arbitrary block reference treated as human mention")
	}
}
