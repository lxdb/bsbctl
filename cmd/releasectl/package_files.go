package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

func prepareOutputDirectory(output string) error {
	if strings.TrimSpace(output) == "" {
		return errors.New("release output directory is required")
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return err
	}
	info, err := os.Lstat(output)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("release output path is not a real directory")
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("release output directory must be empty")
	}
	return nil
}

func readBoundedFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxReleaseInputBytes {
		return nil, errors.New("release input is not a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxReleaseInputBytes+1))
	if err != nil || len(data) > maxReleaseInputBytes {
		return nil, errors.New("release input exceeds limit")
	}
	return data, nil
}

func writeCanonicalFile(path string, data []byte, epoch time.Time) error {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}
	return os.Chtimes(path, epoch, epoch)
}
