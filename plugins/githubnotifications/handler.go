package githubnotifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

// Host is the notification plugin's generation-scoped publication and effect boundary.
type Host interface {
	PublishObservation(context.Context, protocol.Observation) error
	SaveCheckpoint(context.Context, protocol.CheckpointRequest) error
	BeginSessionExecution(context.Context, protocol.SessionExecutionRequest) error
	CompleteSession(context.Context, protocol.CompleteSessionRequest) error
	Log(context.Context, protocol.LogNotification) error
}

// Handler owns configured workers. New does not perform provider I/O.
type Handler struct {
	configureMu sync.Mutex
	mu          sync.RWMutex
	host        Host
	client      *http.Client
	now         func() time.Time
	openURL     func(context.Context, string) error
	workers     map[string]*worker
}
type worker struct {
	mu                                sync.Mutex
	session                           *interactionSession
	ref                               protocol.InstanceRef
	config                            Config
	token                             string
	source                            *provider
	state                             *state
	host                              Host
	now                               func() time.Time
	cancel                            context.CancelFunc
	done                              chan struct{}
	closed                            bool
	retiring                          atomic.Bool
	publicationError, checkpointError bool
	published                         map[string]protocol.Observation
	publishedItems                    map[string]uint64
	revision                          uint64
	initialDiagnostic                 string
	lastPublishedAt                   time.Time
	lastPublishedState                publicationState
	lastLoggedError                   string
	writeEpoch                        uint64
	actionError                       string
}

// Leave headroom inside the two-second input callback budget when background
// checkpoint or publication I/O stalls while holding the worker state lock.
const backgroundHostTimeout = 500 * time.Millisecond

// New constructs the production handler with a package-owned HTTP client.
func New(host Host) *Handler { return newHandler(host, nil, time.Now) }
func newHandler(host Host, client *http.Client, now func() time.Time) *Handler {
	if now == nil {
		now = time.Now
	}
	return &Handler{host: host, client: client, now: now, workers: map[string]*worker{}, openURL: openBrowser}
}

// DefinitionForVersion binds release metadata to the declared plugin contract.
func DefinitionForVersion(version string) pluginsdk.Definition {
	return pluginsdk.Definition{ID: PluginID, Version: version, Contract: pluginsdk.Contract{ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident, protocol.ExecutionModeInteractive}, Channels: []protocol.Channel{{ID: ChannelSummary}, {ID: ChannelAttention}, {ID: ChannelConnection}, {ID: ChannelLive}}, Operations: []protocol.OperationDescriptor{{ID: OperationStatus, Kind: protocol.OperationQuery}, {ID: OperationItems, Kind: protocol.OperationQuery}}}, New: func(host *pluginsdk.Host) pluginsdk.Plugin { return New(host) }}
}
func (h *Handler) ReplaceInstances(ctx context.Context, instances []protocol.Instance) error {
	h.configureMu.Lock()
	defer h.configureMu.Unlock()
	if len(instances) > 8 {
		return pluginsdk.PermanentConfiguration(errors.New("GitHub notifications supports at most eight instances"))
	}
	if err := (protocol.ReplaceInstancesRequest{Instances: instances}).Validate(); err != nil {
		return pluginsdk.PermanentConfiguration(errors.New("invalid notification instances"))
	}
	h.mu.RLock()
	previous := maps.Clone(h.workers)
	h.mu.RUnlock()
	next := map[string]*worker{}
	contexts := map[*worker]context.Context{}
	scopes := map[int64]map[string]bool{}
	// First validate every local configuration, before any authorization or retirement.
	configs := make([]Config, len(instances))
	for index, instance := range instances {
		c, err := DecodeConfig(instance.Config)
		if err != nil {
			return pluginsdk.PermanentConfiguration(err)
		}
		if (!c.Configured && len(instance.Secrets) != 0) || (c.Configured && (len(instance.Secrets) != 1 || instance.Secrets["token"] == "")) {
			return pluginsdk.PermanentConfiguration(errors.New("configured notifications require only the token secret"))
		}
		configs[index] = c
		if old := previous[instance.ID]; old != nil && old.ref.Generation == instance.Generation && (!reflect.DeepEqual(c, old.config) || instance.Secrets["token"] != old.token) {
			return pluginsdk.PermanentConfiguration(errors.New("generation configuration changed"))
		}
	}
	for index, instance := range instances {
		c := configs[index]
		old := previous[instance.ID]
		var who Identity
		if old != nil && old.ref.Generation == instance.Generation && !old.retiring.Load() {
			who = old.state.identity
			next[instance.ID] = old
		} else {
			if c.Configured {
				var err error
				who, err = Authorize(ctx, h.client, instance.Secrets["token"], c.Repositories)
				if err != nil {
					return err
				}
			}
			s := newState(c, who)
			w := &worker{ref: instance.Ref(), config: c, token: instance.Secrets["token"], source: newProvider(h.client, instance.Secrets["token"]), state: s, host: h.host, now: h.now, done: make(chan struct{}), published: map[string]protocol.Observation{}}
			w.initialDiagnostic = s.restore(instance.Checkpoint, h.now())
			next[instance.ID] = w
		}
		if c.Configured {
			userScopes := scopes[who.ID]
			if userScopes == nil {
				userScopes = map[string]bool{}
				scopes[who.ID] = userScopes
			}
			if len(c.Repositories) == 0 {
				if len(userScopes) != 0 {
					return pluginsdk.PermanentConfiguration(errors.New("duplicate authenticated user/repository scope"))
				}
				userScopes[""] = true
				continue
			}
			if userScopes[""] {
				return pluginsdk.PermanentConfiguration(errors.New("duplicate authenticated user/repository scope"))
			}
			for _, repo := range c.Repositories {
				key := strings.ToLower(repo.Name)
				if userScopes[key] {
					return pluginsdk.PermanentConfiguration(errors.New("duplicate authenticated user/repository scope"))
				}
				userScopes[key] = true
			}
		}
	}
	// Cancellation is allocated only after the full candidate set has validated.
	retired := map[string]*worker{}
	for id, w := range previous {
		if next[id] != w {
			retired[id] = w
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Retirement is the commit boundary: once cancellation starts, finish the
	// owned joins and desired-set switch rather than reporting a false rollback.
	if err := stopWorkers(context.WithoutCancel(ctx), retired); err != nil {
		return err
	}
	for id, w := range next {
		if previous[id] != w {
			if old := previous[id]; old != nil && old.ref == w.ref {
				// The joined worker transfers its in-memory baseline and publication
				// history so a retry of the same generation cannot reset revisions.
				w.state, w.revision, w.published = old.state, old.revision, old.published
				w.publishedItems = old.publishedItems
				w.initialDiagnostic = ""
			}
			workerCtx, cancel := context.WithCancel(context.Background())
			w.cancel = cancel
			contexts[w] = workerCtx
		}
	}
	h.mu.Lock()
	h.workers = next
	h.mu.Unlock()
	for w, workerCtx := range contexts {
		go w.run(workerCtx)
	}
	return nil
}
func (h *Handler) Health(context.Context) protocol.HealthResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	healthy := true
	now := h.now()
	for _, w := range h.workers {
		w.mu.Lock()
		if w.config.Configured {
			healthy = healthy && !w.closed && w.state.phase == "ready" && w.state.fresh(now) && !w.publicationError && !w.checkpointError
		}
		w.mu.Unlock()
	}
	return protocol.HealthResult{Healthy: healthy, ObservedAt: now.UTC()}
}
func (h *Handler) Shutdown(ctx context.Context) error {
	h.configureMu.Lock()
	defer h.configureMu.Unlock()
	h.mu.RLock()
	workers := maps.Clone(h.workers)
	h.mu.RUnlock()
	if err := stopWorkers(ctx, workers); err != nil {
		return err
	}
	h.mu.Lock()
	clear(h.workers)
	h.mu.Unlock()
	return nil
}
func stopWorkers(ctx context.Context, workers map[string]*worker) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, w := range workers {
		w.retiring.Store(true)
		w.cancel()
	}
	for _, w := range workers {
		select {
		case <-w.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (w *worker) run(ctx context.Context) {
	defer close(w.done)
	defer func() { w.mu.Lock(); w.closed = true; w.mu.Unlock() }()
	if w.initialDiagnostic != "" {
		callCtx, cancel := context.WithTimeout(ctx, backgroundHostTimeout)
		_ = w.host.Log(callCtx, protocol.LogNotification{Level: protocol.LogLevelWarn, Event: w.initialDiagnostic, Instance: w.ref, Message: "Notification checkpoint ignored"})
		cancel()
	}
	w.mu.Lock()
	if w.state.checkpointDirty {
		callCtx, cancel := context.WithTimeout(ctx, backgroundHostTimeout)
		_ = w.persistCheckpoint(callCtx)
		cancel()
	}
	w.mu.Unlock()
	var group sync.WaitGroup
	group.Go(func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.mu.Lock()
				w.renewBackground(ctx)
				w.mu.Unlock()
			}
		}
	})
	defer group.Wait()
	if !w.config.Configured {
		<-ctx.Done()
		return
	}
	failures := 0
	for ctx.Err() == nil {
		w.mu.Lock()
		modified := w.state.lastModified
		epoch := w.writeEpoch
		w.mu.Unlock()
		result, err := w.source.fetch(ctx, w.config, modified)
		if ctx.Err() != nil {
			return
		}
		w.mu.Lock()
		if epoch != w.writeEpoch {
			w.mu.Unlock()
			continue
		}
		conflict := w.state.apply(result, err, w.now())
		if result.Complete && !result.NotModified && err == nil {
			w.actionError = ""
		}
		w.renewBackground(ctx)
		effective := w.state.effectiveInterval
		serverMinimum := w.state.serverInterval
		w.mu.Unlock()
		delay := max(effective, result.RetryAfter)
		if err != nil {
			failures++
			delay = max(backoff(failures), result.RetryAfter, serverMinimum)
			if IsCredentialRejected(err) {
				delay = max(delay, 5*time.Minute)
			}
		} else {
			failures = 0
		}
		if conflict {
			delay = max(5*time.Second, result.RetryAfter, serverMinimum)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (w *worker) renewBackground(ctx context.Context) {
	callCtx, cancel := context.WithTimeout(ctx, backgroundHostTimeout)
	defer cancel()
	w.renew(callCtx)
}
func backoff(failures int) time.Duration {
	base := min(5*time.Minute, 5*time.Second*time.Duration(1<<min(max(failures-1, 0), 6)))
	return min(5*time.Minute, base+time.Duration(rand.Int64N(int64(base/5)+1)))
}
func (w *worker) renew(ctx context.Context) {
	if ctx.Err() != nil || w.closed {
		return
	}
	if w.state.checkpointDirty || w.checkpointError {
		w.checkpointError = w.persistCheckpoint(ctx) != nil
	}
	w.publicationError = w.publish(ctx) != nil
	code := w.state.lastError
	if w.checkpointError {
		code = "checkpoint_failed"
	}
	if w.publicationError {
		code = "publication_failed"
	}
	if code != w.lastLoggedError {
		level := protocol.LogLevelWarn
		event := code
		message := "Notification collection is degraded"
		if code == "" {
			level = protocol.LogLevelInfo
			event = "recovered"
			message = "Notification collection recovered"
		}
		_ = w.host.Log(ctx, protocol.LogNotification{Level: level, Event: "github_notifications_" + event, Instance: w.ref, Message: message})
		w.lastLoggedError = code
	}
}
func (w *worker) persistCheckpoint(ctx context.Context) error {
	if err := w.host.SaveCheckpoint(ctx, protocol.CheckpointRequest{Instance: w.ref, Data: w.state.checkpointData(w.now())}); err != nil {
		w.checkpointError = true
		return err
	}
	w.state.checkpointDirty = false
	w.checkpointError = false
	return nil
}

type publicationState struct {
	Revision                          uint64
	Count                             int
	Phase, Error, ActionError         string
	Fresh, Truncated, CheckpointError bool
}

func (w *worker) publish(ctx context.Context) error {
	now := w.now().UTC()
	s := w.state
	current := publicationState{s.revision, len(s.items), s.phase, s.lastError, w.actionError, s.fresh(now), s.truncated, w.checkpointError}
	if current == w.lastPublishedState && !w.publicationError && now.Sub(w.lastPublishedAt) < 15*time.Second {
		return nil
	}
	desired := map[string]protocol.Observation{}
	add := func(channel, key string, disposition protocol.Disposition, impact protocol.Impact, observed, until time.Time, scene protocol.Scene) {
		desired[channel+"/"+key] = protocol.Observation{Instance: w.ref, Channel: channel, Key: key, Disposition: disposition, Impact: impact, ReasonCode: "github_notifications_" + channel, ObservedAt: observed, UpdatedAt: now, ValidUntil: until, Scene: new(scene)}
	}
	if w.session != nil {
		until := now.Add(45 * time.Second)
		if s.fresh(now) {
			until = minTime(until, s.freshUntil)
		}
		add(ChannelLive, hashKey("session", w.session.token), protocol.DispositionSnapshot, protocol.ImpactNormal, w.session.observedAt, until, w.sessionScene())
	}
	if s.fresh(now) {
		until := minTime(now.Add(45*time.Second), s.freshUntil)
		for _, i := range s.attention(now) {
			add(ChannelAttention, i.ID, protocol.DispositionActionable, protocol.ImpactNotable, i.ObservedAt, until, attentionScene(w.config, i))
		}
	}
	if s.config.Configured && (!s.fresh(now) || s.phase != "ready" || w.checkpointError || w.actionError != "") {
		add(ChannelConnection, "connection", protocol.DispositionNotable, protocol.ImpactNotable, now, now.Add(45*time.Second), notificationScene(connectionText(w), "GITHUB CONNECTION", "START: OPEN PANEL"))
	}
	for _, identity := range slices.Sorted(maps.Keys(w.published)) {
		if _, exists := desired[identity]; exists {
			continue
		}
		old := w.published[identity]
		w.revision++
		resolved := protocol.Observation{Instance: w.ref, Channel: old.Channel, Key: old.Key, Revision: w.revision, Disposition: protocol.DispositionResolved, Impact: protocol.ImpactNormal, ReasonCode: "github_notifications_withdrawn", ObservedAt: old.ObservedAt, UpdatedAt: now}
		if err := w.host.PublishObservation(ctx, resolved); err != nil {
			return err
		}
		delete(w.published, identity)
		delete(w.publishedItems, identity)
	}
	for _, identity := range slices.Sorted(maps.Keys(desired)) {
		observation := desired[identity]
		w.revision++
		observation.Revision = w.revision
		if err := w.host.PublishObservation(ctx, observation); err != nil {
			return err
		}
		w.published[identity] = observation
		if observation.Channel == ChannelAttention {
			if w.publishedItems == nil {
				w.publishedItems = map[string]uint64{}
			}
			w.publishedItems[identity] = s.items[observation.Key].Revision
		}
	}
	w.lastPublishedAt, w.lastPublishedState = now, current
	return nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

type statusResult struct {
	Phase         string    `json:"phase"`
	LastSuccessAt time.Time `json:"last_success_at,omitzero"`
	LastErrorCode string    `json:"last_error_code,omitempty"`
	RetainedCount int       `json:"retained_count"`
	Truncated     bool      `json:"truncated"`
}
type queryItem struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updated_at"`
	Handled   bool      `json:"handled"`
}

func (h *Handler) findWorker(ref protocol.InstanceRef) *worker {
	h.mu.RLock()
	defer h.mu.RUnlock()
	w := h.workers[ref.ID]
	if w == nil || w.ref != ref {
		return nil
	}
	return w
}
func (h *Handler) InvokeOperation(_ context.Context, request protocol.OperationRequest) (protocol.OperationResult, error) {
	w := h.findWorker(request.Instance)
	if w == nil {
		return protocol.OperationResult{}, errors.New("notification instance is not active")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return protocol.OperationResult{}, errors.New("notification instance is stopped")
	}
	s := w.state
	var value any
	switch request.Operation {
	case OperationStatus:
		var empty struct{}
		if protocol.DecodeStrict(request.Payload, &empty) != nil || bytes.Equal(bytes.TrimSpace(request.Payload), []byte("null")) {
			return protocol.OperationResult{}, errors.New("status requires an empty object")
		}
		phase, code := s.phase, s.lastError
		if s.config.Configured && phase == "ready" && !s.fresh(w.now()) {
			phase, code = "degraded", "source_stale"
		}
		if w.checkpointError {
			phase, code = "degraded", "checkpoint_failed"
		}
		if w.publicationError {
			phase, code = "degraded", "publication_failed"
		}
		value = statusResult{phase, s.lastSuccess, code, len(s.items), s.truncated || s.checkpointTruncated}
	case OperationItems:
		var input struct {
			Limit *int `json:"limit"`
		}
		raw := request.Payload
		if len(raw) == 0 {
			raw = []byte(`{}`)
		}
		if protocol.DecodeStrict(raw, &input) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return protocol.OperationResult{}, errors.New("invalid items query")
		}
		var queryFields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &queryFields)
		if v, exists := queryFields["limit"]; exists && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return protocol.OperationResult{}, errors.New("items limit must be 1-50")
		}
		limit := 20
		if input.Limit != nil {
			limit = *input.Limit
		}
		if limit < 1 || limit > 50 {
			return protocol.OperationResult{}, errors.New("items limit must be 1-50")
		}
		items := []queryItem{}
		ordered := s.ordered()
		for _, i := range ordered[:min(limit, len(ordered))] {
			state := "unread"
			if !s.fresh(w.now()) {
				state = "stale"
			}
			reason := i.Reason
			if !knownReason(reason) {
				reason = "unknown"
			}
			items = append(items, queryItem{i.ID, state, reason, i.UpdatedAt, i.Handled})
		}
		value = struct {
			Items     []queryItem `json:"items"`
			Truncated bool        `json:"truncated"`
		}{items, s.truncated || len(ordered) > limit}
	default:
		return protocol.OperationResult{}, errors.New("unsupported notification operation")
	}
	raw, err := json.Marshal(value)
	return protocol.OperationResult{Payload: raw}, err
}
func knownReason(reason string) bool {
	if actionable(reason) {
		return true
	}
	switch reason {
	case "comment", "subscribed", "author", "manual", "state_change", "ci_activity", "member_feature_requested", "security_advisory_credit", "unknown":
		return true
	}
	return false
}

func publicReason(reason string) string {
	if knownReason(reason) {
		return reason
	}
	return "unknown"
}
