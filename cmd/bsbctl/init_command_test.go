package main

import (
	"bytes"
	"context"
	"github.com/lxdb/bsbctl/internal/config"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitInvalidConfigurationIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{
		"init",
		"--config", filepath.Join(t.TempDir(), "config.json"),
		"--plugin", filepath.Join(t.TempDir(), "bsbctl-plugin-mac-resources"),
		"--device-url", "not-a-url",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if stderr.Len() == 0 || strings.Contains(stderr.String(), "not-a-url") {
		t.Fatalf("unsafe stderr = %q", stderr.String())
	}
}

func TestInitRejectsMalformedDeviceTokenKeychainReference(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{
		"init",
		"--config", filepath.Join(t.TempDir(), "config.json"),
		"--plugin", filepath.Join(t.TempDir(), "bsbctl-plugin-mac-resources"),
		"--device-token-keychain", "plaintext",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != exitUsage || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestInitAcceptsHierarchicalDeviceTokenKeychainReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{
		"init",
		"--config", path,
		"--device-token-keychain", "keychain://bsbctl/device/access-token",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	document, err := config.NewStore(path).Load()
	if err != nil || document.Device.AccessTokenSecret != "keychain://bsbctl/device/access-token" {
		t.Fatalf("configuration = %#v, %v", document, err)
	}
}
