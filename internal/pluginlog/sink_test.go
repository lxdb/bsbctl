package pluginlog

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestSinkWritesStructuredRecord(t *testing.T) {
	var output bytes.Buffer
	sink := New(&output, Options{QueueCapacity: 4, Now: func() time.Time {
		return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	}})
	sink.Log("plugin.one", protocol.LogNotification{
		Level: protocol.LogLevelWarn, Event: "sync.delayed", Instance: protocol.InstanceRef{ID: "app", Generation: 1}, Message: "later",
		Fields: map[string]string{"attempt": "2"},
	})
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("records = %d, want 1: %q", len(lines), output.String())
	}
	var structured map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &structured); err != nil {
		t.Fatalf("structured json: %v", err)
	}
	if structured["plugin_id"] != "plugin.one" || structured["event"] != "sync.delayed" || structured["level"] != "warn" {
		t.Fatalf("structured record = %#v", structured)
	}
}

func TestSinkRedactsStructuredCredentialShapes(t *testing.T) {
	const (
		fieldToken           = "structured-token-canary"
		fieldSecret          = "structured-secret-canary"
		fieldPassword        = "structured-password-canary"
		fieldAuth            = "structured-authorization-canary"
		fieldAPIKey          = "structured-api-key-canary"
		fieldCredential      = "structured-credential-canary"
		fieldCookie          = "structured-cookie-canary"
		messageAuthorization = "structured-authorization-message-canary"
	)
	var output bytes.Buffer
	sink := New(&output, Options{})
	sink.Log("plugin.one", protocol.LogNotification{
		Level: protocol.LogLevelWarn, Event: "sync.failed",
		Message: "Authorization: ApiKey " + messageAuthorization + "\x01",
		Fields: map[string]string{
			"access_token":   fieldToken,
			"client_secret":  fieldSecret,
			"password":       fieldPassword,
			"authorization":  fieldAuth,
			"api_key":        fieldAPIKey,
			"credential":     fieldCredential,
			"session_cookie": fieldCookie,
			"attempt":        "2",
		},
	})
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{fieldToken, fieldSecret, fieldPassword, fieldAuth, fieldAPIKey, fieldCredential, fieldCookie, messageAuthorization} {
		if strings.Contains(output.String(), canary) {
			t.Fatalf("persisted credential canary %q: %s", canary, output.String())
		}
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("records = %d, want 1: %q", len(lines), output.String())
	}
	var structured struct {
		Message string            `json:"message"`
		Fields  map[string]string `json:"fields"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &structured); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"access_token", "client_secret", "password", "authorization", "api_key", "credential", "session_cookie"} {
		if structured.Fields[key] != "[REDACTED]" {
			t.Fatalf("structured field %q = %q", key, structured.Fields[key])
		}
	}
	if structured.Fields["attempt"] != "2" || hasControl(structured.Message) {
		t.Fatalf("structured record = %#v", structured)
	}
}

func TestSinkUpdatesExactDeliveredSecretRedactionAcrossApplyLifecycle(t *testing.T) {
	const oldSecret = "old-delivered-secret-canary"
	const newSecret = "new-delivered-secret-canary"
	var output bytes.Buffer
	sink := New(&output, Options{})
	sink.MergeSecrets(map[string][]string{"plugin.one": {oldSecret}})
	sink.MergeSecrets(map[string][]string{"plugin.one": {newSecret}})
	sink.Log("plugin.one", protocol.LogNotification{
		Level: protocol.LogLevelWarn, Event: oldSecret, Message: oldSecret + " " + newSecret,
		Fields: map[string]string{oldSecret: oldSecret, "note": newSecret},
	})
	sink.ReplaceSecrets(map[string][]string{"plugin.one": {newSecret}})
	sink.Log("plugin.one", protocol.LogNotification{
		Level: protocol.LogLevelInfo, Event: "after.apply", Message: oldSecret + " " + newSecret,
		Fields: map[string]string{"note": oldSecret + " " + newSecret},
	})
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("records = %d, want 2", len(lines))
	}
	var first, second record
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	firstData, _ := json.Marshal(first)
	if strings.Contains(string(firstData), oldSecret) || strings.Contains(string(firstData), newSecret) {
		t.Fatalf("candidate delivered secret escaped before apply completed: %s", firstData)
	}
	if !strings.Contains(second.Message, oldSecret) || !strings.Contains(second.Fields["note"], oldSecret) {
		t.Fatalf("superseded secret was retained after successful apply: %#v", second)
	}
	if strings.Contains(second.Message, newSecret) || strings.Contains(second.Fields["note"], newSecret) {
		t.Fatalf("currently delivered secret escaped after apply: %#v", second)
	}
}

func hasControl(value string) bool {
	for _, current := range value {
		if unicode.IsControl(current) {
			return true
		}
	}
	return false
}

func TestSinkProducersNeverBlockOnSlowWriterAndDropsAreObservable(t *testing.T) {
	writer := &blockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	sink := New(writer, Options{QueueCapacity: 1})
	sink.Log("plugin", protocol.LogNotification{Level: protocol.LogLevelInfo, Event: "one"})
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("writer was not entered")
	}
	sink.Log("plugin", protocol.LogNotification{Level: protocol.LogLevelInfo, Event: "two"})
	done := make(chan struct{})
	go func() {
		sink.Log("plugin", protocol.LogNotification{Level: protocol.LogLevelInfo, Event: "three"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("producer blocked on slow log writer")
	}
	if status := sink.Status(); status.Dropped != 1 {
		t.Fatalf("dropped = %d, want 1", status.Dropped)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := sink.Close(ctx); err == nil {
		t.Fatal("close unexpectedly joined blocked writer")
	}
	close(writer.release)
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("retry close: %v", err)
	}
}

type blockingWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	select {
	case <-w.entered:
	default:
		close(w.entered)
	}
	<-w.release
	return len(p), nil
}
