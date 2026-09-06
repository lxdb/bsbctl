package githubnotifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

type recordingHost struct {
	mu             sync.Mutex
	observations   []protocol.Observation
	checkpoints    []protocol.CheckpointRequest
	checkpointErr  error
	publicationErr error
	notify         chan protocol.Observation
}

type blockingCheckpointHost struct {
	recordingHost
	started chan struct{}
	once    sync.Once
}

func (h *blockingCheckpointHost) SaveCheckpoint(ctx context.Context, _ protocol.CheckpointRequest) error {
	h.once.Do(func() { close(h.started) })
	<-ctx.Done()
	return ctx.Err()
}

func (h *recordingHost) PublishObservation(_ context.Context, o protocol.Observation) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.publicationErr != nil {
		return h.publicationErr
	}
	h.observations = append(h.observations, o)
	if h.notify != nil {
		h.notify <- o
	}
	return nil
}
func (h *recordingHost) SaveCheckpoint(_ context.Context, r protocol.CheckpointRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.checkpointErr != nil {
		return h.checkpointErr
	}
	h.checkpoints = append(h.checkpoints, r)
	return nil
}
func (h *recordingHost) BeginSessionExecution(context.Context, protocol.SessionExecutionRequest) error {
	return nil
}
func (h *recordingHost) CompleteSession(context.Context, protocol.CompleteSessionRequest) error {
	return nil
}
func (h *recordingHost) Log(context.Context, protocol.LogNotification) error { return nil }
func configuredInstance(id string, generation uint64) protocol.Instance {
	return protocol.Instance{ID: id, Generation: generation, Config: json.RawMessage(`{"repositories":[{"name":"acme/service","alias":"SVC"}]}`), Secrets: map[string]string{"token": "private-token"}}
}
func configuredAllRepositoriesInstance(id string, generation uint64) protocol.Instance {
	return protocol.Instance{ID: id, Generation: generation, Config: json.RawMessage(`{"repositories":[]}`), Secrets: map[string]string{"token": "private-token"}}
}
func authorizeResponse(r *http.Request) (*http.Response, bool) {
	switch r.URL.Path {
	case "/user":
		return response(200, `{"id":42,"login":"test-user"}`, nil), true
	case "/repos/acme/service":
		return response(200, `{"id":7,"full_name":"acme/service"}`, nil), true
	case "/notifications":
		if r.URL.Query().Get("per_page") == "1" {
			return response(200, `[]`, nil), true
		}
	}
	return nil, false
}
func TestUnconfiguredIdleQueriesAndNoIO(t *testing.T) {
	var calls atomic.Int32
	h := newHandler(&recordingHost{}, testClient(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("forbidden") }), time.Now)
	instance := protocol.Instance{ID: "gh", Generation: 1, Config: json.RawMessage(`{}`)}
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{instance}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	out, err := h.InvokeOperation(t.Context(), protocol.OperationRequest{Instance: instance.Ref(), Operation: OperationStatus, Payload: json.RawMessage(`{}`)})
	if err != nil || !strings.Contains(string(out.Payload), `"phase":"unconfigured"`) || !h.Health(t.Context()).Healthy || calls.Load() != 0 {
		t.Fatalf("idle state %s %v calls=%d", out.Payload, err, calls.Load())
	}
}

func TestBackgroundCheckpointLeavesInputAndQueryHeadroom(t *testing.T) {
	host := &blockingCheckpointHost{started: make(chan struct{})}
	cfg := testConfig()
	w := &worker{
		ref: protocol.InstanceRef{ID: "gh", Generation: 1}, config: cfg, token: "private-token",
		source: newProvider(testClient(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}), "private-token"),
		state: newState(cfg, Identity{ID: 42, Login: "test-user"}), host: host, now: time.Now,
		done: make(chan struct{}), published: map[string]protocol.Observation{},
	}
	w.session = &interactionSession{token: "session-1", observedAt: time.Now().UTC()}
	h := newHandler(host, nil, time.Now)
	h.workers[w.ref.ID] = w
	workerCtx, cancel := context.WithCancel(t.Context())
	w.cancel = cancel
	go w.run(workerCtx)
	t.Cleanup(func() {
		cancel()
		<-w.done
	})
	<-host.started
	queryDone := make(chan error, 1)
	go func() {
		_, err := h.InvokeOperation(t.Context(), protocol.OperationRequest{Instance: w.ref, Operation: OperationStatus, Payload: json.RawMessage(`{}`)})
		queryDone <- err
	}()
	inputDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err := h.HandleSessionInput(ctx, protocol.SessionInputRequest{
			Instance: w.ref, SessionToken: "session-1", Sequence: 1, OccurredAt: time.Now().UTC(),
			Input: protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: 1}},
		})
		inputDone <- err
	}()
	deadline := time.After(1500 * time.Millisecond)
	for name, result := range map[string]<-chan error{"query": queryDone, "input": inputDone} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case <-deadline:
			t.Fatalf("%s consumed the input callback headroom", name)
		}
	}
}
func TestReplacementRetainsExactGenerationAndJoinsCancelledRequest(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var polls atomic.Int32
	client := testClient(func(r *http.Request) (*http.Response, error) {
		if res, ok := authorizeResponse(r); ok {
			return res, nil
		}
		polls.Add(1)
		close(started)
		<-r.Context().Done()
		close(cancelled)
		return nil, r.Context().Err()
	})
	host := &recordingHost{}
	h := newHandler(host, client, time.Now)
	first := configuredInstance("gh", 1)
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{first}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	<-started
	old := h.findWorker(first.Ref())
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{first}); err != nil {
		t.Fatal(err)
	}
	if h.findWorker(first.Ref()) != old {
		t.Fatal("exact generation replaced")
	}
	replacement := protocol.Instance{ID: "gh", Generation: 2, Config: json.RawMessage(`{}`)}
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{replacement}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("old request not cancelled")
	}
	select {
	case <-old.done:
	default:
		t.Fatal("retired worker not joined")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.observations) != 0 || polls.Load() != 1 {
		t.Fatalf("retired source published or duplicated: %d observations %d polls", len(host.observations), polls.Load())
	}
}
func TestReplacementValidatesEntireSetBeforeRetiring(t *testing.T) {
	h := newHandler(&recordingHost{}, testClient(func(r *http.Request) (*http.Response, error) {
		if res, ok := authorizeResponse(r); ok {
			return res, nil
		}
		return response(200, `[]`, nil), nil
	}), time.Now)
	empty := protocol.Instance{ID: "idle", Generation: 1, Config: json.RawMessage(`{}`)}
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{empty}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	old := h.findWorker(empty.Ref())
	a, b := configuredInstance("a", 1), configuredInstance("b", 1)
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{a, b}); err == nil {
		t.Fatal("duplicate authenticated scope accepted")
	}
	if h.findWorker(empty.Ref()) != old {
		t.Fatal("failed replacement changed old set")
	}
	select {
	case <-old.done:
		t.Fatal("failed replacement retired worker")
	default:
	}
}

func TestReplacementRejectsOverlappingAllRepositoryScopes(t *testing.T) {
	tests := []struct {
		name      string
		instances []protocol.Instance
	}{
		{"all with all", []protocol.Instance{configuredAllRepositoriesInstance("all-a", 1), configuredAllRepositoriesInstance("all-b", 1)}},
		{"all before selected", []protocol.Instance{configuredAllRepositoriesInstance("all", 1), configuredInstance("selected", 1)}},
		{"selected before all", []protocol.Instance{configuredInstance("selected", 1), configuredAllRepositoriesInstance("all", 1)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHandler(&recordingHost{}, testClient(func(r *http.Request) (*http.Response, error) {
				if res, ok := authorizeResponse(r); ok {
					return res, nil
				}
				return response(200, `[]`, nil), nil
			}), time.Now)
			if err := h.ReplaceInstances(t.Context(), test.instances); err == nil {
				t.Fatal("overlapping authenticated scope accepted")
			}
		})
	}
}
func TestPublicationRenewalPreservesEpisodeAndStaleWithdraws(t *testing.T) {
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	s := newState(testConfig(), Identity{ID: 42})
	s.apply(fetchResult{Complete: true}, nil, now)
	s.apply(fetchResult{Complete: true, Items: []notification{{ThreadID: "1", Repository: "acme/service", Alias: "SVC", RepositoryID: 7, Reason: "mention", Unread: true, UpdatedAt: now}}}, nil, now)
	host := &recordingHost{}
	w := &worker{ref: protocol.InstanceRef{ID: "gh", Generation: 1}, config: s.config, state: s, host: host, now: func() time.Time { return now }, published: map[string]protocol.Observation{}}
	if err := w.publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := w.published[ChannelAttention+"/"+s.ordered()[0].ID]
	now = now.Add(15 * time.Second)
	if err := w.publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	renewed := w.published[ChannelAttention+"/"+s.ordered()[0].ID]
	if renewed.Revision <= first.Revision || !renewed.ObservedAt.Equal(first.ObservedAt) || renewed.ValidUntil.After(now.Add(45*time.Second)) {
		t.Fatal("renewal broke acknowledgement identity or TTL")
	}
	now = now.Add(106 * time.Second)
	if err := w.publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(w.published) != 1 || w.published[ChannelConnection+"/connection"].Channel != "connection" || len(s.items) != 1 {
		t.Fatal("stale publication retained ambient source state or lost domain record")
	}
	for _, o := range host.observations {
		if err := o.Validate(o.UpdatedAt); err != nil {
			t.Fatalf("invalid production observation: %v", err)
		}
	}
}
func TestCheckpointFailureIsUnhealthyAndRetryable(t *testing.T) {
	now := time.Now()
	s := newState(testConfig(), Identity{ID: 42})
	s.apply(fetchResult{Complete: true}, nil, now)
	s.checkpointDirty = true
	host := &recordingHost{checkpointErr: errors.New("disk unavailable")}
	w := &worker{ref: protocol.InstanceRef{ID: "gh", Generation: 1}, config: s.config, state: s, host: host, now: func() time.Time { return now }, published: map[string]protocol.Observation{}}
	h := newHandler(host, nil, func() time.Time { return now })
	h.workers["gh"] = w
	w.renew(t.Context())
	if h.Health(t.Context()).Healthy || !s.checkpointDirty {
		t.Fatal("failed checkpoint claimed healthy durable state")
	}
	host.checkpointErr = nil
	w.renew(t.Context())
	if !h.Health(t.Context()).Healthy || s.checkpointDirty || len(host.checkpoints) != 1 {
		t.Fatal("checkpoint did not recover")
	}
}

func TestWorkerPreservesServerMinimumAcrossTransientFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := make(chan time.Time, 3)
		attempt := 0
		client := testClient(func(r *http.Request) (*http.Response, error) {
			if res, ok := authorizeResponse(r); ok {
				return res, nil
			}
			attempt++
			calls <- time.Now()
			if attempt == 1 {
				return response(200, `[]`, http.Header{"X-Poll-Interval": {"120"}}), nil
			}
			return response(503, `{}`, nil), nil
		})
		h := newHandler(&recordingHost{}, client, time.Now)
		if err := h.ReplaceInstances(t.Context(), []protocol.Instance{configuredInstance("gh", 1)}); err != nil {
			t.Fatal(err)
		}
		first := <-calls
		second := <-calls
		third := <-calls
		if err := h.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
		if second.Sub(first) != 120*time.Second || third.Sub(second) < 120*time.Second {
			t.Fatalf("server minimum lost: intervals %s %s", second.Sub(first), third.Sub(second))
		}
	})
}

func TestSyntheticConfiguredLifecycleSoakBoundsRepeatedCollectionAndJoinsReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := make(chan int, 4)
		pollCount := 0
		client := testClient(func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodGet {
				t.Fatalf("provider method = %s, want GET", request.Method)
			}
			if result, ok := authorizeResponse(request); ok {
				return result, nil
			}
			pollCount++
			rows := make([]string, 129)
			for index := range rows {
				reason := "comment"
				updated := time.Date(2026, time.September, 5, 11, 0, 0, index, time.UTC)
				if index == 0 && pollCount > 1 {
					reason = "review_requested"
					updated = updated.Add(time.Minute)
				}
				rows[index] = threadJSON(fmt.Sprintf("synthetic-%03d", index), reason, updated.Format(time.RFC3339Nano))
			}
			polls <- pollCount
			return response(http.StatusOK, "["+strings.Join(rows, ",")+"]", nil), nil
		})

		handler := newHandler(&recordingHost{}, client, time.Now)
		first := configuredInstance("github-notifications", 1)
		if err := handler.ReplaceInstances(t.Context(), []protocol.Instance{first}); err != nil {
			t.Fatal(err)
		}
		old := handler.findWorker(first.Ref())
		if <-polls != 1 || <-polls != 2 || <-polls != 3 {
			t.Fatal("configured lifecycle did not perform repeated collection")
		}
		old.mu.Lock()
		if len(old.state.items) != 128 || !old.state.truncated || len(old.state.attention(time.Now())) != 1 {
			t.Fatalf("bounded repeated state = items %d truncated %t attention %d", len(old.state.items), old.state.truncated, len(old.state.attention(time.Now())))
		}
		old.mu.Unlock()

		replacement := configuredInstance("github-notifications", 2)
		if err := handler.ReplaceInstances(t.Context(), []protocol.Instance{replacement}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-old.done:
		default:
			t.Fatal("configured replacement did not join the previous worker")
		}
		current := handler.findWorker(replacement.Ref())
		if current == nil || current == old {
			t.Fatal("configured replacement did not start the new generation")
		}
		if err := handler.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-current.done:
		default:
			t.Fatal("configured shutdown did not join the current worker")
		}
	})
}

func TestUnknownReadCheckpointFailureRetriesOnlyPersistence(t *testing.T) {
	h, w, host, opened := interactionFixture(t)
	writes := 0
	w.source = newProvider(testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method == "PATCH" {
			writes++
			return response(503, "", nil), nil
		}
		return response(200, `{"html_url":"https://github.com/acme/service/issues/17"}`, nil), nil
	}), "fake")
	host.checkpointErr = errors.New("disk unavailable")
	startPanel(t, h, w, true)
	_, _ = press(h, w, 1, protocol.ButtonStart)
	if !w.checkpointError || len(w.state.attention(w.now())) != 0 {
		t.Fatal("uncertain write did not remain suppressed after failed save")
	}
	host.checkpointErr = nil
	w.renew(t.Context())
	if w.checkpointError || len(host.checkpoints) != 1 || writes != 1 || len(*opened) != 1 {
		t.Fatal("checkpoint retry repeated an external effect")
	}
	restored := newState(w.config, Identity{ID: 42})
	if code := restored.restore(host.checkpoints[0].Data, w.now()); code != "" {
		t.Fatal(code)
	}
	if len(restored.handled) != 1 {
		t.Fatal("uncertain marker not persisted")
	}
}

func TestUnchangedPollDoesNotRerenderInsideFifteenSeconds(t *testing.T) {
	now := time.Now()
	s := newState(testConfig(), Identity{ID: 42})
	s.apply(fetchResult{Complete: true}, nil, now)
	host := &recordingHost{}
	w := &worker{ref: protocol.InstanceRef{ID: "gh", Generation: 1}, config: s.config, state: s, host: host, now: func() time.Time { return now }, published: map[string]protocol.Observation{}}
	if err := w.publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	s.apply(fetchResult{Complete: true, NotModified: true}, nil, now)
	if err := w.publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(host.observations) != 0 {
		t.Fatal("quiet GitHub published ambient summary")
	}
}

func TestCancelledReplacementCommitsRetirementAndPreviousGenerationCanRestart(t *testing.T) {
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var polls atomic.Int32
	client := testClient(func(r *http.Request) (*http.Response, error) {
		if res, ok := authorizeResponse(r); ok {
			return res, nil
		}
		if polls.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			close(cancelled)
			<-release
			return nil, r.Context().Err()
		}
		return response(200, `[]`, nil), nil
	})
	h := newHandler(&recordingHost{}, client, time.Now)
	first := configuredInstance("gh", 1)
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{first}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	<-started
	replacement := protocol.Instance{ID: "gh", Generation: 2, Config: json.RawMessage(`{}`)}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- h.ReplaceInstances(ctx, []protocol.Instance{replacement}) }()
	<-cancelled
	cancel()
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("irreversible retirement reported rollback: %v", err)
	}
	if h.findWorker(replacement.Ref()) == nil {
		t.Fatal("replacement target was not committed")
	}
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{first}); err != nil {
		t.Fatal(err)
	}
	out, err := h.InvokeOperation(t.Context(), protocol.OperationRequest{Instance: first.Ref(), Operation: OperationStatus, Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("previous generation did not restart: %v (%s)", err, out.Payload)
	}
}
func TestAlreadyCancelledReplacementLeavesPreviousWorkerRunning(t *testing.T) {
	h := newHandler(&recordingHost{}, nil, time.Now)
	first := protocol.Instance{ID: "gh", Generation: 1, Config: json.RawMessage(`{}`)}
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{first}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	old := h.findWorker(first.Ref())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.ReplaceInstances(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("precommit cancellation = %v", err)
	}
	if h.findWorker(first.Ref()) != old {
		t.Fatal("cancelled replacement changed accepted set")
	}
	select {
	case <-old.done:
		t.Fatal("precommit cancellation stopped old worker")
	default:
	}
}
func TestInterruptedShutdownDoesNotRetainStoppedGeneration(t *testing.T) {
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var polls atomic.Int32
	client := testClient(func(r *http.Request) (*http.Response, error) {
		if res, ok := authorizeResponse(r); ok {
			return res, nil
		}
		if polls.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			close(cancelled)
			<-release
			return nil, r.Context().Err()
		}
		return response(200, `[]`, nil), nil
	})
	h := newHandler(&recordingHost{}, client, time.Now)
	first := configuredInstance("gh", 1)
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{first}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	<-started
	old := h.findWorker(first.Ref())
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- h.Shutdown(ctx) }()
	<-cancelled
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown cancellation = %v", err)
	}
	close(release)
	<-old.done
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{first}); err != nil {
		t.Fatal(err)
	}
	if h.findWorker(first.Ref()) == old {
		t.Fatal("retained stopped generation after interrupted shutdown")
	}
	if _, err := h.InvokeOperation(t.Context(), protocol.OperationRequest{Instance: first.Ref(), Operation: OperationStatus, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("restarted query: %v", err)
	}
}

func TestInterruptedShutdownRestartPreservesPublicationRevisions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		blocked, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		attempts := 0
		host := &recordingHost{notify: make(chan protocol.Observation, 64)}
		client := testClient(func(r *http.Request) (*http.Response, error) {
			if res, ok := authorizeResponse(r); ok {
				return res, nil
			}
			attempts++
			if attempts == 2 {
				close(blocked)
				<-r.Context().Done()
				close(cancelled)
				<-release
				return nil, r.Context().Err()
			}
			return response(200, `[]`, nil), nil
		})
		h := newHandler(host, client, time.Now)
		first := configuredInstance("gh", 1)
		if err := h.ReplaceInstances(t.Context(), []protocol.Instance{first}); err != nil {
			t.Fatal(err)
		}
		oldWorker := h.findWorker(first.Ref())
		oldWorker.mu.Lock()
		oldWorker.actionError = "read_unknown"
		if err := oldWorker.refreshPanel(t.Context()); err != nil {
			t.Fatal(err)
		}
		oldWorker.mu.Unlock()
		<-blocked
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- h.Shutdown(ctx) }()
		<-cancelled
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		close(release)
		<-h.findWorker(first.Ref()).done
		var previous uint64
		for len(host.notify) > 0 {
			o := <-host.notify
			previous = max(previous, o.Revision)
		}
		if err := h.ReplaceInstances(t.Context(), []protocol.Instance{first}); err != nil {
			t.Fatal(err)
		}
		w := h.findWorker(first.Ref())
		w.mu.Lock()
		w.actionError = "read_unknown"
		if err := w.refreshPanel(t.Context()); err != nil {
			t.Fatal(err)
		}
		w.mu.Unlock()
		next := <-host.notify
		if err := h.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
		if previous == 0 || next.Revision <= previous {
			t.Fatalf("same-generation revision reset: prior=%d next=%d", previous, next.Revision)
		}
	})
}
