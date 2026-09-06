package slack

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/lxdb/bsbctl/internal/configschema"
)

func TestConfigurationSchemaMatchesRuntimeAndShippedExample(t *testing.T) {
	t.Parallel()
	schemaData, err := os.ReadFile("config.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := configschema.Compile(schemaData)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate([]byte(`{}`)); err != nil {
		t.Fatalf("empty unconfigured default: %v", err)
	}

	exampleData, err := os.ReadFile("../../examples/slack.json")
	if err != nil {
		t.Fatal(err)
	}
	var example struct {
		Config       json.RawMessage   `json:"config"`
		Secrets      map[string]string `json:"secrets"`
		LaunchAction string            `json:"launch_action"`
	}
	if err := json.Unmarshal(exampleData, &example); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(example.Config); err != nil {
		t.Fatalf("shipped configured example: %v", err)
	}
	if _, err := decodeConfig(example.Config); err != nil {
		t.Fatalf("runtime rejected shipped configured example: %v", err)
	}
	wantSecrets := map[string]string{"app_token": "keychain://bsbctl/slack-app-token", "user_token": "keychain://bsbctl/slack-user-token"}
	if !reflect.DeepEqual(example.Secrets, wantSecrets) || example.LaunchAction != "open" {
		t.Fatalf("example secret/action contract = %#v / %q", example.Secrets, example.LaunchAction)
	}

	for name, raw := range map[string][]byte{
		"partial configured form": []byte(`{"label":"OPS"}`),
		"missing app ID":          []byte(`{"workspace_id":"T123","user_id":"U123"}`),
		"missing human ID":        []byte(`{"app_id":"A123","workspace_id":"T123"}`),
		"unknown field":           []byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","unknown":true}`),
		"non-ASCII alias":         []byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","label":"é"}`),
		"blank alias":             []byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","label":"   "}`),
		"too many channels":       append([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[`), append(repeatedChannels(33), []byte(`]}`)...)...),
	} {
		t.Run(name, func(t *testing.T) {
			if err := schema.Validate(raw); err == nil {
				t.Fatal("schema accepted invalid configured form")
			}
		})
	}

	// JSON Schema cannot prove that a watch belongs to a selected or enabled
	// domain or make channel IDs unique independently of their aliases. The
	// strict runtime decoder remains the relational owner.
	for name, relational := range map[string][]byte{
		"watched root outside enabled domain": []byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","direct_messages":false,"watched_threads":[{"channel_id":"D123","thread_ts":"1.000001"}]}`),
		"duplicate channel ID":                []byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"ONE"},{"id":"C123","alias":"TWO"}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := schema.Validate(relational); err != nil {
				t.Fatalf("schema should leave relational validation to runtime: %v", err)
			}
			if _, err := decodeConfig(relational); err == nil {
				t.Fatal("runtime accepted invalid relational configuration")
			}
		})
	}
}

func TestConfigurationSchemaAcceptsAllAccessibleChannels(t *testing.T) {
	schemaData, err := os.ReadFile("config.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := configschema.Compile(schemaData)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","all_channels":true}`)
	if err := schema.Validate(raw); err != nil {
		t.Fatalf("schema rejected all accessible channels: %v", err)
	}
	if _, err := decodeConfig(raw); err != nil {
		t.Fatalf("runtime rejected all accessible channels: %v", err)
	}
}

func TestShippedSlackAppManifestIsMinimal(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../examples/slack-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	wantData := []byte(`{
  "display_information": {"name": "bsbctl Slack"},
  "oauth_config": {"scopes": {"user": ["channels:history", "channels:read", "groups:history", "groups:read", "im:history", "mpim:history"]}},
  "settings": {
    "event_subscriptions": {
      "bot_events": ["app_uninstalled", "tokens_revoked"],
      "user_events": ["message.channels", "message.groups", "message.im", "message.mpim"]
    },
    "socket_mode_enabled": true,
    "token_rotation_enabled": false
  }
}`)
	var got, want any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantData, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Slack app manifest = %#v, want %#v", got, want)
	}
}

func repeatedChannels(count int) []byte {
	result := make([]byte, 0, count*30)
	for index := range count {
		if index > 0 {
			result = append(result, ',')
		}
		result = fmt.Appendf(result, `{"id":"C%02d","alias":"A"}`, index)
	}
	return result
}
