package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/lxdb/bsbctl/internal/launchagent"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceFailureDoesNotWriteSuccessShapedOutput(t *testing.T) {
	manager := &fakeServiceManager{
		installResult: launchagent.Result{Status: launchagent.StateNotInstalled},
		installErr:    errors.New("token=secret /private/path provider.invalid"),
	}
	restore := installServiceManager(t, manager)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{
		"service", "install",
		"--config", filepath.Join(t.TempDir(), "config.json"),
		"--socket", filepath.Join(t.TempDir(), "bsbctl.sock"),
		"--plist", filepath.Join(t.TempDir(), "dev.bsbctl.plist"),
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if stderr.Len() == 0 || strings.Contains(stderr.String(), "secret") || strings.Contains(stderr.String(), "/private/path") || strings.Contains(stderr.String(), "provider.invalid") {
		t.Fatalf("unsafe stderr = %q", stderr.String())
	}
}

func TestServiceRestartReturnsObservedLoadedState(t *testing.T) {
	manager := &fakeServiceManager{restartResult: launchagent.Result{Status: launchagent.StateLoaded, PlistMatches: true, Changed: true}}
	restore := installServiceManager(t, manager)
	defer restore()

	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{"service", "restart", "--plist", "/tmp/dev.bsbctl.plist"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stdout.String() != `{"status":"loaded","plist_matches":true,"changed":true}`+"\n" || stderr.Len() != 0 {
		t.Fatalf("service restart = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}
