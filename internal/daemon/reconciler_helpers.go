package daemon

import (
	"maps"
	"math"
	"slices"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/pluginhost"
)

func readyInstancesFromSpecs(specs []pluginhost.Spec) map[string]pluginhost.Instance {
	retained := make(map[string]pluginhost.Instance)
	for _, spec := range specs {
		for _, instance := range spec.Instances {
			instance.Config = slices.Clone(instance.Config)
			instance.Secrets = cloneStringMap(instance.Secrets)
			instance.Policies = clonePolicies(instance.Policies)
			if instance.Checkpoint != nil {
				restore := *instance.Checkpoint
				restore.Data = slices.Clone(instance.Checkpoint.Data)
				instance.Checkpoint = &restore
			}
			retained[instance.ID] = instance
		}
	}
	return retained
}

func secretRetryDelay(attempt int, jitter float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 5 {
		shift = 5
	}
	seconds := 1 << shift
	if seconds > 30 {
		seconds = 30
	}
	if math.IsNaN(jitter) {
		jitter = 1
	}
	if jitter < .8 {
		jitter = .8
	}
	if jitter > 1.2 {
		jitter = 1.2
	}
	delay := time.Duration(float64(time.Duration(seconds)*time.Second) * jitter)
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func cloneCandidate(source *reconcileCandidate) *reconcileCandidate {
	if source == nil {
		return nil
	}
	return &reconcileCandidate{epoch: source.epoch, revision: source.revision, plan: source.plan}
}

func cloneDocument(source config.Document) config.Document {
	result := source
	result.Plugins = make(map[string]config.Plugin, len(source.Plugins))
	for id, plugin := range source.Plugins {
		plugin.ExecutionModes = slices.Clone(plugin.ExecutionModes)
		plugin.Channels = slices.Clone(plugin.Channels)
		plugin.Assets = slices.Clone(plugin.Assets)
		result.Plugins[id] = plugin
	}
	result.Apps = make(map[string]config.App, len(source.Apps))
	for id, app := range source.Apps {
		app.Config = slices.Clone(app.Config)
		app.Secrets = cloneStringMap(app.Secrets)
		app.Policies = clonePolicies(app.Policies)
		result.Apps[id] = app
	}
	return result
}

func clonePlugin(plugin config.Plugin) config.Plugin {
	plugin.ExecutionModes = slices.Clone(plugin.ExecutionModes)
	plugin.Channels = slices.Clone(plugin.Channels)
	plugin.Assets = slices.Clone(plugin.Assets)
	return plugin
}

func cloneStringMap(source map[string]string) map[string]string {
	return maps.Clone(source)
}
