package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	busylib "github.com/lxdb/busylib-go"
)

func TestFakeDependenciesStoreUploadedAssetsAtFirmwarePath(t *testing.T) {
	fake := newFakeDependencies()
	defer fake.Close()

	client, err := busylib.NewClient(
		busylib.WithBaseURL(fake.URL()),
		busylib.WithVersionNegotiation(busylib.VersionNegotiationDisabled),
	)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("synthetic asset")
	localPath := filepath.Join(t.TempDir(), "mark.png")
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	const asset = "dev.bsbctl.codex-quota/content.png"
	if err := client.Assets().UploadFile(t.Context(), "bsbctl", asset, localPath); err != nil {
		t.Fatalf("upload asset: %v", err)
	}

	const stored = "/ext/user_assets/bsbctl/dev.bsbctl.codex-quota/content.png"
	got, err := client.Storage().Read(context.Background(), stored)
	if err != nil {
		t.Fatalf("read uploaded asset: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("stored asset = %q, want %q", got, content)
	}
	if err := client.Storage().Remove(t.Context(), stored); err != nil {
		t.Fatalf("remove uploaded asset: %v", err)
	}
	if _, err := client.Storage().Read(t.Context(), stored); err == nil {
		t.Fatal("read removed asset succeeded")
	}
}
