//go:build !darwin && !linux

package pluginhost

import (
	"errors"
	"os"
)

func openPluginExecutableNoFollow(string) (*os.File, error) {
	return nil, errors.New("descriptor-bound plugin execution is unsupported on this platform")
}

func prepareExecutableLaunch(*os.File, executableIdentity) (executableLaunch, error) {
	return executableLaunch{}, errors.New("descriptor-bound plugin execution is unsupported on this platform")
}
