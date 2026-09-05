package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type inputFailure struct {
	operational bool
}

func (err *inputFailure) Error() string { return "input failure" }

type boundedInput struct {
	path   string
	data   []byte
	digest string
}

func readJSONInput(path string, limit int64) (boundedInput, error) {
	input, err := readBoundedRegularInput(path, limit)
	if err != nil {
		return boundedInput{}, err
	}
	trimmed := bytes.TrimSpace(input.data)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return boundedInput{}, &inputFailure{}
	}
	return input, nil
}

func readBoundedRegularInput(path string, limit int64) (boundedInput, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return boundedInput{}, &inputFailure{operational: true}
	}
	pathInfo, err := os.Lstat(absolute)
	if err != nil {
		return boundedInput{}, &inputFailure{operational: true}
	}
	if !pathInfo.Mode().IsRegular() {
		return boundedInput{}, &inputFailure{}
	}
	fd, err := unix.Open(absolute, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return boundedInput{}, &inputFailure{operational: !errors.Is(err, unix.ELOOP)}
	}
	file := os.NewFile(uintptr(fd), absolute)
	if file == nil {
		_ = unix.Close(fd)
		return boundedInput{}, &inputFailure{operational: true}
	}
	before, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return boundedInput{}, &inputFailure{operational: true}
	}
	if !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > limit {
		if err := file.Close(); err != nil {
			return boundedInput{}, &inputFailure{operational: true}
		}
		return boundedInput{}, &inputFailure{}
	}
	data, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil || !os.SameFile(before, after) || before.Size() != after.Size() {
		return boundedInput{}, &inputFailure{operational: true}
	}
	if int64(len(data)) != after.Size() {
		return boundedInput{}, &inputFailure{operational: true}
	}
	if len(data) < 1 || int64(len(data)) > limit {
		return boundedInput{}, &inputFailure{}
	}
	digest := sha256.Sum256(data)
	return boundedInput{path: absolute, data: data, digest: fmt.Sprintf("%x", digest)}, nil
}
