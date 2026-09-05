// Package launchagent writes the macOS login-service definition for bsbctl.
package launchagent

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/lxdb/bsbctl/internal/localstate"
	"golang.org/x/sys/unix"
)

const Label = "dev.bsbctl"

type Config struct {
	Executable string
	ConfigPath string
	SocketPath string
	LogPath    string
	StdoutPath string
	StderrPath string
}

type fileIdentity struct {
	device uint64
	inode  uint64
}

type entryExpectation struct {
	identity fileIdentity
	data     []byte
	uid      uint32
	mode     uint32
	exact    bool
}

type writeOptions struct {
	exchange           func(int, string, string) error
	renameExclusive    func(int, string, string) error
	unlink             func(int, string) error
	duplicateDirectory func(int) (*os.File, error)
	syncDirectory      func(*os.File) error
	closeDirectory     func(*os.File) error
	beforeRename       func()
}

type tempEntry struct {
	file     *os.File
	name     string
	identity fileIdentity
}

func writeDataPinned(lifecycle *plistLifecycle, data []byte, expected *entryExpectation, options writeOptions) (localstate.CommitOutcome, error) {
	temp, outcome, err := createTempEntry(lifecycle, data, options)
	if err != nil {
		return outcome, err
	}
	if options.beforeRename != nil {
		options.beforeRename()
	}
	renameExclusive := options.renameExclusive
	if renameExclusive == nil {
		renameExclusive = platformRenameExclusive
	}
	exchange := options.exchange
	if exchange == nil {
		exchange = platformExchange
	}
	unlink := options.unlink
	if unlink == nil {
		unlink = func(directoryFD int, name string) error { return unix.Unlinkat(directoryFD, name, 0) }
	}
	if expected == nil {
		if err := renameExclusive(lifecycle.directoryFD, temp.name, lifecycle.name); err != nil {
			return cleanupFailedTemp(lifecycle, temp, unlink, err)
		}
		if !entryHasIdentityAt(lifecycle.directoryFD, lifecycle.name, temp.identity) {
			return finishWriteDirectory(lifecycle, temp, options, localstate.CommittedDurabilityUncertain, errors.New("LaunchAgent plist identity is uncertain"))
		}
		return finishWriteDirectory(lifecycle, temp, options, localstate.Committed, nil)
	}
	if err := exchange(lifecycle.directoryFD, temp.name, lifecycle.name); err != nil {
		return cleanupFailedTemp(lifecycle, temp, unlink, err)
	}
	displaced, displacedErr := fingerprintAt(lifecycle.directoryFD, temp.name)
	validationErr := validateExpectedEntryAt(lifecycle.directoryFD, temp.name, *expected)
	if validationErr != nil {
		if err := exchange(lifecycle.directoryFD, temp.name, lifecycle.name); err != nil {
			return finishWriteDirectory(lifecycle, temp, options, localstate.CommittedDurabilityUncertain, errors.Join(validationErr, errors.New("LaunchAgent plist restoration failed"), err))
		}
		restored := displacedErr == nil && entryHasFingerprintAt(lifecycle.directoryFD, lifecycle.name, displaced)
		cleanupErr := unlinkKnownEntry(lifecycle.directoryFD, temp.name, temp.identity, unlink)
		outcome := localstate.NotCommitted
		if !restored || cleanupErr != nil {
			outcome = localstate.CommittedDurabilityUncertain
		}
		return finishWriteDirectory(lifecycle, temp, options, outcome, errors.Join(validationErr, displacedErr, cleanupErr))
	}
	if err := unlinkKnownEntry(lifecycle.directoryFD, temp.name, expected.identity, unlink); err != nil {
		return finishWriteDirectory(lifecycle, temp, options, localstate.CommittedDurabilityUncertain, err)
	}
	if !entryHasIdentityAt(lifecycle.directoryFD, lifecycle.name, temp.identity) {
		return finishWriteDirectory(lifecycle, temp, options, localstate.CommittedDurabilityUncertain, errors.New("LaunchAgent plist identity is uncertain"))
	}
	return finishWriteDirectory(lifecycle, temp, options, localstate.Committed, nil)
}

func createTempEntry(lifecycle *plistLifecycle, data []byte, options writeOptions) (*tempEntry, localstate.CommitOutcome, error) {
	for attempts := 0; attempts < 16; attempts++ {
		name, err := newPrivateName(".bsbctl-launchagent-temp-")
		if err != nil {
			return nil, localstate.NotCommitted, err
		}
		fd, err := unix.Openat(lifecycle.directoryFD, name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_CREAT|unix.O_EXCL, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, localstate.NotCommitted, err
		}
		file := os.NewFile(uintptr(fd), name)
		if file == nil {
			_ = unix.Close(fd)
			return nil, localstate.CommittedDurabilityUncertain, errors.New("retain unowned LaunchAgent temp")
		}
		var before unix.Stat_t
		if err := unix.Fstat(fd, &before); err != nil || uint32(before.Mode)&unix.S_IFMT != unix.S_IFREG || int(before.Uid) != lifecycle.uid {
			_ = file.Close()
			return nil, localstate.CommittedDurabilityUncertain, errors.New("retain unverifiable LaunchAgent temp")
		}
		entry := &tempEntry{file: file, name: name, identity: identityFromStat(before)}
		if err := file.Chmod(0o600); err != nil {
			outcome, cleanupErr := cleanupCreatedTemp(lifecycle, entry, err)
			return nil, outcome, cleanupErr
		}
		written, writeErr := file.Write(data)
		if writeErr == nil && written != len(data) {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			outcome, cleanupErr := cleanupCreatedTemp(lifecycle, entry, writeErr)
			return nil, outcome, cleanupErr
		}
		if err := file.Sync(); err != nil {
			outcome, cleanupErr := cleanupCreatedTemp(lifecycle, entry, err)
			return nil, outcome, cleanupErr
		}
		var after unix.Stat_t
		if err := unix.Fstat(fd, &after); err != nil || identityFromStat(after) != entry.identity || uint32(after.Mode)&0o7777 != 0o600 || int(after.Uid) != lifecycle.uid || after.Size != int64(len(data)) {
			outcome, cleanupErr := cleanupCreatedTemp(lifecycle, entry, errors.New("LaunchAgent temp identity changed"))
			return nil, outcome, cleanupErr
		}
		return entry, localstate.NotCommitted, nil
	}
	return nil, localstate.NotCommitted, errors.New("create unique LaunchAgent temp")
}

func cleanupCreatedTemp(lifecycle *plistLifecycle, temp *tempEntry, cause error) (localstate.CommitOutcome, error) {
	unlinkErr := unlinkKnownEntry(lifecycle.directoryFD, temp.name, temp.identity, func(directoryFD int, name string) error {
		return unix.Unlinkat(directoryFD, name, 0)
	})
	closeErr := temp.file.Close()
	if unlinkErr != nil || closeErr != nil {
		return localstate.CommittedDurabilityUncertain, errors.Join(cause, unlinkErr, closeErr)
	}
	return localstate.NotCommitted, cause
}

func cleanupFailedTemp(lifecycle *plistLifecycle, temp *tempEntry, unlink func(int, string) error, cause error) (localstate.CommitOutcome, error) {
	unlinkErr := unlinkKnownEntry(lifecycle.directoryFD, temp.name, temp.identity, unlink)
	closeErr := temp.file.Close()
	if unlinkErr != nil || closeErr != nil {
		return localstate.CommittedDurabilityUncertain, errors.Join(cause, unlinkErr, closeErr)
	}
	return localstate.NotCommitted, cause
}

func finishWriteDirectory(lifecycle *plistLifecycle, temp *tempEntry, options writeOptions, outcome localstate.CommitOutcome, operationErr error) (localstate.CommitOutcome, error) {
	closeTempErr := temp.file.Close()
	duplicateDirectory := options.duplicateDirectory
	if duplicateDirectory == nil {
		duplicateDirectory = func(directoryFD int) (*os.File, error) {
			duplicateFD, err := unix.Dup(directoryFD)
			if err != nil {
				return nil, err
			}
			file := os.NewFile(uintptr(duplicateFD), lifecycle.directoryPath)
			if file == nil {
				_ = unix.Close(duplicateFD)
				return nil, errors.New("duplicate LaunchAgent directory")
			}
			return file, nil
		}
	}
	directory, duplicateErr := duplicateDirectory(lifecycle.directoryFD)
	if duplicateErr != nil || directory == nil {
		if directory != nil {
			duplicateErr = errors.Join(duplicateErr, directory.Close())
		}
		return localstate.CommittedDurabilityUncertain, errors.Join(operationErr, closeTempErr, duplicateErr, errors.New("duplicate LaunchAgent directory"))
	}
	syncDirectory := options.syncDirectory
	if syncDirectory == nil {
		syncDirectory = func(file *os.File) error { return file.Sync() }
	}
	closeDirectory := options.closeDirectory
	if closeDirectory == nil {
		closeDirectory = func(file *os.File) error { return file.Close() }
	}
	if err := errors.Join(syncDirectory(directory), closeDirectory(directory)); err != nil {
		return localstate.CommittedDurabilityUncertain, errors.Join(operationErr, closeTempErr, err)
	}
	if closeTempErr != nil {
		return localstate.CommittedDurabilityUncertain, errors.Join(operationErr, closeTempErr)
	}
	return outcome, operationErr
}

type entryFingerprint struct {
	identity fileIdentity
	mode     uint32
	uid      uint32
}

func fingerprintAt(directoryFD int, name string) (entryFingerprint, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(directoryFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return entryFingerprint{}, err
	}
	return entryFingerprint{identity: identityFromStat(stat), mode: uint32(stat.Mode), uid: stat.Uid}, nil
}

func entryHasFingerprintAt(directoryFD int, name string, expected entryFingerprint) bool {
	actual, err := fingerprintAt(directoryFD, name)
	return err == nil && actual == expected
}

func entryHasIdentityAt(directoryFD int, name string, expected fileIdentity) bool {
	actual, err := fingerprintAt(directoryFD, name)
	return err == nil && actual.identity == expected && actual.mode&unix.S_IFMT == unix.S_IFREG
}

func unlinkKnownEntry(directoryFD int, name string, expected fileIdentity, unlink func(int, string) error) error {
	if !entryHasIdentityAt(directoryFD, name, expected) {
		return errors.New("LaunchAgent plist identity changed")
	}
	return unlink(directoryFD, name)
}

func validateExpectedEntryAt(directoryFD int, name string, expected entryExpectation) error {
	fd, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("LaunchAgent plist identity changed")
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("LaunchAgent plist identity changed")
	}
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil || uint32(before.Mode)&unix.S_IFMT != unix.S_IFREG || identityFromStat(before) != expected.identity {
		_ = file.Close()
		return errors.New("LaunchAgent plist identity changed")
	}
	if !expected.exact {
		if err := file.Close(); err != nil {
			return errors.New("LaunchAgent plist identity changed")
		}
		return nil
	}
	if before.Size != int64(len(expected.data)) || before.Size < 1 || before.Size > maxLaunchAgentPlistBytes || before.Uid != expected.uid || uint32(before.Mode)&0o7777 != expected.mode {
		_ = file.Close()
		return errors.New("LaunchAgent plist identity changed")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxLaunchAgentPlistBytes+1))
	var after unix.Stat_t
	statErr := unix.Fstat(fd, &after)
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil || identityFromStat(before) != identityFromStat(after) || before.Size != after.Size || before.Uid != after.Uid || before.Mode != after.Mode || !bytes.Equal(data, expected.data) || !plistMatchesLabel(data) {
		return errors.New("LaunchAgent plist identity changed")
	}
	return nil
}

func identityFromStat(stat unix.Stat_t) fileIdentity {
	return fileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}
}

func render(config Config) ([]byte, error) {
	values := []string{config.Executable, config.ConfigPath, config.SocketPath, config.LogPath, config.StdoutPath, config.StderrPath}
	escaped := make([]string, len(values))
	for index, value := range values {
		var buffer bytes.Buffer
		if err := xml.EscapeText(&buffer, []byte(value)); err != nil {
			return nil, err
		}
		escaped[index] = buffer.String()
	}
	logKeys := ""
	logArgument := ""
	if config.LogPath != "" {
		logArgument = fmt.Sprintf("    <string>--log</string>\n    <string>%s</string>\n", escaped[3])
	}
	if config.StdoutPath != "" {
		logKeys += fmt.Sprintf("  <key>StandardOutPath</key>\n  <string>%s</string>\n", escaped[4])
	}
	if config.StderrPath != "" {
		logKeys += fmt.Sprintf("  <key>StandardErrorPath</key>\n  <string>%s</string>\n", escaped[5])
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>daemon</string>
    <string>--config</string>
    <string>%s</string>
	<string>--socket</string>
	<string>%s</string>
%s
	</array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
%s
</dict>
</plist>
`, Label, escaped[0], escaped[1], escaped[2], logArgument, logKeys)), nil
}
