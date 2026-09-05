//go:build darwin

package pluginhost

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const darwinProcessIdentityHelperEnvironment = "BSBCTL_TEST_DARWIN_PROCESS_IDENTITY_HELPER"

func TestPrepareExecutableLaunchPreservesValidatedSourceBasename(t *testing.T) {
	const executableName = "bsbctl-plugin-codex-quota"
	path := copyDarwinTestExecutable(t, executableName)
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
	if got := filepath.Base(launch.path); got != executableName {
		t.Fatalf("snapshot executable basename = %q, want %q", got, executableName)
	}
}

func TestPreparedSnapshotPreservesPluginProcessIdentityInPS(t *testing.T) {
	for _, executableName := range []string{"bsbctl-plugin-codex-quota", "bsbctl-plugin-mac-resources"} {
		t.Run(executableName, func(t *testing.T) {
			path := copyDarwinTestExecutable(t, executableName)
			source, identity, err := openVerifiedExecutable(path)
			if err != nil {
				t.Fatal(err)
			}
			launch, err := prepareExecutableLaunch(source, identity)
			if closeErr := source.Close(); err == nil && closeErr != nil {
				err = closeErr
			}
			if err != nil {
				t.Fatal(err)
			}
			snapshotDirectory := filepath.Dir(launch.path)
			readyRead, readyWrite, err := os.Pipe()
			if err != nil {
				launch.cleanup()
				t.Fatal(err)
			}
			stopRead, stopWrite, err := os.Pipe()
			if err != nil {
				_ = readyRead.Close()
				_ = readyWrite.Close()
				launch.cleanup()
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			command := exec.Command(launch.path, "-test.run=^TestDarwinSnapshotProcessIdentityHelper$")
			command.Env = append(os.Environ(), darwinProcessIdentityHelperEnvironment+"=1")
			command.ExtraFiles = []*os.File{readyWrite, stopRead}
			command.Stdout = io.Discard
			command.Stderr = &stderr
			waited := false
			t.Cleanup(func() {
				_ = readyRead.Close()
				_ = readyWrite.Close()
				_ = stopRead.Close()
				_ = stopWrite.Close()
				if command.Process != nil && !waited {
					_ = command.Process.Kill()
					_ = command.Wait()
				}
				launch.cleanup()
			})
			if err := command.Start(); err != nil {
				t.Fatalf("start identity helper: %v", err)
			}
			_ = readyWrite.Close()
			_ = stopRead.Close()
			if err := readyRead.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var readiness [1]byte
			if _, err := io.ReadFull(readyRead, readiness[:]); err != nil || readiness[0] != 1 {
				t.Fatalf("identity helper readiness = %v, byte=%d, stderr=%q", err, readiness[0], stderr.String())
			}
			_ = readyRead.Close()
			output, err := exec.Command("ps", "-p", strconv.Itoa(command.Process.Pid), "-o", "comm=").Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := filepath.Base(strings.TrimSpace(string(output))); got != executableName {
				t.Fatalf("ps command basename = %q, want %q (raw %q)", got, executableName, output)
			}
			if err := stopWrite.Close(); err != nil {
				t.Fatal(err)
			}
			if err := command.Wait(); err != nil {
				t.Fatalf("reap identity helper: %v (stderr=%q)", err, stderr.String())
			}
			waited = true
			if command.ProcessState == nil || !command.ProcessState.Exited() || !command.ProcessState.Success() {
				t.Fatalf("identity helper was not cleanly reaped: %#v", command.ProcessState)
			}
			launch.cleanup()
			if _, err := os.Lstat(snapshotDirectory); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("snapshot directory remains after helper reap: %v", err)
			}
		})
	}
}

func TestDarwinSnapshotProcessIdentityHelper(t *testing.T) {
	if os.Getenv(darwinProcessIdentityHelperEnvironment) != "1" {
		return
	}
	ready := os.NewFile(3, "identity-ready")
	stop := os.NewFile(4, "identity-stop")
	if ready == nil || stop == nil {
		t.Fatal("identity helper descriptors are unavailable")
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := ready.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, stop); err != nil {
		t.Fatal(err)
	}
	if err := stop.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExecutableSnapshotBasenameRejectsReservedDotPathAndOversizedNames(t *testing.T) {
	valid := []string{"bsbctl-plugin-codex-quota", "plugin", "plugin.test", "plugin_1-2"}
	for _, name := range valid {
		if !validExecutableSnapshotBasename(name) {
			t.Errorf("valid basename %q was rejected", name)
		}
	}
	invalid := []string{"", ".", "..", "owner.lock", ".plugin", "bad/name", `bad\name`, "bad name", "plugin:", "é", strings.Repeat("a", 129)}
	for _, name := range invalid {
		if validExecutableSnapshotBasename(name) {
			t.Errorf("unsafe basename %q was accepted", name)
		}
	}
}

func TestCleanupStaleExecutableSnapshotsIsBoundedAndPreservesLiveOrUnsafeDirectories(t *testing.T) {
	root := t.TempDir()
	stale := createExecutableSnapshotFixture(t, root, "snapshot-stale", "bsbctl-plugin-codex-quota", false)
	live := createExecutableSnapshotFixture(t, root, "snapshot-live", "bsbctl-plugin-mac-resources", true)
	unsafe := filepath.Join(root, "snapshot-unsafe")
	if err := os.Mkdir(unsafe, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unsafe, "unexpected"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupStaleExecutableSnapshots(root, 64)
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale snapshot remains: %v", err)
	}
	for name, path := range map[string]string{"live": live, "unsafe": unsafe} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s snapshot was removed: %v", name, err)
		}
	}

	boundedRoot := t.TempDir()
	for index := 0; index < 3; index++ {
		createExecutableSnapshotFixture(t, boundedRoot, "snapshot-bounded-"+string(rune('a'+index)), "plugin", false)
	}
	cleanupStaleExecutableSnapshots(boundedRoot, 1)
	entries, err := os.ReadDir(boundedRoot)
	if err != nil {
		t.Fatal(err)
	}
	bounded := 0
	for _, entry := range entries {
		if len(entry.Name()) >= len("snapshot-bounded-") && entry.Name()[:len("snapshot-bounded-")] == "snapshot-bounded-" {
			bounded++
		}
	}
	if bounded != 2 {
		t.Fatalf("bounded stale snapshots remaining = %d, want 2", bounded)
	}
}

func TestCleanupStaleExecutableSnapshotsPreservesWritableLinkedSymlinkedAndBroadDirectories(t *testing.T) {
	root := t.TempDir()
	writable := createExecutableSnapshotFixture(t, root, "snapshot-writable", "bsbctl-plugin-codex-quota", false)
	if err := os.Chmod(filepath.Join(writable, "bsbctl-plugin-codex-quota"), 0o700); err != nil {
		t.Fatal(err)
	}
	twoExecutables := createExecutableSnapshotFixture(t, root, "snapshot-two", "bsbctl-plugin-codex-quota", false)
	if err := os.WriteFile(filepath.Join(twoExecutables, "bsbctl-plugin-mac-resources"), []byte("second"), 0o500); err != nil {
		t.Fatal(err)
	}
	dotEntry := createExecutableSnapshotFixture(t, root, "snapshot-dot", "", false)
	if err := os.WriteFile(filepath.Join(dotEntry, ".hidden"), []byte("hidden"), 0o500); err != nil {
		t.Fatal(err)
	}
	symlinked := createExecutableSnapshotFixture(t, root, "snapshot-symlink", "", false)
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(symlinked, "bsbctl-plugin-codex-quota")); err != nil {
		t.Fatal(err)
	}
	linked := createExecutableSnapshotFixture(t, root, "snapshot-linked", "", false)
	external := filepath.Join(root, "external-plugin")
	if err := os.WriteFile(external, []byte("external"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(external, filepath.Join(linked, "bsbctl-plugin-codex-quota")); err != nil {
		t.Fatal(err)
	}

	cleanupStaleExecutableSnapshots(root, 64)
	for name, path := range map[string]string{
		"owner-writable":  writable,
		"two executables": twoExecutables,
		"dot entry":       dotEntry,
		"symlink":         symlinked,
		"hard link":       linked,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unsafe %s snapshot was removed: %v", name, err)
		}
	}
}

func createExecutableSnapshotFixture(t *testing.T, root, name, executableName string, live bool) string {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(directory, "owner.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if live {
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = lock.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = lock.Close() })
	} else if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if executableName != "" {
		if err := os.WriteFile(filepath.Join(directory, executableName), []byte("verified"), 0o500); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func copyDarwinTestExecutable(t *testing.T, name string) string {
	t.Helper()
	source, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	path := filepath.Join(t.TempDir(), name)
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
