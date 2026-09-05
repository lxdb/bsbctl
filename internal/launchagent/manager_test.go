package launchagent

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestManagerInstallIsIdempotentAndReloadsOnlyChangedPlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), Label+".plist")
	runner := &fakeLaunchctlRunner{}
	manager := NewManager(runner, 501)
	initial := testLaunchAgentConfig("/usr/local/bin/bsbctl")
	result, err := manager.Install(context.Background(), path, initial)
	if err != nil || result.Status != StateLoaded || !result.PlistMatches || !result.Changed {
		t.Fatalf("first Install = %#v, %v", result, err)
	}
	wantFirst := [][]string{{"print", "gui/501/dev.bsbctl"}, {"bootstrap", "gui/501", path}}
	if !reflect.DeepEqual(runner.calls, wantFirst) {
		t.Fatalf("first calls = %#v, want %#v", runner.calls, wantFirst)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("plist mode = %v, %v", info, err)
	}

	runner.calls = nil
	result, err = manager.Install(context.Background(), path, initial)
	if err != nil || result.Changed {
		t.Fatalf("identical Install = %#v, %v", result, err)
	}
	if want := [][]string{{"print", "gui/501/dev.bsbctl"}}; !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("identical calls = %#v, want %#v", runner.calls, want)
	}

	runner.calls = nil
	changed := initial
	changed.Executable = "/opt/bsbctl/bin/bsbctl"
	result, err = manager.Install(context.Background(), path, changed)
	if err != nil || !result.Changed || result.Status != StateLoaded {
		t.Fatalf("changed Install = %#v, %v", result, err)
	}
	wantChanged := [][]string{{"print", "gui/501/dev.bsbctl"}, {"bootout", "gui/501", path}, {"bootstrap", "gui/501", path}}
	if !reflect.DeepEqual(runner.calls, wantChanged) {
		t.Fatalf("changed calls = %#v, want %#v", runner.calls, wantChanged)
	}
}

func TestManagerRestartReexecutesOnlyAnOwnedLoadedService(t *testing.T) {
	path := filepath.Join(t.TempDir(), Label+".plist")
	if err := Write(path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err != nil {
		t.Fatal(err)
	}
	runner := &fakeLaunchctlRunner{loaded: true}
	result, err := NewManager(runner, 501).Restart(context.Background(), path)
	if err != nil || result.Status != StateLoaded || !result.PlistMatches || !result.Changed {
		t.Fatalf("Restart = %#v, %v", result, err)
	}
	want := [][]string{
		{"print", "gui/501/dev.bsbctl"},
		{"kickstart", "-k", "gui/501/dev.bsbctl"},
		{"print", "gui/501/dev.bsbctl"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("restart calls = %#v, want %#v", runner.calls, want)
	}
}

func TestManagerRestartDoesNotStartAnUnloadedService(t *testing.T) {
	path := filepath.Join(t.TempDir(), Label+".plist")
	if err := Write(path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err != nil {
		t.Fatal(err)
	}
	runner := &fakeLaunchctlRunner{}
	result, err := NewManager(runner, 501).Restart(context.Background(), path)
	if !errors.Is(err, ErrNotLoaded) || result.Status != StateInstalledNotLoaded || !result.PlistMatches || result.Changed {
		t.Fatalf("unloaded Restart = %#v, %v", result, err)
	}
	if want := [][]string{{"print", "gui/501/dev.bsbctl"}}; !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("unloaded restart calls = %#v, want %#v", runner.calls, want)
	}
}

func TestManagerInstallRollsBackPlistOnBootstrapFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), Label+".plist")
	runner := &fakeLaunchctlRunner{bootstrapErr: errors.New("launchctl raw secret output")}
	manager := NewManager(runner, 501)
	result, err := manager.Install(context.Background(), path, testLaunchAgentConfig("/usr/local/bin/bsbctl"))
	if err == nil || errors.Is(err, ErrPartial) || result.Status != StateNotInstalled {
		t.Fatalf("new failed bootstrap = %#v, %v", result, err)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed bootstrap left plist: %v", statErr)
	}
	if containsLaunchAgentError(err, "secret", "raw") {
		t.Fatalf("bootstrap error leaked dependency output: %v", err)
	}
}

func TestManagerChangedLoadedBootstrapFailureRestoresPlistAndReportsPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), Label+".plist")
	if err := Write(path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeLaunchctlRunner{loaded: true, bootstrapErr: errors.New("dependency failure")}
	result, err := NewManager(runner, 501).Install(context.Background(), path, testLaunchAgentConfig("/opt/bsbctl/bin/bsbctl"))
	if !errors.Is(err, ErrPartial) || result.Status != StateInstalledNotLoaded {
		t.Fatalf("changed failed bootstrap = %#v, %v", result, err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("restored plist = %q, %v; want %q", after, readErr, before)
	}
}

func TestManagerUninstallAndStatusUseStableStates(t *testing.T) {
	path := filepath.Join(t.TempDir(), Label+".plist")
	if err := Write(path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err != nil {
		t.Fatal(err)
	}
	runner := &fakeLaunchctlRunner{}
	manager := NewManager(runner, os.Getuid())
	result, err := manager.Status(context.Background(), path)
	if err != nil || result.Status != StateInstalledNotLoaded || !result.PlistMatches {
		t.Fatalf("unloaded Status = %#v, %v", result, err)
	}
	runner.loaded = true
	result, err = manager.Status(context.Background(), path)
	if err != nil || result.Status != StateLoaded || !result.PlistMatches {
		t.Fatalf("loaded Status = %#v, %v", result, err)
	}
	runner.calls = nil
	result, err = manager.Uninstall(context.Background(), path)
	if err != nil || result.Status != StateNotInstalled || result.PlistMatches {
		t.Fatalf("Uninstall = %#v, %v", result, err)
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	want := [][]string{{"print", domain + "/dev.bsbctl"}, {"bootout", domain, path}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("uninstall calls = %#v, want %#v", runner.calls, want)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("plist remains: %v", statErr)
	}
	runner.calls = nil
	result, err = manager.Uninstall(context.Background(), path)
	if err != nil || result.Status != StateNotInstalled {
		t.Fatalf("idempotent Uninstall = %#v, %v", result, err)
	}
}

func TestLaunchctlOutputCaptureIsBoundedAndNeverReturned(t *testing.T) {
	writer := &boundedBuffer{remaining: 16}
	input := bytes.Repeat([]byte("sensitive-launchctl-output"), 8)
	written, err := writer.Write(input)
	if err != nil || written != len(input) || len(writer.data) != 16 || writer.remaining != 0 {
		t.Fatalf("bounded write = %d, %v; captured=%d remaining=%d", written, err, len(writer.data), writer.remaining)
	}
	written, err = writer.Write(input)
	if err != nil || written != len(input) || len(writer.data) != 16 {
		t.Fatalf("overflow write = %d, %v; captured=%d", written, err, len(writer.data))
	}
}

func TestManagerStatusTreatsSymlinkPlistAsDegradedWithoutLaunchctl(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "other.plist")
	path := filepath.Join(directory, Label+".plist")
	if err := os.WriteFile(target, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	runner := &fakeLaunchctlRunner{}
	result, err := NewManager(runner, 501).Status(context.Background(), path)
	if err != nil || result.Status != StateDegraded || result.PlistMatches || len(runner.calls) != 0 {
		t.Fatalf("symlink Status = %#v, %v; calls=%#v", result, err, runner.calls)
	}
}

func TestManagerNeverOverwritesOrRemovesAnotherLaunchAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.plist")
	other := []byte(`<?xml version="1.0"?><plist><dict><key>Label</key><string>com.example.other</string></dict></plist>`)
	if err := os.WriteFile(path, other, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeLaunchctlRunner{}
	manager := NewManager(runner, 501)

	if result, err := manager.Install(context.Background(), path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err == nil || result.Status != StateDegraded {
		t.Fatalf("Install another agent = %#v, %v", result, err)
	}
	if result, err := manager.Uninstall(context.Background(), path); err == nil || result.Status != StateDegraded {
		t.Fatalf("Uninstall another agent = %#v, %v", result, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, other) {
		t.Fatalf("other agent changed = %q, %v", after, err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("launchctl calls = %#v, want none", runner.calls)
	}
}

func TestManagerTreatsWrongModeAsDegradedAndInstallRepairsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), Label+".plist")
	config := testLaunchAgentConfig("/usr/local/bin/bsbctl")
	if err := Write(path, config); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeLaunchctlRunner{}
	manager := NewManager(runner, os.Getuid())
	result, err := manager.Status(context.Background(), path)
	if err != nil || result.Status != StateDegraded || result.PlistMatches {
		t.Fatalf("wrong-mode Status = %#v, %v", result, err)
	}
	result, err = manager.Install(context.Background(), path, config)
	if err != nil || result.Status != StateLoaded || !result.PlistMatches || !result.Changed {
		t.Fatalf("repair Install = %#v, %v", result, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("repaired mode = %v, %v", info, err)
	}
}

func TestManagerTreatsWrongUIDAsForeignWithoutMutationOrLaunchctl(t *testing.T) {
	path := filepath.Join(t.TempDir(), Label+".plist")
	config := testLaunchAgentConfig("/usr/local/bin/bsbctl")
	if err := Write(path, config); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeLaunchctlRunner{}
	manager := NewManager(runner, os.Getuid()+1)
	if result, installErr := manager.Install(context.Background(), path, config); installErr == nil || result.Status != StateDegraded || result.PlistMatches {
		t.Fatalf("wrong-UID Install = %#v, %v", result, installErr)
	}
	if result, uninstallErr := manager.Uninstall(context.Background(), path); uninstallErr == nil || result.Status != StateDegraded || result.PlistMatches {
		t.Fatalf("wrong-UID Uninstall = %#v, %v", result, uninstallErr)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("wrong-UID plist changed = %q, %v", after, err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("wrong-UID launchctl calls = %#v, want none", runner.calls)
	}
}

func TestManagerRepairsLaunchAgentSpecialPermissionBits(t *testing.T) {
	for _, mode := range []uint32{0o4600, 0o2600, 0o1600} {
		t.Run(strconv.FormatUint(uint64(mode), 8), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), Label+".plist")
			config := testLaunchAgentConfig("/usr/local/bin/bsbctl")
			if err := Write(path, config); err != nil {
				t.Fatal(err)
			}
			if err := unix.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			var before unix.Stat_t
			if err := unix.Stat(path, &before); err != nil {
				t.Fatal(err)
			}
			if uint32(before.Mode)&0o7777 != mode {
				t.Skipf("filesystem did not preserve special mode: got %04o, want %04o", uint32(before.Mode)&0o7777, mode)
			}
			runner := &fakeLaunchctlRunner{}
			manager := NewManager(runner, os.Getuid())
			if result, err := manager.Status(context.Background(), path); err != nil || result.Status != StateDegraded || result.PlistMatches {
				t.Fatalf("special-mode Status = %#v, %v", result, err)
			}
			if result, err := manager.Install(context.Background(), path, config); err != nil || result.Status != StateLoaded || !result.PlistMatches || !result.Changed {
				t.Fatalf("special-mode repair = %#v, %v", result, err)
			}
			var after unix.Stat_t
			if err := unix.Stat(path, &after); err != nil || uint32(after.Mode)&0o7777 != 0o600 {
				t.Fatalf("repaired mode = %04o, %v", uint32(after.Mode)&0o7777, err)
			}
		})
	}
}

func TestManagerRejectsFIFOAndUnixSocketPlistsWithoutOpeningThem(t *testing.T) {
	directory := t.TempDir()
	fifoPath := filepath.Join(directory, Label+".fifo")
	if err := unix.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatal(err)
	}
	type installResult struct {
		result Result
		err    error
	}
	runner := &fakeLaunchctlRunner{}
	resultChannel := make(chan installResult, 1)
	go func() {
		result, err := NewManager(runner, os.Getuid()).Install(context.Background(), fifoPath, testLaunchAgentConfig("/usr/local/bin/bsbctl"))
		resultChannel <- installResult{result: result, err: err}
	}()
	stopProbe := make(chan struct{})
	probeConnected := make(chan struct{})
	probeDone := make(chan struct{})
	probeError := make(chan error, 1)
	go probeLaunchAgentFIFOWriter(fifoPath, stopProbe, probeConnected, probeDone, probeError)

	var fifoResult installResult
	select {
	case fifoResult = <-resultChannel:
		close(stopProbe)
		<-probeDone
	case <-probeConnected:
		fifoResult = <-resultChannel
		<-probeDone
		t.Fatalf("manager opened FIFO before classifying it: %#v, %v", fifoResult.result, fifoResult.err)
	case err := <-probeError:
		close(stopProbe)
		<-probeDone
		t.Fatal(err)
	}
	if fifoResult.err == nil || fifoResult.result.Status != StateDegraded || len(runner.calls) != 0 {
		t.Fatalf("FIFO Install = %#v, %v; calls=%#v", fifoResult.result, fifoResult.err, runner.calls)
	}

	socketDirectory, err := os.MkdirTemp("/tmp", "bctl-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, Label+".sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	runner.calls = nil
	result, installErr := NewManager(runner, os.Getuid()).Install(context.Background(), socketPath, testLaunchAgentConfig("/usr/local/bin/bsbctl"))
	if installErr == nil || result.Status != StateDegraded || len(runner.calls) != 0 {
		t.Fatalf("Unix socket Install = %#v, %v; calls=%#v", result, installErr, runner.calls)
	}
}

func probeLaunchAgentFIFOWriter(path string, stop <-chan struct{}, connected chan<- struct{}, done chan<- struct{}, failure chan<- error) {
	defer close(done)
	for {
		select {
		case <-stop:
			return
		default:
		}
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			connected <- struct{}{}
			if closeErr := unix.Close(fd); closeErr != nil {
				failure <- closeErr
			}
			return
		}
		if !errors.Is(err, unix.ENXIO) {
			failure <- err
			return
		}
		runtime.Gosched()
	}
}

func TestManagerRejectsDuplicateNestedAndOversizedLabelsWithoutMutation(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "duplicate top-level label", data: []byte(`<?xml version="1.0"?><plist><dict><key>Label</key><string>dev.bsbctl</string><key>Label</key><string>dev.bsbctl</string></dict></plist>`)},
		{name: "nested label", data: []byte(`<?xml version="1.0"?><plist><dict><key>Label</key><string>dev.bsbctl</string><key>Nested</key><dict><key>Label</key><string>dev.bsbctl</string></dict></dict></plist>`)},
		{name: "oversized", data: append([]byte(`<?xml version="1.0"?><plist><dict><key>Label</key><string>dev.bsbctl</string>`), bytes.Repeat([]byte("x"), maxLaunchAgentPlistBytes)...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), Label+".plist")
			if err := os.WriteFile(path, test.data, 0o600); err != nil {
				t.Fatal(err)
			}
			before := append([]byte(nil), test.data...)
			runner := &fakeLaunchctlRunner{}
			manager := NewManager(runner, os.Getuid())
			if result, err := manager.Status(context.Background(), path); err != nil || result.Status != StateDegraded || result.PlistMatches {
				t.Fatalf("Status = %#v, %v", result, err)
			}
			if result, err := manager.Install(context.Background(), path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err == nil || result.Status != StateDegraded {
				t.Fatalf("Install = %#v, %v", result, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, before) || len(runner.calls) != 0 {
				t.Fatalf("foreign plist changed=%t err=%v calls=%#v", !bytes.Equal(after, before), err, runner.calls)
			}
		})
	}
}

func TestManagerRechecksPinnedIdentityBeforeRemoval(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, Label+".plist")
	if err := Write(path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err != nil {
		t.Fatal(err)
	}
	foreignPath := filepath.Join(directory, "foreign.plist")
	foreign := []byte(`<?xml version="1.0"?><plist><dict><key>Label</key><string>com.example.foreign</string></dict></plist>`)
	if err := os.WriteFile(foreignPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(&fakeLaunchctlRunner{}, os.Getuid())
	manager.beforeMutation = func() {
		if err := os.Rename(foreignPath, path); err != nil {
			t.Fatal(err)
		}
		manager.beforeMutation = nil
	}
	result, err := manager.Uninstall(context.Background(), path)
	if err == nil || result.Status != StateDegraded {
		t.Fatalf("Uninstall swapped path = %#v, %v", result, err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, foreign) {
		t.Fatalf("foreign replacement touched = %q, %v", after, readErr)
	}
}

func TestManagerRechecksPinnedIdentityBeforeReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, Label+".plist")
	if err := Write(path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err != nil {
		t.Fatal(err)
	}
	foreignPath := filepath.Join(directory, "foreign.plist")
	foreign := []byte(`<?xml version="1.0"?><plist><dict><key>Label</key><string>com.example.foreign</string></dict></plist>`)
	if err := os.WriteFile(foreignPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(&fakeLaunchctlRunner{}, os.Getuid())
	manager.beforeMutation = func() {
		if err := os.Rename(foreignPath, path); err != nil {
			t.Fatal(err)
		}
		manager.beforeMutation = nil
	}
	result, err := manager.Install(context.Background(), path, testLaunchAgentConfig("/opt/bsbctl/bin/bsbctl"))
	if err == nil || result.Status != StateDegraded {
		t.Fatalf("Install swapped path = %#v, %v", result, err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, foreign) {
		t.Fatalf("foreign replacement touched = %q, %v", after, readErr)
	}
}

func TestManagerAtomicMutationRestoresEntrySwappedAfterValidation(t *testing.T) {
	foreign := []byte(`<?xml version="1.0"?><plist><dict><key>Label</key><string>com.example.foreign</string></dict></plist>`)
	t.Run("replacement", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, Label+".plist")
		if err := Write(path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err != nil {
			t.Fatal(err)
		}
		foreignPath := filepath.Join(directory, "foreign.plist")
		if err := os.WriteFile(foreignPath, foreign, 0o600); err != nil {
			t.Fatal(err)
		}
		runner := &fakeLaunchctlRunner{}
		manager := NewManager(runner, os.Getuid())
		exchanges := 0
		manager.writeOptions.exchange = func(directoryFD int, first, second string) error {
			if exchanges == 0 {
				if err := os.Rename(foreignPath, path); err != nil {
					t.Fatal(err)
				}
			}
			exchanges++
			return platformExchange(directoryFD, first, second)
		}
		result, err := manager.Install(context.Background(), path, testLaunchAgentConfig("/opt/bsbctl/bin/bsbctl"))
		if err == nil || errors.Is(err, ErrPartial) || result.Status != StateDegraded || exchanges != 2 {
			t.Fatalf("swapped replacement = %#v, %v; exchanges=%d", result, err, exchanges)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(after, foreign) {
			t.Fatalf("foreign replacement restored = %q, %v", after, readErr)
		}
		assertNoDestructiveLaunchctl(t, runner.calls)
	})

	t.Run("uninstall", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, Label+".plist")
		if err := Write(path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err != nil {
			t.Fatal(err)
		}
		foreignPath := filepath.Join(directory, "foreign.plist")
		if err := os.WriteFile(foreignPath, foreign, 0o600); err != nil {
			t.Fatal(err)
		}
		runner := &fakeLaunchctlRunner{}
		manager := NewManager(runner, os.Getuid())
		renames := 0
		manager.removeOptions.renameExclusive = func(directoryFD int, oldName, newName string) error {
			if renames == 0 {
				if err := os.Rename(foreignPath, path); err != nil {
					t.Fatal(err)
				}
			}
			renames++
			return platformRenameExclusive(directoryFD, oldName, newName)
		}
		result, err := manager.Uninstall(context.Background(), path)
		if err == nil || errors.Is(err, ErrPartial) || result.Status != StateDegraded || renames != 2 {
			t.Fatalf("swapped uninstall = %#v, %v; renames=%d", result, err, renames)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(after, foreign) {
			t.Fatalf("foreign uninstall target restored = %q, %v", after, readErr)
		}
		assertNoDestructiveLaunchctl(t, runner.calls)
	})
}

func TestManagerAtomicMutationPreservesDisplacedEntryOnRestoreFailure(t *testing.T) {
	foreign := []byte(`<?xml version="1.0"?><plist><dict><key>Label</key><string>com.example.foreign</string></dict></plist>`)
	t.Run("replacement", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, Label+".plist")
		if err := Write(path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err != nil {
			t.Fatal(err)
		}
		foreignPath := filepath.Join(directory, "foreign.plist")
		if err := os.WriteFile(foreignPath, foreign, 0o600); err != nil {
			t.Fatal(err)
		}
		manager := NewManager(&fakeLaunchctlRunner{}, os.Getuid())
		exchanges := 0
		manager.writeOptions.exchange = func(directoryFD int, first, second string) error {
			if exchanges == 0 {
				if err := os.Rename(foreignPath, path); err != nil {
					t.Fatal(err)
				}
				exchanges++
				return platformExchange(directoryFD, first, second)
			}
			exchanges++
			return errors.New("restore exchange failed")
		}
		result, err := manager.Install(context.Background(), path, testLaunchAgentConfig("/opt/bsbctl/bin/bsbctl"))
		if !errors.Is(err, ErrPartial) || result.Status != StateInstalledNotLoaded || !result.PlistMatches || !result.Changed {
			t.Fatalf("replacement restore failure = %#v, %v", result, err)
		}
		if !directoryContainsData(t, directory, foreign) {
			t.Fatal("foreign replacement was not preserved in quarantine")
		}
	})

	t.Run("uninstall", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, Label+".plist")
		if err := Write(path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err != nil {
			t.Fatal(err)
		}
		foreignPath := filepath.Join(directory, "foreign.plist")
		if err := os.WriteFile(foreignPath, foreign, 0o600); err != nil {
			t.Fatal(err)
		}
		manager := NewManager(&fakeLaunchctlRunner{}, os.Getuid())
		renames := 0
		manager.removeOptions.renameExclusive = func(directoryFD int, oldName, newName string) error {
			if renames == 0 {
				if err := os.Rename(foreignPath, path); err != nil {
					t.Fatal(err)
				}
				renames++
				return platformRenameExclusive(directoryFD, oldName, newName)
			}
			renames++
			return errors.New("restore rename failed")
		}
		result, err := manager.Uninstall(context.Background(), path)
		if !errors.Is(err, ErrPartial) || result.Status != StateNotInstalled || !result.Changed {
			t.Fatalf("uninstall restore failure = %#v, %v", result, err)
		}
		if !directoryContainsData(t, directory, foreign) {
			t.Fatal("foreign uninstall target was not preserved in quarantine")
		}
	})
}

func assertNoDestructiveLaunchctl(t *testing.T, calls [][]string) {
	t.Helper()
	for _, call := range calls {
		if len(call) != 0 && (call[0] == "bootout" || call[0] == "bootstrap") {
			t.Fatalf("destructive launchctl call = %#v", call)
		}
	}
}

func directoryContainsData(t *testing.T, directory string, wanted []byte) bool {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
		if readErr == nil && bytes.Equal(data, wanted) {
			return true
		}
	}
	return false
}

func TestManagerReportsAtomicPlistCommitOutcomesFromActualState(t *testing.T) {
	t.Run("pre-rename failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), Label+".plist")
		manager := NewManager(&fakeLaunchctlRunner{}, os.Getuid())
		manager.writeOptions.renameExclusive = func(int, string, string) error { return errors.New("rename failed") }
		result, err := manager.Install(context.Background(), path, testLaunchAgentConfig("/usr/local/bin/bsbctl"))
		if err == nil || errors.Is(err, ErrPartial) || result.Status != StateNotInstalled || result.Changed {
			t.Fatalf("pre-rename Install = %#v, %v", result, err)
		}
	})

	t.Run("post-rename parent sync failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), Label+".plist")
		manager := NewManager(&fakeLaunchctlRunner{}, os.Getuid())
		manager.writeOptions.syncDirectory = func(*os.File) error { return errors.New("sync failed") }
		result, err := manager.Install(context.Background(), path, testLaunchAgentConfig("/usr/local/bin/bsbctl"))
		if !errors.Is(err, ErrPartial) || result.Status != StateInstalledNotLoaded || !result.PlistMatches || !result.Changed {
			t.Fatalf("post-rename Install = %#v, %v", result, err)
		}
	})

	t.Run("post-remove parent sync failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), Label+".plist")
		if err := Write(path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err != nil {
			t.Fatal(err)
		}
		manager := NewManager(&fakeLaunchctlRunner{}, os.Getuid())
		manager.removeOptions.syncDirectory = func(int) error { return errors.New("sync failed") }
		result, err := manager.Uninstall(context.Background(), path)
		if !errors.Is(err, ErrPartial) || result.Status != StateNotInstalled || !result.Changed {
			t.Fatalf("post-remove Uninstall = %#v, %v", result, err)
		}
	})
}

func TestManagerReportsRollbackCommitOutcomesAndBootoutAmbiguity(t *testing.T) {
	tests := []struct {
		name              string
		configureManager  func(*Manager)
		configureRunner   func(*fakeLaunchctlRunner)
		wantPreviousPlist bool
		wantStatus        State
		wantPartial       bool
	}{
		{
			name: "rollback not committed", wantStatus: StateInstalledNotLoaded, wantPartial: true,
			configureManager: func(manager *Manager) {
				calls := 0
				manager.writeOptions.exchange = func(directoryFD int, oldName, newName string) error {
					calls++
					if calls == 2 {
						return errors.New("rollback rename failed")
					}
					return platformExchange(directoryFD, oldName, newName)
				}
			},
		},
		{
			name: "rollback durability uncertain", wantPreviousPlist: true, wantStatus: StateInstalledNotLoaded, wantPartial: true,
			configureManager: func(manager *Manager) {
				calls := 0
				manager.writeOptions.syncDirectory = func(file *os.File) error {
					calls++
					if calls == 2 {
						return errors.New("rollback sync failed")
					}
					return file.Sync()
				}
			},
		},
		{
			name: "bootout failure after unloading is reconciled", wantPreviousPlist: true, wantStatus: StateLoaded,
			configureRunner: func(runner *fakeLaunchctlRunner) {
				runner.bootoutErr = errors.New("ambiguous bootout")
				runner.bootoutBeforeError = true
				runner.bootstrapErr = nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), Label+".plist")
			oldConfig := testLaunchAgentConfig("/usr/local/bin/bsbctl")
			newConfig := testLaunchAgentConfig("/opt/bsbctl/bin/bsbctl")
			if err := Write(path, oldConfig); err != nil {
				t.Fatal(err)
			}
			previous, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			runner := &fakeLaunchctlRunner{loaded: true, bootstrapErr: errors.New("bootstrap failed")}
			if test.configureRunner != nil {
				test.configureRunner(runner)
			}
			manager := NewManager(runner, os.Getuid())
			if test.configureManager != nil {
				test.configureManager(manager)
			}
			result, installErr := manager.Install(context.Background(), path, newConfig)
			if installErr == nil || errors.Is(installErr, ErrPartial) != test.wantPartial || result.Status != test.wantStatus {
				t.Fatalf("Install = %#v, %v", result, installErr)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if test.wantPreviousPlist != bytes.Equal(after, previous) {
				t.Fatalf("previous plist restored = %t, want %t", bytes.Equal(after, previous), test.wantPreviousPlist)
			}
		})
	}
}

func TestManagerRequeriesStateAfterAmbiguousUninstallBootout(t *testing.T) {
	path := filepath.Join(t.TempDir(), Label+".plist")
	if err := Write(path, testLaunchAgentConfig("/usr/local/bin/bsbctl")); err != nil {
		t.Fatal(err)
	}
	runner := &fakeLaunchctlRunner{loaded: true, bootoutErr: errors.New("ambiguous bootout"), bootoutBeforeError: true}
	result, err := NewManager(runner, os.Getuid()).Uninstall(context.Background(), path)
	if !errors.Is(err, ErrPartial) || result.Status != StateInstalledNotLoaded || !result.PlistMatches {
		t.Fatalf("Uninstall = %#v, %v", result, err)
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if got := runner.calls[len(runner.calls)-1]; !reflect.DeepEqual(got, []string{"print", domain + "/dev.bsbctl"}) {
		t.Fatalf("last launchctl call = %#v, want state requery", got)
	}
}

func TestManagerAlreadyCanceledContextDoesNotCreateLifecycleStateOrCallRunner(t *testing.T) {
	operations := []struct {
		name string
		run  func(*Manager, context.Context, string) (Result, error)
	}{
		{
			name: "install",
			run: func(manager *Manager, ctx context.Context, path string) (Result, error) {
				return manager.Install(ctx, path, testLaunchAgentConfig("/usr/local/bin/bsbctl"))
			},
		},
		{
			name: "uninstall",
			run: func(manager *Manager, ctx context.Context, path string) (Result, error) {
				return manager.Uninstall(ctx, path)
			},
		},
		{
			name: "status",
			run: func(manager *Manager, ctx context.Context, path string) (Result, error) {
				return manager.Status(ctx, path)
			},
		},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "LaunchAgents")
			path := filepath.Join(directory, Label+".plist")
			runner := &fakeLaunchctlRunner{}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			result, err := operation.run(NewManager(runner, os.Getuid()), ctx, path)

			if err != context.Canceled || result.Status != StateDegraded {
				t.Fatalf("%s with canceled context = %#v, %v", operation.name, result, err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("%s launchctl calls = %#v", operation.name, runner.calls)
			}
			if _, statErr := os.Lstat(directory); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("%s created lifecycle state: %v", operation.name, statErr)
			}
		})
	}
}

func TestManagerLifecycleLockWaitHonorsCancellationAndDeadlineWithoutOverlappingOwner(t *testing.T) {
	tests := []struct {
		name       string
		newContext func() (context.Context, func(), func())
		wantErr    error
	}{
		{
			name: "canceled",
			newContext: func() (context.Context, func(), func()) {
				ctx, cancel := context.WithCancel(context.Background())
				return ctx, cancel, cancel
			},
			wantErr: context.Canceled,
		},
		{
			name: "deadline exceeded",
			newContext: func() (context.Context, func(), func()) {
				ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				return ctx, func() {}, cancel
			},
			wantErr: context.DeadlineExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, Label+".plist")
			oldConfig := testLaunchAgentConfig("/usr/local/bin/bsbctl-old")
			firstConfig := testLaunchAgentConfig("/usr/local/bin/bsbctl-first")
			secondConfig := testLaunchAgentConfig("/usr/local/bin/bsbctl-second")
			if err := Write(path, oldConfig); err != nil {
				t.Fatal(err)
			}
			oldData, err := render(oldConfig)
			if err != nil {
				t.Fatal(err)
			}
			firstData, err := render(firstConfig)
			if err != nil {
				t.Fatal(err)
			}

			ownerEntered := make(chan struct{})
			releaseOwner := make(chan struct{})
			var ownerEnteredOnce sync.Once
			var releaseOwnerOnce sync.Once
			first := NewManager(&fakeLaunchctlRunner{}, os.Getuid())
			first.beforeMutation = func() {
				ownerEnteredOnce.Do(func() { close(ownerEntered) })
				<-releaseOwner
			}
			t.Cleanup(func() { releaseOwnerOnce.Do(func() { close(releaseOwner) }) })

			type installResponse struct {
				result Result
				err    error
			}
			firstDone := make(chan installResponse, 1)
			go func() {
				result, installErr := first.Install(context.Background(), path, firstConfig)
				firstDone <- installResponse{result: result, err: installErr}
			}()
			select {
			case <-ownerEntered:
			case <-time.After(2 * time.Second):
				t.Fatal("first manager did not enter its mutation barrier")
			}

			lockPath := filepath.Join(directory, lifecycleLockName(filepath.Base(path)))

			ctx, trigger, cancel := test.newContext()
			t.Cleanup(cancel)
			contenderRunner := &fakeLaunchctlRunner{}
			contender := NewManager(contenderRunner, os.Getuid())
			type descriptorObservation struct {
				identities map[int]fileIdentity
				err        error
			}
			descriptorsOpened := make(chan descriptorObservation, 1)
			contender.lockOptions.observeDescriptors = func(directoryFD, lockFD int) {
				observation := descriptorObservation{identities: make(map[int]fileIdentity, 2)}
				for _, fd := range []int{directoryFD, lockFD} {
					var stat unix.Stat_t
					if err := unix.Fstat(fd, &stat); err != nil {
						observation.err = errors.Join(observation.err, err)
						continue
					}
					observation.identities[fd] = identityFromStat(stat)
				}
				descriptorsOpened <- observation
			}
			waitEntered := make(chan struct{})
			inspectDescriptors := make(chan struct{})
			var inspectDescriptorsOnce sync.Once
			t.Cleanup(func() { inspectDescriptorsOnce.Do(func() { close(inspectDescriptors) }) })
			var waitEnteredOnce sync.Once
			waitCalls := 0
			invalidDelay := false
			contender.lockOptions.waitForRetry = func(waitCtx context.Context, delay time.Duration) error {
				waitCalls++
				if delay <= 0 || delay > 50*time.Millisecond {
					invalidDelay = true
				}
				waitEnteredOnce.Do(func() {
					close(waitEntered)
					<-inspectDescriptors
				})
				return waitForLifecycleLockRetry(waitCtx, delay)
			}
			secondDone := make(chan installResponse, 1)
			go func() {
				result, installErr := contender.Install(ctx, path, secondConfig)
				secondDone <- installResponse{result: result, err: installErr}
			}()
			select {
			case <-waitEntered:
			case <-time.After(2 * time.Second):
				t.Fatal("second manager did not reach the contended-lock retry wait")
			}

			observation := <-descriptorsOpened
			if observation.err != nil {
				t.Fatal(observation.err)
			}
			contenderDescriptors := observation.identities
			if len(contenderDescriptors) != 2 {
				t.Fatalf("contender descriptors = %#v, want pinned directory and lock", contenderDescriptors)
			}

			trigger()
			inspectDescriptorsOnce.Do(func() { close(inspectDescriptors) })
			var secondResponse installResponse
			select {
			case secondResponse = <-secondDone:
			case <-time.After(2 * time.Second):
				t.Fatal("second manager did not return promptly after context completion")
			}
			if secondResponse.err != test.wantErr || secondResponse.result.Status != StateDegraded {
				t.Fatalf("second Install = %#v, %v; want exact %v", secondResponse.result, secondResponse.err, test.wantErr)
			}
			if waitCalls < 1 || invalidDelay {
				t.Fatalf("lock retry waits = %d, invalid delay = %t", waitCalls, invalidDelay)
			}
			if len(contenderRunner.calls) != 0 {
				t.Fatalf("canceled contender launchctl calls = %#v", contenderRunner.calls)
			}
			if actual, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(actual, oldData) {
				t.Fatalf("target changed while owner was blocked = %q, %v", actual, readErr)
			}
			for fd, identity := range contenderDescriptors {
				var descriptorStat unix.Stat_t
				if statErr := unix.Fstat(fd, &descriptorStat); statErr == nil && identityFromStat(descriptorStat) == identity {
					t.Errorf("contender descriptor %d remained open after cancellation", fd)
				} else if statErr != nil && !errors.Is(statErr, unix.EBADF) {
					t.Errorf("inspect contender descriptor %d: %v", fd, statErr)
				}
			}

			lockFD, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
				_ = unix.Close(lockFD)
				t.Fatalf("first manager lost lifecycle ownership before release: %v", err)
			}
			if err := unix.Close(lockFD); err != nil {
				t.Fatal(err)
			}

			releaseOwnerOnce.Do(func() { close(releaseOwner) })
			var firstResponse installResponse
			select {
			case firstResponse = <-firstDone:
			case <-time.After(2 * time.Second):
				t.Fatal("first manager did not finish after explicit release")
			}
			if firstResponse.err != nil || firstResponse.result.Status != StateLoaded || !firstResponse.result.Changed {
				t.Fatalf("first Install = %#v, %v", firstResponse.result, firstResponse.err)
			}
			if actual, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(actual, firstData) {
				t.Fatalf("first manager final plist = %q, %v", actual, readErr)
			}
			lockFD, err = unix.Open(lockPath, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
				_ = unix.Close(lockFD)
				t.Fatalf("lifecycle lock remained held after owner release: %v", err)
			}
			if err := errors.Join(unix.Flock(lockFD, unix.LOCK_UN), unix.Close(lockFD)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIndependentManagersSerializeEntireLifecycleAndSecondObservesFirstCommit(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, Label+".plist")
	firstConfig := testLaunchAgentConfig("/usr/local/bin/bsbctl-first")
	secondConfig := testLaunchAgentConfig("/usr/local/bin/bsbctl-second")
	runner := &lockedLaunchctlRunner{}
	first := NewManager(runner, os.Getuid())
	second := NewManager(runner, os.Getuid())
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	first.beforeMutation = func() {
		close(firstEntered)
		<-releaseFirst
	}
	secondAttempted := make(chan struct{})
	second.lockOptions.beforeLock = func() { close(secondAttempted) }

	type installResponse struct {
		result Result
		err    error
	}
	firstDone := make(chan installResponse, 1)
	go func() {
		result, err := first.Install(context.Background(), path, firstConfig)
		firstDone <- installResponse{result: result, err: err}
	}()
	<-firstEntered

	lockPath := filepath.Join(directory, lifecycleLockName(filepath.Base(path)))
	lockFD, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		_ = unix.Close(lockFD)
		t.Fatalf("independent descriptor acquired held lifecycle lock: %v", err)
	}
	if err := unix.Close(lockFD); err != nil {
		t.Fatal(err)
	}

	secondDone := make(chan installResponse, 1)
	go func() {
		result, err := second.Install(context.Background(), path, secondConfig)
		secondDone <- installResponse{result: result, err: err}
	}()
	<-secondAttempted
	close(releaseFirst)

	firstResponse := <-firstDone
	secondResponse := <-secondDone
	if firstResponse.err != nil || firstResponse.result.Status != StateLoaded || !firstResponse.result.Changed {
		t.Fatalf("first Install = %#v, %v", firstResponse.result, firstResponse.err)
	}
	if secondResponse.err != nil || secondResponse.result.Status != StateLoaded || !secondResponse.result.Changed {
		t.Fatalf("second Install = %#v, %v", secondResponse.result, secondResponse.err)
	}
	desired, err := render(secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, desired) {
		t.Fatalf("final plist = %q, %v; want second config", actual, err)
	}
	wantCalls := [][]string{
		{"print", "gui/" + strconv.Itoa(os.Getuid()) + "/dev.bsbctl"},
		{"bootstrap", "gui/" + strconv.Itoa(os.Getuid()), path},
		{"print", "gui/" + strconv.Itoa(os.Getuid()) + "/dev.bsbctl"},
		{"bootout", "gui/" + strconv.Itoa(os.Getuid()), path},
		{"bootstrap", "gui/" + strconv.Itoa(os.Getuid()), path},
	}
	if calls := runner.Calls(); !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("serialized calls = %#v, want %#v", calls, wantCalls)
	}
	var lockStat unix.Stat_t
	if err := unix.Lstat(lockPath, &lockStat); err != nil || lockStat.Mode&unix.S_IFMT != unix.S_IFREG || uint32(lockStat.Mode)&0o7777 != 0o600 || int(lockStat.Uid) != os.Getuid() || lockStat.Size > maxLifecycleLockBytes {
		t.Fatalf("lock identity = %#v, %v", lockStat, err)
	}
}

func TestManagerRejectsUnsafeLifecycleLockWithoutInspectingOrMutatingPlist(t *testing.T) {
	for _, test := range []struct {
		name       string
		managerUID int
		setup      func(*testing.T, string)
	}{
		{name: "symlink", setup: func(t *testing.T, path string) {
			target := filepath.Join(filepath.Dir(path), "foreign-lock")
			if err := os.WriteFile(target, []byte("foreign"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "fifo", setup: func(t *testing.T, path string) {
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong mode", setup: func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "oversized", setup: func(t *testing.T, path string) {
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxLifecycleLockBytes+1), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong owner", managerUID: os.Getuid() + 1, setup: func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, Label+".plist")
			lockPath := filepath.Join(directory, lifecycleLockName(filepath.Base(path)))
			test.setup(t, lockPath)
			runner := &fakeLaunchctlRunner{}
			managerUID := test.managerUID
			if managerUID == 0 {
				managerUID = os.Getuid()
			}
			result, err := NewManager(runner, managerUID).Install(context.Background(), path, testLaunchAgentConfig("/usr/local/bin/bsbctl"))
			if err == nil || result.Status != StateDegraded {
				t.Fatalf("Install with unsafe lock = %#v, %v", result, err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("launchctl calls = %#v", runner.calls)
			}
			if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("plist mutated with unsafe lock: %v", statErr)
			}
		})
	}
}

func TestManagerUsesPinnedParentForInspectionMutationAndResultWhenDirectoryPathIsReplaced(t *testing.T) {
	root := t.TempDir()
	activeDirectory := filepath.Join(root, "LaunchAgents")
	pinnedDirectory := filepath.Join(root, "pinned-LaunchAgents")
	replacementDirectory := filepath.Join(root, "replacement-LaunchAgents")
	if err := os.Mkdir(activeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(replacementDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(activeDirectory, Label+".plist")
	replacementPath := filepath.Join(replacementDirectory, Label+".plist")
	oldConfig := testLaunchAgentConfig("/usr/local/bin/bsbctl-old")
	newConfig := testLaunchAgentConfig("/usr/local/bin/bsbctl-new")
	oldData, err := render(oldConfig)
	if err != nil {
		t.Fatal(err)
	}
	newData, err := render(newConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, oldData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacementPath, oldData, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeLaunchctlRunner{}
	manager := NewManager(runner, os.Getuid())
	manager.lockOptions.afterLock = func() {
		if err := os.Rename(activeDirectory, pinnedDirectory); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacementDirectory, activeDirectory); err != nil {
			t.Fatal(err)
		}
	}
	result, installErr := manager.Install(context.Background(), path, newConfig)
	if !errors.Is(installErr, ErrPartial) || !result.Changed || result.Status != StateDegraded {
		t.Fatalf("Install with replaced parent = %#v, %v", result, installErr)
	}
	visibleData, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(visibleData, oldData) {
		t.Fatalf("replacement-directory plist changed = %q, %v", visibleData, err)
	}
	pinnedData, err := os.ReadFile(filepath.Join(pinnedDirectory, Label+".plist"))
	if err != nil || !bytes.Equal(pinnedData, newData) {
		t.Fatalf("pinned-directory plist = %q, %v; want desired", pinnedData, err)
	}
	assertNoDestructiveLaunchctl(t, runner.calls)
}

func TestDescriptorRelativeTempCleanupNeverDeletesReplacementDirectorySameName(t *testing.T) {
	root := t.TempDir()
	activeDirectory := filepath.Join(root, "LaunchAgents")
	pinnedDirectory := filepath.Join(root, "pinned-LaunchAgents")
	replacementDirectory := filepath.Join(root, "replacement-LaunchAgents")
	if err := os.Mkdir(activeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(replacementDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(activeDirectory, Label+".plist")
	oldData, err := render(testLaunchAgentConfig("/usr/local/bin/bsbctl-old"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, oldData, 0o600); err != nil {
		t.Fatal(err)
	}
	foreignTarget := []byte(`<?xml version="1.0"?><plist><dict><key>Label</key><string>com.example.foreign</string></dict></plist>`)
	if err := os.WriteFile(filepath.Join(replacementDirectory, Label+".plist"), foreignTarget, 0o600); err != nil {
		t.Fatal(err)
	}
	foreignTemp := []byte("replacement-directory foreign temp sentinel")
	var tempName string
	manager := NewManager(&fakeLaunchctlRunner{}, os.Getuid())
	manager.beforeMutation = func() {
		if err := os.Rename(activeDirectory, pinnedDirectory); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacementDirectory, activeDirectory); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(pinnedDirectory)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".bsbctl-launchagent-") && !strings.Contains(entry.Name(), "lock-") && !strings.Contains(entry.Name(), "quarantine-") {
				tempName = entry.Name()
				break
			}
		}
		if tempName == "" {
			t.Fatal("descriptor-relative temp entry was not present before mutation")
		}
		if err := os.WriteFile(filepath.Join(activeDirectory, tempName), foreignTemp, 0o600); err != nil {
			t.Fatal(err)
		}
		manager.beforeMutation = nil
	}
	manager.writeOptions.exchange = func(int, string, string) error { return errors.New("injected exchange failure") }
	result, installErr := manager.Install(context.Background(), path, testLaunchAgentConfig("/usr/local/bin/bsbctl-new"))
	if !errors.Is(installErr, ErrPartial) || result.Status != StateDegraded {
		t.Fatalf("Install with replaced cleanup parent = %#v, %v", result, installErr)
	}
	replacementTemp, err := os.ReadFile(filepath.Join(activeDirectory, tempName))
	if err != nil || !bytes.Equal(replacementTemp, foreignTemp) {
		t.Fatalf("replacement-directory same-name entry changed = %q, %v", replacementTemp, err)
	}
	replacementTarget, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(replacementTarget, foreignTarget) {
		t.Fatalf("replacement-directory target changed = %q, %v", replacementTarget, err)
	}
	entries, err := os.ReadDir(pinnedDirectory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == tempName {
			t.Fatalf("owned temp remained in pinned directory: %q", tempName)
		}
	}
}

func TestLifecycleLockSerializesCooperatingReplacementAcrossFinalCleanup(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, Label+".plist")
	oldConfig := testLaunchAgentConfig("/usr/local/bin/bsbctl-old")
	firstConfig := testLaunchAgentConfig("/usr/local/bin/bsbctl-first")
	secondConfig := testLaunchAgentConfig("/usr/local/bin/bsbctl-second")
	oldData, err := render(oldConfig)
	if err != nil {
		t.Fatal(err)
	}
	firstData, err := render(firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, oldData, 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := []byte(`<?xml version="1.0"?><plist><dict><key>Label</key><string>com.example.foreign</string></dict></plist>`)
	foreignPath := filepath.Join(directory, "foreign.plist")
	if err := os.WriteFile(foreignPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &lockedLaunchctlRunner{}
	first := NewManager(runner, os.Getuid())
	cleanupEntered := make(chan struct{})
	releaseCleanup := make(chan struct{})
	first.writeOptions.unlink = func(directoryFD int, name string) error {
		close(cleanupEntered)
		<-releaseCleanup
		return unix.Unlinkat(directoryFD, name, 0)
	}
	second := NewManager(runner, os.Getuid())
	secondAttempted := make(chan struct{})
	second.lockOptions.beforeLock = func() { close(secondAttempted) }
	second.beforeMutation = func() {
		if err := os.Rename(foreignPath, path); err != nil {
			t.Fatal(err)
		}
		second.beforeMutation = nil
	}

	type response struct {
		result Result
		err    error
	}
	firstDone := make(chan response, 1)
	go func() {
		result, err := first.Install(context.Background(), path, firstConfig)
		firstDone <- response{result: result, err: err}
	}()
	<-cleanupEntered
	secondDone := make(chan response, 1)
	go func() {
		result, err := second.Install(context.Background(), path, secondConfig)
		secondDone <- response{result: result, err: err}
	}()
	<-secondAttempted

	lockPath := filepath.Join(directory, lifecycleLockName(filepath.Base(path)))
	lockFD, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		_ = unix.Close(lockFD)
		t.Fatalf("cleanup lifecycle lock was not held: %v", err)
	}
	if err := unix.Close(lockFD); err != nil {
		t.Fatal(err)
	}
	duringCleanup, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(duringCleanup, firstData) {
		t.Fatalf("second manager overlapped first cleanup = %q, %v", duringCleanup, err)
	}
	close(releaseCleanup)

	firstResponse := <-firstDone
	secondResponse := <-secondDone
	if firstResponse.err != nil || firstResponse.result.Status != StateLoaded || !firstResponse.result.Changed {
		t.Fatalf("first Install = %#v, %v", firstResponse.result, firstResponse.err)
	}
	if secondResponse.err == nil || errors.Is(secondResponse.err, ErrPartial) || secondResponse.result.Status != StateDegraded {
		t.Fatalf("second swapped Install = %#v, %v", secondResponse.result, secondResponse.err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, foreign) {
		t.Fatalf("foreign cooperating replacement changed = %q, %v", after, err)
	}
	assertNoUnexpectedPrivateEntries(t, directory)
}

func assertNoUnexpectedPrivateEntries(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".bsbctl-launchagent-temp-") || strings.HasPrefix(entry.Name(), ".bsbctl-launchagent-quarantine-") {
			t.Fatalf("unexpected private entry remains: %q", entry.Name())
		}
	}
}

type lockedLaunchctlRunner struct {
	mutex  sync.Mutex
	loaded bool
	calls  [][]string
}

func (runner *lockedLaunchctlRunner) Run(_ context.Context, args ...string) error {
	runner.mutex.Lock()
	defer runner.mutex.Unlock()
	runner.calls = append(runner.calls, append([]string(nil), args...))
	switch args[0] {
	case "print":
		if !runner.loaded {
			return ErrNotLoaded
		}
	case "bootstrap":
		runner.loaded = true
	case "bootout":
		runner.loaded = false
	}
	return nil
}

func (runner *lockedLaunchctlRunner) Calls() [][]string {
	runner.mutex.Lock()
	defer runner.mutex.Unlock()
	calls := make([][]string, len(runner.calls))
	for index := range runner.calls {
		calls[index] = append([]string(nil), runner.calls[index]...)
	}
	return calls
}

type fakeLaunchctlRunner struct {
	loaded             bool
	bootstrapErr       error
	bootoutErr         error
	bootoutBeforeError bool
	calls              [][]string
}

func (r *fakeLaunchctlRunner) Run(_ context.Context, args ...string) error {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch args[0] {
	case "print":
		if !r.loaded {
			return ErrNotLoaded
		}
	case "bootstrap":
		if r.bootstrapErr != nil {
			return r.bootstrapErr
		}
		r.loaded = true
	case "bootout":
		if r.bootoutErr != nil {
			if r.bootoutBeforeError {
				r.loaded = false
			}
			return r.bootoutErr
		}
		r.loaded = false
	}
	return nil
}

func testLaunchAgentConfig(executable string) Config {
	return Config{Executable: executable, ConfigPath: "/tmp/bsbctl-config.json", SocketPath: "/tmp/bsbctl-control.sock"}
}

func containsLaunchAgentError(err error, values ...string) bool {
	for _, value := range values {
		if strings.Contains(err.Error(), value) {
			return true
		}
	}
	return false
}
