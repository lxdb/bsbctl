package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProtocolVersionIsExactV1(t *testing.T) {
	if Version != "1.0" {
		t.Fatalf("protocol version = %q, want 1.0", Version)
	}
}

func TestDecodeStrictRejectsNestedDuplicateNames(t *testing.T) {
	t.Parallel()

	var decoded struct {
		Payload map[string]any `json:"payload"`
	}
	err := DecodeStrict(json.RawMessage(`{"payload":{"generation":1,"generation":2}}`), &decoded)
	if err == nil {
		t.Fatal("DecodeStrict accepted a nested duplicate object name")
	}
}

func TestValidateJSONObjectRejectsNestedDuplicateNames(t *testing.T) {
	t.Parallel()
	err := ValidateJSONObject("payload", json.RawMessage(`{"nested":{"value":1,"value":2}}`), false)
	if err == nil {
		t.Fatal("ValidateJSONObject accepted a nested duplicate object name")
	}
}

func TestLogNotificationValidateEnforcesBoundedStructuredContract(t *testing.T) {
	valid := LogNotification{
		Level: LogLevelInfo, Event: "sync.completed", Instance: InstanceRef{ID: "configured-instance", Generation: 1},
		Message: "sync completed", Fields: map[string]string{"item_count": "12"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid notification: %v", err)
	}

	tests := map[string]LogNotification{
		"level":       {Level: LogLevel("trace"), Event: "sync.completed"},
		"event":       {Level: LogLevelInfo, Event: "Sync Completed"},
		"event bytes": {Level: LogLevelInfo, Event: strings.Repeat("a", 65)},
		"message utf8": {
			Level: LogLevelInfo, Event: "sync.completed", Message: string([]byte{0xff}),
		},
		"message control": {
			Level: LogLevelInfo, Event: "sync.completed", Message: "first\nsecond",
		},
		"message bytes": {
			Level: LogLevelInfo, Event: "sync.completed", Message: strings.Repeat("x", 1025),
		},
		"field count": {
			Level: LogLevelInfo, Event: "sync.completed", Fields: numberedFields(17),
		},
		"field key": {
			Level: LogLevelInfo, Event: "sync.completed", Fields: map[string]string{"Bad Key": "value"},
		},
		"field value": {
			Level: LogLevelInfo, Event: "sync.completed", Fields: map[string]string{"detail": strings.Repeat("x", 257)},
		},
	}
	for name, notification := range tests {
		t.Run(name, func(t *testing.T) {
			if err := notification.Validate(); err == nil {
				t.Fatal("invalid notification validated")
			}
		})
	}
}

func TestLogNotificationRejectsUnicodeLineSeparators(t *testing.T) {
	for name, notification := range map[string]LogNotification{
		"message line separator": {
			Level: LogLevelInfo, Event: "sync.completed", Message: "first\u2028second",
		},
		"message paragraph separator": {
			Level: LogLevelInfo, Event: "sync.completed", Message: "first\u2029second",
		},
		"field line separator": {
			Level: LogLevelInfo, Event: "sync.completed", Fields: map[string]string{"detail": "first\u2028second"},
		},
		"field paragraph separator": {
			Level: LogLevelInfo, Event: "sync.completed", Fields: map[string]string{"detail": "first\u2029second"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := notification.Validate(); err == nil {
				t.Fatal("Unicode line separator validated")
			}
		})
	}
}

func numberedFields(count int) map[string]string {
	fields := make(map[string]string, count)
	for i := range count {
		fields["field_"+string(rune('a'+i))] = "value"
	}
	return fields
}

func TestDecodeStrictRejectsFormerCandidatePolicyFields(t *testing.T) {
	raw := json.RawMessage(`{
		"observation": {
			"instance":{"id":"app","generation":1}, "channel":"main", "key":"state", "revision":1,
			"disposition":"actionable", "impact":"critical", "reason_code":"failed",
			"observed_at":"2026-08-20T00:00:00Z", "updated_at":"2026-08-20T00:00:00Z",
			"valid_until":"2026-08-20T00:01:00Z", "policy":"attention",
			"scene":{"elements":[{"id":"text","display":"front","text":{"value":"failed"}}]}
		}
	}`)
	var request PublishRequest
	if err := DecodeStrict(raw, &request); err == nil {
		t.Fatal("former plugin policy field unexpectedly decoded")
	}
}
