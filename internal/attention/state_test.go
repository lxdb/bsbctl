package attention

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/localstate"
)

func TestStateStoreRoundTripsBoundedPrivateState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention-state.json")
	store := NewStateStore(path)
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	document := StateDocument{
		Version: StateVersion,
		Acknowledgements: []AcknowledgementState{{
			Identity: stateTestIdentity("ack", 2), ObservedAt: now.Add(-time.Minute), TouchedAt: now,
		}},
		LastShown: []LastShownState{{Identity: stateTestIdentity("shown", 2), ShownAt: now.Add(-time.Hour)}},
	}
	if outcome, err := store.Save(document); outcome != localstate.Committed || err != nil {
		t.Fatalf("Save() = %q, %v", outcome, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %#o, want 0600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"session_token", "launcher", "scene", "payload", "rotation_cursor", "back_cooldown"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("durable attention state contains %q: %s", forbidden, data)
		}
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Acknowledgements) != 1 || len(loaded.LastShown) != 1 || loaded.Acknowledgements[0].Identity.InstanceID != "ack" {
		t.Fatalf("loaded = %#v", loaded)
	}
	status := store.Status()
	if status.Phase != "loaded" || status.LastErrorCode != "" || status.LastReadAt != now || status.LastWriteAt != now {
		t.Fatalf("status = %#v", status)
	}
}

func TestStateStoreTreatsMissingStateAsEmptyLoadedState(t *testing.T) {
	store := NewStateStore(filepath.Join(t.TempDir(), "missing.json"))
	document, err := store.Load()
	if err != nil || document.Version != StateVersion || len(document.Acknowledgements) != 0 || len(document.LastShown) != 0 {
		t.Fatalf("Load() = %#v, %v", document, err)
	}
	if status := store.Status(); status.Phase != "loaded" || status.LastErrorCode != "" {
		t.Fatalf("status = %#v", status)
	}
}

func TestStateStoreDegradesOnCorruptAndIncompatibleState(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
		code string
	}{
		{name: "truncated", data: `{"version":`, code: "attention_state_corrupt"},
		{name: "unknown field", data: `{"version":1,"acknowledgements":[],"last_shown":[],"display":{}}`, code: "attention_state_corrupt"},
		{name: "incompatible", data: `{"version":2,"acknowledgements":[],"last_shown":[]}`, code: "attention_state_incompatible"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "attention-state.json")
			if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			store := NewStateStore(path)
			if _, err := store.Load(); err == nil {
				t.Fatal("invalid state loaded successfully")
			}
			status := store.Status()
			if status.Phase != "degraded" || status.LastErrorCode != test.code || status.Failures != 1 {
				t.Fatalf("status = %#v", status)
			}
		})
	}
}

func TestStateStoreRejectsMoreThanMaximumEntries(t *testing.T) {
	store := NewStateStore(filepath.Join(t.TempDir(), "attention-state.json"))
	document := StateDocument{Version: StateVersion, LastShown: make([]LastShownState, MaxStateEntries+1)}
	now := time.Now().UTC()
	for index := range document.LastShown {
		document.LastShown[index] = LastShownState{Identity: stateTestIdentity(string(rune(index+1)), 1), ShownAt: now}
	}
	if outcome, err := store.Save(document); outcome != localstate.NotCommitted || err == nil {
		t.Fatalf("Save() = %q, %v", outcome, err)
	}
}

func TestStateStoreReportsDurabilityUncertainWithoutFalseSuccess(t *testing.T) {
	store := NewStateStore(filepath.Join(t.TempDir(), "attention-state.json"))
	store.replace = func(string, any) (localstate.CommitOutcome, error) {
		return localstate.CommittedDurabilityUncertain, errors.New("directory sync failed")
	}
	document := StateDocument{Version: StateVersion, LastShown: []LastShownState{{Identity: stateTestIdentity("app", 1), ShownAt: time.Now().UTC()}}}
	outcome, err := store.Save(document)
	if outcome != localstate.CommittedDurabilityUncertain || err == nil {
		t.Fatalf("Save() = %q, %v", outcome, err)
	}
	if status := store.Status(); status.Phase != "degraded" || status.LastErrorCode != "attention_state_durability_uncertain" {
		t.Fatalf("status = %#v", status)
	}
}

func stateTestIdentity(instance string, generation uint64) StateIdentity {
	return StateIdentity{PluginID: "plugin", InstanceID: instance, Generation: generation, Channel: "main", Key: "state"}
}
