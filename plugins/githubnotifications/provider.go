package githubnotifications

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

const requestTimeout = 10 * time.Second
const cycleTimeout = 30 * time.Second
const authorizationConcurrency = 4
const maxPollInterval = 15 * time.Minute
const maxRetryDelay = 24 * time.Hour
const apiVersion = "2026-03-10"

// Identity is the authenticated GitHub user. It is never emitted by plugin queries.
type Identity struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}
type sourceError struct {
	Code  string
	Delay time.Duration
}

func (e *sourceError) Error() string { return "GitHub notifications: " + e.Code }

// ErrorCode returns a content-free stable failure code.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if e, ok := errors.AsType[*sourceError](err); ok {
		return e.Code
	}
	return "api_unavailable"
}

// IsCredentialRejected distinguishes confirmed credential rejection from transport or throttling failures.
func IsCredentialRejected(err error) bool {
	code := ErrorCode(err)
	return code == "auth_required" || code == "notification_access_required"
}

type provider struct {
	client *http.Client
	token  string
}

func newProvider(client *http.Client, token string) *provider {
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	copy.Timeout = requestTimeout
	copy.Jar = nil
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &provider{client: &copy, token: token}
}
func validAPIURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "api.github.com" && u.User == nil && u.Fragment == "" && u.Opaque == "" && !strings.Contains(raw, "\\")
}
func validBrowserURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.Opaque == "" && !strings.ContainsAny(raw, "\\\r\n")
}
func responseDelay(h http.Header, now time.Time) time.Duration {
	var delay time.Duration
	for _, key := range []string{"Retry-After"} {
		v := h.Get(key)
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			delay = max(delay, time.Duration(n)*time.Second)
		} else if key == "Retry-After" {
			if t, err := http.ParseTime(v); err == nil {
				delay = max(delay, t.Sub(now))
			}
		}
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if n, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			delay = max(delay, time.Unix(n, 0).Sub(now))
		}
	}
	return min(max(delay, 0), maxRetryDelay)
}
func (p *provider) get(ctx context.Context, target, modified string) ([]byte, http.Header, int, error) {
	if !validAPIURL(target) {
		return nil, nil, 0, &sourceError{Code: "unsafe_api_url"}
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, 0, &sourceError{Code: "invalid_request"}
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "bsbctl-github-notifications")
	if modified != "" {
		req.Header.Set("If-Modified-Since", modified)
	}
	res, err := p.client.Do(req)
	if err != nil {
		return nil, nil, 0, &sourceError{Code: "api_unavailable"}
	}
	defer res.Body.Close()
	h := res.Header.Clone()
	delay := responseDelay(h, time.Now())
	body, err := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if err != nil {
		return nil, h, res.StatusCode, &sourceError{Code: "response_incomplete", Delay: delay}
	}
	if len(body) > 2<<20 {
		return nil, h, res.StatusCode, &sourceError{Code: "response_too_large", Delay: delay}
	}
	switch res.StatusCode {
	case 200, 304:
		return body, h, res.StatusCode, nil
	case 401:
		return nil, h, res.StatusCode, &sourceError{Code: "auth_required", Delay: delay}
	case 403, 429:
		if delay > 0 || res.StatusCode == 429 || strings.Contains(strings.ToLower(string(body)), "rate limit") {
			return nil, h, res.StatusCode, &sourceError{Code: "throttled", Delay: max(delay, time.Minute)}
		}
		return nil, h, res.StatusCode, &sourceError{Code: "notification_access_required"}
	case 404:
		return nil, h, res.StatusCode, &sourceError{Code: "repository_access_required"}
	default:
		return nil, h, res.StatusCode, &sourceError{Code: "api_unavailable", Delay: delay}
	}
}

// Authorize proves identity, notification capability, and access to each repository using read requests.
// The borrowed client's transport is preserved; redirects, cookies, and request time are restricted.
func Authorize(ctx context.Context, client *http.Client, token string, repositories []Repository) (Identity, error) {
	if err := ValidateRepositories(repositories); err != nil {
		return Identity{}, err
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return Identity{}, &sourceError{Code: "auth_required"}
	}
	ctx, cancel := context.WithTimeout(ctx, cycleTimeout)
	defer cancel()
	p := newProvider(client, token)
	body, _, code, err := p.get(ctx, "https://api.github.com/user", "")
	if err != nil {
		return Identity{}, err
	}
	var who Identity
	if code != 200 || json.Unmarshal(body, &who) != nil || who.ID <= 0 || who.Login == "" {
		return Identity{}, &sourceError{Code: "identity_invalid"}
	}
	body, _, code, err = p.get(ctx, "https://api.github.com/notifications?all=false&per_page=1", "")
	if err != nil {
		return Identity{}, err
	}
	var notifications []json.RawMessage
	if code != 200 || json.Unmarshal(body, &notifications) != nil || notifications == nil {
		return Identity{}, &sourceError{Code: "response_invalid"}
	}
	semaphore := make(chan struct{}, authorizationConcurrency)
	repositoryErrors := make([]error, len(repositories))
	var group sync.WaitGroup
	for index, repo := range repositories {
		group.Go(func() {
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				repositoryErrors[index] = ctx.Err()
				return
			}
			body, _, code, err := p.get(ctx, "https://api.github.com/repos/"+repo.Name, "")
			if err != nil {
				if code == http.StatusForbidden && ErrorCode(err) == "notification_access_required" {
					repositoryErrors[index] = &sourceError{Code: "repository_access_required"}
					return
				}
				repositoryErrors[index] = err
				return
			}
			var got struct {
				ID       int64  `json:"id"`
				FullName string `json:"full_name"`
			}
			if code != 200 || json.Unmarshal(body, &got) != nil || got.ID <= 0 || !strings.EqualFold(got.FullName, repo.Name) {
				repositoryErrors[index] = &sourceError{Code: "repository_access_required"}
			}
		})
	}
	group.Wait()
	for _, err := range repositoryErrors {
		if err != nil {
			return Identity{}, err
		}
	}
	return who, nil
}

type notification struct {
	ThreadID                                                            string
	Reason                                                              string
	Unread                                                              bool
	RepositoryID                                                        int64
	Repository, Alias, SubjectType, Title, SubjectURL, LatestCommentURL string
	UpdatedAt                                                           time.Time
}
type fetchResult struct {
	Items                    []notification
	Complete, NotModified    bool
	LastModified             string
	PollInterval, RetryAfter time.Duration
}

func nextPage(link string) (string, error) {
	var next string
	for part := range strings.SplitSeq(link, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		if len(fields) < 2 {
			if strings.TrimSpace(part) != "" {
				return "", &sourceError{Code: "pagination_invalid"}
			}
			continue
		}
		for _, v := range fields[1:] {
			if strings.TrimSpace(v) == `rel="next"` {
				target := strings.TrimSpace(fields[0])
				if next != "" || !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
					return "", &sourceError{Code: "pagination_invalid"}
				}
				next = strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">")
				if !validAPIURL(next) {
					return "", &sourceError{Code: "unsafe_api_url"}
				}
				u, _ := url.Parse(next)
				if u.Path != "/notifications" {
					return "", &sourceError{Code: "pagination_invalid"}
				}
			}
		}
	}
	return next, nil
}
func (p *provider) fetch(ctx context.Context, c Config, modified string) (fetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, cycleTimeout)
	defer cancel()
	result := fetchResult{}
	target := "https://api.github.com/notifications?all=false&per_page=100"
	seen := map[string]bool{}
	firstModified := ""
	allow := map[string]string{}
	for _, r := range c.Repositories {
		allow[strings.ToLower(r.Name)] = r.Alias
	}
	allRepositories := len(allow) == 0
	for page := range 10 {
		if seen[target] {
			return result, &sourceError{Code: "pagination_invalid"}
		}
		seen[target] = true
		condition := ""
		if page == 0 {
			condition = modified
		}
		body, h, code, err := p.get(ctx, target, condition)
		result.PollInterval = max(result.PollInterval, secondsHeader(h.Get("X-Poll-Interval")))
		result.RetryAfter = max(result.RetryAfter, responseDelay(h, time.Now()))
		if err != nil {
			if source, ok := errors.AsType[*sourceError](err); ok {
				result.RetryAfter = max(result.RetryAfter, source.Delay)
			}
			return result, err
		}
		if code == 304 {
			if page != 0 || modified == "" {
				return result, &sourceError{Code: "baseline_missing"}
			}
			result.Complete, result.NotModified = true, true
			return result, nil
		}
		if page == 0 {
			firstModified = h.Get("Last-Modified")
			if firstModified != "" {
				if _, err := http.ParseTime(firstModified); err != nil {
					firstModified = ""
				}
			}
		}
		var rows []struct {
			ID         string    `json:"id"`
			Unread     bool      `json:"unread"`
			Reason     string    `json:"reason"`
			UpdatedAt  time.Time `json:"updated_at"`
			Repository struct {
				ID       int64  `json:"id"`
				FullName string `json:"full_name"`
			} `json:"repository"`
			Subject struct {
				Title, Type, URL string
				LatestCommentURL string `json:"latest_comment_url"`
			} `json:"subject"`
		}
		if json.Unmarshal(body, &rows) != nil || rows == nil {
			return result, &sourceError{Code: "response_invalid"}
		}
		for _, r := range rows {
			if r.ID == "" || len(r.ID) > 128 || r.UpdatedAt.IsZero() || r.Repository.ID <= 0 || len(r.Reason) > 64 {
				return result, &sourceError{Code: "response_invalid"}
			}
			name := strings.ToLower(r.Repository.FullName)
			if !repositoryPattern.MatchString(r.Repository.FullName) || strings.HasSuffix(name, "/.") || strings.HasSuffix(name, "/..") {
				return result, &sourceError{Code: "response_invalid"}
			}
			alias, selected := allow[name]
			if !allRepositories && !selected {
				continue
			}
			if allRepositories {
				alias = repositoryDisplayAlias(r.Repository.FullName)
			}
			result.Items = append(result.Items, notification{ThreadID: r.ID, Reason: publicReason(r.Reason), Unread: r.Unread, UpdatedAt: r.UpdatedAt, RepositoryID: r.Repository.ID, Repository: r.Repository.FullName, Alias: alias, SubjectType: safeText(r.Subject.Type, 32), Title: safeText(r.Subject.Title, 160), SubjectURL: boundedURL(r.Subject.URL), LatestCommentURL: boundedURL(r.Subject.LatestCommentURL)})
		}
		target, err = nextPage(h.Get("Link"))
		if err != nil {
			return result, err
		}
		if target == "" {
			result.Complete = true
			result.LastModified = firstModified
			return result, nil
		}
	}
	return result, &sourceError{Code: "page_limit"}
}

func repositoryDisplayAlias(repository string) string {
	name := repository
	if slash := strings.LastIndexByte(repository, '/'); slash >= 0 {
		name = repository[slash+1:]
	}
	alias := safeText(name, 8)
	if !publicAlias(alias) {
		return "GITHUB"
	}
	return alias
}
func secondsHeader(v string) time.Duration {
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 0 {
		return 0
	}
	return min(time.Duration(n)*time.Second, maxPollInterval)
}
func safeText(s string, limit int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= limit {
			break
		}
		if r < 32 || r > 126 {
			r = '?'
		}
		b.WriteRune(r)
	}
	return b.String()
}
func boundedURL(s string) string {
	if len(s) > 2048 {
		return ""
	}
	return s
}

// resolveSubject prefers the latest comment, then the exact subject. No inbox fallback.
func (p *provider) resolveSubject(ctx context.Context, n notification) (target, label string, err error) {
	for _, apiURL := range []string{n.LatestCommentURL, n.SubjectURL} {
		if apiURL == "" {
			continue
		}
		if !repositoryAPIURL(apiURL, n.Repository) {
			return "", "", &sourceError{Code: "unsafe_api_url"}
		}
		body, _, _, readErr := p.get(ctx, apiURL, "")
		if readErr != nil {
			if ctx.Err() != nil {
				return "", "", readErr
			}
			continue
		}
		var wire struct {
			HTMLURL string `json:"html_url"`
		}
		if json.Unmarshal(body, &wire) != nil || wire.HTMLURL == "" {
			continue
		}
		u, _ := url.Parse(wire.HTMLURL)
		if !validBrowserURL(wire.HTMLURL) || (!strings.HasPrefix(strings.ToLower(u.Path), strings.ToLower("/"+n.Repository+"/")) || path.Clean(u.Path) != u.Path) {
			return "", "", &sourceError{Code: "unsafe_browser_url"}
		}
		return wire.HTMLURL, "Open", nil
	}
	return "", "", &sourceError{Code: "target_unavailable"}
}
func repositoryAPIURL(raw, repo string) bool {
	if !validAPIURL(raw) {
		return false
	}
	u, _ := url.Parse(raw)
	return strings.HasPrefix(strings.ToLower(u.Path), strings.ToLower("/repos/"+repo+"/")) && path.Clean(u.Path) == u.Path
}

// markRead sends one thread-scoped write. Any transport ambiguity requires reads,
// never an automatic retry: newer activity may have arrived on the same thread.
func (p *provider) markRead(ctx context.Context, n notification) error {
	if n.ThreadID == "" || len(n.ThreadID) > 128 {
		return &sourceError{Code: "invalid_thread"}
	}
	for _, r := range n.ThreadID {
		if r < '0' || r > '9' {
			return &sourceError{Code: "invalid_thread"}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, "https://api.github.com/notifications/threads/"+n.ThreadID, nil)
	if err != nil {
		return &sourceError{Code: "invalid_request"}
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "bsbctl-github-notifications")
	res, err := p.client.Do(req)
	if err != nil {
		return &sourceError{Code: "read_unknown"}
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case 205, 304:
		return nil
	case 401:
		return &sourceError{Code: "auth_required"}
	case 403:
		if responseDelay(res.Header, time.Now()) > 0 {
			return &sourceError{Code: "throttled"}
		}
		return &sourceError{Code: "notification_access_required"}
	case 404:
		return &sourceError{Code: "target_unavailable"}
	case 429:
		return &sourceError{Code: "throttled"}
	default:
		return &sourceError{Code: "read_unknown"}
	}
}
