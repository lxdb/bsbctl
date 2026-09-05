package cliinput

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sys/unix"
)

func inputFlags(t *testing.T, source *os.File, nonblocking *bool) int {
	t.Helper()
	raw, err := source.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var callErr error
	if err := raw.Control(func(fd uintptr) {
		if nonblocking != nil {
			callErr = unix.SetNonblock(int(fd), *nonblocking)
		}
		if callErr == nil {
			flags, callErr = unix.FcntlInt(fd, unix.F_GETFL, 0)
		}
	}); err != nil || callErr != nil {
		t.Fatalf("stdin flags: %v, %v", err, callErr)
	}
	return flags
}

func TestPartialPipeCancellationRestoresBorrowedDescriptor(t *testing.T) {
	for _, nonblocking := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocking", true: "nonblocking"}[nonblocking], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				source, writer, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer source.Close()
				defer writer.Close()
				before := inputFlags(t, source, &nonblocking)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				input := New(ctx, source)
				defer input.Close()
				if _, err := writer.Write([]byte("prefix")); err != nil {
					t.Fatal(err)
				}
				buffer := make([]byte, 6)
				if n, err := io.ReadFull(input, buffer); err != nil || n != 6 || string(buffer) != "prefix" {
					t.Fatalf("prefix = %q, %v", buffer, err)
				}
				done := make(chan error, 1)
				go func() {
					_, err := input.Read(buffer)
					done <- err
				}()
				// The prefix is consumed and the unfinished read is durably waiting.
				synctest.Wait()
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("unfinished pipe = %v", err)
					}
				case <-time.After(time.Second):
					_ = writer.Close()
					<-done
					t.Fatal("cancellation did not release the unfinished read")
				}
				if err := input.Close(); err != nil {
					t.Fatal(err)
				}
				if after := inputFlags(t, source, nil); after != before {
					t.Fatalf("shared flags = %x, want %x", after, before)
				}
				if _, err := writer.Write([]byte("reused")); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(source, buffer); err != nil || string(buffer) != "reused" {
					t.Fatalf("borrowed descriptor was not reusable: %q, %v", buffer, err)
				}
			})
		})
	}
}

func TestProcessInputEOFAcrossFilePipeAndNamedFIFO(t *testing.T) {
	for _, kind := range []string{"file", "pipe", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			var source, writer *os.File
			var err error
			switch kind {
			case "file":
				source, err = os.CreateTemp(t.TempDir(), "stdin")
				if err == nil {
					_, err = source.WriteString("payload")
					if err == nil {
						_, err = source.Seek(0, io.SeekStart)
					}
				}
			case "pipe":
				source, writer, err = os.Pipe()
			case "fifo":
				path := filepath.Join(t.TempDir(), "stdin.fifo")
				if err = unix.Mkfifo(path, 0o600); err != nil {
					break
				}
				var fd int
				fd, err = unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
				if err != nil {
					break
				}
				source = os.NewFile(uintptr(fd), path)
				writer, err = os.OpenFile(path, os.O_WRONLY, 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			if writer != nil {
				if _, err := writer.WriteString("payload"); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before := inputFlags(t, source, nil)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			input := New(ctx, source)
			data, err := io.ReadAll(input)
			if closeErr := input.Close(); err != nil || closeErr != nil || string(data) != "payload" {
				t.Fatalf("%s input = %q, read=%v close=%v", kind, data, err, closeErr)
			}
			if after := inputFlags(t, source, nil); after != before {
				t.Fatalf("shared flags = %x, want %x", after, before)
			}
		})
	}
}

func TestUnusedInputDoesNotAcquireOrChangeItsSource(t *testing.T) {
	source, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	input := New(t.Context(), source)
	if err := input.Close(); err != nil {
		t.Fatalf("an input-free command touched stdin: %v", err)
	}
}
