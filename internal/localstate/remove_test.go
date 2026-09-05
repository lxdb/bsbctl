package localstate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveAndSyncReportsTruthfulDeletionOutcomes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	outcome, err := RemoveAndSync(path)
	if err != nil || outcome != Committed {
		t.Fatalf("RemoveAndSync = %q, %v", outcome, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal still exists: %v", err)
	}

	outcome, err = RemoveAndSync(path)
	if err != nil || outcome != NotCommitted {
		t.Fatalf("idempotent RemoveAndSync = %q, %v", outcome, err)
	}
}
