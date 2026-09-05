package attention

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestRecorderBoundsMemorySuppressesDuplicatesAndExplains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.jsonl")
	recorder, err := NewRecorder(path, 2, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	first := Trace{At: now, SelectedID: "one", Outcome: OutcomeDrawn, Evaluations: []Evaluation{{ObservationID: "one", Reason: ReasonSelected}}}
	if err := recorder.Append(first); err != nil {
		t.Fatal(err)
	}
	first.At = now.Add(time.Second)
	if err := recorder.Append(first); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Append(Trace{At: now.Add(2 * time.Second), SelectedID: "two", Outcome: OutcomeDrawn, Evaluations: []Evaluation{{ObservationID: "one", Reason: ReasonLowerBand}, {ObservationID: "two", Reason: ReasonSelected}}}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Append(Trace{At: now.Add(3 * time.Second), Outcome: OutcomeDeviceUnavailable, Evaluations: []Evaluation{{ObservationID: "one", Reason: ReasonSelected}}}); err != nil {
		t.Fatal(err)
	}

	history := recorder.History(10, time.Time{})
	if len(history) != 2 || history[0].Sequence != 2 || history[1].Sequence != 3 {
		t.Fatalf("history = %#v", history)
	}
	evaluation, ok := recorder.Explain("two")
	if !ok || evaluation.Reason != ReasonSelected {
		t.Fatalf("explain = %#v/%v", evaluation, ok)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestRecorderSemanticFingerprintIncludesEveryExposedDecisionField(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	base := Trace{At: now, SelectedID: "one", Outcome: OutcomeDrawn, Evaluations: []Evaluation{{
		ObservationID: "one", PluginID: "plugin", InstanceID: "app", Channel: "main",
		Policy: "attention", Disposition: "actionable", Impact: "normal", ReasonCode: "base",
		Reason: ReasonSelected, EvaluatedAt: now, CooldownUntil: now.Add(time.Minute),
	}}}
	mutations := []struct {
		name   string
		mutate func(*Trace)
	}{
		{"selected", func(v *Trace) { v.SelectedID = "two" }},
		{"outcome", func(v *Trace) { v.Outcome = OutcomeUnchanged }},
		{"evaluation order and count", func(v *Trace) { v.Evaluations = append([]Evaluation{{ObservationID: "other"}}, v.Evaluations...) }},
		{"observation id", func(v *Trace) { v.Evaluations[0].ObservationID = "two" }},
		{"plugin id", func(v *Trace) { v.Evaluations[0].PluginID = "other" }},
		{"instance id", func(v *Trace) { v.Evaluations[0].InstanceID = "other" }},
		{"channel", func(v *Trace) { v.Evaluations[0].Channel = "other" }},
		{"policy", func(v *Trace) { v.Evaluations[0].Policy = "rotation" }},
		{"disposition", func(v *Trace) { v.Evaluations[0].Disposition = "snapshot" }},
		{"impact", func(v *Trace) { v.Evaluations[0].Impact = "high" }},
		{"reason code", func(v *Trace) { v.Evaluations[0].ReasonCode = "changed" }},
		{"reason", func(v *Trace) { v.Evaluations[0].Reason = ReasonLowerBand }},
		{"cooldown", func(v *Trace) { v.Evaluations[0].CooldownUntil = now.Add(2 * time.Minute) }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			recorder, err := NewRecorder(filepath.Join(t.TempDir(), "attention.jsonl"), 4, 1<<20, 1)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = recorder.Close() })
			if err := recorder.Append(base); err != nil {
				t.Fatal(err)
			}
			changed := cloneTrace(base)
			changed.Sequence = 99
			changed.At = now.Add(time.Hour)
			changed.Evaluations[0].EvaluatedAt = now.Add(time.Hour)
			mutation.mutate(&changed)
			if err := recorder.Append(changed); err != nil {
				t.Fatal(err)
			}
			if got := recorder.History(0, time.Time{}); len(got) != 2 {
				t.Fatalf("semantic change was suppressed: %#v", got)
			}
			latest, ok := recorder.Explain(changed.Evaluations[len(changed.Evaluations)-1].ObservationID)
			if !ok {
				t.Fatal("newest semantics are not explainable")
			}
			if mutation.name != "evaluation order and count" && !reflect.DeepEqual(latest, changed.Evaluations[0]) {
				t.Fatalf("Explain = %#v, want %#v", latest, changed.Evaluations[0])
			}
		})
	}
}

func TestRecorderSuppressesOnlyTimestampChanges(t *testing.T) {
	recorder, err := NewRecorder(filepath.Join(t.TempDir(), "attention.jsonl"), 4, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	now := time.Now().UTC()
	trace := Trace{At: now, SelectedID: "one", Outcome: OutcomeDrawn, Evaluations: []Evaluation{{ObservationID: "one", Reason: ReasonSelected, EvaluatedAt: now}}}
	if err := recorder.Append(trace); err != nil {
		t.Fatal(err)
	}
	trace.Sequence = 44
	trace.At = now.Add(time.Minute)
	trace.Evaluations[0].EvaluatedAt = now.Add(time.Minute)
	if err := recorder.Append(trace); err != nil {
		t.Fatal(err)
	}
	if got := recorder.History(0, time.Time{}); len(got) != 1 || got[0].Sequence != 1 {
		t.Fatalf("history = %#v", got)
	}
}

func TestRecorderReopenRestoresBoundedHistorySequenceFingerprintAndStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.jsonl")
	now := time.Now().UTC()
	traces := []Trace{
		{At: now, SelectedID: "one", Evaluations: []Evaluation{{ObservationID: "one", Reason: ReasonSelected}}},
		{At: now.Add(time.Second), SelectedID: "two", Evaluations: []Evaluation{{ObservationID: "two", Reason: ReasonSelected}}},
		{At: now.Add(2 * time.Second), SelectedID: "three", Evaluations: []Evaluation{{ObservationID: "three", Reason: ReasonSelected}}},
	}
	recorder, err := NewRecorder(path, 2, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, trace := range traces {
		if err := recorder.Append(trace); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewRecorder(path, 2, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	history := reopened.History(0, time.Time{})
	if len(history) != 2 || history[0].Sequence != 2 || history[1].Sequence != 3 {
		t.Fatalf("history = %#v", history)
	}
	if _, ok := reopened.Explain("one"); ok {
		t.Fatal("capacity-evicted evaluation was restored")
	}
	if got, ok := reopened.Explain("three"); !ok || got.Reason != ReasonSelected {
		t.Fatalf("Explain = %#v/%v", got, ok)
	}
	status := reopened.Status()
	if status.Phase != RecorderHealthy || status.LastSequence != 3 || !status.LastTraceAt.Equal(traces[2].At) || status.LastSuccessAt.IsZero() {
		t.Fatalf("status = %#v", status)
	}
	duplicate := traces[2]
	duplicate.At = duplicate.At.Add(time.Hour)
	duplicate.Evaluations[0].EvaluatedAt = duplicate.At
	if err := reopened.Append(duplicate); err != nil {
		t.Fatal(err)
	}
	if got := reopened.History(0, time.Time{}); len(got) != 2 {
		t.Fatalf("dedup lost on reopen: %#v", got)
	}
	if err := reopened.Append(Trace{At: now.Add(4 * time.Second), SelectedID: "four", Evaluations: []Evaluation{{ObservationID: "four"}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := reopened.Snapshot(); got.Sequence != 4 {
		t.Fatalf("sequence = %d", got.Sequence)
	}
}

func TestRecorderReopenRestoresSequenceFromArchiveWhenActiveIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.jsonl")
	recorder, err := NewRecorder(path, 4, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	first := Trace{At: time.Now().UTC(), SelectedID: "one", Evaluations: []Evaluation{{ObservationID: "one"}}}
	if err := recorder.Append(first); err != nil {
		t.Fatal(err)
	}
	recorder.mu.Lock()
	err = recorder.rotate()
	recorder.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewRecorder(path, 4, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got, ok := reopened.Snapshot(); !ok || got.Sequence != 1 || got.SelectedID != "one" {
		t.Fatalf("snapshot = %#v/%v", got, ok)
	}
	if err := reopened.Append(Trace{At: time.Now().UTC(), SelectedID: "two", Evaluations: []Evaluation{{ObservationID: "two"}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := reopened.Snapshot(); got.Sequence != 2 {
		t.Fatalf("sequence = %d", got.Sequence)
	}
}

func TestRecorderReopenCombinesArchivesAndActiveForHistoryAndDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.jsonl")
	recorder, err := NewRecorder(path, 2, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first := Trace{At: now, SelectedID: "one", Evaluations: []Evaluation{{ObservationID: "one"}}}
	second := Trace{At: now.Add(time.Second), SelectedID: "two", Evaluations: []Evaluation{{ObservationID: "two"}}}
	third := Trace{At: now.Add(2 * time.Second), SelectedID: "three", Evaluations: []Evaluation{{ObservationID: "three"}}}
	if err := recorder.Append(first); err != nil {
		t.Fatal(err)
	}
	recorder.mu.Lock()
	err = recorder.rotate()
	recorder.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Append(second); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Append(third); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewRecorder(path, 2, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	history := reopened.History(0, time.Time{})
	if len(history) != 2 || history[0].Sequence != 2 || history[1].Sequence != 3 {
		t.Fatalf("history = %#v", history)
	}
	third.At = third.At.Add(time.Hour)
	if err := reopened.Append(third); err != nil {
		t.Fatal(err)
	}
	if got := reopened.History(0, time.Time{}); len(got) != 2 {
		t.Fatalf("archive dedup lost: %#v", got)
	}
}

func TestRecorderReopenAcceptsRetainedLedgerStartingAboveOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.jsonl")
	recorder, err := NewRecorder(path, 4, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	for sequence := 1; sequence <= 3; sequence++ {
		trace := Trace{At: time.Now().UTC(), SelectedID: fmt.Sprintf("item-%d", sequence), Evaluations: []Evaluation{{ObservationID: fmt.Sprintf("item-%d", sequence)}}}
		if err := recorder.Append(trace); err != nil {
			t.Fatal(err)
		}
		recorder.mu.Lock()
		err = recorder.rotate()
		recorder.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("oldest archive retained: %v", err)
	}
	reopened, err := NewRecorder(path, 4, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	history := reopened.History(0, time.Time{})
	if len(history) != 2 || history[0].Sequence != 2 || history[1].Sequence != 3 {
		t.Fatalf("history = %#v", history)
	}
	if err := reopened.Append(Trace{At: time.Now().UTC(), SelectedID: "item-4", Evaluations: []Evaluation{{ObservationID: "item-4"}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := reopened.Snapshot(); got.Sequence != 4 {
		t.Fatalf("sequence = %d", got.Sequence)
	}
}

func TestRecorderReopenRejectsGapOrDuplicateAcrossArchiveBoundary(t *testing.T) {
	for _, test := range []struct {
		name           string
		activeSequence uint64
	}{{"gap", 7}, {"duplicate", 5}} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "attention.jsonl")
			writeTraceLedger(t, path+".1", Trace{Sequence: 5, At: time.Now().UTC(), SelectedID: "archive"})
			writeTraceLedger(t, path, Trace{Sequence: test.activeSequence, At: time.Now().UTC(), SelectedID: "active"})
			if recorder, err := NewRecorder(path, 4, 1<<20, 2); err == nil {
				_ = recorder.Close()
				t.Fatal("non-contiguous ledger accepted")
			}
		})
	}
}

func writeTraceLedger(t *testing.T, path string, traces ...Trace) {
	t.Helper()
	var data []byte
	for _, trace := range traces {
		line, err := json.Marshal(trace)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecorderReopenTruncatesPartialTrailingRecordAndRemainsWritable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.jsonl")
	recorder, err := NewRecorder(path, 4, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Append(Trace{At: time.Now().UTC(), SelectedID: "one", Evaluations: []Evaluation{{ObservationID: "one"}}}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"sequence":2,"selected_id":"partial"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewRecorder(path, 4, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	if status := reopened.Status(); status.Phase != RecorderDegraded || status.LastErrorCode != "trailing_partial_recovered" || status.LastSequence != 1 {
		t.Fatalf("status = %#v", status)
	}
	if err := reopened.Append(Trace{At: time.Now().UTC(), SelectedID: "two", Evaluations: []Evaluation{{ObservationID: "two"}}}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	parsed := parseLedger(t, path)
	if len(parsed) != 2 || parsed[0].Sequence != 1 || parsed[1].Sequence != 2 {
		t.Fatalf("ledger = %#v", parsed)
	}
}

func TestRecorderFailedWriteIsTransactionalAndCanRecover(t *testing.T) {
	dir := t.TempDir()
	ops := defaultRecorderOps()
	wrapped := &faultFile{}
	open := ops.openFile
	ops.openFile = func(path string, flags int, mode os.FileMode) (recorderFile, error) {
		file, err := open(path, flags, mode)
		if err != nil {
			return nil, err
		}
		wrapped.recorderFile = file
		return wrapped, nil
	}
	recorder, err := newRecorder(filepath.Join(dir, "attention.jsonl"), 4, 1<<20, 1, ops)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	wrapped.writeErr = errors.New("disk unavailable")
	trace := Trace{At: time.Now().UTC(), SelectedID: "one", Evaluations: []Evaluation{{ObservationID: "one"}}}
	if err := recorder.Append(trace); err == nil {
		t.Fatal("Append succeeded during injected write failure")
	}
	if _, ok := recorder.Snapshot(); ok {
		t.Fatal("failed append entered history")
	}
	if status := recorder.Status(); status.Phase != RecorderDegraded || status.LastErrorCode != "write_failed" || status.LastSequence != 0 {
		t.Fatalf("status = %#v", status)
	}
	wrapped.writeErr = nil
	if err := recorder.Append(trace); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got, ok := recorder.Snapshot(); !ok || got.Sequence != 1 {
		t.Fatalf("snapshot = %#v/%v", got, ok)
	}
	if status := recorder.Status(); status.Phase != RecorderHealthy || status.LastErrorCode != "" || status.LastSequence != 1 || status.LastTraceAt.IsZero() {
		t.Fatalf("recovered status = %#v", status)
	}
}

func TestRecorderShortWriteDoesNotAdvanceSequenceOrHistory(t *testing.T) {
	ops := defaultRecorderOps()
	wrapped := &faultFile{}
	open := ops.openFile
	ops.openFile = func(path string, flags int, mode os.FileMode) (recorderFile, error) {
		file, err := open(path, flags, mode)
		if err != nil {
			return nil, err
		}
		wrapped.recorderFile = file
		return wrapped, nil
	}
	recorder, err := newRecorder(filepath.Join(t.TempDir(), "attention.jsonl"), 4, 1<<20, 1, ops)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	wrapped.shortWrite = true
	wrapped.truncateErr = errors.New("rollback unavailable")
	trace := Trace{At: time.Now().UTC(), SelectedID: "one", Evaluations: []Evaluation{{ObservationID: "one"}}}
	if err := recorder.Append(trace); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Append = %v", err)
	}
	if got := recorder.History(0, time.Time{}); len(got) != 0 {
		t.Fatalf("history = %#v", got)
	}
	wrapped.shortWrite = false
	if err := recorder.Append(trace); err == nil {
		t.Fatal("pending rollback unexpectedly recovered while truncate still failed")
	}
	wrapped.truncateErr = nil
	if err := recorder.Append(trace); err != nil {
		t.Fatal(err)
	}
	if got, _ := recorder.Snapshot(); got.Sequence != 1 {
		t.Fatalf("sequence = %d", got.Sequence)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	parsed := parseLedger(t, recorder.path)
	if len(parsed) != 1 || parsed[0].Sequence != 1 {
		t.Fatalf("ledger = %#v", parsed)
	}
}

func TestRecorderRotationFailureKeepsActiveFileRecoverable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.jsonl")
	ops := defaultRecorderOps()
	realRename := ops.rename
	fail := true
	ops.rename = func(oldPath, newPath string) error {
		if fail && newPath == path+".1" {
			return errors.New("archive rename failed")
		}
		return realRename(oldPath, newPath)
	}
	recorder, err := newRecorder(path, 8, 200, 2, ops)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	first := Trace{At: time.Now().UTC(), SelectedID: "first", Outcome: OutcomeDrawn, Evaluations: []Evaluation{{ObservationID: "first", Reason: ReasonSelected}}}
	if err := recorder.Append(first); err != nil {
		t.Fatal(err)
	}
	second := Trace{At: time.Now().UTC(), SelectedID: "second", Outcome: OutcomeDrawn, Evaluations: []Evaluation{{ObservationID: "second", Reason: ReasonSelected}}}
	if err := recorder.Append(second); err == nil {
		t.Fatal("rotation unexpectedly succeeded")
	}
	if got, _ := recorder.Snapshot(); got.SelectedID != "first" || got.Sequence != 1 {
		t.Fatalf("snapshot = %#v", got)
	}
	fail = false
	if err := recorder.Append(second); err != nil {
		t.Fatalf("retry after rotation fault: %v", err)
	}
	if got, _ := recorder.Snapshot(); got.SelectedID != "second" || got.Sequence != 2 {
		t.Fatalf("snapshot = %#v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestRecorderRotationCommitsArchiveBeforeCleanupAndNeverDuplicatesOnRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.jsonl")
	now := time.Now().UTC()
	writeTraceLedger(t, path+".5", Trace{Sequence: 1, At: now, SelectedID: "old-five"})
	writeTraceLedger(t, path+".8", Trace{Sequence: 2, At: now.Add(time.Second), SelectedID: "old-eight"})
	oldFive, err := os.ReadFile(path + ".5")
	if err != nil {
		t.Fatal(err)
	}
	oldEight, err := os.ReadFile(path + ".8")
	if err != nil {
		t.Fatal(err)
	}
	ops := defaultRecorderOps()
	realRename, realRemove := ops.rename, ops.remove
	failRename, failRemove := true, false
	ops.rename = func(oldPath, newPath string) error {
		if failRename && newPath == path+".9" {
			return errors.New("commit rename failed")
		}
		return realRename(oldPath, newPath)
	}
	ops.remove = func(removePath string) error {
		if failRemove && removePath == path+".5" {
			return errors.New("retention remove failed")
		}
		return realRemove(removePath)
	}
	wrapped := &faultFile{}
	open := ops.openFile
	ops.openFile = func(openPath string, flags int, mode os.FileMode) (recorderFile, error) {
		file, err := open(openPath, flags, mode)
		if err != nil {
			return nil, err
		}
		if openPath == path {
			wrapped.recorderFile = file
			return wrapped, nil
		}
		return file, nil
	}
	recorder, err := newRecorder(path, 8, 1<<20, 2, ops)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	first := Trace{At: time.Now().UTC(), SelectedID: "first", Outcome: OutcomeDrawn, Evaluations: []Evaluation{{ObservationID: "first", Reason: ReasonSelected}}}
	if err := recorder.Append(first); err != nil {
		t.Fatal(err)
	}
	activeBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	recorder.maxBytes = recorder.size + 1
	second := Trace{At: time.Now().UTC(), SelectedID: "second", Outcome: OutcomeDrawn, Evaluations: []Evaluation{{ObservationID: "second", Reason: ReasonSelected}}}

	if err := recorder.Append(second); err == nil {
		t.Fatal("commit rename unexpectedly succeeded")
	}
	assertBytes(t, path+".5", oldFive)
	assertBytes(t, path+".8", oldEight)
	if _, err := os.Stat(path + ".9"); !os.IsNotExist(err) {
		t.Fatalf("uncommitted archive exists: %v", err)
	}
	assertBytes(t, path, activeBefore)

	failRename = false
	wrapped.truncateErr = errors.New("active truncate failed")
	if err := recorder.Append(second); err == nil {
		t.Fatal("truncate unexpectedly succeeded")
	}
	assertBytes(t, path+".5", oldFive)
	assertBytes(t, path+".8", oldEight)
	assertBytes(t, path+".9", activeBefore)
	assertBytes(t, path, activeBefore)

	wrapped.truncateErr = nil
	failRemove = true
	if err := recorder.Append(second); err == nil {
		t.Fatal("retention cleanup unexpectedly succeeded")
	}
	assertBytes(t, path+".5", oldFive)
	assertBytes(t, path+".8", oldEight)
	assertBytes(t, path+".9", activeBefore)
	for _, archive := range []string{path + ".8", path + ".9"} {
		info, err := os.Stat(archive)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", archive, info.Mode().Perm())
		}
	}
	assertBytes(t, path, nil)
	if _, err := os.Stat(path + ".10"); !os.IsNotExist(err) {
		t.Fatalf("duplicate archive exists: %v", err)
	}

	failRemove = false
	if err := recorder.Append(second); err != nil {
		t.Fatalf("recovery append: %v", err)
	}
	if _, err := os.Stat(path + ".5"); !os.IsNotExist(err) {
		t.Fatalf("old archive retained: %v", err)
	}
	assertBytes(t, path+".8", oldEight)
	assertBytes(t, path+".9", activeBefore)
	if _, err := os.Stat(path + ".10"); !os.IsNotExist(err) {
		t.Fatalf("duplicate archive exists: %v", err)
	}
	activeAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(activeAfter, []byte(`"selected_id":"second"`)) || bytes.Contains(activeAfter, []byte(`"selected_id":"first"`)) {
		t.Fatalf("active ledger after recovery = %s", activeAfter)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	assertBytes(t, path, []byte(want))
}
func assertBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func TestRecorderArchiveOpenFailureKeepsActiveFileRecoverable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.jsonl")
	ops := defaultRecorderOps()
	open := ops.openFile
	fail := true
	ops.openFile = func(openPath string, flags int, mode os.FileMode) (recorderFile, error) {
		if fail && openPath == path+".1.tmp" {
			return nil, errors.New("archive open failed")
		}
		return open(openPath, flags, mode)
	}
	recorder, err := newRecorder(path, 8, 200, 1, ops)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	first := Trace{At: time.Now().UTC(), SelectedID: "first", Outcome: OutcomeDrawn, Evaluations: []Evaluation{{ObservationID: "first", Reason: ReasonSelected}}}
	second := Trace{At: time.Now().UTC(), SelectedID: "second", Outcome: OutcomeDrawn, Evaluations: []Evaluation{{ObservationID: "second", Reason: ReasonSelected}}}
	if err := recorder.Append(first); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Append(second); err == nil {
		t.Fatal("rotation unexpectedly succeeded")
	}
	fail = false
	if err := recorder.Append(second); err != nil {
		t.Fatalf("retry after archive open fault: %v", err)
	}
}

func TestRecorderTruncateFailureKeepsActiveFileRecoverable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.jsonl")
	ops := defaultRecorderOps()
	wrapped := &faultFile{}
	open := ops.openFile
	ops.openFile = func(openPath string, flags int, mode os.FileMode) (recorderFile, error) {
		file, err := open(openPath, flags, mode)
		if err != nil {
			return nil, err
		}
		if openPath == path {
			wrapped.recorderFile = file
			return wrapped, nil
		}
		return file, nil
	}
	recorder, err := newRecorder(path, 8, 200, 0, ops)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	first := Trace{At: time.Now().UTC(), SelectedID: "first", Outcome: OutcomeDrawn, Evaluations: []Evaluation{{ObservationID: "first", Reason: ReasonSelected}}}
	if err := recorder.Append(first); err != nil {
		t.Fatal(err)
	}
	wrapped.truncateErr = errors.New("truncate failed")
	second := Trace{At: time.Now().UTC(), SelectedID: "second", Outcome: OutcomeDrawn, Evaluations: []Evaluation{{ObservationID: "second", Reason: ReasonSelected}}}
	if err := recorder.Append(second); err == nil {
		t.Fatal("rotation unexpectedly succeeded")
	}
	wrapped.truncateErr = nil
	if err := recorder.Append(second); err != nil {
		t.Fatalf("retry after truncate fault: %v", err)
	}
}

func TestRecorderReopenFinishesCommittedArchiveWithoutDuplicatingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.jsonl")
	ops := defaultRecorderOps()
	wrapped := &faultFile{}
	open := ops.openFile
	ops.openFile = func(openPath string, flags int, mode os.FileMode) (recorderFile, error) {
		file, err := open(openPath, flags, mode)
		if err != nil {
			return nil, err
		}
		if openPath == path {
			wrapped.recorderFile = file
			return wrapped, nil
		}
		return file, nil
	}
	recorder, err := newRecorder(path, 8, 1<<20, 1, ops)
	if err != nil {
		t.Fatal(err)
	}
	first := Trace{At: time.Now().UTC(), SelectedID: "first", Evaluations: []Evaluation{{ObservationID: "first"}}}
	if err := recorder.Append(first); err != nil {
		t.Fatal(err)
	}
	activeBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	recorder.maxBytes = recorder.size + 1
	wrapped.truncateErr = errors.New("truncate failed")
	second := Trace{At: time.Now().UTC(), SelectedID: "second", Evaluations: []Evaluation{{ObservationID: "second"}}}
	if err := recorder.Append(second); err == nil {
		t.Fatal("rotation unexpectedly succeeded")
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	assertBytes(t, path+".1", activeBefore)

	reopened, err := NewRecorder(path, 8, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	if status := reopened.Status(); status.Phase != RecorderDegraded || status.LastErrorCode != "rotation_recovery_pending" {
		t.Fatalf("status = %#v", status)
	}
	if err := reopened.Append(second); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Fatalf("duplicate archive exists: %v", err)
	}
	assertBytes(t, path+".1", activeBefore)
	parsed := parseLedger(t, path)
	if len(parsed) != 1 || parsed[0].Sequence != 2 || parsed[0].SelectedID != "second" {
		t.Fatalf("ledger = %#v", parsed)
	}
}

func TestRecorderCloseIsIdempotent(t *testing.T) {
	recorder, err := NewRecorder(filepath.Join(t.TempDir(), "attention.jsonl"), 2, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

type faultFile struct {
	recorderFile
	writeErr    error
	shortWrite  bool
	truncateErr error
}

func (f *faultFile) Write(value []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.shortWrite {
		return f.recorderFile.Write(value[:len(value)/2])
	}
	return f.recorderFile.Write(value)
}

func (f *faultFile) Truncate(size int64) error {
	if f.truncateErr != nil {
		return f.truncateErr
	}
	return f.recorderFile.Truncate(size)
}

var _ io.Writer = (*faultFile)(nil)

func parseLedger(t *testing.T, path string) []Trace {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(data, []byte{'\n'})
	result := make([]Trace, 0, len(lines)-1)
	for index, line := range lines {
		if len(line) == 0 {
			continue
		}
		var trace Trace
		if err := json.Unmarshal(line, &trace); err != nil {
			t.Fatalf("line %d: %v (%q)", index+1, err, line)
		}
		result = append(result, trace)
	}
	return result
}

func TestRecorderRotatesBoundedJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.jsonl")
	recorder, err := NewRecorder(path, 8, 180, 2)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 10; index++ {
		trace := Trace{At: time.Now(), SelectedID: string(rune('a' + index)), Outcome: OutcomeDrawn, Evaluations: []Evaluation{{ObservationID: string(rune('a' + index)), Reason: ReasonSelected}}}
		if err := recorder.Append(trace); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	archives, err := filepath.Glob(path + ".[0-9]*")
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 2 {
		t.Fatalf("archives = %#v", archives)
	}
}
