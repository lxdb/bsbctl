package device

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	busylib "github.com/lxdb/busylib-go"
	publicstream "github.com/lxdb/busylib-go/stream"
)

type SecretResolverFunc func(context.Context, string) (string, error)

func (f SecretResolverFunc) Resolve(ctx context.Context, reference string) (string, error) {
	return f(ctx, reference)
}

func TestRuntimeUnavailableThenDelegatesToOneRecoveredClient(t *testing.T) {
	t.Parallel()
	client := &fakeRuntimeClient{statusStream: &fakeRuntimeStream{}}
	allowRetry := make(chan struct{})
	var factoryCalls atomic.Int32
	runtime := NewRuntime(RuntimeConfig{
		BaseURL: "http://busybar.test", AccessTokenReference: "keychain://bsbctl/device/access-token",
		Resolver: SecretResolverFunc(func(context.Context, string) (string, error) {
			return "resolved", nil
		}),
		Factory: func(context.Context, string, string) (Client, error) {
			if factoryCalls.Add(1) == 1 {
				return nil, errors.New("sensitive factory failure")
			}
			return client, nil
		},
		Delay: func(ctx context.Context, _ time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-allowRetry:
				return nil
			}
		},
		Jitter: func() float64 { return 1 },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	awaitRuntimePhase(t, runtime, PhaseBackoff)

	if err := runtime.Draw(context.Background(), busylib.DisplayElements{}); !errors.Is(err, ErrDeviceUnavailable) {
		t.Fatalf("Draw before ready = %v", err)
	}
	if _, err := runtime.NewStatusStream(); !errors.Is(err, ErrDeviceUnavailable) {
		t.Fatalf("NewStatusStream before client construction = %v", err)
	}
	close(allowRetry)
	awaitRuntimePhase(t, runtime, PhaseConnecting)

	if err := runtime.Draw(context.Background(), busylib.DisplayElements{}); !errors.Is(err, ErrDeviceUnavailable) {
		t.Fatalf("Draw before WebSocket ready = %v", err)
	}
	stream, err := runtime.NewStatusStream()
	if err != nil || stream != client.statusStream {
		t.Fatalf("NewStatusStream while connecting = %T, %v", stream, err)
	}
	runtime.ObserveStatusStream(StreamHealth{Phase: StreamStatusTransition, Lifecycle: publicstream.LifecycleConnected, Access: publicstream.AccessAccepted, Attempt: 1})
	awaitRuntimePhase(t, runtime, PhaseReady)

	if err := runtime.Draw(context.Background(), busylib.DisplayElements{}); err != nil {
		t.Fatalf("Draw after ready: %v", err)
	}
	if err := runtime.Clear(context.Background(), "bsbctl"); err != nil {
		t.Fatalf("Clear after ready: %v", err)
	}
	if err := runtime.UploadFile(context.Background(), "bsbctl", "a", "/tmp/a"); err != nil {
		t.Fatalf("UploadFile after ready: %v", err)
	}
	var output bytes.Buffer
	if _, err := runtime.ReadTo(context.Background(), "a", &output); err != nil {
		t.Fatalf("ReadTo after ready: %v", err)
	}
	if err := runtime.Remove(context.Background(), "a"); err != nil {
		t.Fatalf("Remove after ready: %v", err)
	}
	if _, err := runtime.BusySnapshot(context.Background()); err != nil {
		t.Fatalf("BusySnapshot after ready: %v", err)
	}
	if err := runtime.SetBusySnapshot(context.Background(), busylib.BusySnapshot{}); err != nil {
		t.Fatalf("SetBusySnapshot after ready: %v", err)
	}
	if _, err := runtime.DeviceTime(context.Background()); err != nil {
		t.Fatalf("DeviceTime after ready: %v", err)
	}
	if err := runtime.PlayAudio(context.Background(), busylib.NewStockAudio("bsbctl", "shared/tone.snd")); err != nil {
		t.Fatalf("PlayAudio after ready: %v", err)
	}
	stream, err = runtime.NewStatusStream()
	if err != nil || stream != client.statusStream {
		t.Fatalf("NewStatusStream = %T, %v", stream, err)
	}
	if factoryCalls.Load() != 2 || client.calls != 9 {
		t.Fatalf("factory calls = %d, delegated calls = %d", factoryCalls.Load(), client.calls)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run shutdown: %v", err)
	}
}

func TestRetryDelayClampsJitterAndFinalDelay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		attempt int
		jitter  float64
		want    time.Duration
	}{
		{1, 0, 800 * time.Millisecond},
		{2, 2, 2400 * time.Millisecond},
		{6, 1, 30 * time.Second},
		{20, 1.2, 30 * time.Second},
	}
	for _, test := range tests {
		if got := retryDelay(test.attempt, test.jitter); got != test.want {
			t.Errorf("retryDelay(%d, %v) = %v, want %v", test.attempt, test.jitter, got, test.want)
		}
	}
}

func TestRuntimeRetryScheduleIsExactCappedAndCancellationAware(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var delays []time.Duration
	runtime := NewRuntime(RuntimeConfig{
		BaseURL: "http://busybar.test",
		Factory: func(context.Context, string, string) (Client, error) { return nil, errors.New("unavailable") },
		Delay: func(ctx context.Context, delay time.Duration) error {
			mu.Lock()
			delays = append(delays, delay)
			count := len(delays)
			mu.Unlock()
			if count == 7 {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		},
		Jitter: func() float64 { return 1 },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	for {
		mu.Lock()
		count := len(delays)
		mu.Unlock()
		if count >= 7 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run cancellation = %v", err)
	}
	mu.Lock()
	got := append([]time.Duration(nil), delays...)
	mu.Unlock()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("delays = %v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("delays = %v", got)
		}
	}
}

func TestRuntimeStatusIsSafeJSON(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	runtime := NewRuntime(RuntimeConfig{
		BaseURL: "https://private.example.test", AccessTokenReference: "keychain://bsbctl/account/private-domain",
		Resolver: SecretResolverFunc(func(context.Context, string) (string, error) {
			return "", errors.New("account person@example.test secret password-value denied")
		}),
		Factory: func(context.Context, string, string) (Client, error) { t.Fatal("factory called"); return nil, nil },
		Delay:   func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() },
		Clock:   func() time.Time { return now }, Jitter: func() float64 { return 1 },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	awaitRuntimePhase(t, runtime, PhaseBackoff)
	encoded, err := json.Marshal(runtime.Status())
	if err != nil {
		t.Fatal(err)
	}
	got := string(encoded)
	if got != `{"phase":"backoff","attempt":1,"retry_at":"2026-08-20T12:00:01Z","last_error_code":"access_token_unavailable"}` {
		t.Fatalf("status JSON = %s", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeClientConstructionRemainsConnectingAndSignalsChange(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime(RuntimeConfig{
		BaseURL: "http://busybar.test",
		Factory: func(context.Context, string, string) (Client, error) {
			return &fakeRuntimeClient{statusStream: &fakeRuntimeStream{}}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	awaitRuntimePhase(t, runtime, PhaseConnecting)
	select {
	case <-runtime.Changes():
	case <-time.After(time.Second):
		t.Fatal("client construction transition was lost before consumer startup")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeWebSocketDropAndRecoveryGatesOperationsWithoutRecreatingClient(t *testing.T) {
	client := &fakeRuntimeClient{statusStream: &fakeRuntimeStream{}}
	var factoryCalls atomic.Int32
	runtime := NewRuntime(RuntimeConfig{
		BaseURL: "http://busybar.test",
		Factory: func(context.Context, string, string) (Client, error) {
			factoryCalls.Add(1)
			return client, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	awaitRuntimePhase(t, runtime, PhaseConnecting)

	connectedAt := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	stateAt := connectedAt.Add(time.Second)
	runtime.ObserveStatusStream(StreamHealth{
		Phase: StreamStatusTransition, Lifecycle: publicstream.LifecycleConnected, Access: publicstream.AccessAccepted,
		Attempt: 1, ConnectedAt: connectedAt, LastStateAt: stateAt,
	})
	awaitRuntimePhase(t, runtime, PhaseReady)
	if err := runtime.Draw(context.Background(), busylib.DisplayElements{}); err != nil {
		t.Fatal(err)
	}

	retryAt := connectedAt.Add(5 * time.Second)
	runtime.ObserveStatusStream(StreamHealth{
		Phase: StreamBackoff, Lifecycle: publicstream.LifecycleReconnecting, Attempt: 2,
		RetryAt: retryAt, ErrorCode: "status_stream_terminal",
	})
	awaitRuntimePhase(t, runtime, PhaseBackoff)
	if err := runtime.Draw(context.Background(), busylib.DisplayElements{}); !errors.Is(err, ErrDeviceUnavailable) {
		t.Fatalf("Draw during WebSocket backoff = %v", err)
	}
	if stream, err := runtime.NewStatusStream(); err != nil || stream != client.statusStream {
		t.Fatalf("NewStatusStream during backoff = %T, %v", stream, err)
	}
	status := runtime.Status()
	if status.Attempt != 2 || !status.RetryAt.Equal(retryAt) || status.LastErrorCode != "status_stream_terminal" || !status.LastConnectedAt.Equal(connectedAt) || !status.LastStateAt.Equal(stateAt) {
		t.Fatalf("backoff status = %#v", status)
	}

	runtime.ObserveStatusStream(StreamHealth{
		Phase: StreamStatusTransition, Lifecycle: publicstream.LifecycleConnected, Access: publicstream.AccessAccepted,
		Attempt: 3, ConnectedAt: connectedAt.Add(10 * time.Second), LastStateAt: stateAt.Add(10 * time.Second),
	})
	awaitRuntimePhase(t, runtime, PhaseReady)
	if err := runtime.Draw(context.Background(), busylib.DisplayElements{}); err != nil {
		t.Fatal(err)
	}
	if factoryCalls.Load() != 1 {
		t.Fatalf("client factory calls = %d, want one across WebSocket recovery", factoryCalls.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func awaitRuntimePhase(t *testing.T, runtime *Runtime, phase Phase) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runtime.Status().Phase == phase {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("phase = %q, want %q", runtime.Status().Phase, phase)
}

func TestDeviceIdentitySeparatesFirmwareDirtyFlagFromCommitHash(t *testing.T) {
	status := busylib.Status{
		System:   busylib.SystemStatus{APISemVer: "25.0.0", Uptime: "1h"},
		Device:   busylib.DeviceStatus{SerialNumber: "serial", OTPModel: "busy-bar", OTPValid: true},
		Firmware: busylib.FirmwareStatus{Target: 7, Version: "1.2.3", CommitHash: "abcdef-dirty"},
	}
	identity := deviceIdentityFromStatus(status)
	if identity.FirmwareCommit != "abcdef" || identity.FirmwareDirty != "true" {
		t.Fatalf("dirty identity = %#v", identity)
	}
	status.Firmware.CommitHash = "123456"
	identity = deviceIdentityFromStatus(status)
	if identity.FirmwareCommit != "123456" || identity.FirmwareDirty != "false" {
		t.Fatalf("clean identity = %#v", identity)
	}
}

type fakeRuntimeClient struct {
	calls        int
	statusStream publicstream.Stream
}

func (c *fakeRuntimeClient) Draw(context.Context, busylib.DisplayElements) error {
	c.calls++
	return nil
}
func (c *fakeRuntimeClient) Clear(context.Context, string) error { c.calls++; return nil }
func (c *fakeRuntimeClient) UploadFile(context.Context, string, string, string) error {
	c.calls++
	return nil
}
func (c *fakeRuntimeClient) ReadTo(context.Context, string, io.Writer) (int64, error) {
	c.calls++
	return 0, nil
}
func (c *fakeRuntimeClient) Remove(context.Context, string) error { c.calls++; return nil }
func (c *fakeRuntimeClient) BusySnapshot(context.Context) (busylib.BusySnapshot, error) {
	c.calls++
	return busylib.BusySnapshot{}, nil
}
func (c *fakeRuntimeClient) SetBusySnapshot(context.Context, busylib.BusySnapshot) error {
	c.calls++
	return nil
}
func (c *fakeRuntimeClient) DeviceTime(context.Context) (busylib.TimestampInfo, error) {
	c.calls++
	return busylib.TimestampInfo{}, nil
}
func (c *fakeRuntimeClient) PlayAudio(context.Context, busylib.PlayAudio) error {
	c.calls++
	return nil
}
func (c *fakeRuntimeClient) NewStatusStream() (publicstream.Stream, error) {
	return c.statusStream, nil
}

type fakeRuntimeStream struct{}

func (*fakeRuntimeStream) Start(context.Context) error           { return nil }
func (*fakeRuntimeStream) Stop() error                           { return nil }
func (*fakeRuntimeStream) RequestSnapshot(context.Context) error { return nil }
func (*fakeRuntimeStream) Messages() <-chan publicstream.Message { return nil }
func (*fakeRuntimeStream) Statuses() <-chan publicstream.Status  { return nil }
func (*fakeRuntimeStream) Status() publicstream.Status           { return publicstream.Status{} }
func (*fakeRuntimeStream) Wait() error                           { return nil }
