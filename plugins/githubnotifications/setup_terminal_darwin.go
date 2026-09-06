//go:build darwin

package githubnotifications

import (
	"io"
	"os"

	"github.com/lxdb/bsbctl/internal/cliinput"
	"golang.org/x/sys/unix"
)

func disableAppSetupTerminalEcho(input io.Reader) (func() error, error) {
	if reader, ok := input.(*cliinput.Reader); ok {
		input = reader.File()
	}
	file, ok := input.(*os.File)
	if !ok {
		return nil, unix.ENOTTY
	}
	fd := int(file.Fd())
	original, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return nil, err
	}
	hidden := *original
	hidden.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, &hidden); err != nil {
		return nil, err
	}
	return func() error { return unix.IoctlSetTermios(fd, unix.TIOCSETA, original) }, nil
}
