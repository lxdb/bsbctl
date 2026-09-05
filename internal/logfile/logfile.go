// Package logfile provides a private size-bounded rotating file writer.
package logfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

type Writer struct {
	mu        sync.Mutex
	directory *os.File
	name      string
	maxBytes  int64
	archives  int
	file      *os.File
	size      int64
}

func Open(path string, maxBytes int64, archives int) (*Writer, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("log path must be absolute")
	}
	if maxBytes <= 0 || archives < 0 {
		return nil, errors.New("log bounds must be positive")
	}
	directory, err := openDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	writer := &Writer{directory: directory, name: filepath.Base(path), maxBytes: maxBytes, archives: archives}
	if err := writer.openActive(); err != nil {
		return nil, errors.Join(err, directory.Close())
	}
	return writer, nil
}

func openDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return nil, err
		}
		fd, err = unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func (w *Writer) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(data)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	written, err := w.file.Write(data)
	w.size += int64(written)
	return written, err
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var err error
	if w.file != nil {
		err = w.file.Close()
		w.file = nil
	}
	if w.directory != nil {
		err = errors.Join(err, w.directory.Close())
		w.directory = nil
	}
	return err
}

func (w *Writer) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	if w.archives == 0 {
		if err := w.remove(w.name); err != nil {
			return err
		}
	} else {
		if err := w.remove(w.archiveName(w.archives)); err != nil {
			return err
		}
		for index := w.archives - 1; index >= 1; index-- {
			if err := w.rename(w.archiveName(index), w.archiveName(index+1)); err != nil {
				return err
			}
		}
		if err := w.rename(w.name, w.archiveName(1)); err != nil {
			return err
		}
	}
	return w.openActive()
}

func (w *Writer) openActive() error {
	fd, err := unix.Openat(int(w.directory.Fd()), w.name, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), w.name)
	info, err := file.Stat()
	if err != nil {
		return errors.Join(err, file.Close())
	}
	if !info.Mode().IsRegular() {
		return errors.Join(errors.New("log path must be a regular file"), file.Close())
	}
	if err := file.Chmod(0o600); err != nil {
		return errors.Join(err, file.Close())
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *Writer) archiveName(index int) string {
	return fmt.Sprintf("%s.%d", w.name, index)
}

func (w *Writer) remove(name string) error {
	err := unix.Unlinkat(int(w.directory.Fd()), name, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

func (w *Writer) rename(oldName, newName string) error {
	err := unix.Renameat(int(w.directory.Fd()), oldName, int(w.directory.Fd()), newName)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

var _ io.WriteCloser = (*Writer)(nil)
