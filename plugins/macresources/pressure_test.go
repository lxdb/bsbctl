package macresources

import "testing"

func TestPressureRequiresSustainedSamplesAndHysteresis(t *testing.T) {
	t.Parallel()

	machine := newPressureMachine(Config{
		WarningPercent: 70, CriticalPercent: 90, SustainSamples: 3, RecoveryMarginPercent: 5,
	})

	assertLevel := func(cpu float64, count int, want pressureLevel) {
		t.Helper()
		for range count {
			if got := machine.update(pressureValues{CPU: cpu}); got.level != want {
				t.Fatalf("after cpu %.0f, level = %q, want %q", cpu, got.level, want)
			}
		}
	}

	assertLevel(75, 2, pressureNormal)
	assertLevel(60, 1, pressureNormal) // breaks the pending promotion
	assertLevel(75, 2, pressureNormal)
	assertLevel(75, 1, pressureWarning)
	assertLevel(92, 2, pressureWarning)
	assertLevel(92, 1, pressureCritical)
	assertLevel(87, 3, pressureCritical) // not below critical hysteresis boundary
	assertLevel(84, 2, pressureCritical)
	assertLevel(84, 1, pressureWarning)
	assertLevel(66, 3, pressureWarning) // not below warning hysteresis boundary
	assertLevel(64, 2, pressureWarning)
	assertLevel(64, 1, pressureNormal)
}

func TestPressureCanPromoteOrRecoverDirectlyAfterSustainedSamples(t *testing.T) {
	t.Parallel()

	machine := newPressureMachine(Config{
		WarningPercent: 70, CriticalPercent: 90, SustainSamples: 3, RecoveryMarginPercent: 5,
	})
	for range 2 {
		if got := machine.update(pressureValues{Memory: 95}); got.level != pressureNormal {
			t.Fatalf("early level = %q", got.level)
		}
	}
	if got := machine.update(pressureValues{Memory: 95}); got.level != pressureCritical || got.reason != "memory_pressure" {
		t.Fatalf("critical state = %#v", got)
	}
	for range 2 {
		if got := machine.update(pressureValues{CPU: 20, Memory: 20}); got.level != pressureCritical {
			t.Fatalf("early recovery = %q", got.level)
		}
	}
	if got := machine.update(pressureValues{CPU: 20, Memory: 20}); got.level != pressureNormal || got.reason != "pressure_resolved" {
		t.Fatalf("resolved state = %#v", got)
	}
}

func TestPressureReasonNamesAllMaterialSources(t *testing.T) {
	t.Parallel()
	machine := newPressureMachine(Config{WarningPercent: 70, CriticalPercent: 90, SustainSamples: 1, RecoveryMarginPercent: 5})
	if got := machine.update(pressureValues{CPU: 92, Memory: 94, Network: 20}); got.reason != "multiple_pressure" {
		t.Fatalf("reason = %q, want multiple_pressure", got.reason)
	}
}

func TestPressurePreservesCauseWhileHysteresisKeepsConditionActive(t *testing.T) {
	t.Parallel()
	machine := newPressureMachine(Config{WarningPercent: 70, CriticalPercent: 90, SustainSamples: 1, RecoveryMarginPercent: 5})
	if got := machine.update(pressureValues{CPU: 92}); got.reason != "cpu_pressure" {
		t.Fatalf("entry reason = %q", got.reason)
	}
	if got := machine.update(pressureValues{CPU: 87}); got.level != pressureCritical || got.reason != "cpu_pressure" {
		t.Fatalf("hysteresis state = %#v", got)
	}
}
