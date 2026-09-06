package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func diagnosticWorker(t *testing.T, host *checkpointHost, checkpoint json.RawMessage) *worker {
	t.Helper()
	client := newSlackClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"url":"wss://wss-primary.slack.com/socket?ticket=ticket-canary"}`))}, nil
	})})
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"PUBLIC"}]}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Checkpoint: checkpoint, Secrets: map[string]string{"app_token": "app-canary"}}, cfg, host, client, blockedDial, time.Now)
	go w.run()
	t.Cleanup(func() { w.cancel(); <-w.done })
	synctest.Wait()
	return w
}

func TestDiagnosticsCoalesceCountsAndExcludeProviderCanaries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := &checkpointHost{}
		w := diagnosticWorker(t, host, []byte(`{"schema_version":99,"private":"checkpoint-canary"}`))
		for range 40 {
			w.reduce([]byte(`{"private":"malformed-canary",`))
			w.reduce(callback("event-id-canary", `{"type":"private-event-canary","text":"body-canary","url":"wss://ticket-canary"}`))
			w.markGap("queue_overflow", true)
		}
		w.reduce([]byte(`{"type":"app_rate_limited","team_id":"T123"}`))
		w.disconnected("auth_required")
		synctest.Wait()
		if got := host.logRecords(); len(got) != 0 {
			t.Fatalf("per-event log amplification: %d", len(got))
		}
		time.Sleep(15 * time.Second)
		synctest.Wait()
		logs := host.logRecords()
		if len(logs) != 1 {
			t.Fatalf("want one coalesced diagnostic batch, got %d", len(logs))
		}
		want := map[string]string{"invalid_event": "40", "unsupported_event": "40", "queue_overflow": "40", "throttled": "1", "auth_required": "1", "checkpoint_invalid": "1"}
		if !reflect.DeepEqual(logs[0].Fields, want) {
			t.Fatalf("diagnostic counts=%v want=%v", logs[0].Fields, want)
		}
		if err := logs[0].Validate(); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(logs)
		for _, forbidden := range []string{"canary", "T123", "U123", "C123", "wss://"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("diagnostic exposed provider data: %s", raw)
			}
		}
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if len(host.logRecords()) != 1 {
			t.Fatal("clean interval repeated old diagnostics")
		}
	})
}

func TestDiagnosticsSupportedFilteringStaysSilent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := &checkpointHost{}
		w := diagnosticWorker(t, host, nil)
		for i, msg := range []string{
			`{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","text":"ordinary"}`,
			`{"type":"message","channel":"C123","channel_type":"channel","user":"U123","ts":"2.000001","text":"own"}`,
			`{"type":"message","subtype":"bot_message","channel":"C123","channel_type":"channel","bot_id":"B123","ts":"3.000001","text":"bot"}`,
			`{"type":"message","channel":"C999","channel_type":"channel","user":"U456","ts":"4.000001","text":"<@U123> out of scope"}`,
		} {
			w.reduce(callback(fmt.Sprint(i), msg))
		}
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if logs := host.logRecords(); len(logs) != 0 {
			t.Fatalf("supported filtering was noisy: %+v", logs)
		}
	})
}

func TestDiagnosticsFailingLoggerDoesNotRetryAndJoins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{}, 1)
		exited := make(chan struct{})
		host := &checkpointHost{log: func(ctx context.Context, _ protocol.LogNotification) error {
			entered <- struct{}{}
			<-ctx.Done()
			close(exited)
			return errors.New("logger-canary")
		}}
		w := diagnosticWorker(t, host, nil)
		w.markGap("queue_overflow", true)
		time.Sleep(15 * time.Second)
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("reporter not started")
		}
		// A blocked logger cannot hold the reducer or prevent fresh source snapshots.
		w.reduce(callback("next", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"body-canary"}`))
		w.live()
		if s := w.snapshot(); len(s.Items) != 1 || !s.Fresh {
			t.Fatal("logger blocked state or liveness")
		}
		w.cancel()
		<-w.done
		select {
		case <-exited:
		default:
			t.Fatal("worker returned before logger joined")
		}
		if len(host.logRecords()) != 1 {
			t.Fatal("logger failure retried")
		}
	})
}

func TestDiagnosticsSourceFailuresAreCountedOnce(t *testing.T) {
	for _, code := range []string{"auth_required", "throttled", "request_failed", "invalid_response", "disconnected", "missing_scope"} {
		t.Run(code, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				host := &checkpointHost{}
				client := newSlackClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"url":"wss://wss-primary.slack.com/socket?ticket=private-canary"}`))}, nil
				})})
				dial := func(context.Context, string) (*websocket.Conn, error) {
					return nil, &sourceError{code: code, retryAfter: 601 * time.Second}
				}
				w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, config{configured: true}, host, client, dial, time.Now)
				go w.run()
				defer func() { w.cancel(); <-w.done }()
				synctest.Wait()
				time.Sleep(15 * time.Second)
				synctest.Wait()
				logs := host.logRecords()
				if len(logs) != 1 || !reflect.DeepEqual(logs[0].Fields, map[string]string{code: "1"}) {
					t.Fatalf("failure lost or counted twice: %+v", logs)
				}
			})
		})
	}
}

func TestDiagnosticsCheckpointFailureAndFailingLoggerAreBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := &checkpointHost{
			save: func(context.Context, protocol.CheckpointRequest) error { return errors.New("checkpoint-canary") },
			log:  func(context.Context, protocol.LogNotification) error { return errors.New("logger-canary") },
		}
		w := diagnosticWorker(t, host, nil)
		w.reduce(callback("message", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"body-canary"}`))
		w.mu.Lock()
		if w.saveLocked(w.ctx) == nil {
			t.Fatal("fixture save unexpectedly succeeded")
		}
		// Stop background save attempts: this test isolates diagnostics retry policy.
		w.dirty = false
		w.mu.Unlock()
		time.Sleep(15 * time.Second)
		synctest.Wait()
		logs := host.logRecords()
		if len(logs) != 1 || !reflect.DeepEqual(logs[0].Fields, map[string]string{"checkpoint_failed": "1"}) {
			t.Fatalf("checkpoint failure missing: %+v", logs)
		}
		time.Sleep(45 * time.Second)
		synctest.Wait()
		if len(host.logRecords()) != 1 {
			t.Fatal("failed logging retried without new failures")
		}
		w.markGap("queue_overflow", true)
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if logs = host.logRecords(); len(logs) != 2 || !reflect.DeepEqual(logs[1].Fields, map[string]string{"queue_overflow": "1"}) {
			t.Fatalf("logger failure prevented later bounded reports: %+v", logs)
		}
	})
}

func TestDiagnosticsLoggerDeadlineDoesNotStopWorkerOrRetryBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exited := make(chan error, 1)
		host := &checkpointHost{log: func(ctx context.Context, _ protocol.LogNotification) error {
			<-ctx.Done()
			exited <- ctx.Err()
			return ctx.Err()
		}}
		w := diagnosticWorker(t, host, nil)
		w.markGap("invalid_envelope", false)
		time.Sleep(15 * time.Second)
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case err := <-exited:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("logger did not receive deadline: %v", err)
			}
		default:
			t.Fatal("logger deadline missing")
		}
		if w.ctx.Err() != nil {
			t.Fatal("logger deadline canceled worker")
		}
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if len(host.logRecords()) != 1 {
			t.Fatal("timed out batch retried")
		}
	})
}
