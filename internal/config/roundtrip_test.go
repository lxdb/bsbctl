package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestConfigLimitRoundTrip(t *testing.T) {
	document := validDocument()
	app := document.Apps["ball8"]
	app.Config = json.RawMessage(`{"x":"` + strings.Repeat("x", (64<<10)-8) + `"}`)
	document.Apps["ball8"] = app
	store := NewStore(filepath.Join(t.TempDir(), "config.json"))
	if outcome, err := store.ReplaceWithOutcome(0, document); err != nil || !outcome.IsCommitted() {
		t.Fatalf("valid exact-limit write: outcome=%v error=%v", outcome, err)
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("successful exact-limit write cannot reload: %v", err)
	}
}

func TestConfigOperationMutationAdvancesAppGeneration(t *testing.T) {
	document := validDocument()
	plugin := document.Plugins["dev.bsbctl.ball8"]
	plugin.Operations = []protocol.OperationDescriptor{{ID: "inspect", Kind: protocol.OperationQuery}}
	document.Plugins[plugin.ID] = plugin
	store := NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	updated, _, err := store.Update(1, func(next *Document) error {
		next.Plugins[plugin.ID].Operations[0].Kind = protocol.OperationCommand
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Apps["ball8"].Generation; got != 2 {
		t.Fatalf("operation contract changed but app generation = %d, want 2", got)
	}
}

func TestConfigAggregateRejectsBeforeCommit(t *testing.T) {
	document := validDocument()
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path)
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, outcome, err := store.Update(1, func(next *Document) error {
		original := next.Apps["ball8"]
		for index := range 20 {
			app := original
			app.ID = fmt.Sprintf("audit-%d", index)
			app.Config = json.RawMessage(`{"x":"` + strings.Repeat("x", 60<<10) + `"}`)
			next.Apps[app.ID] = app
		}
		return nil
	})
	if err == nil || outcome.IsCommitted() {
		t.Errorf("oversized aggregate accepted: outcome=%v error=%v", outcome, err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(before, after) {
		t.Error("rejected transaction replaced prior config bytes")
	}
	if _, loadErr := store.Load(); loadErr != nil {
		t.Errorf("prior config no longer readable: %v", loadErr)
	}
}
