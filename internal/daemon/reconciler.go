package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/eventbus"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

var ErrAppNotEnabled = errors.New("app is not enabled")
var ErrAppNotReady = errors.New("app generation is not ready")
var ErrAppNotFound = errors.New("app instance was not found")
var ErrAppAlreadyExists = errors.New("app instance already exists")
var ErrInvalidAppConfiguration = errors.New("app configuration is invalid")
var ErrForegroundUnavailable = errors.New("interactive foreground is unavailable")

const (
	serviceShutdownTimeout = 8 * time.Second
	sessionEndTimeout      = 5 * time.Second
)

const ConfigDurabilityUncertainCode = "config_durability_uncertain"
const SessionEndFailedCode = "end_session_failed"

type ConfigPersistenceStatus struct {
	LastErrorCode string `json:"last_error_code,omitempty"`
}

type SessionDiagnostics struct {
	ActiveInstanceID       string    `json:"active_instance_id,omitempty"`
	State                  string    `json:"state"`
	LastLifecycleErrorCode string    `json:"last_lifecycle_error_code,omitempty"`
	LastLifecycleErrorAt   time.Time `json:"last_lifecycle_error_at,omitempty"`
}

func (s SessionDiagnostics) MarshalJSON() ([]byte, error) {
	type sessionDiagnosticsJSON struct {
		ActiveInstanceID       string     `json:"active_instance_id,omitempty"`
		State                  string     `json:"state"`
		LastLifecycleErrorCode string     `json:"last_lifecycle_error_code,omitempty"`
		LastLifecycleErrorAt   *time.Time `json:"last_lifecycle_error_at,omitempty"`
	}
	var lastErrorAt *time.Time
	state := s.State
	if state == "" {
		state = string(foregroundIdle)
	}
	if !s.LastLifecycleErrorAt.IsZero() {
		value := s.LastLifecycleErrorAt.UTC()
		lastErrorAt = &value
	}
	return json.Marshal(sessionDiagnosticsJSON{
		ActiveInstanceID: s.ActiveInstanceID, State: state, LastLifecycleErrorCode: s.LastLifecycleErrorCode,
		LastLifecycleErrorAt: lastErrorAt,
	})
}

type LaunchableApp struct {
	ID       string
	PluginID string
	Action   string
}

// AppConfiguration is the complete caller-supplied replacement for one app
// instance's configurable fields. Package identity and enablement remain
// daemon-owned.
type AppConfiguration struct {
	// ExpectedGeneration is zero for no precondition or the required document generation.
	ExpectedGeneration uint64
	Config             json.RawMessage
	Secrets            map[string]string
	Policies           map[string]presentation.PolicyConfig
	LaunchAction       string
}

// EnableResult contains only facts from one enablement operation. Error is
// reserved for failures before commit; ReconciliationError reports work that
// remains daemon-owned after the returned document committed.
type EnableResult struct {
	config.Document
	Changed             bool
	Outcome             localstate.CommitOutcome
	ReconciliationError error
}

// AppInstanceResult reports the durable desired-state transaction separately
// from any live reconciliation work that remains daemon-owned.
type AppInstanceResult struct {
	config.Document
	AppID               string
	PluginID            string
	Enabled             bool
	Outcome             localstate.CommitOutcome
	ReconciliationError error
}

type DesiredStateValidator func(config.Document) error

type InstanceCleaner interface {
	RemoveInstance(pluginID, instanceID string, throughGeneration uint64)
}

type PluginController interface {
	Apply(context.Context, []pluginhost.Spec) error
	Operation(context.Context, string, protocol.OperationRequest) (protocol.OperationResult, error)
	Invoke(context.Context, string, pluginhost.InvokeRequest, pluginhost.InvocationKind, pluginhost.SessionToken) error
	EndSession(context.Context, string, protocol.InstanceRef, pluginhost.SessionToken) error
	Status() []pluginhost.PluginStatus
	Close(context.Context) error
}

type AttentionController interface {
	SelectedObservation() (observation.Record, bool)
	AttentionSnapshot() (attention.Trace, bool)
	AttentionExplain(string) (attention.Evaluation, bool)
	AttentionHistory(int, time.Time) []attention.Trace
	AcknowledgeAttention(string) error
	Wake()
	Reconcile(context.Context) error
	RecorderStatus() attention.RecorderStatus
	ObservationDiagnostics() observation.StoreDiagnostics
	AttentionStateStatus() AttentionStateDiagnostics
	PresentationCooldownStatus() PresentationCooldownDiagnostics
}

type AssetController interface {
	Reconcile(context.Context, []assets.Package)
	CollectGarbage(context.Context, []assets.Package)
	ReadyFor(assets.Package) bool
	Status() []assets.State
}

// SessionInputController owns the exact instance-to-plugin input routing plan
// for the currently accepted configuration generation.
type SessionInputController interface {
	Apply([]eventbus.TargetSet)
	Cancel(string, protocol.InstanceRef, string)
	Complete(string, protocol.InstanceRef, string)
	Status() []eventbus.Status
}

// Reconciler moves durable desired state toward live plugin state.
type Reconciler struct {
	live                *LiveState
	desired             *DesiredState
	checkpoints         *Checkpoints
	assetMu             sync.Mutex
	eventMu             sync.Mutex
	appRetirements      appRetirements
	resolver            SecretResolver
	plugins             PluginController
	sessions            *SessionRuntime
	policy              *PolicyResolver
	attentionController AttentionController
	assetController     AssetController
	retryCtx            context.Context
	retryCancel         context.CancelFunc
	retryWake           chan struct{}
	retryDone           chan struct{}
	retryStarted        bool
	retryDelay          func(int) time.Duration
	closeOnce           sync.Once
	pluginCloseDone     chan struct{}
	pluginCloseErr      error
}

type reconcileCandidate struct {
	epoch    uint64
	revision uint64
	plan     ReconciliationPlan
}

type sessionTermination struct {
	pluginID, instanceID string
	generation           uint64
	token                pluginhost.SessionToken
}

type ReconcilerOptions struct {
	Desired     *DesiredState
	Live        *LiveState
	Sessions    *SessionRuntime
	Policy      *PolicyResolver
	Checkpoints *Checkpoints
	Resolver    SecretResolver
	Plugins     PluginController
	Attention   AttentionController
	Assets      AssetController
	RetryDelay  func(int) time.Duration
}

func NewReconciler(options ReconcilerOptions) (*Reconciler, error) {
	if options.Desired == nil {
		return nil, errors.New("desired state is required")
	}
	if options.Live == nil {
		return nil, errors.New("live state is required")
	}
	if options.Sessions == nil {
		return nil, errors.New("session runtime is required")
	}
	if options.Checkpoints == nil {
		return nil, errors.New("checkpoints are required")
	}
	if options.Plugins == nil {
		return nil, errors.New("plugin controller is required")
	}
	if options.Attention == nil {
		return nil, errors.New("attention controller is required")
	}
	if options.Assets == nil {
		return nil, errors.New("asset controller is required")
	}
	if options.Policy == nil {
		return nil, errors.New("policy resolver is required")
	}
	retryDelay := options.RetryDelay
	if retryDelay == nil {
		retryDelay = func(attempt int) time.Duration { return secretRetryDelay(attempt, .8+rand.Float64()*.4) }
	}
	retryCtx, retryCancel := context.WithCancel(context.Background())
	service := &Reconciler{
		desired: options.Desired, checkpoints: options.Checkpoints, resolver: options.Resolver, plugins: options.Plugins,
		live:                options.Live,
		sessions:            options.Sessions,
		policy:              options.Policy,
		attentionController: options.Attention,
		assetController:     options.Assets,
		retryCtx:            retryCtx, retryCancel: retryCancel,
		retryWake:       make(chan struct{}, 1),
		retryDelay:      retryDelay,
		pluginCloseDone: make(chan struct{}),
	}
	return service, nil
}
