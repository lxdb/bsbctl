package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SessionInputRequest delivers one acknowledged physical input event to a bound session.
type SessionInputRequest struct {
	Sequence     uint64       `json:"sequence"`
	OccurredAt   time.Time    `json:"occurred_at"`
	Instance     InstanceRef  `json:"instance"`
	SessionToken string       `json:"session_token"`
	Input        SessionInput `json:"input"`
}

// SessionInputDisposition reports whether a plugin handled one routed input.
type SessionInputDisposition string

const (
	SessionInputConsumed    SessionInputDisposition = "consumed"
	SessionInputNotConsumed SessionInputDisposition = "not_consumed"
)

// SessionInputResult is the strict response to plugin.session.input.
type SessionInputResult struct {
	Disposition SessionInputDisposition `json:"disposition"`
}

// Validate accepts exactly the two input dispositions owned by protocol v1.
func (result SessionInputResult) Validate() error {
	switch result.Disposition {
	case SessionInputConsumed, SessionInputNotConsumed:
		return nil
	default:
		return fmt.Errorf("unsupported session input disposition %q", result.Disposition)
	}
}

// SessionInput contains exactly one button or encoder event.
type SessionInput struct {
	Button  *ButtonInput  `json:"button,omitempty"`
	Encoder *EncoderInput `json:"encoder,omitempty"`
}

// Button identifies a supported BUSY Bar button.
type Button string

const (
	ButtonOK    Button = "ok"
	ButtonBack  Button = "back"
	ButtonStart Button = "start"
)

// ButtonAction distinguishes a physical press from a release.
type ButtonAction string

const (
	ButtonPress   ButtonAction = "press"
	ButtonRelease ButtonAction = "release"
)

// ButtonInput describes one button transition.
type ButtonInput struct {
	Button Button       `json:"button"`
	Action ButtonAction `json:"action"`
}

// EncoderInput carries one nonzero encoder delta.
type EncoderInput struct {
	Delta int32 `json:"delta"`
}

// Validate checks sequence, UTC time, session identity, and input payload.
func (request SessionInputRequest) Validate() error {
	var errs []error
	if request.Sequence == 0 {
		errs = append(errs, errors.New("session input sequence must be greater than zero"))
	}
	if err := validateRequiredTimestamp("occurred_at", request.OccurredAt); err != nil {
		errs = append(errs, err)
	}
	if err := validateSessionRequest(request.Instance, request.SessionToken); err != nil {
		errs = append(errs, err)
	}
	if err := request.Input.Validate(); err != nil {
		errs = append(errs, err)
	}
	if wire, err := json.Marshal(request); err != nil {
		errs = append(errs, err)
	} else if len(wire) > MaxSessionInputBytes {
		errs = append(errs, errors.New("session input exceeds 16 KiB"))
	}
	return errors.Join(errs...)
}

// Validate requires exactly one supported button or encoder event.
func (input SessionInput) Validate() error {
	if (input.Button == nil) == (input.Encoder == nil) {
		return errors.New("session input must contain exactly one of button or encoder")
	}
	if input.Button != nil {
		switch input.Button.Button {
		case ButtonOK, ButtonBack, ButtonStart:
		default:
			return fmt.Errorf("unsupported button %q", input.Button.Button)
		}
		switch input.Button.Action {
		case ButtonPress, ButtonRelease:
		default:
			return fmt.Errorf("unsupported button action %q", input.Button.Action)
		}
	}
	if input.Encoder != nil && input.Encoder.Delta == 0 {
		return errors.New("encoder delta must not be zero")
	}
	return nil
}
