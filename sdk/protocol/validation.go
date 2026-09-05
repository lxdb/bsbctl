package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// DecodeStrict decodes exactly one unambiguous JSON value and rejects unknown
// fields.
func DecodeStrict(raw json.RawMessage, target any) error {
	return decodeStrict(raw, target)
}

func validateActionRequest(instance InstanceRef, action string, payload json.RawMessage) error {
	var errs []error
	if err := instance.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := validateIdentifier("action", action); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateJSONObject("action payload", payload, true); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func validateSessionRequest(instance InstanceRef, token string) error {
	var errs []error
	if err := instance.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := validateIdentifier("session token", token); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ValidateJSONObject requires one bounded JSON object, or permits an omitted
// value when optional is true.
func ValidateJSONObject(name string, raw json.RawMessage, optional bool) error {
	if len(raw) == 0 {
		if optional {
			return nil
		}
		return fmt.Errorf("%s is required", name)
	}
	if len(raw) > MaxJSONObjectBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, MaxJSONObjectBytes)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' || !json.Valid(trimmed) {
		return fmt.Errorf("%s must be one JSON object", name)
	}
	if err := rejectDuplicateNames(trimmed); err != nil {
		return fmt.Errorf("%s must be unambiguous: %w", name, err)
	}
	return nil
}

// ValidateEmptyParams permits omitted params or one empty JSON object.
func ValidateEmptyParams(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	if err := ValidateJSONObject("params", raw, false); err != nil {
		return err
	}
	var params struct{}
	return DecodeStrict(raw, &params)
}

func validateIdentifier(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if len(value) > 128 {
		return fmt.Errorf("%s exceeds 128 bytes", name)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", name)
	}
	for _, value := range value {
		if unicode.IsControl(value) {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}

func validateRequiredTimestamp(name string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("%s is required", name)
	}
	return validateOptionalTimestamp(name, value)
}

func validateOptionalTimestamp(name string, value time.Time) error {
	if value.IsZero() {
		return nil
	}
	_, offset := value.Zone()
	if offset != 0 {
		return fmt.Errorf("%s must be UTC", name)
	}
	return nil
}

func validateLogText(value string, maxBytes int) error {
	if len(value) > maxBytes {
		return fmt.Errorf("exceeds %d bytes", maxBytes)
	}
	if !utf8.ValidString(value) {
		return errors.New("must be valid UTF-8")
	}
	for _, value := range value {
		if unicode.IsControl(value) || value == '\u2028' || value == '\u2029' {
			return errors.New("must not contain control characters")
		}
	}
	return nil
}

func countTrue(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}
