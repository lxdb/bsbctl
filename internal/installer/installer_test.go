package installer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"golang.org/x/sys/unix"
)

func TestInstallerInstallsUpdatesAndRollsBackWithoutNetwork(t *testing.T) {
	root := t.TempDir()
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("k", ed25519.SeedSize)))
	artifacts := map[string][]byte{"1.0.0": flowArtifact(t, "1.0.0"), "2.0.0": flowArtifact(t, "2.0.0")}
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		version := strings.TrimSuffix(filepath.Base(request.URL.Path), ".tar.gz")
		body := artifacts[version]
		return response(request, http.StatusOK, io.NopCloser(bytes.NewReader(body)), int64(len(body))), nil
	})}
	activator := &memoryActivator{}
	installer, err := New(Options{Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: client, Activator: activator, Now: func() time.Time { return catalogNowForInstaller }})
	if err != nil {
		t.Fatal(err)
	}

	for sequence, version := range []string{"1.0.0", "2.0.0"} {
		catalogBytes, envelope := signedFlowCatalog(private, uint64(sequence+1), version, artifacts[version])
		result, err := installer.Install(context.Background(), InstallRequest{Catalog: catalogBytes, Envelope: envelope, PluginID: "dev.bsbctl.ball8", Version: version, OS: "darwin", Arch: "arm64"})
		if err != nil {
			t.Fatalf("Install(%s): %v", version, err)
		}
		wantStatus := StatusInstalled
		if sequence == 1 {
			wantStatus = StatusUpdated
		}
		if result.Status != wantStatus || !result.IntentOutcome.IsCommitted() || !result.ActivationOutcome.IsCommitted() || !result.StateOutcome.IsCommitted() || !result.CleanupOutcome.IsCommitted() {
			t.Fatalf("Install(%s) result = %#v", version, result)
		}
		assertExactActivatedPlugin(t, activator.current("dev.bsbctl.ball8"), root, version)
	}
	state, err := NewStateStore(root).LoadState()
	if err != nil {
		t.Fatal(err)
	}
	pluginState := state.Plugins["dev.bsbctl.ball8"]
	if pluginState.Active.Version != "2.0.0" || pluginState.Previous.Version != "1.0.0" || state.CatalogSequence != 2 || len(state.Installed) != 2 {
		t.Fatalf("state after update = %#v", state)
	}

	result, err := installer.Rollback(context.Background(), RollbackRequest{PluginID: "dev.bsbctl.ball8", OS: "darwin", Arch: "arm64"})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if result.Status != StatusRolledBack || requests != 2 {
		t.Fatalf("rollback result = %#v, requests = %d", result, requests)
	}
	assertExactActivatedPlugin(t, activator.current("dev.bsbctl.ball8"), root, "1.0.0")
	state, err = NewStateStore(root).LoadState()
	if err != nil {
		t.Fatal(err)
	}
	pluginState = state.Plugins["dev.bsbctl.ball8"]
	if pluginState.Active.Version != "1.0.0" || pluginState.Previous.Version != "2.0.0" || state.CatalogSequence != 2 {
		t.Fatalf("state after rollback = %#v", state)
	}
	result, err = installer.Rollback(context.Background(), RollbackRequest{PluginID: "dev.bsbctl.ball8", Version: "2.0.0", OS: "darwin", Arch: "arm64"})
	if err != nil || result.Status != StatusRolledBack || requests != 2 {
		t.Fatalf("explicit Rollback = %#v, %v; requests = %d", result, err, requests)
	}
	assertExactActivatedPlugin(t, activator.current("dev.bsbctl.ball8"), root, "2.0.0")
	if _, err := os.Stat(filepath.Join(root, "operation-journal.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remains: %v", err)
	}
}

func TestInstallerRecoversEveryTransactionBoundary(t *testing.T) {
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("k", ed25519.SeedSize)))
	artifact := flowArtifact(t, "1.0.0")
	catalogBytes, envelope := signedFlowCatalog(private, 1, "1.0.0", artifact)
	tests := []struct {
		name              string
		activationOutcome localstate.CommitOutcome
		activateDesired   bool
		failClear         bool
		wantRecovery      Status
	}{
		{name: "after intent restores prior", activationOutcome: localstate.NotCommitted, activateDesired: false, wantRecovery: StatusRecoveredPrior},
		{name: "after config commits target", activationOutcome: localstate.CommittedDurabilityUncertain, activateDesired: true, wantRecovery: StatusRecoveredTarget},
		{name: "after state commits target", activationOutcome: localstate.Committed, activateDesired: true, failClear: true, wantRecovery: StatusRecoveredTarget},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			activator := &memoryActivator{failOutcome: test.activationOutcome, activateBeforeError: test.activateDesired, failOnce: true}
			realState := NewStateStore(root)
			backend := &faultStateBackend{stateBackend: realState, failClearOnce: test.failClear}
			installer, err := New(Options{
				Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)},
				Client: artifactClient(artifact), Activator: activator, Now: func() time.Time { return catalogNowForInstaller }, State: backend,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := installer.Install(context.Background(), InstallRequest{Catalog: catalogBytes, Envelope: envelope, PluginID: "dev.bsbctl.ball8", Version: "1.0.0", OS: "darwin", Arch: "arm64"}); err == nil {
				t.Fatal("faulted install unexpectedly succeeded")
			} else if strings.Contains(err.Error(), "/secret") || strings.Contains(err.Error(), "token-secret") {
				t.Fatalf("installer error exposed dependency details: %q", err)
			}
			if intent, err := realState.LoadIntent(); err != nil || intent == nil {
				t.Fatalf("durable intent = %#v, %v", intent, err)
			}

			result, err := Recover(context.Background(), RecoveryOptions{Root: root, Activator: activator, State: realState})
			if err != nil || result.Status != test.wantRecovery {
				t.Fatalf("Recover = %#v, %v", result, err)
			}
			state, err := realState.LoadState()
			if err != nil {
				t.Fatal(err)
			}
			active := state.Plugins["dev.bsbctl.ball8"].Active
			if test.wantRecovery == StatusRecoveredTarget && (active == nil || active.Version != "1.0.0") {
				t.Fatalf("target recovery state = %#v", state)
			}
			if test.wantRecovery == StatusRecoveredPrior && active != nil {
				t.Fatalf("prior recovery state = %#v", state)
			}
			if intent, err := realState.LoadIntent(); err != nil || intent != nil {
				t.Fatalf("cleared intent = %#v, %v", intent, err)
			}
		})
	}
}

func TestInstallerPreservesIntentWhenCommittedActivationIsCanceled(t *testing.T) {
	root := t.TempDir()
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("q", ed25519.SeedSize)))
	artifact := flowArtifact(t, "1.0.0")
	activator := &memoryActivator{
		failOutcome: localstate.Committed, activateBeforeError: true, failOnce: true, failErr: context.Canceled,
	}
	value, err := New(Options{
		Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: artifactClient(artifact),
		Activator: activator, Now: func() time.Time { return catalogNowForInstaller },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := value.InstallFirst(context.Background(), publicInstallRequest(private, 1, "1.0.0", artifact))
	if CodeOf(err) != CodeActivationFailed || result.ActivationOutcome != localstate.Committed {
		t.Fatalf("InstallFirst = %#v, %v", result, err)
	}
	store := NewStateStore(root)
	if intent, err := store.LoadIntent(); err != nil || intent == nil || intent.Target.Version != "1.0.0" {
		t.Fatalf("preserved intent = %#v, %v", intent, err)
	}
	state, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if active := state.Plugins["dev.bsbctl.ball8"].Active; active != nil {
		t.Fatalf("active state advanced despite canceled activation: %#v", active)
	}
}

func TestInstallerRecoveryRequiresExactKnownDesiredPlugin(t *testing.T) {
	root := t.TempDir()
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("k", ed25519.SeedSize)))
	artifact := flowArtifact(t, "1.0.0")
	catalogBytes, envelope := signedFlowCatalog(private, 1, "1.0.0", artifact)
	activator := &memoryActivator{failOutcome: localstate.CommittedDurabilityUncertain, activateBeforeError: true, failOnce: true}
	installer, err := New(Options{Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: artifactClient(artifact), Activator: activator, Now: func() time.Time { return catalogNowForInstaller }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installer.Install(context.Background(), InstallRequest{Catalog: catalogBytes, Envelope: envelope, PluginID: "dev.bsbctl.ball8", Version: "1.0.0", OS: "darwin", Arch: "arm64"}); err == nil {
		t.Fatal("faulted install unexpectedly succeeded")
	}
	activator.set(config.Plugin{ID: "dev.bsbctl.ball8", Version: "unknown", Executable: "/unknown", ProtocolVersion: protocol.Version, ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive}, Channels: []protocol.Channel{{ID: "answer"}}})
	before, err := os.ReadFile(filepath.Join(root, "operation-journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(context.Background(), RecoveryOptions{Root: root, Activator: activator}); CodeOf(err) != CodeRecoveryRequired {
		t.Fatalf("Recover error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, "operation-journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("recovery_required mutated journal")
	}
}

func TestInstallerRecoveryRejectsInvalidRecordedPriorWhenDesiredIsAbsent(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(t *testing.T, release InstalledRelease)
	}{
		{
			name: "missing prior",
			corrupt: func(t *testing.T, release InstalledRelease) {
				t.Helper()
				if err := os.RemoveAll(release.Root); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "tampered prior",
			corrupt: func(t *testing.T, release InstalledRelease) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(release.Root, release.Manifest.Executable), []byte("tampered"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("k", ed25519.SeedSize)))
			artifact := flowArtifact(t, "1.0.0")
			catalogBytes, envelope := signedFlowCatalog(private, 1, "1.0.0", artifact)
			installer, err := New(Options{Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: artifactClient(artifact), Activator: &memoryActivator{}, Now: func() time.Time { return catalogNowForInstaller }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := installer.Install(context.Background(), InstallRequest{Catalog: catalogBytes, Envelope: envelope, PluginID: "dev.bsbctl.ball8", Version: "1.0.0", OS: "darwin", Arch: "arm64"}); err != nil {
				t.Fatal(err)
			}
			store := NewStateStore(root)
			state, err := store.LoadState()
			if err != nil {
				t.Fatal(err)
			}
			ref := *state.Plugins["dev.bsbctl.ball8"].Active
			release := state.Installed[ref.Key()]
			release.Root = filepath.Join(root, "plugins", ref.ID, ref.Version, ref.OS+"-"+ref.Arch)
			intent := Intent{Version: 1, Kind: OperationRollback, PluginID: ref.ID, Before: state.Plugins[ref.ID], BeforeCatalogSequence: state.CatalogSequence, Target: release, TargetWasInstalled: true}
			if outcome, err := store.WriteIntent(intent); err != nil || !outcome.IsCommitted() {
				t.Fatalf("WriteIntent = %q, %v", outcome, err)
			}
			test.corrupt(t, release)
			stateBefore, err := os.ReadFile(filepath.Join(root, "install-state.json"))
			if err != nil {
				t.Fatal(err)
			}
			intentBefore, err := os.ReadFile(filepath.Join(root, "operation-journal.json"))
			if err != nil {
				t.Fatal(err)
			}

			if _, err := Recover(context.Background(), RecoveryOptions{Root: root, Activator: &memoryActivator{}, State: store}); CodeOf(err) != CodeRecoveryRequired {
				t.Fatalf("Recover error = %v", err)
			}
			stateAfter, _ := os.ReadFile(filepath.Join(root, "install-state.json"))
			intentAfter, _ := os.ReadFile(filepath.Join(root, "operation-journal.json"))
			if !bytes.Equal(stateBefore, stateAfter) || !bytes.Equal(intentBefore, intentAfter) {
				t.Fatal("recovery_required mutated state or journal")
			}
		})
	}
}

func TestInstallerStopsWhenIntentDurabilityIsUncertain(t *testing.T) {
	root := t.TempDir()
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("k", ed25519.SeedSize)))
	artifact := flowArtifact(t, "1.0.0")
	catalogBytes, envelope := signedFlowCatalog(private, 1, "1.0.0", artifact)
	activator := &memoryActivator{}
	realState := NewStateStore(root)
	backend := &faultStateBackend{stateBackend: realState, failIntentOnce: true}
	installer, err := New(Options{Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: artifactClient(artifact), Activator: activator, Now: func() time.Time { return catalogNowForInstaller }, State: backend})
	if err != nil {
		t.Fatal(err)
	}
	result, err := installer.Install(context.Background(), InstallRequest{Catalog: catalogBytes, Envelope: envelope, PluginID: "dev.bsbctl.ball8", Version: "1.0.0", OS: "darwin", Arch: "arm64"})
	if CodeOf(err) != CodeStateFailed || result.IntentOutcome != localstate.CommittedDurabilityUncertain {
		t.Fatalf("Install = %#v, %v", result, err)
	}
	if activator.activationCount() != 0 {
		t.Fatal("activation ran after uncertain intent durability")
	}
	if intent, err := realState.LoadIntent(); err != nil || intent == nil {
		t.Fatalf("recoverable intent = %#v, %v", intent, err)
	}
}

func TestInstallerDoesNotActivateWhenPromotionDurabilityIsUncertain(t *testing.T) {
	root := t.TempDir()
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("k", ed25519.SeedSize)))
	artifact := flowArtifact(t, "1.0.0")
	catalogBytes, envelope := signedFlowCatalog(private, 1, "1.0.0", artifact)
	activator := &memoryActivator{}
	installer, err := New(Options{Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: artifactClient(artifact), Activator: activator, Now: func() time.Time { return catalogNowForInstaller }})
	if err != nil {
		t.Fatal(err)
	}
	versionDirectory := filepath.Join(root, "plugins", "dev.bsbctl.ball8", "1.0.0")
	installer.packages.syncDescriptor = func(fd int, path string) error {
		if path == versionDirectory {
			if _, err := os.Stat(filepath.Join(path, "darwin-arm64")); err == nil {
				return errors.New("post-rename sync fault")
			}
		}
		return unix.Fsync(fd)
	}

	result, err := installer.Install(context.Background(), InstallRequest{Catalog: catalogBytes, Envelope: envelope, PluginID: "dev.bsbctl.ball8", Version: "1.0.0", OS: "darwin", Arch: "arm64"})
	if CodeOf(err) != CodeStateFailed || result.Promotion != PromotionInstalledDurabilityUncertain {
		t.Fatalf("Install = %#v, %v", result, err)
	}
	if activator.activationCount() != 0 {
		t.Fatal("activation ran after uncertain promotion durability")
	}
	if intent, err := NewStateStore(root).LoadIntent(); err != nil || intent != nil {
		t.Fatalf("promotion uncertainty created or cleared recovery metadata: %#v, %v", intent, err)
	}
}

func TestInstallerRetriesExistingPromotionDurabilityBeforeActivation(t *testing.T) {
	root := t.TempDir()
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("k", ed25519.SeedSize)))
	artifact := flowArtifact(t, "1.0.0")
	catalogBytes, envelope := signedFlowCatalog(private, 1, "1.0.0", artifact)
	activator := &memoryActivator{}
	installer, err := New(Options{Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: artifactClient(artifact), Activator: activator, Now: func() time.Time { return catalogNowForInstaller }})
	if err != nil {
		t.Fatal(err)
	}
	versionDirectory := filepath.Join(root, "plugins", "dev.bsbctl.ball8", "1.0.0")
	failVersionSync := true
	installer.packages.syncDescriptor = func(fd int, path string) error {
		if failVersionSync && path == versionDirectory {
			if _, err := os.Stat(filepath.Join(path, "darwin-arm64")); err == nil {
				return errors.New("version sync fault")
			}
		}
		return unix.Fsync(fd)
	}
	request := InstallRequest{Catalog: catalogBytes, Envelope: envelope, PluginID: "dev.bsbctl.ball8", Version: "1.0.0", OS: "darwin", Arch: "arm64"}

	for attempt := 1; attempt <= 2; attempt++ {
		result, err := installer.Install(context.Background(), request)
		if CodeOf(err) != CodeStateFailed || result.Promotion != PromotionInstalledDurabilityUncertain {
			t.Fatalf("attempt %d = %#v, %v", attempt, result, err)
		}
		if activator.activationCount() != 0 {
			t.Fatalf("attempt %d activated before durable adoption", attempt)
		}
	}

	failVersionSync = false
	result, err := installer.Install(context.Background(), request)
	if err != nil || result.Promotion != PromotionAlreadyInstalled {
		t.Fatalf("successful retry = %#v, %v", result, err)
	}
	if activator.activationCount() != 1 {
		t.Fatalf("successful retry activation count = %d", activator.activationCount())
	}
}

func TestInstallerRejectsPublicVersionParentReplacementAtPromotionBoundaries(t *testing.T) {
	tests := []struct {
		name          string
		swapAtPath    func(root string) string
		wantCode      Code
		wantPromotion Promotion
	}{
		{
			name:          "before rename",
			swapAtPath:    func(root string) string { return filepath.Join(root, "plugins", "dev.bsbctl.ball8") },
			wantCode:      CodeInstallConflict,
			wantPromotion: "",
		},
		{
			name: "before durability sync",
			swapAtPath: func(root string) string {
				return filepath.Join(root, "plugins", "dev.bsbctl.ball8", "1.0.0", "darwin-arm64")
			},
			wantCode:      CodeStateFailed,
			wantPromotion: PromotionInstalledDurabilityUncertain,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("k", ed25519.SeedSize)))
			artifact := flowArtifact(t, "1.0.0")
			catalogBytes, envelope := signedFlowCatalog(private, 1, "1.0.0", artifact)
			activator := &memoryActivator{}
			installer, err := New(Options{Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: artifactClient(artifact), Activator: activator, Now: func() time.Time { return catalogNowForInstaller }})
			if err != nil {
				t.Fatal(err)
			}
			versionPath := filepath.Join(root, "plugins", "dev.bsbctl.ball8", "1.0.0")
			publicTarget := filepath.Join(versionPath, "darwin-arm64")
			swapAt := test.swapAtPath(root)
			swapped := false
			installer.packages.syncDescriptor = func(fd int, path string) error {
				if !swapped && path == swapAt {
					swapped = true
					if err := os.Rename(versionPath, versionPath+"-pinned"); err != nil {
						t.Fatalf("replace version parent: %v", err)
					}
					if err := os.Mkdir(versionPath, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(publicTarget, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(publicTarget, "replacement"), []byte("attacker bytes"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return unix.Fsync(fd)
			}

			result, err := installer.Install(context.Background(), InstallRequest{Catalog: catalogBytes, Envelope: envelope, PluginID: "dev.bsbctl.ball8", Version: "1.0.0", OS: "darwin", Arch: "arm64"})
			if !swapped {
				t.Fatal("adversarial replacement hook did not run")
			}
			if CodeOf(err) != test.wantCode || result.Promotion != test.wantPromotion {
				t.Fatalf("Install = %#v, %v", result, err)
			}
			if activator.activationCount() != 0 {
				t.Fatal("replacement-path bytes reached activation")
			}
			data, err := os.ReadFile(filepath.Join(publicTarget, "replacement"))
			if err != nil || string(data) != "attacker bytes" {
				t.Fatalf("public replacement bytes = %q, %v", data, err)
			}
		})
	}
}

func TestTransactionRejectsOversizedNextStateBeforeIntentOrActivation(t *testing.T) {
	var state InstallState
	var target InstalledRelease
	for padding := 1; padding <= 220; padding++ {
		candidate := InstallState{Version: 1, Installed: make(map[string]InstalledRelease), Plugins: make(map[string]PluginInstallState)}
		for index := 0; index < 5; index++ {
			release := installedReleaseWithRecords(fmt.Sprintf("1.0.%d", index), padding)
			candidate.Installed[release.Ref().Key()] = release
		}
		candidateTarget := installedReleaseWithRecords("2.0.0", padding)
		intent := Intent{Version: 1, Kind: OperationRollback, PluginID: candidateTarget.ID, Target: candidateTarget}
		currentBytes, _ := localstate.MarshalJSON(candidate)
		nextBytes, _ := localstate.MarshalJSON(applyTargetState(candidate, intent))
		if len(currentBytes) <= maxInstallStateBytes && len(nextBytes) > maxInstallStateBytes {
			state, target = candidate, candidateTarget
			break
		}
	}
	if target.ID == "" {
		t.Fatal("test could not construct a state that crosses the serialized bound")
	}
	root := t.TempDir()
	activator := &memoryActivator{}
	store := NewStateStore(root)
	installer, err := New(Options{Root: root, Activator: activator, State: store})
	if err != nil {
		t.Fatal(err)
	}
	result, err := installer.transact(context.Background(), state, target, OperationRollback, 0, "", Result{Status: StatusRolledBack, Release: target.Ref()})
	if CodeOf(err) != CodeStateFailed || result.IntentOutcome != "" {
		t.Fatalf("transact = %#v, %v", result, err)
	}
	if activator.activationCount() != 0 {
		t.Fatal("activation ran for state that could not be persisted")
	}
	if intent, err := store.LoadIntent(); err != nil || intent != nil {
		t.Fatalf("oversized transaction created intent: %#v, %v", intent, err)
	}
}

func TestInstallerRecoveryRefusesTamperedIntentTarget(t *testing.T) {
	root := t.TempDir()
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("k", ed25519.SeedSize)))
	artifact := flowArtifact(t, "1.0.0")
	catalogBytes, envelope := signedFlowCatalog(private, 1, "1.0.0", artifact)
	activator := &memoryActivator{failOutcome: localstate.CommittedDurabilityUncertain, activateBeforeError: true, failOnce: true}
	installer, err := New(Options{Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: artifactClient(artifact), Activator: activator, Now: func() time.Time { return catalogNowForInstaller }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installer.Install(context.Background(), InstallRequest{Catalog: catalogBytes, Envelope: envelope, PluginID: "dev.bsbctl.ball8", Version: "1.0.0", OS: "darwin", Arch: "arm64"}); err == nil {
		t.Fatal("faulted install unexpectedly succeeded")
	}
	executable := filepath.Join(root, "plugins", "dev.bsbctl.ball8", "1.0.0", "darwin-arm64", "bsbctl-plugin-ball8")
	if err := os.WriteFile(executable, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(context.Background(), RecoveryOptions{Root: root, Activator: activator}); CodeOf(err) != CodeRecoveryRequired {
		t.Fatalf("Recover error = %v", err)
	}
	state, err := NewStateStore(root).LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Plugins["dev.bsbctl.ball8"].Active != nil {
		t.Fatalf("tampered target became active in state: %#v", state)
	}
	if intent, err := NewStateStore(root).LoadIntent(); err != nil || intent == nil {
		t.Fatalf("intent was not preserved: %#v, %v", intent, err)
	}
}

func TestInstallerSerializesConcurrentOperations(t *testing.T) {
	root := t.TempDir()
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("k", ed25519.SeedSize)))
	artifact := flowArtifact(t, "1.0.0")
	catalogBytes, envelope := signedFlowCatalog(private, 1, "1.0.0", artifact)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	requests := 0
	var requestMu sync.Mutex
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestMu.Lock()
		requests++
		requestMu.Unlock()
		entered <- struct{}{}
		<-release
		return response(request, http.StatusOK, io.NopCloser(bytes.NewReader(artifact)), int64(len(artifact))), nil
	})}
	installer, err := New(Options{Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: client, Activator: &memoryActivator{}, Now: func() time.Time { return catalogNowForInstaller }})
	if err != nil {
		t.Fatal(err)
	}
	request := InstallRequest{Catalog: catalogBytes, Envelope: envelope, PluginID: "dev.bsbctl.ball8", Version: "1.0.0", OS: "darwin", Arch: "arm64"}
	done := make(chan error, 2)
	go func() { _, err := installer.Install(context.Background(), request); done <- err }()
	<-entered
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		_, err := installer.Install(context.Background(), request)
		done <- err
	}()
	<-secondStarted
	select {
	case <-entered:
		t.Fatal("second operation entered transport before first completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-done
	<-done
	requestMu.Lock()
	defer requestMu.Unlock()
	if requests != 1 {
		t.Fatalf("requests = %d, want one serialized fresh-catalog request", requests)
	}
}

var catalogNowForInstaller = time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC)

type memoryActivator struct {
	mu                  sync.Mutex
	desired             map[string]config.Plugin
	failOutcome         localstate.CommitOutcome
	activateBeforeError bool
	failOnce            bool
	failErr             error
	activationCalls     int
}

func (activator *memoryActivator) DesiredPlugin(_ context.Context, pluginID string) (*config.Plugin, error) {
	activator.mu.Lock()
	defer activator.mu.Unlock()
	plugin, exists := activator.desired[pluginID]
	if !exists {
		return nil, nil
	}
	copy := plugin
	return &copy, nil
}

func (activator *memoryActivator) ActivatePlugin(_ context.Context, plugin config.Plugin) (localstate.CommitOutcome, error) {
	activator.mu.Lock()
	defer activator.mu.Unlock()
	activator.activationCalls++
	if activator.failOnce {
		activator.failOnce = false
		if activator.activateBeforeError {
			if activator.desired == nil {
				activator.desired = make(map[string]config.Plugin)
			}
			activator.desired[plugin.ID] = plugin
		}
		if activator.failErr != nil {
			return activator.failOutcome, activator.failErr
		}
		return activator.failOutcome, errors.New("raw activator path /secret token-secret")
	}
	if activator.desired == nil {
		activator.desired = make(map[string]config.Plugin)
	}
	activator.desired[plugin.ID] = plugin
	return localstate.Committed, nil
}

func (activator *memoryActivator) activationCount() int {
	activator.mu.Lock()
	defer activator.mu.Unlock()
	return activator.activationCalls
}

func (activator *memoryActivator) current(pluginID string) config.Plugin {
	activator.mu.Lock()
	defer activator.mu.Unlock()
	return activator.desired[pluginID]
}

func (activator *memoryActivator) set(plugin config.Plugin) {
	activator.mu.Lock()
	defer activator.mu.Unlock()
	if activator.desired == nil {
		activator.desired = make(map[string]config.Plugin)
	}
	activator.desired[plugin.ID] = plugin
}

type faultStateBackend struct {
	stateBackend
	failClearOnce  bool
	failIntentOnce bool
}

func (backend *faultStateBackend) WriteIntent(intent Intent) (localstate.CommitOutcome, error) {
	outcome, err := backend.stateBackend.WriteIntent(intent)
	if err == nil && backend.failIntentOnce {
		backend.failIntentOnce = false
		return localstate.CommittedDurabilityUncertain, errors.New("raw intent sync path /secret")
	}
	return outcome, err
}

func (backend *faultStateBackend) ClearIntent() (localstate.CommitOutcome, error) {
	if backend.failClearOnce {
		backend.failClearOnce = false
		return localstate.NotCommitted, errors.New("raw state path /secret")
	}
	return backend.stateBackend.ClearIntent()
}

func flowArtifact(t *testing.T, version string) []byte {
	t.Helper()
	executable := []byte("executable-" + version)
	digest := sha256.Sum256(executable)
	manifest := []byte(`{"id":"dev.bsbctl.ball8","version":"` + version + `","protocol_version":"1.0","executable":"bsbctl-plugin-ball8","executable_sha256":"` + hex.EncodeToString(digest[:]) + `","executable_size":` + fmtInt(len(executable)) + `,"execution_modes":["interactive"],"channels":[{"id":"answer"}],"assets":[]}`)
	return tarGzip(t, tarItem{name: "manifest.json", body: manifest}, tarItem{name: "bsbctl-plugin-ball8", body: executable})
}

func installedReleaseWithRecords(version string, padding int) InstalledRelease {
	release := installedStateFixture()
	release.Version = version
	release.Manifest.Version = version
	release.Files = cloneRecords(release.Files)
	for index := len(release.Files); index < maxFileRecordsPerRelease; index++ {
		name := fmt.Sprintf("assets/%03d-%s", index, strings.Repeat("x", padding))
		release.Files[name] = FileRecord{SHA256: strings.Repeat("d", 64), Size: 1}
	}
	return release
}

func signedFlowCatalog(private ed25519.PrivateKey, sequence uint64, version string, artifact []byte) ([]byte, []byte) {
	digest := sha256.Sum256(artifact)
	data := []byte(`{"version":1,"channel":"stable","sequence":` + fmtInt(int(sequence)) + `,"generated_at":"2026-08-22T05:00:00Z","plugins":[{"id":"dev.bsbctl.ball8","version":"` + version + `","os":"darwin","arch":"arm64","url":"https://example.invalid/` + version + `.tar.gz","sha256":"` + hex.EncodeToString(digest[:]) + `","compressed_size":` + fmtInt(len(artifact)) + `,"archive_format":"tar.gz","executable":"bsbctl-plugin-ball8","manifest":"manifest.json"}]}`)
	envelope := []byte(`{"key_id":"stable","algorithm":"ed25519","signature":"` + base64.StdEncoding.EncodeToString(ed25519.Sign(private, data)) + `"}`)
	return data, envelope
}

func signedFlowCatalogVersions(private ed25519.PrivateKey, sequence uint64, artifacts map[string][]byte) ([]byte, []byte) {
	versions := []string{"1.0.0", "2.0.0"}
	entries := make([]string, 0, len(versions))
	for _, version := range versions {
		artifact := artifacts[version]
		digest := sha256.Sum256(artifact)
		entries = append(entries, `{"id":"dev.bsbctl.ball8","version":"`+version+`","os":"darwin","arch":"arm64","url":"https://example.invalid/`+version+`.tar.gz","sha256":"`+hex.EncodeToString(digest[:])+`","compressed_size":`+fmtInt(len(artifact))+`,"archive_format":"tar.gz","executable":"bsbctl-plugin-ball8","manifest":"manifest.json"}`)
	}
	data := []byte(`{"version":1,"channel":"stable","sequence":` + fmtInt(int(sequence)) + `,"generated_at":"2026-08-22T05:00:00Z","plugins":[` + strings.Join(entries, ",") + `]}`)
	envelope := []byte(`{"key_id":"stable","algorithm":"ed25519","signature":"` + base64.StdEncoding.EncodeToString(ed25519.Sign(private, data)) + `"}`)
	return data, envelope
}

func artifactClient(artifact []byte) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusOK, io.NopCloser(bytes.NewReader(artifact)), int64(len(artifact))), nil
	})}
}

func assertExactActivatedPlugin(t *testing.T, got config.Plugin, root, version string) {
	t.Helper()
	executable := []byte("executable-" + version)
	digest := sha256.Sum256(executable)
	packageRoot := filepath.Join(root, "plugins", "dev.bsbctl.ball8", version, "darwin-arm64")
	want := config.Plugin{
		ID: "dev.bsbctl.ball8", Version: version, Executable: filepath.Join(packageRoot, "bsbctl-plugin-ball8"),
		ProtocolVersion: protocol.Version,
		SHA256:          hex.EncodeToString(digest[:]), ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
		Channels:    []protocol.Channel{{ID: "answer"}},
		PackageRoot: packageRoot, Assets: []assets.Declaration{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("activated plugin = %#v, want %#v", got, want)
	}
}

func TestConfigFromReleaseCarriesAuthenticatedConfigurationSchema(t *testing.T) {
	release := installedStateFixture()
	release.Manifest.ConfigSchema = &catalog.ConfigSchemaDeclaration{
		Source: "config.schema.json", SHA256: strings.Repeat("d", 64), Size: 42,
	}

	encoded, err := json.Marshal(configFromRelease(release))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if string(document["config_schema"]) != `{"source":"config.schema.json","sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","size":42}` {
		t.Fatalf("config_schema = %s", document["config_schema"])
	}
}

func fmtInt(value int) string {
	return strconv.Itoa(value)
}
