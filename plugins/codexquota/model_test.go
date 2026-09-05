package codexquota

import (
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/codexusage"
)

func TestNormalizeUsageClampsAndSortsKnownWindows(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_900_000_000, 0).UTC()
	response := usageResponse{RateLimit: &rateLimitResponse{
		Primary:   &windowResponse{UsedPercent: 130, ResetAt: now.Add(30 * 24 * time.Hour).Unix(), WindowSeconds: 30 * 24 * 60 * 60},
		Secondary: &windowResponse{UsedPercent: -2, ResetAt: now.Add(5 * time.Hour).Unix(), WindowSeconds: 5 * 60 * 60},
	}}
	snapshot, err := normalizeUsage(response, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Windows[0]; got.Duration != 5*time.Hour || got.UsedPercent != 0 || got.RemainingPercent != 100 || codexusage.FrontWindowLabel(got.Duration) != "5H" {
		t.Fatalf("first window = %#v", got)
	}
	if got := snapshot.Windows[1]; got.Duration != 30*24*time.Hour || got.UsedPercent != 100 || got.RemainingPercent != 0 || codexusage.FrontWindowLabel(got.Duration) != "1M" {
		t.Fatalf("second window = %#v", got)
	}
	if codexusage.BackWindowLabel(7*24*time.Hour) != "WEEKLY" || codexusage.FrontWindowLabel(7*24*time.Hour) != "1W" {
		t.Fatalf("weekly labels = %q/%q", codexusage.FrontWindowLabel(7*24*time.Hour), codexusage.BackWindowLabel(7*24*time.Hour))
	}
}

func TestNormalizeUsageRequiresAtLeastOneUsableWindow(t *testing.T) {
	t.Parallel()
	for _, response := range []usageResponse{
		{},
		{RateLimit: &rateLimitResponse{}},
		{RateLimit: &rateLimitResponse{Primary: &windowResponse{UsedPercent: 10, WindowSeconds: 0}}},
	} {
		if _, err := normalizeUsage(response, time.Now()); err == nil {
			t.Fatalf("normalizeUsage accepted %#v", response)
		}
	}
}

func TestNormalizeUsagePreservesWindowWhenResetTimeIsMissing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	snapshot, err := normalizeUsage(usageResponse{RateLimit: &rateLimitResponse{
		Primary: &windowResponse{UsedPercent: 25, WindowSeconds: int64((5 * time.Hour) / time.Second)},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Windows) != 1 || !snapshot.Windows[0].ResetsAt.IsZero() {
		t.Fatalf("normalized windows = %#v, want one window with unknown reset", snapshot.Windows)
	}
}

func TestQuotaSignalUsesLowestRemainingWindow(t *testing.T) {
	t.Parallel()
	config := defaultConfig("/tmp/codex")
	for _, test := range []struct {
		remaining   []int
		disposition signalDisposition
	}{
		{[]int{80, 21}, signalNone},
		{[]int{80, 20}, signalLow},
		{[]int{80, 6}, signalLow},
		{[]int{80, 5}, signalCritical},
		{[]int{0, 90}, signalCritical},
	} {
		snapshot := Snapshot{}
		for _, remaining := range test.remaining {
			snapshot.Windows = append(snapshot.Windows, Window{RemainingPercent: remaining})
		}
		if got := quotaSignal(snapshot, config); got != test.disposition {
			t.Fatalf("quotaSignal(%v) = %v, want %v", test.remaining, got, test.disposition)
		}
	}
}
