// Package checkpoint owns bounded, non-secret plugin state scoped to one
// authenticated plugin instance generation.
package checkpoint

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/lxdb/bsbctl/internal/identifier"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	MaxDataBytes          = protocol.MaxCheckpointObjectBytes
	MaxAggregateDataBytes = 16 << 20
	envelopeVersion       = 1
	maxEnvelopeBytes      = MaxDataBytes + 1024
)

// DiagnosticCode is a stable, content-free checkpoint failure category.
type DiagnosticCode string

const (
	InvalidCode             DiagnosticCode = "checkpoint_invalid"
	CapacityCode            DiagnosticCode = "checkpoint_capacity_exceeded"
	CommitFailedCode        DiagnosticCode = "checkpoint_commit_failed"
	DurabilityUncertainCode DiagnosticCode = "checkpoint_durability_uncertain"
	CorruptCode             DiagnosticCode = "checkpoint_corrupt"
	IOFailedCode            DiagnosticCode = "checkpoint_io_failed"
)

var (
	errCorruptEnvelope = errors.New("corrupt checkpoint envelope")
	openCheckpointFile = openCheckpointNoFollow
)

// Key is the full authenticated identity for one checkpoint.
type Key struct {
	PluginID   string
	InstanceID string
	Generation uint64
}

// Error exposes a stable diagnostic and commit outcome without rendering a
// checkpoint identity, payload, or underlying path.
type Error struct {
	Code    DiagnosticCode
	Outcome localstate.CommitOutcome
	Err     error
}

func (e *Error) Error() string { return string(e.Code) }
func (e *Error) Unwrap() error { return e.Err }

// Status is safe for daemon diagnostics. It contains counts and stable codes,
// never identities or checkpoint data.
type Status struct {
	Files         int            `json:"files"`
	DataBytes     int            `json:"data_bytes"`
	Failures      uint64         `json:"failures"`
	Corruptions   uint64         `json:"corruptions"`
	LastErrorCode DiagnosticCode `json:"last_error_code,omitempty"`
}

type envelope struct {
	Version    int             `json:"version"`
	PluginID   string          `json:"plugin_id"`
	InstanceID string          `json:"instance_id"`
	Generation uint64          `json:"generation"`
	Data       json.RawMessage `json:"data"`
}

func (e envelope) key() Key {
	return Key{PluginID: e.PluginID, InstanceID: e.InstanceID, Generation: e.Generation}
}

type record struct {
	path string
	data json.RawMessage
}

// Store serializes bounded checkpoint I/O under one lock.
type Store struct {
	mu            sync.Mutex
	root          string
	replace       func(string, any) (localstate.CommitOutcome, error)
	remove        func(string) error
	syncDirectory func(string) error
	status        Status
	records       map[Key]record
	initialized   bool
}

func NewStore(root string) *Store {
	return &Store{
		root:          root,
		replace:       localstate.ReplaceJSONCompact,
		remove:        os.Remove,
		syncDirectory: syncDirectory,
	}
}

// DefaultRoot returns the checkpoint directory beside one daemon config.
func DefaultRoot(configPath string) string { return configPath + ".checkpoints" }

// Save validates and atomically replaces one non-secret JSON checkpoint.
func (s *Store) Save(key Key, data json.RawMessage) (localstate.CommitOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateKey(key); err != nil {
		return localstate.NotCommitted, s.failLocked(InvalidCode, localstate.NotCommitted, err)
	}
	canonical, err := canonicalData(data)
	if err != nil {
		return localstate.NotCommitted, s.failLocked(InvalidCode, localstate.NotCommitted, err)
	}
	if err := s.ensureRoot(); err != nil {
		return localstate.NotCommitted, s.failLocked(IOFailedCode, localstate.NotCommitted, err)
	}
	records, err := s.inventoryLocked()
	if err != nil {
		return localstate.NotCommitted, err
	}
	total := totalBytes(records)
	if current, exists := records[key]; exists {
		total -= len(current.data)
	}
	if total > MaxAggregateDataBytes-len(canonical) {
		return localstate.NotCommitted, s.failLocked(CapacityCode, localstate.NotCommitted, errors.New("aggregate checkpoint capacity exceeded"))
	}
	value := envelope{
		Version: envelopeVersion, PluginID: key.PluginID, InstanceID: key.InstanceID,
		Generation: key.Generation, Data: append(json.RawMessage(nil), canonical...),
	}
	outcome, replaceErr := s.replace(s.path(key), value)
	if outcome.IsCommitted() {
		_, existed := records[key]
		if !existed {
			s.status.Files++
		}
		records[key] = record{path: s.path(key), data: append(json.RawMessage(nil), canonical...)}
		s.status.DataBytes = total + len(canonical)
	}
	if replaceErr == nil {
		return outcome, nil
	}
	if outcome == localstate.CommittedDurabilityUncertain {
		return outcome, s.failLocked(DurabilityUncertainCode, outcome, replaceErr)
	}
	return outcome, s.failLocked(CommitFailedCode, outcome, replaceErr)
}

// Load returns a deep copy only for an exact identity and generation. Corrupt,
// mismatched, and oversized files are removed and never delivered.
func (s *Store) Load(key Key) (json.RawMessage, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateKey(key); err != nil {
		return nil, false, s.failLocked(InvalidCode, localstate.NotCommitted, err)
	}
	if err := s.ensureRoot(); err != nil {
		return nil, false, s.failLocked(IOFailedCode, localstate.NotCommitted, err)
	}
	records, inventoryErr := s.inventoryLocked()
	if inventoryErr != nil {
		return nil, false, inventoryErr
	}
	current, exists := records[key]
	if !exists {
		return nil, false, nil
	}
	path := current.path
	value, err := readEnvelope(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || value.key() != key || filepath.Base(path) != filename(value.key()) {
		if err != nil && !errors.Is(err, errCorruptEnvelope) {
			return nil, false, s.failLocked(IOFailedCode, localstate.NotCommitted, err)
		}
		s.recordLocked(CorruptCode)
		outcome, removeErr := s.removeAndSync(path)
		if outcome.IsCommitted() && s.initialized {
			if current, exists := s.records[key]; exists {
				delete(s.records, key)
				s.status.Files = max(0, s.status.Files-1)
				s.status.DataBytes = max(0, s.status.DataBytes-len(current.data))
			}
		}
		if removeErr != nil {
			return nil, false, s.cleanupErrorLocked(outcome, removeErr)
		}
		return nil, false, nil
	}
	if s.initialized {
		s.records[key] = record{path: path, data: append(json.RawMessage(nil), value.Data...)}
	}
	return append(json.RawMessage(nil), value.Data...), true, nil
}

// Reset durably removes one exact checkpoint when present.
func (s *Store) Reset(key Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateKey(key); err != nil {
		return s.failLocked(InvalidCode, localstate.NotCommitted, err)
	}
	if err := s.ensureRoot(); err != nil {
		return s.failLocked(IOFailedCode, localstate.NotCommitted, err)
	}
	path := s.path(key)
	value, readErr := readEnvelope(path)
	if errors.Is(readErr, os.ErrNotExist) {
		return nil
	}
	outcome, removeErr := s.removeAndSync(path)
	if outcome.IsCommitted() && readErr == nil && value.key() == key {
		if s.initialized {
			delete(s.records, key)
		}
		s.status.Files = max(0, s.status.Files-1)
		s.status.DataBytes = max(0, s.status.DataBytes-len(value.Data))
	}
	if removeErr != nil {
		return s.cleanupErrorLocked(outcome, removeErr)
	}
	return nil
}

// Reconcile removes every checkpoint not in the exact active generation set.
func (s *Store) Reconcile(active []Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	activeSet := make(map[Key]struct{}, len(active))
	for _, key := range active {
		if err := validateKey(key); err != nil {
			return s.failLocked(InvalidCode, localstate.NotCommitted, err)
		}
		activeSet[key] = struct{}{}
	}
	if err := s.ensureRoot(); err != nil {
		return s.failLocked(IOFailedCode, localstate.NotCommitted, err)
	}
	records, scanOutcome, err := s.scanLocked()
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(records))
	keysByPath := make(map[string]Key, len(records))
	for key, record := range records {
		if _, keep := activeSet[key]; keep {
			continue
		}
		paths = append(paths, record.path)
		keysByPath[record.path] = key
	}
	result := s.removePaths(paths)
	for _, path := range result.removed {
		if key, exists := keysByPath[path]; exists {
			delete(records, key)
		}
	}
	s.status.Files = len(records)
	s.status.DataBytes = totalBytes(records)
	if result.err != nil {
		return s.cleanupErrorLocked(combineCleanupOutcomes(scanOutcome, result.outcome), result.err)
	}
	return nil
}

func (s *Store) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Store) ensureRoot() error {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return safeFilesystemError(err)
	}
	info, err := os.Lstat(s.root)
	if err != nil {
		return safeFilesystemError(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("checkpoint root is not a directory")
	}
	if err := os.Chmod(s.root, 0o700); err != nil {
		return safeFilesystemError(err)
	}
	return nil
}

func (s *Store) scanLocked() (map[Key]record, localstate.CommitOutcome, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, localstate.NotCommitted, s.failLocked(IOFailedCode, localstate.NotCommitted, safeFilesystemError(err))
	}
	slices.SortFunc(entries, func(left, right os.DirEntry) int { return cmp.Compare(left.Name(), right.Name()) })
	records := make(map[Key]record)
	cleanupPaths := make([]string, 0)
	total := 0
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(s.root, name)
		if strings.HasPrefix(name, ".bsbctl-state-") {
			cleanupPaths = append(cleanupPaths, path)
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		value, readErr := readEnvelope(path)
		if readErr != nil && !errors.Is(readErr, errCorruptEnvelope) {
			return nil, localstate.NotCommitted, s.failLocked(IOFailedCode, localstate.NotCommitted, readErr)
		}
		if readErr != nil || name != filename(value.key()) {
			s.recordLocked(CorruptCode)
			cleanupPaths = append(cleanupPaths, path)
			continue
		}
		if total > MaxAggregateDataBytes-len(value.Data) {
			s.recordLocked(CapacityCode)
			cleanupPaths = append(cleanupPaths, path)
			continue
		}
		records[value.key()] = record{path: path, data: append(json.RawMessage(nil), value.Data...)}
		total += len(value.Data)
	}
	s.status.Files = len(records)
	s.status.DataBytes = total
	result := s.removePaths(cleanupPaths)
	if result.err != nil {
		s.records = records
		s.initialized = false
		return nil, result.outcome, s.cleanupErrorLocked(result.outcome, result.err)
	}
	s.records = records
	s.initialized = true
	return records, result.outcome, nil
}

func (s *Store) inventoryLocked() (map[Key]record, error) {
	if s.initialized {
		return s.records, nil
	}
	records, _, err := s.scanLocked()
	return records, err
}

func readEnvelope(path string) (envelope, error) {
	file, err := openCheckpointFile(path)
	if err != nil {
		if errors.Is(err, errCorruptEnvelope) {
			return envelope{}, errCorruptEnvelope
		}
		return envelope{}, safeFilesystemError(err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return envelope{}, safeFilesystemError(statErr)
	}
	if !info.Mode().IsRegular() || info.Size() > maxEnvelopeBytes {
		_ = file.Close()
		return envelope{}, errCorruptEnvelope
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return envelope{}, safeFilesystemError(err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxEnvelopeBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return envelope{}, safeFilesystemError(errors.Join(readErr, closeErr))
	}
	if len(data) > maxEnvelopeBytes {
		return envelope{}, errCorruptEnvelope
	}
	var value envelope
	if err := protocol.DecodeStrict(data, &value); err != nil {
		return envelope{}, errCorruptEnvelope
	}
	if value.Version != envelopeVersion || validateKey(value.key()) != nil {
		return envelope{}, errCorruptEnvelope
	}
	canonical, err := canonicalData(value.Data)
	if err != nil {
		return envelope{}, errCorruptEnvelope
	}
	value.Data = canonical
	return value, nil
}

func canonicalData(data json.RawMessage) (json.RawMessage, error) {
	var object map[string]any
	if err := protocol.ValidateJSONObject("checkpoint data", data, false); err != nil || protocol.DecodeStrict(data, &object) != nil {
		return nil, errors.New("checkpoint data must be one JSON object no larger than 64 KiB")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return nil, errors.New("checkpoint data must be valid JSON")
	}
	return append(json.RawMessage(nil), compact.Bytes()...), nil
}

func validateKey(key Key) error {
	if err := identifier.Validate("plugin id", key.PluginID); err != nil {
		return err
	}
	if err := identifier.Validate("instance id", key.InstanceID); err != nil {
		return err
	}
	if key.Generation == 0 {
		return errors.New("checkpoint generation must be greater than zero")
	}
	return nil
}

func (s *Store) path(key Key) string { return filepath.Join(s.root, filename(key)) }

func filename(key Key) string {
	identity, _ := json.Marshal(struct {
		PluginID   string `json:"plugin_id"`
		InstanceID string `json:"instance_id"`
		Generation uint64 `json:"generation"`
	}{key.PluginID, key.InstanceID, key.Generation})
	digest := sha256.Sum256(identity)
	return hex.EncodeToString(digest[:]) + ".json"
}

func totalBytes(records map[Key]record) int {
	total := 0
	for _, record := range records {
		total += len(record.data)
	}
	return total
}

type cleanupResult struct {
	outcome localstate.CommitOutcome
	removed []string
	err     error
}

func (s *Store) removeAndSync(path string) (localstate.CommitOutcome, error) {
	result := s.removePaths([]string{path})
	return result.outcome, result.err
}

func (s *Store) removePaths(paths []string) cleanupResult {
	paths = slices.Clone(paths)
	slices.Sort(paths)
	result := cleanupResult{outcome: localstate.NotCommitted}
	var removeErr error
	for _, path := range paths {
		if err := s.remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			removeErr = safeFilesystemError(err)
			break
		}
		result.removed = append(result.removed, path)
	}
	if len(result.removed) == 0 {
		result.err = removeErr
		return result
	}
	if syncErr := s.syncDirectory(s.root); syncErr != nil {
		result.outcome = localstate.CommittedDurabilityUncertain
		result.err = errors.Join(removeErr, syncErr)
		return result
	}
	result.outcome = localstate.Committed
	result.err = removeErr
	return result
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return safeFilesystemError(err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return safeFilesystemError(errors.Join(syncErr, closeErr))
}

func safeFilesystemError(err error) error {
	if err == nil {
		return nil
	}
	if pathErr, ok := errors.AsType[*os.PathError](err); ok {
		return pathErr.Err
	}
	return err
}

func (s *Store) failLocked(code DiagnosticCode, outcome localstate.CommitOutcome, err error) error {
	s.recordLocked(code)
	return &Error{Code: code, Outcome: outcome, Err: err}
}

func (s *Store) cleanupErrorLocked(outcome localstate.CommitOutcome, err error) error {
	code := IOFailedCode
	if outcome == localstate.CommittedDurabilityUncertain {
		code = DurabilityUncertainCode
	}
	return s.failLocked(code, outcome, err)
}

func combineCleanupOutcomes(first, second localstate.CommitOutcome) localstate.CommitOutcome {
	if first == localstate.CommittedDurabilityUncertain || second == localstate.CommittedDurabilityUncertain {
		return localstate.CommittedDurabilityUncertain
	}
	if first == localstate.Committed || second == localstate.Committed {
		return localstate.Committed
	}
	return localstate.NotCommitted
}

func (s *Store) recordLocked(code DiagnosticCode) {
	s.status.Failures++
	if code == CorruptCode {
		s.status.Corruptions++
	}
	s.status.LastErrorCode = code
}
