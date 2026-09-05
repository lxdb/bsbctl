package deviceownership

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalIdentityCollidesForEquivalentDeviceURLs(t *testing.T) {
	aliases := []string{
		"http://BusyBar.Local",
		"HTTP://busybar.local/",
		"http://busybar.local:80/api/../",
		"http://busybar.local?api_token=secret",
	}
	first, err := canonicalIdentity(aliases[0], "bsbctl")
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range aliases[1:] {
		got, err := canonicalIdentity(alias, "bsbctl")
		if err != nil {
			t.Fatalf("canonicalIdentity(%q): %v", alias, err)
		}
		if got != first {
			t.Fatalf("canonicalIdentity(%q) = %q, want %q", alias, got, first)
		}
	}
	if strings.Contains(first, "busybar") || strings.Contains(first, "secret") {
		t.Fatalf("canonical identity leaks device data: %q", first)
	}
}

func TestLeaseSerializesSameDeviceAndAllowsDifferentDevices(t *testing.T) {
	root := filepath.Join(t.TempDir(), "locks")
	first, err := acquire(root, os.Getuid(), "http://busybar.local", "bsbctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	contender, err := acquire(root, os.Getuid(), "http://BUSYBAR.local:80/", "bsbctl")
	if !errors.Is(err, ErrAlreadyOwned) || contender != nil {
		t.Fatalf("same-device contender = %#v, %v; want ErrAlreadyOwned", contender, err)
	}
	conflict, ok := errors.AsType[*ConflictError](err)
	if !ok || conflict.OwnerPID != os.Getpid() {
		t.Fatalf("conflict = %#v, want owner PID %d", conflict, os.Getpid())
	}
	if strings.Contains(err.Error(), "busybar.local") {
		t.Fatalf("conflict leaks device address: %q", err)
	}

	other, err := acquire(root, os.Getuid(), "http://other.local", "bsbctl")
	if err != nil {
		t.Fatalf("different device: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := acquire(root, os.Getuid(), "http://busybar.local", "bsbctl")
	if err != nil {
		t.Fatalf("restart after close: %v", err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseRejectsUnsafeSharedState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "locks")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if lease, err := acquire(root, os.Getuid(), "http://busybar.local", "bsbctl"); err == nil || lease != nil {
		t.Fatalf("acquire with permissive directory = %#v, %v", lease, err)
	}
}

func TestLeaseIsReleasedWhenOwnerProcessDies(t *testing.T) {
	if os.Getenv("BSBCTL_DEVICE_OWNERSHIP_HELPER") == "1" {
		lease, err := acquire(os.Getenv("BSBCTL_DEVICE_OWNERSHIP_ROOT"), os.Getuid(), "http://busybar.local", "bsbctl")
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		_ = lease
		_, _ = fmt.Fprintln(os.Stdout, "locked")
		select {}
	}

	root := filepath.Join(t.TempDir(), "locks")
	command := exec.Command(os.Args[0], "-test.run=^TestLeaseIsReleasedWhenOwnerProcessDies$")
	command.Env = append(os.Environ(), "BSBCTL_DEVICE_OWNERSHIP_HELPER=1", "BSBCTL_DEVICE_OWNERSHIP_ROOT="+root)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len("locked\n"))
	if _, err := io.ReadFull(stdout, buffer); err != nil || string(buffer) != "locked\n" {
		_ = command.Process.Kill()
		t.Fatalf("helper readiness = %q, %v", buffer, err)
	}
	if _, err := acquire(root, os.Getuid(), "http://busybar.local", "bsbctl"); !errors.Is(err, ErrAlreadyOwned) {
		_ = command.Process.Kill()
		t.Fatalf("live helper contender error = %v, want ErrAlreadyOwned", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("killed helper exited successfully")
	}
	restarted, err := acquire(root, os.Getuid(), "http://busybar.local", "bsbctl")
	if err != nil {
		t.Fatalf("restart after owner death: %v", err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
}
