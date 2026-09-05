package daemonrun

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/deviceownership"
	"github.com/lxdb/bsbctl/internal/installer"
	"github.com/lxdb/bsbctl/internal/pluginhost"
)

func TestRunSameDeviceOwnershipStopsAfterRecoveryAndBeforeRuntime(t *testing.T) {
	deviceURL := "http://" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + ".local"
	owner, err := deviceownership.Acquire(deviceURL, device.ApplicationName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })

	configPath := filepath.Join(t.TempDir(), "other-config.json")
	document := serviceMainDocument()
	document.Device.BaseURL = strings.ToUpper(deviceURL[:4]) + deviceURL[4:] + ":80/"
	if _, err := config.NewStore(configPath).ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	dependencies := productionDependencies("test")
	previousDevice := dependencies.newDeviceRuntime
	previousAcquire := dependencies.acquireDeviceOwnership
	var recoveryCalls, installerConstructions, deviceConstructions atomic.Int32
	dependencies.recoverInstaller = func(context.Context, installer.RecoveryOptions) (installer.Result, error) {
		recoveryCalls.Add(1)
		return installer.Result{Status: installer.StatusNoRecovery}, nil
	}
	dependencies.acquireDeviceOwnership = func(baseURL, application string) (deviceOwnership, error) {
		if recoveryCalls.Load() != 1 {
			t.Fatal("device ownership was attempted before one-shot installer recovery")
		}
		return previousAcquire(baseURL, application)
	}
	dependencies.newRuntimeInstaller = func(installer.Options) (runtimeInstaller, error) {
		installerConstructions.Add(1)
		return &startupInstaller{}, nil
	}
	dependencies.newDeviceRuntime = func(value device.RuntimeConfig) *device.Runtime {
		deviceConstructions.Add(1)
		return previousDevice(value)
	}

	err = run(context.Background(), Options{
		Version: "test", ConfigPath: configPath, SocketPath: filepath.Join(t.TempDir(), "other-control.sock"), Stderr: io.Discard,
	}, dependencies)
	code, message := classifyRunError(err)
	wantMessage := fmt.Sprintf("device display is already owned by bsbctl process %d", os.Getpid())
	if code != ErrorOperational || message != wantMessage {
		t.Fatalf("exit=%d message=%q err=%v", code, message, err)
	}
	if recoveryCalls.Load() != 1 || installerConstructions.Load() != 0 || deviceConstructions.Load() != 0 {
		t.Fatalf("contender crossed startup: recoveries=%d installers=%d devices=%d", recoveryCalls.Load(), installerConstructions.Load(), deviceConstructions.Load())
	}
	if strings.Contains(message, configPath) || strings.Contains(message, deviceURL) {
		t.Fatalf("collision diagnostic leaks config or device identity: %q", message)
	}
}

func TestRunRecoveryRequiredStopsBeforeConfigurationAndRuntimeConstruction(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if _, err := config.NewStore(configPath).ReplaceWithOutcome(0, serviceMainDocument()); err != nil {
		t.Fatal(err)
	}
	dependencies := testDaemonDependencies()
	previousPlugins := dependencies.newPluginRuntime
	previousDevice := dependencies.newDeviceRuntime
	var recoveryCalls, runtimeInstallerConstructions, pluginConstructions, deviceConstructions atomic.Int32
	dependencies.recoverInstaller = func(context.Context, installer.RecoveryOptions) (installer.Result, error) {
		recoveryCalls.Add(1)
		return installer.Result{}, &installer.Error{Code: installer.CodeRecoveryRequired}
	}
	dependencies.newRuntimeInstaller = func(installer.Options) (runtimeInstaller, error) {
		runtimeInstallerConstructions.Add(1)
		return &startupInstaller{}, nil
	}
	dependencies.newPluginRuntime = func(callbacks pluginhost.Callbacks) pluginRuntime {
		pluginConstructions.Add(1)
		return previousPlugins(callbacks)
	}
	dependencies.newDeviceRuntime = func(value device.RuntimeConfig) *device.Runtime {
		deviceConstructions.Add(1)
		return previousDevice(value)
	}
	err := run(context.Background(), Options{
		Version: "test", ConfigPath: configPath, SocketPath: filepath.Join(t.TempDir(), "control.sock"), Stderr: io.Discard,
	}, dependencies)
	code, _ := classifyRunError(err)
	if code != ErrorPartial || recoveryCalls.Load() != 1 || runtimeInstallerConstructions.Load() != 0 || pluginConstructions.Load() != 0 || deviceConstructions.Load() != 0 {
		t.Fatalf("startup gate code=%d recoveries=%d runtime_installers=%d plugins=%d devices=%d err=%v", code, recoveryCalls.Load(), runtimeInstallerConstructions.Load(), pluginConstructions.Load(), deviceConstructions.Load(), err)
	}
}

type noopDeviceOwnership struct{}

func (noopDeviceOwnership) Close() error { return nil }

func testDaemonDependencies() dependencies {
	dependencies := productionDependencies("test")
	dependencies.acquireDeviceOwnership = func(string, string) (deviceOwnership, error) { return noopDeviceOwnership{}, nil }
	return dependencies
}

func classifyRunError(err error) (ErrorKind, string) {
	typed, ok := errors.AsType[*Error](err)
	if !ok {
		return 0, ""
	}
	return typed.Kind, typed.Message
}

func TestPrepareRuntimeInstallerUsesDeterministicRootAndInjectedKeyring(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "nested", "config.json")
	store := config.NewStore(configPath)
	activator := daemon.NewConfigStoreActivator(store)
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("q", ed25519.SeedSize)))
	public := private.Public().(ed25519.PublicKey)
	fake := &startupInstaller{}
	dependencies := testDaemonDependencies()
	var captured installer.Options
	dependencies.productionCatalogKeyring = func() (catalog.Keyring, error) { return catalog.Keyring{"stable": public}, nil }
	dependencies.newRuntimeInstaller = func(options installer.Options) (runtimeInstaller, error) {
		captured = options
		return fake, nil
	}
	value, err := prepareRuntimeInstaller(configPath, activator, dependencies)
	if err != nil || value != fake {
		t.Fatalf("prepareRuntimeInstaller = %#v, %v; fake=%#v", value, err, fake)
	}
	wantRoot := filepath.Join(filepath.Dir(configPath), "installer")
	if captured.Root != wantRoot || !filepath.IsAbs(captured.Root) || !bytes.Equal(captured.Keyring["stable"], public) || captured.Activator != activator {
		t.Fatalf("installer options root=%q keyring=%#v", captured.Root, captured.Keyring)
	}
}

func TestRecoverInstallerStateUsesOneShotConfigStoreActivator(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "nested", "config.json")
	store := config.NewStore(configPath)
	dependencies := testDaemonDependencies()
	var captured installer.RecoveryOptions
	dependencies.recoverInstaller = func(_ context.Context, options installer.RecoveryOptions) (installer.Result, error) {
		captured = options
		return installer.Result{Status: installer.StatusNoRecovery}, nil
	}
	if err := recoverInstallerState(context.Background(), configPath, store, dependencies); err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(filepath.Dir(configPath), "installer")
	if captured.Root != wantRoot || captured.Activator == nil {
		t.Fatalf("recovery options root=%q activator=%T", captured.Root, captured.Activator)
	}
}

func TestPrepareRuntimeInstallerRejectsInvalidProductionCatalogKeyring(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	dependencies := testDaemonDependencies()
	var installerConstructions atomic.Int32
	dependencies.productionCatalogKeyring = func() (catalog.Keyring, error) { return nil, errors.New("malformed tracked key document") }
	dependencies.newRuntimeInstaller = func(installer.Options) (runtimeInstaller, error) {
		installerConstructions.Add(1)
		return &startupInstaller{}, nil
	}

	value, err := prepareRuntimeInstaller(configPath, daemon.NewConfigStoreActivator(config.NewStore(configPath)), dependencies)
	code, message := classifyRunError(err)
	if value != nil || code != ErrorOperational || message != "load catalog public keys failed" || installerConstructions.Load() != 0 {
		t.Fatalf("prepareRuntimeInstaller = %#v code=%d message=%q constructions=%d err=%v", value, code, message, installerConstructions.Load(), err)
	}
}

func TestRunRecoversBeforeClassifyingInvalidConfiguration(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := testDaemonDependencies()
	previousDevice := dependencies.newDeviceRuntime
	var recoveryCalls, runtimeInstallerConstructions, deviceConstructions atomic.Int32
	dependencies.recoverInstaller = func(context.Context, installer.RecoveryOptions) (installer.Result, error) {
		recoveryCalls.Add(1)
		return installer.Result{}, nil
	}
	dependencies.newRuntimeInstaller = func(installer.Options) (runtimeInstaller, error) {
		runtimeInstallerConstructions.Add(1)
		return &startupInstaller{}, nil
	}
	dependencies.newDeviceRuntime = func(value device.RuntimeConfig) *device.Runtime {
		deviceConstructions.Add(1)
		return previousDevice(value)
	}

	err := run(context.Background(), Options{
		Version: "test", ConfigPath: configPath, SocketPath: filepath.Join(t.TempDir(), "control.sock"), Stderr: io.Discard,
	}, dependencies)
	code, message := classifyRunError(err)
	if code != ErrorInvalidInput || message != "daemon configuration is invalid" || recoveryCalls.Load() != 1 || runtimeInstallerConstructions.Load() != 0 || deviceConstructions.Load() != 0 {
		t.Fatalf("invalid config code=%d message=%q recoveries=%d runtime_installers=%d devices=%d err=%v", code, message, recoveryCalls.Load(), runtimeInstallerConstructions.Load(), deviceConstructions.Load(), err)
	}
	err = run(context.Background(), Options{
		Version: "test", ConfigPath: filepath.Join(t.TempDir(), "missing.json"), SocketPath: filepath.Join(t.TempDir(), "control.sock"), Stderr: io.Discard,
	}, dependencies)
	code, message = classifyRunError(err)
	if code != ErrorOperational || message != "load daemon configuration failed" || recoveryCalls.Load() != 2 || runtimeInstallerConstructions.Load() != 0 || deviceConstructions.Load() != 0 {
		t.Fatalf("missing config code=%d message=%q recoveries=%d runtime_installers=%d devices=%d err=%v", code, message, recoveryCalls.Load(), runtimeInstallerConstructions.Load(), deviceConstructions.Load(), err)
	}
}

type startupInstaller struct{}

func (*startupInstaller) InstallFirst(context.Context, installer.InstallRequest) (installer.Result, error) {
	return installer.Result{}, errors.New("not used")
}
func (*startupInstaller) Update(context.Context, installer.InstallRequest) (installer.Result, error) {
	return installer.Result{}, errors.New("not used")
}
func (*startupInstaller) Rollback(context.Context, installer.RollbackRequest) (installer.Result, error) {
	return installer.Result{}, errors.New("not used")
}
func (*startupInstaller) Snapshot(context.Context, string) (installer.Snapshot, error) {
	return installer.Snapshot{}, errors.New("not used")
}

var _ daemon.InstallerController = (*startupInstaller)(nil)
