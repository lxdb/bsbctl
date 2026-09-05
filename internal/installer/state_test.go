package installer

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestStateStorePersistsBoundedNonSecretStateAndSingleIntent(t *testing.T) {
	root := t.TempDir()
	store := NewStateStore(root)
	release := installedStateFixture()
	ref := release.Ref()
	state := InstallState{
		Version: 1, CatalogSequence: 9,
		Installed: map[string]InstalledRelease{ref.Key(): release},
		Plugins:   map[string]PluginInstallState{release.ID: {Active: &ref}},
	}
	outcome, err := store.WriteState(state)
	if err != nil || outcome != localstate.Committed {
		t.Fatalf("WriteState = %q, %v", outcome, err)
	}
	intent := Intent{Version: 1, Kind: OperationInstall, PluginID: release.ID, Target: release, CatalogSequence: 9}
	outcome, err = store.WriteIntent(intent)
	if err != nil || outcome != localstate.Committed {
		t.Fatalf("WriteIntent = %q, %v", outcome, err)
	}

	loaded, err := store.LoadState()
	if err != nil || loaded.CatalogSequence != 9 || loaded.Installed[ref.Key()].ManifestSHA256 != release.ManifestSHA256 {
		t.Fatalf("LoadState = %#v, %v", loaded, err)
	}
	loadedIntent, err := store.LoadIntent()
	if err != nil || loadedIntent == nil || loadedIntent.Target.Ref() != ref {
		t.Fatalf("LoadIntent = %#v, %v", loadedIntent, err)
	}
	for _, name := range []string{"install-state.json", "operation-journal.json"} {
		path := filepath.Join(root, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, forbidden := range []string{"https://", "token-secret", "/outside/install/root"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s exposes %q", name, forbidden)
			}
		}
		assertMode(t, path, 0o600)
	}
	assertMode(t, root, 0o700)

	outcome, err = store.ClearIntent()
	if err != nil || outcome != localstate.Committed {
		t.Fatalf("ClearIntent = %q, %v", outcome, err)
	}
	if intent, err := store.LoadIntent(); err != nil || intent != nil {
		t.Fatalf("cleared intent = %#v, %v", intent, err)
	}
}

func TestStateStoreRejectsUnknownTrailingOversizedAndInvalidDocuments(t *testing.T) {
	root := t.TempDir()
	store := NewStateStore(root)
	tests := []struct{ name, data string }{
		{name: "unknown", data: `{"version":1,"catalog_sequence":0,"installed":{},"plugins":{},"url":"https://secret.invalid"}`},
		{name: "duplicate", data: `{"version":1,"version":1,"catalog_sequence":0,"installed":{},"plugins":{}}`},
		{name: "trailing", data: `{"version":1,"catalog_sequence":0,"installed":{},"plugins":{}} {}`},
		{name: "invalid", data: `{"version":2,"catalog_sequence":0,"installed":{},"plugins":{}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(root, "install-state.json"), []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadState(); CodeOf(err) != CodeStateFailed {
				t.Fatalf("LoadState error = %v", err)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "install-state.json"), []byte(strings.Repeat(" ", (1<<20)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadState(); CodeOf(err) != CodeStateFailed {
		t.Fatalf("oversized LoadState error = %v", err)
	}
}

func TestStateValidationRejectsConsistentlyCorruptedManifestPath(t *testing.T) {
	release := installedStateFixture()
	delete(release.Files, release.Manifest.Executable)
	release.Manifest.Executable = "../escape"
	release.Files[release.Manifest.Executable] = FileRecord{SHA256: release.Manifest.ExecutableSHA256, Size: release.Manifest.ExecutableSize}
	if err := validateInstalledRelease(release); err == nil {
		t.Fatal("state accepted an executable path escaping the immutable release root")
	}
}

func TestSerializedDocumentBoundMatchesLocalStateEncodingExactly(t *testing.T) {
	for _, size := range []int{maxInstallStateBytes - 64, maxInstallStateBytes, maxInstallStateBytes + 1} {
		value := map[string]string{"payload": strings.Repeat("x", size)}
		encoded, err := localstate.MarshalJSON(value)
		if err != nil {
			t.Fatal(err)
		}
		err = validateSerializedDocument(value)
		if (len(encoded) <= maxInstallStateBytes) != (err == nil) {
			t.Fatalf("encoded bytes = %d, validation error = %v", len(encoded), err)
		}
	}
}

func TestStateStoreRejectsOversizedWritesWithoutReplacingReadableDocuments(t *testing.T) {
	root := t.TempDir()
	store := NewStateStore(root)
	release := installedStateFixture()
	ref := release.Ref()
	baseline := InstallState{Version: 1, Installed: map[string]InstalledRelease{ref.Key(): release}, Plugins: map[string]PluginInstallState{}}
	if outcome, err := store.WriteState(baseline); err != nil || !outcome.IsCommitted() {
		t.Fatalf("baseline WriteState = %q, %v", outcome, err)
	}
	baselineIntent := Intent{Version: 1, Kind: OperationRollback, PluginID: release.ID, Target: release}
	if outcome, err := store.WriteIntent(baselineIntent); err != nil || !outcome.IsCommitted() {
		t.Fatalf("baseline WriteIntent = %q, %v", outcome, err)
	}
	stateBefore, _ := os.ReadFile(filepath.Join(root, "install-state.json"))
	intentBefore, _ := os.ReadFile(filepath.Join(root, "operation-journal.json"))

	tooManyReleases := InstallState{Version: 1, Installed: make(map[string]InstalledRelease), Plugins: make(map[string]PluginInstallState)}
	for index := 0; index <= maxInstalledReleases; index++ {
		candidate := installedStateFixture()
		candidate.Version = fmt.Sprintf("1.0.%d", index)
		candidate.Manifest.Version = candidate.Version
		tooManyReleases.Installed[candidate.Ref().Key()] = candidate
	}
	if outcome, err := store.WriteState(tooManyReleases); CodeOf(err) != CodeStateFailed || outcome != localstate.NotCommitted {
		t.Fatalf("oversized WriteState = %q, %v", outcome, err)
	}
	tooManyFiles := release
	tooManyFiles.Files = cloneRecords(release.Files)
	for index := len(tooManyFiles.Files); index <= maxFileRecordsPerRelease; index++ {
		tooManyFiles.Files[fmt.Sprintf("extra-%03d", index)] = FileRecord{SHA256: strings.Repeat("d", 64), Size: 1}
	}
	oversizedIntent := baselineIntent
	oversizedIntent.Target = tooManyFiles
	if outcome, err := store.WriteIntent(oversizedIntent); CodeOf(err) != CodeStateFailed || outcome != localstate.NotCommitted {
		t.Fatalf("oversized WriteIntent = %q, %v", outcome, err)
	}
	stateAfter, _ := os.ReadFile(filepath.Join(root, "install-state.json"))
	intentAfter, _ := os.ReadFile(filepath.Join(root, "operation-journal.json"))
	if !bytes.Equal(stateBefore, stateAfter) || !bytes.Equal(intentBefore, intentAfter) {
		t.Fatal("oversized write replaced committed readable state")
	}
	if _, err := store.LoadState(); err != nil {
		t.Fatalf("old state is unreadable: %v", err)
	}
	if _, err := store.LoadIntent(); err != nil {
		t.Fatalf("old intent is unreadable: %v", err)
	}
}

func installedStateFixture() InstalledRelease {
	return InstalledRelease{
		ID: "dev.bsbctl.ball8", Version: "1.0.0", OS: "darwin", Arch: "arm64",
		ManifestSHA256: strings.Repeat("b", 64), Files: map[string]FileRecord{
			"manifest.json":       {SHA256: strings.Repeat("b", 64), Size: 10},
			"bsbctl-plugin-ball8": {SHA256: strings.Repeat("c", 64), Size: 10},
		},
		Root: "/outside/install/root", Manifest: packageManifestValueFixture(),
	}
}

func packageManifestValueFixture() catalog.PackageManifest {
	return catalog.PackageManifest{
		ID: "dev.bsbctl.ball8", Version: "1.0.0", ProtocolVersion: protocol.Version,
		Executable: "bsbctl-plugin-ball8", ExecutableSHA256: strings.Repeat("c", 64), ExecutableSize: 10,
		ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive}, Channels: []protocol.Channel{{ID: "answer"}},
		Assets: []assets.Declaration{},
	}
}
