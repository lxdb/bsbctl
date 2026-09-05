package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ExecutionMode controls whether the daemon keeps a plugin resident or starts it for a session.
type ExecutionMode string

const (
	ExecutionModeResident    ExecutionMode = "resident"
	ExecutionModeInteractive ExecutionMode = "interactive"
)

// InstanceRef identifies one exact generation of an app instance.
type InstanceRef struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
}

// Validate checks the instance identity and nonzero generation.
func (ref InstanceRef) Validate() error {
	if err := validateIdentifier("instance id", ref.ID); err != nil {
		return err
	}
	if ref.Generation == 0 {
		return errors.New("instance generation must be greater than zero")
	}
	return nil
}

// Instance contains the complete plugin-owned state for one app generation.
type Instance struct {
	ID         string            `json:"id"`
	Generation uint64            `json:"generation"`
	Config     json.RawMessage   `json:"config"`
	Secrets    map[string]string `json:"secrets,omitempty"`
	Checkpoint json.RawMessage   `json:"checkpoint,omitempty"`
}

// Ref returns the generation-scoped reference for the instance.
func (instance Instance) Ref() InstanceRef {
	return InstanceRef{ID: instance.ID, Generation: instance.Generation}
}

// Validate checks instance identity and bounded plugin-owned JSON state.
func (instance Instance) Validate() error {
	var errs []error
	if err := instance.Ref().Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateJSONObject("config", instance.Config, false); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateJSONObject("checkpoint", instance.Checkpoint, true); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Channel declares one observation channel exported by a plugin.
type Channel struct {
	ID string `json:"id"`
}

// OperationKind distinguishes read-only queries from state-changing commands.
type OperationKind string

const (
	OperationQuery   OperationKind = "query"
	OperationCommand OperationKind = "command"
)

// OperationDescriptor declares one bounded operation supported by a plugin.
type OperationDescriptor struct {
	ID   string        `json:"id"`
	Kind OperationKind `json:"kind"`
}

// Validate checks the operation identifier and kind.
func (descriptor OperationDescriptor) Validate() error {
	if err := validateIdentifier("operation id", descriptor.ID); err != nil {
		return err
	}
	if descriptor.Kind != OperationQuery && descriptor.Kind != OperationCommand {
		return fmt.Errorf("operation %q has unsupported kind %q", descriptor.ID, descriptor.Kind)
	}
	return nil
}

// InitializeRequest carries the daemon and exact protocol identity.
type InitializeRequest struct {
	CoreVersion     string `json:"core_version"`
	PluginID        string `json:"plugin_id"`
	PluginVersion   string `json:"plugin_version"`
	ProtocolVersion string `json:"protocol_version"`
}

// Validate requires the exact protocol version and complete peer identity.
func (request InitializeRequest) Validate() error {
	if request.ProtocolVersion != Version {
		return fmt.Errorf("protocol version %q is unsupported", request.ProtocolVersion)
	}
	if err := validateIdentifier("plugin id", request.PluginID); err != nil {
		return err
	}
	if strings.TrimSpace(request.CoreVersion) == "" || strings.TrimSpace(request.PluginVersion) == "" {
		return errors.New("core_version and plugin_version are required")
	}
	return nil
}

// InitializeResult declares the immutable plugin contract for this process.
type InitializeResult struct {
	PluginID        string                `json:"plugin_id"`
	PluginVersion   string                `json:"plugin_version"`
	ProtocolVersion string                `json:"protocol_version"`
	ExecutionModes  []ExecutionMode       `json:"execution_modes"`
	Channels        []Channel             `json:"channels"`
	Operations      []OperationDescriptor `json:"operations,omitempty"`
}

// Validate checks the immutable plugin identity and declared capabilities.
func (result InitializeResult) Validate() error {
	var errs []error
	if err := validateIdentifier("plugin id", result.PluginID); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSpace(result.PluginVersion) == "" {
		errs = append(errs, errors.New("plugin_version is required"))
	}
	if result.ProtocolVersion != Version {
		errs = append(errs, fmt.Errorf("protocol version %q is unsupported", result.ProtocolVersion))
	}
	if err := ValidateExecutionModes(result.ExecutionModes); err != nil {
		errs = append(errs, err)
	}
	seenChannels := make(map[string]struct{}, len(result.Channels))
	for _, channel := range result.Channels {
		if err := validateIdentifier("channel id", channel.ID); err != nil {
			errs = append(errs, err)
		}
		if _, exists := seenChannels[channel.ID]; exists {
			errs = append(errs, fmt.Errorf("channel %q is duplicated", channel.ID))
		}
		seenChannels[channel.ID] = struct{}{}
	}
	seenOperations := make(map[string]struct{}, len(result.Operations))
	for _, operation := range result.Operations {
		if err := operation.Validate(); err != nil {
			errs = append(errs, err)
		}
		if _, exists := seenOperations[operation.ID]; exists {
			errs = append(errs, fmt.Errorf("operation %q is duplicated", operation.ID))
		}
		seenOperations[operation.ID] = struct{}{}
	}
	return errors.Join(errs...)
}

// ValidateExecutionModes rejects empty, duplicate, or unsupported execution modes.
func ValidateExecutionModes(modes []ExecutionMode) error {
	if len(modes) == 0 {
		return errors.New("at least one execution mode is required")
	}
	seen := make(map[ExecutionMode]struct{}, len(modes))
	var errs []error
	for _, mode := range modes {
		switch mode {
		case ExecutionModeResident, ExecutionModeInteractive:
		default:
			errs = append(errs, fmt.Errorf("unsupported execution mode %q", mode))
		}
		if _, exists := seen[mode]; exists {
			errs = append(errs, fmt.Errorf("execution mode %q is duplicated", mode))
		}
		seen[mode] = struct{}{}
	}
	return errors.Join(errs...)
}

// ReplaceInstancesRequest supplies the complete desired instance set atomically.
type ReplaceInstancesRequest struct {
	Instances []Instance `json:"instances"`
}

// Validate checks every desired instance and rejects duplicate IDs.
func (request ReplaceInstancesRequest) Validate() error {
	var errs []error
	seen := make(map[string]struct{}, len(request.Instances))
	for index, instance := range request.Instances {
		if err := instance.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("instance %d: %w", index, err))
		}
		if _, exists := seen[instance.ID]; exists {
			errs = append(errs, fmt.Errorf("instance id %q is duplicated", instance.ID))
		}
		seen[instance.ID] = struct{}{}
	}
	return errors.Join(errs...)
}
