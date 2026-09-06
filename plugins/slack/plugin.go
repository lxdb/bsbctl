package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/coder/websocket"
	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

const PluginID = "dev.bsbctl.slack"

// Host supplies metadata, observations, and exact-session execution boundaries.
type Host interface {
	Log(context.Context, protocol.LogNotification) error
	SaveCheckpoint(context.Context, protocol.CheckpointRequest) error
	PublishObservation(context.Context, protocol.Observation) error
	WithdrawObservation(context.Context, protocol.WithdrawRequest) error
	BeginSessionExecution(context.Context, protocol.SessionExecutionRequest) error
	CompleteSession(context.Context, protocol.CompleteSessionRequest) error
}

// Handler owns up to eight independent human workspace connections.
type Handler struct {
	configureMu sync.Mutex
	mu          sync.RWMutex
	workers     map[string]*worker
	host        Host
	client      *slackClient
	dial        socketDialer
	now         func() time.Time
	open        func(context.Context, string) error
}

// New constructs a collector without starting provider I/O.
func New(host Host) *Handler {
	client := newSlackClient(nil)
	dial := func(ctx context.Context, ticket string) (*websocket.Conn, error) {
		conn, response, err := websocket.Dial(ctx, ticket, &websocket.DialOptions{HTTPClient: client.http})
		if err != nil {
			if response != nil && response.StatusCode == 429 {
				return nil, &sourceError{code: "throttled", retryAfter: parseRetryAfter(response.Header.Get("Retry-After"), time.Now())}
			}
			if response != nil && (response.StatusCode == 401 || response.StatusCode == 403) {
				return nil, &sourceError{code: "auth_required"}
			}
			return nil, &sourceError{code: "disconnected"}
		}
		return conn, nil
	}
	return newHandler(host, client, dial, time.Now)
}

func newHandler(host Host, client *slackClient, dial socketDialer, now func() time.Time) *Handler {
	return &Handler{workers: make(map[string]*worker), host: host, client: client, dial: dial, now: now, open: openNative}
}

// ReplaceInstances validates the whole set before canceling any current worker.
// Identical live generations retain their state, connection and dedup window.
func (h *Handler) ReplaceInstances(ctx context.Context, instances []protocol.Instance) error {
	h.configureMu.Lock()
	defer h.configureMu.Unlock()
	if len(instances) > 8 {
		return pluginsdk.PermanentConfiguration(errConfig)
	}
	h.mu.RLock()
	previous := maps.Clone(h.workers)
	h.mu.RUnlock()
	type candidate struct {
		instance protocol.Instance
		cfg      config
		reuse    *worker
		restart  *worker
	}
	candidates := make([]candidate, len(instances))
	seen := make(map[string]bool)
	for i, instance := range instances {
		if instance.Ref().Validate() != nil || seen[instance.ID] {
			return pluginsdk.PermanentConfiguration(errConfig)
		}
		seen[instance.ID] = true
		cfg, err := decodeConfig(instance.Config)
		if err != nil {
			return pluginsdk.PermanentConfiguration(err)
		}
		if err = cfg.validateSecrets(instance.Secrets); err != nil {
			return pluginsdk.PermanentConfiguration(err)
		}
		instance.Config = bytes.Clone(instance.Config)
		instance.Secrets = maps.Clone(instance.Secrets)
		instance.Checkpoint = bytes.Clone(instance.Checkpoint)
		candidates[i] = candidate{instance: instance, cfg: cfg}
		if old := previous[instance.ID]; old != nil && old.instance.Generation == instance.Generation {
			if !bytes.Equal(old.instance.Config, instance.Config) || !maps.Equal(old.instance.Secrets, instance.Secrets) {
				return pluginsdk.PermanentConfiguration(errConfig)
			}
			if old.ctx.Err() == nil {
				candidates[i].reuse = old
			} else {
				candidates[i].restart = old
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	accounts := make(map[string]bool)
	for _, c := range candidates {
		if !c.cfg.configured {
			continue
		}
		account := c.cfg.workspaceID + ":" + c.cfg.userID
		if accounts[account] {
			return pluginsdk.PermanentConfiguration(errors.New("duplicate Slack account scope"))
		}
		accounts[account] = true
	}
	retained := make(map[string]*worker)
	for _, c := range candidates {
		if c.reuse != nil {
			retained[c.instance.ID] = c.reuse
		}
	}
	for id, w := range previous {
		if retained[id] != w {
			w.cancel()
		}
	}
	for id, w := range previous {
		if retained[id] != w {
			// Cancellation is the commit boundary. Finish joining and install the
			// validated desired set instead of reporting a false rollback.
			<-w.done
		}
	}
	for _, c := range candidates {
		if c.reuse != nil {
			continue
		}
		w := newWorker(c.instance, c.cfg, h.host, h.client, h.dial, h.now)
		if c.restart != nil {
			// A canceled exact generation restarts after join without resetting revision
			// counters or losing already observed state and callback deduplication.
			w.state = c.restart.state
			w.dirty = c.restart.dirty
			w.publications.revision = c.restart.publications.revision
			w.publications.current = c.restart.publications.current
			w.gap = true
			w.cacheLocked()
			if c.restart.requiresAuthentication() {
				w.disconnected("auth_required")
			}
		}
		retained[c.instance.ID] = w
	}
	h.mu.Lock()
	h.workers = retained
	h.mu.Unlock()
	for _, c := range candidates {
		if c.reuse == nil {
			go retained[c.instance.ID].run()
		}
	}
	return nil
}

func (h *Handler) lookup(ref protocol.InstanceRef) (*worker, error) {
	h.mu.RLock()
	w := h.workers[ref.ID]
	h.mu.RUnlock()
	if w == nil || w.instance.Generation != ref.Generation || w.ctx.Err() != nil {
		return nil, protocol.NewDomainError(protocol.ErrorGenerationConflict, errors.New("Slack instance is not current"))
	}
	return w, nil
}

// Shutdown cancels intake and joins all owned receiver and reducer goroutines.
func (h *Handler) Shutdown(ctx context.Context) error {
	h.configureMu.Lock()
	defer h.configureMu.Unlock()
	h.mu.RLock()
	workers := maps.Clone(h.workers)
	h.mu.RUnlock()
	for _, w := range workers {
		w.cancel()
	}
	for _, w := range workers {
		select {
		case <-w.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.mu.Lock()
	clear(h.workers)
	h.mu.Unlock()
	return nil
}

// Health reports content-free collector health, including failed persistence.
func (h *Handler) Health(context.Context) protocol.HealthResult {
	result := protocol.HealthResult{Healthy: true, ObservedAt: h.now().UTC()}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, w := range h.workers {
		phase := w.snapshot().Phase
		if phase != "ready" && phase != "unconfigured" {
			result.Healthy = false
		}
	}
	return result
}

// InvokeOperation reads bounded local snapshots and never calls Slack.
func (h *Handler) InvokeOperation(ctx context.Context, request protocol.OperationRequest) (protocol.OperationResult, error) {
	if ctx.Err() != nil {
		return protocol.OperationResult{}, ctx.Err()
	}
	w, err := h.lookup(request.Instance)
	if err != nil {
		return protocol.OperationResult{}, err
	}
	snap := w.snapshot()
	snap.Items = pendingItems(snap.Items)
	var result any
	switch request.Operation {
	case "status":
		if !bytes.Equal(bytes.TrimSpace(request.Payload), []byte("{}")) {
			return protocol.OperationResult{}, protocol.NewDomainError(protocol.ErrorInvalidArgument, errConfig)
		}
		result = struct {
			Phase       string    `json:"phase"`
			LastSuccess time.Time `json:"last_success_at,omitzero"`
			ErrorCode   string    `json:"last_error_code"`
			Pending     int       `json:"pending_count"`
			Truncated   bool      `json:"truncated"`
		}{snap.Phase, snap.LastSuccess, snap.ErrorCode, len(snap.Items), snap.Truncated || snap.Gap}
	case "items":
		var input struct {
			Limit *int `json:"limit"`
		}
		if len(request.Payload) == 0 {
			request.Payload = []byte("{}")
		}
		if protocol.DecodeStrict(request.Payload, &input) != nil || bytes.Contains(request.Payload, []byte("null")) {
			return protocol.OperationResult{}, protocol.NewDomainError(protocol.ErrorInvalidArgument, errConfig)
		}
		limit := 20
		if input.Limit != nil {
			limit = *input.Limit
		}
		if limit < 1 || limit > 50 {
			return protocol.OperationResult{}, protocol.NewDomainError(protocol.ErrorInvalidArgument, errConfig)
		}
		type item struct {
			ID      string    `json:"id"`
			State   string    `json:"state"`
			Reason  string    `json:"reason"`
			Updated time.Time `json:"updated_at"`
			Handled bool      `json:"handled"`
		}
		items := make([]item, 0, min(limit, len(snap.Items)))
		for _, a := range snap.Items[:min(limit, len(snap.Items))] {
			state := "active"
			if a.Handled {
				state = "handled"
			}
			if !snap.Fresh {
				state = "stale"
			}
			items = append(items, item{a.ID, state, a.Kind, a.UpdatedAt, a.Handled})
		}
		result = struct {
			Items     []item `json:"items"`
			Truncated bool   `json:"truncated"`
		}{items, snap.Truncated || snap.Gap || len(snap.Items) > limit}
	default:
		return protocol.OperationResult{}, protocol.NewDomainError(protocol.ErrorInvalidArgument, errConfig)
	}
	raw, err := json.Marshal(result)
	return protocol.OperationResult{Payload: raw}, err
}

// DefinitionForVersion binds the resident collector and exact foreground panel.
func DefinitionForVersion(version string) pluginsdk.Definition {
	return pluginsdk.Definition{ID: PluginID, Version: version, Contract: pluginsdk.Contract{
		ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident, protocol.ExecutionModeInteractive},
		Channels:       []protocol.Channel{{ID: ChannelSummary}, {ID: ChannelAttention}, {ID: ChannelConnection}, {ID: ChannelLive}},
		Operations:     []protocol.OperationDescriptor{{ID: "status", Kind: protocol.OperationQuery}, {ID: "items", Kind: protocol.OperationQuery}},
	}, New: func(host *pluginsdk.Host) pluginsdk.Plugin { return New(host) }}
}
