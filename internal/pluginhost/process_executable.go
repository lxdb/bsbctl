package pluginhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

type executableIdentity struct {
	SHA256 string
	Size   int64
}

type executableLaunch struct {
	path                    string
	extra                   *os.File
	releaseParentAfterStart func()
	cleanup                 func()
}

func verifyExecutable(path string) (executableIdentity, error) {
	file, identity, err := openVerifiedExecutable(path)
	if err != nil {
		return executableIdentity{}, err
	}
	if err := file.Close(); err != nil {
		return executableIdentity{}, safeFileError("close plugin executable", err)
	}
	return identity, nil
}

func openVerifiedExecutable(path string) (*os.File, executableIdentity, error) {
	file, err := openPluginExecutableNoFollow(path)
	if err != nil {
		return nil, executableIdentity{}, safeFileError("open plugin executable", err)
	}
	identity, err := verifyOpenExecutable(file)
	if err != nil {
		_ = file.Close()
		return nil, executableIdentity{}, err
	}
	return file, identity, nil
}

func verifyOpenExecutable(file *os.File) (executableIdentity, error) {
	if file == nil {
		return executableIdentity{}, errors.New("plugin executable descriptor is unavailable")
	}
	before, err := file.Stat()
	if err != nil {
		return executableIdentity{}, safeFileError("stat opened plugin executable", err)
	}
	if !before.Mode().IsRegular() {
		return executableIdentity{}, errors.New("plugin executable must be a regular file")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return executableIdentity{}, safeFileError("rewind plugin executable", err)
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return executableIdentity{}, safeFileError("hash plugin executable", err)
	}
	after, err := file.Stat()
	if err != nil {
		return executableIdentity{}, safeFileError("restat opened plugin executable", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return executableIdentity{}, safeFileError("rewind plugin executable", err)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || size != before.Size() || !before.ModTime().Equal(after.ModTime()) {
		return executableIdentity{}, errors.New("plugin executable changed during verification")
	}
	return executableIdentity{SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size}, nil
}

func safeFileError(operation string, err error) error {
	if pathErr, ok := errors.AsType[*os.PathError](err); ok {
		err = pathErr.Err
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func killGroup(pid int, signal syscall.Signal) error { return syscall.Kill(-pid, signal) }

func ignoreNoProcess(err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func ignoreExitSignal(err error) error {
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return nil
	}
	return err
}
