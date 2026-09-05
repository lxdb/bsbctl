package installer

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/localstate"
	protocoljson "github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	maxInstallStateBytes     = 1 << 20
	maxInstalledReleases     = 128
	maxFileRecordsPerRelease = 512
)

type ReleaseRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func (release InstalledRelease) Ref() ReleaseRef {
	return ReleaseRef{ID: release.ID, Version: release.Version, OS: release.OS, Arch: release.Arch}
}

func (ref ReleaseRef) Key() string { return ref.ID + "@" + ref.Version + "@" + ref.OS + "-" + ref.Arch }

type PluginInstallState struct {
	Active   *ReleaseRef `json:"active,omitempty"`
	Previous *ReleaseRef `json:"previous,omitempty"`
}

type InstallState struct {
	Version         int                           `json:"version"`
	CatalogSequence uint64                        `json:"catalog_sequence"`
	CatalogSHA256   string                        `json:"catalog_sha256,omitempty"`
	Installed       map[string]InstalledRelease   `json:"installed"`
	Plugins         map[string]PluginInstallState `json:"plugins"`
}

type Operation string

const (
	OperationInstall  Operation = "install"
	OperationRollback Operation = "rollback"
)

type Intent struct {
	Version               int                `json:"version"`
	Kind                  Operation          `json:"kind"`
	PluginID              string             `json:"plugin_id"`
	Target                InstalledRelease   `json:"target"`
	Before                PluginInstallState `json:"before"`
	BeforeCatalogSequence uint64             `json:"before_catalog_sequence"`
	BeforeCatalogSHA256   string             `json:"before_catalog_sha256,omitempty"`
	TargetWasInstalled    bool               `json:"target_was_installed"`
	CatalogSequence       uint64             `json:"catalog_sequence"`
	CatalogSHA256         string             `json:"catalog_sha256,omitempty"`
}

type StateStore struct {
	root        string
	statePath   string
	journalPath string
}

func NewStateStore(root string) *StateStore {
	return &StateStore{root: root, statePath: filepath.Join(root, "install-state.json"), journalPath: filepath.Join(root, "operation-journal.json")}
}

func (store *StateStore) LoadState() (InstallState, error) {
	var state InstallState
	err := loadBoundedJSON(store.statePath, &state)
	if errors.Is(err, os.ErrNotExist) {
		return InstallState{Version: 1, Installed: make(map[string]InstalledRelease), Plugins: make(map[string]PluginInstallState)}, nil
	}
	if err != nil || validateInstallState(state) != nil {
		return InstallState{}, errorCode(CodeStateFailed)
	}
	attachReleaseRoots(state.Installed, store.root)
	return state, nil
}

func (store *StateStore) WriteState(state InstallState) (localstate.CommitOutcome, error) {
	if validateInstallState(state) != nil || validateSerializedDocument(state) != nil {
		return localstate.NotCommitted, errorCode(CodeStateFailed)
	}
	if err := store.prepareRoot(); err != nil {
		return localstate.NotCommitted, errorCode(CodeStateFailed)
	}
	outcome, err := localstate.ReplaceJSON(store.statePath, state)
	if err != nil {
		return outcome, errorCode(CodeStateFailed)
	}
	return outcome, nil
}

func (store *StateStore) LoadIntent() (*Intent, error) {
	var intent Intent
	err := loadBoundedJSON(store.journalPath, &intent)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || validateIntent(intent) != nil {
		return nil, errorCode(CodeStateFailed)
	}
	return &intent, nil
}

func (store *StateStore) WriteIntent(intent Intent) (localstate.CommitOutcome, error) {
	if validateIntent(intent) != nil || validateSerializedDocument(intent) != nil {
		return localstate.NotCommitted, errorCode(CodeStateFailed)
	}
	if err := store.prepareRoot(); err != nil {
		return localstate.NotCommitted, errorCode(CodeStateFailed)
	}
	outcome, err := localstate.ReplaceJSON(store.journalPath, intent)
	if err != nil {
		return outcome, errorCode(CodeStateFailed)
	}
	return outcome, nil
}

func (store *StateStore) prepareRoot() error {
	info, err := os.Lstat(store.root)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(store.root, 0o700)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("state root is invalid")
	}
	return os.Chmod(store.root, 0o700)
}

func (store *StateStore) ClearIntent() (localstate.CommitOutcome, error) {
	outcome, err := localstate.RemoveAndSync(store.journalPath)
	if err != nil {
		return outcome, errorCode(CodeStateFailed)
	}
	return outcome, nil
}

func validateInstallState(state InstallState) error {
	if state.Version != 1 || state.Installed == nil || state.Plugins == nil || len(state.Installed) > maxInstalledReleases {
		return errors.New("invalid install state")
	}
	if state.CatalogSHA256 != "" && (state.CatalogSequence == 0 || !validDigest(state.CatalogSHA256)) {
		return errors.New("invalid catalog checkpoint")
	}
	for key, release := range state.Installed {
		if key != release.Ref().Key() || validateInstalledRelease(release) != nil {
			return errors.New("invalid installed release")
		}
	}
	for pluginID, plugin := range state.Plugins {
		if !safePluginToken.MatchString(pluginID) {
			return errors.New("invalid plugin state")
		}
		for _, ref := range []*ReleaseRef{plugin.Active, plugin.Previous} {
			if ref == nil {
				continue
			}
			if ref.ID != pluginID {
				return errors.New("plugin reference identity mismatch")
			}
			if _, exists := state.Installed[ref.Key()]; !exists {
				return errors.New("plugin reference is not installed")
			}
		}
		if plugin.Active != nil && plugin.Previous != nil && *plugin.Active == *plugin.Previous {
			return errors.New("active and previous releases match")
		}
	}
	return nil
}

func validateIntent(intent Intent) error {
	if intent.Version != 1 || (intent.Kind != OperationInstall && intent.Kind != OperationRollback) || intent.PluginID != intent.Target.ID || validateInstalledRelease(intent.Target) != nil {
		return errors.New("invalid operation intent")
	}
	if intent.Kind == OperationInstall && intent.CatalogSequence == 0 {
		return errors.New("install intent requires catalog sequence")
	}
	if intent.CatalogSHA256 != "" && !validDigest(intent.CatalogSHA256) {
		return errors.New("invalid intent catalog digest")
	}
	if intent.BeforeCatalogSHA256 != "" && (intent.BeforeCatalogSequence == 0 || !validDigest(intent.BeforeCatalogSHA256)) {
		return errors.New("invalid prior catalog digest")
	}
	for _, ref := range []*ReleaseRef{intent.Before.Active, intent.Before.Previous} {
		if ref != nil && ref.ID != intent.PluginID {
			return errors.New("intent prior identity mismatch")
		}
	}
	return nil
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
}

func validateInstalledRelease(release InstalledRelease) error {
	if !validReleaseIdentity(release.ID, release.Version, release.OS, release.Arch) || release.Manifest.ID != release.ID || release.Manifest.Version != release.Version || release.Manifest.Executable == "" || release.ManifestSHA256 == "" || len(release.Files) < 2 || len(release.Files) > maxFileRecordsPerRelease {
		return errors.New("invalid release")
	}
	manifestData, err := json.Marshal(release.Manifest)
	if err != nil {
		return errors.New("invalid release manifest")
	}
	entry := catalog.Entry{ID: release.ID, Version: release.Version, Executable: release.Manifest.Executable}
	if _, err := catalog.VerifyPackageManifest(manifestData, entry, string(filepath.Separator)); err != nil {
		return errors.New("invalid release manifest")
	}
	manifestRecord, manifestExists := release.Files["manifest.json"]
	executableRecord, executableExists := release.Files[release.Manifest.Executable]
	if !manifestExists || !executableExists || manifestRecord.SHA256 != release.ManifestSHA256 || executableRecord.SHA256 != release.Manifest.ExecutableSHA256 || executableRecord.Size != release.Manifest.ExecutableSize {
		return errors.New("release file records do not match manifest")
	}
	return nil
}

func validateSerializedDocument(value any) error {
	data, err := localstate.MarshalJSON(value)
	if err != nil || len(data) > maxInstallStateBytes {
		return errors.New("local state exceeds bound")
	}
	return nil
}

func loadBoundedJSON(path string, destination any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxInstallStateBytes+1))
	if err != nil || len(data) > maxInstallStateBytes {
		return errors.New("local state exceeds bound")
	}
	return protocoljson.DecodeStrict(data, destination)
}

func attachReleaseRoots(releases map[string]InstalledRelease, installRoot string) {
	for key, release := range releases {
		release.Root = filepath.Join(installRoot, "plugins", release.ID, release.Version, release.OS+"-"+release.Arch)
		releases[key] = release
	}
}
