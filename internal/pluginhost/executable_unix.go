//go:build darwin || linux

package pluginhost

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openPluginExecutableNoFollow(path string) (*os.File, error) {
	descriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, errors.New("plugin executable must not be a symbolic link")
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(descriptor), path), nil
}
