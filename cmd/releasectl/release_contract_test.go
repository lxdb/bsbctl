package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/installer"
)

func TestProductionPluginArchivesAreInstallable(t *testing.T) {
	root := packageFixture(t)
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(_ context.Context, request buildRequest) ([]byte, error) {
		return []byte("binary:" + request.Component.ID), nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })
	output := filepath.Join(t.TempDir(), "artifacts")
	if _, err := packageComponents(t.Context(), root, output, "darwin", "arm64", time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, "release-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := decodePackageManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.Component == "bsbctl" {
			continue
		}
		t.Run(artifact.Component, func(t *testing.T) {
			archivePath := filepath.Join(output, artifact.Filename)
			var pluginManifest catalog.PackageManifest
			if err := json.Unmarshal(readArchiveMember(t, archivePath, "manifest.json"), &pluginManifest); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			entry := catalog.Entry{ID: artifact.Component, Version: artifact.Version, OS: "darwin", Arch: "arm64", SHA256: artifact.SHA256, CompressedSize: artifact.Size, ArchiveFormat: "tar.gz", Executable: pluginManifest.Executable, Manifest: "manifest.json"}
			verified, err := installer.ExtractAndVerifyFile(file, t.TempDir(), entry)
			if err != nil {
				t.Fatalf("production archive %s cannot be installed: %v", artifact.Filename, err)
			}
			defer verified.Close()
			for _, name := range []string{"LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md", "sbom.cdx.json"} {
				if _, exists := verified.Files[name]; !exists {
					t.Errorf("installed package lost release payload %q", name)
				}
			}
		})
	}
}

func TestGitHubUploadSendsKnownLength(t *testing.T) {
	want := []byte("release artifact content\n")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != int64(len(want)) || len(r.TransferEncoding) != 0 {
			t.Errorf("upload framing: ContentLength=%d TransferEncoding=%v, want %d and no chunking", r.ContentLength, r.TransferEncoding, len(want))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, want) {
			t.Errorf("upload body=%q error=%v, want %q", body, err, want)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(file, want, 0o600); err != nil {
		t.Fatal(err)
	}
	asset, err := releaseAssetFromFile(file, "artifact.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	remote := githubReleaseRemote{client: server.Client(), apiBase: server.URL, repository: "owner/repo", token: "test-token"}
	if err := remote.UploadAsset(t.Context(), remoteRelease{UploadURL: server.URL + "/assets{?name,label}"}, asset); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactInspectionSurvivesBytePreservingTransport(t *testing.T) {
	root := packageFixture(t)
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(_ context.Context, request buildRequest) ([]byte, error) {
		return []byte("binary:" + request.Component.ID), nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })
	source := filepath.Join(t.TempDir(), "source")
	if _, err := packageComponents(t.Context(), root, source, "darwin", "arm64", time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := verifyArtifactDirectory(source); err != nil {
		t.Fatalf("control archive is invalid: %v", err)
	}
	destination := t.TempDir()
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(destination, entry.Name())
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, time.Unix(1800000000, 0), time.Unix(1800000000, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := verifyArtifactDirectory(destination); err != nil {
		t.Fatalf("byte-preserving transport invalidated release artifact content: %v", err)
	}
}
