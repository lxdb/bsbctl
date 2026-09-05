// Package pluginlog owns bounded, non-blocking plugin diagnostic delivery.
package pluginlog

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	defaultQueueCapacity = 256
)

type Options struct {
	QueueCapacity int
	Now           func() time.Time
}

type Status struct {
	Queued        int       `json:"queued"`
	Dropped       uint64    `json:"dropped"`
	LastErrorCode string    `json:"last_error_code,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitempty"`
}

type record struct {
	At         time.Time         `json:"at"`
	Component  string            `json:"component"`
	PluginID   string            `json:"plugin_id"`
	Level      string            `json:"level"`
	Event      string            `json:"event"`
	InstanceID string            `json:"instance_id,omitempty"`
	Message    string            `json:"message,omitempty"`
	Fields     map[string]string `json:"fields,omitempty"`
}

type Sink struct {
	mu      sync.Mutex
	queue   chan record
	done    chan struct{}
	out     io.Writer
	now     func() time.Time
	closed  bool
	dropped uint64
	errCode string
	errAt   time.Time
	secrets map[string][]string
	redact  []string
}

func New(out io.Writer, options Options) *Sink {
	if out == nil {
		out = io.Discard
	}
	if options.QueueCapacity <= 0 {
		options.QueueCapacity = defaultQueueCapacity
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	sink := &Sink{
		queue: make(chan record, options.QueueCapacity), done: make(chan struct{}),
		out: out, now: options.Now, secrets: make(map[string][]string),
	}
	go sink.run()
	return sink
}

func (s *Sink) Log(pluginID string, notification protocol.LogNotification) {
	s.mu.Lock()
	redactions := slices.Clone(s.redact)
	s.mu.Unlock()
	fields := make(map[string]string, len(notification.Fields))
	for key, value := range notification.Fields {
		redactedKey := sanitizeDiagnostic(key, redactions)
		if sensitiveFieldName(key) {
			fields[redactedKey] = "[REDACTED]"
		} else {
			fields[redactedKey] = sanitizeDiagnostic(value, redactions)
		}
	}
	s.enqueue(record{
		At: s.now().UTC(), Component: "plugin", PluginID: sanitizeDiagnostic(pluginID, redactions),
		Level: string(notification.Level), Event: sanitizeDiagnostic(notification.Event, redactions),
		InstanceID: sanitizeDiagnostic(notification.Instance.ID, redactions),
		Message:    sanitizeDiagnostic(notification.Message, redactions), Fields: fields,
	})
}

// MergeSecrets adds candidate delivered values before a plugin start or
// reconcile. Existing values remain protected until the transition completes.
func (s *Sink) MergeSecrets(values map[string][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pluginID, candidates := range normalizeSecrets(values) {
		merged := append(append([]string(nil), s.secrets[pluginID]...), candidates...)
		s.secrets[pluginID] = uniqueSecrets(merged)
	}
	s.rebuildRedactionsLocked()
}

// ReplaceSecrets records the exact values reachable by successfully applied
// plugin specifications. Removed values are forgotten only after old children
// have joined.
func (s *Sink) ReplaceSecrets(values map[string][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets = normalizeSecrets(values)
	s.rebuildRedactionsLocked()
}

func (s *Sink) enqueue(value record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.dropped++
		return
	}
	select {
	case s.queue <- value:
	default:
		s.dropped++
	}
}

func (s *Sink) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Queued: len(s.queue), Dropped: s.dropped, LastErrorCode: s.errCode, LastErrorAt: s.errAt}
}

func (s *Sink) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Sink) run() {
	defer close(s.done)
	encoder := json.NewEncoder(s.out)
	for value := range s.queue {
		if err := encoder.Encode(value); err != nil {
			s.mu.Lock()
			s.errCode = "plugin_log_write_failed"
			s.errAt = s.now().UTC()
			s.mu.Unlock()
		}
	}
}

var (
	authorizationAssignment = regexp.MustCompile(`(?i)(["']?[a-z0-9_.-]*authorization[a-z0-9_.-]*["']?\s*[:=]\s*)[^\r\n,;]+`)
	sensitiveAssignment     = regexp.MustCompile(`(?i)(["']?[a-z0-9_.-]*(?:token|secret|password|authorization|credential|cookie|api[_.-]?key|access[_.-]?key|private[_.-]?key|signing[_.-]?key)[a-z0-9_.-]*["']?\s*[:=]\s*)((?:bearer|basic)\s+[^\s,;]+|"[^"]*"|'[^']*'|[^\s,;]+)`)
	bearerValue             = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;]+`)
)

func sensitiveFieldName(key string) bool {
	lower := strings.ToLower(key)
	compact := strings.NewReplacer("_", "", "-", "", ".", "").Replace(lower)
	return strings.Contains(compact, "token") || strings.Contains(compact, "secret") ||
		strings.Contains(compact, "password") || strings.Contains(compact, "authorization") ||
		strings.Contains(compact, "credential") || strings.Contains(compact, "cookie") ||
		strings.Contains(compact, "apikey") || strings.Contains(compact, "accesskey") ||
		strings.Contains(compact, "privatekey") || strings.Contains(compact, "signingkey")
}

func sanitizeDiagnostic(value string, redactions []string) string {
	for _, secret := range redactions {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	value = authorizationAssignment.ReplaceAllString(value, `${1}[REDACTED]`)
	value = sensitiveAssignment.ReplaceAllString(value, `${1}[REDACTED]`)
	value = bearerValue.ReplaceAllString(value, "Bearer [REDACTED]")
	return strings.Map(func(current rune) rune {
		if unicode.IsControl(current) || current == '\u2028' || current == '\u2029' {
			return ' '
		}
		return current
	}, value)
}

func normalizeSecrets(values map[string][]string) map[string][]string {
	result := make(map[string][]string, len(values))
	for pluginID, candidates := range values {
		if normalized := uniqueSecrets(candidates); len(normalized) != 0 {
			result[pluginID] = normalized
		}
	}
	return result
}

func uniqueSecrets(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	slices.SortFunc(result, func(left, right string) int {
		return cmp.Or(cmp.Compare(len(right), len(left)), cmp.Compare(left, right))
	})
	return result
}

func (s *Sink) rebuildRedactionsLocked() {
	values := make([]string, 0)
	for _, secrets := range s.secrets {
		values = append(values, secrets...)
	}
	s.redact = uniqueSecrets(values)
}
