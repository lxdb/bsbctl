package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

// SessionTriggerKind identifies why core opened an interactive session.
type SessionTriggerKind string

const (
	SessionTriggerLauncher    SessionTriggerKind = "launcher"
	SessionTriggerObservation SessionTriggerKind = "observation"
)

// ObservationRef binds a session to one exact observation revision.
type ObservationRef struct {
	Channel  string `json:"channel"`
	Key      string `json:"key"`
	Revision uint64 `json:"revision"`
}

// SessionTrigger describes the launcher or observation that opened a session.
type SessionTrigger struct {
	Kind        SessionTriggerKind `json:"kind"`
	Observation *ObservationRef    `json:"observation,omitempty"`
}

// SessionStartRequest opens a generation-scoped interactive session.
type SessionStartRequest struct {
	Instance     InstanceRef     `json:"instance"`
	Action       string          `json:"action"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	SessionToken string          `json:"session_token"`
	Trigger      *SessionTrigger `json:"trigger,omitempty"`
}

// Validate checks the session identity, action, payload, and optional trigger.
func (request SessionStartRequest) Validate() error {
	var errs []error
	if err := validateActionRequest(request.Instance, request.Action, request.Payload); err != nil {
		errs = append(errs, err)
	}
	if err := validateIdentifier("session token", request.SessionToken); err != nil {
		errs = append(errs, err)
	}
	if request.Trigger != nil {
		switch request.Trigger.Kind {
		case SessionTriggerLauncher:
			if request.Trigger.Observation != nil {
				errs = append(errs, errors.New("launcher trigger must not contain an observation"))
			}
		case SessionTriggerObservation:
			if request.Trigger.Observation == nil {
				errs = append(errs, errors.New("observation trigger requires an observation"))
			} else {
				if err := validateIdentifier("observation channel", request.Trigger.Observation.Channel); err != nil {
					errs = append(errs, err)
				}
				if err := validateIdentifier("observation key", request.Trigger.Observation.Key); err != nil {
					errs = append(errs, err)
				}
				if request.Trigger.Observation.Revision == 0 {
					errs = append(errs, errors.New("observation revision must be greater than zero"))
				}
			}
		default:
			errs = append(errs, fmt.Errorf("unsupported session trigger %q", request.Trigger.Kind))
		}
	}
	return errors.Join(errs...)
}

// SessionEndRequest closes one exact core-owned session.
type SessionEndRequest struct {
	Instance     InstanceRef `json:"instance"`
	SessionToken string      `json:"session_token"`
}

// Validate checks the exact instance and session token.
func (request SessionEndRequest) Validate() error {
	return validateSessionRequest(request.Instance, request.SessionToken)
}

// CompleteSessionRequest asks core to finish one exact session.
type CompleteSessionRequest struct {
	Instance     InstanceRef `json:"instance"`
	SessionToken string      `json:"session_token"`
}

// Validate checks the exact instance and session token.
func (request CompleteSessionRequest) Validate() error {
	return validateSessionRequest(request.Instance, request.SessionToken)
}

// SessionExecutionRequest asks core for the final permission to perform one
// irreversible action for an exact foreground session.
type SessionExecutionRequest struct {
	Instance     InstanceRef `json:"instance"`
	SessionToken string      `json:"session_token"`
}

// Validate checks the exact instance and session token.
func (request SessionExecutionRequest) Validate() error {
	return validateSessionRequest(request.Instance, request.SessionToken)
}
