package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const requestTimeout = 10 * time.Second

// sourceError intentionally never wraps provider text, credentials, or ticket URLs.
type sourceError struct {
	code       string
	retryAfter time.Duration
}

func (e *sourceError) Error() string { return "Slack source: " + e.code }

type slackClient struct{ http *http.Client }

func newSlackClient(client *http.Client) *slackClient {
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	copy.Timeout = requestTimeout
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect rejected") }
	return &slackClient{http: &copy}
}

func (c *slackClient) openSocket(ctx context.Context, token string) (string, error) {
	var response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		URL   string `json:"url"`
	}
	if _, err := c.call(ctx, "apps.connections.open", token, &response); err != nil {
		return "", err
	}
	if !response.OK {
		return "", providerError(response.Error)
	}
	u, err := url.Parse(response.URL)
	if err != nil || u.Scheme != "wss" || u.User != nil || u.Port() != "" || !strings.HasSuffix(u.Hostname(), ".slack.com") || len(response.URL) > 8192 {
		return "", &sourceError{code: "invalid_response"}
	}
	return response.URL, nil
}

func (c *slackClient) conversationName(ctx context.Context, token, channelID string) (string, error) {
	if !validID(channelID, "CG") {
		return "", &sourceError{code: "invalid_response"}
	}
	var response struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Channel struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"channel"`
	}
	if _, err := c.callForm(ctx, "conversations.info", token, url.Values{"channel": {channelID}}, &response); err != nil {
		return "", err
	}
	if !response.OK {
		return "", providerError(response.Error)
	}
	if response.Channel.ID != channelID || !validLabel(response.Channel.Name) {
		return "", &sourceError{code: "invalid_response"}
	}
	return response.Channel.Name, nil
}

func (c *slackClient) call(ctx context.Context, method, token string, target any) (http.Header, error) {
	return c.callForm(ctx, method, token, nil, target)
}

func (c *slackClient) callForm(ctx context.Context, method, token string, form url.Values, target any) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/"+method, body)
	if err != nil {
		return nil, &sourceError{code: "request_failed"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &sourceError{code: "request_failed"}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		return nil, &sourceError{code: "throttled", retryAfter: parseRetryAfter(response.Header.Get("Retry-After"), time.Now())}
	}
	if response.StatusCode == 401 || response.StatusCode == 403 {
		return nil, &sourceError{code: "auth_required"}
	}
	if response.StatusCode != http.StatusOK {
		return nil, &sourceError{code: "request_failed"}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || len(raw) > 64*1024 || json.Unmarshal(raw, target) != nil {
		return nil, &sourceError{code: "invalid_response"}
	}
	return response.Header, nil
}

func providerError(code string) error {
	switch code {
	case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive", "not_allowed_token_type":
		return &sourceError{code: "auth_required"}
	case "missing_scope":
		return &sourceError{code: "missing_scope"}
	case "ratelimited":
		return &sourceError{code: "throttled"}
	default:
		return &sourceError{code: "request_failed"}
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	// A day bounds hostile headers while preserving ordinary provider cooldowns.
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Duration(min(seconds, 86400)) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		return min(max(deadline.Sub(now), 0), 24*time.Hour)
	}
	return 0
}
