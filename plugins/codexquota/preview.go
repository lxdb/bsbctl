//go:build preview

package codexquota

import (
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

// PreviewScenes returns deterministic normal and critical presentations built
// exclusively from public-safe mock data. It does not read Codex credentials
// or call an API.
func PreviewScenes(now time.Time) []protocol.Scene {
	normalFiveHour := Window{
		UsedPercent:      42,
		RemainingPercent: 58,
		Duration:         5 * time.Hour,
		ResetsAt:         now.Add(3 * time.Hour),
	}
	criticalFiveHour := Window{
		UsedPercent:      95,
		RemainingPercent: 5,
		Duration:         5 * time.Hour,
		ResetsAt:         now.Add(30 * time.Minute),
	}
	weekly := Window{
		UsedPercent:      28,
		RemainingPercent: 72,
		Duration:         7 * 24 * time.Hour,
		ResetsAt:         now.Add(6 * 24 * time.Hour),
	}
	config := Config{
		Label:                    "MOCK",
		Badge:                    "M",
		WarningRemainingPercent:  20,
		CriticalRemainingPercent: 5,
	}
	return []protocol.Scene{
		quotaScene(Snapshot{Windows: []Window{normalFiveHour, weekly}, UpdatedAt: now}, weekly, config, signalNone),
		quotaScene(Snapshot{Windows: []Window{criticalFiveHour, weekly}, UpdatedAt: now}, criticalFiveHour, config, signalCritical),
	}
}
