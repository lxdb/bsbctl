package codexquota

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAPISourceReadsAuthAndNormalizesWindowsByDuration(t *testing.T) {
	credentialHome := t.TempDir()
	configurationHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(credentialHome, "auth.json"), []byte(`{
		"tokens":{"access_token":"access-secret","refresh_token":"refresh-secret","id_token":"id-secret","account_id":"acct-secret"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer access-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := request.Header.Get("ChatGPT-Account-Id"); got != "acct-secret" {
			t.Errorf("ChatGPT-Account-Id = %q", got)
		}
		if got := request.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		if got := request.Header.Get("User-Agent"); !strings.HasPrefix(got, "bsbctl-codex-quota/") {
			t.Errorf("User-Agent = %q", got)
		}
		_, _ = fmt.Fprint(writer, `{
			"plan_type":"pro",
			"rate_limit":{
				"primary_window":{"used_percent":76,"reset_at":2000001000,"limit_window_seconds":604800},
				"secondary_window":{"used_percent":38,"reset_at":2000000000,"limit_window_seconds":18000}
			}
		}`)
	}))
	defer server.Close()
	if err := os.WriteFile(filepath.Join(configurationHome, "config.toml"), []byte(`chatgpt_base_url = "`+server.URL+`/backend-api/"`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := defaultConfig(credentialHome)
	config.CredentialsHome = credentialHome
	config.ConfigurationHome = configurationHome
	source := newAPISource(config, server.Client())

	snapshot, err := source.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Windows) != 2 {
		t.Fatalf("windows = %#v", snapshot.Windows)
	}
	if got := snapshot.Windows[0]; got.Duration != 5*time.Hour || got.RemainingPercent != 62 || got.UsedPercent != 38 {
		t.Fatalf("short window = %#v", got)
	}
	if got := snapshot.Windows[1]; got.Duration != 7*24*time.Hour || got.RemainingPercent != 24 || got.UsedPercent != 76 {
		t.Fatalf("long window = %#v", got)
	}
}

func TestAPISourceUsesNonBackendPathAndBoundsResponse(t *testing.T) {
	credentialHome := t.TempDir()
	configurationHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(credentialHome, "auth.json"), []byte(`{"tokens":{"access_token":"token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/codex/usage" {
			t.Errorf("path = %q", request.URL.Path)
		}
		_, _ = writer.Write(make([]byte, maxUsageResponseBytes+1))
	}))
	defer server.Close()
	if err := os.WriteFile(filepath.Join(configurationHome, "config.toml"), []byte(`chatgpt_base_url = "`+server.URL+`"`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := defaultConfig(credentialHome)
	config.CredentialsHome = credentialHome
	config.ConfigurationHome = configurationHome
	_, err := newAPISource(config, server.Client()).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "response_too_large") {
		t.Fatalf("Fetch error = %v", err)
	}
}

func TestAPISourceErrorsAreRedacted(t *testing.T) {
	credentialHome := t.TempDir()
	configurationHome := t.TempDir()
	secret := "do-not-leak-this-token"
	if err := os.WriteFile(filepath.Join(credentialHome, "auth.json"), []byte(`{"tokens":{"access_token":"`+secret+`","account_id":"private-account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(writer, secret+" /Users/private raw provider response")
	}))
	defer server.Close()
	if err := os.WriteFile(filepath.Join(configurationHome, "config.toml"), []byte(`chatgpt_base_url = "`+server.URL+`/backend-api"`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := defaultConfig(credentialHome)
	config.CredentialsHome = credentialHome
	config.ConfigurationHome = configurationHome
	_, err := newAPISource(config, server.Client()).Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch unexpectedly succeeded")
	}
	for _, forbidden := range []string{secret, "private-account", credentialHome, "/Users/private", server.URL} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("unsafe error %q contains %q", err, forbidden)
		}
	}
	if !strings.Contains(err.Error(), "auth_unavailable") {
		t.Fatalf("error = %q, want safe auth code", err)
	}
}

func TestResolveBaseURLRejectsNonLoopbackHTTP(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`chatgpt_base_url = "http://example.com/backend-api"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveBaseURL(home); err == nil || !strings.Contains(err.Error(), "insecure_endpoint") {
		t.Fatalf("resolveBaseURL error = %v", err)
	}
}
