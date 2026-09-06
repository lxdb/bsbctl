package githubnotifications

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestUpdatedThreadCreatesEpisodeWithoutReasonChange(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	s := newState(testConfig(), Identity{ID: 42})
	n := notification{ThreadID: "1", Reason: "mention", Unread: true, UpdatedAt: now}
	s.apply(fetchResult{Complete: true, Items: []notification{n}}, nil, now)
	if len(s.attention(now)) != 1 {
		t.Fatal("current unread notification did not alert on first complete fetch")
	}
	episode := s.attention(now)[0].EpisodeID
	n.UpdatedAt = now.Add(time.Minute)
	s.apply(fetchResult{Complete: true, Items: []notification{n}}, nil, n.UpdatedAt)
	if len(s.attention(n.UpdatedAt)) != 1 {
		t.Fatal("new update did not alert")
	}
	if s.attention(n.UpdatedAt)[0].EpisodeID == episode {
		t.Fatal("new update reused the first-fetch episode")
	}
	episode = s.attention(n.UpdatedAt)[0].EpisodeID
	s.apply(fetchResult{Complete: true, Items: []notification{n}}, nil, n.UpdatedAt)
	if s.attention(n.UpdatedAt)[0].EpisodeID != episode {
		t.Fatal("same update rearmed")
	}
}

func TestReasonFilterAcceptsAllAndExactList(t *testing.T) {
	for _, reasons := range []string{`"actionable"`, `"all"`, `["mention","state_change"]`} {
		if _, err := DecodeConfig([]byte(`{"repositories":[{"name":"acme/service","alias":"SVC"}],"notification_reasons":` + reasons + `}`)); err != nil {
			t.Fatalf("%s: %v", reasons, err)
		}
	}
}

func TestNotificationMarkReadHTTPContract(t *testing.T) {
	for _, code := range []int{205, 304} {
		calls := 0
		p := newProvider(testClient(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Method != "PATCH" || r.URL.String() != "https://api.github.com/notifications/threads/17" || r.Header.Get("Authorization") != "Bearer token" {
				t.Fatalf("request: %s %s", r.Method, r.URL)
			}
			return response(code, "", nil), nil
		}), "token")
		if err := p.markRead(t.Context(), notification{ThreadID: "17"}); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatal("write repeated")
		}
	}
}

func TestNotificationUsesFixedIconAndIndependentClippedMarquees(t *testing.T) {
	scene := attentionScene(testConfig(), item{notification: notification{Reason: "review_requested", Title: "Improve release verification", Repository: "acme/long-widget-repository"}})
	var icon, headline, context bool
	for _, e := range scene.Elements {
		if e.Display != "front" {
			continue
		}
		if e.Image != nil {
			icon = e.X == 0 && e.Y == 0 && e.Image.Asset.PackagePath == "assets/github-mark.png"
		}
		if e.Text != nil && e.Text.Value == "Review requested: Improve release verification" {
			headline = e.X == 18 && e.Y == 0 && e.Text.Font == "normal" && e.Text.Width == 54 && e.Text.Marquee != nil
		}
		if e.Text != nil && e.Text.Value == "acme/long-widget-repository" {
			context = e.X == 18 && e.Y == 9 && e.Text.Font == "tiny" && e.Text.Width == 54 && e.Text.Marquee != nil
		}
	}
	if !icon || !headline || !context {
		t.Fatalf("notification layout: %+v", scene)
	}
}

func TestNotificationScenesCompileAtHostBoundary(t *testing.T) {
	scenes := []protocol.Scene{
		attentionScene(testConfig(), item{notification: notification{Reason: "mention", Title: "Review the release", Repository: "acme/widget", Alias: "widget", SubjectType: "PullRequest"}}),
		notificationScene("No unread GitHub notifications", "GITHUB NOTIFICATIONS", "BACK: CLOSE"),
	}
	for _, scene := range scenes {
		if err := scene.Validate(); err != nil {
			t.Fatal(err)
		}
		resolved := presentation.ResolveScene(scene)
		for index := range resolved.Elements {
			if resolved.Elements[index].Image != nil {
				resolved.Elements[index].Path = "pabcdefghij_abcdefghijklmn.png"
			}
		}
		if _, err := presentation.CompileScene("bsbctl", 100, resolved); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnknownReadOutcomeReconcilesWithoutRepeatingWrite(t *testing.T) {
	h, w, host, opened := interactionFixture(t)
	calls := 0
	w.source = newProvider(testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method == "PATCH" {
			calls++
			return response(503, "", nil), nil
		}
		return response(200, `{"html_url":"https://github.com/acme/service/issues/17"}`, nil), nil
	}), "fake")
	selected := w.state.ordered()[0]
	startPanel(t, h, w, true)
	if _, err := press(h, w, 1, protocol.ButtonStart); ErrorCode(err) != "read_unknown" {
		t.Fatalf("unknown outcome: %v", err)
	}
	if calls != 1 || len(*opened) != 1 || len(host.completed) != 1 || len(w.state.attention(w.now())) != 0 || len(host.checkpoints) != 1 {
		t.Fatal("unknown write replayed or stayed actionable")
	}
	if !w.state.handled[selected.ID].Uncertain || w.state.lastModified != "" {
		t.Fatal("missing durable reconciliation marker")
	}
	w.renew(t.Context())
	if calls != 1 {
		t.Fatal("renew retried provider write")
	}
	w.state.apply(fetchResult{Complete: true, NotModified: true}, nil, w.now())
	if len(w.state.attention(w.now())) != 0 {
		t.Fatal("304 cleared uncertain marker")
	}
	w.state.apply(fetchResult{Complete: true, Items: []notification{selected.notification}}, nil, w.now())
	if len(w.state.attention(w.now())) != 1 || calls != 1 {
		t.Fatal("complete unread readback did not allow explicit retry")
	}
}

func TestOpenReadFailureAndDismissHaveDifferentEffects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dismiss bool
	}{
		{name: "open succeeds before mark-read fails"},
		{name: "dismiss does not open before mark-read fails", dismiss: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, w, host, opened := interactionFixture(t)
			writes := 0
			w.source = newProvider(testClient(func(r *http.Request) (*http.Response, error) {
				if r.Method == "PATCH" {
					writes++
					return response(403, "", nil), nil
				}
				return response(200, `{"html_url":"https://github.com/acme/service/issues/17"}`, nil), nil
			}), "fake")
			startPanel(t, h, w, true)
			if tc.dismiss {
				rotate(t, h, w, 1, 1)
			}
			if _, err := press(h, w, 2, protocol.ButtonStart); err == nil {
				t.Fatal("rejected read reported success")
			}
			expectedOpens := 1
			if tc.dismiss {
				expectedOpens = 0
			}
			if writes != 1 || len(*opened) != expectedOpens || len(w.state.items) != 1 || len(host.completed) != 1 {
				t.Fatal("partial effect order")
			}
		})
	}
}

func TestLatestCommentResolutionPrecedesSubjectAndRejectsEscapes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		commentStatus int
	}{
		{name: "latest comment resolves", commentStatus: 200},
		{name: "missing comment falls back to subject", commentStatus: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			p := newProvider(testClient(func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.URL.Path)
				if strings.Contains(r.URL.Path, "comments") {
					return response(tc.commentStatus, `{"html_url":"https://github.com/acme/service/issues/17#issuecomment-42"}`, nil), nil
				}
				return response(200, `{"html_url":"https://github.com/acme/service/issues/17"}`, nil), nil
			}), "fake")
			n := notification{Repository: "acme/service", SubjectURL: "https://api.github.com/repos/acme/service/issues/17", LatestCommentURL: "https://api.github.com/repos/acme/service/issues/comments/42"}
			target, _, err := p.resolveSubject(t.Context(), n)
			if err != nil || paths[0] != "/repos/acme/service/issues/comments/42" {
				t.Fatalf("comment priority %v %v", paths, err)
			}
			if tc.commentStatus == 200 && (len(paths) != 1 || !strings.HasSuffix(target, "#issuecomment-42")) {
				t.Fatal("exact comment lost")
			}
			if tc.commentStatus == 404 && (len(paths) != 2 || target != "https://github.com/acme/service/issues/17") {
				t.Fatal("subject fallback lost")
			}
		})
	}
}
