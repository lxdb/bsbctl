package attention

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type DeliveryOutcome string

const (
	OutcomeDrawn               DeliveryOutcome = "drawn"
	OutcomeCleared             DeliveryOutcome = "cleared"
	OutcomeUnchanged           DeliveryOutcome = "unchanged"
	OutcomeFirmwareSuppressed  DeliveryOutcome = "firmware_suppressed_409"
	OutcomeDeviceUnavailable   DeliveryOutcome = "device_unavailable"
	OutcomeAssetMissing        DeliveryOutcome = "asset_missing"
	OutcomeInvalidPresentation DeliveryOutcome = "invalid_presentation"
)

type Trace struct {
	Sequence    uint64          `json:"sequence"`
	At          time.Time       `json:"at"`
	SelectedID  string          `json:"selected_id,omitempty"`
	Outcome     DeliveryOutcome `json:"outcome,omitempty"`
	Evaluations []Evaluation    `json:"evaluations"`
}

type RecorderPhase string

const (
	RecorderUnavailable RecorderPhase = "unavailable"
	RecorderHealthy     RecorderPhase = "healthy"
	RecorderDegraded    RecorderPhase = "degraded"
)

// RecorderStatus exposes recorder freshness without leaking paths or raw I/O errors.
type RecorderStatus struct {
	Phase         RecorderPhase `json:"phase"`
	LastErrorCode string        `json:"last_error_code,omitempty"`
	LastErrorAt   time.Time     `json:"last_error_at,omitempty"`
	LastSuccessAt time.Time     `json:"last_success_at,omitempty"`
	LastTraceAt   time.Time     `json:"last_trace_at,omitempty"`
	LastSequence  uint64        `json:"last_sequence"`
}

type recorderFile interface {
	io.Reader
	io.Writer
	Stat() (os.FileInfo, error)
	Chmod(os.FileMode) error
	Close() error
	Truncate(int64) error
	Seek(int64, int) (int64, error)
	Sync() error
}

type recorderOps struct {
	mkdirAll func(string, os.FileMode) error
	openFile func(string, int, os.FileMode) (recorderFile, error)
	openRead func(string) (io.ReadCloser, error)
	remove   func(string) error
	rename   func(string, string) error
	readDir  func(string) ([]os.DirEntry, error)
}

func defaultRecorderOps() recorderOps {
	return recorderOps{
		mkdirAll: os.MkdirAll,
		openFile: func(path string, flags int, mode os.FileMode) (recorderFile, error) {
			return os.OpenFile(path, flags, mode)
		},
		openRead: func(path string) (io.ReadCloser, error) { return os.Open(path) },
		remove:   os.Remove, rename: os.Rename,
		readDir: os.ReadDir,
	}
}

// Recorder keeps bounded in-memory decisions and a rotated local JSONL ledger.
type Recorder struct {
	mu                   sync.Mutex
	path                 string
	capacity             int
	maxBytes             int64
	retained             int
	ops                  recorderOps
	file                 recorderFile
	size                 int64
	next                 uint64
	values               []Trace
	lastKey              string
	status               RecorderStatus
	closed               bool
	archives             []uint64
	nextArchive          uint64
	pendingArchive       bool
	cleanupPending       bool
	writeRecoveryPending bool
	writeRecoverySize    int64
}

func NewRecorder(path string, capacity int, maxBytes int64, retained int) (*Recorder, error) {
	return newRecorder(path, capacity, maxBytes, retained, defaultRecorderOps())
}

func newRecorder(path string, capacity int, maxBytes int64, retained int, ops recorderOps) (*Recorder, error) {
	if path == "" || capacity < 1 || maxBytes < 1 || retained < 0 {
		return nil, errors.New("attention recorder requires valid path and positive bounds")
	}
	if err := ops.mkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := ops.openFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	r := &Recorder{path: path, capacity: capacity, maxBytes: maxBytes, retained: retained, ops: ops, file: file, size: info.Size(), status: RecorderStatus{Phase: RecorderHealthy}}
	if err := r.initializeArchives(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := r.detectCommittedArchiveAwaitingTruncate(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if r.pendingArchive {
		if err := r.truncateActive(); err != nil {
			_ = file.Close()
			return nil, err
		}
		r.pendingArchive = false
	}
	if r.cleanupPending {
		if err := r.cleanupArchives(); err != nil {
			r.status.Phase, r.status.LastErrorCode, r.status.LastErrorAt = RecorderDegraded, "retention_cleanup_failed", time.Now().UTC()
		}
	}
	if err := r.restoreLedger(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return r, nil
}

func (r *Recorder) Append(trace Trace) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.fail("closed", errors.New("attention recorder is closed"))
	}
	if r.writeRecoveryPending {
		if err := r.recoverPartialWrite(); err != nil {
			return r.fail("write_recovery_failed", err)
		}
	}
	if r.pendingArchive {
		if err := r.rotate(); err != nil {
			return r.fail("rotation_failed", err)
		}
		r.status.Phase, r.status.LastErrorCode, r.status.LastErrorAt = RecorderHealthy, "", time.Time{}
	}
	if r.cleanupPending {
		if err := r.cleanupArchives(); err != nil {
			return r.fail("retention_cleanup_failed", err)
		}
	}
	key, err := materialKey(trace)
	if err != nil {
		return r.fail("encode_failed", err)
	}
	if key == r.lastKey {
		return nil
	}
	trace.Sequence = r.next + 1
	trace.Evaluations = slices.Clone(trace.Evaluations)
	line, err := json.Marshal(trace)
	if err != nil {
		return r.fail("encode_failed", err)
	}
	line = append(line, '\n')
	if r.size > 0 && r.size+int64(len(line)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return r.fail("rotation_failed", err)
		}
	}
	start := r.size
	written, writeErr := r.file.Write(line)
	if writeErr != nil || written != len(line) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		rollbackErr := r.file.Truncate(start)
		_, seekErr := r.file.Seek(0, io.SeekEnd)
		if rollbackErr != nil || seekErr != nil {
			r.writeRecoveryPending, r.writeRecoverySize = true, start
		}
		r.refreshSize()
		return r.fail("write_failed", errors.Join(writeErr, rollbackErr, seekErr))
	}
	r.size = start + int64(len(line))
	r.next = trace.Sequence
	r.values = append(r.values, trace)
	if len(r.values) > r.capacity {
		r.values = append([]Trace(nil), r.values[len(r.values)-r.capacity:]...)
	}
	r.lastKey = key
	r.status = RecorderStatus{Phase: RecorderHealthy, LastSuccessAt: time.Now().UTC(), LastTraceAt: trace.At, LastSequence: trace.Sequence}
	return nil
}

func (r *Recorder) Snapshot() (Trace, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.values) == 0 {
		return Trace{}, false
	}
	return cloneTrace(r.values[len(r.values)-1]), true
}

func (r *Recorder) Explain(observationID string) (Evaluation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := len(r.values) - 1; index >= 0; index-- {
		for _, value := range r.values[index].Evaluations {
			if value.ObservationID == observationID {
				return value, true
			}
		}
	}
	return Evaluation{}, false
}

func (r *Recorder) History(limit int, since time.Time) []Trace {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > len(r.values) {
		limit = len(r.values)
	}
	start := len(r.values) - limit
	for start < len(r.values) && !since.IsZero() && r.values[start].At.Before(since) {
		start++
	}
	result := make([]Trace, 0, len(r.values)-start)
	for _, value := range r.values[start:] {
		result = append(result, cloneTrace(value))
	}
	return result
}

func (r *Recorder) Status() RecorderStatus { r.mu.Lock(); defer r.mu.Unlock(); return r.status }

func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.file.Close()
}

// rotate prepares the archive while the active descriptor remains open. Any
// archive failure leaves the active ledger available for a later retry.
func (r *Recorder) rotate() error {
	if r.pendingArchive {
		if err := r.truncateActive(); err != nil {
			return err
		}
		r.pendingArchive = false
		if err := r.cleanupArchives(); err != nil {
			r.cleanupPending = true
			return err
		}
		return nil
	}
	if r.retained > 0 {
		if err := r.file.Sync(); err != nil {
			return err
		}
		generation := r.nextArchive
		tempPath := fmt.Sprintf("%s.%d.tmp", r.path, generation)
		if err := r.ops.remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		source, err := r.ops.openRead(r.path)
		if err != nil {
			return err
		}
		archive, err := r.ops.openFile(tempPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			_ = source.Close()
			return err
		}
		_, copyErr := io.Copy(archive, source)
		closeSourceErr := source.Close()
		chmodErr := archive.Chmod(0o600)
		syncErr := archive.Sync()
		closeArchiveErr := archive.Close()
		if err := errors.Join(copyErr, closeSourceErr, chmodErr, syncErr, closeArchiveErr); err != nil {
			_ = r.ops.remove(tempPath)
			return err
		}
		archivePath := fmt.Sprintf("%s.%d", r.path, generation)
		if err := r.ops.rename(tempPath, archivePath); err != nil {
			_ = r.ops.remove(tempPath)
			return err
		}
		r.archives = append(r.archives, generation)
		r.nextArchive++
		r.pendingArchive = true
	}
	if err := r.truncateActive(); err != nil {
		return err
	}
	r.pendingArchive = false
	if err := r.cleanupArchives(); err != nil {
		r.cleanupPending = true
		return err
	}
	return nil
}

func (r *Recorder) truncateActive() error {
	if err := r.file.Truncate(0); err != nil {
		return err
	}
	if _, err := r.file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	r.size = 0
	return nil
}

func (r *Recorder) cleanupArchives() error {
	for len(r.archives) > r.retained {
		path := fmt.Sprintf("%s.%d", r.path, r.archives[0])
		if err := r.ops.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		r.archives = r.archives[1:]
	}
	r.cleanupPending = false
	return nil
}

func (r *Recorder) refreshSize() {
	if info, err := r.file.Stat(); err == nil {
		r.size = info.Size()
	}
}

func (r *Recorder) recoverPartialWrite() error {
	if err := r.file.Truncate(r.writeRecoverySize); err != nil {
		return err
	}
	if _, err := r.file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	r.size = r.writeRecoverySize
	r.writeRecoveryPending = false
	return nil
}

func (r *Recorder) initializeArchives() error {
	entries, err := r.ops.readDir(filepath.Dir(r.path))
	if err != nil {
		return err
	}
	prefix := filepath.Base(r.path) + "."
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		suffix := strings.TrimPrefix(entry.Name(), prefix)
		generation, err := strconv.ParseUint(suffix, 10, 64)
		if err != nil || generation == 0 || strconv.FormatUint(generation, 10) != suffix {
			continue
		}
		r.archives = append(r.archives, generation)
	}
	slices.Sort(r.archives)
	if len(r.archives) > 0 {
		r.nextArchive = r.archives[len(r.archives)-1] + 1
	} else {
		r.nextArchive = 1
	}
	r.cleanupPending = len(r.archives) > r.retained
	return nil
}

func (r *Recorder) restoreLedger() error {
	var previous uint64
	recordNumber := 0
	consumeLine := func(line []byte) error {
		recordNumber++
		var trace Trace
		if err := json.Unmarshal(line, &trace); err != nil {
			return fmt.Errorf("invalid attention trace at record %d", recordNumber)
		}
		if trace.Sequence == 0 || (previous != 0 && trace.Sequence != previous+1) {
			return errors.New("attention trace sequence is not strictly contiguous")
		}
		previous = trace.Sequence
		key, err := materialKey(trace)
		if err != nil {
			return err
		}
		r.next, r.lastKey = trace.Sequence, key
		r.values = append(r.values, cloneTrace(trace))
		if len(r.values) > r.capacity {
			copy(r.values, r.values[len(r.values)-r.capacity:])
			r.values = r.values[:r.capacity]
		}
		return nil
	}
	for _, generation := range r.archives {
		archive, err := r.ops.openRead(fmt.Sprintf("%s.%d", r.path, generation))
		if err != nil {
			return err
		}
		_, partial, readErr := consumeJSONLines(archive, r.maxBytes, consumeLine)
		closeErr := archive.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if partial {
			return errors.New("attention recorder archive has a partial trailing record")
		}
	}
	if r.size > r.maxBytes {
		return errors.New("attention recorder active ledger exceeds configured bound")
	}
	if _, err := r.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	validEnd, partial, err := consumeJSONLines(r.file, r.maxBytes, consumeLine)
	if err != nil {
		return err
	}
	if partial {
		if err := r.file.Truncate(validEnd); err != nil {
			return err
		}
		r.status.Phase, r.status.LastErrorCode, r.status.LastErrorAt = RecorderDegraded, "trailing_partial_recovered", time.Now().UTC()
	}
	if _, err := r.file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	r.size = validEnd
	if len(r.values) > 0 {
		latest := r.values[len(r.values)-1]
		r.status.LastSuccessAt, r.status.LastTraceAt, r.status.LastSequence = latest.At, latest.At, latest.Sequence
	}
	return nil
}

func consumeJSONLines(reader io.Reader, maxBytes int64, consume func([]byte) error) (validBytes int64, partial bool, result error) {
	buffered := bufio.NewReader(io.LimitReader(reader, maxBytes+1))
	var total int64
	for {
		line, err := buffered.ReadBytes('\n')
		total += int64(len(line))
		if total > maxBytes {
			return validBytes, false, errors.New("attention recorder ledger exceeds configured bound")
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			if err := consume(line[:len(line)-1]); err != nil {
				return validBytes, false, err
			}
			validBytes = total
		}
		if errors.Is(err, io.EOF) {
			return validBytes, len(line) > 0 && line[len(line)-1] != '\n', nil
		}
		if err != nil {
			return validBytes, false, err
		}
	}
}

// A process can stop after committing an archive but before truncating the
// active ledger. Exact byte equality identifies that recoverable transaction
// without requiring a separate mutable journal.
func (r *Recorder) detectCommittedArchiveAwaitingTruncate() error {
	if r.size == 0 || len(r.archives) == 0 {
		return nil
	}
	if _, err := r.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	activeHash, activeBytes, err := boundedHash(r.file, r.maxBytes)
	if err != nil {
		return err
	}
	if _, err := r.file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	latestPath := fmt.Sprintf("%s.%d", r.path, r.archives[len(r.archives)-1])
	archive, err := r.ops.openRead(latestPath)
	if err != nil {
		return err
	}
	archiveHash, archiveBytes, readErr := boundedHash(archive, r.maxBytes)
	closeErr := archive.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if activeBytes == archiveBytes && activeHash == archiveHash {
		r.pendingArchive = true
		r.status.Phase = RecorderDegraded
		r.status.LastErrorCode = "rotation_recovery_pending"
		r.status.LastErrorAt = time.Now().UTC()
	}
	return nil
}

func boundedHash(reader io.Reader, maxBytes int64) ([sha256.Size]byte, int64, error) {
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return [sha256.Size]byte{}, written, err
	}
	if written > maxBytes {
		return [sha256.Size]byte{}, written, errors.New("attention recorder ledger exceeds configured bound")
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, written, nil
}
func (r *Recorder) fail(code string, err error) error {
	r.status.Phase, r.status.LastErrorCode, r.status.LastErrorAt = RecorderDegraded, code, time.Now().UTC()
	return err
}

type semanticTrace struct {
	SelectedID  string               `json:"selected_id"`
	Outcome     DeliveryOutcome      `json:"outcome"`
	Evaluations []semanticEvaluation `json:"evaluations"`
}
type semanticEvaluation struct {
	ObservationID string    `json:"observation_id"`
	PluginID      string    `json:"plugin_id"`
	InstanceID    string    `json:"instance_id"`
	Channel       string    `json:"channel"`
	Policy        string    `json:"policy"`
	Disposition   string    `json:"disposition"`
	Impact        string    `json:"impact"`
	ReasonCode    string    `json:"reason_code"`
	Reason        Reason    `json:"reason"`
	CooldownUntil time.Time `json:"cooldown_until"`
}

func materialKey(trace Trace) (string, error) {
	semantic := semanticTrace{SelectedID: trace.SelectedID, Outcome: trace.Outcome, Evaluations: make([]semanticEvaluation, len(trace.Evaluations))}
	for index, evaluation := range trace.Evaluations {
		semantic.Evaluations[index] = semanticEvaluation{
			ObservationID: evaluation.ObservationID, PluginID: evaluation.PluginID, InstanceID: evaluation.InstanceID,
			Channel: evaluation.Channel, Policy: string(evaluation.Policy), Disposition: string(evaluation.Disposition),
			Impact: string(evaluation.Impact), ReasonCode: evaluation.ReasonCode, Reason: evaluation.Reason, CooldownUntil: evaluation.CooldownUntil,
		}
	}
	value, err := json.Marshal(semantic)
	return string(value), err
}

func cloneTrace(value Trace) Trace {
	value.Evaluations = slices.Clone(value.Evaluations)
	return value
}
