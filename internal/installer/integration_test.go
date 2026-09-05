package installer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/catalog"
)

func TestPublicInstallAndUpdateMeaningsAreEnforcedBeforeNetwork(t *testing.T) {
	root := t.TempDir()
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("m", ed25519.SeedSize)))
	artifacts := map[string][]byte{"1.0.0": flowArtifact(t, "1.0.0"), "2.0.0": flowArtifact(t, "2.0.0")}
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		version := strings.TrimSuffix(filepath.Base(request.URL.Path), ".tar.gz")
		body := artifacts[version]
		return response(request, http.StatusOK, io.NopCloser(bytes.NewReader(body)), int64(len(body))), nil
	})}
	value, err := New(Options{
		Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: client,
		Activator: &memoryActivator{}, Now: func() time.Time { return catalogNowForInstaller },
	})
	if err != nil {
		t.Fatal(err)
	}
	request1 := publicInstallRequest(private, 1, "1.0.0", artifacts["1.0.0"])
	result, err := value.InstallFirst(context.Background(), request1)
	if err != nil || result.Status != StatusInstalled || requests != 1 {
		t.Fatalf("first install = %#v, %v, requests=%d", result, err, requests)
	}
	result, err = value.InstallFirst(context.Background(), request1)
	if err != nil || result.Status != StatusInstalled || result.Promotion != PromotionAlreadyInstalled || requests != 1 {
		t.Fatalf("idempotent install = %#v, %v, requests=%d", result, err, requests)
	}
	request2 := publicInstallRequest(private, 2, "2.0.0", artifacts["2.0.0"])
	if _, err := value.InstallFirst(context.Background(), request2); CodeOf(err) != CodeInstallConflict || requests != 1 {
		t.Fatalf("different install error=%v requests=%d", err, requests)
	}
	result, err = value.Update(context.Background(), request2)
	if err != nil || result.Status != StatusUpdated || requests != 2 {
		t.Fatalf("update = %#v, %v, requests=%d", result, err, requests)
	}
	result, err = value.Update(context.Background(), request2)
	if err != nil || result.Status != StatusUpdated || result.Promotion != PromotionAlreadyInstalled || requests != 2 {
		t.Fatalf("idempotent update = %#v, %v, requests=%d", result, err, requests)
	}
}

func TestPublicUpdateRequiresInstalledActiveReleaseBeforeNetwork(t *testing.T) {
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("n", ed25519.SeedSize)))
	artifact := flowArtifact(t, "1.0.0")
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return response(request, http.StatusOK, io.NopCloser(bytes.NewReader(artifact)), int64(len(artifact))), nil
	})}
	value, err := New(Options{Root: t.TempDir(), Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: client, Activator: &memoryActivator{}, Now: func() time.Time { return catalogNowForInstaller }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Update(context.Background(), publicInstallRequest(private, 1, "1.0.0", artifact)); CodeOf(err) != CodeNotInstalled || requests != 0 {
		t.Fatalf("Update error=%v requests=%d", err, requests)
	}
}

func TestExactCatalogCanInstallThenUpdateWithoutAdvancingSequence(t *testing.T) {
	root := t.TempDir()
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("s", ed25519.SeedSize)))
	artifacts := map[string][]byte{"1.0.0": flowArtifact(t, "1.0.0"), "2.0.0": flowArtifact(t, "2.0.0")}
	catalogBytes, envelope := signedFlowCatalogVersions(private, 1, artifacts)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		version := strings.TrimSuffix(filepath.Base(request.URL.Path), ".tar.gz")
		body := artifacts[version]
		return response(request, http.StatusOK, io.NopCloser(bytes.NewReader(body)), int64(len(body))), nil
	})}
	value, err := New(Options{
		Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: client,
		Activator: &memoryActivator{}, Now: func() time.Time { return catalogNowForInstaller },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := InstallRequest{Catalog: catalogBytes, Envelope: envelope, PluginID: "dev.bsbctl.ball8", Version: "1.0.0", OS: "darwin", Arch: "arm64"}
	if _, err := value.InstallFirst(t.Context(), request); err != nil {
		t.Fatalf("InstallFirst: %v", err)
	}
	request.Version = "2.0.0"
	if _, err := value.Update(t.Context(), request); err != nil {
		t.Fatalf("Update from exact catalog: %v", err)
	}
}

func TestIdempotentInstallAndUpdateReverifyActivePackageWithoutNetwork(t *testing.T) {
	tests := []struct {
		name   string
		update bool
		tamper bool
	}{
		{name: "repeat install missing active package"},
		{name: "repeat update tampered active package", update: true, tamper: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("r", ed25519.SeedSize)))
			artifacts := map[string][]byte{"1.0.0": flowArtifact(t, "1.0.0"), "2.0.0": flowArtifact(t, "2.0.0")}
			requests := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				version := strings.TrimSuffix(filepath.Base(request.URL.Path), ".tar.gz")
				body := artifacts[version]
				return response(request, http.StatusOK, io.NopCloser(bytes.NewReader(body)), int64(len(body))), nil
			})}
			value, err := New(Options{
				Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: client,
				Activator: &memoryActivator{}, Now: func() time.Time { return catalogNowForInstaller },
			})
			if err != nil {
				t.Fatal(err)
			}
			request := publicInstallRequest(private, 1, "1.0.0", artifacts["1.0.0"])
			if _, err := value.InstallFirst(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if test.update {
				request = publicInstallRequest(private, 2, "2.0.0", artifacts["2.0.0"])
				if _, err := value.Update(context.Background(), request); err != nil {
					t.Fatal(err)
				}
			}
			beforeRequests := requests
			state, err := NewStateStore(root).LoadState()
			if err != nil {
				t.Fatal(err)
			}
			active := state.Plugins[request.PluginID].Active
			release := state.Installed[active.Key()]
			executable := filepath.Join(release.Root, release.Manifest.Executable)
			if test.tamper {
				if err := os.WriteFile(executable, []byte("tampered"), 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(executable); err != nil {
				t.Fatal(err)
			}
			var repeatErr error
			if test.update {
				_, repeatErr = value.Update(context.Background(), request)
			} else {
				_, repeatErr = value.InstallFirst(context.Background(), request)
			}
			if CodeOf(repeatErr) != CodePackageInvalid || requests != beforeRequests {
				t.Fatalf("repeat error=%v requests=%d, want package_invalid and %d", repeatErr, requests, beforeRequests)
			}
		})
	}
}

func TestInstallerSnapshotIsRedactedAndReportsRecoveryState(t *testing.T) {
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("o", ed25519.SeedSize)))
	artifact := flowArtifact(t, "1.0.0")
	value, err := New(Options{Root: t.TempDir(), Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: artifactClient(artifact), Activator: &memoryActivator{}, Now: func() time.Time { return catalogNowForInstaller }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.InstallFirst(context.Background(), publicInstallRequest(private, 1, "1.0.0", artifact)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := value.Snapshot(context.Background(), "dev.bsbctl.ball8")
	if err != nil || snapshot.CatalogSequence != 1 || snapshot.RecoveryRequired || len(snapshot.Plugins) != 1 || snapshot.Plugins[0].PluginID != "dev.bsbctl.ball8" || snapshot.Plugins[0].Active == nil || snapshot.Plugins[0].Active.Version != "1.0.0" {
		t.Fatalf("Snapshot = %#v, %v", snapshot, err)
	}
}

func TestInstallerActivationOwnershipIsFixedAtConstruction(t *testing.T) {
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("p", ed25519.SeedSize)))
	artifact := flowArtifact(t, "1.0.0")
	startup := &memoryActivator{}
	runtimeActivator := &memoryActivator{}
	root := t.TempDir()
	if _, err := Recover(context.Background(), RecoveryOptions{Root: root, Activator: startup}); err != nil {
		t.Fatal(err)
	}
	value, err := New(Options{Root: root, Keyring: catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, Client: artifactClient(artifact), Activator: runtimeActivator, Now: func() time.Time { return catalogNowForInstaller }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.InstallFirst(context.Background(), publicInstallRequest(private, 1, "1.0.0", artifact)); err != nil {
		t.Fatal(err)
	}
	if startup.activationCount() != 0 || runtimeActivator.activationCount() != 1 {
		t.Fatalf("activation ownership startup=%d runtime=%d", startup.activationCount(), runtimeActivator.activationCount())
	}
}

func publicInstallRequest(private ed25519.PrivateKey, sequence uint64, version string, artifact []byte) InstallRequest {
	catalogBytes, envelope := signedFlowCatalog(private, sequence, version, artifact)
	return InstallRequest{Catalog: catalogBytes, Envelope: envelope, PluginID: "dev.bsbctl.ball8", Version: version, OS: "darwin", Arch: "arm64"}
}
