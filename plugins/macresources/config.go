package macresources

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	protocoljson "github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	PluginID        = "dev.bsbctl.mac-resources"
	PluginVersion   = "0.1.0"
	AppID           = "mac-resources"
	ChannelSummary  = "summary"
	ChannelPressure = "pressure"
	ChannelLive     = "live"
	observationKey  = "current"
)

type Config struct {
	SampleInterval                time.Duration
	SummaryInterval               time.Duration
	WarningPercent                float64
	CriticalPercent               float64
	SustainSamples                int
	RecoveryMarginPercent         float64
	NetworkCapacityBytesPerSecond float64
}

type configJSON struct {
	SampleIntervalSeconds         *int     `json:"sample_interval_seconds,omitempty"`
	SummaryIntervalSeconds        *int     `json:"summary_interval_seconds,omitempty"`
	WarningPercent                *float64 `json:"warning_percent,omitempty"`
	CriticalPercent               *float64 `json:"critical_percent,omitempty"`
	SustainSamples                *int     `json:"sustain_samples,omitempty"`
	RecoveryMarginPercent         *float64 `json:"recovery_margin_percent,omitempty"`
	NetworkCapacityBytesPerSecond *float64 `json:"network_capacity_bytes_per_second,omitempty"`
}

func decodeConfig(raw json.RawMessage) (Config, error) {
	value := configJSON{}
	if err := protocoljson.DecodeStrict(raw, &value); err != nil {
		return Config{}, fmt.Errorf("decode resources configuration: %w", err)
	}
	result := Config{
		SampleInterval:                2 * time.Second,
		SummaryInterval:               3 * time.Minute,
		WarningPercent:                70,
		CriticalPercent:               90,
		SustainSamples:                3,
		RecoveryMarginPercent:         5,
		NetworkCapacityBytesPerSecond: 10 * 1024 * 1024,
	}
	if value.SampleIntervalSeconds != nil {
		if *value.SampleIntervalSeconds < 1 || *value.SampleIntervalSeconds > 60 {
			return Config{}, errors.New("sample_interval_seconds must be between 1 and 60")
		}
		result.SampleInterval = time.Duration(*value.SampleIntervalSeconds) * time.Second
	}
	if value.SummaryIntervalSeconds != nil {
		if *value.SummaryIntervalSeconds < 120 || *value.SummaryIntervalSeconds > 300 {
			return Config{}, errors.New("summary_interval_seconds must be between 120 and 300")
		}
		result.SummaryInterval = time.Duration(*value.SummaryIntervalSeconds) * time.Second
	}
	if value.WarningPercent != nil {
		result.WarningPercent = *value.WarningPercent
	}
	if value.CriticalPercent != nil {
		result.CriticalPercent = *value.CriticalPercent
	}
	if value.SustainSamples != nil {
		result.SustainSamples = *value.SustainSamples
	}
	if value.RecoveryMarginPercent != nil {
		result.RecoveryMarginPercent = *value.RecoveryMarginPercent
	}
	if value.NetworkCapacityBytesPerSecond != nil {
		result.NetworkCapacityBytesPerSecond = *value.NetworkCapacityBytesPerSecond
	}
	if err := result.validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

func (c Config) validate() error {
	var errs []error
	if c.SampleInterval < time.Second || c.SampleInterval > time.Minute {
		errs = append(errs, errors.New("sample_interval_seconds must be between 1 and 60"))
	}
	if c.SummaryInterval < 2*time.Minute || c.SummaryInterval > 5*time.Minute {
		errs = append(errs, errors.New("summary_interval_seconds must be between 120 and 300"))
	}
	if c.WarningPercent <= 0 || c.WarningPercent >= 100 {
		errs = append(errs, errors.New("warning_percent must be greater than 0 and less than 100"))
	}
	if c.CriticalPercent <= c.WarningPercent || c.CriticalPercent > 100 {
		errs = append(errs, errors.New("critical_percent must be greater than warning_percent and at most 100"))
	}
	if c.SustainSamples < 1 || c.SustainSamples > 30 {
		errs = append(errs, errors.New("sustain_samples must be between 1 and 30"))
	}
	if c.RecoveryMarginPercent < 0 || c.RecoveryMarginPercent > 20 || c.RecoveryMarginPercent >= c.WarningPercent {
		errs = append(errs, errors.New("recovery_margin_percent must be between 0 and 20 and below warning_percent"))
	}
	if c.NetworkCapacityBytesPerSecond < 1024 || c.NetworkCapacityBytesPerSecond > 1024*1024*1024*1024 {
		errs = append(errs, errors.New("network_capacity_bytes_per_second must be between 1 KiB/s and 1 TiB/s"))
	}
	return errors.Join(errs...)
}
