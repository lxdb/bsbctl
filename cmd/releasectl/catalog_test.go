package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/catalog"
)

func TestRunCatalogBuildsStableMetadataOnlyFromVerifiedPluginArtifacts(t *testing.T) {
	root := packageFixture(t)
	arm64 := filepath.Join(t.TempDir(), "arm64")
	amd64 := filepath.Join(t.TempDir(), "amd64")
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(_ context.Context, request buildRequest) ([]byte, error) {
		return []byte(fmt.Sprintf("binary:%s:%s:%s\n", request.Component.ID, request.Component.Version, request.GOARCH)), nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })
	epoch := time.Unix(1700000000, 0).UTC()
	if _, err := packageComponents(context.Background(), root, arm64, "darwin", "arm64", epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := packageComponents(context.Background(), root, amd64, "darwin", "amd64", epoch); err != nil {
		t.Fatal(err)
	}

	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	args := []string{
		"catalog", "--root", root, "--artifacts", arm64, "--artifacts", amd64,
		"--base-url", "https://github.com/lxdb/bsbctl/releases/download", "--sequence", "7",
		"--generated-at", "2026-08-22T11:00:00Z", "--out", catalogPath,
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != exitSuccess || stdout.String() != "stable catalog: written with 12 platform entries\n" || stderr.Len() != 0 {
		t.Fatalf("catalog exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x5a}, ed25519.SeedSize))
	installReleaseKeyring(t, catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)})
	signaturePath := filepath.Join(t.TempDir(), "catalog.sig")
	stdout.Reset()
	if code := runWithInput(context.Background(), []string{"sign-catalog", "--catalog", catalogPath, "--key-id", "stable", "--out", signaturePath}, strings.NewReader(base64.StdEncoding.EncodeToString(private)), &stdout, &stderr); code != exitSuccess {
		t.Fatalf("sign exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	envelope, err := os.ReadFile(signaturePath)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := catalog.Verify(data, envelope, catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, 6, time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("catalog.Verify: %v", err)
	}
	if len(verified.Plugins) != 12 {
		t.Fatalf("plugin entries = %d, want 12", len(verified.Plugins))
	}
	for _, entry := range verified.Plugins {
		if entry.ID == "bsbctl" || !strings.HasPrefix(entry.URL, "https://github.com/lxdb/bsbctl/releases/download/plugin%2F") {
			t.Fatalf("unexpected catalog entry: %#v", entry)
		}
	}
}

func TestRunCatalogRejectsUnverifiedOrIncompleteArtifactSets(t *testing.T) {
	root := packageFixture(t)
	output := filepath.Join(t.TempDir(), "catalog.json")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"catalog", "--root", root, "--artifacts", t.TempDir(),
		"--base-url", "http://example.invalid", "--sequence", "0",
		"--generated-at", "bad", "--out", output,
	}, &stdout, &stderr)
	if code != exitFailure || stdout.Len() != 0 || stderr.String() != "releasectl: catalog generation failed\n" {
		t.Fatalf("catalog exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("invalid catalog output exists: %v", err)
	}
}
