package githubnotifications

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

type interactionHost struct {
	recordingHost
	grants, completed []string
	deny              error
	beforeGrant       func()
}

func (h *interactionHost) BeginSessionExecution(_ context.Context, r protocol.SessionExecutionRequest) error {
	if h.beforeGrant != nil {
		h.beforeGrant()
	}
	if h.deny != nil {
		return h.deny
	}
	h.grants = append(h.grants, r.SessionToken)
	return nil
}
func (h *interactionHost) CompleteSession(_ context.Context, r protocol.CompleteSessionRequest) error {
	h.completed = append(h.completed, r.SessionToken)
	return nil
}

func interactionFixture(t *testing.T) (*Handler, *worker, *interactionHost, *[]string) {
	t.Helper()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	host := &interactionHost{}
	h := newHandler(host, testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPatch {
			return response(205, "", nil), nil
		}
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		return response(200, `{"html_url":"https://github.com/acme/service/issues/17"}`, nil), nil
	}), func() time.Time { return now })
	s := newState(testConfig(), Identity{ID: 42})
	s.apply(fetchResult{Complete: true}, nil, now)
	s.apply(fetchResult{Complete: true, Items: []notification{{ThreadID: "17", Repository: "acme/service", Alias: "SVC", SubjectType: "Issue", SubjectURL: "https://api.github.com/repos/acme/service/issues/17", Title: "PRIVATE TITLE", Reason: "mention", Unread: true, UpdatedAt: now}}}, nil, now)
	w := &worker{ref: protocol.InstanceRef{ID: "gh", Generation: 1}, config: s.config, state: s, host: host, source: newProvider(h.client, "fake"), now: h.now, published: map[string]protocol.Observation{}}
	h.workers["gh"] = w
	opened := []string{}
	h.openURL = func(_ context.Context, url string) error { opened = append(opened, url); return nil }
	if err := w.publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	return h, w, host, &opened
}
func startPanel(t *testing.T, h *Handler, w *worker, attention bool) {
	t.Helper()
	trigger := &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher}
	if attention {
		i := w.state.ordered()[0]
		o := w.published[ChannelAttention+"/"+i.ID]
		trigger = &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{Channel: ChannelAttention, Key: i.ID, Revision: o.Revision}}
	}
	if err := h.StartSession(t.Context(), protocol.SessionStartRequest{Instance: w.ref, SessionToken: "session-1", Action: "open", Trigger: trigger}); err != nil {
		t.Fatal(err)
	}
}
func press(h *Handler, w *worker, seq uint64, button protocol.Button) (protocol.SessionInputResult, error) {
	return h.HandleSessionInput(context.Background(), protocol.SessionInputRequest{Instance: w.ref, SessionToken: "session-1", Sequence: seq, OccurredAt: w.now(), Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: button, Action: protocol.ButtonPress}}})
}
func TestAttentionOnePressOpensExactURLAndMarksRead(t *testing.T) {
	h, w, host, opened := interactionFixture(t)
	startPanel(t, h, w, true)
	if len(*opened) != 0 || len(host.grants) != 0 {
		t.Fatal("effect before promotion")
	}
	if _, err := press(h, w, 1, protocol.ButtonStart); err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 1 || (*opened)[0] != "https://github.com/acme/service/issues/17" || len(host.grants) != 1 || len(host.completed) != 1 {
		t.Fatalf("effect=%v grants=%v completion=%v", *opened, host.grants, host.completed)
	}
	if len(w.state.items) != 0 {
		t.Fatal("open did not remove confirmed-read notification")
	}
	_, _ = press(h, w, 1, protocol.ButtonStart)
	_, _ = press(h, w, 2, protocol.ButtonStart)
	if len(*opened) != 1 {
		t.Fatal("duplicate opened again")
	}
}
func TestEncoderDismissMarksReadWithoutOpening(t *testing.T) {
	h, w, host, opened := interactionFixture(t)
	startPanel(t, h, w, true)
	_, err := h.HandleSessionInput(t.Context(), protocol.SessionInputRequest{Instance: w.ref, SessionToken: "session-1", Sequence: 1, OccurredAt: w.now(), Input: protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if w.session.level != panelConfirm || len(host.grants) != 0 {
		t.Fatal("encoder did not stage dismissal")
	}
	if _, err := press(h, w, 2, protocol.ButtonBack); err != nil {
		t.Fatal(err)
	}
	if w.session.level != panelDetail {
		t.Fatal("BACK did not cancel dismissal")
	}
	_, _ = h.HandleSessionInput(t.Context(), protocol.SessionInputRequest{Instance: w.ref, SessionToken: "session-1", Sequence: 3, OccurredAt: w.now(), Input: protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: -2}}})
	if _, err := press(h, w, 4, protocol.ButtonStart); err != nil {
		t.Fatal(err)
	}
	if len(w.state.items) != 0 || len(*opened) != 0 || len(host.grants) != 1 || len(host.completed) != 1 {
		t.Fatal("dismissal did not mark read exactly once without opener")
	}
}
func TestPanelRejectsInvalidatedSelectionAndUnsafeTargets(t *testing.T) {
	for _, mode := range []string{"removed", "changed", "stale", "retired", "denied", "unsafe", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			h, w, host, opened := interactionFixture(t)
			startPanel(t, h, w, true)
			i := w.state.ordered()[0]
			switch mode {
			case "removed":
				delete(w.state.items, i.ID)
			case "changed":
				i.Revision++
				w.state.items[i.ID] = i
			case "stale":
				w.state.freshUntil = w.now()
			case "retired":
				w.retiring.Store(true)
			case "denied":
				host.deny = errors.New("preempted")
			case "unsafe":
				w.source = newProvider(testClient(func(*http.Request) (*http.Response, error) {
					return response(200, `{"html_url":"https://github.com.evil.test/pwn"}`, nil), nil
				}), "fake")
			}
			ctx := t.Context()
			if mode == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, err := h.HandleSessionInput(ctx, protocol.SessionInputRequest{Instance: w.ref, SessionToken: "session-1", Sequence: 1, OccurredAt: w.now(), Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonStart, Action: protocol.ButtonPress}}})
			if err == nil {
				t.Fatal("invalid target accepted")
			}
			if len(*opened) != 0 || len(host.completed) != 0 {
				t.Fatal("effect or completion without grant")
			}
		})
	}
}
func TestNewSubjectTypeUsesExactTargetAndOpenerFailureCompletes(t *testing.T) {
	h, w, host, opened := interactionFixture(t)
	i := w.state.ordered()[0]
	i.SubjectType = "Discussion"
	w.state.items[i.ID] = i
	writes := 0
	w.source = newProvider(testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method == "PATCH" {
			writes++
			return response(205, "", nil), nil
		}
		return response(200, `{"html_url":"https://github.com/acme/service/discussions/17"}`, nil), nil
	}), "fake")
	h.openURL = func(_ context.Context, target string) error {
		*opened = append(*opened, target)
		return errors.New("private command error")
	}
	startPanel(t, h, w, true)
	_, err := press(h, w, 1, protocol.ButtonStart)
	if err == nil || strings.Contains(err.Error(), "private command error") || len(*opened) != 1 || (*opened)[0] != "https://github.com/acme/service/discussions/17" || writes != 0 || len(host.completed) != 1 || len(w.state.items) != 1 {
		t.Fatalf("failed opener effects=%v writes=%d err=%v", *opened, writes, err)
	}
}

func sceneText(scene protocol.Scene) string {
	var out strings.Builder
	for _, e := range scene.Elements {
		if e.Text != nil {
			out.WriteString(e.Text.Value)
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func TestAttentionTriggerRejectsDomainChangeAfterFailedPublication(t *testing.T) {
	h, w, host, opened := interactionFixture(t)
	i := w.state.ordered()[0]
	o := w.published[ChannelAttention+"/"+i.ID]
	i.Revision++
	i.SubjectURL = "https://api.github.com/repos/acme/service/issues/99"
	w.state.items[i.ID] = i
	host.publicationErr = errors.New("host unavailable")
	_ = w.refreshPanel(t.Context())
	host.publicationErr = nil
	err := h.StartSession(t.Context(), protocol.SessionStartRequest{Instance: w.ref, SessionToken: "session-1", Action: "open", Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{Channel: o.Channel, Key: o.Key, Revision: o.Revision}}})
	if err == nil {
		t.Fatal("old rendered revision rebound to materially changed item")
	}
	if len(*opened) != 0 {
		t.Fatal("stale session opened")
	}
}

func TestUnconfiguredPanelRenewsWithoutProviderIO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := &recordingHost{}
		h := newHandler(host, testClient(func(*http.Request) (*http.Response, error) {
			t.Error("unconfigured provider I/O")
			return nil, errors.New("forbidden")
		}), time.Now)
		ref := protocol.InstanceRef{ID: "gh", Generation: 1}
		if err := h.ReplaceInstances(t.Context(), []protocol.Instance{{ID: ref.ID, Generation: ref.Generation, Config: json.RawMessage(`{}`)}}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
		if err := h.StartSession(t.Context(), protocol.SessionStartRequest{Instance: ref, SessionToken: "empty-panel", Action: "open", Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher}}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(46 * time.Second)
		synctest.Wait()
		host.mu.Lock()
		defer host.mu.Unlock()
		last := host.observations[len(host.observations)-1]
		if last.Channel != ChannelLive || !last.ValidUntil.After(time.Now()) || !strings.Contains(sceneText(*last.Scene), "GitHub setup required") {
			t.Fatal("unconfigured foreground panel expired")
		}
	})
}

func rotate(t *testing.T, h *Handler, w *worker, seq uint64, delta int32) {
	t.Helper()
	if _, err := h.HandleSessionInput(t.Context(), protocol.SessionInputRequest{Instance: w.ref, SessionToken: "session-1", Sequence: seq, OccurredAt: w.now(), Input: protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: delta}}}); err != nil {
		t.Fatal(err)
	}
}
func TestPanelSelectionNavigationFreezesConfirmation(t *testing.T) {
	h, w, host, opened := interactionFixture(t)
	first := w.state.ordered()[0]
	other := first
	other.ID = "item-other"
	other.ThreadID = "18"
	other.Alias = "OTHER"
	other.Reason = "subscribed"
	w.state.items[other.ID] = other
	startPanel(t, h, w, false)
	rotate(t, h, w, 1, 1)
	if w.session.selected.ID != other.ID {
		t.Fatal("manual list did not select next thread")
	}
	if _, err := press(h, w, 2, protocol.ButtonOK); err != nil {
		t.Fatal(err)
	}
	rotate(t, h, w, 3, 1)
	other.Revision++
	w.state.items[other.ID] = other
	if _, err := press(h, w, 4, protocol.ButtonStart); !errors.Is(err, ErrStaleNotification) {
		t.Fatalf("changed confirmation: %v", err)
	}
	if len(*opened) != 0 || len(host.grants) != 0 {
		t.Fatal("changed confirmation executed")
	}
	if _, err := press(h, w, 5, protocol.ButtonBack); err != nil {
		t.Fatal(err)
	}
	if w.session.level != panelDetail {
		t.Fatal("dismiss BACK did not return to detail")
	}
}

func TestConnectionTriggerOpensListWithoutEffect(t *testing.T) {
	for _, channel := range []string{ChannelConnection} {
		t.Run(channel, func(t *testing.T) {
			h, w, host, opened := interactionFixture(t)
			if channel == ChannelConnection {
				w.state.phase = "degraded"
				w.state.lastError = "network_error"
				if err := w.refreshPanel(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			o := w.published[channel+"/"+channel]
			if err := h.StartSession(t.Context(), protocol.SessionStartRequest{Instance: w.ref, SessionToken: "session-1", Action: "open", Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{Channel: channel, Key: o.Key, Revision: o.Revision}}}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(sceneText(w.sessionScene()), "NOTIFICATIONS 1/1") || len(*opened) != 0 || len(host.grants) != 0 {
				t.Fatal("summary/connection did not open list")
			}
		})
	}
}
func TestReleaseDuplicateAndReplacedSessionInputsDoNotNavigateOrOpen(t *testing.T) {
	h, w, _, opened := interactionFixture(t)
	startPanel(t, h, w, false)
	if _, err := press(h, w, 1, protocol.ButtonOK); err != nil {
		t.Fatal(err)
	}
	_, _ = press(h, w, 1, protocol.ButtonOK)
	if !strings.Contains(sceneText(w.sessionScene()), "TURN: DISMISS") {
		t.Fatal("duplicate OK navigated twice")
	}
	r := protocol.SessionInputRequest{Instance: w.ref, SessionToken: "session-1", Sequence: 2, OccurredAt: w.now(), Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonStart, Action: protocol.ButtonRelease}}}
	if _, err := h.HandleSessionInput(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err := h.EndSession(t.Context(), protocol.SessionEndRequest{Instance: w.ref, SessionToken: "older"}); err != nil {
		t.Fatal(err)
	}
	if w.session == nil {
		t.Fatal("old EndSession cleared newer panel")
	}
	if err := h.EndSession(t.Context(), protocol.SessionEndRequest{Instance: w.ref, SessionToken: "session-1"}); err != nil {
		t.Fatal(err)
	}
	startPanel(t, h, w, false)
	w.session.token = "replacement"
	r.Sequence = 3
	r.Input.Button.Action = protocol.ButtonPress
	if _, err := h.HandleSessionInput(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	r.SessionToken = "replacement"
	r.Instance.Generation++
	if _, err := h.HandleSessionInput(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 0 || !strings.Contains(sceneText(w.sessionScene()), "NOTIFICATIONS 1/1") {
		t.Fatal("release or retired input affected replacement")
	}
}
func TestSessionResolutionBudgetAndCancellationAfterGrant(t *testing.T) {
	t.Run("resolution budget", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h, w, host, opened := interactionFixture(t)
			w.source = newProvider(testClient(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() }), "fake")
			startPanel(t, h, w, true)
			before := time.Now()
			if _, err := press(h, w, 1, protocol.ButtonStart); err == nil {
				t.Fatal("timeout accepted")
			}
			if time.Since(before) != 5*time.Second || len(host.grants) != 0 || len(*opened) != 0 {
				t.Fatal("resolution exceeded total effect budget or obtained grant")
			}
		})
	})
	t.Run("cancel after grant", func(t *testing.T) {
		h, w, host, opened := interactionFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		host.beforeGrant = cancel
		startPanel(t, h, w, true)
		_, err := h.HandleSessionInput(ctx, protocol.SessionInputRequest{Instance: w.ref, SessionToken: "session-1", Sequence: 1, OccurredAt: w.now(), Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonStart, Action: protocol.ButtonPress}}})
		if !errors.Is(err, context.Canceled) || len(*opened) != 0 || len(host.completed) != 1 || w.session != nil {
			t.Fatalf("grant cleanup after cancellation: %v %v %v", err, *opened, host.completed)
		}
	})
}
func TestInaccessibleSubjectNeverOpensInboxOrObtainsGrant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		retire bool
	}{
		{name: "subject unavailable"},
		{name: "instance retires during resolution", retire: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, w, host, opened := interactionFixture(t)
			w.source = newProvider(testClient(func(*http.Request) (*http.Response, error) {
				if tc.retire {
					w.retiring.Store(true)
				}
				return response(404, `{}`, nil), nil
			}), "fake")
			startPanel(t, h, w, true)
			_, err := press(h, w, 1, protocol.ButtonStart)
			if err == nil || len(*opened) != 0 || len(host.grants) != 0 || len(w.state.items) != 1 {
				t.Fatalf("unavailable target effect: %v", err)
			}
		})
	}
}

func TestFrontSubjectMarqueeAndRearDetailsStayBounded(t *testing.T) {
	h, w, host, _ := interactionFixture(t)
	w.config.RearDetails = true
	i := w.state.ordered()[0]
	i.Title = strings.Repeat("private ", 18) + "TAILMARK"
	w.state.items[i.ID] = i
	startPanel(t, h, w, true)
	scene := w.sessionScene()
	if !strings.Contains(sceneText(scene), "TAILMARK") {
		t.Fatal("marquee truncated subject")
	}
	for _, e := range scene.Elements {
		if e.Text != nil && e.Display == protocol.DisplayBack && len(e.Text.Value) > 30 {
			t.Fatal("unbounded rear row")
		}
	}
	for _, o := range host.observations {
		if err := o.Validate(w.now()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFreshLivePanelExpiresAtSourceDeadlineThenRenewsAsStale(t *testing.T) {
	h, w, _, _ := interactionFixture(t)
	deadline := w.state.freshUntil
	now := deadline.Add(-time.Second)
	w.now = func() time.Time { return now }
	startPanel(t, h, w, false)
	key := ChannelLive + "/" + hashKey("session", "session-1")
	fresh := w.published[key]
	if !fresh.ValidUntil.Equal(deadline) || !strings.Contains(sceneText(*fresh.Scene), "NOTIFICATIONS") {
		t.Fatalf("fresh live deadline/scene = %s / %s; source deadline %s", fresh.ValidUntil, sceneText(*fresh.Scene), deadline)
	}
	now = deadline
	if err := w.publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	stale := w.published[key]
	if !stale.ValidUntil.Equal(now.Add(45*time.Second)) || !strings.Contains(sceneText(*stale.Scene), "coverage may be incomplete") {
		t.Fatalf("stale live deadline/scene = %s / %s", stale.ValidUntil, sceneText(*stale.Scene))
	}
	if err := stale.Validate(now); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultRearDoesNotRevealSubjectWhileFrontShowsRequestedContext(t *testing.T) {
	h, w, _, _ := interactionFixture(t)
	startPanel(t, h, w, true)
	scene := w.sessionScene()
	for _, e := range scene.Elements {
		if e.Display == protocol.DisplayBack && e.Text != nil && strings.Contains(e.Text.Value, "PRIVATE TITLE") {
			t.Fatal("rear consent broadened")
		}
	}
	if !strings.Contains(sceneText(scene), "Mentioned: PRIVATE TITLE") {
		t.Fatal("front subject missing")
	}
}
