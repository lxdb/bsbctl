package daemon

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
)

type AppReadinessPhase string

const (
	AppDisabled         AppReadinessPhase = "disabled"
	AppSecretPending    AppReadinessPhase = "secret_pending"
	AppReconcilePending AppReadinessPhase = "reconcile_pending"
	AppReady            AppReadinessPhase = "ready"

	secretUnavailableCode = "secret_unavailable"
	reconcileFailedCode   = "reconcile_failed"
)

// AppReadiness is safe to expose through the local control API. It deliberately
// omits secret references, values, resolver errors, accounts, and domains.
type AppReadiness struct {
	AppID         string            `json:"app_id"`
	PluginID      string            `json:"plugin_id"`
	Phase         AppReadinessPhase `json:"phase"`
	Attempt       int               `json:"attempt"`
	RetryAt       time.Time         `json:"retry_at,omitempty"`
	LastErrorCode string            `json:"last_error_code,omitempty"`
}

// ReconciliationPlan is the structurally valid, currently runnable subset of
// desired state. Enabled apps whose secrets are unavailable are represented in
// Readiness but intentionally absent from Specs and Generations.
type ReconciliationPlan struct {
	Specs       []pluginhost.Spec
	Generations Generations
	Readiness   []AppReadiness
}

// SecretResolver turns a durable opaque reference into a value delivered only
// over the private inherited plugin socket.
type SecretResolver interface {
	Resolve(context.Context, string) (string, error)
}

// Generations is an immutable lookup used to reject publications from stale
// app configurations.
type Generations struct {
	values map[generationKey]uint64
}

type generationKey struct {
	pluginID   string
	instanceID string
}

func (g Generations) Lookup(pluginID, instanceID string) (uint64, bool) {
	value, ok := g.values[generationKey{pluginID: pluginID, instanceID: instanceID}]
	return value, ok
}

func (g Generations) matches(pluginID, instanceID string, generation uint64) bool {
	current, exists := g.Lookup(pluginID, instanceID)
	return exists && current == generation
}

// BuildPlan first performs structural validation as a hard transaction gate,
// then fault-contains runtime secret resolution to each enabled app.
// buildPlan can retain already-ready secret material while resolving only a
// selected pending subset. A nil shouldResolve resolves every enabled app.
func buildPlan(
	ctx context.Context,
	document config.Document,
	resolver SecretResolver,
	retained map[string]pluginhost.Instance,
	shouldResolve func(string) bool,
) (ReconciliationPlan, error) {
	if err := document.Validate(); err != nil {
		return ReconciliationPlan{}, err
	}
	instances := make(map[string][]pluginhost.Instance)
	appIDs := slices.Sorted(maps.Keys(document.Apps))
	generations := Generations{values: make(map[generationKey]uint64)}
	readiness := make([]AppReadiness, 0, len(appIDs))
	for _, id := range appIDs {
		app := document.Apps[id]
		if !app.Enabled {
			readiness = append(readiness, AppReadiness{AppID: app.ID, PluginID: app.PluginID, Phase: AppDisabled})
			continue
		}
		if instance, exists := retained[app.ID]; exists && (shouldResolve == nil || !shouldResolve(app.ID)) {
			instance.ID = app.ID
			instance.Generation = app.Generation
			instance.Config = slices.Clone(app.Config)
			instance.Policies = runtimePolicies(app.Policies)
			instance.Checkpoint = nil
			instances[app.PluginID] = append(instances[app.PluginID], instance)
			generations.values[generationKey{pluginID: app.PluginID, instanceID: app.ID}] = instance.Generation
			readiness = append(readiness, AppReadiness{AppID: app.ID, PluginID: app.PluginID, Phase: AppReady})
			continue
		}
		if shouldResolve != nil && !shouldResolve(app.ID) {
			readiness = append(readiness, AppReadiness{
				AppID: app.ID, PluginID: app.PluginID, Phase: AppSecretPending,
				LastErrorCode: secretUnavailableCode,
			})
			continue
		}
		secrets := make(map[string]string, len(app.Secrets))
		secretNames := slices.Sorted(maps.Keys(app.Secrets))
		secretPending := false
		for _, name := range secretNames {
			if resolver == nil {
				secretPending = true
				break
			}
			value, err := resolver.Resolve(ctx, app.Secrets[name])
			if err != nil {
				secretPending = true
				break
			}
			secrets[name] = value
		}
		if secretPending {
			readiness = append(readiness, AppReadiness{
				AppID: app.ID, PluginID: app.PluginID, Phase: AppSecretPending,
				LastErrorCode: secretUnavailableCode,
			})
			continue
		}
		instance := pluginhost.Instance{
			ID: app.ID, Generation: app.Generation,
			Config: slices.Clone(app.Config), Secrets: secrets, Policies: runtimePolicies(app.Policies),
		}
		instances[app.PluginID] = append(instances[app.PluginID], instance)
		generations.values[generationKey{pluginID: app.PluginID, instanceID: app.ID}] = instance.Generation
		readiness = append(readiness, AppReadiness{AppID: app.ID, PluginID: app.PluginID, Phase: AppReady})
	}

	pluginIDs := slices.Sorted(maps.Keys(instances))
	specs := make([]pluginhost.Spec, 0, len(pluginIDs))
	for _, id := range pluginIDs {
		plugin := document.Plugins[id]
		specs = append(specs, pluginhost.Spec{
			ID: plugin.ID, Version: plugin.Version, Executable: plugin.Executable, SHA256: plugin.SHA256,
			ProtocolVersion: plugin.ProtocolVersion,
			ExecutionModes:  slices.Clone(plugin.ExecutionModes),
			Channels:        slices.Clone(plugin.Channels),
			Operations:      slices.Clone(plugin.Operations),
			Instances:       instances[id],
		})
	}
	return ReconciliationPlan{Specs: specs, Generations: generations, Readiness: readiness}, nil
}

func clonePolicies(source map[string]presentation.PolicyConfig) map[string]presentation.PolicyConfig {
	return maps.Clone(source)
}

func runtimePolicies(source map[string]presentation.PolicyConfig) map[string]presentation.PolicyConfig {
	return clonePolicies(source)
}
