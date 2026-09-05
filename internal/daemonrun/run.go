// Package daemonrun owns construction and bounded shutdown of the daemon
// runtime. Command packages provide paths, streams, and version metadata but do
// not assemble domain objects.
package daemonrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/checkpoint"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/deviceownership"
	"github.com/lxdb/bsbctl/internal/eventbus"
	busyinput "github.com/lxdb/bsbctl/internal/input"
	"github.com/lxdb/bsbctl/internal/installer"
	"github.com/lxdb/bsbctl/internal/logfile"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginlog"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	busylib "github.com/lxdb/busylib-go"
)

const (
	defaultLogMaxBytes = 10 << 20
	defaultLogArchives = 3
)

// Options contains the complete command-owned inputs to a daemon runtime.
type Options struct {
	Version    string
	ConfigPath string
	SocketPath string
	LogPath    string
	Stderr     io.Writer
}

// Run constructs the production daemon, runs it until cancellation or a
// worker failure, and joins every owned resource before returning.
func Run(ctx context.Context, options Options) error {
	return run(ctx, options, productionDependencies(options.Version))
}

func run(ctx context.Context, options Options, dependencies dependencies) (result error) {
	if ctx == nil {
		return failure(ErrorInvalidInput, "daemon context is required", nil)
	}
	if options.Version == "" {
		return failure(ErrorInvalidInput, "daemon version is required", nil)
	}
	if options.ConfigPath == "" {
		return failure(ErrorInvalidInput, "daemon configuration path is required", nil)
	}
	if options.SocketPath == "" {
		return failure(ErrorInvalidInput, "daemon socket path is required", nil)
	}
	if options.Stderr == nil {
		return failure(ErrorInvalidInput, "daemon diagnostic stream is required", nil)
	}
	stderr := options.Stderr
	if options.LogPath != "" {
		if !filepath.IsAbs(options.LogPath) {
			return failure(ErrorInvalidInput, "daemon log path must be absolute", nil)
		}
		logWriter, err := logfile.Open(options.LogPath, defaultLogMaxBytes, defaultLogArchives)
		if err != nil {
			return failure(ErrorOperational, "open daemon log failed", err)
		}
		defer func() { result = errors.Join(result, logWriter.Close()) }()
		stderr = logWriter
	}

	configStore := config.NewStore(options.ConfigPath)
	if err := recoverInstallerState(ctx, options.ConfigPath, configStore, dependencies); err != nil {
		return err
	}
	document, err := configStore.Load()
	if err != nil {
		if _, ok := errors.AsType[*os.PathError](err); ok {
			return failure(ErrorOperational, "load daemon configuration failed", err)
		}
		return failure(ErrorInvalidInput, "daemon configuration is invalid", err)
	}
	deviceURL := document.Device.BaseURL
	if deviceURL == "" {
		deviceURL = busylib.DefaultLocalBaseURL
	}
	ownership, err := dependencies.acquireDeviceOwnership(deviceURL, device.ApplicationName)
	if err != nil {
		if errors.Is(err, deviceownership.ErrAlreadyOwned) {
			if conflict, ok := errors.AsType[*deviceownership.ConflictError](err); ok && conflict.OwnerPID > 0 {
				return failure(ErrorOperational, fmt.Sprintf("device display is already owned by bsbctl process %d", conflict.OwnerPID), err)
			}
			return failure(ErrorOperational, "device display is already owned by another bsbctl daemon", err)
		}
		return failure(ErrorOperational, "acquire device display ownership failed", err)
	}
	defer func() { result = errors.Join(result, ownership.Close()) }()

	keychain := dependencies.newSecretResolver()
	checkpointStore := checkpoint.NewStore(checkpoint.DefaultRoot(options.ConfigPath))
	deviceRuntime := dependencies.newDeviceRuntime(device.RuntimeConfig{
		BaseURL: deviceURL, AccessTokenReference: document.Device.AccessTokenSecret, Resolver: keychain,
	})
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	runtimeDone := make(chan error, 1)
	go func() { runtimeDone <- deviceRuntime.Run(runtimeCtx) }()
	output := device.NewOutput(deviceRuntime, device.OutputOptions{})
	assetReconciler := assets.NewReconciler(output)
	gateway, err := device.NewGateway(output, assetReconciler)
	if err != nil {
		cancelRuntime()
		<-runtimeDone
		return failure(ErrorOperational, "construct device gateway failed", err)
	}
	defer func() {
		result = errors.Join(result, shutdownDeviceWithBudgets(
			context.Background(), gateway, output, cancelRuntime, runtimeDone, defaultShutdownBudgets(),
		))
	}()

	logs := pluginlog.New(stderr, pluginlog.Options{})
	defer func() {
		result = errors.Join(result, runShutdownPhase(context.Background(), defaultShutdownBudgets().logs, logs.Close))
	}()
	renderer := &audioDiagnosticRenderer{gateway: gateway, logs: logs}
	desiredState, err := daemon.NewDesiredState(configStore, nil)
	if err != nil {
		return failure(ErrorOperational, "construct desired state failed", err)
	}
	liveState := daemon.NewLiveState()
	relayCtx, cancelRelays := context.WithCancel(context.Background())
	defer cancelRelays()
	sessionChanges := make(chan daemon.SessionChange, 64)
	sessions := daemon.NewSessionCoordinator(func(change daemon.SessionChange) {
		select {
		case sessionChanges <- change:
		case <-relayCtx.Done():
		}
	})
	retiredInstances := make(chan retiredInstance, 64)
	sessionFailures := make(chan eventbus.Failure, 64)
	sessionContextInvalidations := make(chan sessionContextInvalidation)
	checkpointState, err := daemon.NewCheckpoints(checkpointStore, liveState)
	if err != nil {
		return failure(ErrorOperational, "construct checkpoint state failed", err)
	}

	observationStore := observation.NewStore(func(pluginID, instanceID string) (uint64, bool) {
		if pluginID == "bsbctl" && instanceID == "launcher" {
			return 1, true
		}
		return liveState.Generation(pluginID, instanceID)
	}, time.Now)
	callbacks := daemonPluginLogCallbacks(logs)
	callbacks.Observe = daemonObservationCallback(observationStore, assetReconciler)
	callbacks.Withdraw = func(pluginID string, request protocol.WithdrawRequest) error {
		return observationStore.Withdraw(pluginID, request.Instance.ID, request.Channel, request.Key)
	}
	callbacks.WithdrawGeneration = observationStore.WithdrawGeneration
	callbacks.WithdrawInstance = func(pluginID, instanceID string, generation uint64) {
		select {
		case retiredInstances <- retiredInstance{pluginID: pluginID, instanceID: instanceID, generation: generation}:
		case <-relayCtx.Done():
		}
	}
	callbacks.Checkpoint = checkpointState.SaveCheckpoint
	callbacks.SessionInvalidated = sessions.PluginSessionInvalidated
	callbacks.SessionCleanup = sessions.PluginSessionCleanup
	callbacks.BeginExecution = func(ctx context.Context, pluginID string, request protocol.SessionExecutionRequest) error {
		return sessions.BeginExecution(ctx, liveState, pluginID, request)
	}
	callbacks.SessionCompleted = func(pluginID string, request protocol.CompleteSessionRequest) {
		sessions.PluginSessionCompleted(liveState, pluginID, request)
	}
	manager := &redactingPluginRuntime{pluginRuntime: dependencies.newPluginRuntime(callbacks), logs: logs}
	broker := eventbus.New(manager.SessionInputResult, func(_ context.Context, failure eventbus.Failure) {
		select {
		case sessionFailures <- failure:
		case <-relayCtx.Done():
		}
	})
	sessionRuntime, err := daemon.NewSessionRuntime(sessions, manager, broker, func(ctx context.Context) error {
		request := sessionContextInvalidation{done: make(chan struct{})}
		select {
		case sessionContextInvalidations <- request:
		case <-ctx.Done():
			return ctx.Err()
		case <-relayCtx.Done():
			return nil
		}
		select {
		case <-request.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-relayCtx.Done():
			return nil
		}
	})
	if err != nil {
		broker.Close()
		return failure(ErrorOperational, "construct session runtime failed", err)
	}
	policyResolver, err := daemon.NewPolicyResolver(liveState, sessions, assetReconciler)
	if err != nil {
		broker.Close()
		return failure(ErrorOperational, "construct presentation policy failed", err)
	}
	resolve := func(record observation.Record) (attention.Rule, bool) {
		if record.PluginID == "bsbctl" && record.Observation.Instance.ID == "launcher" {
			return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyInteractive, Foreground: true}, true
		}
		return policyResolver.Resolve(record)
	}
	recorder, recorderErr := dependencies.newAttentionRecorder(filepath.Join(filepath.Dir(options.ConfigPath), "attention.jsonl"), 2048, 10<<20, 3)
	if recorderErr != nil {
		_, _ = fmt.Fprintln(stderr, `{"component":"bsbctl","level":"warn","event":"attention_recorder_unavailable"}`)
		recorder = nil
	} else {
		defer func() { result = errors.Join(result, recorder.Close()) }()
	}
	engine, err := daemon.NewEngine(daemon.EngineOptions{
		Store: observationStore, Resolve: resolve, Renderer: renderer, Foreground: sessionRuntime,
		StateStore: attention.NewStateStore(filepath.Join(filepath.Dir(options.ConfigPath), "attention-state.json")),
		Generation: func(pluginID, instanceID string) (uint64, bool) {
			app, exists := document.Apps[instanceID]
			return app.Generation, exists && app.Enabled && app.PluginID == pluginID
		}, Recorder: recorder,
	})
	if err != nil {
		broker.Close()
		return failure(ErrorOperational, "construct attention engine failed", err)
	}
	reconciler, err := daemon.NewReconciler(daemon.ReconcilerOptions{
		Desired: desiredState, Live: liveState, Sessions: sessionRuntime, Policy: policyResolver, Checkpoints: checkpointState,
		Resolver: keychain, Plugins: manager, Attention: engine, Assets: assetReconciler,
	})
	if err != nil {
		broker.Close()
		return failure(ErrorOperational, "construct reconciler failed", err)
	}
	defer func() {
		result = errors.Join(result, runShutdownPhase(context.Background(), defaultShutdownBudgets().service, reconciler.Close))
	}()
	defer broker.Close()

	pluginInstaller, err := prepareRuntimeInstaller(options.ConfigPath, reconciler, dependencies)
	if err != nil {
		return err
	}
	packageOps, err := daemon.NewPackageOps(pluginInstaller, liveState)
	if err != nil {
		return failure(ErrorOperational, "construct package operations failed", err)
	}

	runCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	var sessionRelayDone, retirementRelayDone, sessionFailureRelayDone, sessionContextRelayDone <-chan error
	defer func() {
		cancelRelays()
		result = errors.Join(result, waitRelays(sessionRelayDone, retirementRelayDone, sessionFailureRelayDone, sessionContextRelayDone))
	}()
	sessionRelayResult := make(chan error, 1)
	sessionRelayDone = sessionRelayResult
	go func() { sessionRelayResult <- runSessionChangeRelay(relayCtx, sessionChanges, broker, engine.Wake) }()
	retirementRelayResult := make(chan error, 1)
	retirementRelayDone = retirementRelayResult
	go func() { retirementRelayResult <- runInstanceRetirementRelay(relayCtx, retiredInstances, engine) }()
	sessionFailureRelayResult := make(chan error, 1)
	sessionFailureRelayDone = sessionFailureRelayResult
	go func() { sessionFailureRelayResult <- runSessionFailureRelay(relayCtx, sessionFailures, sessionRuntime) }()
	if err := reconciler.Load(ctx); err != nil {
		return failure(ErrorOperational, "initial daemon reconciliation failed", err)
	}

	launcher := busyinput.NewRouter(&launcherAdapter{reconciler: reconciler, logs: logs, fallbackLogged: make(map[string]struct{})}, func(value protocol.Observation) error {
		return observationStore.Publish(observation.Source{PluginID: "bsbctl", Generation: 1}, value)
	}, func() {
		_ = observationStore.Withdraw("bsbctl", "launcher", "apps", "menu")
	}, time.Now)
	backHandling := busyinput.BackHandling{
		Publish: func(ctx context.Context, instanceID, token string, payload protocol.SessionInput, occurredAt time.Time) (protocol.SessionInputResult, error) {
			ref, foregroundToken := reconciler.ForegroundSessionRef()
			if ref.ID != instanceID || foregroundToken != token {
				return protocol.SessionInputResult{}, errors.New("session input target is no longer foreground")
			}
			return broker.PublishSessionInputAndWait(ctx, ref, token, &payload, occurredAt)
		},
		Begin: func() busyinput.BackAttempt {
			captured := engine.CaptureBackPresentation()
			return busyinput.BackAttempt{
				Consumed: func(ctx context.Context) error {
					return engine.ReconcileConsumedBack(ctx, captured, gateway.InvalidateCanvas)
				},
				Fallback: func(ctx context.Context, reason string) error {
					return engine.DismissForBack(ctx, captured, reason)
				},
			}
		},
	}
	coordinator := busyinput.NewCoordinator(launcher, reconciler, reconciler, func(instanceID, token string, payload protocol.SessionInput, occurredAt time.Time) error {
		ref, foregroundToken := reconciler.ForegroundSessionRef()
		if ref.ID != instanceID || foregroundToken != token {
			return errors.New("session input target is no longer foreground")
		}
		return broker.PublishSessionInput(ref, token, &payload, occurredAt)
	}, backHandling, func() {
		gateway.InvalidateCanvas()
		engine.InvalidateRenderedSelection()
	}, engine.Reconcile, time.Now)
	dispatcher := busyinput.NewDispatcherWithClearCapture(coordinator.Handle, func() func(context.Context) {
		ref, token := reconciler.ForegroundSessionRef()
		if ref.ID == "" || token == "" {
			return nil
		}
		return func(clearCtx context.Context) {
			reconciler.ClearForegroundSessionContext(clearCtx, ref.ID, token)
		}
	})
	sessionContextRelayResult := make(chan error, 1)
	sessionContextRelayDone = sessionContextRelayResult
	go func() {
		sessionContextRelayResult <- runSessionContextRelay(relayCtx, sessionContextInvalidations, launcher, dispatcher)
	}()

	assetRetry := make(chan struct{}, 1)
	identityRead := make(chan struct{}, 1)
	subscriber, err := device.NewStatusSubscriber(device.StatusSubscriberOptions{
		Factory: deviceRuntime.NewStatusStream, Submit: dispatcher.Submit, Backoff: 2 * time.Second,
		Observer: deviceRuntime,
		OnConnected: func() {
			assetReconciler.InvalidateConnection()
			gateway.InvalidateConnection()
			engine.InvalidateRenderedSelection()
			select {
			case identityRead <- struct{}{}:
			default:
			}
			select {
			case assetRetry <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		return failure(ErrorOperational, "construct device status subscriber failed", err)
	}
	runtimeStatus, err := daemon.NewRuntimeStatus(daemon.RuntimeStatusOptions{
		Live: liveState, Plugins: manager, Assets: assetReconciler, Sessions: sessionRuntime,
		Attention: engine, Configuration: desiredState, Checkpoints: checkpointState,
		Input: dispatcher, Device: deviceRuntime, Output: output, Audio: gateway, Logs: logs,
	})
	if err != nil {
		return failure(ErrorOperational, "construct runtime status failed", err)
	}
	server, err := control.Listen(options.SocketPath, options.Version, control.Backends{
		Apps: reconciler, Catalog: packageOps, Operations: reconciler, Attention: reconciler, Status: runtimeStatus,
	})
	if err != nil {
		return failure(ErrorOperational, "open daemon control socket failed", err)
	}

	errorsC := make(chan error, 6)
	go func() { errorsC <- server.Serve(runCtx) }()
	go func() { errorsC <- engine.Run(runCtx) }()
	go func() { errorsC <- subscriber.Run(runCtx) }()
	go func() { errorsC <- dispatcher.Run(runCtx) }()
	go func() {
		errorsC <- dependencies.runAssetRetry(runCtx, deviceRuntime, reconciler, device.AssetRetryOptions{Wake: assetRetry})
	}()
	go func() {
		errorsC <- runDeviceIdentityDiagnostics(runCtx, deviceRuntime, identityRead, func(notification protocol.LogNotification) {
			logs.Log("bsbctl", notification)
		})
	}()
	first := <-errorsC
	cancelWorkers()
	second := <-errorsC
	third := <-errorsC
	fourth := <-errorsC
	fifth := <-errorsC
	sixth := <-errorsC
	return errors.Join(first, second, third, fourth, fifth, sixth)
}

func recoverInstallerState(ctx context.Context, configPath string, store daemon.ConfigurationStore, dependencies dependencies) error {
	if _, err := dependencies.recoverInstaller(ctx, installer.RecoveryOptions{
		Root: defaultInstallerRoot(configPath), Activator: daemon.NewConfigStoreActivator(store),
	}); err != nil {
		switch installer.CodeOf(err) {
		case installer.CodeRecoveryRequired, installer.CodeStateFailed, installer.CodeActivationFailed:
			return failure(ErrorPartial, "plugin installer recovery is required", err)
		default:
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return failure(ErrorOperational, "plugin installer recovery failed", err)
		}
	}
	return nil
}

func prepareRuntimeInstaller(configPath string, activator installer.Activator, dependencies dependencies) (runtimeInstaller, error) {
	keyring, err := dependencies.productionCatalogKeyring()
	if err != nil {
		return nil, failure(ErrorOperational, "load catalog public keys failed", err)
	}
	value, err := dependencies.newRuntimeInstaller(installer.Options{
		Root: defaultInstallerRoot(configPath), Keyring: keyring, Activator: activator,
	})
	if err != nil {
		return nil, failure(ErrorOperational, "initialize plugin installer failed", err)
	}
	return value, nil
}

func defaultInstallerRoot(configPath string) string {
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		absolute = filepath.Clean(configPath)
	}
	return filepath.Join(filepath.Dir(absolute), "installer")
}
