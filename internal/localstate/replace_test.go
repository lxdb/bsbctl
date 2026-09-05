package localstate

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

var errInjected = errors.New("injected failure")

func TestReplaceJSONClassifiesEveryWriteBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		inject    func(*operations)
		committed bool
	}{
		{name: "create directory", inject: func(ops *operations) { ops.mkdirAll = func(string, os.FileMode) error { return errInjected } }},
		{name: "create temporary file", inject: func(ops *operations) {
			ops.createTemp = func(string, string) (*os.File, error) { return nil, errInjected }
		}},
		{name: "chmod temporary file", inject: func(ops *operations) { ops.chmod = func(*os.File, os.FileMode) error { return errInjected } }},
		{name: "encode document", inject: func(ops *operations) { ops.encode = func(io.Writer, any) error { return errInjected } }},
		{name: "sync temporary file", inject: func(ops *operations) { ops.syncFile = func(*os.File) error { return errInjected } }},
		{name: "close temporary file", inject: func(ops *operations) {
			ops.closeFile = func(file *os.File) error { _ = file.Close(); return errInjected }
		}},
		{name: "rename temporary file", inject: func(ops *operations) { ops.rename = func(string, string) error { return errInjected } }},
		{name: "open destination directory", committed: true, inject: func(ops *operations) {
			ops.openDirectory = func(string) (*os.File, error) { return nil, errInjected }
		}},
		{name: "sync destination directory", committed: true, inject: func(ops *operations) {
			ops.syncDirectory = func(*os.File) error { return errInjected }
		}},
		{name: "close destination directory", committed: true, inject: func(ops *operations) {
			ops.closeDirectory = func(file *os.File) error { _ = file.Close(); return errInjected }
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "state.json")
			if err := os.WriteFile(path, []byte(`{"state":"old"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			ops := defaultOperations()
			test.inject(&ops)
			outcome, err := replaceJSON(path, map[string]string{"state": "new"}, ops)
			if err == nil {
				t.Fatal("injected failure unexpectedly succeeded")
			}
			wantOutcome := NotCommitted
			wantState := "old"
			if test.committed {
				wantOutcome = CommittedDurabilityUncertain
				wantState = "new"
			}
			if outcome != wantOutcome {
				t.Fatalf("outcome = %q, want %q", outcome, wantOutcome)
			}
			commitErr, ok := errors.AsType[*CommitError](err)
			if !ok || commitErr.Outcome != wantOutcome || !errors.Is(err, errInjected) {
				t.Fatalf("error = %#v, want typed outcome %q wrapping injected failure", err, wantOutcome)
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var stored map[string]string
			if err := json.Unmarshal(data, &stored); err != nil {
				t.Fatalf("decode stored state: %v", err)
			}
			if stored["state"] != wantState {
				t.Fatalf("stored state = %q, want %q", stored["state"], wantState)
			}
			matches, globErr := filepath.Glob(filepath.Join(directory, ".bsbctl-state-*"))
			if globErr != nil || len(matches) != 0 {
				t.Fatalf("temporary files = %v, error %v", matches, globErr)
			}
		})
	}
}

func TestReplaceJSONCommitsRestrictedJSONState(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(directory, "document.json")
	outcome, err := ReplaceJSON(path, map[string]any{"generation": 7, "enabled": true})
	if err != nil {
		t.Fatalf("ReplaceJSON: %v", err)
	}
	if outcome != Committed || !outcome.IsCommitted() {
		t.Fatalf("outcome = %q, want committed", outcome)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o, want 700", dirInfo.Mode().Perm())
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var stored map[string]any
	if err := decoder.Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored["generation"] != float64(7) || stored["enabled"] != true {
		t.Fatalf("stored state = %#v", stored)
	}
}
