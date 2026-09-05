package protocol

import (
	"encoding/json"
	"fmt"
)

// ErrorKind is the stable safe classification carried by a bsbctl domain error.
type ErrorKind string

// Domain error codes and kinds are the only private errors accepted by protocol v1.
const (
	DomainErrorCode                          = -32000
	ErrorInvalidArgument           ErrorKind = "invalid_argument"
	ErrorNotReady                  ErrorKind = "not_ready"
	ErrorGenerationConflict        ErrorKind = "generation_conflict"
	ErrorSessionNotActive          ErrorKind = "session_not_active"
	ErrorSessionCanceled           ErrorKind = "session_canceled"
	ErrorSessionGenerationMismatch ErrorKind = "session_generation_mismatch"
)

// DecodeRemoteError validates the only server-domain error and the standard
// JSON-RPC errors accepted by protocol v1.
func DecodeRemoteError(code int, raw json.RawMessage) (ErrorKind, bool, error) {
	if code == DomainErrorCode {
		var data ErrorData
		if err := DecodeStrict(raw, &data); err != nil {
			return "", false, fmt.Errorf("invalid domain error data: %w", err)
		}
		if err := data.Validate(); err != nil {
			return "", false, err
		}
		return data.Kind, true, nil
	}
	switch code {
	case -32700, -32600, -32601, -32602, -32603:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("unsupported remote error code %d", code)
	}
}

// ErrorData is the complete safe wire payload for a domain error.
type ErrorData struct {
	Kind ErrorKind `json:"kind"`
}

// Validate accepts only the stable protocol v1 domain kinds.
func (data ErrorData) Validate() error {
	switch data.Kind {
	case ErrorInvalidArgument, ErrorNotReady, ErrorGenerationConflict,
		ErrorSessionNotActive, ErrorSessionCanceled, ErrorSessionGenerationMismatch:
		return nil
	default:
		return fmt.Errorf("unknown error kind %q", data.Kind)
	}
}

// DomainError preserves a private local cause while exposing only a stable kind.
type DomainError struct {
	kind  ErrorKind
	cause error
}

// NewDomainError constructs a typed domain error with an optional private cause.
func NewDomainError(kind ErrorKind, cause error) *DomainError {
	return &DomainError{kind: kind, cause: cause}
}

func (err *DomainError) Error() string {
	if err == nil {
		return ""
	}
	return string(err.kind)
}

func (err *DomainError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

// Kind returns the safe public classification.
func (err *DomainError) Kind() ErrorKind {
	if err == nil {
		return ""
	}
	return err.kind
}

// Data returns the complete safe wire representation.
func (err *DomainError) Data() ErrorData {
	return ErrorData{Kind: err.Kind()}
}
