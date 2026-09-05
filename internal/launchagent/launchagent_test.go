package launchagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lxdb/bsbctl/internal/localstate"
	"golang.org/x/sys/unix"
)

func Write(path string, config Config) error {
	if !filepath.IsAbs(config.Executable) || !filepath.IsAbs(config.ConfigPath) || !filepath.IsAbs(config.SocketPath) || (config.LogPath != "" && !filepath.IsAbs(config.LogPath)) || (config.StdoutPath != "" && !filepath.IsAbs(config.StdoutPath)) || (config.StderrPath != "" && !filepath.IsAbs(config.StderrPath)) {
		return errors.New("LaunchAgent paths must be absolute")
	}
	data, err := render(config)
	if err != nil {
		return err
	}
	_, err = writeData(path, data)
	return err
}

func writeData(path string, data []byte) (localstate.CommitOutcome, error) {
	lifecycle, err := acquirePlistLifecycle(context.Background(), path, os.Getuid(), lifecycleLockOptions{})
	if err != nil {
		return localstate.NotCommitted, err
	}
	expected, err := currentPathIdentityAt(lifecycle)
	if err != nil {
		return localstate.NotCommitted, errors.Join(err, lifecycle.close())
	}
	var expectation *entryExpectation
	if expected != nil {
		expectation = &entryExpectation{identity: *expected}
	}
	outcome, operationErr := writeDataPinned(lifecycle, data, expectation, writeOptions{})
	if !lifecycle.pathStillPinned() {
		outcome = localstate.CommittedDurabilityUncertain
		operationErr = errors.Join(operationErr, errors.New("LaunchAgent directory identity changed"))
	}
	if err := lifecycle.close(); err != nil {
		outcome = localstate.CommittedDurabilityUncertain
		operationErr = errors.Join(operationErr, errors.New("close LaunchAgent lifecycle"))
	}
	return outcome, operationErr
}

func currentPathIdentityAt(lifecycle *plistLifecycle) (*fileIdentity, error) {
	var pathStat unix.Stat_t
	if err := unix.Fstatat(lifecycle.directoryFD, lifecycle.name, &pathStat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return nil, nil
	} else if err != nil || uint32(pathStat.Mode)&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("invalid LaunchAgent plist")
	}
	fd, err := unix.Openat(lifecycle.directoryFD, lifecycle.name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	statErr := unix.Fstat(fd, &stat)
	closeErr := unix.Close(fd)
	if statErr != nil || closeErr != nil {
		return nil, errors.Join(statErr, closeErr)
	}
	if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("invalid LaunchAgent plist")
	}
	identity := identityFromStat(stat)
	return &identity, nil
}

func TestWriteCreatesProtectedKeepAliveLaunchAgent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "dev.bsbctl.plist")
	err := Write(path, Config{
		Executable: "/Applications/Busy & Useful/bsbctl",
		ConfigPath: "/Users/test/Library/Application Support/bsbctl/config.json",
		SocketPath: "/Users/test/Library/Caches/bsbctl/ctl.sock",
		LogPath:    "/Users/test/.bsbctl/logs/daemon.jsonl",
		StdoutPath: "/Users/test/Library/Logs/bsbctl.log",
		StderrPath: "/Users/test/Library/Logs/bsbctl.err.log",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, wanted := range []string{
		"<string>dev.bsbctl</string>", "<key>KeepAlive</key>", "<true/>",
		"/Applications/Busy &amp; Useful/bsbctl", "<string>daemon</string>", "<string>--config</string>",
		"<string>--log</string>", "<string>/Users/test/.bsbctl/logs/daemon.jsonl</string>",
	} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("plist does not contain %q:\n%s", wanted, text)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestAtomicWriterReportsPreAndPostRenameCommitOutcomes(t *testing.T) {
	t.Run("pre-rename failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), Label+".plist")
		lifecycle, err := acquirePlistLifecycle(context.Background(), path, os.Getuid(), lifecycleLockOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := lifecycle.close(); err != nil {
				t.Error(err)
			}
		}()
		outcome, err := writeDataPinned(lifecycle, []byte("desired"), nil, writeOptions{
			renameExclusive: func(int, string, string) error { return errors.New("rename failed") },
		})
		if err == nil || outcome != localstate.NotCommitted {
			t.Fatalf("write outcome/error = %q, %v", outcome, err)
		}
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("pre-rename failure changed target: %v", statErr)
		}
	})

	postRename := []struct {
		name      string
		configure func(*writeOptions)
	}{
		{name: "parent open failure", configure: func(options *writeOptions) {
			options.duplicateDirectory = func(int) (*os.File, error) { return nil, errors.New("open failed") }
		}},
		{name: "parent sync failure", configure: func(options *writeOptions) {
			options.syncDirectory = func(*os.File) error { return errors.New("sync failed") }
		}},
		{name: "parent close failure", configure: func(options *writeOptions) {
			options.closeDirectory = func(file *os.File) error {
				_ = file.Close()
				return errors.New("close failed")
			}
		}},
	}
	for _, test := range postRename {
		t.Run("post-rename "+test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), Label+".plist")
			lifecycle, err := acquirePlistLifecycle(context.Background(), path, os.Getuid(), lifecycleLockOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lifecycle.close(); err != nil {
					t.Error(err)
				}
			}()
			options := writeOptions{}
			test.configure(&options)
			outcome, err := writeDataPinned(lifecycle, []byte("desired"), nil, options)
			if err == nil || outcome != localstate.CommittedDurabilityUncertain {
				t.Fatalf("write outcome/error = %q, %v", outcome, err)
			}
			if data, readErr := os.ReadFile(path); readErr != nil || string(data) != "desired" {
				t.Fatalf("committed target = %q, %v", data, readErr)
			}
		})
	}
}

func TestWriteOmitsApplicationOwnedLogFilesByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.bsbctl.plist")
	if err := Write(path, Config{Executable: "/usr/local/bin/bsbctl", ConfigPath: "/tmp/config.json", SocketPath: "/tmp/control.sock"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "StandardOutPath") || strings.Contains(string(data), "StandardErrorPath") {
		t.Fatalf("plist unexpectedly owns log files:\n%s", data)
	}
}
