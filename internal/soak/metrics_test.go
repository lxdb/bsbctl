package soak

import (
	"strings"
	"testing"
	"time"
)

func TestSelectDaemonTreeRequiresExactParentAndResidentChildren(t *testing.T) {
	t.Parallel()
	rows, err := ParseProcessSnapshot(strings.NewReader(`
101 1 0:00.10 12000 /private/tmp/soak/bsbctl
102 101 0:00.02 8000 /private/tmp/soak/bsbctl-plugin-mac-resources
103 101 0:00.03 9000 /private/tmp/soak/bsbctl-plugin-codex-quota
104 999 0:00.01 7000 /private/tmp/soak/releasectl
`))
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"bsbctl", "bsbctl-plugin-codex-quota", "bsbctl-plugin-mac-resources"}
	identities, err := SelectDaemonTree(rows, 101)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != len(wantNames) {
		t.Fatalf("identities = %#v", identities)
	}
	for index, want := range wantNames {
		if identities[index].Name != want {
			t.Fatalf("identities[%d].Name = %q, want %q", index, identities[index].Name, want)
		}
	}

	for name, raw := range map[string]string{
		"missing child": `101 1 0:00.10 12000 /tmp/bsbctl
102 101 0:00.02 8000 /tmp/bsbctl-plugin-mac-resources`,
		"unexpected child": `101 1 0:00.10 12000 /tmp/bsbctl
102 101 0:00.02 8000 /tmp/bsbctl-plugin-mac-resources
103 101 0:00.03 9000 /tmp/bsbctl-plugin-codex-quota
105 101 0:00.01 1000 /tmp/unexpected-helper`,
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := ParseProcessSnapshot(strings.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := SelectDaemonTree(rows, 101); err == nil {
				t.Fatal("SelectDaemonTree unexpectedly accepted an incomplete or expanded tree")
			}
		})
	}
}

func TestMeasureIntervalReportsEveryProcessAndAggregate(t *testing.T) {
	t.Parallel()
	before, err := ParseProcessSnapshot(strings.NewReader(`
101 1 0:00.10 12000 /tmp/bsbctl
102 101 0:00.02 8000 /tmp/bsbctl-plugin-mac-resources
103 101 0:00.03 9000 /tmp/bsbctl-plugin-codex-quota
`))
	if err != nil {
		t.Fatal(err)
	}
	after, err := ParseProcessSnapshot(strings.NewReader(`
101 1 0:00.11 12100 /tmp/bsbctl
102 101 0:00.02 8100 /tmp/bsbctl-plugin-mac-resources
103 101 0:00.04 9200 /tmp/bsbctl-plugin-codex-quota
`))
	if err != nil {
		t.Fatal(err)
	}
	identities, err := SelectDaemonTree(before, 101)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	sample, err := MeasureInterval(1, start, start.Add(2*time.Second), identities, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(sample.Processes) != 3 {
		t.Fatalf("process count = %d", len(sample.Processes))
	}
	if got := sample.AggregateCPUPercent; got < 0.999 || got > 1.001 {
		t.Fatalf("aggregate CPU = %f, want 1.0", got)
	}
	if got, want := sample.AggregateRSSBytes, int64((12100+8100+9200)*1024); got != want {
		t.Fatalf("aggregate RSS = %d, want %d", got, want)
	}
	if got := sample.Processes[0].CPUPercent; got < 0.499 || got > 0.501 {
		t.Fatalf("bsbctl CPU = %f, want 0.5", got)
	}
}

func TestMeasureIntervalRejectsUnavailableOrRegressedTelemetry(t *testing.T) {
	t.Parallel()
	before, err := ParseProcessSnapshot(strings.NewReader(`
101 1 0:01.00 12000 /tmp/bsbctl
102 101 0:00.02 8000 /tmp/bsbctl-plugin-mac-resources
103 101 0:00.03 9000 /tmp/bsbctl-plugin-codex-quota
`))
	if err != nil {
		t.Fatal(err)
	}
	identities, err := SelectDaemonTree(before, 101)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for name, raw := range map[string]string{
		"missing process": `101 1 0:01.01 12000 /tmp/bsbctl
102 101 0:00.02 8000 /tmp/bsbctl-plugin-mac-resources`,
		"cpu regressed": `101 1 0:00.99 12000 /tmp/bsbctl
102 101 0:00.02 8000 /tmp/bsbctl-plugin-mac-resources
103 101 0:00.03 9000 /tmp/bsbctl-plugin-codex-quota`,
	} {
		t.Run(name, func(t *testing.T) {
			after, err := ParseProcessSnapshot(strings.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := MeasureInterval(1, start, start.Add(time.Second), identities, before, after); err == nil {
				t.Fatal("MeasureInterval unexpectedly treated unavailable telemetry as zero")
			}
		})
	}
}

func TestEvaluateEnforcesEveryAggregateSample(t *testing.T) {
	t.Parallel()
	limits := Limits{CPUPercent: 1, RSSBytes: 100 << 20}
	passing := []Sample{
		{AggregateCPUPercent: 0.5, AggregateRSSBytes: 50 << 20},
		{AggregateCPUPercent: 1.0, AggregateRSSBytes: 100 << 20},
	}
	summary, err := Evaluate(passing, limits)
	if err != nil {
		t.Fatal(err)
	}
	if summary.SampleCount != 2 || summary.MaxAggregateCPUPercent != 1 || summary.MaxAggregateRSSBytes != 100<<20 {
		t.Fatalf("summary = %#v", summary)
	}

	for name, samples := range map[string][]Sample{
		"no telemetry": nil,
		"cpu exceeded": {{AggregateCPUPercent: 1.01, AggregateRSSBytes: 50 << 20}},
		"rss exceeded": {{AggregateCPUPercent: 0.1, AggregateRSSBytes: (100 << 20) + 1}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Evaluate(samples, limits); err == nil {
				t.Fatal("Evaluate unexpectedly passed unavailable or over-limit telemetry")
			}
		})
	}
}

func TestParseProcessSnapshotRejectsMalformedOrDuplicateRows(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"malformed":    "101 bad 0:00.01 100 /tmp/bsbctl",
		"bad time":     "101 1 not-time 100 /tmp/bsbctl",
		"duplicate":    "101 1 0:00.01 100 /tmp/bsbctl\n101 1 0:00.02 101 /tmp/bsbctl",
		"empty":        "",
		"negative rss": "101 1 0:00.01 -1 /tmp/bsbctl",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseProcessSnapshot(strings.NewReader(raw)); err == nil {
				t.Fatal("ParseProcessSnapshot unexpectedly accepted invalid telemetry")
			}
		})
	}
}
