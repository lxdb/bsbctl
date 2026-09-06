package daemonrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/pluginlog"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	busylib "github.com/lxdb/busylib-go"
	publicstream "github.com/lxdb/busylib-go/stream"
)

type deviceSecretResolverFunc func(context.Context, string) (string, error)

func (f deviceSecretResolverFunc) Resolve(ctx context.Context, reference string) (string, error) {
	return f(ctx, reference)
}

func TestShutdownDeviceClearsAndDrainsBeforeCancelingRuntime(t *testing.T) {
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	backend := &lifetimeDeviceBackend{runtimeCtx: runtimeCtx}
	output := device.NewOutput(backend, device.OutputOptions{CallTimeout: time.Second})
	gateway, err := device.NewGateway(output, assets.NewReconciler(nil))
	if err != nil {
		t.Fatal(err)
	}
	candidate := presentation.Candidate{PluginID: "p", InstanceID: "i", Channel: "c", Key: "k", Revision: 1, Generation: 1, AdmissionSequence: 1, Policy: presentation.PolicyWhenRelevant, Band: presentation.BandRelevant, Impact: protocol.ImpactNormal, Scene: presentation.Scene{Elements: []presentation.Element{{ID: "text", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: "shown", Font: "normal"}}}}}
	if _, err := gateway.Render(context.Background(), &candidate); err != nil {
		t.Fatal(err)
	}
	runtimeDone := make(chan error, 1)
	go func() { <-runtimeCtx.Done(); runtimeDone <- nil }()
	if err := shutdownDeviceWithBudgets(context.Background(), gateway, output, cancelRuntime, runtimeDone, defaultShutdownBudgets()); err != nil {
		t.Fatal(err)
	}
	if status := output.Status(); status.Phase != device.OutputClosed {
		t.Fatalf("output was not joined: %#v", status)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if got, want := backend.operations, []string{"draw", "clear"}; !equalMainStrings(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
}

func TestShutdownPhasesDoNotLetSlowServiceStarveFinalClearAndOutputJoin(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "bctl-shutdown-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	configPath := filepath.Join(directory, "config.json")
	document := serviceMainDocument()
	document.Plugins["resident"] = config.Plugin{
		ID: "resident", Version: "1", Executable: "/test/resident", ProtocolVersion: protocol.Version,
		ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident}, Channels: []protocol.Channel{{ID: "main"}},
	}
	document.Apps["resident"] = config.App{
		ID: "resident", PluginID: "resident", Enabled: true, Config: []byte(`{}`),
		Policies: map[string]presentation.PolicyConfig{"main": {Policy: presentation.PolicyWhenRelevant}},
	}
	if _, err := config.NewStore(configPath).ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	backend := &shutdownDeviceClient{drawn: make(chan struct{}, 1), clearing: make(chan struct{}, 1), releaseClear: make(chan struct{})}
	plugins := &slowShutdownRuntime{
		applied: make(chan protocol.InstanceRef, 1), closeStarted: make(chan struct{}, 1), releaseClose: make(chan struct{}),
	}
	dependencies := testDaemonDependencies()
	dependencies.newPluginRuntime = func(callbacks pluginhost.Callbacks) pluginRuntime {
		plugins.callbacks = callbacks
		return plugins
	}
	var deviceRuntime *device.Runtime
	dependencies.newDeviceRuntime = func(runtimeConfig device.RuntimeConfig) *device.Runtime {
		runtimeConfig.Factory = func(ctx context.Context, _, _ string) (device.Client, error) {
			backend.runtimeCtx = ctx
			return backend, nil
		}
		deviceRuntime = device.NewRuntime(runtimeConfig)
		return deviceRuntime
	}
	ctx, cancel := context.WithCancel(t.Context())
	var logs bytes.Buffer
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- run(ctx, Options{Version: "test", ConfigPath: configPath, SocketPath: filepath.Join(directory, "control.sock"), Stderr: &logs}, dependencies)
	}()
	releaseService := sync.OnceFunc(func() { close(plugins.releaseClose) })
	releaseClear := sync.OnceFunc(func() { close(backend.releaseClear) })
	t.Cleanup(func() {
		releaseService()
		releaseClear()
		cancel()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not exit during failure cleanup")
		}
	})
	var instance protocol.InstanceRef
	select {
	case instance = <-plugins.applied:
	case <-time.After(3 * time.Second):
		t.Fatal("plugin configuration was not applied")
	}
	for deadline := time.Now().Add(3 * time.Second); deviceRuntime.Status().Phase != device.PhaseReady; {
		if time.Now().After(deadline) {
			t.Fatalf("device did not become ready: %#v", deviceRuntime.Status())
		}
		time.Sleep(time.Millisecond)
	}
	now := time.Now().UTC()
	value := protocol.Observation{
		Instance: instance, Channel: "main", Key: "state", Revision: 1,
		Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNormal, ReasonCode: "test_state",
		ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(time.Minute),
		Scene: &protocol.Scene{Elements: []protocol.Element{{ID: "text", Display: protocol.DisplayFront,
			Text: &protocol.TextElement{Value: "shown", Font: "normal", Color: "#FFFFFFFF", Align: "center", Width: 10}}}},
	}
	// Apply precedes the live-generation commit; wait for actual admission.
	for {
		err := plugins.callbacks.Observe(observation.Source{PluginID: "resident", Generation: instance.Generation}, value)
		if err == nil {
			break
		}
		if !errors.Is(err, observation.ErrStaleGeneration) || time.Since(now) > 3*time.Second {
			t.Fatalf("admit scene: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-backend.drawn:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not render the scene")
	}
	cancel()
	select {
	case <-plugins.closeStarted:
	case err := <-done:
		t.Fatalf("daemon exited before service shutdown started: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("service shutdown did not start")
	}
	select {
	case <-backend.clearing:
		t.Fatal("device cleared before service shutdown finished")
	case err := <-done:
		t.Fatalf("daemon exited while service shutdown was blocked: %v", err)
	default:
	}
	releaseService()
	select {
	case <-backend.clearing:
	case err := <-done:
		t.Fatalf("daemon exited without final clear: %v", err)
	case <-time.After(12 * time.Second):
		t.Fatal("slow service prevented final clear")
	}
	if err := backend.runtimeCtx.Err(); err != nil {
		t.Fatalf("runtime canceled before clear drained: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("daemon did not join blocked clear: %v", err)
	default:
	}
	releaseClear()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want slow service deadline", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not join device shutdown")
	}
	if backend.runtimeCtx.Err() == nil {
		t.Fatal("daemon returned with device runtime still live")
	}
	if !strings.Contains(logs.String(), "shutdown.finished") {
		t.Fatalf("final plugin log was not drained: %s", logs.String())
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if got, want := backend.operations, []string{"draw", "clear"}; !equalMainStrings(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
}

type slowShutdownRuntime struct {
	fakePluginRuntime
	callbacks    pluginhost.Callbacks
	applied      chan protocol.InstanceRef
	closeStarted chan struct{}
	releaseClose chan struct{}
}

func (f *slowShutdownRuntime) Apply(_ context.Context, specs []pluginhost.Spec) error {
	if len(specs) != 1 || len(specs[0].Instances) != 1 {
		return errors.New("unexpected plugin specification")
	}
	select {
	case f.applied <- specs[0].Instances[0].Ref():
	default:
	}
	return nil
}

func (f *slowShutdownRuntime) Close(ctx context.Context) error {
	select {
	case f.closeStarted <- struct{}{}:
	default:
	}
	select {
	case <-f.releaseClose:
		f.callbacks.Log("resident", protocol.LogNotification{Level: protocol.LogLevelInfo, Event: "shutdown.finished", Message: "service shutdown finished"})
		return context.DeadlineExceeded
	case <-ctx.Done():
		return ctx.Err()
	}
}

type shutdownDeviceClient struct {
	lifetimeDeviceBackend
	drawn, clearing chan struct{}
	releaseClear    chan struct{}
}

func (b *shutdownDeviceClient) Draw(ctx context.Context, elements busylib.DisplayElements) error {
	if err := b.lifetimeDeviceBackend.Draw(ctx, elements); err != nil {
		return err
	}
	select {
	case b.drawn <- struct{}{}:
	default:
	}
	return nil
}

func (b *shutdownDeviceClient) Clear(ctx context.Context, app string) error {
	if err := b.lifetimeDeviceBackend.Clear(ctx, app); err != nil {
		return err
	}
	b.clearing <- struct{}{}
	select {
	case <-b.releaseClear:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (*shutdownDeviceClient) NewStatusStream() (publicstream.Stream, error) {
	return shutdownStatusStream{messages: make(chan publicstream.Message)}, nil
}

type shutdownStatusStream struct{ messages chan publicstream.Message }

func (shutdownStatusStream) Start(context.Context) error             { return nil }
func (shutdownStatusStream) Stop() error                             { return nil }
func (shutdownStatusStream) RequestSnapshot(context.Context) error   { return nil }
func (s shutdownStatusStream) Messages() <-chan publicstream.Message { return s.messages }
func (shutdownStatusStream) Statuses() <-chan publicstream.Status    { return nil }
func (shutdownStatusStream) Status() publicstream.Status {
	return publicstream.Status{Lifecycle: publicstream.LifecycleConnected, Access: publicstream.AccessAccepted}
}
func (shutdownStatusStream) Wait() error { return nil }

func TestRunDaemonRecorderFailureDegradesWithoutBlockingControl(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "bctl-recorder-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	configPath := filepath.Join(directory, "config.json")
	socketPath := filepath.Join(directory, "control.sock")
	if _, err := config.NewStore(configPath).ReplaceWithOutcome(0, serviceMainDocument()); err != nil {
		t.Fatal(err)
	}
	dependencies := testDaemonDependencies()
	dependencies.newAttentionRecorder = func(string, int, int64, int) (*attention.Recorder, error) {
		return nil, errors.New("recorder unavailable")
	}
	dependencies.newPluginRuntime = func(pluginhost.Callbacks) pluginRuntime { return &fakePluginRuntime{} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Options{Version: "test", ConfigPath: configPath, SocketPath: socketPath, Stderr: io.Discard}, dependencies)
	}()
	var stopped atomic.Bool
	t.Cleanup(func() {
		if stopped.Load() {
			return
		}
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		client, err := control.Dial(context.Background(), socketPath)
		if err == nil {
			var status control.Status
			callErr := client.Call(context.Background(), "daemon.status", nil, &status)
			_ = client.Close()
			if callErr != nil {
				t.Fatal(callErr)
			}
			if status.AttentionRecorder.Phase != attention.RecorderUnavailable {
				t.Fatalf("recorder status = %#v", status.AttentionRecorder)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control socket did not start: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		stopped.Store(true)
		if err != nil {
			t.Fatalf("runDaemon shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runDaemon did not stop")
	}
}

func TestDaemonPluginLogCallbacksEnableStructuredOutput(t *testing.T) {
	sink := pluginlog.New(io.Discard, pluginlog.Options{})
	t.Cleanup(func() { _ = sink.Close(context.Background()) })
	callbacks := daemonPluginLogCallbacks(sink)
	if callbacks.Log == nil {
		t.Fatal("daemon disabled structured plugin logs")
	}
}

func TestDaemonObservationCallbackRejectsInvalidPresentationBeforeStore(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	source := observation.Source{PluginID: "dev.bsbctl.test", Generation: 1}
	valid := func() protocol.Observation {
		return protocol.Observation{
			Instance: protocol.InstanceRef{ID: "app", Generation: source.Generation}, Channel: "main", Key: "state", Revision: 1,
			Disposition: protocol.DispositionSnapshot, Impact: protocol.ImpactNormal, ReasonCode: "test_state",
			ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(time.Minute),
			Scene: &protocol.Scene{Elements: []protocol.Element{{
				ID: "text", Display: protocol.DisplayFront,
				Text: &protocol.TextElement{Value: "ready", Font: "normal", Color: "#FFFFFFFF", Align: "center", Width: 10},
			}}},
		}
	}
	tests := []struct {
		name   string
		mutate func(*protocol.Observation)
	}{
		{name: "font", mutate: func(value *protocol.Observation) { value.Scene.Elements[0].Text.Font = "unknown" }},
		{name: "color", mutate: func(value *protocol.Observation) { value.Scene.Elements[0].Text.Color = "#fff" }},
		{name: "align", mutate: func(value *protocol.Observation) { value.Scene.Elements[0].Text.Align = "middle" }},
		{name: "x coordinate", mutate: func(value *protocol.Observation) { value.Scene.Elements[0].X = -1 }},
		{name: "y coordinate", mutate: func(value *protocol.Observation) { value.Scene.Elements[0].Y = 16 }},
		{name: "text width", mutate: func(value *protocol.Observation) { value.Scene.Elements[0].Text.Width = -1 }},
		{name: "rectangle dimensions", mutate: func(value *protocol.Observation) {
			value.Scene.Elements[0].Text = nil
			value.Scene.Elements[0].Rectangle = &protocol.RectangleElement{Width: 0, Height: 1, Color: "#FFFFFFFF"}
		}},
		{name: "countdown", mutate: func(value *protocol.Observation) {
			value.Scene.Elements[0].Text = nil
			value.Scene.Elements[0].Countdown = &protocol.CountdownElement{EndsAtUnixSeconds: now.Add(time.Minute).Unix(), ShowHours: "sometimes", Color: "#FFFFFFFF"}
		}},
		{name: "display", mutate: func(value *protocol.Observation) { value.Scene.Elements[0].Display = "side" }},
		{name: "unresolved audio", mutate: func(value *protocol.Observation) {
			value.Audio = &protocol.AudioCue{ID: "cue", Asset: protocol.AssetRef{PackagePath: "assets/tone.snd"}, ExpiresAt: now.Add(30 * time.Second)}
		}},
		{name: "undeclared package asset", mutate: func(value *protocol.Observation) {
			value.Scene.Elements[0].Text = nil
			value.Scene.Elements[0].Image = &protocol.ImageElement{Asset: protocol.AssetRef{PackagePath: "assets/missing.png"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := observation.NewStore(func(pluginID, instanceID string) (uint64, bool) {
				return 1, pluginID == source.PluginID && instanceID == "app"
			}, func() time.Time { return now })
			resolver := assets.NewReconciler(nil)
			resolver.Reconcile(t.Context(), []assets.Package{{PluginID: source.PluginID, Version: "1", Enabled: true}})
			callback := daemonObservationCallback(store, resolver)
			value := valid()
			test.mutate(&value)
			err := callback(source, value)
			domain, ok := errors.AsType[*protocol.DomainError](err)
			if !ok || domain.Kind() != protocol.ErrorInvalidArgument {
				t.Fatalf("callback error = %v, want invalid_argument", err)
			}
			if records := store.Snapshot(); len(records) != 0 {
				t.Fatalf("invalid observation reached Store: %#v", records)
			}
		})
	}
}

func TestDaemonObservationCallbackPreservesPendingAssetAsNotReady(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	source := observation.Source{PluginID: "dev.bsbctl.test", Generation: 1}
	store := observation.NewStore(func(pluginID, instanceID string) (uint64, bool) {
		return 1, pluginID == source.PluginID && instanceID == "app"
	}, func() time.Time { return now })
	callback := daemonObservationCallback(store, assets.NewReconciler(nil))
	err := callback(source, protocol.Observation{
		Instance: protocol.InstanceRef{ID: "app", Generation: 1}, Channel: "main", Key: "state", Revision: 1,
		Disposition: protocol.DispositionSnapshot, Impact: protocol.ImpactNormal, ReasonCode: "assets_pending",
		ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(time.Minute),
		Scene: &protocol.Scene{Elements: []protocol.Element{{
			ID: "image", Display: protocol.DisplayFront,
			Image: &protocol.ImageElement{Asset: protocol.AssetRef{PackagePath: "assets/declared.png"}},
		}}},
	})
	domain, ok := errors.AsType[*protocol.DomainError](err)
	if !ok || domain.Kind() != protocol.ErrorNotReady || !errors.Is(err, assets.ErrAssetsNotReady) {
		t.Fatalf("callback error = %v, want not_ready wrapping ErrAssetsNotReady", err)
	}
	if records := store.Snapshot(); len(records) != 0 {
		t.Fatalf("asset-pending observation reached Store: %#v", records)
	}
}

func TestDaemonObservationCallbackInvalidHighPriorityDoesNotBlockValidLowerPriority(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	source := observation.Source{PluginID: "dev.bsbctl.test", Generation: 1}
	store := observation.NewStore(func(pluginID, instanceID string) (uint64, bool) {
		return 1, pluginID == source.PluginID && instanceID == "app"
	}, func() time.Time { return now })
	callback := daemonObservationCallback(store, assets.NewReconciler(nil))
	lower := protocol.Observation{
		Instance: protocol.InstanceRef{ID: "app", Generation: source.Generation}, Channel: "main", Key: "lower", Revision: 1,
		Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNormal, ReasonCode: "lower_priority",
		ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(time.Minute),
		Scene: &protocol.Scene{Elements: []protocol.Element{{
			ID: "text", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: "lower", Font: "normal"},
		}}},
	}
	if err := callback(source, lower); err != nil {
		t.Fatal(err)
	}
	higher := lower
	higher.Key = "higher"
	higher.Impact = protocol.ImpactCritical
	higher.Scene = &protocol.Scene{Elements: []protocol.Element{{
		ID: "text", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: "higher", Font: "invalid"},
	}}}
	err := callback(source, higher)
	domain, ok := errors.AsType[*protocol.DomainError](err)
	if !ok || domain.Kind() != protocol.ErrorInvalidArgument {
		t.Fatalf("higher-priority callback error = %v, want invalid_argument", err)
	}
	records := store.Snapshot()
	if len(records) != 1 || records[0].Observation.Key != lower.Key {
		t.Fatalf("Store records = %#v, want only lower-priority observation", records)
	}
	decision := attention.Select(records, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyWhenRelevant}, true
	}, presentation.History{LastShown: map[string]time.Time{}}, now)
	if decision.Candidate == nil || decision.Candidate.Key != lower.Key {
		t.Fatalf("selected candidate = %#v, want valid lower-priority observation", decision.Candidate)
	}
}

func TestAudioDiagnosticRendererReportsRedactedDegradationOnce(t *testing.T) {
	const (
		messageCanary = "confidential-scene-canary"
		cueCanary     = "confidential-cue-canary"
		errorCanary   = "speaker-error-canary token=secret path=/private/audio"
	)
	now := time.Now().UTC()
	backend := &lifetimeDeviceBackend{runtimeCtx: t.Context(), audioErr: errors.New(errorCanary)}
	gateway, err := device.NewGateway(backend, assets.NewReconciler(nil))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	logs := pluginlog.New(&output, pluginlog.Options{Now: func() time.Time { return now }})
	renderer := &audioDiagnosticRenderer{
		gateway: gateway, logs: logs,
	}
	value := presentation.Candidate{
		PluginID: "dev.bsbctl.calendar", InstanceID: "work", Channel: "active", Key: "meeting",
		Revision: 1, Generation: 1, AdmissionSequence: 1, Policy: presentation.PolicyWhenRelevant, Band: presentation.BandRelevant, Impact: protocol.ImpactNormal,
		UpdatedAt: now, ExpiresAt: now.Add(time.Minute),
		Scene: presentation.Scene{Elements: []presentation.Element{{
			ID: "text", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: messageCanary, Font: "normal"},
		}}},
		AudioCue: &protocol.AudioCue{
			ID: cueCanary, Asset: protocol.AssetRef{StockName: "calendar_event_starts.snd"}, ExpiresAt: now.Add(30 * time.Second),
		},
	}
	outcome, err := renderer.Render(t.Context(), &value)
	if err != nil {
		t.Fatalf("Render returned best-effort audio failure: %v", err)
	}
	if outcome != attention.OutcomeDrawn {
		t.Fatalf("visual outcome = %q, want %q", outcome, attention.OutcomeDrawn)
	}
	if got := gateway.Status().LastErrorCode; got != "audio_play_failed" {
		t.Fatalf("status audio error = %q, want audio_play_failed", got)
	}
	value.Revision++
	value.Scene.Elements[0].Text.Value = "updated"
	if _, err := renderer.Render(t.Context(), &value); err != nil {
		t.Fatalf("second Render returned best-effort audio failure: %v", err)
	}
	if got := gateway.Status().LastErrorCode; got != "audio_play_failed" {
		t.Fatalf("deduplicated cue erased status audio error: %q", got)
	}
	if err := logs.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	operations := append([]string(nil), backend.operations...)
	backend.mu.Unlock()
	if got := strings.Count(strings.Join(operations, ","), "audio-play"); got != 1 {
		t.Fatalf("audio attempts = %d, want one: %v", got, operations)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("diagnostics = %d, want one: %q", len(lines), output.String())
	}
	var record struct {
		PluginID string            `json:"plugin_id"`
		Level    string            `json:"level"`
		Event    string            `json:"event"`
		Message  string            `json:"message"`
		Fields   map[string]string `json:"fields"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatal(err)
	}
	if record.PluginID != "bsbctl" || record.Level != string(protocol.LogLevelWarn) || record.Event != "device_audio_degraded" ||
		record.Message != "" || !reflect.DeepEqual(record.Fields, map[string]string{"error_code": "audio_play_failed"}) {
		t.Fatalf("audio diagnostic = %#v", record)
	}
	for _, canary := range []string{messageCanary, cueCanary, errorCanary, "/private/audio"} {
		if strings.Contains(output.String(), canary) {
			t.Fatalf("audio diagnostic leaked %q: %s", canary, output.String())
		}
	}
}

func TestDeviceIdentityDiagnosticsJoinsDaemonCancellation(t *testing.T) {
	started := make(chan struct{})
	readerStopped := make(chan struct{})
	reader := deviceIdentityReaderFunc(func(ctx context.Context) (device.DeviceIdentity, error) {
		close(started)
		<-ctx.Done()
		close(readerStopped)
		return device.DeviceIdentity{}, ctx.Err()
	})
	wake := make(chan struct{}, 1)
	wake <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runDeviceIdentityDiagnostics(ctx, reader, wake, func(protocol.LogNotification) {
			t.Error("shutdown cancellation emitted a device identity diagnostic")
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("device identity read did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("identity worker = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("identity worker outlived daemon cancellation")
	}
	select {
	case <-readerStopped:
	default:
		t.Fatal("identity read did not inherit daemon cancellation")
	}
}

func TestDeviceIdentityDiagnosticsEmitsEverySuccessfulConnectionTuple(t *testing.T) {
	identities := make(chan device.DeviceIdentity, 2)
	identities <- device.DeviceIdentity{
		APISemVer: "1.2.3", SerialNumber: "BB-001", OTPModel: "busy-bar", OTPValid: true,
		FirmwareTarget: 42, FirmwareVersion: "2.0.1", FirmwareCommit: "abc123", FirmwareDirty: "false", Uptime: "1h2m3s",
	}
	identities <- device.DeviceIdentity{
		APISemVer: "1.2.4", SerialNumber: "BB-002", OTPModel: "busy-bar-pro", OTPValid: false,
		FirmwareTarget: 43, FirmwareVersion: "2.1.0", FirmwareCommit: "def456", FirmwareDirty: "true", Uptime: "4h5m6s",
	}
	reader := deviceIdentityReaderFunc(func(ctx context.Context) (device.DeviceIdentity, error) {
		select {
		case identity := <-identities:
			return identity, nil
		case <-ctx.Done():
			return device.DeviceIdentity{}, ctx.Err()
		}
	})
	wake := make(chan struct{}, 1)
	logged := make(chan protocol.LogNotification, 2)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runDeviceIdentityDiagnostics(ctx, reader, wake, func(value protocol.LogNotification) { logged <- value })
	}()
	wants := []map[string]string{
		{
			"api_semver": "1.2.3", "serial": "BB-001", "otp_model": "busy-bar", "otp_valid": "true",
			"firmware_target": "42", "firmware_version": "2.0.1", "firmware_commit": "abc123", "firmware_dirty": "false", "uptime": "1h2m3s",
		},
		{
			"api_semver": "1.2.4", "serial": "BB-002", "otp_model": "busy-bar-pro", "otp_valid": "false",
			"firmware_target": "43", "firmware_version": "2.1.0", "firmware_commit": "def456", "firmware_dirty": "true", "uptime": "4h5m6s",
		},
	}
	for index, want := range wants {
		wake <- struct{}{}
		select {
		case notification := <-logged:
			if err := notification.Validate(); err != nil {
				t.Fatalf("connection %d notification: %v", index+1, err)
			}
			if notification.Level != protocol.LogLevelInfo || notification.Event != "device_connected" ||
				notification.Message != "BUSY Bar connection identity" || !reflect.DeepEqual(notification.Fields, want) {
				t.Fatalf("connection %d diagnostic = %#v, want fields %#v", index+1, notification, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("connection %d diagnostic was not emitted", index+1)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("identity worker did not join cancellation")
	}
}

type deviceIdentityReaderFunc func(context.Context) (device.DeviceIdentity, error)

func (read deviceIdentityReaderFunc) DeviceIdentity(ctx context.Context) (device.DeviceIdentity, error) {
	return read(ctx)
}

func TestRunDaemonRedactsResolvedSecretsBeforePluginApply(t *testing.T) {
	const secretCanary = "resolved-keychain-value-canary"
	directory, err := os.MkdirTemp("/tmp", "bctl-secret-log-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	configPath := filepath.Join(directory, "config.json")
	socketPath := filepath.Join(directory, "control.sock")
	document := config.Document{
		Version: config.CurrentVersion, Generation: 1, Device: config.Device{BaseURL: "http://127.0.0.1:65534"},
		Plugins: map[string]config.Plugin{"plugin": {
			ID: "plugin", Version: "1", Executable: "/test/plugin",
			ProtocolVersion: protocol.Version, ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident},
			Channels: []protocol.Channel{{ID: "main"}},
		}},
		Apps: map[string]config.App{"app": {
			ID: "app", PluginID: "plugin", Enabled: true, Config: json.RawMessage(`{}`),
			Secrets:  map[string]string{"api_key": "keychain://bsbctl/app-api-key"},
			Policies: map[string]presentation.PolicyConfig{"main": {Policy: presentation.PolicyWhenRelevant}},
		}},
	}
	if _, err := config.NewStore(configPath).ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}

	dependencies := testDaemonDependencies()
	dependencies.newSecretResolver = func() secretResolver {
		return deviceSecretResolverFunc(func(context.Context, string) (string, error) { return secretCanary, nil })
	}
	dependencies.newDeviceRuntime = func(runtimeConfig device.RuntimeConfig) *device.Runtime {
		runtimeConfig.Factory = func(context.Context, string, string) (device.Client, error) {
			return nil, errors.New("device unavailable")
		}
		runtimeConfig.Delay = func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		}
		return device.NewRuntime(runtimeConfig)
	}
	applied := make(chan struct{})
	dependencies.newPluginRuntime = func(callbacks pluginhost.Callbacks) pluginRuntime {
		return &secretLoggingRuntime{callbacks: callbacks, applied: applied}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Options{Version: "test", ConfigPath: configPath, SocketPath: socketPath, Stderr: &output}, dependencies)
	}()
	select {
	case <-applied:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("plugin configuration was not applied")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDaemon: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runDaemon did not stop")
	}
	if strings.Contains(output.String(), secretCanary) || !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("structured plugin output persisted a delivered secret: %s", output.String())
	}
}

func serviceMainDocument() config.Document {
	return config.Document{Version: config.CurrentVersion, Generation: 1, Plugins: map[string]config.Plugin{}, Apps: map[string]config.App{}}
}

type lifetimeDeviceBackend struct {
	mu         sync.Mutex
	runtimeCtx context.Context
	operations []string
	audioErr   error
}

func (b *lifetimeDeviceBackend) record(operation string) error {
	if err := b.runtimeCtx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	b.operations = append(b.operations, operation)
	b.mu.Unlock()
	return nil
}
func (b *lifetimeDeviceBackend) Draw(context.Context, busylib.DisplayElements) error {
	return b.record("draw")
}
func (b *lifetimeDeviceBackend) Clear(context.Context, string) error { return b.record("clear") }
func (b *lifetimeDeviceBackend) PlayAudio(context.Context, busylib.PlayAudio) error {
	if err := b.record("audio-play"); err != nil {
		return err
	}
	return b.audioErr
}
func (*lifetimeDeviceBackend) UploadFile(context.Context, string, string, string) error { return nil }
func (*lifetimeDeviceBackend) ReadTo(context.Context, string, io.Writer) (int64, error) {
	return 0, nil
}
func (*lifetimeDeviceBackend) Remove(context.Context, string) error { return nil }
func equalMainStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestRunDaemonStartsControlWhileDeviceCredentialIsUnavailable(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "bctl-main-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	configPath := filepath.Join(directory, "config.json")
	socketPath := filepath.Join(directory, "control.sock")
	document := config.Document{
		Version: config.CurrentVersion, Generation: 1,
		Device: config.Device{BaseURL: "http://busybar.test", AccessTokenSecret: "keychain://bsbctl/device/access-token"},
		Plugins: map[string]config.Plugin{"resident": {
			ID: "resident", Version: "1", Executable: "/test/resident",
			ProtocolVersion: protocol.Version, ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident},
			Channels: []protocol.Channel{{ID: "main"}},
		}},
		Apps: map[string]config.App{"resident": {
			ID: "resident", PluginID: "resident", Enabled: true,
			Config: []byte(`{}`), Policies: map[string]presentation.PolicyConfig{"main": {Policy: presentation.PolicyWhenRelevant}},
		}},
	}
	if _, err := config.NewStore(configPath).ReplaceWithOutcome(0, document); err != nil {
		t.Fatalf("write config: %v", err)
	}

	dependencies := testDaemonDependencies()
	plugins := &fakePluginRuntime{}
	dependencies.newPluginRuntime = func(pluginhost.Callbacks) pluginRuntime { return plugins }
	var resolveCalls atomic.Int32
	resolver := deviceSecretResolverFunc(func(context.Context, string) (string, error) {
		resolveCalls.Add(1)
		return "", errors.New("sensitive credential failure")
	})
	dependencies.newSecretResolver = func() secretResolver { return resolver }
	dependencies.newDeviceRuntime = func(runtimeConfig device.RuntimeConfig) *device.Runtime {
		runtimeConfig.Delay = func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		}
		runtimeConfig.Jitter = func() float64 { return 1 }
		return device.NewRuntime(runtimeConfig)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Options{Version: "test", ConfigPath: configPath, SocketPath: socketPath, Stderr: os.Stderr}, dependencies)
	}()

	var client *control.Client
	var dialErr error
	var daemonErr error
	daemonDone := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client, dialErr = control.Dial(context.Background(), socketPath)
		if dialErr == nil {
			break
		}
		select {
		case daemonErr = <-done:
			daemonDone = true
			break
		default:
		}
		if daemonDone {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if client == nil {
		cancel()
		if !daemonDone {
			daemonErr = <-done
		}
		t.Fatalf("control socket did not start: dial=%v daemon=%v", dialErr, daemonErr)
	}
	defer client.Close()
	var status control.Status
	statusDeadline := time.Now().Add(2 * time.Second)
	for status.Device.Phase != device.PhaseBackoff {
		select {
		case err := <-done:
			t.Fatalf("daemon exited before device entered backoff: %v", err)
		default:
		}
		if err := client.Call(ctx, "daemon.status", nil, &status); err != nil {
			t.Fatalf("daemon.status while awaiting device backoff: %v", err)
		}
		if status.Device.Phase == device.PhaseBackoff {
			break
		}
		if time.Now().After(statusDeadline) {
			t.Fatalf("device did not enter backoff: %#v", status.Device)
		}
		select {
		case err := <-done:
			t.Fatalf("daemon exited before device entered backoff: %v", err)
		case <-time.After(2 * time.Millisecond):
		}
	}
	if status.Device.LastErrorCode != "access_token_unavailable" {
		t.Fatalf("device status = %#v", status.Device)
	}
	if resolveCalls.Load() != 1 {
		t.Fatalf("resolve calls = %d", resolveCalls.Load())
	}
	if !plugins.appliedResident() {
		t.Fatal("non-asset resident plugin was not reconciled while device was unavailable")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDaemon shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runDaemon did not join supervised goroutines")
	}
}

func TestRunDaemonJoinsAssetRetryBeforeClosingService(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "bctl-assets-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	configPath := filepath.Join(directory, "config.json")
	socketPath := filepath.Join(directory, "control.sock")
	document := config.Document{
		Version: config.CurrentVersion, Generation: 1,
		Device: config.Device{BaseURL: "http://busybar.test"},
		Plugins: map[string]config.Plugin{"resident": {
			ID: "resident", Version: "1", Executable: "/test/resident",
			ProtocolVersion: protocol.Version, ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident},
			Channels: []protocol.Channel{{ID: "main"}},
		}},
		Apps: map[string]config.App{"resident": {
			ID: "resident", PluginID: "resident", Enabled: true,
			Config: []byte(`{}`), Policies: map[string]presentation.PolicyConfig{"main": {Policy: presentation.PolicyWhenRelevant}},
		}},
	}
	if _, err := config.NewStore(configPath).ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	dependencies := testDaemonDependencies()
	retryStarted := make(chan struct{})
	retryStopped := make(chan struct{})
	plugins := &fakePluginRuntime{closeAfter: retryStopped}
	dependencies.newPluginRuntime = func(pluginhost.Callbacks) pluginRuntime { return plugins }
	dependencies.runAssetRetry = func(ctx context.Context, _ device.AssetRuntime, _ device.AssetReconciler, _ device.AssetRetryOptions) error {
		close(retryStarted)
		<-ctx.Done()
		close(retryStopped)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	daemonExited := make(chan struct{})
	go func() {
		defer close(daemonExited)
		done <- run(ctx, Options{Version: "test", ConfigPath: configPath, SocketPath: socketPath, Stderr: os.Stderr}, dependencies)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-daemonExited:
		case <-time.After(2 * time.Second):
			t.Error("runDaemon did not exit during failure cleanup")
		}
	})
	select {
	case <-retryStarted:
	case err := <-done:
		t.Fatalf("daemon ended before asset retry: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("asset retry did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDaemon shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runDaemon did not join asset retry")
	}
	plugins.mu.Lock()
	closeCalled, closeBeforeRetryStopped := plugins.closeCalled, plugins.closeBeforeRetryStopped
	plugins.mu.Unlock()
	if !closeCalled || closeBeforeRetryStopped {
		t.Fatalf("service close called=%v before asset retry stopped=%v", closeCalled, closeBeforeRetryStopped)
	}
}

type fakePluginRuntime struct {
	mu                      sync.Mutex
	specs                   []pluginhost.Spec
	closeAfter              <-chan struct{}
	closeCalled             bool
	closeBeforeRetryStopped bool
}

type secretLoggingRuntime struct {
	fakePluginRuntime
	callbacks pluginhost.Callbacks
	applied   chan struct{}
	once      sync.Once
}

func (f *secretLoggingRuntime) Apply(_ context.Context, specs []pluginhost.Spec) error {
	if len(specs) != 1 || len(specs[0].Instances) != 1 {
		return errors.New("unexpected plugin specification")
	}
	secret := specs[0].Instances[0].Secrets["api_key"]
	f.callbacks.Log(specs[0].ID, protocol.LogNotification{
		Level: protocol.LogLevelInfo, Event: "apply.secret", Message: "opaque " + secret,
		Fields: map[string]string{"note": secret},
	})
	f.once.Do(func() { close(f.applied) })
	return nil
}

func (f *fakePluginRuntime) Apply(_ context.Context, specs []pluginhost.Spec) error {
	f.mu.Lock()
	f.specs = append([]pluginhost.Spec(nil), specs...)
	f.mu.Unlock()
	return nil
}
func (*fakePluginRuntime) Invoke(context.Context, string, pluginhost.InvokeRequest, pluginhost.InvocationKind, pluginhost.SessionToken) error {
	return nil
}
func (*fakePluginRuntime) EndSession(context.Context, string, protocol.InstanceRef, pluginhost.SessionToken) error {
	return nil
}
func (*fakePluginRuntime) Status() []pluginhost.PluginStatus { return nil }
func (*fakePluginRuntime) Operation(context.Context, string, protocol.OperationRequest) (protocol.OperationResult, error) {
	return protocol.OperationResult{}, errors.New("unexpected plugin operation")
}
func (f *fakePluginRuntime) Close(context.Context) error {
	f.mu.Lock()
	f.closeCalled = true
	if f.closeAfter != nil {
		select {
		case <-f.closeAfter:
		default:
			f.closeBeforeRetryStopped = true
		}
	}
	f.mu.Unlock()
	return nil
}
func (*fakePluginRuntime) SessionInputResult(context.Context, string, protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, nil
}
func (f *fakePluginRuntime) appliedResident() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specs) == 1 && f.specs[0].ID == "resident" && len(f.specs[0].Instances) == 1
}
