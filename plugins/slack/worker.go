package slack

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	channelNameRetry   = time.Minute
	channelNameRefresh = time.Hour
	channelNameQueue   = 128
)

type domainSnapshot struct {
	Items     []activity
	Truncated bool
}

// workerSnapshot is a value copy. FreshUntil is a source deadline, never a render deadline.
type workerSnapshot struct {
	Items       []activity
	Phase       string
	LastSuccess time.Time
	ErrorCode   string
	FreshUntil  time.Time
	Fresh       bool
	Gap         bool
	Dropped     uint64
	Truncated   bool
	OpenUnsaved bool
}

type worker struct {
	instance  protocol.Instance
	cfg       config
	host      Host
	client    *slackClient
	dial      socketDialer
	now       func() time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	queue     chan json.RawMessage
	nameQueue chan string
	changed   chan struct{}

	diagnostics [len(diagnosticCodes)]atomic.Uint64

	// mu serializes reduction and durable handling; host calls must use w.ctx.
	// The socket reader never acquires it. Snapshots use an immutable cached view.
	mu               sync.Mutex
	state            *state
	nameRetry        map[string]time.Time
	dirty            bool
	domain           atomic.Pointer[domainSnapshot]
	transportMu      sync.Mutex
	activeConnection uint64
	authRequired     bool
	connected        bool
	lastSuccess      time.Time
	freshUntil       time.Time
	sourceCode       string
	gap              bool
	dropped          uint64
	checkpointFailed bool
	openUnsaved      bool
	restoreFailed    bool
	publications     publisher
	panelMu          sync.Mutex
	panel            *panelSession
}

func newWorker(instance protocol.Instance, cfg config, host Host, client *slackClient, dial socketDialer, now func() time.Time) *worker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &worker{instance: instance, cfg: cfg, host: host, client: client, dial: dial, now: now, ctx: ctx, cancel: cancel, done: make(chan struct{}), queue: make(chan json.RawMessage, 256), nameQueue: make(chan string, channelNameQueue), changed: make(chan struct{}, 1), state: newState(cfg, cfg.userID), nameRetry: make(map[string]time.Time)}
	if err := w.state.restoreCheckpoint(instance.Checkpoint, now()); err != nil {
		w.recordDiagnostic("checkpoint_invalid")
		w.restoreFailed = true
		w.gap = true
	}
	w.publications.current = make(map[string]publishedItem)
	w.cacheLocked()
	return w
}

func (w *worker) notify() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}
func (w *worker) cacheLocked() {
	w.domain.Store(&domainSnapshot{Items: w.state.items(), Truncated: w.state.truncated})
	w.notify()
}

func (w *worker) snapshot() workerSnapshot {
	now := w.now()
	d := w.domain.Load()
	result := workerSnapshot{Items: append([]activity(nil), d.Items...), Truncated: d.Truncated}
	w.transportMu.Lock()
	result.OpenUnsaved = w.openUnsaved
	result.LastSuccess, result.FreshUntil, result.ErrorCode, result.Gap, result.Dropped = w.lastSuccess, w.freshUntil, w.sourceCode, w.gap, w.dropped
	result.Fresh = w.freshUntil.After(now) && w.sourceCode != "auth_required" && w.ctx.Err() == nil
	switch {
	case !w.cfg.configured:
		result.Phase = "unconfigured"
	case w.sourceCode == "auth_required":
		result.Phase = "auth_required"
	case w.checkpointFailed:
		result.Phase = "degraded"
		result.ErrorCode = "checkpoint_failed"
	case w.restoreFailed:
		result.Phase = "degraded"
		if result.ErrorCode == "" {
			result.ErrorCode = "checkpoint_invalid"
		}
	case w.gap || !w.connected && !w.lastSuccess.IsZero():
		result.Phase = "degraded"
	case result.Fresh:
		result.Phase = "ready"
	default:
		result.Phase = "syncing"
	}
	if w.cfg.configured && !result.Fresh && !w.lastSuccess.IsZero() && result.Phase != "auth_required" {
		result.Phase = "degraded"
		if result.ErrorCode == "" {
			result.ErrorCode = "stale"
		}
	}
	w.transportMu.Unlock()
	if result.ErrorCode == "" && result.Gap {
		result.ErrorCode = "coverage_gap"
	}
	return result
}

func (w *worker) markGap(code string, drop bool) {
	w.recordDiagnostic(code)
	w.setGap(code, drop)
}

// Terminating read failures update health here; runTransport reports them once.
func (w *worker) setGap(code string, drop bool) {
	w.transportMu.Lock()
	w.gap = true
	if !w.authRequired {
		w.sourceCode = code
	}
	if drop {
		w.dropped++
	}
	w.transportMu.Unlock()
	w.notify()
}

func (w *worker) activateConnection(id uint64) {
	w.transportMu.Lock()
	w.activeConnection = id
	w.liveLocked()
	w.transportMu.Unlock()
	w.notify()
}

func (w *worker) connectionLive(id uint64) {
	w.transportMu.Lock()
	if id != w.activeConnection {
		w.transportMu.Unlock()
		return
	}
	w.liveLocked()
	w.transportMu.Unlock()
	w.notify()
}

func (w *worker) liveLocked() {
	if w.authRequired {
		return
	}
	w.connected = true
	w.lastSuccess = w.now().UTC()
	w.freshUntil = w.lastSuccess.Add(30 * time.Second)
	w.sourceCode = ""
}

func (w *worker) connectionGap(id uint64, code string, drop, diagnostic bool) {
	w.transportMu.Lock()
	if id != w.activeConnection {
		w.transportMu.Unlock()
		return
	}
	w.gap = true
	if !w.authRequired {
		w.sourceCode = code
	}
	if drop {
		w.dropped++
	}
	w.transportMu.Unlock()
	if diagnostic {
		w.recordDiagnostic(code)
	}
	w.notify()
}

func (w *worker) connectionDisconnected(id uint64, code string) {
	w.transportMu.Lock()
	if id != w.activeConnection {
		w.transportMu.Unlock()
		return
	}
	w.disconnectedLocked(code)
	w.transportMu.Unlock()
	w.recordDiagnostic(code)
	w.notify()
}

func (w *worker) disconnected(code string) {
	w.transportMu.Lock()
	w.disconnectedLocked(code)
	w.transportMu.Unlock()
	w.recordDiagnostic(code)
	w.notify()
}

func (w *worker) disconnectedLocked(code string) {
	w.connected = false
	w.gap = true
	w.activeConnection = 0
	w.authRequired = w.authRequired || code == "auth_required"
	if w.authRequired {
		code = "auth_required"
	}
	w.sourceCode = code
	deadline := w.now().UTC().Add(30 * time.Second)
	// A failed attempt cannot continually extend an earlier disconnect deadline.
	if !w.lastSuccess.IsZero() && (w.freshUntil.IsZero() || deadline.Before(w.freshUntil)) {
		w.freshUntil = deadline
	}
	if code == "auth_required" {
		w.freshUntil = w.now().UTC()
	}
}

func (w *worker) requiresAuthentication() bool {
	w.transportMu.Lock()
	defer w.transportMu.Unlock()
	return w.authRequired
}

func (w *worker) run() {
	defer close(w.done)
	// Cancellation rejects new actions; acquiring panelMu joins any admitted one.
	defer func() { w.panelMu.Lock(); w.panel = nil; w.panelMu.Unlock() }()
	var background sync.WaitGroup
	background.Go(w.runPublisher)
	background.Go(w.runDiagnostics)
	background.Go(w.runChannelNameResolver)
	defer background.Wait()
	if !w.cfg.configured {
		<-w.ctx.Done()
		return
	}
	var sockets sync.WaitGroup
	sockets.Go(func() { w.runTransport() })
	defer sockets.Wait()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if w.ctx.Err() != nil {
			return
		}
		select {
		case <-w.ctx.Done():
			return
		case raw := <-w.queue:
			w.reduce(raw)
		case <-ticker.C:
			w.mu.Lock()
			if w.state.prune(w.now()) {
				w.dirty = true
				w.cacheLocked()
			}
			if w.dirty {
				w.saveLocked(w.ctx)
			}
			w.mu.Unlock()
		}
	}
}

func (w *worker) reduce(raw json.RawMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx.Err() != nil || w.requiresAuthentication() {
		return
	}
	var callback struct {
		Type   string `json:"type"`
		TeamID string `json:"team_id"`
		Event  struct {
			Type        string `json:"type"`
			Channel     string `json:"channel"`
			ChannelType string `json:"channel_type"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &callback) != nil {
		w.markGap("invalid_event", false)
		return
	}
	if callback.Type == "app_rate_limited" && callback.TeamID == w.cfg.workspaceID {
		w.markGap("throttled", false)
		return
	}
	changed, err := w.state.apply(raw, w.now())
	if err != nil {
		code := "invalid_event"
		if errors.Is(err, errUnsupportedEvent) {
			code = "unsupported_event"
		}
		if errors.Is(err, errAuthorization) {
			code = "unproven_authorization"
		}
		w.markGap(code, false)
	}
	if changed {
		w.dirty = true
		w.cacheLocked()
		w.queueChannelNameLocked(callback.Event.Channel, callback.Event.ChannelType)
	}
}

func (w *worker) queueChannelNameLocked(channelID, channelType string) {
	for id := range w.nameRetry {
		if !w.state.hasChannel(id) {
			delete(w.nameRetry, id)
		}
	}
	if !w.cfg.allChannels || w.cfg.channels[channelID] != "" || channelType != "channel" && channelType != "group" || w.now().Before(w.nameRetry[channelID]) {
		return
	}
	select {
	case w.nameQueue <- channelID:
		w.nameRetry[channelID] = w.now().Add(channelNameRetry)
	default:
	}
}

func (w *worker) runChannelNameResolver() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case channelID := <-w.nameQueue:
			name, err := w.client.conversationName(w.ctx, w.instance.Secrets["user_token"], channelID)
			if w.ctx.Err() != nil {
				return
			}
			w.mu.Lock()
			if !w.state.hasChannel(channelID) {
				delete(w.nameRetry, channelID)
				w.mu.Unlock()
				continue
			}
			if err != nil {
				if source, ok := errors.AsType[*sourceError](err); ok {
					w.recordDiagnostic(source.code)
					w.nameRetry[channelID] = w.now().Add(max(channelNameRetry, source.retryAfter))
				} else {
					w.recordDiagnostic("request_failed")
				}
			} else {
				w.nameRetry[channelID] = w.now().Add(channelNameRefresh)
				if w.state.setChannelName(channelID, name) {
					w.cacheLocked()
				}
			}
			w.mu.Unlock()
		}
	}
}

func (w *worker) saveLocked(ctx context.Context) error {
	raw, err := w.state.checkpoint(w.now())
	if err == nil {
		err = w.saveCheckpoint(ctx, raw)
	} else {
		w.recordDiagnostic("checkpoint_failed")
	}
	w.transportMu.Lock()
	w.checkpointFailed = err != nil
	if err == nil {
		w.openUnsaved = false
	}
	w.transportMu.Unlock()
	if err == nil {
		w.dirty = false
	}
	w.cacheLocked()
	return err
}

func (w *worker) saveCheckpoint(ctx context.Context, raw json.RawMessage) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	stop := context.AfterFunc(w.ctx, cancel)
	defer stop()
	if w.ctx.Err() != nil {
		return context.Canceled
	}
	if err := w.host.SaveCheckpoint(ctx, protocol.CheckpointRequest{Instance: w.instance.Ref(), Data: raw}); err != nil {
		w.recordDiagnostic("checkpoint_failed")
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &sourceError{code: "checkpoint_failed"}
	}
	return nil
}

// handleLocked is called with mu held after exact-session admission. It commits
// only after durable save; queued arrivals stay intact until the lock is released.
func (w *worker) handleLocked(ctx context.Context, id string, revision uint64, fingerprint string) error {
	if w.ctx.Err() != nil || !w.snapshot().Fresh {
		return errStaleActivity
	}
	proposal, raw, err := w.state.proposeHandle(id, revision, fingerprint, w.now())
	if err != nil {
		return err
	}
	err = w.saveCheckpoint(ctx, raw)
	w.transportMu.Lock()
	w.checkpointFailed = err != nil
	w.transportMu.Unlock()
	if err != nil {
		w.notify()
		return err
	}
	w.state = proposal
	w.dirty = false
	w.cacheLocked()
	return nil
}

// The opener already succeeded. Keep the episode hidden even if saving fails;
// only the checkpoint is retried by the worker's existing dirty-state loop.
func (w *worker) openedLocked(ctx context.Context, target activity) error {
	proposal, _, err := w.state.proposeHandle(target.ID, target.Revision, target.Fingerprint, w.now())
	if err != nil {
		return err
	}
	w.state = proposal
	w.dirty = true
	w.transportMu.Lock()
	w.openUnsaved = true
	w.transportMu.Unlock()
	w.cacheLocked()
	return w.saveLocked(ctx)
}
