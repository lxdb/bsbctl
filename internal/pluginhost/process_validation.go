package pluginhost

import (
	"errors"
	"fmt"

	"github.com/lxdb/bsbctl/internal/identifier"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func validateSpec(spec Spec) error {
	if err := identifier.Validate("plugin id", spec.ID); err != nil {
		return err
	}
	if spec.Version == "" || spec.Executable == "" {
		return errors.New("plugin spec requires version and executable")
	}
	if err := protocol.ValidateExecutionModes(spec.ExecutionModes); err != nil {
		return fmt.Errorf("plugin spec execution modes: %w", err)
	}
	if spec.ProtocolVersion != protocol.Version {
		return errors.New("plugin spec protocol_version must be 1.0")
	}
	for _, channel := range spec.Channels {
		if err := identifier.Validate("channel id", channel.ID); err != nil {
			return err
		}
	}
	operations := make(map[string]struct{}, len(spec.Operations))
	for _, descriptor := range spec.Operations {
		if err := descriptor.Validate(); err != nil {
			return err
		}
		if _, exists := operations[descriptor.ID]; exists {
			return fmt.Errorf("operation %q is duplicated", descriptor.ID)
		}
		operations[descriptor.ID] = struct{}{}
	}
	if err := validateDesiredInstances(spec.Instances); err != nil {
		return err
	}
	return nil
}

func validateDesiredInstances(instances []Instance) error {
	var errs []error
	seen := make(map[string]struct{}, len(instances))
	for index, instance := range instances {
		if err := instance.Wire().Validate(); err != nil {
			errs = append(errs, fmt.Errorf("instance %d: %w", index, err))
		}
		if instance.Checkpoint != nil && instance.Checkpoint.Generation != instance.Generation {
			errs = append(errs, fmt.Errorf("instance %d checkpoint generation must match instance generation", index))
		}
		if _, exists := seen[instance.ID]; exists {
			errs = append(errs, fmt.Errorf("instance id %q is duplicated", instance.ID))
		}
		seen[instance.ID] = struct{}{}
	}
	return errors.Join(errs...)
}
