package codex

import "testing"

func TestDecodeConfigDefaultsSensitiveRequestDetailsOn(t *testing.T) {
	config, err := decodeConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !config.ShowSensitiveRequestDetails {
		t.Fatal("sensitive request details defaulted off")
	}
	if config.ShowQuota {
		t.Fatal("app-server quota defaulted on")
	}
	if config.QuotaWarningRemainingPercent != 20 || config.QuotaCriticalRemainingPercent != 5 {
		t.Fatalf("quota thresholds = %d/%d, want 20/5", config.QuotaWarningRemainingPercent, config.QuotaCriticalRemainingPercent)
	}
}

func TestDecodeConfigAcceptsOptInQuotaAndValidatedThresholds(t *testing.T) {
	config, err := decodeConfig([]byte(`{"show_quota":true,"quota_warning_remaining_percent":25,"quota_critical_remaining_percent":10}`))
	if err != nil {
		t.Fatal(err)
	}
	if !config.ShowQuota || config.QuotaWarningRemainingPercent != 25 || config.QuotaCriticalRemainingPercent != 10 {
		t.Fatalf("quota config = %#v", config)
	}
}

func TestDecodeConfigRejectsInvalidQuotaThresholdOrdering(t *testing.T) {
	for _, raw := range []string{
		`{"quota_warning_remaining_percent":0}`,
		`{"quota_warning_remaining_percent":101}`,
		`{"quota_critical_remaining_percent":-1}`,
		`{"quota_critical_remaining_percent":21,"quota_warning_remaining_percent":20}`,
	} {
		if _, err := decodeConfig([]byte(raw)); err == nil {
			t.Fatalf("invalid quota thresholds accepted: %s", raw)
		}
	}
}

func TestDecodeConfigAcceptsExplicitSensitiveRequestDetails(t *testing.T) {
	config, err := decodeConfig([]byte(`{"show_sensitive_request_details":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !config.ShowSensitiveRequestDetails {
		t.Fatal("explicit sensitive request details were ignored")
	}
}

func TestDecodeConfigAcceptsSensitiveRequestDetailsOptOut(t *testing.T) {
	config, err := decodeConfig([]byte(`{"show_sensitive_request_details":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.ShowSensitiveRequestDetails {
		t.Fatal("explicit sensitive request details opt-out was ignored")
	}
}

func TestDecodeConfigRejectsUnknownFields(t *testing.T) {
	if _, err := decodeConfig([]byte(`{"codex_bin":"/tmp/codex"}`)); err == nil {
		t.Fatal("unknown configuration field was accepted")
	}
}

func TestDecodeConfigRejectsDuplicateFields(t *testing.T) {
	t.Parallel()
	if _, err := decodeConfig([]byte(`{"show_quota":true,"show_quota":false}`)); err == nil {
		t.Fatal("duplicate configuration field was accepted")
	}
}
