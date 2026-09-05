package daemonrun

import (
	"context"
	"sync"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/deviceownership"
	"github.com/lxdb/bsbctl/internal/installer"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/pluginlog"
	"github.com/lxdb/bsbctl/internal/releasekeys"
	"github.com/lxdb/bsbctl/internal/secrets"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type secretResolver interface {
	Resolve(context.Context, string) (string, error)
}

type pluginRuntime interface {
	daemon.PluginController
	SessionInputResult(context.Context, string, protocol.SessionInputRequest) (protocol.SessionInputResult, error)
}

type redactingPluginRuntime struct {
	pluginRuntime
	lifecycleMu sync.Mutex
	logs        *pluginlog.Sink
}

func (r *redactingPluginRuntime) Apply(ctx context.Context, specs []pluginhost.Spec) error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	secrets := pluginSecrets(specs)
	r.logs.MergeSecrets(secrets)
	if err := r.pluginRuntime.Apply(ctx, specs); err != nil {
		return err
	}
	r.logs.ReplaceSecrets(secrets)
	return nil
}

func (r *redactingPluginRuntime) Close(ctx context.Context) error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if err := r.pluginRuntime.Close(ctx); err != nil {
		return err
	}
	r.logs.ReplaceSecrets(nil)
	return nil
}

func pluginSecrets(specs []pluginhost.Spec) map[string][]string {
	result := make(map[string][]string, len(specs))
	for _, spec := range specs {
		for _, instance := range spec.Instances {
			for _, value := range instance.Secrets {
				if value != "" {
					result[spec.ID] = append(result[spec.ID], value)
				}
			}
		}
	}
	return result
}

type deviceOwnership interface {
	Close() error
}

type runtimeInstaller interface {
	daemon.InstallerController
}

type dependencies struct {
	newSecretResolver        func() secretResolver
	newDeviceRuntime         func(device.RuntimeConfig) *device.Runtime
	newAttentionRecorder     func(string, int, int64, int) (*attention.Recorder, error)
	runAssetRetry            func(context.Context, device.AssetRuntime, device.AssetReconciler, device.AssetRetryOptions) error
	newPluginRuntime         func(pluginhost.Callbacks) pluginRuntime
	productionCatalogKeyring func() (catalog.Keyring, error)
	acquireDeviceOwnership   func(string, string) (deviceOwnership, error)
	recoverInstaller         func(context.Context, installer.RecoveryOptions) (installer.Result, error)
	newRuntimeInstaller      func(installer.Options) (runtimeInstaller, error)
}

func productionDependencies(version string) dependencies {
	return dependencies{
		newSecretResolver:    func() secretResolver { return secrets.NewKeychain(nil) },
		newDeviceRuntime:     func(config device.RuntimeConfig) *device.Runtime { return device.NewRuntime(config) },
		newAttentionRecorder: attention.NewRecorder,
		runAssetRetry:        device.RunAssetRetry,
		newPluginRuntime: func(callbacks pluginhost.Callbacks) pluginRuntime {
			return pluginhost.NewManager(version, nil, callbacks)
		},
		productionCatalogKeyring: releasekeys.CatalogKeyring,
		acquireDeviceOwnership: func(baseURL, application string) (deviceOwnership, error) {
			return deviceownership.Acquire(baseURL, application)
		},
		recoverInstaller:    installer.Recover,
		newRuntimeInstaller: func(options installer.Options) (runtimeInstaller, error) { return installer.New(options) },
	}
}
