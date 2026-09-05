package control

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/lxdb/bsbctl/sdk/protocol"
	"golang.org/x/sys/unix"
)

var errInvalidCatalogInput = errors.New("invalid catalog input")

func readRegularBoundedDigest(path string, limit int64, expectedDigest string) ([]byte, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("input file is unavailable")
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, errInvalidCatalogInput
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, errInvalidCatalogInput
		}
		return nil, errors.New("input file is unavailable")
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("input file is unavailable")
	}
	before, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, errors.New("input file is unavailable")
	}
	if !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > limit {
		if err := file.Close(); err != nil {
			return nil, errors.New("input file is unavailable")
		}
		return nil, errInvalidCatalogInput
	}
	data, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil || !os.SameFile(before, after) || before.Size() != after.Size() || int64(len(data)) != after.Size() || len(data) < 1 || int64(len(data)) > limit {
		return nil, errors.New("input file changed")
	}
	want, err := hex.DecodeString(expectedDigest)
	digest := sha256.Sum256(data)
	if err != nil || len(want) != sha256.Size || subtle.ConstantTimeCompare(digest[:], want) != 1 {
		return nil, errors.New("input digest mismatch")
	}
	return data, nil
}

func strictDecode(data []byte, destination any) error {
	return protocol.DecodeStrict(data, destination)
}
