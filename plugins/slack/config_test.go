package slack

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigDefaultsAndExactEmpty(t *testing.T) {
	for _, raw := range []string{`{}`, ` { } `} {
		cfg, err := decodeConfig(json.RawMessage(raw))
		if err != nil || cfg.configured {
			t.Fatalf("empty config: %#v %v", cfg, err)
		}
		if err := cfg.validateSecrets(nil); err != nil {
			t.Fatal(err)
		}
		if err := cfg.validateSecrets(map[string]string{"app_token": "canary"}); err == nil {
			t.Fatal("empty config accepted secrets")
		}
	}
	cfg, err := decodeConfig(json.RawMessage(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"BUILD"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.configured || cfg.label != "SLK" || !cfg.directMessages || cfg.groupDirectMessages || !cfg.watchParticipatedThreads || cfg.rearDetails || cfg.channels["C123"] != "BUILD" {
		t.Fatalf("defaults: %#v", cfg)
	}
	for _, secrets := range []map[string]string{nil, {"app_token": ""}, {"app_token": "x", "other": "z"}} {
		if err := cfg.validateSecrets(secrets); err == nil {
			t.Fatal("accepted incomplete/extra secrets")
		}
	}
	if err := cfg.validateSecrets(map[string]string{"app_token": "xapp-local"}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigAllChannelsAllowsUnlistedThreadRoots(t *testing.T) {
	cfg, err := decodeConfig(json.RawMessage(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","all_channels":true,"watched_threads":[{"channel_id":"C999","thread_ts":"1700000000.000001"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.watchedThreads) != 1 {
		t.Fatalf("config: %#v", cfg)
	}
	if err := cfg.validateSecrets(map[string]string{"app_token": "app-canary", "user_token": "user-canary"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.validateSecrets(map[string]string{"app_token": "app-canary"}); err == nil {
		t.Fatal("all accessible channels accepted without user metadata token")
	}
}

func TestConfigPreservesFullWorkspaceAndChannelLabels(t *testing.T) {
	cfg, err := decodeConfig(json.RawMessage(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","label":"Engineering Workspace","channels":[{"id":"C123","alias":"engineering-platform"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.label != "Engineering Workspace" {
		t.Fatalf("workspace label = %q", cfg.label)
	}
	if cfg.channels["C123"] != "engineering-platform" {
		t.Fatalf("channel label = %q", cfg.channels["C123"])
	}
}

func TestConfigRejectsPartialUnknownDuplicateAndOversize(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, ``, `{} {}`, `{"label":"SLK"}`, `{"workspace_id":""}`, `{"workspace_id":"T123","user_id":"U123"}`, `{"app_id":"A123","workspace_id":"T123"}`, `{"app_id":"A123","workspace_id":"T123","user_id":""}`,
		`{"app_id":"A123","workspace_id":"T123","user_id":"U123","unknown":"canary"}`, `{"app_id":"A123","workspace_id":"T123","user_id":"U123","workspace_id":"T456"}`,
		`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"A"},{"id":"C123","alias":"B"}]}`,
		`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`,
		`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"é"}]}`,
		`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"D123","alias":"DM"}]}`,
		`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":null}`, `{"app_id":"A123","workspace_id":"T123","user_id":"U123","direct_messages":null}`,
		`{"app_id":"A123","workspace_id":"T123","user_id":"U123","label":"\n"}`, `{"app_id":"A123","workspace_id":"T123","user_id":"U123","rear_details":"true"}`,
		`{"app_id":"A123","workspace_id":"T123","user_id":"U123","watched_threads":[{"channel_id":"C999","thread_ts":"1.000001"}]}`,
		`{"app_id":"A123","workspace_id":"T123","user_id":"U123","direct_messages":false,"watched_threads":[{"channel_id":"D123","thread_ts":"1.000001"}]}`,
		`{"app_id":"A123","workspace_id":"T123","user_id":"U123","watched_threads":[{"channel_id":"D123","thread_ts":"1.000001"},{"channel_id":"D123","thread_ts":"1.000001"}]}`,
		`{"app_id":"A123","workspace_id":"T123","user_id":"U123","watched_threads":[{"channel_id":"D123","thread_ts":"1e3"}]}`,
		strings.Repeat(" ", 65537),
	} {
		t.Run(raw[:min(len(raw), 100)], func(t *testing.T) {
			_, err := decodeConfig(json.RawMessage(raw))
			if err == nil {
				t.Fatal("accepted invalid config")
			}
			if strings.Contains(err.Error(), "canary") {
				t.Fatal("error leaked input")
			}
		})
	}
	channels := make([]map[string]string, 33)
	for i := range channels {
		channels[i] = map[string]string{"id": "C" + strings.Repeat("A", i+1), "alias": "A"}
	}
	raw, _ := json.Marshal(map[string]any{"app_id": "A123", "workspace_id": "T123", "user_id": "U123", "channels": channels})
	if _, err := decodeConfig(raw); err == nil {
		t.Fatal("accepted 33 channels")
	}
}

func TestConfigExplicitWatchesAndDisabledDefaults(t *testing.T) {
	cfg, err := decodeConfig(json.RawMessage(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"G123","alias":"PRIVATE"}],"direct_messages":false,"group_direct_messages":true,"watch_participated_threads":false,"rear_details":true,"label":"WORK","watched_threads":[{"channel_id":"G123","thread_ts":"1700000000.000001"},{"channel_id":"G456","thread_ts":"1700000001.000002"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.directMessages || !cfg.groupDirectMessages || cfg.watchParticipatedThreads || !cfg.rearDetails || cfg.label != "WORK" || len(cfg.watchedThreads) != 2 {
		t.Fatalf("config: %#v", cfg)
	}
}
