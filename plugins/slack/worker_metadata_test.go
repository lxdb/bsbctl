package slack

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestWorkerProvesChannelMembershipWithoutBlockingDirectMessages(t *testing.T) {
	lookupStarted := make(chan struct{})
	releaseLookup := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseLookup) })
	defer release()
	var lookups atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/apps.connections.open":
			_, _ = io.WriteString(w, `{"ok":true,"url":"wss://wss-primary.slack.com/socket?ticket=canary"}`)
		case "/api/conversations.info":
			lookups.Add(1)
			if r.Header.Get("Authorization") != "Bearer user-canary" {
				t.Errorf("metadata authorization = %q", r.Header.Get("Authorization"))
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("channel") == "C999" {
				close(lookupStarted)
				<-releaseLookup
				_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"C999","name":"engineering-platform","is_member":true}}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"G999","name":"private-platform","is_member":true}}`)
		default:
			t.Errorf("unexpected Slack API path %q", r.URL.Path)
		}
	})
	cfg, err := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","all_channels":true}`))
	if err != nil {
		t.Fatal(err)
	}
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"app_token": "app-canary", "user_token": "user-canary"}}, cfg, &checkpointHost{}, client, blockedDial, time.Now)
	go w.run()
	t.Cleanup(func() {
		w.cancel()
		<-w.done
	})

	w.queue <- callback("Ev1", `{"type":"message","channel":"C999","channel_type":"channel","user":"U456","ts":"1.000001","text":"first"}`)
	select {
	case <-lookupStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("channel metadata lookup did not start")
	}
	if got := len(w.snapshot().Items); got != 0 {
		t.Fatalf("unverified channel activity became visible: %d items", got)
	}
	w.queue <- callback("Ev1b", `{"type":"message","channel":"C999","channel_type":"channel","user":"U456","ts":"1.000002","text":"second"}`)
	w.queue <- callback("Ev2", `{"type":"message","channel":"D999","channel_type":"im","user":"U456","ts":"2.000001","text":"direct"}`)
	waitSnapshot(t, w, func(s workerSnapshot) bool { return len(s.Items) == 1 && s.Items[0].Kind == "dm" })
	release()
	waitSnapshot(t, w, func(s workerSnapshot) bool {
		if len(s.Items) != 3 {
			return false
		}
		for _, item := range s.Items {
			if item.ChannelID == "C999" {
				return item.Alias == "engineering-platform"
			}
		}
		return false
	})
	if lookups.Load() != 1 {
		t.Fatalf("membership lookups = %d, want one per channel", lookups.Load())
	}
}

func TestWorkerDiscardsActivityFromChannelWhereUserIsNotMember(t *testing.T) {
	lookupDone := make(chan struct{})
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/apps.connections.open" {
			_, _ = io.WriteString(w, `{"ok":true,"url":"wss://wss-primary.slack.com/socket?ticket=canary"}`)
			return
		}
		if r.URL.Path != "/api/conversations.info" {
			t.Fatalf("unexpected Slack API path %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"C999","name":"workspace-wide","is_member":false}}`)
		close(lookupDone)
	})
	cfg, err := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","all_channels":true}`))
	if err != nil {
		t.Fatal(err)
	}
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"app_token": "app-canary", "user_token": "user-canary"}}, cfg, &checkpointHost{}, client, blockedDial, time.Now)
	go w.run()
	t.Cleanup(func() { w.cancel(); <-w.done })

	w.queue <- callback("Ev1", `{"type":"message","channel":"C999","channel_type":"channel","user":"U456","ts":"1.000001","text":"not joined"}`)
	select {
	case <-lookupDone:
	case <-time.After(5 * time.Second):
		t.Fatal("membership lookup did not complete")
	}
	w.queue <- callback("Ev2", `{"type":"message","channel":"D999","channel_type":"im","user":"U456","ts":"2.000001","text":"barrier"}`)
	waitSnapshot(t, w, func(s workerSnapshot) bool { return len(s.Items) == 1 && s.Items[0].Kind == "dm" })
	for _, item := range w.snapshot().Items {
		if item.ChannelID == "C999" {
			t.Fatalf("non-member channel activity became visible: %+v", item)
		}
	}
}

func TestWorkerBoundsMembershipProofsAndPendingEvents(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	cfg := config{configured: true, workspaceID: "T123", userID: "U123", allChannels: true, channels: map[string]string{}}
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, nil, nil, nil, func() time.Time { return now })
	defer w.cancel()
	for index := 0; index < 2*maxRetained; index++ {
		channelID := fmt.Sprintf("C%08d", index)
		w.setMembershipProofLocked(channelID, membershipProof{member: true, name: fmt.Sprintf("channel-%d", index), expires: now.Add(time.Duration(index) * time.Second)})
		eventID := fmt.Sprintf("Ev%d", index)
		w.admitByMembershipLocked(normalizedEvent{callbackID: hashParts(cfg.workspaceID, eventID), channelID: "C99999999"})
	}
	if len(w.membershipProofs) != maxRetained || w.membershipPendingCount != maxPendingMembership || len(w.membershipQueue) != 1 || w.snapshot().Dropped != maxRetained {
		t.Fatalf("membership bounds: proofs=%d pending=%d queued=%d dropped=%d", len(w.membershipProofs), w.membershipPendingCount, len(w.membershipQueue), w.snapshot().Dropped)
	}
}

func TestWorkerDuplicatePendingCallbackDoesNotConsumeAdmissionCapacity(t *testing.T) {
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"BUILD"}]}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, nil, nil, nil, time.Now)
	defer w.cancel()
	duplicate := callback("Repeated", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001"}`)
	for range maxPendingMembership {
		w.reduce(duplicate)
	}
	w.reduce(callback("Distinct", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"2.000001"}`))
	if w.membershipPendingCount != 2 || w.snapshot().Dropped != 0 {
		t.Fatalf("duplicate callbacks exhausted admission capacity: pending=%d dropped=%d", w.membershipPendingCount, w.snapshot().Dropped)
	}
}

func TestWorkerPersistsCoverageUncertaintyBeforeMembershipAdmission(t *testing.T) {
	host := &checkpointHost{}
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"BUILD"}],"rear_details":true}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, host, nil, nil, time.Now)
	defer w.cancel()
	w.reduce(callback("Pending", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","text":"private-body-canary"}`))

	if host.recordCount() != 1 {
		t.Fatalf("unresolved admission checkpoints = %d, want 1", host.recordCount())
	}
	host.mu.Lock()
	raw := append(json.RawMessage(nil), host.records[0].Data...)
	host.mu.Unlock()
	if len(raw) == 0 {
		t.Fatal("unresolved-admission checkpoint was empty")
	}
	for _, secret := range []string{"private-body-canary", "C123", "U456"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("unresolved-admission checkpoint leaked %q: %s", secret, raw)
		}
	}
	restarted := newWorker(protocol.Instance{ID: "slack", Generation: 2, Checkpoint: raw}, cfg, nil, nil, nil, time.Now)
	defer restarted.cancel()
	if snap := restarted.snapshot(); !snap.CoverageIncomplete || !snap.Gap {
		t.Fatalf("restart hid acknowledged unresolved admission: %+v", snap)
	}
}

func TestWorkerClearsDurableUncertaintyAfterMembershipAdmissionCompletes(t *testing.T) {
	host := &checkpointHost{}
	client := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"C123","name":"engineering","is_member":true}}`)
	})
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"BUILD"}]}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"user_token": "user-canary"}}, cfg, host, client, nil, time.Now)
	defer w.cancel()
	w.reduce(callback("Pending", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001"}`))
	w.resolveMembership(<-w.membershipQueue)

	if host.recordCount() != 2 {
		t.Fatalf("membership admission checkpoints = %d, want unresolved and settled records", host.recordCount())
	}
	host.mu.Lock()
	raw := append(json.RawMessage(nil), host.records[1].Data...)
	host.mu.Unlock()
	restarted := newWorker(protocol.Instance{ID: "slack", Generation: 2, Checkpoint: raw}, cfg, nil, nil, nil, time.Now)
	defer restarted.cancel()
	if snap := restarted.snapshot(); snap.CoverageIncomplete || snap.Gap {
		t.Fatalf("settled membership admission remained uncertain after restart: %+v", snap)
	}
}

func TestWorkerMembershipLifecycleAdmitsJoinAndPurgesLeave(t *testing.T) {
	cfg, err := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"BUILD"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, nil, nil, nil, time.Now)
	defer w.cancel()
	w.reduce(callback("Join", `{"type":"member_joined_channel","channel":"C123","user":"U123"}`))
	w.reduce(callback("Message", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","text":"joined"}`))
	if items := w.snapshot().Items; len(items) != 1 || items[0].Alias != "BUILD" {
		t.Fatalf("joined channel was not admitted: %+v", items)
	}
	w.reduce(callback("Leave", `{"type":"member_left_channel","channel":"C123","user":"U123"}`))
	if items := w.snapshot().Items; len(items) != 0 {
		t.Fatalf("left channel remained locally visible: %+v", items)
	}
	w.reduce(callback("AfterLeave", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"2.000001","text":"left"}`))
	if len(w.snapshot().Items) != 0 || w.membershipPendingCount != 0 {
		t.Fatal("negative membership cache admitted or buffered channel activity")
	}
}

func TestWorkerBoundsMembershipLifecycleGenerations(t *testing.T) {
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","all_channels":true}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, nil, nil, nil, time.Now)
	defer w.cancel()
	for index := 0; index < 2*maxRetained; index++ {
		channelID := fmt.Sprintf("C%08d", index)
		w.reduce(callback(fmt.Sprintf("Join%d", index), fmt.Sprintf(`{"type":"member_joined_channel","channel":%q,"user":"U123"}`, channelID)))
	}
	if len(w.membershipEpoch) > maxRetained {
		t.Fatalf("membership lifecycle generations grew without bound: %d", len(w.membershipEpoch))
	}
}

func TestWorkerLeaveSupersedesInFlightMembershipLookup(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	client := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"C123","name":"engineering","is_member":true}}`)
	})
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"BUILD"}]}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"user_token": "user-canary"}}, cfg, nil, client, nil, time.Now)
	defer w.cancel()
	w.reduce(callback("Message", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001"}`))
	request := <-w.membershipQueue
	resolved := make(chan struct{})
	go func() { w.resolveMembership(request); close(resolved) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("membership lookup did not start")
	}
	w.reduce(callback("Leave", `{"type":"member_left_channel","channel":"C123","user":"U123"}`))
	close(release)
	<-resolved
	w.mu.Lock()
	proof := w.membershipProofs["C123"]
	pending := w.membershipPendingCount
	w.mu.Unlock()
	if proof.member || len(w.snapshot().Items) != 0 || pending != 0 || len(w.membershipQueue) != 0 {
		t.Fatal("stale lookup overrode the leave event")
	}
}

func TestWorkerEvictedLifecycleGenerationStillRejectsStaleLookup(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"C00000000","name":"stale","is_member":true}}`)
	})
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","all_channels":true}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"user_token": "user-canary"}}, cfg, nil, client, nil, time.Now)
	defer w.cancel()
	w.reduce(callback("Message", `{"type":"message","channel":"C00000000","channel_type":"channel","user":"U456","ts":"1.000001"}`))
	stale := <-w.membershipQueue
	w.reduce(callback("Leave", `{"type":"member_left_channel","channel":"C00000000","user":"U123"}`))
	for index := 1; index <= 2*maxRetained; index++ {
		channelID := fmt.Sprintf("C%08d", index)
		w.reduce(callback(fmt.Sprintf("Join%d", index), fmt.Sprintf(`{"type":"member_joined_channel","channel":%q,"user":"U123"}`, channelID)))
	}
	if _, retained := w.membershipEpoch["C00000000"]; retained {
		t.Fatal("evicted channel retained its lifecycle generation")
	}
	w.resolveMembership(stale)
	if proof, exists := w.membershipProofs["C00000000"]; exists && proof.member {
		t.Fatal("stale lookup was accepted after its lifecycle generation was evicted")
	}
}

func TestWorkerMembershipProofExpiresAndIsRefreshed(t *testing.T) {
	var nanos atomic.Int64
	nanos.Store(fixtureNow.UnixNano())
	now := func() time.Time { return time.Unix(0, nanos.Load()).UTC() }
	var lookups atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		lookups.Add(1)
		_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"C123","name":"engineering","is_member":true}}`)
	})
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"BUILD"}]}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"user_token": "user-canary"}}, cfg, nil, client, nil, now)
	done := make(chan struct{})
	go func() { w.runMembershipResolver(); close(done) }()
	t.Cleanup(func() { w.cancel(); <-done })

	w.reduce(callback("First", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001"}`))
	waitSnapshot(t, w, func(s workerSnapshot) bool { return len(s.Items) == 1 })
	w.reduce(callback("Cached", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"2.000001"}`))
	if len(w.snapshot().Items) != 2 || lookups.Load() != 1 {
		t.Fatalf("positive proof was not reused: items=%d lookups=%d", len(w.snapshot().Items), lookups.Load())
	}
	nanos.Store(fixtureNow.Add(membershipPositiveTTL).UnixNano())
	w.reduce(callback("Expired", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"3.000001"}`))
	waitSnapshot(t, w, func(s workerSnapshot) bool { return len(s.Items) == 3 })
	if lookups.Load() != 2 {
		t.Fatalf("expired proof lookups = %d, want 2", lookups.Load())
	}
}

func TestWorkerNegativeMembershipProofExpires(t *testing.T) {
	var nanos atomic.Int64
	nanos.Store(fixtureNow.UnixNano())
	now := func() time.Time { return time.Unix(0, nanos.Load()).UTC() }
	lookups := make(chan struct{}, 2)
	client := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		lookups <- struct{}{}
		_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"C123","name":"engineering","is_member":false}}`)
	})
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"BUILD"}]}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"user_token": "user-canary"}}, cfg, nil, client, nil, now)
	done := make(chan struct{})
	go func() { w.runMembershipResolver(); close(done) }()
	t.Cleanup(func() { w.cancel(); <-done })

	w.reduce(callback("First", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001"}`))
	select {
	case <-lookups:
	case <-time.After(5 * time.Second):
		t.Fatal("first membership lookup did not complete")
	}
	waitSnapshot(t, w, func(workerSnapshot) bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		proof, ok := w.membershipProofs["C123"]
		return ok && !proof.member
	})
	w.reduce(callback("Cached", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"2.000001"}`))
	if len(lookups) != 0 || w.membershipPendingCount != 0 || len(w.snapshot().Items) != 0 {
		t.Fatal("negative proof was not reused")
	}
	nanos.Store(fixtureNow.Add(membershipNegativeTTL).UnixNano())
	w.reduce(callback("Expired", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"3.000001"}`))
	select {
	case <-lookups:
	case <-time.After(5 * time.Second):
		t.Fatal("expired negative proof was not refreshed")
	}
}

func TestWorkerMembershipLookupFailureDegradesWithoutBlockingDMs(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":false,"error":"missing_scope"}`)
	})
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"BUILD"}]}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"user_token": "user-canary"}}, cfg, nil, client, nil, time.Now)
	w.live()
	done := make(chan struct{})
	go func() { w.runMembershipResolver(); close(done) }()
	t.Cleanup(func() { w.cancel(); <-done })
	w.reduce(callback("Channel", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001"}`))
	waitSnapshot(t, w, func(s workerSnapshot) bool { return s.Phase == "degraded" && s.ErrorCode == "missing_scope" })
	w.reduce(callback("DM", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"2.000001"}`))
	if items := w.snapshot().Items; len(items) != 1 || items[0].Kind != "dm" {
		t.Fatalf("membership dependency blocked DMs: %+v", items)
	}
	h := newHandler(nil, nil, nil, time.Now)
	h.workers["slack"] = w
	if h.Health(t.Context()).Healthy {
		t.Fatal("membership dependency failure reported healthy")
	}
}

func TestWorkerRecoveredMembershipLookupDoesNotCreateHistoricalCoverageGap(t *testing.T) {
	now := fixtureNow
	var lookups atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if lookups.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"C123","name":"engineering","is_member":true}}`)
	})
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"BUILD"}]}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"user_token": "user-canary"}}, cfg, nil, client, nil, func() time.Time { return now })
	defer w.cancel()
	w.live()
	w.reduce(callback("Channel", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001"}`))
	w.resolveMembership(<-w.membershipQueue)
	if snap := w.snapshot(); snap.Phase != "degraded" || snap.ErrorCode != "request_failed" || snap.CoverageIncomplete {
		t.Fatalf("recoverable lookup failure was misclassified: %+v", snap)
	}

	now = now.Add(2 * membershipRetryDelay)
	w.mu.Lock()
	w.queueMembershipRetriesLocked()
	w.mu.Unlock()
	w.resolveMembership(<-w.membershipQueue)
	if snap := w.snapshot(); snap.Phase != "ready" || snap.ErrorCode != "" || snap.CoverageIncomplete || len(snap.Items) != 1 {
		t.Fatalf("recovered lookup left stale failure state: %+v", snap)
	}
}

func TestWorkerMembershipAuthenticationFailureIsTerminalUntilReplacement(t *testing.T) {
	var now = fixtureNow
	var lookups atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		lookups.Add(1)
		_, _ = io.WriteString(w, `{"ok":false,"error":"token_revoked"}`)
	})
	cfg, _ := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"BUILD"},{"id":"C124","alias":"OPS"}]}`))
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"user_token": "user-canary"}}, cfg, nil, client, nil, func() time.Time { return now })
	defer w.cancel()
	w.live()
	w.reduce(callback("Channel", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001"}`))
	w.reduce(callback("SecondChannel", `{"type":"message","channel":"C124","channel_type":"channel","user":"U456","ts":"1.000002"}`))
	w.resolveMembership(<-w.membershipQueue)
	w.resolveMembership(<-w.membershipQueue)

	w.live()
	snap := w.snapshot()
	if snap.Phase != "auth_required" || snap.Fresh || connectionText(snap) != "Slack access expired - update the saved Slack tokens" {
		t.Fatalf("membership authentication state was not terminal: %+v", snap)
	}
	now = now.Add(2 * membershipRetryDelay)
	w.mu.Lock()
	w.queueMembershipRetriesLocked()
	w.mu.Unlock()
	if lookups.Load() != 1 || len(w.membershipQueue) != 0 {
		t.Fatalf("terminal membership authentication retried: lookups=%d queued=%d", lookups.Load(), len(w.membershipQueue))
	}
}
