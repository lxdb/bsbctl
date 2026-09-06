package slack

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

// Task 2's checkpoint-only fixture never admits desktop effects.
func (*checkpointHost) PublishObservation(context.Context, protocol.Observation) error { return nil }
func (*checkpointHost) WithdrawObservation(context.Context, protocol.WithdrawRequest) error {
	return nil
}
func (*checkpointHost) BeginSessionExecution(context.Context, protocol.SessionExecutionRequest) error {
	return errors.New("not admitted")
}
func (*checkpointHost) CompleteSession(context.Context, protocol.CompleteSessionRequest) error {
	return nil
}

type panelHost struct {
	checkpointHost
	pubMu        sync.Mutex
	observations []protocol.Observation
	withdrawals  []protocol.WithdrawRequest
	grant        func(context.Context) error
	completes    int
	failPublish  bool
	publish      func(context.Context, protocol.Observation) error
	withdraw     func(context.Context, protocol.WithdrawRequest) error
	complete     func() error
}

func (h *panelHost) PublishObservation(ctx context.Context, o protocol.Observation) error {
	if h.publish != nil {
		if err := h.publish(ctx, o); err != nil {
			return err
		}
	}
	h.pubMu.Lock()
	defer h.pubMu.Unlock()
	if h.failPublish {
		return errors.New("private-provider-error")
	}
	h.observations = append(h.observations, o)
	return nil
}
func (h *panelHost) WithdrawObservation(ctx context.Context, r protocol.WithdrawRequest) error {
	if h.withdraw != nil {
		if err := h.withdraw(ctx, r); err != nil {
			return err
		}
	}
	h.pubMu.Lock()
	defer h.pubMu.Unlock()
	h.withdrawals = append(h.withdrawals, r)
	return nil
}
func (h *panelHost) BeginSessionExecution(ctx context.Context, _ protocol.SessionExecutionRequest) error {
	if h.grant != nil {
		return h.grant(ctx)
	}
	return nil
}
func (h *panelHost) CompleteSession(context.Context, protocol.CompleteSessionRequest) error {
	h.completes++
	if h.complete != nil {
		return h.complete()
	}
	return nil
}
func panelFixture(t *testing.T) (*Handler, *worker, *panelHost) {
	t.Helper()
	host := new(panelHost)
	cfg := fixtureState(t, "").config
	h := newHandler(host, nil, nil, func() time.Time { return fixtureNow })
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, host, nil, nil, h.now)
	h.workers["slack"] = w
	t.Cleanup(w.cancel)
	w.live()
	w.reduce(callback("Ev1", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"private-canary https://evil.invalid"}`))
	return h, w, host
}
func startPanel(t *testing.T, h *Handler, w *worker, trigger *protocol.SessionTrigger) {
	t.Helper()
	if trigger == nil {
		trigger = &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher}
	}
	if err := h.StartSession(t.Context(), protocol.SessionStartRequest{Instance: w.instance.Ref(), Action: "open", SessionToken: "session-1", Trigger: trigger}); err != nil {
		t.Fatal(err)
	}
}

func TestPanelPublishesUTCTimestampsWithLocalClock(t *testing.T) {
	location := time.FixedZone("local", -6*60*60)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, location)
	host := new(panelHost)
	host.publish = func(_ context.Context, observation protocol.Observation) error {
		return observation.Validate(now.UTC())
	}
	cfg := fixtureState(t, "").config
	h := newHandler(host, nil, nil, func() time.Time { return now })
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1}, cfg, host, nil, nil, h.now)
	h.workers["slack"] = w
	t.Cleanup(w.cancel)
	w.live()

	startPanel(t, h, w, nil)
	host.pubMu.Lock()
	observations := append([]protocol.Observation(nil), host.observations...)
	host.pubMu.Unlock()
	if len(observations) != 1 || observations[0].ValidUntil.Location() != time.UTC {
		t.Fatalf("panel observations = %+v, want one UTC lease", observations)
	}
}

var testInputSequence atomic.Uint64

func press(h *Handler, w *worker, b protocol.Button) (protocol.SessionInputResult, error) {
	return h.HandleSessionInput(context.Background(), protocol.SessionInputRequest{Instance: w.instance.Ref(), SessionToken: "session-1", Sequence: testInputSequence.Add(1), OccurredAt: w.now().UTC(), Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: b, Action: protocol.ButtonPress}}})
}
func TestFailedOpenKeepsPendingAndDoesNotRetry(t *testing.T) {
	h, w, host := panelFixture(t)
	opens := 0
	h.open = func(ctx context.Context, target string) error {
		opens++
		if target != "slack://channel?id=D123&team=T123" {
			t.Errorf("target %q", target)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Error("unbounded effect")
		}
		return errors.New("private opener detail")
	}
	startPanel(t, h, w, nil)
	if r, e := press(h, w, protocol.ButtonBack); e != nil || r.Disposition != protocol.SessionInputNotConsumed {
		t.Fatalf("root: %v %v", r, e)
	}
	if _, err := press(h, w, protocol.ButtonOK); err != nil {
		t.Fatal(err)
	}
	if _, err := press(h, w, protocol.ButtonStart); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("opener error %v", err)
	}
	_, _ = press(h, w, protocol.ButtonStart)
	found := false
	for _, o := range host.observations {
		if o.ReasonCode == "open_failed" {
			found = true
		}
	}
	if !found {
		t.Error("native failure was not displayed")
	}
	if opens != 1 || host.completes != 1 || w.snapshot().Items[0].Handled || host.recordCount() != 0 {
		t.Fatalf("opens=%d completes=%d state=%+v", opens, host.completes, w.snapshot())
	}
}
func TestPanelRejectsChangedTargetAndGrantCancellation(t *testing.T) {
	for _, change := range []string{"reply", "stale", "cancel-during-grant", "reject-grant"} {
		t.Run(change, func(t *testing.T) {
			h, w, host := panelFixture(t)
			opens := 0
			h.open = func(context.Context, string) error { opens++; return nil }
			startPanel(t, h, w, nil)
			_, _ = press(h, w, protocol.ButtonOK)
			switch change {
			case "reply":
				w.reduce(callback("Ev2", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"2.000001","thread_ts":"1.000001","text":"reply"}`))
			case "stale":
				w.disconnected("auth_required")
			case "cancel-during-grant":
				host.grant = func(context.Context) error { w.cancel(); return nil }
			case "reject-grant":
				host.grant = func(context.Context) error { return errors.New("rejected") }
			}
			if _, err := press(h, w, protocol.ButtonStart); err == nil {
				t.Fatal("accepted invalid target")
			}
			if opens != 0 {
				t.Fatal("opened invalid target")
			}
			if change == "cancel-during-grant" && host.completes != 1 {
				t.Fatal("grant not completed")
			}
		})
	}
}
func TestHandleFailureRemainsRetryableAndNewReplyRearms(t *testing.T) {
	h, w, host := panelFixture(t)
	host.save = func(context.Context, protocol.CheckpointRequest) error { return errors.New("disk failure") }
	handle := func() {
		startPanel(t, h, w, nil)
		_, _ = press(h, w, protocol.ButtonOK)
		_, err := h.HandleSessionInput(t.Context(), protocol.SessionInputRequest{Instance: w.instance.Ref(), SessionToken: "session-1", Sequence: testInputSequence.Add(1), OccurredAt: w.now().UTC(), Input: protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: 1}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	handle()
	if _, err := press(h, w, protocol.ButtonStart); err == nil {
		t.Fatal("save failure succeeded")
	}
	if w.snapshot().Items[0].Handled || host.recordCount() != 0 {
		t.Fatal("failed proposal committed")
	}
	host.save = nil
	handle()
	if _, err := press(h, w, protocol.ButtonStart); err != nil {
		t.Fatal(err)
	}
	if !w.snapshot().Items[0].Handled || host.recordCount() != 1 {
		t.Fatal("handling not durable")
	}
	w.reduce(callback("Ev2", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"2.000001","thread_ts":"1.000001","text":"reply"}`))
	if w.snapshot().Items[0].Handled {
		t.Fatal("new reply suppressed")
	}
}
