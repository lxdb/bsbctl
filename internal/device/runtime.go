package device

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	busylib "github.com/lxdb/busylib-go"
	publicstream "github.com/lxdb/busylib-go/stream"
)

var ErrDeviceUnavailable = errors.New("BUSY Bar device is unavailable")

type Phase string

const (
	PhaseUnavailable Phase = "unavailable"
	PhaseConnecting  Phase = "connecting"
	PhaseReady       Phase = "ready"
	PhaseBackoff     Phase = "backoff"
)

const (
	errorAccessTokenUnavailable = "access_token_unavailable"
	errorClientUnavailable      = "client_unavailable"
)

// RuntimeStatus is deliberately safe for local diagnostics. It never contains
// device addresses, secret references or values, or raw dependency errors.
type RuntimeStatus struct {
	Phase           Phase     `json:"phase"`
	Attempt         int       `json:"attempt"`
	RetryAt         time.Time `json:"retry_at,omitempty"`
	LastErrorCode   string    `json:"last_error_code,omitempty"`
	LastConnectedAt time.Time `json:"last_connected_at,omitempty"`
	LastStateAt     time.Time `json:"last_state_at,omitempty"`
}

func (s RuntimeStatus) MarshalJSON() ([]byte, error) {
	type runtimeStatusJSON struct {
		Phase           Phase      `json:"phase"`
		Attempt         int        `json:"attempt"`
		RetryAt         *time.Time `json:"retry_at,omitempty"`
		LastErrorCode   string     `json:"last_error_code,omitempty"`
		LastConnectedAt *time.Time `json:"last_connected_at,omitempty"`
		LastStateAt     *time.Time `json:"last_state_at,omitempty"`
	}
	return json.Marshal(runtimeStatusJSON{
		Phase: s.Phase, Attempt: s.Attempt, RetryAt: nonZeroTime(s.RetryAt), LastErrorCode: s.LastErrorCode,
		LastConnectedAt: nonZeroTime(s.LastConnectedAt), LastStateAt: nonZeroTime(s.LastStateAt),
	})
}

func nonZeroTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

type StreamHealthPhase string

const (
	StreamCreating         StreamHealthPhase = "creating"
	StreamStarting         StreamHealthPhase = "starting"
	StreamStatusTransition StreamHealthPhase = "status"
	StreamTerminal         StreamHealthPhase = "terminal"
	StreamBackoff          StreamHealthPhase = "backoff"
)

type StreamHealth struct {
	Phase       StreamHealthPhase
	Lifecycle   publicstream.Lifecycle
	Access      publicstream.AccessStatus
	Attempt     int
	RetryAt     time.Time
	ConnectedAt time.Time
	LastStateAt time.Time
	ErrorCode   string
}

type StreamHealthObserver interface {
	ObserveStatusStream(StreamHealth)
}

type SecretResolver interface {
	Resolve(context.Context, string) (string, error)
}

// Client is the complete device surface shared by every runtime proxy.
type Client interface {
	Draw(context.Context, busylib.DisplayElements) error
	Clear(context.Context, string) error
	UploadFile(context.Context, string, string, string) error
	ReadTo(context.Context, string, io.Writer) (int64, error)
	Remove(context.Context, string) error
	NewStatusStream() (publicstream.Stream, error)
}

type BusyTimerClient interface {
	BusySnapshot(context.Context) (busylib.BusySnapshot, error)
	SetBusySnapshot(context.Context, busylib.BusySnapshot) error
	DeviceTime(context.Context) (busylib.TimestampInfo, error)
}

type AudioClient interface {
	PlayAudio(context.Context, busylib.PlayAudio) error
}

type identityClient interface {
	DeviceIdentity(context.Context) (DeviceIdentity, error)
}

// DeviceIdentity is the bounded diagnostic tuple read on each connection.
type DeviceIdentity struct {
	APISemVer       string
	SerialNumber    string
	OTPModel        string
	OTPValid        bool
	FirmwareTarget  int
	FirmwareVersion string
	FirmwareCommit  string
	FirmwareDirty   string
	Uptime          string
}

type ClientFactory func(context.Context, string, string) (Client, error)
type DelayFunc func(context.Context, time.Duration) error

type RuntimeConfig struct {
	BaseURL              string
	AccessTokenReference string
	Resolver             SecretResolver
	Factory              ClientFactory
	Delay                DelayFunc
	Clock                func() time.Time
	Jitter               func() float64
}

// Runtime resolves device credentials and constructs the process-wide client
// without coupling transient device failure to daemon availability.
type Runtime struct {
	mu      sync.RWMutex
	config  RuntimeConfig
	client  Client
	status  RuntimeStatus
	changes chan struct{}
	runMu   sync.Mutex
	running bool
}

func NewRuntime(config RuntimeConfig) *Runtime {
	if config.Factory == nil {
		config.Factory = newBusylibClient
	}
	if config.Delay == nil {
		config.Delay = waitRuntimeDelay
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Jitter == nil {
		config.Jitter = func() float64 { return .8 + rand.Float64()*.4 }
	}
	return &Runtime{config: config, status: RuntimeStatus{Phase: PhaseUnavailable}, changes: make(chan struct{}, 1)}
}

func (r *Runtime) Run(ctx context.Context) error {
	r.runMu.Lock()
	if r.running {
		r.runMu.Unlock()
		return errors.New("device runtime is already running")
	}
	r.running = true
	r.runMu.Unlock()
	defer func() {
		r.runMu.Lock()
		r.running = false
		r.runMu.Unlock()
	}()

	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return nil
		}
		r.setStatus(RuntimeStatus{Phase: PhaseConnecting, Attempt: attempt})
		accessToken := ""
		if r.config.AccessTokenReference != "" {
			if r.config.Resolver == nil {
				if !r.backoff(ctx, attempt, errorAccessTokenUnavailable) {
					return nil
				}
				continue
			}
			var err error
			accessToken, err = r.config.Resolver.Resolve(ctx, r.config.AccessTokenReference)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if !r.backoff(ctx, attempt, errorAccessTokenUnavailable) {
					return nil
				}
				continue
			}
		}
		if ctx.Err() != nil {
			accessToken = ""
			return nil
		}
		client, err := r.config.Factory(ctx, r.config.BaseURL, accessToken)
		accessToken = ""
		if err != nil || client == nil {
			if !r.backoff(ctx, attempt, errorClientUnavailable) {
				return nil
			}
			continue
		}
		r.setClient(client, RuntimeStatus{Phase: PhaseConnecting, Attempt: attempt})
		<-ctx.Done()
		return nil
	}
}

func (r *Runtime) backoff(ctx context.Context, attempt int, code string) bool {
	delay := retryDelay(attempt, r.config.Jitter())
	r.setStatus(RuntimeStatus{
		Phase: PhaseBackoff, Attempt: attempt, RetryAt: r.config.Clock().UTC().Add(delay), LastErrorCode: code,
	})
	return r.config.Delay(ctx, delay) == nil && ctx.Err() == nil
}

func retryDelay(attempt int, jitter float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if jitter < .8 {
		jitter = .8
	} else if jitter > 1.2 {
		jitter = 1.2
	}
	shift := attempt - 1
	if shift > 5 {
		shift = 5
	}
	base := time.Second * time.Duration(1<<shift)
	if base > 30*time.Second {
		base = 30 * time.Second
	}
	delay := time.Duration(float64(base) * jitter)
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func (r *Runtime) Status() RuntimeStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.status
}

func (r *Runtime) DeviceIdentity(ctx context.Context) (DeviceIdentity, error) {
	r.mu.RLock()
	client := r.client
	r.mu.RUnlock()
	if client == nil {
		return DeviceIdentity{}, ErrDeviceUnavailable
	}
	identity, ok := client.(identityClient)
	if !ok {
		return DeviceIdentity{}, errors.New("device client does not expose identity")
	}
	return identity.DeviceIdentity(ctx)
}

// Changes is a coalesced edge signal. Consumers read Status after each edge.
// Buffering preserves a transition that occurs before consumer startup.
func (r *Runtime) Changes() <-chan struct{} { return r.changes }

func (r *Runtime) setStatus(status RuntimeStatus) {
	r.mu.Lock()
	changed := r.status != status
	r.status = status
	r.mu.Unlock()
	if changed {
		r.signalChange()
	}
}

func (r *Runtime) setClient(client Client, status RuntimeStatus) {
	r.mu.Lock()
	changed := r.client == nil || r.status != status
	r.client = client
	r.status = status
	r.mu.Unlock()
	if changed {
		r.signalChange()
	}
}

func (r *Runtime) ObserveStatusStream(update StreamHealth) {
	r.mu.Lock()
	if r.client == nil {
		r.mu.Unlock()
		return
	}
	status := r.status
	if update.Attempt > 0 {
		status.Attempt = update.Attempt
	}
	if !update.ConnectedAt.IsZero() {
		status.LastConnectedAt = update.ConnectedAt.UTC()
	}
	if !update.LastStateAt.IsZero() {
		status.LastStateAt = update.LastStateAt.UTC()
	}
	status.RetryAt = update.RetryAt.UTC()
	status.LastErrorCode = update.ErrorCode
	if update.Phase == StreamStatusTransition && update.Lifecycle == publicstream.LifecycleConnected && update.Access == publicstream.AccessAccepted {
		status.Phase = PhaseReady
		status.RetryAt = time.Time{}
		status.LastErrorCode = ""
	} else if update.Phase == StreamBackoff || update.Phase == StreamTerminal || update.Lifecycle == publicstream.LifecycleReconnecting || update.Lifecycle == publicstream.LifecycleFailed {
		status.Phase = PhaseBackoff
	} else {
		status.Phase = PhaseConnecting
	}
	changed := r.status != status
	r.status = status
	r.mu.Unlock()
	if changed {
		r.signalChange()
	}
}

func (r *Runtime) signalChange() {
	select {
	case r.changes <- struct{}{}:
	default:
	}
}

func (r *Runtime) currentClient() (Client, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.client == nil || r.status.Phase != PhaseReady {
		return nil, ErrDeviceUnavailable
	}
	return r.client, nil
}

func (r *Runtime) Draw(ctx context.Context, request busylib.DisplayElements) error {
	client, err := r.currentClient()
	if err != nil {
		return err
	}
	return client.Draw(ctx, request)
}

func (r *Runtime) Clear(ctx context.Context, applicationName string) error {
	client, err := r.currentClient()
	if err != nil {
		return err
	}
	return client.Clear(ctx, applicationName)
}

func (r *Runtime) UploadFile(ctx context.Context, applicationName, path, localPath string) error {
	client, err := r.currentClient()
	if err != nil {
		return err
	}
	return client.UploadFile(ctx, applicationName, path, localPath)
}

func (r *Runtime) ReadTo(ctx context.Context, path string, writer io.Writer) (int64, error) {
	client, err := r.currentClient()
	if err != nil {
		return 0, err
	}
	return client.ReadTo(ctx, path, writer)
}

func (r *Runtime) Remove(ctx context.Context, path string) error {
	client, err := r.currentClient()
	if err != nil {
		return err
	}
	return client.Remove(ctx, path)
}

func (r *Runtime) BusySnapshot(ctx context.Context) (busylib.BusySnapshot, error) {
	client, err := r.currentClient()
	if err != nil {
		return busylib.BusySnapshot{}, err
	}
	timer, ok := client.(BusyTimerClient)
	if !ok {
		return busylib.BusySnapshot{}, errors.New("device client does not support BUSY timers")
	}
	return timer.BusySnapshot(ctx)
}

func (r *Runtime) SetBusySnapshot(ctx context.Context, value busylib.BusySnapshot) error {
	client, err := r.currentClient()
	if err != nil {
		return err
	}
	timer, ok := client.(BusyTimerClient)
	if !ok {
		return errors.New("device client does not support BUSY timers")
	}
	return timer.SetBusySnapshot(ctx, value)
}

func (r *Runtime) DeviceTime(ctx context.Context) (busylib.TimestampInfo, error) {
	client, err := r.currentClient()
	if err != nil {
		return busylib.TimestampInfo{}, err
	}
	timer, ok := client.(BusyTimerClient)
	if !ok {
		return busylib.TimestampInfo{}, errors.New("device client does not support device time")
	}
	return timer.DeviceTime(ctx)
}

func (r *Runtime) PlayAudio(ctx context.Context, value busylib.PlayAudio) error {
	client, err := r.currentClient()
	if err != nil {
		return err
	}
	audio, ok := client.(AudioClient)
	if !ok {
		return errors.New("device client does not support audio")
	}
	return audio.PlayAudio(ctx, value)
}

func (r *Runtime) NewStatusStream() (publicstream.Stream, error) {
	r.mu.RLock()
	client := r.client
	r.mu.RUnlock()
	if client == nil {
		return nil, ErrDeviceUnavailable
	}
	return client.NewStatusStream()
}

func waitRuntimeDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type busylibClient struct{ client *busylib.Client }

func newBusylibClient(ctx context.Context, baseURL, accessToken string) (Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	options := []busylib.Option{busylib.WithBaseURL(baseURL)}
	if accessToken != "" {
		options = append(options, busylib.WithLocalAccessToken(accessToken))
	}
	client, err := busylib.NewClient(options...)
	if err != nil {
		return nil, err
	}
	return &busylibClient{client: client}, nil
}

func (c *busylibClient) Draw(ctx context.Context, request busylib.DisplayElements) error {
	return c.client.Display().Draw(ctx, request)
}
func (c *busylibClient) Clear(ctx context.Context, applicationName string) error {
	return c.client.Display().Clear(ctx, applicationName)
}
func (c *busylibClient) UploadFile(ctx context.Context, applicationName, path, localPath string) error {
	return c.client.Assets().UploadFile(ctx, applicationName, path, localPath)
}
func (c *busylibClient) ReadTo(ctx context.Context, path string, writer io.Writer) (int64, error) {
	return c.client.Storage().ReadTo(ctx, path, writer)
}
func (c *busylibClient) Remove(ctx context.Context, path string) error {
	return c.client.Storage().Remove(ctx, path)
}
func (c *busylibClient) BusySnapshot(ctx context.Context) (busylib.BusySnapshot, error) {
	return c.client.Busy().Snapshot(ctx)
}
func (c *busylibClient) SetBusySnapshot(ctx context.Context, value busylib.BusySnapshot) error {
	return c.client.Busy().SetSnapshot(ctx, value)
}
func (c *busylibClient) DeviceTime(ctx context.Context) (busylib.TimestampInfo, error) {
	return c.client.Time().Now(ctx)
}
func (c *busylibClient) PlayAudio(ctx context.Context, value busylib.PlayAudio) error {
	return c.client.Audio().Play(ctx, value)
}
func (c *busylibClient) DeviceIdentity(ctx context.Context) (DeviceIdentity, error) {
	status, err := c.client.System().Status(ctx)
	if err != nil {
		return DeviceIdentity{}, err
	}
	return deviceIdentityFromStatus(status), nil
}

func deviceIdentityFromStatus(status busylib.Status) DeviceIdentity {
	commit, dirty := strings.CutSuffix(status.Firmware.CommitHash, "-dirty")
	return DeviceIdentity{
		APISemVer: status.System.APISemVer, SerialNumber: status.Device.SerialNumber,
		OTPModel: status.Device.OTPModel, OTPValid: status.Device.OTPValid,
		FirmwareTarget: status.Firmware.Target, FirmwareVersion: status.Firmware.Version,
		FirmwareCommit: commit, FirmwareDirty: strconv.FormatBool(dirty), Uptime: status.System.Uptime,
	}
}
func (c *busylibClient) NewStatusStream() (publicstream.Stream, error) {
	return c.client.NewStatusStream()
}
