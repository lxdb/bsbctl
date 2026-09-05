//go:build darwin && cgo

package macresources

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNativeCollectorReturnsBoundedMacCounters(t *testing.T) {
	collector := NewNativeCollector()
	sample, err := collector.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if sample.CPUTotal == 0 || sample.CPUIdle > sample.CPUTotal {
		t.Fatalf("CPU counters = total %d idle %d", sample.CPUTotal, sample.CPUIdle)
	}
	if sample.MemoryPercent < 0 || sample.MemoryPercent > 100 {
		t.Fatalf("memory percent = %f", sample.MemoryPercent)
	}
	if sample.CollectedAt.IsZero() {
		t.Fatal("sample time is zero")
	}
}

func TestNativeCounterOwnershipAndNetworkABI(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "native-counters")
	command := exec.CommandContext(t.Context(), "cc", "-Wall", "-Wextra", "-Werror", "testdata/native-counters.c", "-o", executable)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile native counter regression: %v\n%s", err, output)
	}
	if output, err := exec.CommandContext(t.Context(), executable).CombinedOutput(); err != nil {
		t.Fatalf("native counter contract: %v\n%s", err, output)
	} else {
		t.Logf("%s", output)
	}
}

func TestNativeCollectorHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewNativeCollector().Sample(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sample error = %v, want context.Canceled", err)
	}
}

func BenchmarkNativeCollector(b *testing.B) {
	collector := NewNativeCollector()
	ctx := context.Background()
	for b.Loop() {
		if _, err := collector.Sample(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
