package pluginhost

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	supervisorQueueCapacity   = 32
	maxPendingSessionCleanups = 256
	operationTimeout          = 5 * time.Second
	eventTimeout              = 2 * time.Second
	sessionActionTimeout      = 7 * time.Second
	defaultHealthInterval     = 30 * time.Second
	failureWindow             = 10 * time.Minute
)

var (
	ErrPluginNotConfigured = errors.New("plugin is not configured")
	ErrPluginUnavailable   = errors.New("plugin is temporarily unavailable")
	ErrManagerClosed       = errors.New("plugin manager is closed")
	ErrPermanentStart      = errors.New("plugin cannot start with the current executable and desired state")
	errSessionCleanupLimit = errors.New("session cleanup capacity exhausted")
)

// PermanentStart classifies a start failure that cannot recover until the
// executable or desired specification changes.
func PermanentStart(err error) error {
	if err == nil {
		return ErrPermanentStart
	}
	return errors.Join(ErrPermanentStart, err)
}

type Child interface {
	Invoke(context.Context, InvokeRequest) error
	EndSession(context.Context, EndSessionRequest) error
	ReplaceInstances(context.Context, []Instance) error
	SessionInput(context.Context, protocol.SessionInputRequest) (protocol.SessionInputResult, error)
	Operation(context.Context, protocol.OperationRequest) (protocol.OperationResult, error)
	Ping(context.Context) (protocol.HealthResult, error)
	Done() <-chan error
	Stop(context.Context) error
}

type StartFunc func(context.Context, string, Spec, Callbacks) (Child, error)

type InvocationKind string
type SessionToken string
type SessionInvalidationReason string

const (
	SessionInvalidatedExit       SessionInvalidationReason = "child_exit"
	SessionInvalidatedHealth     SessionInvalidationReason = "health_failure"
	SessionInvalidatedDisabled   SessionInvalidationReason = "disabled"
	SessionInvalidatedGeneration SessionInvalidationReason = "generation_changed"
	SessionInvalidatedReplaced   SessionInvalidationReason = "child_replaced"
	SessionInvalidatedInput      SessionInvalidationReason = "session_input_failed"
)

type SessionInvalidation struct {
	PluginID   string
	InstanceID string
	Generation uint64
	Token      SessionToken
	Reason     SessionInvalidationReason
}

// SessionCleanup reports supervisor-owned session cleanup lifecycle state.
// PluginID is assigned by the authenticated supervisor, never by a child.
type SessionCleanup struct {
	PluginID     string
	Sequence     uint64
	At           time.Time
	PendingCount int
	ErrorCode    string
}

const (
	InvocationInteractive InvocationKind = "interactive"
)

type Phase string

const (
	PhaseStopped     Phase = "stopped"
	PhaseStarting    Phase = "starting"
	PhaseRunning     Phase = "running"
	PhaseUnhealthy   Phase = "unhealthy"
	PhaseBackoff     Phase = "backoff"
	PhaseQuarantined Phase = "quarantined"
	PhaseStopping    Phase = "stopping"
)

const (
	ErrorCodeStartFailed           = "start_failed"
	ErrorCodeReconcileFailed       = "reconcile_failed"
	ErrorCodeInvokeFailed          = "invoke_failed"
	ErrorCodeEndSessionFailed      = "end_session_failed"
	ErrorCodeSessionInputFailed    = "session_input_failed"
	ErrorCodeOperationFailed       = "operation_failed"
	ErrorCodeHealthTimeout         = "health_timeout"
	ErrorCodeHealthReported        = "health_reported_unhealthy"
	ErrorCodeUnexpectedExit        = "unexpected_exit"
	ErrorCodeStopFailed            = "stop_failed"
	ErrorCodeUnsupportedPlatform   = "unsupported_platform"
	ErrorCodeConfigurationRejected = "configuration_rejected"
)

type PluginStatus struct {
	ID                        string    `json:"id"`
	Desired                   bool      `json:"desired"`
	Phase                     Phase     `json:"phase"`
	Running                   bool      `json:"running"`
	Healthy                   bool      `json:"healthy"`
	HealthMisses              int       `json:"health_misses"`
	ExitCount                 int       `json:"exit_count"`
	RetryAt                   time.Time `json:"retry_at,omitempty"`
	LastErrorCode             string    `json:"last_error_code,omitempty"`
	LastErrorAt               time.Time `json:"last_error_at,omitempty"`
	SessionLifecycleErrorCode string    `json:"session_lifecycle_error_code,omitempty"`
	SessionLifecycleErrorAt   time.Time `json:"session_lifecycle_error_at,omitempty"`
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

type JitterFunc func() float64
type ManagerOption func(*managerOptions)

func WithClock(clock Clock) ManagerOption {
	return func(options *managerOptions) {
		if clock != nil {
			options.clock = clock
		}
	}
}

func WithJitter(jitter JitterFunc) ManagerOption {
	return func(options *managerOptions) {
		if jitter != nil {
			options.jitter = jitter
		}
	}
}

func WithHealthInterval(interval time.Duration) ManagerOption {
	return func(options *managerOptions) {
		if interval > 0 {
			options.healthInterval = interval
		}
	}
}

type managerOptions struct {
	clock          Clock
	jitter         JitterFunc
	healthInterval time.Duration
}

type Manager struct {
	mu                     sync.Mutex
	lifecycleMu            sync.Mutex
	coreVersion            string
	start                  StartFunc
	callbacks              Callbacks
	options                managerOptions
	supervisors            map[string]*supervisor
	closeErrors            []error
	changes                chan struct{}
	childIncarnation       atomic.Uint64
	sessionCleanupSequence atomic.Uint64
	closed                 bool
}

func NewManager(coreVersion string, start StartFunc, callbacks Callbacks, optionValues ...ManagerOption) *Manager {
	if start == nil {
		start = func(ctx context.Context, version string, spec Spec, callbacks Callbacks) (Child, error) {
			return Start(ctx, version, spec, callbacks)
		}
	}
	random := rand.New(rand.NewSource(time.Now().UnixNano()))
	var randomMu sync.Mutex
	options := managerOptions{
		clock: realClock{},
		jitter: func() float64 {
			randomMu.Lock()
			defer randomMu.Unlock()
			return 0.8 + random.Float64()*0.4
		},
		healthInterval: defaultHealthInterval,
	}
	for _, option := range optionValues {
		option(&options)
	}
	return &Manager{coreVersion: coreVersion, start: start, callbacks: callbacks, options: options, supervisors: make(map[string]*supervisor), changes: make(chan struct{}, 1)}
}

func (m *Manager) Changes() <-chan struct{} { return m.changes }

func (m *Manager) Apply(ctx context.Context, specs []Spec) error {
	next := make(map[string]Spec, len(specs))
	for _, spec := range specs {
		if err := validateSpec(spec); err != nil {
			return fmt.Errorf("plugin %q: %w", spec.ID, err)
		}
		if _, exists := next[spec.ID]; exists {
			return fmt.Errorf("plugin %q is duplicated", spec.ID)
		}
		next[spec.ID] = cloneSpec(spec)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	type update struct {
		id         string
		supervisor *supervisor
		command    applyCommand
		remove     bool
		created    bool
		replace    *Spec
	}
	type result struct {
		update update
		err    error
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	updates := make([]update, 0, len(next)+len(m.supervisors))
	for id, spec := range next {
		current := m.supervisors[id]
		if current != nil && current.shutdownRequested.Load() {
			replacement := cloneSpec(spec)
			updates = append(updates, update{id: id, supervisor: current, remove: true, replace: &replacement})
			continue
		}
		created := current == nil
		if created {
			current = newSupervisor(m, id)
			m.supervisors[id] = current
		}
		updates = append(updates, update{id: id, supervisor: current, command: applyCommand{ctx: ctx, spec: spec}, created: created})
	}
	for id, current := range m.supervisors {
		if _, remains := next[id]; remains {
			continue
		}
		current.requestShutdown()
		m.ownTeardown(id, current)
		updates = append(updates, update{id: id, supervisor: current, remove: true})
	}
	m.mu.Unlock()
	results := make(chan result, len(updates))
	for _, current := range updates {
		current := current
		go func() {
			var err error
			if current.remove {
				err = current.supervisor.join(ctx)
			} else {
				err = current.supervisor.call(ctx, current.command)
			}
			results <- result{update: current, err: err}
		}()
	}
	var errs []error
	retired := make([]update, 0)
	for range updates {
		current := <-results
		if current.err != nil {
			errs = append(errs, current.err)
			if current.update.created {
				current.update.supervisor.requestShutdown()
				m.ownTeardown(current.update.id, current.update.supervisor)
			}
		}
		if current.update.replace != nil {
			select {
			case <-current.update.supervisor.joined:
				retired = append(retired, current.update)
			default:
			}
		}
	}
	m.pruneJoinedSupervisors()
	if len(retired) == 0 || ctx.Err() != nil {
		return errors.Join(errs...)
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		errs = append(errs, ErrManagerClosed)
		return errors.Join(errs...)
	}
	replacements := make([]update, 0, len(retired))
	for _, old := range retired {
		current := m.supervisors[old.id]
		if current != nil && current != old.supervisor {
			continue
		}
		if current == old.supervisor {
			delete(m.supervisors, old.id)
		}
		current = newSupervisor(m, old.id)
		m.supervisors[old.id] = current
		replacements = append(replacements, update{id: old.id, supervisor: current, command: applyCommand{ctx: ctx, spec: *old.replace}, created: true})
	}
	m.mu.Unlock()
	results = make(chan result, len(replacements))
	for _, current := range replacements {
		current := current
		go func() {
			results <- result{update: current, err: current.supervisor.call(ctx, current.command)}
		}()
	}
	for range replacements {
		current := <-results
		if current.err == nil {
			continue
		}
		errs = append(errs, current.err)
		current.update.supervisor.requestShutdown()
		m.ownTeardown(current.update.id, current.update.supervisor)
	}
	return errors.Join(errs...)
}

func (m *Manager) Invoke(ctx context.Context, pluginID string, request InvokeRequest, kind InvocationKind, token SessionToken) error {
	current, err := m.lookup(pluginID)
	if err != nil {
		return err
	}
	return current.call(ctx, invokeCommand{ctx: ctx, request: request, kind: kind, token: token})
}

func (m *Manager) EndSession(ctx context.Context, pluginID string, target protocol.InstanceRef, token SessionToken) error {
	current, err := m.lookup(pluginID)
	if err != nil {
		return err
	}
	return current.call(ctx, endSessionCommand{ctx: ctx, target: target, token: token})
}

// CompleteSession releases the exact child-completed interactive session
// without sending a redundant end-session request back to that child.
func (m *Manager) CompleteSession(ctx context.Context, pluginID string, request protocol.CompleteSessionRequest) error {
	current, err := m.lookup(pluginID)
	if err != nil {
		return err
	}
	return current.call(ctx, completeSessionCommand{
		ctx: ctx, target: request.Instance, token: SessionToken(request.SessionToken),
	})
}

func (m *Manager) Restart(ctx context.Context, pluginID string) error {
	current, err := m.lookup(pluginID)
	if err != nil {
		return err
	}
	return current.call(ctx, restartCommand{ctx: ctx})
}

func (m *Manager) SessionInputResult(ctx context.Context, pluginID string, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	current, err := m.lookup(pluginID)
	if err != nil {
		return protocol.SessionInputResult{}, err
	}
	var result protocol.SessionInputResult
	err = current.call(ctx, sessionInputCommand{ctx: ctx, request: request, result: &result})
	if err != nil {
		return protocol.SessionInputResult{}, err
	}
	return result, nil
}

func (m *Manager) Operation(ctx context.Context, pluginID string, request protocol.OperationRequest) (protocol.OperationResult, error) {
	current, err := m.lookup(pluginID)
	if err != nil {
		return protocol.OperationResult{}, err
	}
	var result protocol.OperationResult
	err = current.call(ctx, operationCommand{ctx: ctx, request: request, result: &result})
	if err != nil {
		return protocol.OperationResult{}, err
	}
	return result, nil
}

func (m *Manager) Status() []PluginStatus {
	m.mu.Lock()
	current := make([]*supervisor, 0, len(m.supervisors))
	for _, value := range m.supervisors {
		if value.shutdownRequested.Load() {
			continue
		}
		current = append(current, value)
	}
	m.mu.Unlock()
	result := make([]PluginStatus, 0, len(current))
	for _, value := range current {
		result = append(result, value.snapshot())
	}
	slices.SortFunc(result, func(left, right PluginStatus) int { return cmp.Compare(left.ID, right.ID) })
	return result
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	current := make([]*supervisor, 0, len(m.supervisors))
	for _, value := range m.supervisors {
		current = append(current, value)
	}
	for id, value := range m.supervisors {
		value.requestShutdown()
		m.ownTeardown(id, value)
	}
	m.mu.Unlock()
	type closeResult struct {
		supervisor *supervisor
		err        error
	}
	results := make(chan closeResult, len(current))
	for _, value := range current {
		value := value
		go func() {
			results <- closeResult{supervisor: value, err: value.join(ctx)}
		}()
	}
	var errs []error
	for range current {
		result := <-results
		if result.err == nil {
			continue
		}
		if errors.Is(result.err, context.Canceled) || errors.Is(result.err, context.DeadlineExceeded) || result.supervisor.errorReported.CompareAndSwap(false, true) {
			errs = append(errs, result.err)
		}
	}
	m.mu.Lock()
	errs = append(errs, m.closeErrors...)
	m.closeErrors = nil
	m.mu.Unlock()
	return errors.Join(errs...)
}

func (m *Manager) ownTeardown(id string, current *supervisor) {
	current.cleanupOnce.Do(func() {
		go func() {
			<-current.joined
			err := current.shutdownError()
			m.mu.Lock()
			if m.supervisors[id] == current {
				delete(m.supervisors, id)
			}
			if m.closed && err != nil && current.errorReported.CompareAndSwap(false, true) {
				m.closeErrors = append(m.closeErrors, err)
			}
			m.mu.Unlock()
		}()
	})
}

func (m *Manager) pruneJoinedSupervisors() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, current := range m.supervisors {
		if !current.shutdownRequested.Load() {
			continue
		}
		select {
		case <-current.joined:
			if m.supervisors[id] == current {
				delete(m.supervisors, id)
			}
		default:
		}
	}
}

func (m *Manager) lookup(id string) (*supervisor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrManagerClosed
	}
	current := m.supervisors[id]
	if current == nil || current.shutdownRequested.Load() {
		return nil, ErrPluginNotConfigured
	}
	return current, nil
}

func (m *Manager) notify() {
	select {
	case m.changes <- struct{}{}:
	default:
	}
}
