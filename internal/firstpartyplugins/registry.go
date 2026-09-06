// Package firstpartyplugins owns immutable metadata shared by the built-in
// plugin development, verification, packaging, and release workflows.
package firstpartyplugins

import (
	"bytes"
	"maps"
	"path/filepath"
	"slices"

	"github.com/lxdb/bsbctl/internal/appsetup"
	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/plugins/calendar"
	"github.com/lxdb/bsbctl/plugins/codex"
	"github.com/lxdb/bsbctl/plugins/codexquota"
	"github.com/lxdb/bsbctl/plugins/githubnotifications"
	"github.com/lxdb/bsbctl/plugins/macresources"
	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
)

// Descriptor is the canonical non-version metadata for one first-party plugin.
// Runtime code must remain plugin-agnostic and must not branch on these IDs.
type Descriptor struct {
	ID                   string
	DevelopmentVersion   string
	DisplayName          string
	Description          string
	Requirement          string
	Binary               string
	CommandPackage       string
	TagPrefix            string
	ReleaseTitle         string
	DefinitionForVersion func(string) pluginsdk.Definition
	SchemaPath           string
	AssetRoot            string
	Assets               []assets.Declaration
	FixturePath          string
	SoakProfile          string
	SoakExclusion        string
	DefaultApp           config.App
	Setup                appsetup.Runner
}

var descriptors = []Descriptor{
	{
		ID: calendar.PluginID, DevelopmentVersion: calendar.PluginVersion, DisplayName: "Calendar", Binary: "bsbctl-plugin-calendar",
		Description: "Show upcoming and active meetings from Apple Calendar.", Requirement: "Calendar access on macOS",
		CommandPackage: "./cmd/bsbctl-plugin-calendar", TagPrefix: "plugin/calendar/v", ReleaseTitle: "Calendar plugin",
		DefinitionForVersion: calendar.DefinitionForVersion, SchemaPath: "plugins/calendar/config.schema.json",
		FixturePath: "docs/protocol/v1/fixtures/plugins/calendar.json", SoakExclusion: "requires EventKit",
		DefaultApp: config.App{
			ID: calendar.AppID, PluginID: calendar.PluginID, Enabled: true, Config: []byte(`{}`), LaunchAction: calendar.CalendarOpenAction,
			Policies: map[string]presentation.PolicyConfig{
				calendar.ChannelUpcoming:    {Policy: presentation.PolicyWhenRelevant, DevicePriority: 100, ActivationAction: calendar.CalendarOptionsAction},
				calendar.ChannelActive:      {Policy: presentation.PolicyWhenRelevant, DevicePriority: 100, ActivationAction: calendar.CalendarOptionsAction},
				calendar.ChannelInteraction: {Policy: presentation.PolicyInteractive},
			},
		},
	},
	{
		ID: codex.PluginID, DevelopmentVersion: codex.PluginVersion, DisplayName: "Codex", Binary: "bsbctl-plugin-codex",
		Description: "Show live Codex activity, progress, and requests.", Requirement: "Codex CLI app-server",
		CommandPackage: "./cmd/bsbctl-plugin-codex", TagPrefix: "plugin/codex/v", ReleaseTitle: "Codex plugin",
		DefinitionForVersion: codex.DefinitionForVersion, SchemaPath: "plugins/codex/config.schema.json",
		AssetRoot: "plugins/codex", Assets: codex.AssetDeclarations(),
		FixturePath: "docs/protocol/v1/fixtures/plugins/codex.json", SoakExclusion: "requires the Codex app server",
		DefaultApp: config.App{
			ID: codex.AppID, PluginID: codex.PluginID, Enabled: true, Config: []byte(`{}`), LaunchAction: "open",
			Policies: map[string]presentation.PolicyConfig{
				codex.ChannelAttention:     {Policy: presentation.PolicyAttention, ActivationAction: "open"},
				codex.ChannelGuidance:      {Policy: presentation.PolicyWhenRelevant},
				codex.ChannelOutcome:       {Policy: presentation.PolicyWhenRelevant, ActivationAction: "open"},
				codex.ChannelActivity:      {Policy: presentation.PolicyRotation, ActivationAction: "open", RotationIntervalMS: 30_000, RotationJitterPercent: 10},
				codex.ChannelProgress:      {Policy: presentation.PolicyWhenRelevant, ActivationAction: "open", CooldownMS: 1},
				codex.ChannelOverview:      {Policy: presentation.PolicyRotation, ActivationAction: "open", RotationIntervalMS: 60_000, RotationJitterPercent: 10},
				codex.ChannelConnection:    {Policy: presentation.PolicyWhenRelevant, ActivationAction: "open"},
				codex.ChannelDetail:        {Policy: presentation.PolicyInteractive},
				codex.ChannelQuotaSummary:  {Policy: presentation.PolicyRotation, RotationIntervalMS: 300_000, RotationJitterPercent: 10},
				codex.ChannelQuotaPressure: {Policy: presentation.PolicyWhenRelevant},
			},
		},
	},
	{
		ID: codexquota.PluginID, DevelopmentVersion: codexquota.PluginVersion, DisplayName: "Codex Quota", Binary: "bsbctl-plugin-codex-quota",
		Description: "Show Codex usage limits and reset windows.", Requirement: "Authenticated Codex profile",
		CommandPackage: "./cmd/bsbctl-plugin-codex-quota", TagPrefix: "plugin/codex-quota/v", ReleaseTitle: "Codex Quota plugin",
		DefinitionForVersion: codexquota.DefinitionForVersion, SchemaPath: "plugins/codexquota/config.schema.json",
		AssetRoot: "plugins/codexquota", Assets: codexquota.AssetDeclarations(),
		FixturePath: "docs/protocol/v1/fixtures/plugins/codex-quota.json", SoakProfile: "synthetic-resident-data-sources",
		DefaultApp: config.App{
			ID: codexquota.AppID, PluginID: codexquota.PluginID, Enabled: true, Config: []byte(`{}`), LaunchAction: "open",
			Policies: map[string]presentation.PolicyConfig{
				codexquota.ChannelSummary:  {Policy: presentation.PolicyRotation, RotationIntervalMS: 300_000, RotationJitterPercent: 10},
				codexquota.ChannelPressure: {Policy: presentation.PolicyWhenRelevant},
				codexquota.ChannelLive:     {Policy: presentation.PolicyInteractive},
			},
		},
	},
	{
		ID: githubnotifications.PluginID, DevelopmentVersion: githubnotifications.PluginVersion, DisplayName: "GitHub Notifications", Binary: "bsbctl-plugin-github-notifications",
		Description: "Show selected unread GitHub notification threads.", Requirement: "Classic GitHub token with notifications or repo scope",
		CommandPackage: "./cmd/bsbctl-plugin-github-notifications", TagPrefix: "plugin/github-notifications/v", ReleaseTitle: "GitHub Notifications plugin",
		DefinitionForVersion: githubnotifications.DefinitionForVersion, SchemaPath: "plugins/githubnotifications/config.schema.json",
		AssetRoot: "plugins/githubnotifications", Assets: githubnotifications.AssetDeclarations(),
		FixturePath: "docs/protocol/v1/fixtures/plugins/github-notifications.json", SoakProfile: "synthetic-resident-data-sources",
		Setup: githubnotifications.RunSetup,
		DefaultApp: config.App{
			ID: githubnotifications.AppID, PluginID: githubnotifications.PluginID, Enabled: true, Config: []byte(`{}`), LaunchAction: "open",
			Policies: map[string]presentation.PolicyConfig{
				githubnotifications.ChannelAttention: {
					Policy: presentation.PolicyAttention, ActivationAction: "open", ActivationInput: "start_or_encoder", RequiresAck: true,
				},
				githubnotifications.ChannelConnection: {Policy: presentation.PolicyWhenRelevant, ActivationAction: "open"},
				githubnotifications.ChannelLive:       {Policy: presentation.PolicyInteractive},
			},
		},
	},
	{
		ID: macresources.PluginID, DevelopmentVersion: macresources.PluginVersion, DisplayName: "Mac Resources", Binary: "bsbctl-plugin-mac-resources",
		Description: "Show CPU, memory, and network pressure.", Requirement: "No external account",
		CommandPackage: "./cmd/bsbctl-plugin-mac-resources", TagPrefix: "plugin/mac-resources/v", ReleaseTitle: "Mac Resources plugin",
		DefinitionForVersion: macresources.DefinitionForVersion, SchemaPath: "plugins/macresources/config.schema.json",
		FixturePath: "docs/protocol/v1/fixtures/plugins/mac-resources.json", SoakProfile: "synthetic-resident-data-sources",
		DefaultApp: config.App{
			ID: macresources.AppID, PluginID: macresources.PluginID, Enabled: true, Config: []byte(`{}`), LaunchAction: "open",
			Policies: map[string]presentation.PolicyConfig{
				macresources.ChannelSummary:  {Policy: presentation.PolicyRotation, RotationIntervalMS: 60_000, RotationJitterPercent: 10},
				macresources.ChannelPressure: {Policy: presentation.PolicyWhenRelevant},
				macresources.ChannelLive:     {Policy: presentation.PolicyInteractive},
			},
		},
	},
}

// All returns the complete registry in canonical launcher and release order.
func All() []Descriptor {
	result := slices.Clone(descriptors)
	for index := range result {
		result[index] = cloneDescriptor(result[index])
	}
	return result
}

// LookupID finds a first-party plugin by its stable package ID.
func LookupID(id string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.ID == id {
			return cloneDescriptor(descriptor), true
		}
	}
	return Descriptor{}, false
}

// LookupBinary finds a first-party plugin by executable basename.
func LookupBinary(binary string) (Descriptor, bool) {
	binary = filepath.Base(binary)
	for _, descriptor := range descriptors {
		if descriptor.Binary == binary {
			return cloneDescriptor(descriptor), true
		}
	}
	return Descriptor{}, false
}

// LookupAppID finds the default app profile for a first-party plugin.
func LookupAppID(id string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.DefaultApp.ID == id {
			return cloneDescriptor(descriptor), true
		}
	}
	return Descriptor{}, false
}

func cloneDescriptor(descriptor Descriptor) Descriptor {
	descriptor.Assets = slices.Clone(descriptor.Assets)
	descriptor.DefaultApp.Config = bytes.Clone(descriptor.DefaultApp.Config)
	descriptor.DefaultApp.Secrets = maps.Clone(descriptor.DefaultApp.Secrets)
	descriptor.DefaultApp.Policies = maps.Clone(descriptor.DefaultApp.Policies)
	return descriptor
}
