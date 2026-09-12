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

// The checkpoint-only fixture never admits desktop effects.
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
	w.setMembershipProofLocked("C123", membershipProof{member: true, name: "BUILD", expires: fixtureNow.Add(membershipPositiveTTL)})
	w.setMembershipProofLocked("G123", membershipProof{member: true, name: "PRIVATE", expires: fixtureNow.Add(membershipPositiveTTL)})
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

func publishedPanelText(t *testing.T, host *panelHost) string {
	t.Helper()
	host.pubMu.Lock()
	defer host.pubMu.Unlock()
	for i := len(host.observations) - 1; i >= 0; i-- {
		o := host.observations[i]
		if o.Channel != ChannelLive || o.Scene == nil {
			continue
		}
		var text []string
		for _, e := range o.Scene.Elements {
			if e.Text != nil {
				text = append(text, e.Text.Value)
			}
		}
		return strings.Join(text, "\n")
	}
	t.Fatal("no live panel was published")
	return ""
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
	if _, err := press(h, w, protocol.ButtonStart); err != nil {
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

func TestRotaryPressDoesNotChangeTheSelectedSlackAction(t *testing.T) {
	h, w, host := panelFixture(t)
	opens, grants := 0, 0
	h.open = func(context.Context, string) error { opens++; return nil }
	host.grant = func(context.Context) error { grants++; return nil }
	startPanel(t, h, w, nil)
	if _, err := press(h, w, protocol.ButtonStart); err != nil {
		t.Fatal(err)
	}
	if _, err := press(h, w, protocol.ButtonOK); err != nil {
		t.Fatal(err)
	}
	if text := publishedPanelText(t, host); !strings.Contains(text, "PLAY: OPEN IN SLACK") {
		t.Fatalf("rotary press changed the displayed action: %q", text)
	}
	if opens != 0 || grants != 0 || host.recordCount() != 0 || host.completes != 0 {
		t.Fatalf("rotary press executed: opens=%d grants=%d checkpoints=%d completes=%d", opens, grants, host.recordCount(), host.completes)
	}
	if _, err := press(h, w, protocol.ButtonStart); err != nil {
		t.Fatal(err)
	}
	if opens != 1 || grants != 1 || host.completes != 1 {
		t.Fatalf("Play did not execute the retained Open action: opens=%d grants=%d completes=%d", opens, grants, host.completes)
	}
}

func TestReaderScrollClampsAtEndpointAndReverses(t *testing.T) {
	h, w, host := panelFixture(t)
	// Distinct page contents detect a correct page label paired with wrong text.
	p := &panelSession{token: "session-1", level: panelReader, target: activity{Preview: strings.Repeat("A", 48) + strings.Repeat("B", 48) + strings.Repeat("C", 48) + "final-page"}}
	w.panel = p
	input := func(sequence uint64, delta int32) error {
		_, err := h.HandleSessionInput(t.Context(), protocol.SessionInputRequest{Instance: w.instance.Ref(), SessionToken: p.token, Sequence: sequence, OccurredAt: w.now().UTC(), Input: protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: delta}}})
		return err
	}
	if err := input(1, 100); err != nil {
		t.Fatal(err)
	}
	if text := publishedPanelText(t, host); !strings.Contains(text, "PAGE 4/4") || !strings.Contains(text, "final-page") {
		t.Fatalf("overscroll did not display the final page: %q", text)
	}
	if err := input(2, -1); err != nil {
		t.Fatal(err)
	}
	if text := publishedPanelText(t, host); !strings.Contains(text, "PAGE 3/4") || !strings.Contains(text, "CCCCCCCCCCCCCCCCCCCCCCCC") || strings.Contains(text, "final-page") {
		t.Fatalf("reader did not display the previous page after reversing: %q", text)
	}
}

func TestListPinsRenderedItemAcrossReorderAndDoesNotSubstituteRemoval(t *testing.T) {
	t.Run("reorder", func(t *testing.T) {
		h, w, host := panelFixture(t)
		w.cfg.frontMessagePreview = true
		w.reduce(callback("EvVisible", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"2.000001","text":"selected-message"}`))
		startPanel(t, h, w, nil)
		selected := w.panel.target
		other := selected
		other.ID = "other-item"
		other.ChannelID = "C999"
		other.UpdatedAt = selected.UpdatedAt.Add(time.Minute)
		other.Revision++
		other.Preview = "other-message"
		w.mu.Lock()
		w.state.aggregates[other.ID] = other
		w.cacheLocked()
		w.mu.Unlock()
		if err := w.publishCurrentPanel(t.Context()); err != nil {
			t.Fatal(err)
		}
		if text := publishedPanelText(t, host); !strings.Contains(text, "selected-message") || strings.Contains(text, "other-message") {
			t.Fatalf("reordered list replaced the displayed selection: %q", text)
		}
		opened := ""
		h.open = func(_ context.Context, target string) error { opened = target; return nil }
		if _, err := press(h, w, protocol.ButtonStart); err != nil {
			t.Fatal(err)
		}
		if _, err := press(h, w, protocol.ButtonStart); err != nil {
			t.Fatal(err)
		}
		if opened != "slack://channel?id=D123&team=T123" {
			t.Fatalf("opened %q, want selected item", opened)
		}
	})
	t.Run("removed", func(t *testing.T) {
		h, w, host := panelFixture(t)
		startPanel(t, h, w, nil)
		selected := w.panel.target
		w.reduce(callback("EvSurvivor", `{"type":"message","channel":"D456","channel_type":"im","user":"U456","ts":"2.000001","text":"surviving-message"}`))
		w.mu.Lock()
		delete(w.state.aggregates, selected.ID)
		w.cacheLocked()
		w.mu.Unlock()
		opened, grants := "", 0
		h.open = func(_ context.Context, target string) error { opened = target; return nil }
		host.grant = func(context.Context) error { grants++; return nil }
		for range 2 {
			if _, err := press(h, w, protocol.ButtonStart); err != nil {
				t.Fatal(err)
			}
		}
		if text := publishedPanelText(t, host); !strings.Contains(text, "Slack item changed") || opened != "" || grants != 0 {
			t.Fatalf("removed selection was substituted: panel=%q opened=%q grants=%d", text, opened, grants)
		}
		_, err := h.HandleSessionInput(t.Context(), protocol.SessionInputRequest{Instance: w.instance.Ref(), SessionToken: "session-1", Sequence: testInputSequence.Add(1), OccurredAt: w.now().UTC(), Input: protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := press(h, w, protocol.ButtonStart); err != nil {
				t.Fatal(err)
			}
		}
		if opened != "slack://channel?id=D456&team=T123" || grants != 1 {
			t.Fatalf("explicit navigation did not select survivor: opened=%q grants=%d", opened, grants)
		}
	})
}

func TestInitiallyEmptyListRequiresSelectionBeforeOpenOrDismiss(t *testing.T) {
	for _, dismiss := range []bool{false, true} {
		name := "open"
		if dismiss {
			name = "dismiss"
		}
		t.Run(name, func(t *testing.T) {
			h, w, host := panelFixture(t)
			w.mu.Lock()
			w.cfg.frontMessagePreview = true
			w.state = newState(w.cfg, w.cfg.userID)
			w.cacheLocked()
			w.mu.Unlock()
			var opened string
			grants := 0
			h.open = func(_ context.Context, target string) error { opened = target; return nil }
			host.grant = func(context.Context) error { grants++; return nil }
			rotate := func(delta int32) {
				t.Helper()
				_, err := h.HandleSessionInput(t.Context(), protocol.SessionInputRequest{Instance: w.instance.Ref(), SessionToken: "session-1", Sequence: testInputSequence.Add(1), OccurredAt: w.now().UTC(), Input: protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: delta}}})
				if err != nil {
					t.Fatal(err)
				}
			}
			startPanel(t, h, w, nil)
			w.reduce(callback("EvFirst", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"first-message"}`))
			if err := w.publishCurrentPanel(t.Context()); err != nil {
				t.Fatal(err)
			}
			if text := publishedPanelText(t, host); !strings.Contains(text, "TURN TO SELECT") || strings.Contains(text, "PLAY:") {
				t.Errorf("arrival must require a selection: %q", text)
			}
			// A newer item becomes first after publication, before the button press.
			w.reduce(callback("EvSecond", `{"type":"message","channel":"D456","channel_type":"im","user":"U456","ts":"2.000001","text":"second-message"}`))
			for range 2 {
				if _, err := press(h, w, protocol.ButtonStart); err != nil {
					t.Fatal(err)
				}
			}
			if grants != 0 || opened != "" {
				t.Fatalf("unselected arrival executed: grants=%d opened=%q", grants, opened)
			}
			if text := publishedPanelText(t, host); !strings.Contains(text, "TURN TO SELECT") {
				t.Fatalf("unselected button changed the panel: %q", text)
			}
			rotate(-1) // Select the last item, the original D123 message.
			if text := publishedPanelText(t, host); !strings.Contains(text, "first-message") || !strings.Contains(text, "PLAY: DETAILS") {
				t.Fatalf("explicit selection was not displayed: %q", text)
			}
			if _, err := press(h, w, protocol.ButtonStart); err != nil {
				t.Fatal(err)
			}
			if dismiss {
				rotate(1)
			}
			if _, err := press(h, w, protocol.ButtonStart); err != nil {
				t.Fatal(err)
			}
			wantOpened := "slack://channel?id=D123&team=T123"
			if dismiss {
				wantOpened = ""
			}
			pending := pendingItems(w.snapshot().Items)
			if grants != 1 || opened != wantOpened || len(pending) != 1 || pending[0].ChannelID != "D456" {
				t.Fatalf("selection effect: grants=%d opened=%q pending=%+v", grants, opened, pending)
			}
		})
	}
}
func TestPanelRejectsChangedTargetAndGrantCancellation(t *testing.T) {
	for _, change := range []string{"reply", "stale", "cancel-during-grant", "reject-grant"} {
		t.Run(change, func(t *testing.T) {
			h, w, host := panelFixture(t)
			opens := 0
			h.open = func(context.Context, string) error { opens++; return nil }
			startPanel(t, h, w, nil)
			_, _ = press(h, w, protocol.ButtonStart)
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
		_, _ = press(h, w, protocol.ButtonStart)
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
