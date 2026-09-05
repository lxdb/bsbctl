package codexquota

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxdb/bsbctl/internal/codexusage"
	"github.com/lxdb/bsbctl/internal/livesession"
	"github.com/lxdb/bsbctl/sdk/protocol"

	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
)

type Host interface {
	PublishObservation(context.Context, protocol.Observation) error
	CompleteSession(context.Context, protocol.CompleteSessionRequest) error
	Log(context.Context, protocol.LogNotification) error
}

type sourceFactory func(Config) quotaSource

type Handler struct {
	configureMu sync.Mutex
	mu          sync.RWMutex
	host        Host
	sources     sourceFactory
	home        func() (string, error)
	now         func() time.Time
	workers     map[string]*accountWorker
	slots       chan struct{}
}

type accountWorker struct {
	instanceID string
	generation uint64
	config     Config
	showBadge  atomic.Bool
	host       Host
	source     quotaSource
	now        func() time.Time
	slots      chan struct{}
	cancel     context.CancelFunc
	done       chan struct{}

	summaryRevision      uint64
	pressureRevision     uint64
	summaryActive        map[string]struct{}
	pressureActive       bool
	failureActive        bool
	retryAttempt         int
	healthMu             sync.RWMutex
	consecutiveFailures  int
	unhealthy            bool
	publicationFailures  int
	publicationUnhealthy bool
	liveOnce             sync.Once
	live                 *livesession.Session
}

const unhealthyFailureThreshold = 3

func New(host Host) *Handler {
	client := &http.Client{Timeout: requestTimeout}
	return newHandler(host, func(config Config) quotaSource { return newAPISource(config, client) }, defaultMainHome, time.Now)
}

func newHandler(host Host, sources sourceFactory, home func() (string, error), now func() time.Time) *Handler {
	return &Handler{host: host, sources: sources, home: home, now: now, workers: make(map[string]*accountWorker), slots: make(chan struct{}, 2)}
}

// DefinitionForVersion binds immutable release metadata into the child
// handshake.
func DefinitionForVersion(version string) pluginsdk.Definition {
	return pluginsdk.Definition{
		ID: PluginID, Version: version,
		Contract: pluginsdk.Contract{ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident, protocol.ExecutionModeInteractive}, Channels: []protocol.Channel{{ID: ChannelSummary}, {ID: ChannelPressure}, {ID: ChannelLive}}},
		New:      func(host *pluginsdk.Host) pluginsdk.Plugin { return New(host) },
	}
}

func (h *Handler) ReplaceInstances(ctx context.Context, instances []protocol.Instance) error {
	for _, instance := range instances {
		if err := pluginsdk.RejectSecrets(instance.ID, instance.Secrets); err != nil {
			return err
		}
	}
	mainHome, err := h.home()
	if err != nil || !filepath.IsAbs(mainHome) {
		return pluginsdk.PermanentConfiguration(errors.New("main Codex home is unavailable"))
	}
	configured, err := decodeInstances(instances, mainHome)
	if err != nil {
		return pluginsdk.PermanentConfiguration(err)
	}
	h.configureMu.Lock()
	defer h.configureMu.Unlock()
	h.mu.RLock()
	previous := make(map[string]*accountWorker, len(h.workers))
	for id, worker := range h.workers {
		previous[id] = worker
	}
	h.mu.RUnlock()
	workers := make(map[string]*accountWorker, len(configured))
	workerContexts := make(map[*accountWorker]context.Context, len(configured))
	for _, account := range configured {
		if worker := previous[account.ID]; worker != nil && worker.generation == account.Generation {
			workers[account.ID] = worker
			delete(previous, account.ID)
			continue
		}
		workerCtx, cancel := context.WithCancel(context.Background())
		worker := &accountWorker{
			instanceID: account.ID, generation: account.Generation, config: account.Config,
			host: h.host, source: h.sources(account.Config), now: h.now, slots: h.slots,
			cancel: cancel, done: make(chan struct{}),
		}
		workers[account.ID] = worker
		workerContexts[worker] = workerCtx
	}
	if err := stopAccountWorkers(ctx, previous); err != nil {
		for worker := range workerContexts {
			worker.cancel()
		}
		return err
	}
	for _, account := range configured {
		workers[account.ID].showBadge.Store(account.ShowBadge)
	}
	h.mu.Lock()
	h.workers = workers
	h.mu.Unlock()
	for worker, workerCtx := range workerContexts {
		go worker.run(workerCtx)
	}
	return nil
}

func (h *Handler) Health(_ context.Context) protocol.HealthResult {
	h.mu.RLock()
	workers := make([]*accountWorker, 0, len(h.workers))
	for _, worker := range h.workers {
		workers = append(workers, worker)
	}
	h.mu.RUnlock()
	healthy := true
	for _, worker := range workers {
		worker.healthMu.RLock()
		healthy = healthy && !worker.unhealthy && !worker.publicationUnhealthy
		worker.healthMu.RUnlock()
	}
	return protocol.HealthResult{Healthy: healthy, ObservedAt: h.now().UTC()}
}

func (h *Handler) Shutdown(ctx context.Context) error {
	h.configureMu.Lock()
	defer h.configureMu.Unlock()
	return h.stopWorkers(ctx)
}

func (h *Handler) StartSession(ctx context.Context, request protocol.SessionStartRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if request.Action != "open" || request.Trigger == nil || request.Trigger.Kind != protocol.SessionTriggerLauncher || request.Trigger.Observation != nil {
		return errors.New("Codex quota requires an open launcher session")
	}
	h.mu.RLock()
	worker := h.workers[request.Instance.ID]
	h.mu.RUnlock()
	if worker == nil || request.Instance != (protocol.InstanceRef{ID: worker.instanceID, Generation: worker.generation}) {
		return errors.New("Codex quota instance is not active")
	}
	return worker.startLive(ctx, request.SessionToken)
}

func (h *Handler) EndSession(ctx context.Context, request protocol.SessionEndRequest) error {
	h.mu.RLock()
	worker := h.workers[request.Instance.ID]
	h.mu.RUnlock()
	if worker == nil || request.Instance != (protocol.InstanceRef{ID: worker.instanceID, Generation: worker.generation}) || request.SessionToken == "" {
		return nil
	}
	return worker.finishLive(ctx, request.SessionToken, false)
}

func (h *Handler) HandleSessionInput(ctx context.Context, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	h.mu.RLock()
	worker := h.workers[request.Instance.ID]
	h.mu.RUnlock()
	if worker == nil || request.Instance != (protocol.InstanceRef{ID: worker.instanceID, Generation: worker.generation}) || request.SessionToken == "" {
		return quotaInputResult(false), nil
	}
	button := request.Input.Button
	if button == nil || button.Button != protocol.ButtonBack || button.Action != protocol.ButtonPress {
		return quotaInputResult(false), nil
	}
	return quotaInputResult(true), worker.finishLive(ctx, request.SessionToken, true)
}

func quotaInputResult(consumed bool) protocol.SessionInputResult {
	if consumed {
		return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}
	}
	return protocol.SessionInputResult{Disposition: protocol.SessionInputNotConsumed}
}

func (h *Handler) stopWorkers(ctx context.Context) error {
	h.mu.RLock()
	workers := make(map[string]*accountWorker, len(h.workers))
	for id, worker := range h.workers {
		workers[id] = worker
	}
	h.mu.RUnlock()
	if err := stopAccountWorkers(ctx, workers); err != nil {
		return err
	}
	h.mu.Lock()
	h.workers = make(map[string]*accountWorker)
	h.mu.Unlock()
	return nil
}

func stopAccountWorkers(ctx context.Context, workers map[string]*accountWorker) error {
	for _, worker := range workers {
		_ = worker.finishLive(ctx, "", false)
		worker.cancel()
	}
	for _, worker := range workers {
		select {
		case <-worker.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (w *accountWorker) run(ctx context.Context) {
	defer close(w.done)
	delay := time.Duration(0)
	for {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return
		}
		snapshot, err := w.fetch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.recordFailure(ctx, err)
			w.retryAttempt++
			delay = retryDelay(w.retryAttempt, w.config.PollInterval)
			continue
		}
		w.recordSuccess(ctx)
		w.retryAttempt = 0
		if err := w.publish(ctx, snapshot, w.now()); err != nil {
			if ctx.Err() == nil {
				w.recordPublicationFailure(ctx)
			}
		} else {
			w.recordPublicationSuccess(ctx)
		}
		delay = w.config.PollInterval
	}
}

func (w *accountWorker) fetch(ctx context.Context) (Snapshot, error) {
	select {
	case w.slots <- struct{}{}:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
	operationCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	snapshot, err := w.source.Fetch(operationCtx)
	cancel()
	<-w.slots
	if err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (w *accountWorker) publish(ctx context.Context, snapshot Snapshot, now time.Time) error {
	residentErr := w.publishResident(ctx, snapshot, now)
	liveErr := w.publishLiveSnapshot(ctx, snapshot, now)
	return errors.Join(residentErr, liveErr)
}

func (w *accountWorker) publishResident(ctx context.Context, snapshot Snapshot, now time.Time) error {
	config := w.presentationConfig()
	validUntil := now.Add(3 * w.config.PollInterval).UTC()
	nextSummaries := make(map[string]struct{}, len(snapshot.Windows))
	for _, window := range snapshot.Windows {
		key := summaryObservationKey(window.Duration)
		w.summaryRevision++
		if err := w.host.PublishObservation(ctx, protocol.Observation{
			Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, Channel: ChannelSummary, Key: key, Revision: w.summaryRevision,
			Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNormal, ReasonCode: "codex_quota_summary",
			ObservedAt: now.UTC(), UpdatedAt: now.UTC(), ValidUntil: validUntil,
			Scene: new(quotaScene(snapshot, window, config, signalNone)),
		}); err != nil {
			return err
		}
		nextSummaries[key] = struct{}{}
	}
	for key := range w.summaryActive {
		if _, exists := nextSummaries[key]; exists {
			continue
		}
		w.summaryRevision++
		if err := w.host.PublishObservation(ctx, protocol.Observation{
			Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, Channel: ChannelSummary, Key: key, Revision: w.summaryRevision,
			Disposition: protocol.DispositionResolved, Impact: protocol.ImpactNormal,
			ReasonCode: "codex_quota_window_removed", ObservedAt: now.UTC(), UpdatedAt: now.UTC(),
		}); err != nil {
			return err
		}
	}
	w.summaryActive = nextSummaries
	signal := quotaSignal(snapshot, w.config)
	if signal == signalNone {
		if !w.pressureActive {
			return nil
		}
		w.pressureRevision++
		if err := w.host.PublishObservation(ctx, protocol.Observation{
			Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, Channel: ChannelPressure, Key: observationKey, Revision: w.pressureRevision,
			Disposition: protocol.DispositionResolved, Impact: protocol.ImpactNormal, ReasonCode: "codex_quota_recovered",
			ObservedAt: now.UTC(), UpdatedAt: now.UTC(),
		}); err != nil {
			return err
		}
		w.pressureActive = false
		return nil
	}
	w.pressureRevision++
	value := protocol.Observation{
		Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, Channel: ChannelPressure, Key: observationKey, Revision: w.pressureRevision,
		Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNotable, ReasonCode: "codex_quota_low",
		ObservedAt: now.UTC(), UpdatedAt: now.UTC(), ValidUntil: validUntil,
		Scene: new(quotaScene(snapshot, mostConstrainedWindow(snapshot), config, signal)),
	}
	if signal == signalCritical {
		value.Disposition = protocol.DispositionActionable
		value.Impact = protocol.ImpactCritical
		value.ReasonCode = "codex_quota_critical"
	}
	if err := w.host.PublishObservation(ctx, value); err != nil {
		return err
	}
	w.pressureActive = true
	return nil
}

func summaryObservationKey(duration time.Duration) string {
	return codexusage.SummaryKey(duration)
}

func mostConstrainedWindow(snapshot Snapshot) Window {
	return codexusage.MostConstrained(snapshot)
}

func (w *accountWorker) recordFailure(ctx context.Context, err error) {
	w.healthMu.Lock()
	wasUnhealthy := w.unhealthy
	w.consecutiveFailures++
	if w.consecutiveFailures >= unhealthyFailureThreshold {
		w.unhealthy = true
	}
	becameUnhealthy := !wasUnhealthy && w.unhealthy
	logFailure := !w.failureActive
	w.failureActive = true
	w.healthMu.Unlock()
	if w.host == nil {
		return
	}
	if logFailure {
		event := safeFailureEvent(err)
		_ = w.host.Log(ctx, protocol.LogNotification{
			Level: protocol.LogLevelWarn, Event: event, Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation},
			Message: "Codex quota monitoring is temporarily unavailable",
		})
	}
	if becameUnhealthy {
		_ = w.publishLiveUnavailable(ctx)
		_ = w.host.Log(ctx, protocol.LogNotification{
			Level: protocol.LogLevelWarn, Event: "codex_quota_unhealthy", Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation},
			Message: "Codex quota monitoring is unhealthy after sustained failures",
		})
	}
}

func (w *accountWorker) startLive(ctx context.Context, token string) error {
	return w.liveSession().Start(ctx, token, quotaStatusScene("WAITING", quotaWaitingColor), "codex_quota_live")
}

func (w *accountWorker) publishLiveSnapshot(ctx context.Context, snapshot Snapshot, now time.Time) error {
	config := w.presentationConfig()
	scene := quotaScene(snapshot, mostConstrainedWindow(snapshot), config, quotaSignal(snapshot, config))
	return w.liveSession().SetCurrent(ctx, scene, "codex_quota_live", now)
}

func (w *accountWorker) presentationConfig() Config {
	config := w.config
	config.ShowBadge = w.showBadge.Load()
	return config
}

func (w *accountWorker) publishLiveUnavailable(ctx context.Context) error {
	scene := quotaStatusScene("UNAVAILABLE", quotaUnavailableColor)
	return w.liveSession().PublishTransient(ctx, scene, "codex_quota_unavailable")
}

func (w *accountWorker) finishLive(ctx context.Context, token string, notify bool) error {
	return w.liveSession().Finish(ctx, token, notify, "codex_quota_live_closed")
}

func (w *accountWorker) liveSession() *livesession.Session {
	w.liveOnce.Do(func() {
		w.live = livesession.New(w.host, protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, ChannelLive, "panel", 24*time.Hour, w.now)
	})
	return w.live
}

func (w *accountWorker) recordSuccess(ctx context.Context) {
	w.healthMu.Lock()
	logRecovery := w.failureActive
	w.failureActive = false
	w.consecutiveFailures = 0
	w.unhealthy = false
	w.healthMu.Unlock()
	if !logRecovery || w.host == nil {
		return
	}
	_ = w.host.Log(ctx, protocol.LogNotification{
		Level: protocol.LogLevelInfo, Event: "codex_quota_recovered", Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation},
		Message: "Codex quota monitoring recovered",
	})
}

func (w *accountWorker) recordPublicationFailure(ctx context.Context) {
	w.healthMu.Lock()
	logFailure := w.publicationFailures == 0
	wasUnhealthy := w.publicationUnhealthy
	w.publicationFailures++
	if w.publicationFailures >= unhealthyFailureThreshold {
		w.publicationUnhealthy = true
	}
	becameUnhealthy := !wasUnhealthy && w.publicationUnhealthy
	w.healthMu.Unlock()
	if w.host == nil {
		return
	}
	if logFailure {
		_ = w.host.Log(ctx, protocol.LogNotification{
			Level: protocol.LogLevelWarn, Event: "codex_quota_publication_failed", Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation},
			Message: "Codex quota observations could not be published",
		})
	}
	if becameUnhealthy {
		_ = w.host.Log(ctx, protocol.LogNotification{
			Level: protocol.LogLevelWarn, Event: "codex_quota_publication_unhealthy", Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation},
			Message: "Codex quota publication is unhealthy after sustained failures",
		})
	}
}

func (w *accountWorker) recordPublicationSuccess(ctx context.Context) {
	w.healthMu.Lock()
	recovered := w.publicationFailures > 0
	w.publicationFailures = 0
	w.publicationUnhealthy = false
	w.healthMu.Unlock()
	if recovered && w.host != nil {
		_ = w.host.Log(ctx, protocol.LogNotification{
			Level: protocol.LogLevelInfo, Event: "codex_quota_publication_recovered", Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation},
			Message: "Codex quota observation publication recovered",
		})
	}
}

func retryDelay(attempt int, poll time.Duration) time.Duration {
	ladder := [...]time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 60 * time.Second}
	if attempt <= len(ladder) {
		return min(poll, ladder[max(1, attempt)-1])
	}
	return poll
}

func defaultMainHome() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("CODEX_HOME")); configured != "" {
		if !filepath.IsAbs(configured) {
			return "", errors.New("CODEX_HOME must be absolute")
		}
		return filepath.Clean(configured), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", errors.New("user home is unavailable")
	}
	return filepath.Join(home, ".codex"), nil
}
