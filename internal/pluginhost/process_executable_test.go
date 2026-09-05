package pluginhost

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestChildEnvironmentIsDeterministicAllowlistAndProtectsCoreContract(t *testing.T) {
	parent := []string{
		"PATH=/unsafe/bin",
		"HOME=/Users/plugin",
		"TMPDIR=/private/tmp/plugin",
		"LANG=en_US.UTF-8",
		"LC_ALL=C",
		"TZ=UTC",
		"CODEX_HOME=/Users/plugin/.codex",
		"HOME=/Users/plugin-final",
		"TOKEN=do-not-propagate",
		"AWS_SECRET_ACCESS_KEY=do-not-propagate",
		"HTTPS_PROXY=http://credential@example.invalid",
		"BSBCTL_RPC_FD=99",
		"BSBCTL_OWN_PROCESS_GROUP=0",
		"BSBCTL_UNRELATED=do-not-propagate",
		"EMPTY_ALLOWED=",
	}
	want := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"BSBCTL_RPC_FD=3",
		"BSBCTL_OWN_PROCESS_GROUP=1",
		"HOME=/Users/plugin-final",
		"TMPDIR=/private/tmp/plugin",
		"LANG=en_US.UTF-8",
		"LC_ALL=C",
		"TZ=UTC",
		"CODEX_HOME=/Users/plugin/.codex",
	}
	if got := childEnvironment(parent); !equalStrings(got, want) {
		t.Fatalf("child environment = %q, want %q", got, want)
	}
}

func TestExecutableIdentityHashesCompleteFileAndReturnsSize(t *testing.T) {
	const formerHashLimit = int64(512 << 20)
	path := filepath.Join(t.TempDir(), "sparse-plugin")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	size := formerHashLimit + 4096
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("after-former-limit"), formerHashLimit+128); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	expectedFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	expectedHash := sha256.New()
	if _, err := io.Copy(expectedHash, expectedFile); err != nil {
		_ = expectedFile.Close()
		t.Fatal(err)
	}
	if err := expectedFile.Close(); err != nil {
		t.Fatal(err)
	}

	identity, err := verifyExecutable(path)
	if err != nil {
		t.Fatal(err)
	}
	if identity.SHA256 != fmt.Sprintf("%x", expectedHash.Sum(nil)) || identity.Size != size {
		t.Fatalf("identity = %#v, want sha256=%x size=%d", identity, expectedHash.Sum(nil), size)
	}
}

func TestExecutableIdentityRejectsNonRegularFiles(t *testing.T) {
	directory := t.TempDir()
	fifo := filepath.Join(t.TempDir(), "plugin-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"directory": directory, "fifo": fifo} {
		t.Run(name, func(t *testing.T) {
			result := make(chan error, 1)
			go func() {
				_, err := verifyExecutable(path)
				result <- err
			}()
			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), "regular file") {
					t.Fatalf("error = %v, want regular-file rejection", err)
				}
			case <-time.After(time.Second):
				t.Fatal("verification blocked while opening a non-regular file")
			}
		})
	}
}

func TestStartExecutesTheVerifiedDescriptorAfterExecutablePathExchange(t *testing.T) {
	directory := t.TempDir()
	executablePath := os.Args[0]
	verifiedPath := executablePath + ".verified-path-exchange"
	attackMarker := filepath.Join(directory, "replacement-ran")
	restored := false
	restoreExecutable := func() {
		if restored {
			return
		}
		restored = true
		_ = os.Remove(executablePath)
		_ = os.Rename(verifiedPath, executablePath)
	}
	t.Cleanup(restoreExecutable)
	originalBeforeStart := beforeExecutableStart
	beforeExecutableStart = func(path string) error {
		if renameErr := os.Rename(path, verifiedPath); renameErr != nil {
			return renameErr
		}
		replacement := []byte("#!/bin/sh\nprintf replacement > " + attackMarker + "\nexit 1\n")
		if writeErr := os.WriteFile(path, replacement, 0o700); writeErr != nil {
			return writeErr
		}
		return nil
	}
	t.Cleanup(func() { beforeExecutableStart = originalBeforeStart })

	process, err := Start(context.Background(), "test", Spec{
		ID: "dev.bsbctl.test", Version: "test", Executable: executablePath,
		ProtocolVersion: protocol.Version,
		Args:            []string{"-test.run=^TestPluginHelperProcess$", "-test.bsbctl-plugin-helper=true"},
		ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
		Channels:        []protocol.Channel{{ID: "main"}},
	}, Callbacks{})
	restoreExecutable()
	if err != nil {
		_, markerErr := os.Stat(attackMarker)
		t.Fatalf("Start verified descriptor: %v (replacement marker: %v)", err, markerErr)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := process.Stop(stopCtx); err != nil {
		t.Fatalf("Stop verified descriptor: %v", err)
	}
	if _, err := os.Stat(attackMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement executable ran: %v", err)
	}
}
