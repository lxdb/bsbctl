package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type checkpointHost struct {
	mu      sync.Mutex
	records []protocol.CheckpointRequest
	save    func(context.Context, protocol.CheckpointRequest) error
	logs    []protocol.LogNotification
	log     func(context.Context, protocol.LogNotification) error
}

func (h *checkpointHost) Log(ctx context.Context, value protocol.LogNotification) error {
	h.mu.Lock()
	h.logs = append(h.logs, value)
	h.mu.Unlock()
	if h.log != nil {
		return h.log(ctx, value)
	}
	return nil
}
func (h *checkpointHost) logRecords() []protocol.LogNotification {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]protocol.LogNotification(nil), h.logs...)
}

func (h *checkpointHost) SaveCheckpoint(ctx context.Context, r protocol.CheckpointRequest) error {
	if h.save != nil {
		if err := h.save(ctx, r); err != nil {
			return err
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *checkpointHost) recordCount() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.records) }
func instanceFixture(id, team string, generation uint64) protocol.Instance {
	return protocol.Instance{ID: id, Generation: generation, Config: json.RawMessage(fmt.Sprintf(`{"app_id":"A123","workspace_id":%q,"user_id":"U123"}`, team)), Secrets: map[string]string{"app_token": "app-canary"}}
}
func blockedDial(ctx context.Context, _ string) (*websocket.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func fixtureHandler(t *testing.T, host Host, request func(http.ResponseWriter, *http.Request)) *Handler {
	t.Helper()
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if request != nil {
			request(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"url":"wss://wss-primary.slack.com/socket?ticket=canary"}`)
	})
	h := newHandler(host, client, blockedDial, time.Now)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return h
}
func TestHandlerReplacementPreservesLiveWorkersUntilAllConfigurationValidates(t *testing.T) {
	h := fixtureHandler(t, &checkpointHost{}, nil)
	original := instanceFixture("slack", "T123", 1)
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{original}); err != nil {
		t.Fatal(err)
	}
	old, _ := h.lookup(original.Ref())
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{original}); err != nil {
		t.Fatal(err)
	}
	same, _ := h.lookup(original.Ref())
	if same != old {
		t.Fatal("unchanged generation restarted")
	}
	badSecrets := instanceFixture("slack", "T123", 2)
	badSecrets.Secrets["unexpected"] = "bad"
	for _, replacement := range [][]protocol.Instance{
		{badSecrets},
		{original, instanceFixture("duplicate", "T123", 1)},
		{original, {ID: "invalid", Generation: 1, Config: []byte(`{"label":"partial"}`)}},
	} {
		if err := h.ReplaceInstances(t.Context(), replacement); err == nil {
			t.Fatal("invalid replacement accepted")
		}
		current, _ := h.lookup(original.Ref())
		if current != old || old.ctx.Err() != nil {
			t.Fatal("failed validation retired old generation")
		}
	}
	newer := instanceFixture("slack", "T123", 2)
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{newer}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.done:
	default:
		t.Fatal("replacement returned before old worker joined")
	}
	if _, err := h.lookup(original.Ref()); err == nil {
		t.Fatal("retired generation admitted")
	}
}

func TestHandlerAcceptsEightDistinctWorkspacesAndCapsInstances(t *testing.T) {
	h := fixtureHandler(t, &checkpointHost{}, nil)
	instances := make([]protocol.Instance, 8)
	for i := range instances {
		team := fmt.Sprintf("T%d", i+100)
		instances[i] = instanceFixture(fmt.Sprintf("slack%d", i), team, 1)
	}
	if err := h.ReplaceInstances(t.Context(), instances); err != nil {
		t.Fatal(err)
	}
	if err := h.ReplaceInstances(t.Context(), append(instances, instanceFixture("ninth", "T999", 1))); err == nil {
		t.Fatal("ninth instance accepted")
	}
}

func TestHandlerCanceledExactGenerationRestartsAndUnconfiguredIsIdle(t *testing.T) {
	h := fixtureHandler(t, &checkpointHost{}, nil)
	idle := protocol.Instance{ID: "slack", Generation: 1, Config: []byte(`{}`)}
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{idle}); err != nil {
		t.Fatal(err)
	}
	if !h.Health(t.Context()).Healthy {
		t.Fatal("unconfigured instance was unhealthy")
	}
	status, err := h.InvokeOperation(t.Context(), protocol.OperationRequest{Instance: idle.Ref(), Operation: "status", Payload: []byte(`{}`)})
	if err != nil || string(status.Payload) != `{"phase":"unconfigured","last_error_code":"","pending_count":0,"truncated":false}` {
		t.Fatalf("idle status contract: %s %v", status.Payload, err)
	}
	configured := instanceFixture("slack", "T123", 2)
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{configured}); err != nil {
		t.Fatal(err)
	}
	old, _ := h.lookup(configured.Ref())
	original := seedEvictedReplay(t, old)
	old.cancel()
	<-old.done
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{configured}); err != nil {
		t.Fatal(err)
	}
	current, _ := h.lookup(configured.Ref())
	if current == old || current.ctx.Err() != nil || len(current.snapshot().Items) != 128 || !current.snapshot().Gap {
		t.Fatal("canceled generation not resumed with retained state")
	}
	current.reduce(original)
	assertReplaySurvivors(t, current)
	if err := h.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-current.done:
	default:
		t.Fatal("shutdown returned before join")
	}
}

func TestWorkerFreshnessCannotBeExtendedByCachedReadsOrFailedReconnects(t *testing.T) {
	now := fixtureNow
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123"}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, nil, nil, nil, func() time.Time { return now })
	defer w.cancel()
	w.live()
	w.disconnected("disconnected")
	if !w.snapshot().Gap {
		t.Fatal("disconnect did not record coverage gap")
	}
	grace := 30 * time.Second
	if got := w.snapshot().FreshUntil; !got.Equal(fixtureNow.Add(grace)) {
		t.Fatalf("deadline=%v", got)
	}
	now = fixtureNow.Add(grace - time.Nanosecond)
	if !w.snapshot().Fresh {
		t.Fatal("grace ended early")
	}
	w.disconnected("throttled")
	now = now.Add(time.Nanosecond)
	if w.snapshot().Fresh {
		t.Fatal("failed reconnect extended freshness")
	}
	w.live()
	if !w.snapshot().Fresh || !w.snapshot().Gap || w.snapshot().Phase != "degraded" {
		t.Fatal("reconnect erased gap or failed to renew liveness")
	}
	w.disconnected("auth_required")
	if w.snapshot().Fresh || w.snapshot().Phase != "auth_required" {
		t.Fatal("auth failure retained freshness")
	}
}

func TestWorkerFailedHandleDoesNotCommitAndQueriesStayAvailableDuringSave(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	fail := true
	host := &checkpointHost{save: func(ctx context.Context, r protocol.CheckpointRequest) error {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		if fail {
			return errors.New("checkpoint-canary")
		}
		return nil
	}}
	now := fixtureNow
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","rear_details":true}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, host, nil, nil, func() time.Time { return now })
	defer w.cancel()
	w.live()
	w.reduce(callback("Ev1", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"private-body-canary"}`))
	selected := w.snapshot().Items[0]
	done := make(chan error, 1)
	go func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		done <- w.handleLocked(t.Context(), selected.ID, selected.Revision, selected.Fingerprint)
	}()
	<-entered
	h := newHandler(host, nil, nil, func() time.Time { return now })
	h.workers["slack"] = w
	for _, op := range []string{"status", "items"} {
		result, err := h.InvokeOperation(t.Context(), protocol.OperationRequest{Instance: w.instance.Ref(), Operation: op, Payload: []byte(`{}`)})
		if err != nil || strings.Contains(string(result.Payload), "canary") || strings.Contains(string(result.Payload), "D123") || strings.Contains(string(result.Payload), "U123") {
			t.Fatalf("query blocked/leaked: %s %v", result.Payload, err)
		}
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("failed durable save reported success")
	}
	if w.snapshot().Items[0].Handled || w.snapshot().Items[0].Revision != selected.Revision || h.Health(t.Context()).Healthy {
		t.Fatal("failed intent committed or health remained healthy")
	}
	fail = false
	w.mu.Lock()
	err := w.handleLocked(t.Context(), selected.ID, selected.Revision, selected.Fingerprint)
	w.mu.Unlock()
	if err != nil || !w.snapshot().Items[0].Handled || !h.Health(t.Context()).Healthy {
		t.Fatal("durable retry failed to recover")
	}
	if len(host.records) != 1 {
		t.Fatal("failed checkpoint recorded as durable")
	}
}

func TestWorkerAcceptsDeliveredAuthorizedEventAfterCorruptCheckpoint(t *testing.T) {
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"PRIVATE"}]}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Checkpoint: []byte(`{"schema_version":99}`)}, cfg, nil, nil, nil, time.Now)
	defer w.cancel()
	if w.snapshot().ErrorCode != "checkpoint_invalid" || !w.snapshot().Gap {
		t.Fatal("restore mismatch silent")
	}
	w.reduce(callback("Ev1", `{"type":"message","channel":"C123","channel_type":"group","user":"U456","ts":"1.000001","text":"<@U123> private-canary"}`))
	if len(w.snapshot().Items) != 1 || !w.snapshot().Gap || w.snapshot().ErrorCode != "checkpoint_invalid" {
		t.Fatal("authorized event was rejected without a Web API scope preflight")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportPreservesDialAuthenticationAndThrottleClassification(t *testing.T) {
	for _, code := range []string{"auth_required", "throttled"} {
		t.Run(code, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := newSlackClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"url":"wss://wss-primary.slack.com/socket?ticket=canary"}`))}, nil
				})})
				var calls atomic.Int32
				dial := func(context.Context, string) (*websocket.Conn, error) {
					calls.Add(1)
					return nil, &sourceError{code: code, retryAfter: 601 * time.Second}
				}
				w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"app_token": "canary"}}, config{configured: true}, nil, client, dial, time.Now)
				done := make(chan struct{})
				go func() { w.runTransport(); close(done) }()
				defer func() { w.cancel(); <-done }()
				synctest.Wait()
				if w.snapshot().ErrorCode != code || !w.snapshot().Gap {
					t.Fatalf("dial error classification lost: %+v", w.snapshot())
				}
				time.Sleep(600 * time.Second)
				synctest.Wait()
				if calls.Load() != 1 {
					t.Fatalf("retried before provider cooldown/auth replacement: %d", calls.Load())
				}
				if code == "throttled" {
					time.Sleep(time.Second)
					synctest.Wait()
					if calls.Load() != 2 {
						t.Fatal("did not reconnect after cooldown")
					}
				}
			})
		})
	}
}

func TestWorkerProviderThrottleEnvelopeCreatesPersistentGap(t *testing.T) {
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123"}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, nil, nil, nil, time.Now)
	defer w.cancel()
	w.live()
	w.reduce([]byte(`{"type":"app_rate_limited","team_id":"T123","minute_rate_limited":123}`))
	if w.snapshot().ErrorCode != "throttled" || !w.snapshot().Gap {
		t.Fatal("provider event throttle ignored")
	}
	w.live()
	if !w.snapshot().Gap {
		t.Fatal("liveness erased throttle gap")
	}
}

func TestWorkerCoalescesCheckpointFailureAndRetriesWithinOneSecond(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		host := &checkpointHost{save: func(context.Context, protocol.CheckpointRequest) error {
			if calls.Add(1) == 1 {
				return errors.New("canary")
			}
			return nil
		}}
		client := newSlackClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })})
		cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123"}`))
		w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, host, client, blockedDial, time.Now)
		go w.run()
		defer func() { w.cancel(); <-w.done }()
		synctest.Wait()
		for i := range 3 {
			w.reduce(callback(fmt.Sprint(i), fmt.Sprintf(`{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"%d.000001","text":"canary"}`, i+1)))
		}
		if calls.Load() != 0 {
			t.Fatal("checkpoint was not coalesced")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if calls.Load() != 1 || w.snapshot().ErrorCode != "checkpoint_failed" || host.recordCount() != 0 {
			t.Fatal("failed checkpoint claimed durability/health")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if calls.Load() != 2 || w.snapshot().ErrorCode == "checkpoint_failed" || host.recordCount() != 1 {
			t.Fatal("checkpoint failure did not recover")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("clean checkpoint repeatedly saved")
		}
	})
}
