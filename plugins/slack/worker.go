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
	membershipRetryDelay  = time.Second
	membershipPositiveTTL = 5 * time.Minute
	membershipNegativeTTL = time.Minute
	membershipQueueSize   = 128
	maxPendingMembership  = 128
	coverageNoticeTTL     = 15 * time.Second
)

type membershipProof struct {
	member  bool
	name    string
	expires time.Time
}

type membershipRequest struct {
	channelID string
	epoch     uint64
}

type domainSnapshot struct {
	Items     []activity
	Truncated bool
}

// workerSnapshot is a value copy. FreshUntil is a source deadline, never a render deadline.
type workerSnapshot struct {
	Items              []activity
	Phase              string
	LastSuccess        time.Time
	ErrorCode          string
	FreshUntil         time.Time
	Fresh              bool
	Gap                bool
	Connected          bool
	CoverageIncomplete bool
	NoticeUntil        time.Time
	Dropped            uint64
	Truncated          bool
	OpenUnsaved        bool
}

type worker struct {
	instance        protocol.Instance
	cfg             config
	host            Host
	client          *slackClient
	dial            socketDialer
	now             func() time.Time
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	queue           chan json.RawMessage
	membershipQueue chan membershipRequest
	changed         chan struct{}

	diagnostics [len(diagnosticCodes)]atomic.Uint64

	// mu serializes reduction and durable handling; host calls must use w.ctx.
	// The socket reader never acquires it. Snapshots use an immutable cached view.
	mu                     sync.Mutex
	state                  *state
	membershipProofs       map[string]membershipProof
	membershipRetry        map[string]time.Time
	membershipInFlight     map[string]uint64
	membershipEpoch        map[string]uint64
	nextMembershipEpoch    uint64
	membershipPending      map[string][]normalizedEvent
	membershipPendingIDs   map[string]bool
	membershipPendingCount int
	membershipFailures     map[string]string
	dirty                  bool
	domain                 atomic.Pointer[domainSnapshot]
	transportMu            sync.Mutex
	activeConnection       uint64
	authRequired           bool
	connected              bool
	lastSuccess            time.Time
	freshUntil             time.Time
	sourceCode             string
	membershipCode         string
	membershipAuthRequired bool
	gap                    bool
	gapIncident            bool
	gapNoticeUntil         time.Time
	dropped                uint64
	checkpointFailed       bool
	openUnsaved            bool
	restoreFailed          bool
	publications           publisher
	panelMu                sync.Mutex
	panel                  *panelSession
}

func newWorker(instance protocol.Instance, cfg config, host Host, client *slackClient, dial socketDialer, now func() time.Time) *worker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &worker{
		instance: instance, cfg: cfg, host: host, client: client, dial: dial, now: now,
		ctx: ctx, cancel: cancel, done: make(chan struct{}), queue: make(chan json.RawMessage, 256),
		membershipQueue: make(chan membershipRequest, membershipQueueSize), changed: make(chan struct{}, 1),
		state: newState(cfg, cfg.userID), membershipProofs: make(map[string]membershipProof),
		membershipRetry: make(map[string]time.Time), membershipInFlight: make(map[string]uint64),
		membershipEpoch: make(map[string]uint64), membershipPending: make(map[string][]normalizedEvent),
		membershipPendingIDs: make(map[string]bool), membershipFailures: make(map[string]string),
	}
	if err := w.state.restoreCheckpoint(instance.Checkpoint, now()); err != nil {
		w.recordDiagnostic("checkpoint_invalid")
		w.restoreFailed = true
		w.gap = true
	} else {
		if w.state.unresolvedMembership {
			w.state.unresolvedMembership = false
			w.state.coverageIncomplete = true
			w.dirty = true
		}
		w.gap = w.state.coverageIncomplete
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
	result.Connected, result.NoticeUntil = w.connected, w.gapNoticeUntil
	result.CoverageIncomplete = w.gap || result.Truncated
	result.Fresh = w.freshUntil.After(now) && w.sourceCode != "auth_required" && !w.membershipAuthRequired && w.ctx.Err() == nil
	switch {
	case !w.cfg.configured:
		result.Phase = "unconfigured"
	case w.sourceCode == "auth_required" || w.membershipAuthRequired:
		result.Phase = "auth_required"
		result.ErrorCode = "auth_required"
	case w.checkpointFailed:
		result.Phase = "degraded"
		result.ErrorCode = "checkpoint_failed"
	case w.restoreFailed:
		result.Phase = "degraded"
		if result.ErrorCode == "" {
			result.ErrorCode = "checkpoint_invalid"
		}
	case w.membershipCode != "":
		result.Phase = "degraded"
		result.ErrorCode = w.membershipCode
	case !w.connected && !w.lastSuccess.IsZero():
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
	w.recordGapLocked()
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
	w.gapIncident = false
}

func (w *worker) recordGapLocked() {
	w.gap = true
	if !w.gapIncident {
		w.gapIncident = true
		w.gapNoticeUntil = w.now().UTC().Add(coverageNoticeTTL)
	}
}

func (w *worker) connectionGap(id uint64, code string, drop, diagnostic bool) {
	w.transportMu.Lock()
	if id != w.activeConnection {
		w.transportMu.Unlock()
		return
	}
	w.recordGapLocked()
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
	w.recordGapLocked()
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
	background.Go(w.runMembershipResolver)
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
			w.queueMembershipRetriesLocked()
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
	callback, err := normalizeCallbackEvent(raw, w.cfg.appID, w.cfg.workspaceID, w.cfg.userID, w.cfg.rearDetails || w.cfg.frontMessagePreview)
	if err != nil {
		w.handleReductionErrorLocked(err)
		return
	}
	switch callback.kind {
	case callbackIgnored:
		return
	case callbackMembership:
		w.applyMembershipLifecycleLocked(callback.membership)
		return
	case callbackRateLimited:
		w.markGap("throttled", false)
		return
	}
	event := callback.message
	_, selected := w.cfg.channels[event.channelID]
	if (event.channelType == "channel" || event.channelType == "group") && (w.cfg.allChannels || selected) {
		w.admitByMembershipLocked(event)
		return
	}
	w.applyNormalizedLocked(event)
}

func (w *worker) handleReductionErrorLocked(err error) {
	code := "invalid_event"
	if errors.Is(err, errUnsupportedEvent) {
		code = "unsupported_event"
	}
	if errors.Is(err, errAuthorization) {
		code = "unproven_authorization"
	}
	w.markGap(code, false)
}

func (w *worker) applyNormalizedLocked(event normalizedEvent) {
	changed := w.state.applyNormalized(event, w.now())
	if changed {
		w.dirty = true
		w.cacheLocked()
	}
}

func (w *worker) admitByMembershipLocked(event normalizedEvent) {
	channelID := event.channelID
	now := w.now()
	if proof, ok := w.membershipProofs[channelID]; ok && now.Before(proof.expires) {
		if proof.member {
			w.applyNormalizedLocked(event)
			if proof.name != "" && w.state.setChannelName(channelID, proof.name) {
				w.cacheLocked()
			}
		}
		return
	}
	delete(w.membershipProofs, channelID)
	if w.membershipPendingIDs[event.callbackID] || w.state.hasAcceptedCallback(event.callbackID) {
		return
	}
	if w.membershipPendingCount == maxPendingMembership {
		w.markGap("queue_overflow", true)
		return
	}
	w.membershipPending[channelID] = append(w.membershipPending[channelID], event)
	w.membershipPendingIDs[event.callbackID] = true
	w.membershipPendingCount++
	if !w.state.unresolvedMembership {
		w.state.unresolvedMembership = true
		w.dirty = true
		if w.host != nil {
			_ = w.saveLocked(w.ctx)
		}
	}
	w.queueMembershipLookupLocked(channelID)
}

func (w *worker) queueMembershipLookupLocked(channelID string) {
	w.transportMu.Lock()
	authRequired := w.membershipAuthRequired
	w.transportMu.Unlock()
	if authRequired {
		return
	}
	if _, active := w.membershipInFlight[channelID]; active || w.now().Before(w.membershipRetry[channelID]) {
		return
	}
	epoch := w.membershipEpochLocked(channelID)
	select {
	case w.membershipQueue <- membershipRequest{channelID: channelID, epoch: epoch}:
		w.membershipInFlight[channelID] = epoch
	default:
		w.membershipRetry[channelID] = w.now().Add(membershipRetryDelay)
	}
}

func (w *worker) queueMembershipRetriesLocked() {
	for channelID := range w.membershipPending {
		w.queueMembershipLookupLocked(channelID)
	}
}

func (w *worker) membershipEpochLocked(channelID string) uint64 {
	if epoch := w.membershipEpoch[channelID]; epoch != 0 {
		return epoch
	}
	w.nextMembershipEpoch++
	if w.nextMembershipEpoch == 0 {
		w.nextMembershipEpoch++
	}
	w.membershipEpoch[channelID] = w.nextMembershipEpoch
	return w.nextMembershipEpoch
}

func (w *worker) advanceMembershipEpochLocked(channelID string) {
	delete(w.membershipEpoch, channelID)
	w.membershipEpochLocked(channelID)
}

func (w *worker) runMembershipResolver() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case request := <-w.membershipQueue:
			w.resolveMembership(request)
		}
	}
}

func (w *worker) resolveMembership(request membershipRequest) {
	w.transportMu.Lock()
	authRequired := w.membershipAuthRequired
	w.transportMu.Unlock()
	if authRequired {
		w.mu.Lock()
		if w.membershipInFlight[request.channelID] == request.epoch {
			delete(w.membershipInFlight, request.channelID)
		}
		w.mu.Unlock()
		return
	}
	name, err := w.client.conversationName(w.ctx, w.instance.Secrets["user_token"], request.channelID)
	if w.ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.membershipInFlight[request.channelID] == request.epoch {
		delete(w.membershipInFlight, request.channelID)
	}
	if request.epoch != w.membershipEpoch[request.channelID] {
		if len(w.membershipPending[request.channelID]) != 0 {
			w.queueMembershipLookupLocked(request.channelID)
		}
		return
	}
	switch {
	case errors.Is(err, errNotMember):
		w.setMembershipProofLocked(request.channelID, membershipProof{expires: w.now().Add(membershipNegativeTTL)})
		delete(w.membershipRetry, request.channelID)
		w.discardPendingMembershipLocked(request.channelID)
		changed := w.state.removeChannel(request.channelID, w.now())
		w.clearMembershipFailureLocked(request.channelID)
		if changed {
			w.dirty = true
			w.cacheLocked()
		}
		w.settlePendingMembershipLocked()
	case err != nil:
		code := "request_failed"
		retry := membershipRetryDelay
		if source, ok := errors.AsType[*sourceError](err); ok {
			code = source.code
			retry = max(retry, source.retryAfter)
		}
		w.recordDiagnostic(code)
		if code == "auth_required" {
			delete(w.membershipRetry, request.channelID)
			w.membershipFailures[request.channelID] = code
			w.transportMu.Lock()
			w.membershipAuthRequired = true
			w.membershipCode = code
			w.transportMu.Unlock()
			w.notify()
			return
		}
		w.membershipRetry[request.channelID] = w.now().Add(retry)
		w.membershipFailures[request.channelID] = code
		w.updateMembershipCodeLocked()
	case err == nil:
		w.setMembershipProofLocked(request.channelID, membershipProof{member: true, name: name, expires: w.now().Add(membershipPositiveTTL)})
		delete(w.membershipRetry, request.channelID)
		w.clearMembershipFailureLocked(request.channelID)
		pending := w.membershipPending[request.channelID]
		w.discardPendingMembershipLocked(request.channelID)
		for _, event := range pending {
			w.applyNormalizedLocked(event)
		}
		if w.state.setChannelName(request.channelID, name) {
			w.cacheLocked()
		}
		w.settlePendingMembershipLocked()
	}
}

func (w *worker) discardPendingMembershipLocked(channelID string) {
	for _, event := range w.membershipPending[channelID] {
		delete(w.membershipPendingIDs, event.callbackID)
		w.membershipPendingCount--
	}
	delete(w.membershipPending, channelID)
}

func (w *worker) settlePendingMembershipLocked() {
	if w.membershipPendingCount != 0 || !w.state.unresolvedMembership {
		return
	}
	w.state.unresolvedMembership = false
	w.dirty = true
	if w.host != nil {
		_ = w.saveLocked(w.ctx)
	}
}

func (w *worker) setMembershipProofLocked(channelID string, proof membershipProof) {
	w.membershipProofs[channelID] = proof
	for len(w.membershipProofs) > maxRetained {
		oldest := ""
		for id, candidate := range w.membershipProofs {
			if oldest == "" || candidate.expires.Before(w.membershipProofs[oldest].expires) || candidate.expires.Equal(w.membershipProofs[oldest].expires) && id < oldest {
				oldest = id
			}
		}
		delete(w.membershipProofs, oldest)
		if len(w.membershipPending[oldest]) == 0 {
			if _, inFlight := w.membershipInFlight[oldest]; !inFlight {
				delete(w.membershipEpoch, oldest)
			}
		}
	}
}

func (w *worker) clearMembershipFailureLocked(channelID string) {
	delete(w.membershipFailures, channelID)
	w.updateMembershipCodeLocked()
}

func (w *worker) updateMembershipCodeLocked() {
	code := ""
	for _, failure := range w.membershipFailures {
		if code == "" || failure < code {
			code = failure
		}
	}
	w.transportMu.Lock()
	w.membershipCode = code
	w.transportMu.Unlock()
	w.notify()
}

func (w *worker) applyMembershipLifecycleLocked(event membershipEvent) {
	if !w.state.acceptCallback(event.callbackID) || event.userID != w.cfg.userID {
		return
	}
	w.advanceMembershipEpochLocked(event.channelID)
	delete(w.membershipInFlight, event.channelID)
	delete(w.membershipRetry, event.channelID)
	w.clearMembershipFailureLocked(event.channelID)
	if event.kind == "member_left_channel" {
		w.setMembershipProofLocked(event.channelID, membershipProof{expires: w.now().Add(membershipNegativeTTL)})
		w.discardPendingMembershipLocked(event.channelID)
		if w.state.removeChannel(event.channelID, w.now()) {
			w.dirty = true
			w.cacheLocked()
		}
		w.settlePendingMembershipLocked()
		return
	}
	w.setMembershipProofLocked(event.channelID, membershipProof{member: true, expires: w.now().Add(membershipPositiveTTL)})
	pending := w.membershipPending[event.channelID]
	w.discardPendingMembershipLocked(event.channelID)
	for _, pendingEvent := range pending {
		w.applyNormalizedLocked(pendingEvent)
	}
	w.settlePendingMembershipLocked()
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
