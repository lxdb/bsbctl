package slack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type rewriteTransport struct {
	target string
	base   http.RoundTripper
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	u := *req.URL
	target, _ := http.NewRequest(http.MethodGet, r.target, nil)
	u.Scheme, u.Host = target.URL.Scheme, target.URL.Host
	clone.URL = &u
	return r.base.RoundTrip(clone)
}
func fixtureClient(t *testing.T, handler http.HandlerFunc) *slackClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return newSlackClient(&http.Client{Transport: rewriteTransport{server.URL, server.Client().Transport}})
}
func TestClientDeadlineRedirectAndThrottle(t *testing.T) {
	t.Run("caller deadline wins", func(t *testing.T) {
		started := make(chan struct{})
		client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() })
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		_, err := client.openSocket(ctx, "app-canary")
		<-started
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline lost: %v", err)
		}
	})
	t.Run("redirect rejected", func(t *testing.T) {
		calls := 0
		client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			http.Redirect(w, r, "https://slack.com/steal-canary", http.StatusFound)
		})
		_, err := client.openSocket(t.Context(), "app-canary")
		if err == nil || calls != 1 || strings.Contains(err.Error(), "canary") {
			t.Fatalf("redirect followed/leaked: calls=%d err=%v", calls, err)
		}
	})
	t.Run("retry after", func(t *testing.T) {
		client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer app-canary" || r.URL.Path != "/api/apps.connections.open" {
				t.Error("wrong app boundary")
			}
			w.Header().Set("Retry-After", "601")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, "canary")
		})
		_, err := client.openSocket(t.Context(), "app-canary")
		e, ok := errors.AsType[*sourceError](err)
		if !ok || e.code != "throttled" || e.retryAfter != 601*time.Second {
			t.Fatalf("throttle=%v", err)
		}
	})
}

func TestClientResolvesFullChannelNameWithUserToken(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/conversations.info" || r.Header.Get("Authorization") != "Bearer user-canary" {
			t.Fatalf("channel metadata request = %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("channel") != "C123" {
			t.Fatalf("channel form = %v, %v", r.Form, err)
		}
		_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"C123","name":"engineering-platform"}}`)
	})
	name, err := client.conversationName(t.Context(), "user-canary", "C123")
	if err != nil || name != "engineering-platform" {
		t.Fatalf("channel name = %q, %v", name, err)
	}
}

func TestClientRejectsUnsafeOrUnauthorizedChannelMetadata(t *testing.T) {
	for name, response := range map[string]string{
		"missing scope": `{"ok":false,"error":"missing_scope"}`,
		"wrong channel": `{"ok":true,"channel":{"id":"C999","name":"private-canary"}}`,
		"blank name":    `{"ok":true,"channel":{"id":"C123","name":""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			client := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, response) })
			value, err := client.conversationName(t.Context(), "user-canary", "C123")
			if err == nil || value != "" || strings.Contains(err.Error(), "canary") {
				t.Fatalf("unsafe metadata = %q, %v", value, err)
			}
		})
	}
}
