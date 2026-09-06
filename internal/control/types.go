package control

import (
	"encoding/json"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/checkpoint"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/eventbus"
	busyinput "github.com/lxdb/bsbctl/internal/input"
	"github.com/lxdb/bsbctl/internal/installer"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/pluginlog"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type AttentionExplainRequest struct {
	ObservationID string `json:"observation_id"`
}
type AttentionAcknowledgeRequest struct {
	ObservationID string `json:"observation_id"`
}
type AttentionHistoryRequest struct {
	Limit int       `json:"limit,omitempty"`
	Since time.Time `json:"since,omitempty"`
}

// AttentionHistoryResult contains the newest contiguous requested traces that
// fit the control response. Truncated reports omissions caused by byte limits,
// not history older than the caller's requested count or time window.
type AttentionHistoryResult struct {
	Traces    []attention.Trace `json:"traces"`
	Truncated bool              `json:"truncated"`
}

type AppStatus struct {
	AppID             string `json:"app_id"`
	PluginID          string `json:"plugin_id"`
	RuntimeGeneration uint64 `json:"runtime_generation"`
	Enabled           bool   `json:"enabled"`
}

type Status struct {
	Version              string                                 `json:"version"`
	Generation           uint64                                 `json:"generation"`
	Apps                 []AppStatus                            `json:"apps"`
	Plugins              []pluginhost.PluginStatus              `json:"plugins"`
	Assets               []assets.State                         `json:"assets,omitempty"`
	SessionInput         []eventbus.Status                      `json:"session_input,omitempty"`
	Input                busyinput.DispatcherStatus             `json:"input"`
	Readiness            []daemon.AppReadiness                  `json:"app_readiness,omitempty"`
	Device               device.RuntimeStatus                   `json:"device"`
	AttentionRecorder    attention.RecorderStatus               `json:"attention_recorder"`
	AttentionState       daemon.AttentionStateDiagnostics       `json:"attention_state"`
	PresentationCooldown daemon.PresentationCooldownDiagnostics `json:"presentation_cooldown"`
	PluginLogs           pluginlog.Status                       `json:"plugin_logs"`
	Output               device.OutputStatus                    `json:"output"`
	Audio                device.AudioStatus                     `json:"audio"`
	Observations         observation.StoreDiagnostics           `json:"observations"`
	Configuration        daemon.ConfigPersistenceStatus         `json:"configuration"`
	Checkpoints          checkpoint.Status                      `json:"checkpoints"`
	Session              daemon.SessionDiagnostics              `json:"session"`
}

type SetEnabledRequest struct {
	AppID   string `json:"app_id"`
	Enabled bool   `json:"enabled"`
}

type MutationStatus string

const (
	MutationUpdated             MutationStatus = "updated"
	MutationCreated             MutationStatus = "created"
	MutationDeleted             MutationStatus = "deleted"
	MutationUnchanged           MutationStatus = "unchanged"
	MutationDurabilityUncertain MutationStatus = "durability_uncertain"
	MutationPartial             MutationStatus = "partial"
)

type CreateAppRequest struct {
	AppID        string                               `json:"app_id"`
	PluginID     string                               `json:"plugin_id"`
	Enabled      bool                                 `json:"enabled"`
	Config       json.RawMessage                      `json:"config"`
	Secrets      map[string]string                    `json:"secrets,omitempty"`
	Policies     map[string]presentation.PolicyConfig `json:"policies"`
	LaunchAction string                               `json:"launch_action,omitempty"`
}

type DeleteAppRequest struct {
	AppID string `json:"app_id"`
}

type AppInstanceResult struct {
	Status     MutationStatus `json:"status"`
	AppID      string         `json:"app_id"`
	PluginID   string         `json:"plugin_id"`
	Enabled    bool           `json:"enabled"`
	Generation uint64         `json:"generation"`
}

type AppMutationResult struct {
	Status     MutationStatus `json:"status"`
	AppID      string         `json:"app_id"`
	Enabled    bool           `json:"enabled"`
	Generation uint64         `json:"generation"`
}

type ReplaceConfigRequest struct {
	AppID string `json:"app_id"`
	// ExpectedGeneration is an optional precondition on the complete daemon document.
	ExpectedGeneration uint64                               `json:"expected_generation,omitzero"`
	Config             json.RawMessage                      `json:"config"`
	Secrets            map[string]string                    `json:"secrets,omitempty"`
	Policies           map[string]presentation.PolicyConfig `json:"policies"`
	LaunchAction       string                               `json:"launch_action,omitempty"`
}

type AppConfigResult struct {
	Status     MutationStatus `json:"status"`
	AppID      string         `json:"app_id"`
	Generation uint64         `json:"generation"`
}

type CatalogInstallRequest struct {
	CatalogPath     string `json:"catalog_path"`
	SignaturePath   string `json:"signature_path"`
	CatalogSHA256   string `json:"catalog_sha256"`
	SignatureSHA256 string `json:"signature_sha256"`
	PluginID        string `json:"plugin_id"`
	Version         string `json:"version"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
}

type CatalogRollbackRequest struct {
	PluginID string `json:"plugin_id"`
	Version  string `json:"version,omitempty"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

type CatalogStatusRequest struct {
	PluginID string `json:"plugin_id,omitempty"`
}

type CatalogOperationResponse struct {
	Result    installer.Result `json:"result"`
	ErrorCode installer.Code   `json:"error_code,omitempty"`
}

const CatalogDependencyFailed installer.Code = "dependency_failed"

type LaunchRequest struct {
	AppID   string          `json:"app_id"`
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type PluginOperationRequest struct {
	AppID     string                 `json:"app_id"`
	Operation string                 `json:"operation"`
	Kind      protocol.OperationKind `json:"kind"`
	Payload   json.RawMessage        `json:"payload,omitempty"`
}
