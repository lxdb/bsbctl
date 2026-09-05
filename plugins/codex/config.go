package codex

import (
	"encoding/json"
	"errors"
	"fmt"

	protocoljson "github.com/lxdb/bsbctl/sdk/protocol"
)

const maxConfigBytes = 64 * 1024

type Config struct {
	ShowSensitiveRequestDetails   bool
	ShowQuota                     bool
	QuotaWarningRemainingPercent  int
	QuotaCriticalRemainingPercent int
}

type configJSON struct {
	ShowSensitiveRequestDetails   *bool `json:"show_sensitive_request_details,omitempty"`
	ShowQuota                     *bool `json:"show_quota,omitempty"`
	QuotaWarningRemainingPercent  *int  `json:"quota_warning_remaining_percent,omitempty"`
	QuotaCriticalRemainingPercent *int  `json:"quota_critical_remaining_percent,omitempty"`
}

func decodeConfig(raw json.RawMessage) (Config, error) {
	if len(raw) == 0 || len(raw) > maxConfigBytes {
		return Config{}, fmt.Errorf("Codex configuration must contain between 1 and %d bytes", maxConfigBytes)
	}
	var value configJSON
	if err := protocoljson.DecodeStrict(raw, &value); err != nil {
		return Config{}, fmt.Errorf("decode Codex configuration: %w", err)
	}
	result := Config{
		ShowSensitiveRequestDetails:   true,
		QuotaWarningRemainingPercent:  20,
		QuotaCriticalRemainingPercent: 5,
	}
	if value.ShowSensitiveRequestDetails != nil {
		result.ShowSensitiveRequestDetails = *value.ShowSensitiveRequestDetails
	}
	if value.ShowQuota != nil {
		result.ShowQuota = *value.ShowQuota
	}
	if value.QuotaWarningRemainingPercent != nil {
		result.QuotaWarningRemainingPercent = *value.QuotaWarningRemainingPercent
	}
	if value.QuotaCriticalRemainingPercent != nil {
		result.QuotaCriticalRemainingPercent = *value.QuotaCriticalRemainingPercent
	}
	if result.QuotaCriticalRemainingPercent < 0 || result.QuotaWarningRemainingPercent > 100 ||
		result.QuotaCriticalRemainingPercent >= result.QuotaWarningRemainingPercent {
		return Config{}, errors.New("quota thresholds must satisfy 0 <= critical < warning <= 100")
	}
	return result, nil
}
