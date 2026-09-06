//go:build !darwin

package githubnotifications

import (
	"errors"
	"io"
)

func disableAppSetupTerminalEcho(io.Reader) (func() error, error) {
	return nil, errors.New("terminal echo control is unsupported")
}
