package protocol

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
)

// PublishRequest submits one validated observation to core.
type PublishRequest struct {
	Observation Observation `json:"observation"`
}

// WithdrawRequest removes one exact instance, channel, and key observation.
type WithdrawRequest struct {
	Instance InstanceRef `json:"instance"`
	Channel  string      `json:"channel"`
	Key      string      `json:"key"`
}

// Validate checks the exact observation identity to withdraw.
func (request WithdrawRequest) Validate() error {
	var errs []error
	if err := request.Instance.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := validateIdentifier("withdraw channel", request.Channel); err != nil {
		errs = append(errs, err)
	}
	if err := validateIdentifier("withdraw key", request.Key); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// HealthResult reports plugin health at one required UTC observation time.
type HealthResult struct {
	Healthy    bool      `json:"healthy"`
	ObservedAt time.Time `json:"observed_at"`
}

// Validate requires a nonzero UTC observation time.
func (result HealthResult) Validate() error {
	return validateRequiredTimestamp("health observed_at", result.ObservedAt)
}

// MetricNotification emits one finite, bounded diagnostic metric.
type MetricNotification struct {
	Instance InstanceRef `json:"instance,omitzero"`
	Name     string      `json:"name"`
	Value    float64     `json:"value"`
	Unit     string      `json:"unit,omitempty"`
}

// UnmarshalJSON strictly decodes a metric and requires an explicit value.
func (notification *MetricNotification) UnmarshalJSON(data []byte) error {
	var wire struct {
		Instance InstanceRef `json:"instance,omitzero"`
		Name     string      `json:"name"`
		Value    *float64    `json:"value"`
		Unit     string      `json:"unit,omitempty"`
	}
	if err := DecodeStrict(data, &wire); err != nil {
		return err
	}
	if wire.Value == nil {
		return errors.New("metric value is required")
	}
	*notification = MetricNotification{
		Instance: wire.Instance,
		Name:     wire.Name,
		Value:    *wire.Value,
		Unit:     wire.Unit,
	}
	return nil
}

// Validate checks metric identity, optional instance, unit, and finite value.
func (notification MetricNotification) Validate() error {
	var errs []error
	if notification.Instance != (InstanceRef{}) {
		if err := notification.Instance.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := validateIdentifier("metric name", notification.Name); err != nil {
		errs = append(errs, err)
	}
	if math.IsNaN(notification.Value) || math.IsInf(notification.Value, 0) {
		errs = append(errs, errors.New("metric value must be finite"))
	}
	if notification.Unit != "" {
		if err := validateIdentifier("metric unit", notification.Unit); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// LogLevel is a supported structured plugin-log severity.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

var logIdentifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// LogNotification emits one bounded structured and redacted plugin event.
type LogNotification struct {
	Level    LogLevel          `json:"level"`
	Event    string            `json:"event"`
	Instance InstanceRef       `json:"instance,omitzero"`
	Message  string            `json:"message,omitempty"`
	Fields   map[string]string `json:"fields,omitempty"`
}

// Validate checks the level, event identity, optional instance, and bounded fields.
func (notification LogNotification) Validate() error {
	switch notification.Level {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
	default:
		return fmt.Errorf("unsupported log level %q", notification.Level)
	}
	if !logIdentifierPattern.MatchString(notification.Event) {
		return errors.New("log event must be a safe lowercase identifier of at most 64 bytes")
	}
	if notification.Instance != (InstanceRef{}) {
		if err := notification.Instance.Validate(); err != nil {
			return err
		}
	}
	if err := validateLogText(notification.Message, 1024); err != nil {
		return fmt.Errorf("log message: %w", err)
	}
	if len(notification.Fields) > 16 {
		return errors.New("log fields exceed 16 entries")
	}
	for key, value := range notification.Fields {
		if !logIdentifierPattern.MatchString(key) {
			return fmt.Errorf("log field key %q must be a safe lowercase identifier of at most 64 bytes", key)
		}
		if err := validateLogText(value, 256); err != nil {
			return fmt.Errorf("log field %q: %w", key, err)
		}
	}
	return nil
}
