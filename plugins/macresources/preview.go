//go:build preview

package macresources

import "github.com/lxdb/bsbctl/sdk/protocol"

// PreviewScenes returns deterministic presentations built exclusively from
// public-safe mock data. It does not sample the host operating system.
func PreviewScenes() []protocol.Scene {
	config := Config{
		WarningPercent:                70,
		CriticalPercent:               90,
		NetworkCapacityBytesPerSecond: 10 * 1024 * 1024,
	}
	readings := []reading{
		{CPUPercent: 24, MemoryPercent: 51, RXBytesPerSecond: 720 * 1024, TXBytesPerSecond: 180 * 1024},
		{CPUPercent: 47, MemoryPercent: 58, RXBytesPerSecond: 2.4 * 1024 * 1024, TXBytesPerSecond: 640 * 1024},
		{CPUPercent: 68, MemoryPercent: 63, RXBytesPerSecond: 4.1 * 1024 * 1024, TXBytesPerSecond: 1.2 * 1024 * 1024},
	}
	scenes := make([]protocol.Scene, 0, len(readings))
	for _, value := range readings {
		scenes = append(scenes, summaryScene(value, config))
	}
	return scenes
}
