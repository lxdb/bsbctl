// Package localstate persists small local JSON documents with explicit commit
// outcomes at the atomic rename boundary.
package localstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CommitOutcome identifies whether replacement bytes crossed the atomic
// rename boundary.
type CommitOutcome string

const (
	NotCommitted                 CommitOutcome = "not_committed"
	Committed                    CommitOutcome = "committed"
	CommittedDurabilityUncertain CommitOutcome = "committed_durability_uncertain"
)

// IsCommitted reports whether the new bytes were renamed into place.
func (o CommitOutcome) IsCommitted() bool {
	return o == Committed || o == CommittedDurabilityUncertain
}

// CommitError preserves the truthful outcome when a replacement operation
// fails. Its text intentionally contains only the operation, never state data.
type CommitError struct {
	Outcome CommitOutcome
	Op      string
	Err     error
}

func (e *CommitError) Error() string { return fmt.Sprintf("%s: %v", e.Op, e.Err) }
func (e *CommitError) Unwrap() error { return e.Err }

type operations struct {
	mkdirAll       func(string, os.FileMode) error
	createTemp     func(string, string) (*os.File, error)
	chmod          func(*os.File, os.FileMode) error
	encode         func(io.Writer, any) error
	syncFile       func(*os.File) error
	closeFile      func(*os.File) error
	rename         func(string, string) error
	openDirectory  func(string) (*os.File, error)
	syncDirectory  func(*os.File) error
	closeDirectory func(*os.File) error
	remove         func(string) error
}

func defaultOperations() operations {
	return operations{
		mkdirAll:       os.MkdirAll,
		createTemp:     os.CreateTemp,
		chmod:          func(file *os.File, mode os.FileMode) error { return file.Chmod(mode) },
		encode:         encodeJSON,
		syncFile:       func(file *os.File) error { return file.Sync() },
		closeFile:      func(file *os.File) error { return file.Close() },
		rename:         os.Rename,
		openDirectory:  os.Open,
		syncDirectory:  func(file *os.File) error { return file.Sync() },
		closeDirectory: func(file *os.File) error { return file.Close() },
		remove:         os.Remove,
	}
}

// MarshalJSON returns the exact indented bytes written by ReplaceJSON.
func MarshalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := encodeJSON(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func encodeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// ReplaceJSON atomically replaces path with one JSON document. Files are mode
// 0600 and newly created directories are mode 0700.
func ReplaceJSON(path string, value any) (CommitOutcome, error) {
	return replaceJSON(path, value, defaultOperations())
}

// ReplaceJSONCompact atomically replaces path without adding presentation
// whitespace. It is suitable for stores whose byte bounds include envelopes.
func ReplaceJSONCompact(path string, value any) (CommitOutcome, error) {
	ops := defaultOperations()
	ops.encode = encodeJSONCompact
	return replaceJSON(path, value, ops)
}

// MarshalJSONCompact returns the exact bytes written by ReplaceJSONCompact.
// Raw JSON payloads retain their accepted byte representation without HTML
// escaping or presentation whitespace that could exceed their read limits.
func MarshalJSONCompact(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := encodeJSONCompact(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func encodeJSONCompact(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func replaceJSON(path string, value any, ops operations) (outcome CommitOutcome, resultErr error) {
	directory := filepath.Dir(path)
	if err := ops.mkdirAll(directory, 0o700); err != nil {
		return NotCommitted, commitError(NotCommitted, "create state directory", err)
	}
	temporary, err := ops.createTemp(directory, ".bsbctl-state-*")
	if err != nil {
		return NotCommitted, commitError(NotCommitted, "create temporary state", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	promoted := false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, ops.closeFile(temporary))
		}
		if !promoted {
			if removeErr := ops.remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, removeErr)
			}
		}
	}()
	failBeforeRename := func(operation string, err error) (CommitOutcome, error) {
		return NotCommitted, commitError(NotCommitted, operation, err)
	}
	if err := ops.chmod(temporary, 0o600); err != nil {
		return failBeforeRename("set temporary state permissions", err)
	}
	if err := ops.encode(temporary, value); err != nil {
		return failBeforeRename("encode temporary state", err)
	}
	if err := ops.syncFile(temporary); err != nil {
		return failBeforeRename("sync temporary state", err)
	}
	if err := ops.closeFile(temporary); err != nil {
		closed = true
		return failBeforeRename("close temporary state", err)
	}
	closed = true
	if err := ops.rename(temporaryPath, path); err != nil {
		return failBeforeRename("replace state", err)
	}
	promoted = true
	directoryFile, err := ops.openDirectory(directory)
	if err != nil {
		return CommittedDurabilityUncertain, commitError(CommittedDurabilityUncertain, "open state directory for sync", err)
	}
	if err := ops.syncDirectory(directoryFile); err != nil {
		closeErr := ops.closeDirectory(directoryFile)
		return CommittedDurabilityUncertain, commitError(CommittedDurabilityUncertain, "sync state directory", errors.Join(err, closeErr))
	}
	if err := ops.closeDirectory(directoryFile); err != nil {
		return CommittedDurabilityUncertain, commitError(CommittedDurabilityUncertain, "close state directory", err)
	}
	return Committed, nil
}

func commitError(outcome CommitOutcome, operation string, err error) error {
	return &CommitError{Outcome: outcome, Op: operation, Err: err}
}
