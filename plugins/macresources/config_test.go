package macresources

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestDecodeConfigAppliesDefaults(t *testing.T) {
	t.Parallel()

	got, err := decodeConfig(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("decodeConfig: %v", err)
	}
	want := Config{
		SampleInterval:                2 * time.Second,
		SummaryInterval:               3 * time.Minute,
		WarningPercent:                70,
		CriticalPercent:               90,
		SustainSamples:                3,
		RecoveryMarginPercent:         5,
		NetworkCapacityBytesPerSecond: 10 * 1024 * 1024,
	}
	if got != want {
		t.Fatalf("config = %#v, want %#v", got, want)
	}
}

func TestDecodeConfigRejectsUnknownAndOutOfBoundsValues(t *testing.T) {
	t.Parallel()

	tests := []string{
		`{"extra":true}`,
		`{"warning_percent":70,"warning_percent":80}`,
		`{"sample_interval_seconds":0}`,
		`{"sample_interval_seconds":61}`,
		`{"sample_interval_seconds":36028797018963970}`,
		`{"summary_interval_seconds":119}`,
		`{"summary_interval_seconds":301}`,
		`{"warning_percent":90,"critical_percent":90}`,
		`{"warning_percent":0}`,
		`{"critical_percent":101}`,
		`{"sustain_samples":31}`,
		`{"recovery_margin_percent":21}`,
		`{"warning_percent":5,"recovery_margin_percent":5}`,
		`{"network_capacity_bytes_per_second":1023}`,
		`{"network_capacity_bytes_per_second":1099511627777}`,
		`{} {}`,
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if _, err := decodeConfig(json.RawMessage(raw)); err == nil {
				t.Fatal("decodeConfig accepted invalid configuration")
			}
		})
	}
}

func TestDecodeConfigAcceptsSummaryIntervalBounds(t *testing.T) {
	t.Parallel()

	for _, seconds := range []int{120, 300} {
		raw := json.RawMessage(fmt.Sprintf(`{"summary_interval_seconds":%d}`, seconds))
		got, err := decodeConfig(raw)
		if err != nil {
			t.Fatalf("decodeConfig(%d): %v", seconds, err)
		}
		if got.SummaryInterval != time.Duration(seconds)*time.Second {
			t.Fatalf("summary interval = %v, want %ds", got.SummaryInterval, seconds)
		}
	}
}
