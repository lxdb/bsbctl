package daemon

import (
	"errors"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/checkpoint"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/eventbus"
	busyinput "github.com/lxdb/bsbctl/internal/input"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/pluginlog"
)

// RuntimeDiagnostics is the redacted health snapshot exposed to the local
// control plane. RuntimeStatus assembles it without owning mutable runtime
// truth.
type RuntimeDiagnostics struct {
	Assets               []assets.State
	SessionInput         []eventbus.Status
	Input                busyinput.DispatcherStatus
	Readiness            []AppReadiness
	Device               device.RuntimeStatus
	Output               device.OutputStatus
	Audio                device.AudioStatus
	AttentionRecorder    attention.RecorderStatus
	AttentionState       AttentionStateDiagnostics
	PresentationCooldown PresentationCooldownDiagnostics
	PluginLogs           pluginlog.Status
	Observations         observation.StoreDiagnostics
	Configuration        ConfigPersistenceStatus
	Checkpoints          checkpoint.Status
	Session              SessionDiagnostics
}

type inputDiagnostics interface {
	Status() busyinput.DispatcherStatus
}
type deviceDiagnostics interface{ Status() device.RuntimeStatus }
type outputDiagnostics interface{ Status() device.OutputStatus }
type audioDiagnostics interface{ Status() device.AudioStatus }
type logDiagnostics interface{ Status() pluginlog.Status }

type liveDiagnostics interface {
	Document() (config.Document, bool)
	AppReadiness() []AppReadiness
}
type pluginDiagnostics interface {
	Status() []pluginhost.PluginStatus
}
type assetDiagnostics interface{ Status() []assets.State }
type sessionDiagnostics interface {
	Diagnostics() SessionDiagnostics
	InputStatus() []eventbus.Status
}
type attentionDiagnostics interface {
	RecorderStatus() attention.RecorderStatus
	AttentionStateStatus() AttentionStateDiagnostics
	PresentationCooldownStatus() PresentationCooldownDiagnostics
	ObservationDiagnostics() observation.StoreDiagnostics
}
type configurationDiagnostics interface {
	PersistenceStatus() ConfigPersistenceStatus
}
type checkpointDiagnostics interface{ Status() checkpoint.Status }

// RuntimeStatusOptions names the read-only view of each runtime owner.
type RuntimeStatusOptions struct {
	Live          liveDiagnostics
	Plugins       pluginDiagnostics
	Assets        assetDiagnostics
	Sessions      sessionDiagnostics
	Attention     attentionDiagnostics
	Configuration configurationDiagnostics
	Checkpoints   checkpointDiagnostics
	Input         inputDiagnostics
	Device        deviceDiagnostics
	Output        outputDiagnostics
	Audio         audioDiagnostics
	Logs          logDiagnostics
}

// RuntimeStatus is a read-only control adapter over the runtime owners.
type RuntimeStatus struct {
	sources RuntimeStatusOptions
}

func NewRuntimeStatus(options RuntimeStatusOptions) (*RuntimeStatus, error) {
	required := []struct {
		name  string
		value any
	}{
		{name: "live diagnostics", value: options.Live},
		{name: "plugin diagnostics", value: options.Plugins},
		{name: "asset diagnostics", value: options.Assets},
		{name: "session diagnostics", value: options.Sessions},
		{name: "attention diagnostics", value: options.Attention},
		{name: "configuration diagnostics", value: options.Configuration},
		{name: "checkpoint diagnostics", value: options.Checkpoints},
		{name: "input diagnostics", value: options.Input},
		{name: "device diagnostics", value: options.Device},
		{name: "output diagnostics", value: options.Output},
		{name: "audio diagnostics", value: options.Audio},
		{name: "log diagnostics", value: options.Logs},
	}
	for _, dependency := range required {
		if dependency.value == nil {
			return nil, errors.New(dependency.name + " is required")
		}
	}
	return &RuntimeStatus{sources: options}, nil
}

func (s *RuntimeStatus) Document() (config.Document, bool) {
	return s.sources.Live.Document()
}

func (s *RuntimeStatus) Status() []pluginhost.PluginStatus {
	return s.sources.Plugins.Status()
}

func (s *RuntimeStatus) RuntimeDiagnostics() RuntimeDiagnostics {
	sources := s.sources
	return RuntimeDiagnostics{
		Assets:               sources.Assets.Status(),
		SessionInput:         sources.Sessions.InputStatus(),
		Input:                sources.Input.Status(),
		Readiness:            sources.Live.AppReadiness(),
		Device:               sources.Device.Status(),
		Output:               sources.Output.Status(),
		Audio:                sources.Audio.Status(),
		AttentionRecorder:    sources.Attention.RecorderStatus(),
		AttentionState:       sources.Attention.AttentionStateStatus(),
		PresentationCooldown: sources.Attention.PresentationCooldownStatus(),
		PluginLogs:           sources.Logs.Status(),
		Observations:         sources.Attention.ObservationDiagnostics(),
		Configuration:        sources.Configuration.PersistenceStatus(),
		Checkpoints:          sources.Checkpoints.Status(),
		Session:              sources.Sessions.Diagnostics(),
	}
}
