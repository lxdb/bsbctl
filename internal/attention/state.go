package attention

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/internal/identifier"
	"github.com/lxdb/bsbctl/internal/localstate"
)

const (
	StateVersion    = 1
	MaxStateEntries = 2048
	maxStateBytes   = 2 << 20
)

type StateIdentity struct {
	PluginID   string `json:"plugin_id"`
	InstanceID string `json:"instance_id"`
	Generation uint64 `json:"generation"`
	Channel    string `json:"channel"`
	Key        string `json:"key"`
}

func (i StateIdentity) Validate() error {
	var errs []error
	for name, value := range map[string]string{
		"plugin ID": i.PluginID, "instance ID": i.InstanceID, "channel": i.Channel, "key": i.Key,
	} {
		if err := identifier.Validate(name, value); err != nil {
			errs = append(errs, err)
		}
	}
	if i.Generation == 0 {
		errs = append(errs, errors.New("generation must be greater than zero"))
	}
	return errors.Join(errs...)
}

func (i StateIdentity) key() string {
	value, _ := identifier.Encode(i.PluginID, i.InstanceID, i.Channel, i.Key)
	return value
}

type AcknowledgementState struct {
	Identity   StateIdentity `json:"identity"`
	ObservedAt time.Time     `json:"observed_at"`
	TouchedAt  time.Time     `json:"touched_at"`
}

type LastShownState struct {
	Identity StateIdentity `json:"identity"`
	ShownAt  time.Time     `json:"shown_at"`
}

type StateDocument struct {
	Version          int                    `json:"version"`
	Acknowledgements []AcknowledgementState `json:"acknowledgements"`
	LastShown        []LastShownState       `json:"last_shown"`
}

type StateStoreStatus struct {
	Phase         string    `json:"phase"`
	LastErrorCode string    `json:"last_error_code,omitempty"`
	LastReadAt    time.Time `json:"last_read_at,omitempty"`
	LastWriteAt   time.Time `json:"last_write_at,omitempty"`
	Failures      uint64    `json:"failures"`
}

type StateError struct {
	Code string
	Err  error
}

func (e *StateError) Error() string { return e.Code }
func (e *StateError) Unwrap() error { return e.Err }

type StateStore struct {
	mu      sync.Mutex
	path    string
	replace func(string, any) (localstate.CommitOutcome, error)
	now     func() time.Time
	status  StateStoreStatus
}

func NewStateStore(path string) *StateStore {
	return &StateStore{path: path, replace: localstate.ReplaceJSON, now: time.Now, status: StateStoreStatus{Phase: "unavailable"}}
}

func (s *StateStore) Load() (StateDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	pathInfo, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.status.Phase = "loaded"
		s.status.LastErrorCode = ""
		s.status.LastReadAt = now
		return StateDocument{Version: StateVersion}, nil
	}
	if err != nil || !pathInfo.Mode().IsRegular() {
		return StateDocument{}, s.failLocked("attention_state_read_failed", errors.Join(err, errors.New("attention state is not a regular file")))
	}
	file, err := os.Open(s.path)
	if err != nil {
		return StateDocument{}, s.failLocked("attention_state_read_failed", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(pathInfo, info) {
		return StateDocument{}, s.failLocked("attention_state_read_failed", errors.Join(err, errors.New("attention state is not a regular file")))
	}
	if err := file.Chmod(0o600); err != nil {
		return StateDocument{}, s.failLocked("attention_state_read_failed", err)
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxStateBytes+1))
	decoder.DisallowUnknownFields()
	var document StateDocument
	if err := decoder.Decode(&document); err != nil {
		return StateDocument{}, s.failLocked("attention_state_corrupt", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return StateDocument{}, s.failLocked("attention_state_corrupt", errors.New("attention state has trailing JSON"))
	}
	if info.Size() > maxStateBytes {
		return StateDocument{}, s.failLocked("attention_state_corrupt", errors.New("attention state exceeds the byte limit"))
	}
	if document.Version != StateVersion {
		return StateDocument{}, s.failLocked("attention_state_incompatible", errors.New("attention state version is unsupported"))
	}
	if err := validateStateDocument(document); err != nil {
		return StateDocument{}, s.failLocked("attention_state_corrupt", err)
	}
	s.status.Phase = "loaded"
	s.status.LastErrorCode = ""
	s.status.LastReadAt = now
	return document, nil
}

func (s *StateStore) Save(document StateDocument) (localstate.CommitOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if document.Version == 0 {
		document.Version = StateVersion
	}
	if err := validateStateDocument(document); err != nil {
		return localstate.NotCommitted, s.failLocked("attention_state_invalid", err)
	}
	sortStateDocument(&document)
	outcome, err := s.replace(s.path, document)
	if err != nil {
		code := "attention_state_write_failed"
		if outcome == localstate.CommittedDurabilityUncertain {
			code = "attention_state_durability_uncertain"
		}
		return outcome, s.failLocked(code, err)
	}
	s.status.Phase = "loaded"
	s.status.LastErrorCode = ""
	s.status.LastWriteAt = s.now().UTC()
	return outcome, nil
}

func (s *StateStore) Status() StateStoreStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *StateStore) failLocked(code string, err error) error {
	s.status.Phase = "degraded"
	s.status.LastErrorCode = code
	s.status.Failures++
	return &StateError{Code: code, Err: err}
}

func validateStateDocument(document StateDocument) error {
	if document.Version != StateVersion {
		return errors.New("attention state version is unsupported")
	}
	if len(document.Acknowledgements)+len(document.LastShown) > MaxStateEntries {
		return fmt.Errorf("attention state exceeds %d entries", MaxStateEntries)
	}
	seenAcknowledgements := make(map[string]struct{}, len(document.Acknowledgements))
	for _, entry := range document.Acknowledgements {
		if err := entry.Identity.Validate(); err != nil || entry.ObservedAt.IsZero() || entry.TouchedAt.IsZero() {
			return errors.New("attention acknowledgement is invalid")
		}
		key := entry.Identity.key()
		if _, duplicate := seenAcknowledgements[key]; duplicate {
			return errors.New("attention acknowledgement identity is duplicated")
		}
		seenAcknowledgements[key] = struct{}{}
	}
	seenLastShown := make(map[string]struct{}, len(document.LastShown))
	for _, entry := range document.LastShown {
		if err := entry.Identity.Validate(); err != nil || entry.ShownAt.IsZero() {
			return errors.New("attention last-shown entry is invalid")
		}
		key := entry.Identity.key()
		if _, duplicate := seenLastShown[key]; duplicate {
			return errors.New("attention last-shown identity is duplicated")
		}
		seenLastShown[key] = struct{}{}
	}
	return nil
}

func sortStateDocument(document *StateDocument) {
	slices.SortFunc(document.Acknowledgements, func(left, right AcknowledgementState) int {
		return cmp.Compare(left.Identity.key(), right.Identity.key())
	})
	slices.SortFunc(document.LastShown, func(left, right LastShownState) int {
		return cmp.Compare(left.Identity.key(), right.Identity.key())
	})
}
