package localstate

import (
	"errors"
	"os"
	"path/filepath"
)

// RemoveAndSync removes one local-state file and fsyncs its parent directory.
// Its outcome identifies whether deletion crossed the filesystem boundary.
func RemoveAndSync(path string) (CommitOutcome, error) {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NotCommitted, nil
		}
		return NotCommitted, commitError(NotCommitted, "remove local state", err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return CommittedDurabilityUncertain, commitError(CommittedDurabilityUncertain, "open local state directory for sync", err)
	}
	if err := directory.Sync(); err != nil {
		closeErr := directory.Close()
		return CommittedDurabilityUncertain, commitError(CommittedDurabilityUncertain, "sync local state directory", errors.Join(err, closeErr))
	}
	if err := directory.Close(); err != nil {
		return CommittedDurabilityUncertain, commitError(CommittedDurabilityUncertain, "close local state directory", err)
	}
	return Committed, nil
}
