//go:build !darwin && !linux

package checkpoint

import (
	"errors"
	"os"
)

func openCheckpointNoFollow(string) (*os.File, error) {
	return nil, errors.New("secure checkpoint reads are unsupported on this platform")
}
