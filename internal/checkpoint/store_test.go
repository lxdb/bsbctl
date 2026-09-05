package checkpoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/lxdb/bsbctl/internal/localstate"
)

func TestStoreSaveLoadResetUsesRestrictedGenerationScopedFiles(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "checkpoints")
	store := NewStore(root)
	key := Key{PluginID: "dev.bsbctl.test", InstanceID: "primary", Generation: 7}
	outcome, err := store.Save(key, json.RawMessage(`{"cursor":"next"}`))
	if err != nil || outcome != localstate.Committed {
		t.Fatalf("Save outcome/error = %q, %v", outcome, err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("root mode = %o, want 700", rootInfo.Mode().Perm())
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("checkpoint files = %d, want 1", len(entries))
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("checkpoint mode = %o, want 600", info.Mode().Perm())
	}
	loaded, found, err := store.Load(key)
	if err != nil || !found || string(loaded) != `{"cursor":"next"}` {
		t.Fatalf("Load = %s, %v, %v", loaded, found, err)
	}
	loaded[0] = '['
	again, found, err := store.Load(key)
	if err != nil || !found || string(again) != `{"cursor":"next"}` {
		t.Fatalf("Load alias changed stored data = %s, %v, %v", again, found, err)
	}
	if stale, found, err := store.Load(Key{PluginID: key.PluginID, InstanceID: key.InstanceID, Generation: 8}); err != nil || found || stale != nil {
		t.Fatalf("stale Load = %s, %v, %v", stale, found, err)
	}
	if err := store.Reset(key); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if data, found, err := store.Load(key); err != nil || found || data != nil {
		t.Fatalf("Load after Reset = %s, %v, %v", data, found, err)
	}
}

func TestStoreFilenameUsesCollisionResistantFullIdentity(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	first := Key{PluginID: "a/b", InstanceID: "c", Generation: 1}
	second := Key{PluginID: "a", InstanceID: "b/c", Generation: 1}
	if _, err := store.Save(first, json.RawMessage(`{"value":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(second, json.RawMessage(`{"value":2}`)); err != nil {
		t.Fatal(err)
	}
	firstData, firstFound, firstErr := store.Load(first)
	secondData, secondFound, secondErr := store.Load(second)
	if firstErr != nil || secondErr != nil || !firstFound || !secondFound || string(firstData) != `{"value":1}` || string(secondData) != `{"value":2}` {
		t.Fatalf("loads = %s/%v/%v and %s/%v/%v", firstData, firstFound, firstErr, secondData, secondFound, secondErr)
	}
	entries, err := os.ReadDir(store.root)
	if err != nil || len(entries) != 2 || entries[0].Name() == entries[1].Name() {
		t.Fatalf("checkpoint filenames = %v, %v", entries, err)
	}
}

func TestStoreEnforcesDataAndAggregateBoundsWithReplacementAccounting(t *testing.T) {
	store := NewStore(t.TempDir())
	maxData := checkpointObjectOfSize(t, MaxDataBytes)
	for generation := uint64(1); generation <= MaxAggregateDataBytes/MaxDataBytes; generation++ {
		key := Key{PluginID: "plugin", InstanceID: "app", Generation: generation}
		if outcome, err := store.Save(key, maxData); err != nil || !outcome.IsCommitted() {
			t.Fatalf("Save generation %d = %q, %v", generation, outcome, err)
		}
	}
	full := store.Status()
	if full.Files != MaxAggregateDataBytes/MaxDataBytes || full.DataBytes != MaxAggregateDataBytes {
		t.Fatalf("full status = %#v", full)
	}
	replacement := Key{PluginID: "plugin", InstanceID: "app", Generation: 1}
	if outcome, err := store.Save(replacement, maxData); err != nil || !outcome.IsCommitted() {
		t.Fatalf("same-size replacement = %q, %v", outcome, err)
	}
	_, err := store.Save(Key{PluginID: "plugin", InstanceID: "overflow", Generation: 1}, json.RawMessage(`{}`))
	assertCheckpointErrorCode(t, err, CapacityCode)
	if outcome, err := store.Save(Key{PluginID: "plugin", InstanceID: "invalid", Generation: 1}, json.RawMessage(`{"broken"`)); outcome != localstate.NotCommitted {
		t.Fatalf("invalid outcome = %q", outcome)
	} else {
		assertCheckpointErrorCode(t, err, InvalidCode)
	}
	if outcome, err := store.Save(Key{PluginID: "plugin", InstanceID: "duplicate", Generation: 1}, json.RawMessage(`{"cursor":1,"cursor":2}`)); outcome != localstate.NotCommitted {
		t.Fatalf("duplicate-field outcome = %q", outcome)
	} else {
		assertCheckpointErrorCode(t, err, InvalidCode)
	}
	oversized := checkpointObjectOfSize(t, MaxDataBytes+1)
	if outcome, err := store.Save(Key{PluginID: "plugin", InstanceID: "oversized", Generation: 1}, oversized); outcome != localstate.NotCommitted {
		t.Fatalf("oversized outcome = %q", outcome)
	} else {
		assertCheckpointErrorCode(t, err, InvalidCode)
	}
}

func TestStoreRejectsNonObjectCheckpointData(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		data json.RawMessage
	}{
		{name: "scalar", data: json.RawMessage(`"cursor"`)},
		{name: "array", data: json.RawMessage(`["cursor"]`)},
		{name: "null", data: json.RawMessage(`null`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := NewStore(t.TempDir())
			outcome, err := store.Save(Key{PluginID: "plugin", InstanceID: "app", Generation: 1}, test.data)
			if outcome != localstate.NotCommitted {
				t.Fatalf("Save outcome = %q, want %q", outcome, localstate.NotCommitted)
			}
			assertCheckpointErrorCode(t, err, InvalidCode)
			if status := store.Status(); status.Files != 0 || status.DataBytes != 0 {
				t.Fatalf("status after rejected checkpoint = %#v", status)
			}
		})
	}
}

func TestStoreRejectsNonObjectCheckpointWithoutReplacingCurrentData(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		data json.RawMessage
	}{
		{name: "scalar", data: json.RawMessage(`"cursor"`)},
		{name: "array", data: json.RawMessage(`["cursor"]`)},
		{name: "null", data: json.RawMessage(`null`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := NewStore(t.TempDir())
			key := Key{PluginID: "plugin", InstanceID: "app", Generation: 1}
			current := json.RawMessage(`{"cursor":"kept"}`)
			if outcome, err := store.Save(key, current); err != nil || !outcome.IsCommitted() {
				t.Fatalf("initial Save = %q, %v", outcome, err)
			}
			fileBefore, err := os.ReadFile(store.path(key))
			if err != nil {
				t.Fatal(err)
			}
			statusBefore := store.Status()

			outcome, err := store.Save(key, test.data)
			if outcome != localstate.NotCommitted {
				t.Fatalf("invalid Save outcome = %q, want %q", outcome, localstate.NotCommitted)
			}
			assertCheckpointErrorCode(t, err, InvalidCode)
			fileAfter, err := os.ReadFile(store.path(key))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(fileAfter, fileBefore) {
				t.Fatalf("checkpoint file changed after rejected replacement:\n before: %s\n  after: %s", fileBefore, fileAfter)
			}
			loaded, found, err := store.Load(key)
			if err != nil || !found || string(loaded) != string(current) {
				t.Fatalf("Load after rejected replacement = %s, %v, %v", loaded, found, err)
			}
			statusAfter := store.Status()
			if statusAfter.Files != statusBefore.Files || statusAfter.DataBytes != statusBefore.DataBytes ||
				statusAfter.Failures != statusBefore.Failures+1 || statusAfter.LastErrorCode != InvalidCode {
				t.Fatalf("status before/after rejected replacement = %#v / %#v", statusBefore, statusAfter)
			}
		})
	}
}

func TestStoreKeepsLargeStructuredJSONWithinBoundedEnvelope(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	data := json.RawMessage(`{"values":[` + strings.Repeat("0,", (MaxDataBytes-14)/2) + `0]}`)
	if len(data) > MaxDataBytes || !json.Valid(data) {
		t.Fatalf("invalid boundary fixture: %d bytes", len(data))
	}
	key := Key{PluginID: "plugin", InstanceID: "structured", Generation: 1}
	if _, err := store.Save(key, data); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := store.Load(key)
	if err != nil || !found || string(loaded) != string(data) {
		t.Fatalf("Load large structured JSON = %d bytes, %v, %v", len(loaded), found, err)
	}
}

func TestStoreRestartDeterministicallyRemovesPreexistingDataAboveAggregateLimit(t *testing.T) {
	root := t.TempDir()
	fixtureStore := NewStore(root)
	data := checkpointObjectOfSize(t, MaxDataBytes)
	keys := make([]Key, 0, MaxAggregateDataBytes/MaxDataBytes+1)
	for index := 0; index < cap(keys); index++ {
		key := Key{PluginID: "plugin", InstanceID: fmt.Sprintf("app-%03d", index), Generation: 1}
		keys = append(keys, key)
		encoded, err := json.Marshal(envelope{
			Version: envelopeVersion, PluginID: key.PluginID, InstanceID: key.InstanceID,
			Generation: key.Generation, Data: data,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixtureStore.path(key), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return filepath.Base(fixtureStore.path(keys[i])) < filepath.Base(fixtureStore.path(keys[j]))
	})

	restarted := NewStore(root)
	if err := restarted.Reconcile(keys); err != nil {
		t.Fatalf("Reconcile restart inventory: %v", err)
	}
	status := restarted.Status()
	if status.Files != MaxAggregateDataBytes/MaxDataBytes || status.DataBytes != MaxAggregateDataBytes || status.LastErrorCode != CapacityCode {
		t.Fatalf("bounded restart status = %#v", status)
	}
	for index, key := range keys {
		loaded, found, err := restarted.Load(key)
		wantFound := index < MaxAggregateDataBytes/MaxDataBytes
		if err != nil || found != wantFound {
			t.Fatalf("Load sorted key %d = %v, %v; want found %v", index, found, err, wantFound)
		}
		if found && string(loaded) != string(data) {
			t.Fatalf("Load sorted key %d data size = %d", index, len(loaded))
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != MaxAggregateDataBytes/MaxDataBytes {
		t.Fatalf("retained checkpoint files = %d, %v", len(entries), err)
	}
}

func TestStoreReconcileRetainsOnlyExactActiveGenerations(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	current := Key{PluginID: "plugin", InstanceID: "current", Generation: 4}
	stale := Key{PluginID: "plugin", InstanceID: "current", Generation: 3}
	disabled := Key{PluginID: "plugin", InstanceID: "disabled", Generation: 4}
	missing := Key{PluginID: "removed", InstanceID: "missing", Generation: 4}
	for _, key := range []Key{current, stale, disabled, missing} {
		if _, err := store.Save(key, json.RawMessage(`{"cursor":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Reconcile([]Key{current}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, test := range []struct {
		key   Key
		found bool
	}{{current, true}, {stale, false}, {disabled, false}, {missing, false}} {
		_, found, err := store.Load(test.key)
		if err != nil || found != test.found {
			t.Fatalf("Load(%#v) found/error = %v, %v", test.key, found, err)
		}
	}
}

func TestStoreRemovesCorruptMismatchedAndOversizedFilesWithoutDelivery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content func(Key) []byte
	}{
		{name: "corrupt", content: func(Key) []byte { return []byte(`{"version":1,"data":`) }},
		{name: "mismatched identity", content: func(key Key) []byte {
			value, _ := json.Marshal(envelope{Version: envelopeVersion, PluginID: key.PluginID, InstanceID: "other", Generation: key.Generation, Data: json.RawMessage(`{}`)})
			return value
		}},
		{name: "oversized", content: func(Key) []byte { return []byte(strings.Repeat("x", maxEnvelopeBytes+1)) }},
		{name: "unknown envelope field", content: func(key Key) []byte {
			return []byte(`{"version":1,"plugin_id":"` + key.PluginID + `","instance_id":"` + key.InstanceID + `","generation":1,"data":{},"unknown":true}`)
		}},
		{name: "duplicate envelope field", content: func(key Key) []byte {
			return []byte(`{"version":1,"version":1,"plugin_id":"` + key.PluginID + `","instance_id":"` + key.InstanceID + `","generation":1,"data":{}}`)
		}},
		{name: "duplicate checkpoint data field", content: func(key Key) []byte {
			return []byte(`{"version":1,"plugin_id":"` + key.PluginID + `","instance_id":"` + key.InstanceID + `","generation":1,"data":{"cursor":1,"cursor":2}}`)
		}},
		{name: "scalar checkpoint data", content: func(key Key) []byte {
			return []byte(`{"version":1,"plugin_id":"` + key.PluginID + `","instance_id":"` + key.InstanceID + `","generation":1,"data":"cursor"}`)
		}},
		{name: "array checkpoint data", content: func(key Key) []byte {
			return []byte(`{"version":1,"plugin_id":"` + key.PluginID + `","instance_id":"` + key.InstanceID + `","generation":1,"data":[]}`)
		}},
		{name: "null checkpoint data", content: func(key Key) []byte {
			return []byte(`{"version":1,"plugin_id":"` + key.PluginID + `","instance_id":"` + key.InstanceID + `","generation":1,"data":null}`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewStore(t.TempDir())
			key := Key{PluginID: "plugin", InstanceID: "app", Generation: 1}
			if err := store.ensureRoot(); err != nil {
				t.Fatal(err)
			}
			path := store.path(key)
			if err := os.WriteFile(path, test.content(key), 0o600); err != nil {
				t.Fatal(err)
			}
			data, found, err := store.Load(key)
			if err != nil || found || data != nil {
				t.Fatalf("corrupt Load = %s, %v, %v", data, found, err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("corrupt file remains: %v", err)
			}
			status := store.Status()
			if status.Corruptions != 1 || status.LastErrorCode != CorruptCode {
				t.Fatalf("status = %#v", status)
			}
		})
	}
}

func TestReadEnvelopeKeepsValidationPermissionRepairAndReadOnOneDescriptor(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "checkpoint.json")
	key := Key{PluginID: "plugin", InstanceID: "app", Generation: 1}
	encoded, err := json.Marshal(envelope{
		Version: envelopeVersion, PluginID: key.PluginID, InstanceID: key.InstanceID,
		Generation: key.Generation, Data: json.RawMessage(`{"cursor":"verified"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	originalOpen := openCheckpointFile
	openCheckpointFile = func(openPath string) (*os.File, error) {
		file, openErr := openCheckpointNoFollow(openPath)
		if openErr != nil {
			return nil, openErr
		}
		if renameErr := os.Rename(openPath, filepath.Join(directory, "opened.json")); renameErr != nil {
			_ = file.Close()
			return nil, renameErr
		}
		replacement := []byte(`{"version":1,"plugin_id":"plugin","instance_id":"app","generation":1,"data":{"cursor":"replacement"}}`)
		if writeErr := os.WriteFile(openPath, replacement, 0o644); writeErr != nil {
			_ = file.Close()
			return nil, writeErr
		}
		return file, nil
	}
	t.Cleanup(func() { openCheckpointFile = originalOpen })

	value, err := readEnvelope(path)
	if err != nil || string(value.Data) != `{"cursor":"verified"}` {
		t.Fatalf("readEnvelope = %#v, %v", value, err)
	}
	replacementInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if replacementInfo.Mode().Perm() != 0o644 {
		t.Fatalf("replacement mode = %o, want unchanged 644", replacementInfo.Mode().Perm())
	}
	openedInfo, err := os.Stat(filepath.Join(directory, "opened.json"))
	if err != nil || openedInfo.Mode().Perm() != 0o600 {
		t.Fatalf("opened file mode = %v, %v", openedInfo, err)
	}
}

func TestStoreSaveReportsCommittedDurabilityUncertain(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	store.replace = func(path string, value any) (localstate.CommitOutcome, error) {
		outcome, err := localstate.ReplaceJSON(path, value)
		if err != nil || outcome != localstate.Committed {
			t.Fatalf("write replacement: %q, %v", outcome, err)
		}
		return localstate.CommittedDurabilityUncertain, &localstate.CommitError{
			Outcome: localstate.CommittedDurabilityUncertain,
			Op:      "sync state directory",
			Err:     errors.New("injected directory sync failure"),
		}
	}
	key := Key{PluginID: "plugin", InstanceID: "app", Generation: 1}
	outcome, err := store.Save(key, json.RawMessage(`{"cursor":"committed"}`))
	if outcome != localstate.CommittedDurabilityUncertain {
		t.Fatalf("outcome = %q", outcome)
	}
	assertCheckpointErrorCode(t, err, DurabilityUncertainCode)
	data, found, loadErr := store.Load(key)
	if loadErr != nil || !found || string(data) != `{"cursor":"committed"}` {
		t.Fatalf("committed Load = %s, %v, %v", data, found, loadErr)
	}
	if store.Status().LastErrorCode != DurabilityUncertainCode {
		t.Fatalf("status = %#v", store.Status())
	}
}

func TestStoreResetReportsTruthfulCleanupOutcome(t *testing.T) {
	t.Run("remove failure is not committed", func(t *testing.T) {
		store := NewStore(t.TempDir())
		key := Key{PluginID: "plugin", InstanceID: "app", Generation: 1}
		if _, err := store.Save(key, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		store.remove = func(string) error { return errors.New("injected remove failure") }
		err := store.Reset(key)
		assertCheckpointError(t, err, IOFailedCode, localstate.NotCommitted)
		if _, statErr := os.Stat(store.path(key)); statErr != nil {
			t.Fatalf("not-committed cleanup removed file: %v", statErr)
		}
	})

	t.Run("post-remove sync failure is durability uncertain", func(t *testing.T) {
		store := NewStore(t.TempDir())
		key := Key{PluginID: "plugin", InstanceID: "app", Generation: 1}
		if _, err := store.Save(key, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		store.syncDirectory = func(string) error { return errors.New("injected directory sync failure") }
		err := store.Reset(key)
		assertCheckpointError(t, err, DurabilityUncertainCode, localstate.CommittedDurabilityUncertain)
		if _, statErr := os.Stat(store.path(key)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("committed cleanup file remains: %v", statErr)
		}
	})
}

func TestStoreReconcileReportsCommittedPartialMultiFileCleanup(t *testing.T) {
	store := NewStore(t.TempDir())
	keys := []Key{
		{PluginID: "plugin", InstanceID: "first", Generation: 1},
		{PluginID: "plugin", InstanceID: "second", Generation: 1},
		{PluginID: "plugin", InstanceID: "third", Generation: 1},
	}
	for _, key := range keys {
		if _, err := store.Save(key, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return store.path(keys[i]) < store.path(keys[j]) })
	removeCalls := 0
	store.remove = func(path string) error {
		removeCalls++
		if removeCalls == 2 {
			return errors.New("injected second remove failure")
		}
		return os.Remove(path)
	}
	err := store.Reconcile(nil)
	assertCheckpointError(t, err, IOFailedCode, localstate.Committed)
	if _, statErr := os.Stat(store.path(keys[0])); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("first cleanup did not commit: %v", statErr)
	}
	for _, key := range keys[1:] {
		if _, statErr := os.Stat(store.path(key)); statErr != nil {
			t.Fatalf("cleanup continued past failing boundary: %v", statErr)
		}
	}
}

func TestStoreReconcileAggregatesCommittedScanCleanupBeforeLaterFailure(t *testing.T) {
	store := NewStore(t.TempDir())
	key := Key{PluginID: "plugin", InstanceID: "stale", Generation: 1}
	encoded, err := json.Marshal(envelope{
		Version: envelopeVersion, PluginID: key.PluginID, InstanceID: key.InstanceID,
		Generation: key.Generation, Data: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path(key), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	corruptPath := filepath.Join(store.root, "000-corrupt.json")
	if err := os.WriteFile(corruptPath, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	store.remove = func(path string) error {
		if path == corruptPath {
			return os.Remove(path)
		}
		return errors.New("injected stale remove failure")
	}
	err = store.Reconcile(nil)
	assertCheckpointError(t, err, IOFailedCode, localstate.Committed)
	if _, statErr := os.Stat(corruptPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("scan cleanup did not commit: %v", statErr)
	}
	if _, statErr := os.Stat(store.path(key)); statErr != nil {
		t.Fatalf("later failing cleanup removed stale file: %v", statErr)
	}
}

func assertCheckpointErrorCode(t *testing.T, err error, code DiagnosticCode) {
	t.Helper()
	checkpointErr, ok := errors.AsType[*Error](err)
	if !ok || checkpointErr.Code != code {
		t.Fatalf("error = %#v, want checkpoint code %q", err, code)
	}
}

func assertCheckpointError(t *testing.T, err error, code DiagnosticCode, outcome localstate.CommitOutcome) {
	t.Helper()
	checkpointErr, ok := errors.AsType[*Error](err)
	if !ok || checkpointErr.Code != code || checkpointErr.Outcome != outcome {
		t.Fatalf("error = %#v, want checkpoint code %q and outcome %q", err, code, outcome)
	}
}

func checkpointObjectOfSize(t *testing.T, size int) json.RawMessage {
	t.Helper()
	const envelope = `{"value":""}`
	if size < len(envelope) {
		t.Fatalf("checkpoint object size %d is smaller than minimum %d", size, len(envelope))
	}
	data := json.RawMessage(`{"value":"` + strings.Repeat("a", size-len(envelope)) + `"}`)
	if len(data) != size || !json.Valid(data) {
		t.Fatalf("invalid checkpoint object fixture: %d bytes", len(data))
	}
	return data
}
