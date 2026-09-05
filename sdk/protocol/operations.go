package protocol

import (
	"encoding/json"
	"errors"
)

// OperationRequest invokes a declared query or command for one instance generation.
type OperationRequest struct {
	Instance  InstanceRef     `json:"instance"`
	Operation string          `json:"operation"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Validate checks the target, operation identifier, and bounded payload.
func (request OperationRequest) Validate() error {
	var errs []error
	if err := request.Instance.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := validateIdentifier("operation", request.Operation); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateJSONObject("operation payload", request.Payload, true); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// OperationResult carries one required bounded JSON object.
type OperationResult struct {
	Payload json.RawMessage `json:"payload"`
}

// Validate requires one bounded JSON object.
func (result OperationResult) Validate() error {
	return ValidateJSONObject("operation result", result.Payload, false)
}

// CheckpointRequest replaces the generation-scoped durable plugin checkpoint.
type CheckpointRequest struct {
	Instance InstanceRef     `json:"instance"`
	Data     json.RawMessage `json:"data"`
}

// Validate checks the target and bounded checkpoint object.
func (request CheckpointRequest) Validate() error {
	var errs []error
	if err := request.Instance.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateJSONObject("checkpoint data", request.Data, false); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
