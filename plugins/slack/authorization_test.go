package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func sendAuthorizationEnvelope(t *testing.T, ctx context.Context, peer *websocket.Conn, id string, payload json.RawMessage) {
	t.Helper()
	raw, _ := json.Marshal(struct {
		Type    string          `json:"type"`
		ID      string          `json:"envelope_id"`
		Payload json.RawMessage `json:"payload"`
	}{"events_api", id, payload})
	if err := peer.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, ack, err := peer.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(ack) != fmt.Sprintf(`{"envelope_id":%q}`, id) {
		t.Fatalf("ACK=%s", ack)
	}
}

func TestAuthorizationLifecycleMatchesWorkspaceAndHumanWithoutMessageAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		revoked       bool
	}{
		{"human without authorizations", `{"type":"event_callback","api_app_id":"A123","team_id":"T123","event":{"type":"tokens_revoked","tokens":{"oauth":["U123"]}}}`, true},
		{"human among other revoked users", `{"type":"event_callback","api_app_id":"A123","team_id":"T123","event":{"type":"tokens_revoked","tokens":{"oauth":["U999","U123"]}}}`, true},
		{"app uninstalled", `{"type":"event_callback","api_app_id":"A123","team_id":"T123","event":{"type":"app_uninstalled"}}`, true},
		{"different workspace", `{"type":"event_callback","api_app_id":"A123","team_id":"T999","event":{"type":"tokens_revoked","tokens":{"oauth":["U123"]}}}`, false},
		{"different app", `{"type":"event_callback","api_app_id":"A999","team_id":"T123","event":{"type":"tokens_revoked","tokens":{"oauth":["U123"]}}}`, false},
		{"different human", `{"type":"event_callback","api_app_id":"A123","team_id":"T123","event":{"type":"tokens_revoked","tokens":{"oauth":["U999"]}}}`, false},
		{"bot only", `{"type":"event_callback","api_app_id":"A123","team_id":"T123","event":{"type":"tokens_revoked","tokens":{"bot":["U123"]}}}`, false},
		{"other workspace uninstall", `{"type":"event_callback","api_app_id":"A123","team_id":"T999","event":{"type":"app_uninstalled"}}`, false},
		{"wrong wrapper", `{"type":"different_callback","team_id":"T123","event":{"type":"app_uninstalled"}}`, false},
		{"malformed token list", `{"type":"event_callback","api_app_id":"A123","team_id":"T123","event":{"type":"tokens_revoked","tokens":{"oauth":"U123"}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, peer := socketPair(t)
			_, w, _ := panelFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := w.readConnection(ctx, client); done <- err }()
			defer func() { cancel(); _ = client.CloseNow() }()
			sendAuthorizationEnvelope(t, ctx, peer, "lifecycle", []byte(tc.payload))
			select {
			case err := <-done:
				source, ok := errors.AsType[*sourceError](err)
				if !tc.revoked || !ok || source.code != "auth_required" {
					t.Fatalf("unexpected terminal event: %v", err)
				}
			case <-w.queue:
				if tc.revoked {
					t.Fatal("targeted revocation was queued as an ordinary message")
				}
				cancel()
				<-done
			case <-ctx.Done():
				t.Fatal("lifecycle event neither completed nor queued")
			}
			snap := w.snapshot()
			if tc.revoked && (snap.Phase != "auth_required" || snap.Fresh || !snap.Gap || w.ctx.Err() != nil) {
				t.Fatalf("invalid terminal snapshot: %+v", snap)
			}
			if !tc.revoked && snap.Phase == "auth_required" {
				t.Fatal("unrelated lifecycle event revoked human")
			}
		})
	}
}

func TestAuthorizationRevocationBypassesBlockedCheckpointAndFullQueue(t *testing.T) {
	h, w, host := panelFixture(t)
	client, peer := socketPair(t)
	var tickets atomic.Int32
	w.client = fixtureClient(t, func(out http.ResponseWriter, _ *http.Request) {
		tickets.Add(1)
		_, _ = io.WriteString(out, `{"ok":true,"url":"wss://wss-primary.slack.com/socket?ticket=canary"}`)
	})
	w.dial = func(context.Context, string) (*websocket.Conn, error) { return client, nil }
	entered, release := make(chan struct{}), make(chan struct{})
	host.save = func(ctx context.Context, _ protocol.CheckpointRequest) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	opens := 0
	h.open = func(context.Context, string) error { opens++; return nil }
	startPanel(t, h, w, nil)
	_, _ = press(h, w, protocol.ButtonOK)
	if err := w.publishResident(t.Context()); err != nil {
		t.Fatal(err)
	}
	saved := make(chan struct{})
	go func() { w.mu.Lock(); _ = w.saveLocked(t.Context()); w.mu.Unlock(); close(saved) }()
	<-entered
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-saved
	}()
	transportDone := make(chan struct{})
	go func() { w.runTransport(); close(transportDone) }()
	defer func() { w.cancel(); <-transportDone }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := peer.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","connection_info":{"app_id":"A123"}}`)); err != nil {
		t.Fatal(err)
	}
	waitSnapshot(t, w, func(s workerSnapshot) bool { return s.Phase == "ready" })
	ordinary := callback("queued", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"2.000001","thread_ts":"1.000001","text":"private-canary"}`)
	for i := range 257 {
		sendAuthorizationEnvelope(t, ctx, peer, fmt.Sprintf("message-%d", i), ordinary)
	}
	sendAuthorizationEnvelope(t, ctx, peer, "revoked", []byte(`{"type":"event_callback","api_app_id":"A123","team_id":"T123","event":{"type":"tokens_revoked","tokens":{"oauth":["U123"]}}}`))
	select {
	case <-transportDone:
	case <-ctx.Done():
		t.Fatal("revocation did not stop the source while checkpoint and queue were blocked")
	}
	snap := w.snapshot()
	if snap.Phase != "auth_required" || snap.Fresh || !snap.Gap || snap.Dropped != 1 || len(w.queue) != 256 || w.ctx.Err() != nil || tickets.Load() != 1 {
		t.Fatalf("terminal source boundary: %+v queue=%d tickets=%d", snap, len(w.queue), tickets.Load())
	}
	select {
	case <-saved:
		t.Fatal("checkpoint was no longer blocked at the authorization boundary")
	default:
	}
	if _, _, err := peer.Read(ctx); err == nil {
		t.Fatal("revoked source socket remained open")
	}
	// Simulate a concurrently completed pong and delayed diagnostic. Neither may
	// resurrect the source after the invalidation latch was set.
	w.live()
	w.markGap("disconnected", false)
	w.disconnected("disconnected")
	if w.snapshot().Phase != "auth_required" || w.snapshot().Fresh {
		t.Fatal("later liveness/diagnostic cleared terminal auth")
	}
	if err := w.publishResident(t.Context()); err != nil {
		t.Fatal(err)
	}
	w.publications.mu.Lock()
	for _, p := range w.publications.current {
		if p.observation.Channel == ChannelAttention || p.observation.Channel == ChannelSummary {
			t.Error("fresh publication survived revocation")
		}
	}
	w.publications.mu.Unlock()
	close(release)
	<-saved
	before := w.snapshot().Items[0]
	w.reduce(<-w.queue)
	after := w.snapshot().Items[0]
	if after.Count != before.Count || after.Revision != before.Revision {
		t.Fatal("queued message mutated revoked account state")
	}
	w.mu.Lock()
	handleErr := w.handleLocked(t.Context(), before.ID, before.Revision, before.Fingerprint)
	w.mu.Unlock()
	if !errors.Is(handleErr, errStaleActivity) || w.snapshot().Items[0].Handled {
		t.Fatal("revoked generation admitted local handling")
	}
	if _, err := press(h, w, protocol.ButtonStart); err == nil || opens != 0 {
		t.Fatal("revoked generation admitted native action")
	}
	result, err := h.InvokeOperation(t.Context(), protocol.OperationRequest{Instance: w.instance.Ref(), Operation: "status", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatalf("auth status no longer addressable: %v", err)
	}
	var status struct {
		Phase string `json:"phase"`
	}
	_ = json.Unmarshal(result.Payload, &status)
	if status.Phase != "auth_required" {
		t.Fatalf("status=%s", result.Payload)
	}
}

func TestAuthorizationLatchSurvivesExactGenerationReuseAndRestart(t *testing.T) {
	h := fixtureHandler(t, &checkpointHost{}, nil)
	instance := instanceFixture("slack", "T123", 1)
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{instance}); err != nil {
		t.Fatal(err)
	}
	old, _ := h.lookup(instance.Ref())
	old.disconnected("auth_required")
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{instance}); err != nil {
		t.Fatal(err)
	}
	same, _ := h.lookup(instance.Ref())
	if same != old || same.snapshot().Phase != "auth_required" {
		t.Fatal("same generation reuse cleared auth latch")
	}
	old.cancel()
	<-old.done
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{instance}); err != nil {
		t.Fatal(err)
	}
	restarted, _ := h.lookup(instance.Ref())
	restarted.live()
	if restarted == old || restarted.snapshot().Phase != "auth_required" || restarted.snapshot().Fresh {
		t.Fatal("same generation restart cleared auth latch")
	}
	// Replacing the credential through a new generation is the recovery boundary.
	instance.Generation = 2
	if err := h.ReplaceInstances(t.Context(), []protocol.Instance{instance}); err != nil {
		t.Fatal(err)
	}
	replacement, _ := h.lookup(instance.Ref())
	replacement.live()
	if replacement.snapshot().Phase != "ready" || !replacement.snapshot().Fresh {
		t.Fatal("new generation did not recover")
	}
}
