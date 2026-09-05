// Package cliinput owns cancellable process stdin without detached read workers.
package cliinput

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Reader is used by one command goroutine. Close follows its final Read; it does
// not run concurrently with Read. The source remains owned by the caller.
type Reader struct {
	ctx         context.Context
	source      *os.File
	fd          int
	nonblocking bool
	closed      bool
}

// New defers descriptor acquisition until the command actually reads stdin.
func New(ctx context.Context, source *os.File) *Reader {
	return &Reader{ctx: ctx, source: source, fd: -1}
}

// File returns the borrowed source for the command's existing terminal check.
func (r *Reader) File() *os.File { return r.source }

func (r *Reader) open() error {
	raw, err := r.source.SyscallConn()
	if err != nil {
		return err
	}
	var descriptorErr error
	err = raw.Control(func(fd uintptr) {
		var flags int
		flags, descriptorErr = unix.FcntlInt(fd, unix.F_GETFL, 0)
		if descriptorErr != nil {
			return
		}
		r.nonblocking = flags&unix.O_NONBLOCK != 0
		r.fd, descriptorErr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0)
	})
	if err != nil || descriptorErr != nil {
		return errors.Join(err, descriptorErr)
	}
	if err := unix.SetNonblock(r.fd, true); err != nil {
		_ = unix.Close(r.fd)
		r.fd = -1
		return err
	}
	return nil
}

func (r *Reader) Read(buffer []byte) (int, error) {
	if r.closed {
		return 0, os.ErrClosed
	}
	if len(buffer) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.fd < 0 {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	var retry *time.Timer
	defer func() {
		if retry != nil {
			retry.Stop()
		}
	}()
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := unix.Read(r.fd, buffer)
		if n > 0 {
			return n, err
		}
		if err == nil {
			return 0, io.EOF
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
			return 0, err
		}
		// Explicit waiting also handles Darwin named-FIFO EOF, which the Go
		// kqueue file poller does not reliably report when the writer closes.
		if retry == nil {
			retry = time.NewTimer(10 * time.Millisecond)
		} else {
			retry.Reset(10 * time.Millisecond)
		}
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-retry.C:
		}
	}
}

// Close restores the shared open-file description's nonblocking state, then
// closes only the owned duplicate. Forced process termination cannot run this
// restoration; callers must not read the shared source concurrently.
func (r *Reader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.fd < 0 {
		return nil
	}
	err := unix.SetNonblock(r.fd, r.nonblocking)
	closeErr := unix.Close(r.fd)
	r.fd = -1
	return errors.Join(err, closeErr)
}
