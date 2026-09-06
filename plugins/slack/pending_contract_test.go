package slack

import (
	"context"
	"errors"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"strings"
	"testing"
)

func TestConfiguredChannelMessagesEnterPendingWithoutMention(t *testing.T) {
	s := fixtureState(t, "")
	applyFixture(t, s, "Ev1", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","text":"Deployment is ready"}`, fixtureNow)
	items := s.items()
	if len(items) != 1 || items[0].Kind != "channel" {
		t.Fatalf("configured channel pending: %+v", items)
	}
	if got := attentionItems(items); len(got) != 0 {
		t.Fatalf("ordinary configured-channel activity entered attention: %+v", got)
	}
}

func TestAttentionIncludesMentionsAndDirectConversationsOnly(t *testing.T) {
	items := []activity{
		{ID: "mentioned-channel", Kind: "channel", Alias: "BUILD", Mention: true},
		{ID: "direct-message", Kind: "dm"},
		{ID: "direct-thread", Kind: "thread"},
		{ID: "ordinary-channel", Kind: "channel", Alias: "BUILD"},
		{ID: "watched-channel-thread", Kind: "thread", Alias: "BUILD"},
		{ID: "handled-direct", Kind: "dm", Handled: true},
	}
	got := attentionItems(items)
	want := []string{"mentioned-channel", "direct-message", "direct-thread"}
	if len(got) != len(want) {
		t.Fatalf("attention items = %+v, want %v", got, want)
	}
	for index := range want {
		if got[index].ID != want[index] {
			t.Fatalf("attention item %d = %q, want %q", index, got[index].ID, want[index])
		}
	}
}
func TestAcceptedSlackOpenClearsPendingEvenWhenCheckpointFails(t *testing.T) {
	h, w, host := panelFixture(t)
	opened := 0
	h.open = func(_ context.Context, target string) error {
		opened++
		if target != "slack://channel?id=D123&team=T123" {
			t.Errorf("native target %q", target)
		}
		return nil
	}
	host.save = func(context.Context, protocol.CheckpointRequest) error { return errors.New("disk unavailable") }
	startPanel(t, h, w, nil)
	_, _ = press(h, w, protocol.ButtonOK)
	_, _ = press(h, w, protocol.ButtonStart)
	if opened != 1 || !w.state.items()[0].Handled || !w.dirty {
		t.Fatal("accepted Open did not clear pending in memory")
	}
	host.save = nil
	w.mu.Lock()
	err := w.saveLocked(t.Context())
	w.mu.Unlock()
	if err != nil || opened != 1 || w.dirty {
		t.Fatal("checkpoint retry replayed opener")
	}
}

func TestPendingCountsKeepMentionAndLocationIndependent(t *testing.T) {
	items := []activity{{Kind: "dm", Mention: true}, {Kind: "channel", Mention: true}, {Kind: "thread"}, {Kind: "dm", Handled: true, Mention: true}}
	got := countPending(items)
	if got != (pendingCounts{Pending: 3, Mentions: 2, DMs: 1, Channels: 1, Threads: 1}) {
		t.Fatalf("pending counts: %+v", got)
	}
	if len(pendingItems(items)) != 3 {
		t.Fatal("handled episode leaked into pending list")
	}
}

func TestFrontPreviewConsentIsIndependentOfRearDetails(t *testing.T) {
	for _, front := range []bool{false, true} {
		for _, rear := range []bool{false, true} {
			cfg := config{frontMessagePreview: front, rearDetails: rear}
			scene := detailScene(cfg, workerSnapshot{Fresh: true}, activity{Kind: "dm", Preview: "message-canary"}, 0, fixtureNow)
			var frontBody, rearBody bool
			for _, e := range scene.Elements {
				if e.Text != nil && strings.Contains(e.Text.Value, "message-canary") {
					if e.Display == protocol.DisplayFront {
						frontBody = true
					} else {
						rearBody = true
					}
				}
			}
			if frontBody != front || rearBody != rear {
				t.Fatalf("consent front=%t rear=%t rendered front=%t rear=%t", front, rear, frontBody, rearBody)
			}
		}
	}
}

func TestRepeatedInputDoesNotToggleDismissOrReopen(t *testing.T) {
	h, w, host := panelFixture(t)
	opens := 0
	h.open = func(context.Context, string) error { opens++; return nil }
	startPanel(t, h, w, nil)
	_, _ = press(h, w, protocol.ButtonOK)
	request := protocol.SessionInputRequest{Instance: w.instance.Ref(), SessionToken: "session-1", Sequence: testInputSequence.Add(1), OccurredAt: w.now().UTC(), Input: protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: 1}}}
	for range 2 {
		if _, err := h.HandleSessionInput(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	}
	if w.panel.level != panelDismiss {
		t.Fatal("duplicate encoder toggled confirmation away")
	}
	request.Sequence = testInputSequence.Add(1)
	request.Input = protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonStart, Action: protocol.ButtonPress}}
	for range 2 {
		if _, err := h.HandleSessionInput(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	}
	if opens != 0 || host.completes != 1 || len(pendingItems(w.snapshot().Items)) != 0 {
		t.Fatal("dismiss replayed or failed to clear pending")
	}
}
