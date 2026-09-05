package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	CalendarOptionsAction = "calendar_event_options"
)

type Host interface {
	observationHost
	SaveCheckpoint(context.Context, protocol.CheckpointRequest) error
	BeginSessionExecution(context.Context, protocol.SessionExecutionRequest) error
	Log(context.Context, protocol.LogNotification) error
	CompleteSession(context.Context, protocol.CompleteSessionRequest) error
}

type eventStoreFactory func() (managedEventStore, error)

type Handler struct {
	configureMu sync.Mutex
	mu          sync.RWMutex
	host        Host
	stores      eventStoreFactory
	opener      urlOpener
	now         func() time.Time
	scene       func(calendarCard) protocol.Scene
	worker      *calendarWorker
	healthy     bool
}

type calendarWorker struct {
	instanceID      string
	generation      uint64
	host            Host
	store           managedEventStore
	state           *calendarState
	publisher       *calendarPublisher
	monitor         *calendarMonitor
	owner           *Handler
	cancel          context.CancelFunc
	done            chan struct{}
	actionMu        sync.Mutex
	session         *calendarInteractionSession
	interaction     *calendarInteractionPublisher
	checkpointDirty bool
	closed          bool
}

func New(host Host) *Handler {
	return newHandler(host, newNativeEventStore, nativeURLOpener{}, time.Now, calendarScene)
}

func DefinitionForVersion(version string) pluginsdk.Definition {
	return pluginsdk.Definition{
		ID: PluginID, Version: version,
		Contract: pluginsdk.Contract{
			ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident, protocol.ExecutionModeInteractive},
			Channels:       []protocol.Channel{{ID: ChannelUpcoming}, {ID: ChannelActive}, {ID: ChannelInteraction}},
			Operations:     []protocol.OperationDescriptor{{ID: OperationCalendars, Kind: protocol.OperationQuery}},
		},
		New: func(host *pluginsdk.Host) pluginsdk.Plugin { return New(host) },
	}
}

func newHandler(
	host Host,
	stores eventStoreFactory,
	opener urlOpener,
	now func() time.Time,
	scene func(calendarCard) protocol.Scene,
) *Handler {
	if now == nil {
		now = time.Now
	}
	return &Handler{host: host, stores: stores, opener: opener, now: now, scene: scene, healthy: true}
}

func (h *Handler) ReplaceInstances(ctx context.Context, instances []protocol.Instance) error {
	var selected *protocol.Instance
	var config Config
	for index := range instances {
		instance := instances[index]
		if selected != nil {
			return pluginsdk.PermanentConfiguration(errors.New("Calendar supports at most one enabled instance"))
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

	var store managedEventStore
	if selected != nil {
		if h.stores == nil {
			return pluginsdk.PermanentConfiguration(errors.New("Calendar event store factory is unavailable"))
		}
		var err error
		store, err = h.stores()
		if err != nil {
			if errors.Is(err, ErrUnsupported) {
				return pluginsdk.PermanentConfiguration(err)
			}
			return fmt.Errorf("create Calendar event store: %w", err)
		}
	}

	h.configureMu.Lock()
	defer h.configureMu.Unlock()
	h.mu.RLock()
	previous := h.worker
	h.mu.RUnlock()
	if err := h.stopWorker(ctx); err != nil {
		if store != nil {
			_ = store.Close()
		}
		return err
	}
	if selected == nil {
		return nil
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	state := newCalendarStateWithConfig(store, h.opener, h.now, config)
	if len(selected.Checkpoint) != 0 {
		state.RestoreCheckpoint(selected.Checkpoint)
	}
	publisher := newCalendarPublisher(h.host, selected.Ref(), config, h.now, h.scene)
	if previous != nil && previous.instanceID == selected.ID && previous.generation == selected.Generation {
		publisher = previous.publisher
	}
	worker := &calendarWorker{
		instanceID: selected.ID, generation: selected.Generation, host: h.host, store: store,
		state: state, publisher: publisher, owner: h, cancel: cancel, done: make(chan struct{}),
		interaction: newCalendarInteractionPublisher(h.host, selected.Ref(), h.now, h.scene),
	}
	worker.monitor = newCalendarMonitor(state, store, nil, nil, nil, worker.apply)
	h.mu.Lock()
	h.worker, h.healthy = worker, true
	h.mu.Unlock()
	go worker.run(workerCtx)
	return nil
}

func (h *Handler) StartSession(ctx context.Context, request protocol.SessionStartRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	h.mu.RLock()
	worker := h.worker
	h.mu.RUnlock()
	if worker == nil || request.Instance != (protocol.InstanceRef{ID: worker.instanceID, Generation: worker.generation}) {
		return errors.New("Calendar instance is not active")
	}
	if request.Trigger == nil {
		return errors.New("Calendar session trigger is required")
	}
	if request.Action == CalendarOpenAction && request.Trigger.Kind == protocol.SessionTriggerLauncher && request.Trigger.Observation == nil {
		return worker.startLauncherSession(ctx, request.SessionToken)
	}
	if !calendarOptionsAction(request.Action) || request.Trigger.Kind != protocol.SessionTriggerObservation || request.Trigger.Observation == nil {
		return errors.New("Calendar meeting action is invalid")
	}
	trigger := request.Trigger.Observation
	if (trigger.Channel != ChannelUpcoming && trigger.Channel != ChannelActive) || trigger.Key == "" || trigger.Revision == 0 ||
		!worker.publisher.Matches(trigger.Channel, trigger.Key, trigger.Revision) {
		return ErrStaleMeetingAction
	}
	worker.actionMu.Lock()
	defer worker.actionMu.Unlock()
	if worker.closed {
		return errors.New("Calendar worker is stopped")
	}
	if worker.session != nil {
		return errors.New("Calendar meeting chooser is already active")
	}
	choices, ok := worker.state.Choices(trigger.Key)
	if !ok || len(choices) == 0 {
		return ErrStaleMeetingAction
	}
	session := &calendarInteractionSession{token: request.SessionToken, eventKey: trigger.Key, choices: choices}
	worker.session = session
	if err := worker.interaction.Publish(ctx, session); err != nil {
		worker.session = nil
		return err
	}
	return nil
}

func (w *calendarWorker) startLauncherSession(ctx context.Context, token string) error {
	w.actionMu.Lock()
	defer w.actionMu.Unlock()
	if w.closed {
		return errors.New("Calendar worker is stopped")
	}
	if w.session != nil {
		return errors.New("Calendar session is already active")
	}
	session := &calendarInteractionSession{token: token, launcher: true, card: w.state.LauncherCard()}
	w.session = session
	if err := w.interaction.Publish(ctx, session); err != nil {
		w.session = nil
		return err
	}
	return nil
}

func calendarOptionsAction(action string) bool {
	return action == CalendarOptionsAction
}

func (h *Handler) EndSession(ctx context.Context, request protocol.SessionEndRequest) error {
	h.mu.RLock()
	worker := h.worker
	h.mu.RUnlock()
	if worker == nil || request.Instance != (protocol.InstanceRef{ID: worker.instanceID, Generation: worker.generation}) || request.SessionToken == "" {
		return nil
	}
	worker.actionMu.Lock()
	defer worker.actionMu.Unlock()
	return worker.finishInteraction(ctx, request.SessionToken, false)
}

func (h *Handler) InvokeOperation(ctx context.Context, request protocol.OperationRequest) (protocol.OperationResult, error) {
	h.mu.RLock()
	worker := h.worker
	h.mu.RUnlock()
	if worker == nil || request.Instance != (protocol.InstanceRef{ID: worker.instanceID, Generation: worker.generation}) {
		return protocol.OperationResult{}, errors.New("Calendar instance is not active")
	}
	payload := bytes.TrimSpace(request.Payload)
	if request.Operation != OperationCalendars ||
		(len(payload) != 0 && !bytes.Equal(payload, []byte(`{}`))) {
		return protocol.OperationResult{}, errors.New("Calendar operation is unsupported")
	}
	worker.actionMu.Lock()
	defer worker.actionMu.Unlock()
	if worker.closed {
		return protocol.OperationResult{}, errors.New("Calendar worker is stopped")
	}
	checkpointChanged, err := worker.state.RefreshCatalog(ctx)
	if err != nil {
		return protocol.OperationResult{}, err
	}
	worker.checkpointDirty = worker.checkpointDirty || checkpointChanged
	if worker.checkpointDirty {
		if err := worker.persistCheckpoint(ctx); err != nil {
			worker.state.RetryAfter(calendarPublicationRetry)
			if worker.owner != nil {
				worker.owner.setHealth(worker, false, "calendar_checkpoint_failed", "Calendar catalog checkpoint failed")
			}
			return protocol.OperationResult{}, err
		}
	}
	payload, err = json.Marshal(struct {
		Calendars []calendarCatalogEntry `json:"calendars"`
	}{Calendars: worker.state.CalendarCatalog()})
	return protocol.OperationResult{Payload: payload}, err
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
	worker.cancel()
	select {
	case <-worker.done:
		h.mu.Lock()
		if h.worker == worker {
			h.worker = nil
		}
		h.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *calendarWorker) run(ctx context.Context) {
	defer close(w.done)
	_ = w.monitor.Run(ctx)
	w.actionMu.Lock()
	w.closed = true
	_ = w.store.Close()
	w.actionMu.Unlock()
}

func (w *calendarWorker) apply(ctx context.Context, result calendarRefresh, refreshErr error) {
	w.actionMu.Lock()
	defer w.actionMu.Unlock()
	w.checkpointDirty = w.checkpointDirty || result.CheckpointChanged
	if refreshErr != nil {
		publicationErr := w.publisher.Publish(ctx, selectedEvents{})
		event := "calendar_refresh_failed"
		message := "Calendar event refresh failed"
		if errors.Is(refreshErr, ErrCalendarAccess) {
			event = "calendar_access_unavailable"
			message = "Calendar full access is unavailable"
		}
		if publicationErr != nil {
			w.state.RetryAfter(calendarPublicationRetry)
			event = "calendar_publication_failed"
			message = "Calendar observation publication failed"
		}
		w.owner.setHealth(w, false, event, message)
		return
	}
	if err := w.publisher.Publish(ctx, result.selectedEvents); err != nil {
		w.state.RetryAfter(calendarPublicationRetry)
		w.owner.setHealth(w, false, "calendar_publication_failed", "Calendar observation publication failed")
		return
	}
	if w.checkpointDirty {
		if err := w.persistCheckpoint(ctx); err != nil {
			w.state.RetryAfter(calendarPublicationRetry)
			w.owner.setHealth(w, false, "calendar_checkpoint_failed", "Calendar attendance checkpoint failed")
			return
		}
	}
	w.owner.setHealth(w, true, "calendar_recovered", "Calendar integration recovered")
}

func (h *Handler) setHealth(worker *calendarWorker, healthy bool, event, message string) {
	h.mu.Lock()
	if h.worker != worker {
		h.mu.Unlock()
		return
	}
	changed := h.healthy != healthy
	h.healthy = healthy
	h.mu.Unlock()
	if changed {
		level := protocol.LogLevelInfo
		if !healthy {
			level = protocol.LogLevelWarn
		}
		worker.log(context.Background(), level, event, message)
	}
}

func (w *calendarWorker) log(ctx context.Context, level protocol.LogLevel, event, message string) {
	if w.host == nil {
		return
	}
	_ = w.host.Log(ctx, protocol.LogNotification{
		Level: level, Event: event, Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, Message: message,
	})
}
