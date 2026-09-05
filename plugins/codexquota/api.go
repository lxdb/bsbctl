package codexquota

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxAuthBytes          = 64 << 10
	maxUsageResponseBytes = 64 << 10
	requestTimeout        = 10 * time.Second
)

type quotaSource interface {
	Fetch(context.Context) (Snapshot, error)
}

type sourceError struct{ code string }

func (e *sourceError) Error() string { return e.code }

type apiSource struct {
	config Config
	client *http.Client
	now    func() time.Time
}

type credentialDocument struct {
	Tokens struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

func newAPISource(config Config, client *http.Client) *apiSource {
	if client == nil {
		client = http.DefaultClient
	}
	copyClient := *client
	copyClient.Timeout = requestTimeout
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errors.New("redirects_disabled")
	}
	return &apiSource{config: config, client: &copyClient, now: time.Now}
}

func (s *apiSource) Fetch(ctx context.Context) (Snapshot, error) {
	credentials, err := loadCredentials(s.config.CredentialsHome)
	if err != nil {
		return Snapshot{}, err
	}
	base, err := resolveBaseURL(s.config.ConfigurationHome)
	if err != nil {
		return Snapshot{}, err
	}
	path := "/api/codex/usage"
	if strings.Contains(base.Path, "/backend-api") {
		path = "/wham/usage"
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return Snapshot{}, &sourceError{code: "request_invalid"}
	}
	request.Header.Set("Authorization", "Bearer "+credentials.Tokens.AccessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "bsbctl-codex-quota/"+PluginVersion)
	if credentials.Tokens.AccountID != "" {
		request.Header.Set("ChatGPT-Account-Id", credentials.Tokens.AccountID)
	}
	response, err := s.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return Snapshot{}, ctx.Err()
		}
		return Snapshot{}, &sourceError{code: "api_unavailable"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return Snapshot{}, &sourceError{code: "auth_unavailable"}
		case http.StatusTooManyRequests:
			return Snapshot{}, &sourceError{code: "api_rate_limited"}
		default:
			return Snapshot{}, &sourceError{code: "api_unavailable"}
		}
	}
	data, err := readBounded(response.Body, maxUsageResponseBytes)
	if err != nil {
		return Snapshot{}, err
	}
	decoded := usageResponse{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return Snapshot{}, &sourceError{code: "response_invalid"}
	}
	return normalizeUsage(decoded, s.now())
}

func loadCredentials(home string) (credentialDocument, error) {
	file, err := os.Open(filepath.Join(home, "auth.json"))
	if err != nil {
		return credentialDocument{}, &sourceError{code: "auth_unavailable"}
	}
	defer file.Close()
	data, err := readBounded(file, maxAuthBytes)
	if err != nil {
		return credentialDocument{}, &sourceError{code: "auth_invalid"}
	}
	result := credentialDocument{}
	if err := json.Unmarshal(data, &result); err != nil || strings.TrimSpace(result.Tokens.AccessToken) == "" {
		return credentialDocument{}, &sourceError{code: "auth_invalid"}
	}
	return result, nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, &sourceError{code: "read_failed"}
	}
	if int64(len(data)) > limit {
		return nil, &sourceError{code: "response_too_large"}
	}
	return data, nil
}

func resolveBaseURL(configurationHome string) (*url.URL, error) {
	base := "https://chatgpt.com/backend-api"
	file, err := os.Open(filepath.Join(configurationHome, "config.toml"))
	if err == nil {
		scanner := bufio.NewScanner(io.LimitReader(file, maxAuthBytes))
		for scanner.Scan() {
			line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 || strings.TrimSpace(parts[0]) != "chatgpt_base_url" {
				continue
			}
			if value, unquoteErr := strconv.Unquote(strings.TrimSpace(parts[1])); unquoteErr == nil && strings.TrimSpace(value) != "" {
				base = value
			}
			break
		}
		_ = file.Close()
	}
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, &sourceError{code: "endpoint_invalid"}
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return nil, &sourceError{code: "insecure_endpoint"}
	}
	if (parsed.Hostname() == "chatgpt.com" || parsed.Hostname() == "chat.openai.com") && !strings.Contains(parsed.Path, "/backend-api") {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/backend-api"
	}
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sourceErrorCode(err error) string {
	if safe, ok := errors.AsType[*sourceError](err); ok {
		return safe.code
	}
	return "api_unavailable"
}

func safeFailureEvent(err error) string {
	code := sourceErrorCode(err)
	if strings.HasPrefix(code, "auth_") {
		return "codex_quota_auth_unavailable"
	}
	return "codex_quota_api_unavailable"
}
