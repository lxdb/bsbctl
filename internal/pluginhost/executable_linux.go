//go:build linux

package pluginhost

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

func prepareExecutableLaunch(source *os.File, identity executableIdentity) (executableLaunch, error) {
	fd, err := createExecutableMemfd()
	if err != nil {
		return executableLaunch{}, safeFileError("create sealed plugin executable", err)
	}
	snapshot := os.NewFile(uintptr(fd), "bsbctl-plugin-memfd")
	var cleanupOnce sync.Once
	cleanup := func() { cleanupOnce.Do(func() { _ = snapshot.Close() }) }
	fail := func(operation string, err error) (executableLaunch, error) {
		cleanup()
		return executableLaunch{}, safeFileError(operation, err)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fail("rewind verified plugin executable", err)
	}
	copied, err := io.Copy(snapshot, io.LimitReader(source, identity.Size+1))
	if err != nil {
		return fail("copy verified plugin executable", err)
	}
	if copied != identity.Size {
		cleanup()
		return executableLaunch{}, errors.New("sealed plugin executable copy does not match verified size")
	}
	if err := snapshot.Chmod(0o500); err != nil {
		return fail("make sealed plugin executable runnable", err)
	}
	if err := snapshot.Sync(); err != nil {
		return fail("sync sealed plugin executable", err)
	}
	wantSeals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if _, err := unix.FcntlInt(snapshot.Fd(), unix.F_ADD_SEALS, wantSeals); err != nil {
		return fail("seal plugin executable", err)
	}
	seals, err := unix.FcntlInt(snapshot.Fd(), unix.F_GET_SEALS, 0)
	if err != nil {
		return fail("inspect sealed plugin executable", err)
	}
	if seals&wantSeals != wantSeals {
		cleanup()
		return executableLaunch{}, errors.New("plugin executable memfd is not fully sealed")
	}
	verified, err := verifyOpenExecutable(snapshot)
	if err != nil {
		cleanup()
		return executableLaunch{}, err
	}
	if verified != identity {
		cleanup()
		return executableLaunch{}, errors.New("sealed plugin executable does not match verified identity")
	}
	return executableLaunch{
		path: fmt.Sprintf("/proc/self/fd/%d", 4), extra: snapshot,
		releaseParentAfterStart: cleanup, cleanup: cleanup,
	}, nil
}

func createExecutableMemfd() (int, error) {
	const mfdExec = 0x0010 // Linux 6.3 MFD_EXEC; older kernels reject it with EINVAL.
	flags := unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING
	fd, err := unix.MemfdCreate("bsbctl-plugin", flags|mfdExec)
	if errors.Is(err, unix.EINVAL) {
		return unix.MemfdCreate("bsbctl-plugin", flags)
	}
	return fd, err
}
