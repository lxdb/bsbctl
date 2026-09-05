//go:build darwin

package pluginhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const executableSnapshotCleanupLimit = 64

const maxExecutableSnapshotBasenameBytes = 128

var executableSnapshotMu sync.Mutex

func prepareExecutableLaunch(source *os.File, identity executableIdentity) (executableLaunch, error) {
	executableName, err := executableSnapshotBasename(source)
	if err != nil {
		return executableLaunch{}, err
	}
	directory, ownerLock, err := newExecutableSnapshotDirectory()
	if err != nil {
		return executableLaunch{}, safeFileError("create private plugin executable directory", err)
	}
	path := filepath.Join(directory, executableName)
	lockPath := filepath.Join(directory, "owner.lock")
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			_ = os.Remove(path)
			_ = os.Remove(lockPath)
			_ = ownerLock.Close()
			_ = os.Remove(directory)
		})
	}
	if err := unix.Fclonefileat(int(source.Fd()), unix.AT_FDCWD, path, 0); err != nil {
		_ = os.Remove(path)
		if err := copyVerifiedExecutable(source, path, identity); err != nil {
			cleanup()
			return executableLaunch{}, err
		}
	}
	if err := os.Chmod(path, 0o500); err != nil {
		cleanup()
		return executableLaunch{}, safeFileError("restrict private plugin executable", err)
	}
	snapshot, err := verifyExecutable(path)
	if err != nil || snapshot != identity {
		cleanup()
		return executableLaunch{}, errors.New("private plugin executable snapshot does not match verified identity")
	}
	return executableLaunch{path: path, cleanup: cleanup}, nil
}

func executableSnapshotBasename(source *os.File) (string, error) {
	if source == nil {
		return "", errors.New("plugin executable descriptor is unavailable")
	}
	name := filepath.Base(source.Name())
	if !validExecutableSnapshotBasename(name) {
		return "", errors.New("plugin executable basename is unsafe")
	}
	return name, nil
}

func validExecutableSnapshotBasename(name string) bool {
	if len(name) < 1 || len(name) > maxExecutableSnapshotBasenameBytes || name == "owner.lock" || name == "." || name == ".." || filepath.Base(name) != name || filepath.Clean(name) != name {
		return false
	}
	for index := 0; index < len(name); index++ {
		value := name[index]
		alphanumeric := value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
		if alphanumeric || index > 0 && (value == '.' || value == '_' || value == '-') {
			continue
		}
		return false
	}
	return true
}

func newExecutableSnapshotDirectory() (string, *os.File, error) {
	executableSnapshotMu.Lock()
	defer executableSnapshotMu.Unlock()
	root := filepath.Join(os.TempDir(), "bsbctl-plugin-exec")
	if err := ensureExecutableSnapshotRoot(root); err != nil {
		return "", nil, err
	}
	rootLock, err := openSnapshotLock(filepath.Join(root, ".cleanup.lock"), true)
	if err != nil {
		return "", nil, err
	}
	defer rootLock.Close()
	if err := unix.Flock(int(rootLock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return "", nil, errors.New("plugin executable snapshot cleanup is busy")
	}
	cleanupStaleExecutableSnapshots(root, executableSnapshotCleanupLimit)
	directory, err := os.MkdirTemp(root, "snapshot-")
	if err != nil {
		return "", nil, err
	}
	lockPath := filepath.Join(directory, "owner.lock")
	ownerLock, err := openSnapshotLock(lockPath, true)
	if err != nil {
		_ = os.Remove(directory)
		return "", nil, err
	}
	if err := unix.Flock(int(ownerLock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = ownerLock.Close()
		_ = os.Remove(lockPath)
		_ = os.Remove(directory)
		return "", nil, err
	}
	return directory, ownerLock, nil
}

func ensureExecutableSnapshotRoot(root string) error {
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	var state unix.Stat_t
	if err := unix.Lstat(root, &state); err != nil || state.Mode&unix.S_IFMT != unix.S_IFDIR || state.Uid != uint32(os.Getuid()) {
		return errors.New("plugin executable snapshot root is unsafe")
	}
	return os.Chmod(root, 0o700)
}

func openSnapshotLock(path string, create bool) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if create {
		flags |= unix.O_CREAT
	}
	descriptor, err := unix.Open(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	var state unix.Stat_t
	if err := unix.Fstat(descriptor, &state); err != nil || state.Mode&unix.S_IFMT != unix.S_IFREG || state.Uid != uint32(os.Getuid()) || state.Nlink != 1 {
		_ = unix.Close(descriptor)
		return nil, errors.New("plugin executable snapshot lock is unsafe")
	}
	if err := unix.Fchmod(descriptor, 0o600); err != nil {
		_ = unix.Close(descriptor)
		return nil, err
	}
	return os.NewFile(uintptr(descriptor), path), nil
}

func cleanupStaleExecutableSnapshots(root string, limit int) {
	if limit < 1 {
		return
	}
	directory, err := os.Open(root)
	if err != nil {
		return
	}
	defer directory.Close()
	entries, err := directory.ReadDir(limit*2 + 2)
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	inspected := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "snapshot-") || inspected >= limit {
			continue
		}
		inspected++
		cleanupStaleExecutableSnapshot(filepath.Join(root, entry.Name()))
	}
}

func cleanupStaleExecutableSnapshot(directory string) {
	var directoryState unix.Stat_t
	if err := unix.Lstat(directory, &directoryState); err != nil || directoryState.Mode&unix.S_IFMT != unix.S_IFDIR || directoryState.Uid != uint32(os.Getuid()) || directoryState.Mode&0o777 != 0o700 {
		return
	}
	handle, err := os.Open(directory)
	if err != nil {
		return
	}
	entries, readErr := handle.ReadDir(3)
	_ = handle.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || len(entries) > 2 {
		return
	}
	hasOwnerLock := false
	executableName := ""
	for _, entry := range entries {
		if entry.Name() == "owner.lock" {
			if hasOwnerLock {
				return
			}
			hasOwnerLock = true
			continue
		}
		if executableName != "" || !validExecutableSnapshotBasename(entry.Name()) {
			return
		}
		executableName = entry.Name()
	}
	lockPath := filepath.Join(directory, "owner.lock")
	var lock *os.File
	if hasOwnerLock {
		lock, err = openSnapshotLock(lockPath, false)
		if err != nil {
			return
		}
		defer lock.Close()
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return
		}
	} else if len(entries) != 0 {
		return
	}
	if executableName != "" {
		executablePath := filepath.Join(directory, executableName)
		var executableState unix.Stat_t
		if err := unix.Lstat(executablePath, &executableState); err != nil || executableState.Mode&unix.S_IFMT != unix.S_IFREG || executableState.Uid != uint32(os.Getuid()) || executableState.Mode&0o222 != 0 || executableState.Nlink != 1 {
			return
		}
		if err := os.Remove(executablePath); err != nil {
			return
		}
	}
	if lock != nil {
		if err := os.Remove(lockPath); err != nil {
			return
		}
	}
	_ = os.Remove(directory)
}

func copyVerifiedExecutable(source *os.File, path string, identity executableIdentity) error {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return safeFileError("rewind verified plugin executable", err)
	}
	destination, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o500)
	if err != nil {
		return safeFileError("create private plugin executable", err)
	}
	digest := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(destination, digest), source)
	syncErr := destination.Sync()
	closeErr := destination.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		return safeFileError("copy private plugin executable", errors.Join(copyErr, syncErr, closeErr))
	}
	if size != identity.Size || hex.EncodeToString(digest.Sum(nil)) != identity.SHA256 {
		return errors.New("private plugin executable copy does not match verified identity")
	}
	return nil
}
