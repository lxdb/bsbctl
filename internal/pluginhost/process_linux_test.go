//go:build linux

package pluginhost

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
	"golang.org/x/sys/unix"
)

func TestPrepareExecutableLaunchCopiesIntoSealedExecutableMemfd(t *testing.T) {
	path := copyLinuxTestExecutable(t)
	source, identity, err := openVerifiedExecutable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	launch, err := prepareExecutableLaunch(source, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer launch.cleanup()
	if launch.extra == nil || launch.extra == source || launch.path != "/proc/self/fd/4" {
		t.Fatalf("launch = %#v, want an independent descriptor at /proc/self/fd/4", launch)
	}
	wantSeals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	seals, err := unix.FcntlInt(launch.extra.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals&wantSeals != wantSeals {
		t.Fatalf("memfd seals = %#x, %v; want %#x", seals, err, wantSeals)
	}
	if _, err := launch.extra.WriteAt([]byte("changed"), 0); !errors.Is(err, unix.EPERM) {
		t.Fatalf("sealed memfd write error = %v, want EPERM", err)
	}
	if err := launch.extra.Truncate(0); !errors.Is(err, unix.EPERM) {
		t.Fatalf("sealed memfd truncate error = %v, want EPERM", err)
	}
	verified, err := verifyOpenExecutable(launch.extra)
	if err != nil || verified != identity {
		t.Fatalf("sealed memfd identity = %#v, %v; want %#v", verified, err, identity)
	}
}

func TestStartExecutesSealedSnapshotAfterOriginalInodeIsOverwritten(t *testing.T) {
	executablePath := copyLinuxTestExecutable(t)
	attackMarker := filepath.Join(t.TempDir(), "overwritten-inode-ran")
	before, err := os.Stat(executablePath)
	if err != nil {
		t.Fatal(err)
	}
	originalBeforeStart := beforeExecutableStart
	beforeExecutableStart = func(path string) error {
		replacement := []byte("#!/bin/sh\nprintf overwritten > " + attackMarker + "\nexit 1\n")
		if err := os.WriteFile(path, replacement, 0o700); err != nil {
			return err
		}
		after, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !os.SameFile(before, after) {
			return errors.New("test did not mutate the verified inode in place")
		}
		return nil
	}
	t.Cleanup(func() { beforeExecutableStart = originalBeforeStart })

	process, err := Start(context.Background(), "test", linuxHelperSpec(executablePath), Callbacks{})
	if err != nil {
		t.Fatalf("Start sealed snapshot: %v", err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := process.Stop(stopCtx); err != nil {
		t.Fatalf("Stop sealed snapshot: %v", err)
	}
	if _, err := os.Stat(attackMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("overwritten original inode executed: %v", err)
	}
}

func TestStartClosesPluginMemfdAfterFailureAndReap(t *testing.T) {
	if got := openPluginMemfds(t); len(got) != 0 {
		t.Fatalf("preexisting plugin memfds = %v", got)
	}
	executablePath := copyLinuxTestExecutable(t)
	originalBeforeStart := beforeExecutableStart
	beforeExecutableStart = func(string) error {
		if got := openPluginMemfds(t); len(got) != 1 {
			return errors.New("prepared plugin memfd is not uniquely open")
		}
		return errors.New("stop before exec")
	}
	_, err := Start(context.Background(), "test", linuxHelperSpec(executablePath), Callbacks{})
	beforeExecutableStart = originalBeforeStart
	if err == nil {
		t.Fatal("Start succeeded after the injected pre-exec failure")
	}
	if got := openPluginMemfds(t); len(got) != 0 {
		t.Fatalf("plugin memfds after failed start = %v", got)
	}

	executablePath = copyLinuxTestExecutable(t)
	process, err := Start(context.Background(), "test", linuxHelperSpec(executablePath), Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := process.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if got := openPluginMemfds(t); len(got) != 0 {
		t.Fatalf("plugin memfds after child reap = %v", got)
	}
}

func linuxHelperSpec(path string) Spec {
	return Spec{
		ID: "dev.bsbctl.test", Version: "test", Executable: path,
		ProtocolVersion: protocol.Version,
		Args:            []string{"-test.run=^TestPluginHelperProcess$", "-test.bsbctl-plugin-helper=true"},
		ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
		Channels:        []protocol.Channel{{ID: "main"}},
	}
}

func copyLinuxTestExecutable(t *testing.T) string {
	t.Helper()
	source, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	path := filepath.Join(t.TempDir(), "plugin-test")
	destination, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(destination, source); err != nil {
		_ = destination.Close()
		t.Fatal(err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func openPluginMemfds(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && strings.Contains(target, "memfd:bsbctl-plugin") {
			found = append(found, target)
		}
	}
	return found
}
