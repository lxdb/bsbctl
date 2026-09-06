package githubnotifications

import (
	"fmt"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string, headers http.Header) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: headers}
}
func testClient(f transportFunc) *http.Client { return &http.Client{Transport: f} }
func threadJSON(id, reason, updated string) string {
	return fmt.Sprintf(`{"id":%q,"unread":true,"reason":%q,"updated_at":%q,"repository":{"id":7,"full_name":"acme/service"},"subject":{"title":"private title","type":"PullRequest","url":"https://api.github.com/repos/acme/service/pulls/9"}}`, id, reason, updated)
}
func testConfig() Config {
	c, _ := DecodeConfig([]byte(`{"repositories":[{"name":"acme/service","alias":"SVC"}]}`))
	return c
}
func TestConfigAcceptsAllRepositoriesAndRejectsPartialOrAmbiguousScope(t *testing.T) {
	for _, raw := range []string{`null`, `{"label":"GH"}`, `{"repositories":[{"name":"acme/service","alias":"SVC"}],"poll_interval_seconds":59}`, `{"repositories":[{"name":"acme/service","alias":"SVC"},{"name":"ACME/Service","alias":"TWO"}]}`, `{"repositories":[{"name":"acme/service","alias":"SVC","extra":1}]}`, `{"repositories":[{"name":"acme/service","alias":"SVC"}],"rear_details":null}`} {
		if _, err := DecodeConfig([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if c, err := DecodeConfig([]byte(`{"repositories":[]}`)); err != nil || !c.Configured || len(c.Repositories) != 0 {
		t.Fatalf("all repositories config: %+v %v", c, err)
	}
	if c, err := DecodeConfig([]byte(`{}`)); err != nil || c.Configured {
		t.Fatalf("empty config: %+v %v", c, err)
	}
}

func TestAllRepositoriesCollectsInboxAndDerivesAlias(t *testing.T) {
	c, err := DecodeConfig([]byte(`{"repositories":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := newProvider(testClient(func(r *http.Request) (*http.Response, error) {
		return response(200, `[{"id":"1","unread":true,"reason":"mention","updated_at":"2026-09-05T10:00:00Z","repository":{"id":7,"full_name":"acme/long-service-name"},"subject":{"title":"title","type":"PullRequest","url":"https://api.github.com/repos/acme/long-service-name/pulls/9"}}]`, nil), nil
	}), "token")
	result, err := p.fetch(t.Context(), c, "")
	if err != nil || !result.Complete || len(result.Items) != 1 {
		t.Fatalf("all repository fetch: %+v %v", result, err)
	}
	if result.Items[0].Repository != "acme/long-service-name" || result.Items[0].Alias != "long-ser" {
		t.Fatalf("all repository item: %+v", result.Items[0])
	}
}
func TestPaginationPreservesPartialAndRejectsHostileLinks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hostile bool
	}{
		{name: "provider pagination fails", hostile: false},
		{name: "cross-origin pagination is rejected", hostile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := testClient(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "api.github.com" {
					t.Fatal("credential left GitHub")
				}
				if r.Header.Get("Authorization") != "Bearer private-token" || r.Header.Get("X-GitHub-Api-Version") != "2026-03-10" {
					t.Fatal("request contract")
				}
				if calls == 2 {
					return response(503, `{}`, nil), nil
				}
				next := "https://api.github.com/notifications?page=2"
				if tc.hostile {
					next = "https://evil.example/steal"
				}
				return response(200, "["+threadJSON("1", "mention", "2026-09-05T10:00:00Z")+"]", http.Header{"Link": {`<` + next + `>; rel="next"`}, "Last-Modified": {"Sat, 05 Sep 2026 10:00:00 GMT"}}), nil
			})
			result, err := newProvider(client, "private-token").fetch(t.Context(), testConfig(), "")
			if err == nil || result.Complete || len(result.Items) != 1 || result.LastModified != "" {
				t.Fatalf("partial = %+v %v", result, err)
			}
			if tc.hostile && calls != 1 {
				t.Fatal("followed hostile page")
			}
		})
	}
}
func TestBaselineReasonEpisodesAndPartialAbsence(t *testing.T) {
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	s := newState(testConfig(), Identity{ID: 42})
	item := notification{ThreadID: "1", Reason: "mention", Unread: true, RepositoryID: 7, Repository: "acme/service", UpdatedAt: now}
	s.apply(fetchResult{Items: []notification{item}, Complete: true}, nil, now)
	if len(s.attention(now)) != 1 {
		t.Fatal("current unread notification did not interrupt rotation")
	}
	item.Reason = "review_requested"
	item.UpdatedAt = now.Add(time.Minute)
	s.apply(fetchResult{Items: []notification{item}, Complete: true}, nil, now.Add(time.Minute))
	a := s.attention(now.Add(time.Minute))
	if len(a) != 1 {
		t.Fatal("reason change did not arm")
	}
	observed := a[0].ObservedAt
	s.apply(fetchResult{NotModified: true, Complete: true}, nil, now.Add(90*time.Second))
	if got := s.attention(now.Add(90 * time.Second))[0].ObservedAt; !got.Equal(observed) {
		t.Fatal("304 rearmed episode")
	}
	s.apply(fetchResult{}, &sourceError{Code: "api_unavailable"}, now.Add(100*time.Second))
	if len(s.items) != 1 {
		t.Fatal("partial removed unread")
	}
	s.apply(fetchResult{Complete: true}, nil, now.Add(110*time.Second))
	if len(s.items) != 0 {
		t.Fatal("complete absence retained unread")
	}
}

func TestStateNormalizesEpisodeTimestampsToUTC(t *testing.T) {
	zone := time.FixedZone("local", -6*60*60)
	now := time.Date(2026, 9, 6, 1, 0, 0, 0, zone)
	s := newState(testConfig(), Identity{ID: 42})
	s.apply(fetchResult{Complete: true, Items: []notification{{
		ThreadID: "1", Reason: "mention", Unread: true, Repository: "acme/service", UpdatedAt: now.UTC(),
	}}}, nil, now)
	item := s.attention(now)[0]
	if _, offset := item.ObservedAt.Zone(); offset != 0 {
		t.Fatalf("episode observed_at offset = %d, want UTC", offset)
	}
	if _, offset := s.lastSuccess.Zone(); offset != 0 {
		t.Fatalf("last_success offset = %d, want UTC", offset)
	}
	observation := protocol.Observation{
		Instance: protocol.InstanceRef{ID: "github", Generation: 1}, Channel: ChannelAttention, Key: item.ID, Revision: 1,
		Disposition: protocol.DispositionActionable, Impact: protocol.ImpactNotable, ReasonCode: "github_notifications_attention",
		ObservedAt: item.ObservedAt, UpdatedAt: now.UTC(), ValidUntil: now.UTC().Add(time.Minute), Scene: new(attentionScene(testConfig(), item)),
	}
	if err := observation.Validate(now.UTC()); err != nil {
		t.Fatal(err)
	}
}
func TestCold304CannotEstablishBaseline(t *testing.T) {
	s := newState(testConfig(), Identity{ID: 42})
	s.apply(fetchResult{NotModified: true, Complete: true}, nil, time.Now())
	if s.baseline || !s.lastSuccess.IsZero() {
		t.Fatal("cold 304 claimed complete baseline")
	}
}
func TestStaleAttentionWithdrawnWithoutDeletingSource(t *testing.T) {
	now := time.Now()
	s := newState(testConfig(), Identity{ID: 42})
	s.apply(fetchResult{Complete: true}, nil, now)
	s.apply(fetchResult{Complete: true, Items: []notification{{ThreadID: "1", Repository: "acme/service", RepositoryID: 7, Reason: "mention", Unread: true, UpdatedAt: now}}}, nil, now)
	if len(s.attention(now)) != 1 {
		t.Fatal("missing attention")
	}
	if len(s.attention(now.Add(121*time.Second))) != 0 || len(s.items) != 1 {
		t.Fatal("stale source treatment")
	}
}

func TestCompletePaginationCommitsFirstValidator(t *testing.T) {
	calls := 0
	p := newProvider(testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response(200, "["+threadJSON("1", "mention", "2026-09-05T10:00:00Z")+"]", http.Header{"Link": {`<https://api.github.com/notifications?page=2>; rel="next"`}, "Last-Modified": {"Sat, 05 Sep 2026 10:00:00 GMT"}, "X-Poll-Interval": {"120"}}), nil
		}
		if r.Header.Get("If-Modified-Since") != "" {
			t.Fatal("validator leaked onto later page")
		}
		return response(200, "[]", http.Header{"Last-Modified": {"Sat, 05 Sep 2026 11:00:00 GMT"}}), nil
	}), "token")
	r, err := p.fetch(t.Context(), testConfig(), "old")
	if err != nil || !r.Complete || r.LastModified != "Sat, 05 Sep 2026 10:00:00 GMT" || r.PollInterval != 120*time.Second {
		t.Fatalf("complete pagination: %+v %v", r, err)
	}
}
func TestNewUpdateReplacesHandledEpisodeAndRestartRestoresCurrentUnread(t *testing.T) {
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	s := newState(testConfig(), Identity{ID: 42})
	s.apply(fetchResult{Complete: true}, nil, now)
	n := notification{ThreadID: "1", Repository: "acme/service", RepositoryID: 7, Reason: "mention", Unread: true, UpdatedAt: now}
	s.apply(fetchResult{Complete: true, Items: []notification{n}}, nil, now)
	i := s.ordered()[0]
	s.handled[i.ID] = handledEpisode{ID: i.ID, Reason: i.Reason, EpisodeID: i.EpisodeID, ObservedAt: i.ObservedAt, UpdatedAt: i.UpdatedAt, HandledAt: now}
	n.UpdatedAt = now.Add(time.Minute)
	s.apply(fetchResult{Complete: true, Items: []notification{n}}, nil, now.Add(time.Minute))
	if len(s.attention(now.Add(time.Minute))) != 1 {
		t.Fatal("new comment did not create episode")
	}
	raw := s.checkpointData(now.Add(time.Minute))
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "https:") {
		t.Fatal("private checkpoint")
	}
	restarted := newState(testConfig(), Identity{ID: 42})
	if code := restarted.restore(raw, now.Add(time.Minute)); code != "" {
		t.Fatal(code)
	}
	if restarted.lastModified != "" || restarted.baseline {
		t.Fatal("checkpoint restored dataset freshness")
	}
	restarted.apply(fetchResult{Complete: true, Items: []notification{n}}, nil, now.Add(time.Minute))
	if restarted.ordered()[0].Handled || len(restarted.attention(now.Add(time.Minute))) != 1 {
		t.Fatal("restart restored obsolete handling or suppressed current unread notification")
	}
	n.Reason = "assign"
	n.UpdatedAt = now.Add(2 * time.Minute)
	restarted.apply(fetchResult{Complete: true, Items: []notification{n}}, nil, n.UpdatedAt)
	if len(restarted.attention(n.UpdatedAt)) != 1 {
		t.Fatal("material reason change stayed suppressed")
	}
}
func TestSameTimestampConflictDoesNotAdvanceBaselineOrRemoveOthers(t *testing.T) {
	now := time.Now()
	s := newState(testConfig(), Identity{ID: 42})
	a := notification{ThreadID: "a", Repository: "acme/service", Reason: "mention", Unread: true, UpdatedAt: now}
	b := a
	b.ThreadID = "b"
	s.apply(fetchResult{Complete: true, Items: []notification{a, b}, LastModified: "old"}, nil, now)
	a.Reason = "assign"
	if !s.apply(fetchResult{Complete: true, Items: []notification{a}, LastModified: "new"}, nil, now.Add(time.Second)) {
		t.Fatal("conflict not scheduled")
	}
	if len(s.items) != 2 || s.lastModified != "" || !s.lastSuccess.Equal(now) {
		t.Fatal("conflict advanced complete state")
	}
}

func TestHandlingExpiresAndReasonRoundTripCreatesNewEpisode(t *testing.T) {
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	for _, mode := range []string{"expiry", "reason_round_trip", "unread_again"} {
		t.Run(mode, func(t *testing.T) {
			s := newState(testConfig(), Identity{ID: 42})
			s.apply(fetchResult{Complete: true}, nil, now)
			n := notification{ThreadID: "1", Repository: "acme/service", Reason: "mention", Unread: true, UpdatedAt: now}
			s.apply(fetchResult{Complete: true, Items: []notification{n}}, nil, now)
			i := s.ordered()[0]
			s.handled[i.ID] = handledEpisode{ID: i.ID, EpisodeID: i.EpisodeID, Reason: i.Reason, ObservedAt: now, UpdatedAt: now, HandledAt: now}
			s.apply(fetchResult{Complete: true, Items: []notification{n}}, nil, now)
			switch mode {
			case "expiry":
				s.pruneHandled(now.Add(8 * 24 * time.Hour))
				n.UpdatedAt = now.Add(8 * 24 * time.Hour)
			case "reason_round_trip":
				n.Reason = "comment"
				n.UpdatedAt = now.Add(time.Minute)
				s.apply(fetchResult{Complete: true, Items: []notification{n}}, nil, n.UpdatedAt)
				n.Reason = "mention"
				n.UpdatedAt = now.Add(2 * time.Minute)
			case "unread_again":
				s.apply(fetchResult{Complete: true}, nil, now.Add(time.Minute))
				n.UpdatedAt = now.Add(2 * time.Minute)
			}
			s.apply(fetchResult{Complete: true, Items: []notification{n}}, nil, n.UpdatedAt)
			if s.ordered()[0].Handled {
				t.Fatal("expired or different unread episode stayed handled")
			}
			if mode != "expiry" && len(s.attention(n.UpdatedAt)) != 1 {
				t.Fatal("new actionable episode did not arm")
			}
		})
	}
}
func TestItemsQueryRejectsNullLimit(t *testing.T) {
	s := newState(testConfig(), Identity{ID: 42})
	h := newHandler(&recordingHost{}, nil, time.Now)
	w := &worker{ref: protocol.InstanceRef{ID: "gh", Generation: 1}, state: s, now: time.Now}
	h.workers["gh"] = w
	if _, err := h.InvokeOperation(t.Context(), protocol.OperationRequest{Instance: w.ref, Operation: OperationItems, Payload: []byte(`{"limit":null}`)}); err == nil {
		t.Fatal("null limit accepted")
	}
}

func Test304RetainsCapacityTruncation(t *testing.T) {
	now := time.Now()
	s := newState(testConfig(), Identity{ID: 42})
	rows := make([]notification, 129)
	for i := range rows {
		rows[i] = notification{ThreadID: fmt.Sprint(i), Repository: "acme/service", Unread: true, UpdatedAt: now}
	}
	s.apply(fetchResult{Complete: true, Items: rows}, nil, now)
	s.apply(fetchResult{Complete: true, NotModified: true}, nil, now.Add(time.Minute))
	if !s.truncated || len(s.items) != 128 {
		t.Fatal("304 claimed bounded view was complete")
	}
}
func TestConflictPreventsEarlierUnreadRemoval(t *testing.T) {
	now := time.Now()
	s := newState(testConfig(), Identity{ID: 42})
	a := notification{ThreadID: "a", Repository: "acme/service", Reason: "mention", Unread: true, UpdatedAt: now}
	b := a
	b.ThreadID = "b"
	s.apply(fetchResult{Complete: true, Items: []notification{a, b}}, nil, now)
	a.Unread = false
	a.UpdatedAt = now.Add(time.Second)
	b.Reason = "assign"
	s.apply(fetchResult{Complete: true, Items: []notification{a, b}}, nil, now.Add(time.Second))
	if len(s.items) != 2 {
		t.Fatal("incomplete conflicting cycle removed unread record")
	}
}

func TestCheckpointBoundsAndIncompatibleRecovery(t *testing.T) {
	now := time.Now()
	s := newState(testConfig(), Identity{ID: 42})
	for index := range 140 {
		id := hashKey(fmt.Sprint(index))
		s.handled[id] = handledEpisode{ID: id, EpisodeID: hashKey(id, "episode"), Reason: "mention", ObservedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour), HandledAt: now.Add(-time.Duration(index) * time.Minute)}
	}
	expired := hashKey("expired")
	s.handled[expired] = handledEpisode{ID: expired, EpisodeID: hashKey(expired, "episode"), Reason: "mention", ObservedAt: now.Add(-8 * 24 * time.Hour), UpdatedAt: now.Add(-8 * 24 * time.Hour), HandledAt: now.Add(-8 * 24 * time.Hour)}
	raw := s.checkpointData(now)
	if len(s.handled) != 128 || len(raw) > 64<<10 || !s.checkpointTruncated {
		t.Fatalf("checkpoint bounds entries=%d bytes=%d truncated=%v", len(s.handled), len(raw), s.checkpointTruncated)
	}
	if _, ok := s.handled[expired]; ok {
		t.Fatal("expired marker retained")
	}
	wrongScope := newState(testConfig(), Identity{ID: 43})
	if wrongScope.restore(raw, now) != "checkpoint_ignored" || len(wrongScope.handled) != 0 {
		t.Fatal("cross-scope checkpoint restored")
	}
	if code := newState(testConfig(), Identity{ID: 42}).restore([]byte(`{"schema_version":99}`), now); code != "checkpoint_ignored" {
		t.Fatal("unknown checkpoint accepted")
	}
}
func TestInstanceCapRejectsBeforeIO(t *testing.T) {
	h := newHandler(&recordingHost{}, testClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("provider I/O before instance cap validation")
		return nil, nil
	}), time.Now)
	instances := make([]protocol.Instance, 9)
	for index := range instances {
		instances[index] = configuredInstance(fmt.Sprintf("gh-%d", index), 1)
	}
	if err := h.ReplaceInstances(t.Context(), instances); err == nil {
		t.Fatal("nine enabled instances accepted")
	}
}

func Test304ReconcilesExpiredAndEvictedHandlingWithoutNewEpisode(t *testing.T) {
	for _, mode := range []string{"expiry", "eviction"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			s := newState(testConfig(), Identity{ID: 42})
			s.apply(fetchResult{Complete: true}, nil, now)
			n := notification{ThreadID: "original", Repository: "acme/service", Reason: "mention", Unread: true, UpdatedAt: now}
			s.apply(fetchResult{Complete: true, Items: []notification{n}}, nil, now)
			i := s.ordered()[0]
			s.handled[i.ID] = handledEpisode{ID: i.ID, Reason: i.Reason, EpisodeID: i.EpisodeID, ObservedAt: i.ObservedAt, UpdatedAt: i.UpdatedAt, HandledAt: now}
			i.Handled = true
			s.items[i.ID] = i
			handled := s.items[i.ID]
			if mode == "expiry" {
				now = now.Add(7*24*time.Hour + time.Second)
			} else {
				for index := range 128 {
					id := hashKey(fmt.Sprint(index))
					s.handled[id] = handledEpisode{ID: id, EpisodeID: hashKey(id, "new"), Reason: "mention", ObservedAt: now, UpdatedAt: now, HandledAt: now.Add(time.Second)}
				}
				now = now.Add(2 * time.Second)
			}
			s.apply(fetchResult{Complete: true, NotModified: true}, nil, now)
			got := s.items[i.ID]
			if got.Handled || got.Revision <= handled.Revision {
				t.Fatalf("retained handling not reconciled: handled=%v revision=%d previous=%d", got.Handled, got.Revision, handled.Revision)
			}
			if got.EpisodeID != handled.EpisodeID || got.ObservedAt != handled.ObservedAt {
				t.Fatal("marker removal manufactured a new episode")
			}
			if len(s.attention(now)) != 1 {
				t.Fatal("expired local marker still suppresses attention")
			}
		})
	}
}
