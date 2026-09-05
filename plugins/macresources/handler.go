// Package macresources publishes bounded Mac CPU, memory, and network observations.
package macresources

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/internal/livesession"
	"github.com/lxdb/bsbctl/sdk/protocol"

	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
)

type Host interface {
	PublishObservation(context.Context, protocol.Observation) error
	CompleteSession(context.Context, protocol.CompleteSessionRequest) error
	Log(context.Context, protocol.LogNotification) error
}

type Collector interface {
	Sample(context.Context) (RawSample, error)
}

type collectorAvailability interface {
	Availability() error
}

var ErrUnsupported = errors.New("mac resources collection requires macOS with cgo enabled")

const (
	observationLifetime   = 10 * time.Second
	materialChangeQuiet   = 90 * time.Second
	materialChangePercent = 15.0
)

type RawSample struct {
	CPUTotal      uint64
	CPUIdle       uint64
	MemoryPercent float64
	RXBytes       uint64
	TXBytes       uint64
	NetworkSet    uint64
	CollectedAt   time.Time
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type tickerFactory func(time.Duration) Ticker

type Handler struct {
	configureMu         sync.Mutex
	mu                  sync.RWMutex
	host                Host
	collector           Collector
	newTicker           tickerFactory
	worker              *worker
	healthy             bool
	collectionFailures  int
	publicationFailures int
}

type worker struct {
	instanceID      string
	generation      uint64
	config          Config
	host            Host
	collector       Collector
	ticker          Ticker
	owner           *Handler
	cancel          context.CancelFunc
	done            chan struct{}
	pressure        *pressureMachine
	summaryRev      uint64
	pressureRev     uint64
	pendingPressure *protocol.Observation
	lastSummary     reading
	lastSummaryAt   time.Time
	hasSummary      bool
	lastPressureAt  time.Time
	previous        RawSample
	hasPrevious     bool
	last            reading
	liveOnce        sync.Once
	live            *livesession.Session
}

func New(host Host) *Handler {
	return newHandler(host, nil, nil)
}

func newHandler(host Host, collector Collector, factory tickerFactory) *Handler {
	if collector == nil {
		collector = NewNativeCollector()
	}
	if factory == nil {
		factory = func(interval time.Duration) Ticker { return realTicker{Ticker: time.NewTicker(interval)} }
	}
	return &Handler{host: host, collector: collector, newTicker: factory, healthy: true}
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

func (h *Handler) ReplaceInstances(ctx context.Context, configured []protocol.Instance) error {
	var selected *protocol.Instance
	var config Config
	for index := range configured {
		item := configured[index]
		if selected != nil {
			return pluginsdk.PermanentConfiguration(errors.New("mac resources supports at most one enabled instance"))
		}
		if err := pluginsdk.RejectSecrets(item.ID, item.Secrets); err != nil {
			return err
		}
		decoded, err := decodeConfig(item.Config)
		if err != nil {
			return pluginsdk.PermanentConfiguration(fmt.Errorf("instance %q: %w", item.ID, err))
		}
		selected = &item
		config = decoded
	}
	if selected != nil {
		if availability, ok := h.collector.(collectorAvailability); ok {
			if err := availability.Availability(); err != nil {
				return pluginsdk.PermanentConfiguration(err)
			}
		}
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
	w := &worker{
		instanceID: selected.ID, generation: selected.Generation, config: config, host: h.host, collector: h.collector,
		ticker: h.newTicker(config.SampleInterval), owner: h, cancel: cancel, done: make(chan struct{}),
		pressure: newPressureMachine(config),
	}
	if previous != nil && previous.instanceID == selected.ID && previous.generation == selected.Generation {
		w.summaryRev = previous.summaryRev
		w.pressureRev = previous.pressureRev
	}
	h.mu.Lock()
	h.worker = w
	h.mu.Unlock()
	go w.run(workerCtx)
	return nil
}

func (h *Handler) Health(_ context.Context) protocol.HealthResult {
	h.mu.RLock()
	healthy := h.healthy
	h.mu.RUnlock()
	return protocol.HealthResult{Healthy: healthy, ObservedAt: time.Now().UTC()}
}

func (h *Handler) Shutdown(ctx context.Context) error {
	h.configureMu.Lock()
	defer h.configureMu.Unlock()
	return h.stopWorker(ctx)
}

func (h *Handler) StartSession(ctx context.Context, request protocol.SessionStartRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if request.Action != "open" || request.Trigger == nil || request.Trigger.Kind != protocol.SessionTriggerLauncher || request.Trigger.Observation != nil {
		return errors.New("Mac resources requires an open launcher session")
	}
	h.mu.RLock()
	w := h.worker
	h.mu.RUnlock()
	if w == nil || request.Instance != (protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}) {
		return errors.New("Mac resources instance is not active")
	}
	return w.startLive(ctx, request.SessionToken)
}

func (h *Handler) EndSession(ctx context.Context, request protocol.SessionEndRequest) error {
	h.mu.RLock()
	w := h.worker
	h.mu.RUnlock()
	if w == nil || request.Instance != (protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}) || request.SessionToken == "" {
		return nil
	}
	return w.finishLive(ctx, request.SessionToken, false)
}

func (h *Handler) HandleSessionInput(ctx context.Context, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	h.mu.RLock()
	w := h.worker
	h.mu.RUnlock()
	if w == nil || request.Instance != (protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}) || request.SessionToken == "" {
		return resourcesInputResult(false), nil
	}
	button := request.Input.Button
	if button == nil || button.Button != protocol.ButtonBack || button.Action != protocol.ButtonPress {
		return resourcesInputResult(false), nil
	}
	return resourcesInputResult(true), w.finishLive(ctx, request.SessionToken, true)
}

func resourcesInputResult(consumed bool) protocol.SessionInputResult {
	if consumed {
		return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}
	}
	return protocol.SessionInputResult{Disposition: protocol.SessionInputNotConsumed}
}

func (h *Handler) stopWorker(ctx context.Context) error {
	h.mu.RLock()
	w := h.worker
	h.mu.RUnlock()
	if w == nil {
		return nil
	}
	_ = w.finishLive(ctx, "", false)
	w.cancel()
	select {
	case <-w.done:
		h.mu.Lock()
		if h.worker == w {
			h.worker = nil
		}
		h.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *worker) run(ctx context.Context) {
	defer close(w.done)
	defer w.ticker.Stop()
	w.collect(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.ticker.C():
			w.collect(ctx)
		}
	}
}

func (w *worker) collect(ctx context.Context) {
	sample, err := w.collector.Sample(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.owner.recordFailure(ctx, w.instanceID, w.generation, "collection_failed")
		}
		return
	}
	w.owner.recordSuccess(ctx, w.instanceID, w.generation)
	if !w.hasPrevious {
		w.previous = sample
		w.hasPrevious = true
		return
	}
	value, ok := deriveReading(w.previous, sample, w.last)
	w.previous = sample
	if !ok {
		return
	}
	w.last = value
	if err := w.publish(ctx, sample.CollectedAt.UTC(), value); err != nil {
		if ctx.Err() == nil {
			w.owner.recordPublicationFailure(ctx, w.instanceID, w.generation)
		}
		return
	}
	w.owner.recordPublicationSuccess(ctx, w.instanceID, w.generation)
}

func deriveReading(previous, current RawSample, last reading) (reading, bool) {
	elapsed := current.CollectedAt.Sub(previous.CollectedAt).Seconds()
	if elapsed <= 0 {
		return last, false
	}
	result := last
	result.MemoryPercent = clampPercent(current.MemoryPercent)
	if current.CPUTotal >= previous.CPUTotal && current.CPUIdle >= previous.CPUIdle {
		total := current.CPUTotal - previous.CPUTotal
		idle := current.CPUIdle - previous.CPUIdle
		if total > 0 && idle <= total {
			result.CPUPercent = clampPercent(float64(total-idle) / float64(total) * 100)
		}
	}
	if current.NetworkSet == previous.NetworkSet && current.RXBytes >= previous.RXBytes {
		result.RXBytesPerSecond = float64(current.RXBytes-previous.RXBytes) / elapsed
	}
	if current.NetworkSet == previous.NetworkSet && current.TXBytes >= previous.TXBytes {
		result.TXBytesPerSecond = float64(current.TXBytes-previous.TXBytes) / elapsed
	}
	return result, true
}

func (w *worker) publish(ctx context.Context, now time.Time, value reading) error {
	residentErr := w.publishResident(ctx, now, value)
	liveErr := w.publishLiveReading(ctx, now, value)
	return errors.Join(residentErr, liveErr)
}

func (w *worker) publishResident(ctx context.Context, now time.Time, value reading) error {
	if w.pendingPressure != nil {
		if w.pendingPressure.Disposition != protocol.DispositionResolved && !w.pendingPressure.ValidUntil.After(now) {
			w.pressureRev++
			refreshed := *w.pendingPressure
			refreshed.Revision = w.pressureRev
			refreshed.ObservedAt = now
			refreshed.UpdatedAt = now
			refreshed.ValidUntil = now.Add(observationLifetime)
			level := pressureWarning
			if refreshed.Disposition == protocol.DispositionActionable {
				level = pressureCritical
			}
			refreshed.Scene = new(pressureScene(value, w.config, pressureState{level: level, reason: refreshed.ReasonCode}))
			w.pendingPressure = &refreshed
		}
		if err := w.host.PublishObservation(ctx, *w.pendingPressure); err != nil {
			return fmt.Errorf("publish resource pressure: %w", err)
		}
		if w.pendingPressure.Disposition != protocol.DispositionResolved {
			w.lastPressureAt = now
		}
		w.pendingPressure = nil
		return nil
	}
	networkPercent := clampPercent((value.RXBytesPerSecond + value.TXBytesPerSecond) / w.config.NetworkCapacityBytesPerSecond * 100)
	state := w.pressure.update(pressureValues{CPU: value.CPUPercent, Memory: value.MemoryPercent, Network: networkPercent})
	if state.level != pressureNormal || state.transition {
		if state.transition || w.lastPressureAt.IsZero() || !now.Before(w.lastPressureAt.Add(w.config.SummaryInterval)) {
			if state.level == pressureNormal {
				w.resetSummaryBaseline(now, value)
			}
			return w.publishPressure(ctx, now, state, value)
		}
		return nil
	}
	if !w.summaryDue(now, value) {
		return nil
	}
	return w.publishSummary(ctx, now, value, summaryScene(value, w.config))
}

func (w *worker) publishSummary(ctx context.Context, now time.Time, value reading, scene protocol.Scene) error {
	w.summaryRev++
	if err := w.host.PublishObservation(ctx, protocol.Observation{
		Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, Channel: ChannelSummary, Key: observationKey, Revision: w.summaryRev,
		Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNormal, ReasonCode: "resource_snapshot",
		ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(observationLifetime), Scene: new(scene),
	}); err != nil {
		return fmt.Errorf("publish resource summary: %w", err)
	}
	w.resetSummaryBaseline(now, value)
	return nil
}

func (w *worker) publishPressure(ctx context.Context, now time.Time, state pressureState, value reading) error {
	w.pressureRev++
	pressure := protocol.Observation{
		Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, Channel: ChannelPressure, Key: observationKey, Revision: w.pressureRev,
		ReasonCode: state.reason, ObservedAt: now, UpdatedAt: now,
	}
	switch state.level {
	case pressureCritical:
		pressure.Disposition = protocol.DispositionActionable
		pressure.Impact = protocol.ImpactCritical
		pressure.ValidUntil = now.Add(observationLifetime)
		pressure.Scene = new(pressureScene(value, w.config, state))
	case pressureWarning:
		pressure.Disposition = protocol.DispositionNotable
		pressure.Impact = protocol.ImpactNotable
		pressure.ValidUntil = now.Add(observationLifetime)
		pressure.Scene = new(pressureScene(value, w.config, state))
	default:
		pressure.Disposition = protocol.DispositionResolved
		pressure.Impact = protocol.ImpactNormal
	}
	if err := w.host.PublishObservation(ctx, pressure); err != nil {
		w.pendingPressure = &pressure
		return fmt.Errorf("publish resource pressure: %w", err)
	}
	if state.level != pressureNormal {
		w.lastPressureAt = now
	}
	return nil
}

func (w *worker) summaryDue(now time.Time, value reading) bool {
	if !w.hasSummary {
		return true
	}
	if !now.Before(w.lastSummaryAt.Add(w.config.SummaryInterval)) {
		return true
	}
	return !now.Before(w.lastSummaryAt.Add(materialChangeQuiet)) && materiallyChanged(w.lastSummary, value, w.config)
}

func (w *worker) resetSummaryBaseline(now time.Time, value reading) {
	w.lastSummary = value
	w.lastSummaryAt = now
	w.hasSummary = true
}

func materiallyChanged(previous, current reading, config Config) bool {
	return math.Abs(current.CPUPercent-previous.CPUPercent) >= materialChangePercent ||
		math.Abs(current.MemoryPercent-previous.MemoryPercent) >= materialChangePercent ||
		math.Abs(networkUtilization(current, config)-networkUtilization(previous, config)) >= materialChangePercent
}

func networkUtilization(value reading, config Config) float64 {
	return clampPercent((value.RXBytesPerSecond + value.TXBytesPerSecond) / config.NetworkCapacityBytesPerSecond * 100)
}

func (h *Handler) recordFailure(ctx context.Context, instanceID string, generation uint64, event string) {
	h.mu.Lock()
	first := h.collectionFailures == 0
	h.collectionFailures++
	becameUnavailable := h.collectionFailures == 3
	h.healthy = h.collectionFailures < 3 && h.publicationFailures < 3
	w := h.worker
	h.mu.Unlock()
	if becameUnavailable && w != nil && w.instanceID == instanceID && w.generation == generation {
		_ = w.publishLiveUnavailable(ctx)
	}
	if first && h.host != nil {
		_ = h.host.Log(ctx, protocol.LogNotification{
			Level: protocol.LogLevelWarn, Event: event, Instance: protocol.InstanceRef{ID: instanceID, Generation: generation},
			Message: "resource monitoring is temporarily unavailable",
		})
	}
}

func (w *worker) startLive(ctx context.Context, token string) error {
	return w.liveSession().Start(ctx, token, resourceStatusScene("WAITING", resourceWaitingColor), "mac_resources_live")
}

func (w *worker) publishLiveReading(ctx context.Context, now time.Time, value reading) error {
	scene := w.liveScene(value)
	return w.liveSession().SetCurrent(ctx, scene, "mac_resources_live", now)
}

func (w *worker) publishLiveUnavailable(ctx context.Context) error {
	scene := resourceStatusScene("UNAVAILABLE", resourceUnavailableColor)
	return w.liveSession().PublishTransient(ctx, scene, "mac_resources_unavailable")
}

func (w *worker) finishLive(ctx context.Context, token string, notify bool) error {
	return w.liveSession().Finish(ctx, token, notify, "mac_resources_live_closed")
}

func (w *worker) liveSession() *livesession.Session {
	w.liveOnce.Do(func() {
		w.live = livesession.New(w.host, protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, ChannelLive, "panel", 24*time.Hour, time.Now)
	})
	return w.live
}

func (w *worker) liveScene(value reading) protocol.Scene {
	state := pressureState{level: w.pressure.level, reason: w.pressure.reason}
	if state.level == pressureNormal {
		return summaryScene(value, w.config)
	}
	return pressureScene(value, w.config, state)
}

func (h *Handler) recordSuccess(ctx context.Context, instanceID string, generation uint64) {
	h.mu.Lock()
	recovered := h.collectionFailures > 0
	h.collectionFailures = 0
	h.healthy = h.publicationFailures < 3
	h.mu.Unlock()
	if recovered && h.host != nil {
		_ = h.host.Log(ctx, protocol.LogNotification{
			Level: protocol.LogLevelInfo, Event: "collection_recovered", Instance: protocol.InstanceRef{ID: instanceID, Generation: generation},
			Message: "resource monitoring recovered",
		})
	}
}

func (h *Handler) recordPublicationFailure(ctx context.Context, instanceID string, generation uint64) {
	h.mu.Lock()
	first := h.publicationFailures == 0
	h.publicationFailures++
	h.healthy = h.collectionFailures < 3 && h.publicationFailures < 3
	h.mu.Unlock()
	if first && h.host != nil {
		_ = h.host.Log(ctx, protocol.LogNotification{
			Level: protocol.LogLevelWarn, Event: "publication_failed", Instance: protocol.InstanceRef{ID: instanceID, Generation: generation},
			Message: "resource observations could not be published",
		})
	}
}

func (h *Handler) recordPublicationSuccess(ctx context.Context, instanceID string, generation uint64) {
	h.mu.Lock()
	recovered := h.publicationFailures > 0
	h.publicationFailures = 0
	h.healthy = h.collectionFailures < 3
	h.mu.Unlock()
	if recovered && h.host != nil {
		_ = h.host.Log(ctx, protocol.LogNotification{
			Level: protocol.LogLevelInfo, Event: "publication_recovered", Instance: protocol.InstanceRef{ID: instanceID, Generation: generation},
			Message: "resource observation publication recovered",
		})
	}
}

type realTicker struct{ *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.Ticker.C }
