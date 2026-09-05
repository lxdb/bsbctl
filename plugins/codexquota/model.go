// Package codexquota publishes bounded Codex account quota observations.
package codexquota

import (
	"errors"
	"time"

	"github.com/lxdb/bsbctl/internal/codexusage"
)

const (
	PluginID        = "dev.bsbctl.codex-quota"
	PluginVersion   = "0.1.0"
	AppID           = "codex-quota"
	ChannelSummary  = "summary"
	ChannelPressure = "pressure"
	ChannelLive     = "live"
	observationKey  = "quota"
)

type Window = codexusage.Window
type Snapshot = codexusage.Snapshot

type usageResponse struct {
	RateLimit *rateLimitResponse `json:"rate_limit"`
}

type rateLimitResponse struct {
	Primary   *windowResponse `json:"primary_window"`
	Secondary *windowResponse `json:"secondary_window"`
}

type windowResponse struct {
	UsedPercent   int   `json:"used_percent"`
	ResetAt       int64 `json:"reset_at"`
	WindowSeconds int64 `json:"limit_window_seconds"`
}

type signalDisposition = codexusage.Signal

const (
	signalNone     = codexusage.SignalNone
	signalLow      = codexusage.SignalLow
	signalCritical = codexusage.SignalCritical
)

func normalizeUsage(response usageResponse, now time.Time) (Snapshot, error) {
	if response.RateLimit == nil {
		return Snapshot{}, errors.New("usage_windows_unavailable")
	}
	raw := make([]codexusage.RawWindow, 0, 2)
	for _, source := range []*windowResponse{response.RateLimit.Primary, response.RateLimit.Secondary} {
		if source == nil {
			continue
		}
		raw = append(raw, codexusage.RawWindow{
			UsedPercent: source.UsedPercent,
			Duration:    time.Duration(source.WindowSeconds) * time.Second,
			ResetsAt:    time.Unix(source.ResetAt, 0),
		})
	}
	return codexusage.NormalizeWindows(raw, now)
}

func quotaPresentationConfig(config Config) codexusage.PresentationConfig {
	return codexusage.PresentationConfig{
		Label: config.Label, Badge: config.Badge, ShowBadge: config.ShowBadge,
		WarningRemainingPercent:  config.WarningRemainingPercent,
		CriticalRemainingPercent: config.CriticalRemainingPercent,
	}
}

func quotaSignal(snapshot Snapshot, config Config) signalDisposition {
	return codexusage.SignalFor(snapshot, quotaPresentationConfig(config))
}
