package codexquota

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestDecodeConfigDefaultsToMainCodexHome(t *testing.T) {
	t.Parallel()
	home := filepath.Join(string(filepath.Separator), "Users", "tester", ".codex")
	config, err := decodeConfig(json.RawMessage(`{}`), home)
	if err != nil {
		t.Fatal(err)
	}
	if config.CredentialsHome != home || config.ConfigurationHome != home {
		t.Fatalf("homes = %q/%q, want %q", config.CredentialsHome, config.ConfigurationHome, home)
	}
	if config.Label != "MAIN" || config.Badge != "M" {
		t.Fatalf("identity = %q/%q", config.Label, config.Badge)
	}
	if config.PollInterval != 120*time.Second || config.WarningRemainingPercent != 20 || config.CriticalRemainingPercent != 5 {
		t.Fatalf("defaults = %#v", config)
	}
}

func TestDecodeConfigRejectsDuplicateFields(t *testing.T) {
	t.Parallel()
	home := filepath.Join(string(filepath.Separator), "Users", "tester", ".codex")
	if _, err := decodeConfig(json.RawMessage(`{"label":"ONE","label":"TWO"}`), home); err == nil {
		t.Fatal("duplicate configuration field was accepted")
	}
}

func TestDecodeConfigExpandsHomesAndRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	userHome := filepath.Join(string(filepath.Separator), "Users", "tester")
	mainHome := filepath.Join(userHome, ".codex")
	config, err := decodeConfig(json.RawMessage(`{
		"credentials_home":"~/Accounts/work",
		"configuration_home":"~/.codex",
		"label":"WORK",
		"badge":"W",
		"poll_interval_seconds":300,
		"warning_remaining_percent":25,
		"critical_remaining_percent":7
	}`), mainHome)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.CredentialsHome, filepath.Join(userHome, "Accounts", "work"); got != want {
		t.Fatalf("CredentialsHome = %q, want %q", got, want)
	}
	if config.ConfigurationHome != mainHome || config.Label != "WORK" || config.Badge != "W" {
		t.Fatalf("config = %#v", config)
	}
	if _, err := decodeConfig(json.RawMessage(`{"token":"secret"}`), mainHome); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestDecodeInstancesBoundsAccountsAndRequiresUniqueHomesAndBadges(t *testing.T) {
	t.Parallel()
	mainHome := filepath.Join(string(filepath.Separator), "Users", "tester", ".codex")
	instances := []protocol.Instance{
		{ID: "main", Generation: 1, Config: json.RawMessage(`{"label":"MAIN","badge":"M"}`)},
		{ID: "work", Generation: 1, Config: json.RawMessage(`{"credentials_home":"~/work","label":"WORK","badge":"W"}`)},
	}
	configured, err := decodeInstances(instances, mainHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(configured) != 2 || !configured[0].ShowBadge || !configured[1].ShowBadge {
		t.Fatalf("configured = %#v", configured)
	}

	for _, raw := range [][]protocol.Instance{
		{
			{ID: "one", Generation: 1, Config: json.RawMessage(`{"credentials_home":"~/same","badge":"A"}`)},
			{ID: "two", Generation: 1, Config: json.RawMessage(`{"credentials_home":"~/same","badge":"B"}`)},
		},
		{
			{ID: "one", Generation: 1, Config: json.RawMessage(`{"credentials_home":"~/one","badge":"A"}`)},
			{ID: "two", Generation: 1, Config: json.RawMessage(`{"credentials_home":"~/two","badge":"A"}`)},
		},
	} {
		if _, err := decodeInstances(raw, mainHome); err == nil {
			t.Fatalf("decodeInstances accepted %#v", raw)
		}
	}

	tooMany := make([]protocol.Instance, 9)
	for index := range tooMany {
		tooMany[index] = protocol.Instance{
			ID: "account-" + string(rune('a'+index)), Generation: 1,
			Config: json.RawMessage(`{"credentials_home":"~/account-` + string(rune('a'+index)) + `","badge":"` + string(rune('A'+index)) + `"}`),
		}
	}
	if _, err := decodeInstances(tooMany, mainHome); err == nil {
		t.Fatal("decodeInstances accepted more than eight accounts")
	}
}

func TestConfigRejectsUnsafeIdentityThresholdsAndIntervals(t *testing.T) {
	t.Parallel()
	mainHome := filepath.Join(string(filepath.Separator), "Users", "tester", ".codex")
	for _, raw := range []string{
		`{"label":""}`,
		`{"label":"this label is much too long"}`,
		`{"badge":"AB"}`,
		`{"badge":"!"}`,
		`{"poll_interval_seconds":59}`,
		`{"poll_interval_seconds":901}`,
		`{"warning_remaining_percent":5,"critical_remaining_percent":5}`,
		`{"warning_remaining_percent":101}`,
		`{"critical_remaining_percent":-1}`,
		`{"credentials_home":"relative"}`,
	} {
		if _, err := decodeConfig(json.RawMessage(raw), mainHome); err == nil {
			t.Fatalf("decodeConfig accepted %s", raw)
		}
	}
}
