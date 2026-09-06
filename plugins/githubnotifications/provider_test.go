package githubnotifications

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestAuthorizeUsesCapabilityNotTokenPrefixAndChecksRepositories(t *testing.T) {
	for _, scope := range []string{"notifications", "repo"} {
		t.Run(scope, func(t *testing.T) {
			calls := []string{}
			client := testClient(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.URL.Path)
				if r.Header.Get("X-GitHub-Api-Version") != "2026-03-10" {
					t.Fatal("wrong API version")
				}
				res, ok := authorizeResponse(r)
				if !ok {
					t.Fatal("unexpected authorization request")
				}
				res.Header = http.Header{"X-Oauth-Scopes": {scope}}
				return res, nil
			})
			who, err := Authorize(t.Context(), client, "opaque-credential", testConfig().Repositories)
			if err != nil || who.ID != 42 || strings.Join(calls, ",") != "/user,/notifications,/repos/acme/service" {
				t.Fatalf("capability validation = %+v %v %v", who, err, calls)
			}
		})
	}
}

func TestAuthorizeChecksRepositoriesWithBoundedConcurrency(t *testing.T) {
	repositories := make([]Repository, 8)
	for index := range repositories {
		repositories[index] = Repository{Name: fmt.Sprintf("acme/service-%d", index), Alias: fmt.Sprintf("S%d", index)}
	}
	started := make(chan struct{}, len(repositories))
	release := make(chan struct{})
	var active, maximum atomic.Int32
	var requestedMu sync.Mutex
	requested := map[string]int{}
	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/user":
			return response(200, `{"id":42,"login":"test-user"}`, nil), nil
		case "/notifications":
			return response(200, `[]`, nil), nil
		}
		requestedMu.Lock()
		requested[r.URL.Path]++
		requestedMu.Unlock()
		current := active.Add(1)
		for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			active.Add(-1)
			return nil, r.Context().Err()
		}
		active.Add(-1)
		name := strings.TrimPrefix(r.URL.Path, "/repos/")
		return response(200, fmt.Sprintf(`{"id":7,"full_name":%q}`, name), nil), nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := Authorize(ctx, client, "opaque-credential", repositories)
		result <- err
	}()
	for range 4 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("repository authorization remained serial")
		}
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 4 {
		t.Fatalf("maximum repository authorization concurrency = %d, want 4", maximum.Load())
	}
	requestedMu.Lock()
	defer requestedMu.Unlock()
	if len(requested) != len(repositories) {
		t.Fatalf("authorized repository paths = %#v", requested)
	}
	for _, repository := range repositories {
		if requested["/repos/"+repository.Name] != 1 {
			t.Fatalf("repository %q authorization calls = %d", repository.Name, requested["/repos/"+repository.Name])
		}
	}
}

func TestAuthorizeAllRepositoriesNeedsOnlyIdentityAndNotificationCapability(t *testing.T) {
	var calls []string
	client := testClient(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Path)
		response, ok := authorizeResponse(r)
		if !ok {
			t.Fatalf("unexpected authorization request %s", r.URL.Path)
		}
		response.Header = http.Header{"X-Oauth-Scopes": {"notifications"}}
		return response, nil
	})
	who, err := Authorize(t.Context(), client, "opaque-credential", nil)
	if err != nil || who.ID != 42 || strings.Join(calls, ",") != "/user,/notifications" {
		t.Fatalf("all repository authorization = %+v %v %v", who, err, calls)
	}
}
func TestAuthorizationFailuresAreContentFreeAndClassified(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		headers  http.Header
		want     string
		rejected bool
	}{{"expired", 401, nil, "auth_required", true}, {"unsupported", 403, http.Header{"X-Poll-Interval": {"60"}}, "notification_access_required", true}, {"secondary_limit", 403, http.Header{"Retry-After": {"600"}}, "throttled", false}, {"unavailable", 503, nil, "api_unavailable", false}} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(func(r *http.Request) (*http.Response, error) {
				return response(tc.status, `{"message":"private-token private-title"}`, tc.headers), nil
			})
			_, err := Authorize(t.Context(), client, "private-token", testConfig().Repositories)
			if ErrorCode(err) != tc.want || IsCredentialRejected(err) != tc.rejected || strings.Contains(err.Error(), "private") {
				t.Fatalf("error classification %v", err)
			}
		})
	}
}
func TestProviderBodyPageAndParseLimits(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		link             bool
	}{{"oversize", strings.Repeat("x", (2<<20)+1), "response_too_large", false}, {"malformed", "[", "response_invalid", false}, {"missing_array", "null", "response_invalid", false}, {"page_cap", "[]", "page_limit", true}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := newProvider(testClient(func(r *http.Request) (*http.Response, error) {
				calls++
				headers := http.Header{}
				if tc.link {
					headers.Set("Link", fmt.Sprintf(`<https://api.github.com/notifications?page=%d>; rel="next"`, calls+1))
				}
				return response(200, tc.body, headers), nil
			}), "token")
			r, err := p.fetch(t.Context(), testConfig(), "")
			if r.Complete || ErrorCode(err) != tc.want {
				t.Fatalf("bounds: %+v %v", r, err)
			}
			if tc.link && calls != 10 {
				t.Fatalf("page bound %d", calls)
			}
		})
	}
}
func Test304AndThrottlingFreshnessContracts(t *testing.T) {
	now := time.Now()
	s := newState(testConfig(), Identity{ID: 42})
	s.apply(fetchResult{Complete: true, PollInterval: 120 * time.Second, LastModified: "old"}, nil, now)
	deadline := s.freshUntil
	s.apply(fetchResult{RetryAfter: time.Hour}, &sourceError{Code: "throttled", Delay: time.Hour}, now.Add(time.Minute))
	if !s.freshUntil.Equal(deadline) || !s.lastSuccess.Equal(now) {
		t.Fatal("throttling extended dataset freshness")
	}
	p := newProvider(testClient(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("If-Modified-Since") != "old" {
			t.Fatal("missing conditional validator")
		}
		return response(304, "", http.Header{"X-Poll-Interval": {"180"}}), nil
	}), "token")
	r, err := p.fetch(t.Context(), testConfig(), "old")
	if err != nil || !r.NotModified {
		t.Fatalf("valid 304: %+v %v", r, err)
	}
	s.apply(r, nil, now.Add(2*time.Minute))
	if s.lastModified != "old" || !s.lastSuccess.Equal(now.Add(2*time.Minute)) || s.effectiveInterval != 180*time.Second {
		t.Fatal("304 did not refresh baseline")
	}
}

func TestProviderSchedulingHintsAreBoundedAndRecover(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	headers := http.Header{
		"Retry-After":           {"2147483647"},
		"X-RateLimit-Remaining": {"0"},
		"X-RateLimit-Reset":     {fmt.Sprint(now.AddDate(10, 0, 0).Unix())},
	}
	if got := responseDelay(headers, now); got != 24*time.Hour {
		t.Fatalf("bounded retry delay = %s, want 24h", got)
	}
	if got := secondsHeader("2147483647"); got != 15*time.Minute {
		t.Fatalf("bounded poll interval = %s, want 15m", got)
	}
	s := newState(testConfig(), Identity{ID: 42})
	s.apply(fetchResult{Complete: true, PollInterval: 15 * time.Minute}, nil, now)
	s.apply(fetchResult{Complete: true, NotModified: true, PollInterval: time.Minute}, nil, now.Add(time.Minute))
	if s.serverInterval != time.Minute || s.effectiveInterval != time.Minute {
		t.Fatalf("successful lower provider interval did not recover: server=%s effective=%s", s.serverInterval, s.effectiveInterval)
	}
}
func TestSubjectResolutionNeverOpensUntrustedTargets(t *testing.T) {
	for _, tc := range []struct{ name, subjectType, apiURL, htmlURL, wantLabel, wantCode string }{{"unsupported", "Discussion", "https://evil.example", "", "", "unsafe_api_url"}, {"cross_host_api", "Issue", "https://evil.example/issue", "", "", "unsafe_api_url"}, {"credential_url", "Issue", "https://token@api.github.com/repos/acme/service/issues/1", "", "", "unsafe_api_url"}, {"cross_host_browser", "Issue", "https://api.github.com/repos/acme/service/issues/1", "https://evil.example/issue", "", "unsafe_browser_url"}, {"unsafe_scheme", "Issue", "https://api.github.com/repos/acme/service/issues/1", "javascript:alert(1)", "", "unsafe_browser_url"}, {"supported", "PullRequest", "https://api.github.com/repos/acme/service/pulls/1", "https://github.com/acme/service/pull/1", "Open", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := newProvider(testClient(func(r *http.Request) (*http.Response, error) {
				calls++
				return response(200, fmt.Sprintf(`{"html_url":%q}`, tc.htmlURL), nil), nil
			}), "token")
			target, label, err := p.resolveSubject(t.Context(), notification{SubjectType: tc.subjectType, SubjectURL: tc.apiURL, Repository: "acme/service"})
			if label != tc.wantLabel || ErrorCode(err) != tc.wantCode {
				t.Fatalf("resolution = %q %q %v", target, label, err)
			}
			if tc.wantCode == "unsafe_api_url" && calls != 0 {
				t.Fatal("requested malicious subject URL")
			}
			if label == "Open inbox" && target != "https://github.com/notifications" {
				t.Fatal("incorrect inbox fallback")
			}
		})
	}
}
func TestReadPermissionFailureKeepsSelectedPageRecords(t *testing.T) {
	calls := 0
	p := newProvider(testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response(200, "["+threadJSON("1", "mention", "2026-09-05T10:00:00Z")+"]", http.Header{"Link": {`<https://api.github.com/notifications?page=2>; rel="next"`}}), nil
		}
		return response(403, `{"message":"forbidden"}`, nil), nil
	}), "token")
	r, err := p.fetch(t.Context(), testConfig(), "")
	if len(r.Items) != 1 || r.Complete || !IsCredentialRejected(err) {
		t.Fatalf("partial permission coverage: %+v %v", r, err)
	}
}

func TestRequestAndCycleDeadlinesCancelProviderWork(t *testing.T) {
	for _, tc := range []struct {
		name            string
		pageDelay, want time.Duration
	}{{"request", 20 * time.Second, 10 * time.Second}, {"cycle", 9 * time.Second, 30 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				p := newProvider(testClient(func(r *http.Request) (*http.Response, error) {
					calls++
					select {
					case <-r.Context().Done():
						return nil, r.Context().Err()
					case <-time.After(tc.pageDelay):
						return response(200, `[]`, http.Header{"Link": {fmt.Sprintf(`<https://api.github.com/notifications?page=%d>; rel="next"`, calls+1)}}), nil
					}
				}), "token")
				started := time.Now()
				result, err := p.fetch(t.Context(), testConfig(), "")
				if err == nil || result.Complete || time.Since(started) != tc.want {
					t.Fatalf("deadline result complete=%v err=%v elapsed=%s", result.Complete, err, time.Since(started))
				}
			})
		})
	}
}

func TestRepositoryForbiddenDoesNotRejectNotificationCredential(t *testing.T) {
	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/repos/acme/service" {
			return response(403, `{"message":"forbidden"}`, nil), nil
		}
		if res, ok := authorizeResponse(r); ok {
			return res, nil
		}
		t.Fatal("unexpected request")
		return nil, nil
	})
	_, err := Authorize(t.Context(), client, "valid-notification-token", testConfig().Repositories)
	if ErrorCode(err) != "repository_access_required" || IsCredentialRejected(err) {
		t.Fatalf("repository error = %v, rejected=%v", err, IsCredentialRejected(err))
	}
}
