package macresources

import (
	"testing"
	"time"
)

func TestDeriveReadingCalculatesCPUAndSeparateNetworkRates(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	got, ok := deriveReading(
		RawSample{CPUTotal: 100, CPUIdle: 50, RXBytes: 1000, TXBytes: 2000, CollectedAt: start},
		RawSample{CPUTotal: 200, CPUIdle: 70, MemoryPercent: 63, RXBytes: 3048, TXBytes: 6096, CollectedAt: start.Add(2 * time.Second)},
		reading{},
	)
	if !ok {
		t.Fatal("deriveReading rejected an increasing sample")
	}
	if got.CPUPercent != 80 || got.MemoryPercent != 63 || got.RXBytesPerSecond != 1024 || got.TXBytesPerSecond != 2048 {
		t.Fatalf("reading = %#v", got)
	}
}

func TestDeriveReadingRebaselinesResetCountersWithoutZeroingLastValues(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	last := reading{CPUPercent: 44, MemoryPercent: 50, RXBytesPerSecond: 123, TXBytesPerSecond: 456}
	got, ok := deriveReading(
		RawSample{CPUTotal: 1000, CPUIdle: 500, RXBytes: 9000, TXBytes: 8000, CollectedAt: start},
		RawSample{CPUTotal: 10, CPUIdle: 5, MemoryPercent: 75, RXBytes: 9, TXBytes: 8, CollectedAt: start.Add(2 * time.Second)},
		last,
	)
	if !ok {
		t.Fatal("deriveReading rejected a timed reset sample")
	}
	if got.CPUPercent != 44 || got.MemoryPercent != 75 || got.RXBytesPerSecond != 123 || got.TXBytesPerSecond != 456 {
		t.Fatalf("reading after reset = %#v", got)
	}
}

func TestDeriveReadingRebaselinesWhenActiveInterfaceSetChanges(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	last := reading{RXBytesPerSecond: 123, TXBytesPerSecond: 456}
	got, ok := deriveReading(
		RawSample{CPUTotal: 100, CPUIdle: 50, RXBytes: 1000, TXBytes: 2000, NetworkSet: 11, CollectedAt: start},
		RawSample{CPUTotal: 200, CPUIdle: 100, RXBytes: 900000, TXBytes: 800000, NetworkSet: 22, CollectedAt: start.Add(2 * time.Second)},
		last,
	)
	if !ok {
		t.Fatal("deriveReading rejected an interface-change sample")
	}
	if got.RXBytesPerSecond != 123 || got.TXBytesPerSecond != 456 {
		t.Fatalf("network rate after interface change = %.0f/%.0f", got.RXBytesPerSecond, got.TXBytesPerSecond)
	}
}
