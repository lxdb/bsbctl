package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/internal/codexusage"
	"github.com/lxdb/bsbctl/plugins/codex/internal/appserver"
	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type Host interface {
	PublishObservation(context.Context, protocol.Observation) error
	SaveCheckpoint(context.Context, protocol.CheckpointRequest) error
	BeginSessionExecution(context.Context, protocol.SessionExecutionRequest) error
	CompleteSession(context.Context, protocol.CompleteSessionRequest) error
	Log(context.Context, protocol.LogNotification) error
}

type appServerClient interface {
	Run(context.Context, chan<- appserver.ManagerEvent) error
	Respond(context.Context, appserver.RawID, any) error
	Interrupt(context.Context, appserver.Connection, string, string) error
}

type appServerFactory func(codexExecutable string, rateLimitsEnabled bool) appServerClient

type Handler struct {
	configureMu sync.Mutex
	mu          sync.RWMutex
	host        Host
	clients     appServerFactory
	home        func() (string, error)
	now         func() time.Time
	after       func(time.Duration) <-chan time.Time
	worker      *codexWorker
	healthy     bool
}

type codexWorker struct {
	instanceID       string
	generation       uint64
	config           Config
	host             Host
	client           appServerClient
	reducer          *Reducer
	publisher        *cardPublisher
	owner            *Handler
	cancel           context.CancelFunc
	done             chan struct{}
	stateMu          sync.Mutex
	session          *interactionSession
	quotaUnavailable bool
	after            func(time.Duration) <-chan time.Time
}

func New(host Host) *Handler {
	return newHandler(host, func(executable string, rateLimitsEnabled bool) appServerClient {
		return appserver.NewManager(&appserver.ProxyConnector{CodexBin: executable}, appserver.ManagerOptions{RateLimitsEnabled: rateLimitsEnabled})
	}, os.UserHomeDir, time.Now)
}

func newHandler(host Host, clients appServerFactory, home func() (string, error), now func() time.Time) *Handler {
	return &Handler{host: host, clients: clients, home: home, now: now, after: time.After, healthy: true}
}

func DefinitionForVersion(version string) pluginsdk.Definition {
	return pluginsdk.Definition{
		ID: PluginID, Version: version,
		Contract: pluginsdk.Contract{
			ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident, protocol.ExecutionModeInteractive},
			Channels: []protocol.Channel{
				{ID: ChannelAttention}, {ID: ChannelGuidance}, {ID: ChannelOutcome}, {ID: ChannelActivity}, {ID: ChannelProgress},
				{ID: ChannelOverview}, {ID: ChannelConnection}, {ID: ChannelDetail},
				{ID: ChannelQuotaSummary}, {ID: ChannelQuotaPressure},
			},
			Operations: []protocol.OperationDescriptor{
				{ID: OperationSessions, Kind: protocol.OperationQuery},
				{ID: OperationPin, Kind: protocol.OperationCommand},
				{ID: OperationUnpin, Kind: protocol.OperationCommand},
			},
		},
		New: func(host *pluginsdk.Host) pluginsdk.Plugin { return New(host) },
	}
}

func (h *Handler) ReplaceInstances(ctx context.Context, instances []protocol.Instance) error {
	var selected *protocol.Instance
	var config Config
	for index := range instances {
		instance := instances[index]
		if selected != nil {
			return pluginsdk.PermanentConfiguration(errors.New("Codex app-server plugin supports at most one enabled instance"))
		}
		if err := pluginsdk.RejectSecrets(instance.ID, instance.Secrets); err != nil {
			return err
		}
		decoded, err := decodeConfig(instance.Config)
		if err != nil {
			return pluginsdk.PermanentConfiguration(fmt.Errorf("instance %q: %w", instance.ID, err))
		}
		selected, config = &instance, decoded
	}
	var executable string
	if selected != nil {
		home, err := h.home()
		if err != nil || !filepath.IsAbs(home) {
			return pluginsdk.PermanentConfiguration(errors.New("user home is unavailable"))
		}
		executable = filepath.Join(filepath.Clean(home), ".local", "bin", "codex")
	}

	h.configureMu.Lock()
	defer h.configureMu.Unlock()
	h.mu.RLock()
	previous := h.worker
	h.mu.RUnlock()
	if err := h.stopWorker(ctx); err != nil {
		return err
	}
	if selected == nil {
		return nil
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	publisher := newCardPublisher(h.host, selected.Ref(), h.now)
	if previous != nil && previous.instanceID == selected.ID && previous.generation == selected.Generation {
		publisher = previous.publisher
	}
	worker := &codexWorker{
		instanceID: selected.ID, generation: selected.Generation, config: config,
		host: h.host, client: h.clients(executable, config.ShowQuota), reducer: NewReducerWithQuota(h.now, QuotaOptions{
			Enabled: config.ShowQuota, AssetPath: codexMarkSource,
			Presentation: codexusage.PresentationConfig{
				Label: "MAIN", WarningRemainingPercent: config.QuotaWarningRemainingPercent,
				CriticalRemainingPercent: config.QuotaCriticalRemainingPercent,
			},
		}), publisher: publisher,
		owner: h, cancel: cancel, done: make(chan struct{}),
		after: h.after,
	}
	if selected.Checkpoint != nil {
		worker.reducer.RestorePinnedThread(decodePinnedCheckpoint(selected.Checkpoint))
	}
	h.mu.Lock()
	h.worker, h.healthy = worker, true
	h.mu.Unlock()
	go worker.run(workerCtx)
	return nil
}

func (h *Handler) Health(context.Context) protocol.HealthResult {
	h.mu.RLock()
	healthy := h.healthy
	h.mu.RUnlock()
	return protocol.HealthResult{Healthy: healthy, ObservedAt: h.now().UTC()}
}

func (h *Handler) Shutdown(ctx context.Context) error {
	h.configureMu.Lock()
	defer h.configureMu.Unlock()
	return h.stopWorker(ctx)
}

func (h *Handler) stopWorker(ctx context.Context) error {
	h.mu.RLock()
	worker := h.worker
	h.mu.RUnlock()
	if worker == nil {
		return nil
	}
	if err := worker.finishSession(ctx, "", false); err != nil {
		return err
	}
	worker.cancel()
	select {
	case <-worker.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	h.mu.Lock()
	if h.worker == worker {
		h.worker = nil
	}
	h.mu.Unlock()
	return nil
}

func (w *codexWorker) run(ctx context.Context) {
	defer close(w.done)
	if w.after == nil {
		w.after = time.After
	}
	var reconnectC <-chan time.Time
	var reconnectAt time.Time
	scheduleReconnect := func() {
		deadline, ok := w.reducer.ReconnectDeadline()
		if !ok {
			reconnectC, reconnectAt = nil, time.Time{}
			return
		}
		if deadline.Equal(reconnectAt) {
			return
		}
		delay := deadline.Sub(w.owner.now().UTC())
		if delay < 0 {
			delay = 0
		}
		reconnectC, reconnectAt = w.after(delay), deadline
	}
	w.stateMu.Lock()
	cards := w.reducer.Cards()
	scheduleReconnect()
	w.stateMu.Unlock()
	if err := w.publisher.Publish(ctx, cards); err != nil {
		w.log(ctx, protocol.LogLevelWarn, "codex_publish_failed", "Codex device status publication failed")
	}
	events := make(chan appserver.ManagerEvent, 64)
	clientDone := make(chan error, 1)
	go func() { clientDone <- w.client.Run(ctx, events) }()
	for {
		select {
		case event := <-events:
			w.stateMu.Lock()
			logQuotaFailure := event.Kind == appserver.ManagerRateLimitsReadFailed && !w.quotaUnavailable
			logQuotaRecovery := event.Kind == appserver.ManagerRateLimitsSnapshot && w.quotaUnavailable
			if event.Kind == appserver.ManagerRateLimitsReadFailed {
				w.quotaUnavailable = true
			} else if event.Kind == appserver.ManagerRateLimitsSnapshot {
				w.quotaUnavailable = false
			}
			w.reducer.Apply(event)
			cards := w.reducer.Cards()
			scheduleReconnect()
			staleToken := w.staleSessionTokenLocked(event.Kind == appserver.ManagerDisconnected)
			liveDetail, refreshLiveDetail := w.launcherDetailLocked()
			w.stateMu.Unlock()
			if err := w.publisher.Publish(ctx, cards); err != nil && ctx.Err() == nil {
				w.log(ctx, protocol.LogLevelWarn, "codex_publish_failed", "Codex device status publication failed")
			}
			if staleToken != "" {
				_ = w.finishSession(ctx, staleToken, true)
			}
			if refreshLiveDetail {
				if err := w.publisher.PublishDetail(ctx, liveDetail); err != nil && ctx.Err() == nil {
					w.log(ctx, protocol.LogLevelWarn, "codex_publish_failed", "Codex live detail publication failed")
				}
			}
			if logQuotaFailure {
				w.logFields(ctx, protocol.LogLevelWarn, "codex_quota_unavailable", "Codex quota is temporarily unavailable", map[string]string{
					"stage": event.FailureStage,
					"code":  event.FailureCode,
				})
			}
			if logQuotaRecovery {
				w.log(ctx, protocol.LogLevelInfo, "codex_quota_recovered", "Codex quota is available again")
			}
			if event.Kind == appserver.ManagerDisconnected {
				w.logFields(ctx, protocol.LogLevelWarn, "codex_app_server_disconnected", "Codex app-server connection is unavailable", map[string]string{
					"stage": event.FailureStage,
					"code":  event.FailureCode,
				})
			}
		case <-reconnectC:
			w.stateMu.Lock()
			reconnectC, reconnectAt = nil, time.Time{}
			cards := w.reducer.Cards()
			scheduleReconnect()
			liveDetail, refreshLiveDetail := w.launcherDetailLocked()
			w.stateMu.Unlock()
			if err := w.publisher.Publish(ctx, cards); err != nil && ctx.Err() == nil {
				w.log(ctx, protocol.LogLevelWarn, "codex_publish_failed", "Codex device status publication failed")
			}
			if refreshLiveDetail {
				if err := w.publisher.PublishDetail(ctx, liveDetail); err != nil && ctx.Err() == nil {
					w.log(ctx, protocol.LogLevelWarn, "codex_publish_failed", "Codex live detail publication failed")
				}
			}
		case err := <-clientDone:
			if ctx.Err() == nil {
				w.owner.setUnhealthy(w)
				w.log(ctx, protocol.LogLevelError, "codex_client_stopped", "Codex app-server client stopped unexpectedly")
			}
			_ = err
			return
		case <-ctx.Done():
			<-clientDone
			return
		}
	}
}

func (w *codexWorker) launcherDetailLocked() (Card, bool) {
	if w.session == nil || !w.session.launcher {
		return Card{}, false
	}
	w.session.card = w.reducer.LiveCard()
	return w.session.detailCard(w.owner.now()), true
}

func (w *codexWorker) log(ctx context.Context, level protocol.LogLevel, event, message string) {
	w.logFields(ctx, level, event, message, nil)
}

func (w *codexWorker) logFields(ctx context.Context, level protocol.LogLevel, event, message string, fields map[string]string) {
	if w.host == nil {
		return
	}
	_ = w.host.Log(ctx, protocol.LogNotification{
		Level: level, Event: event, Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, Message: message, Fields: fields,
	})
}

func (h *Handler) setUnhealthy(worker *codexWorker) {
	h.mu.Lock()
	if h.worker == worker {
		h.healthy = false
	}
	h.mu.Unlock()
}
