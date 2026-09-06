package attention

import (
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestSelectEnforcesModeBeforeImpactAndExplainsRejection(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	criticalRotation := record("rotation", protocol.DispositionActionable, protocol.ImpactCritical, now)
	normalAttention := record("attention", protocol.DispositionActionable, protocol.ImpactNormal, now)
	rules := map[string]Rule{
		criticalRotation.ID(): {Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation, RotationIntervalMS: 10_000},
		normalAttention.ID():  {Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention},
	}
	decision := Select([]observation.Record{criticalRotation, normalAttention}, func(record observation.Record) (Rule, bool) {
		rule, ok := rules[record.ID()]
		return rule, ok
	}, presentation.History{LastShown: map[string]time.Time{}}, now)
	if decision.Candidate == nil || decision.Candidate.ID() != normalAttention.ID() {
		t.Fatalf("winner = %#v, want attention", decision.Candidate)
	}
	if got := reasonFor(decision.Evaluations, criticalRotation.ID()); got != ReasonLowerBand {
		t.Fatalf("rotation reason = %q, want %q", got, ReasonLowerBand)
	}
}

func TestSelectRotationNeverShownIsImmediatelyDue(t *testing.T) {
	now := time.Date(2026, 8, 21, 4, 0, 0, 0, time.UTC)
	rotation := record("rotation", protocol.DispositionNotable, protocol.ImpactNormal, now)
	decision := Select([]observation.Record{rotation}, func(observation.Record) (Rule, bool) {
		return Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation, RotationIntervalMS: 10_000, RotationJitterPercent: 50}, true
	}, presentation.History{LastShown: map[string]time.Time{}}, now)
	if decision.Candidate == nil || decision.Candidate.ID() != rotation.ID() {
		t.Fatalf("winner = %#v, want never-shown rotation", decision.Candidate)
	}
	if got := reasonFor(decision.Evaluations, rotation.ID()); got != ReasonSelected {
		t.Fatalf("reason = %q, want %q", got, ReasonSelected)
	}
}

func TestSelectRotationReportsExactNextDueAndNotDueReason(t *testing.T) {
	shownAt := time.Date(2026, 8, 21, 5, 0, 0, 0, time.UTC)
	rotation := record("rotation", protocol.DispositionNotable, protocol.ImpactNormal, shownAt)
	decision := Select([]observation.Record{rotation}, func(observation.Record) (Rule, bool) {
		return Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation, RotationIntervalMS: 10_000}, true
	}, presentation.History{LastShown: map[string]time.Time{rotation.ID(): shownAt}}, shownAt.Add(9*time.Second))
	if decision.Candidate != nil {
		t.Fatalf("winner = %#v, want not due", decision.Candidate)
	}
	evaluation := evaluationFor(decision.Evaluations, rotation.ID())
	if evaluation.Reason != ReasonNotDue || !evaluation.NextDue.Equal(shownAt.Add(10*time.Second)) {
		t.Fatalf("evaluation = %#v", evaluation)
	}
}

func TestSelectCurrentRotationYieldsAfterReadableHold(t *testing.T) {
	shownAt := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	rotation := record("rotation", protocol.DispositionNotable, protocol.ImpactNormal, shownAt)
	resolve := func(observation.Record) (Rule, bool) {
		return Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation, RotationIntervalMS: 60_000}, true
	}
	history := presentation.History{
		CurrentID: rotation.ID(), CurrentSince: shownAt,
		LastShown: map[string]time.Time{rotation.ID(): shownAt},
	}
	if decision := Select([]observation.Record{rotation}, resolve, history, shownAt.Add(7*time.Second)); decision.Candidate == nil {
		t.Fatal("current rotation did not receive its readable hold")
	}
	decision := Select([]observation.Record{rotation}, resolve, history, shownAt.Add(8*time.Second))
	if decision.Candidate != nil {
		t.Fatalf("rotation remained resident after hold: %#v", decision.Candidate)
	}
	evaluation := evaluationFor(decision.Evaluations, rotation.ID())
	if evaluation.Reason != ReasonNotDue || !evaluation.NextDue.Equal(shownAt.Add(time.Minute)) {
		t.Fatalf("evaluation = %#v", evaluation)
	}
}

func TestSelectRotationJitterIsDeterministicIdentityBasedAndBounded(t *testing.T) {
	shownAt := time.Date(2026, 8, 21, 6, 0, 0, 0, time.UTC)
	first := record("first", protocol.DispositionNotable, protocol.ImpactNormal, shownAt)
	second := record("second", protocol.DispositionNotable, protocol.ImpactNormal, shownAt)
	resolve := func(observation.Record) (Rule, bool) {
		return Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation, RotationIntervalMS: 20_000, RotationJitterPercent: 25}, true
	}
	history := presentation.History{LastShown: map[string]time.Time{first.ID(): shownAt, second.ID(): shownAt}}
	firstDecision := Select([]observation.Record{first, second}, resolve, history, shownAt.Add(time.Second))
	restartedDecision := Select([]observation.Record{first, second}, resolve, history, shownAt.Add(time.Second))
	firstDue := evaluationFor(firstDecision.Evaluations, first.ID()).NextDue
	secondDue := evaluationFor(firstDecision.Evaluations, second.ID()).NextDue
	if !firstDue.Equal(evaluationFor(restartedDecision.Evaluations, first.ID()).NextDue) || !secondDue.Equal(evaluationFor(restartedDecision.Evaluations, second.ID()).NextDue) {
		t.Fatal("identity jitter changed across equivalent evaluations")
	}
	minimum, maximum := shownAt.Add(15*time.Second), shownAt.Add(25*time.Second)
	for id, due := range map[string]time.Time{first.ID(): firstDue, second.ID(): secondDue} {
		if due.Before(minimum) || due.After(maximum) {
			t.Fatalf("%s next due = %v, want [%v, %v]", id, due, minimum, maximum)
		}
	}
	if firstDue.Equal(secondDue) {
		t.Fatalf("distinct identities received identical deterministic jitter: %v", firstDue)
	}
}

func TestSelectCriticalActionableAttentionInterruptsOrdinaryHold(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	current := record("current", protocol.DispositionSnapshot, protocol.ImpactNormal, now)
	critical := record("critical", protocol.DispositionActionable, protocol.ImpactCritical, now)
	rules := map[string]Rule{
		current.ID():  {Enabled: true, AssetsReady: true, Policy: presentation.PolicyInteractive, Foreground: true, HoldMS: 30_000},
		critical.ID(): {Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention},
	}
	history := presentation.History{CurrentID: current.ID(), CurrentSince: now, LastShown: map[string]time.Time{current.ID(): now}}
	decision := Select([]observation.Record{current, critical}, func(record observation.Record) (Rule, bool) {
		return rules[record.ID()], true
	}, history, now.Add(time.Second))
	if decision.Candidate == nil || decision.Candidate.ID() != critical.ID() {
		t.Fatalf("winner = %#v, want critical attention", decision.Candidate)
	}
}

func TestSelectDefersCriticalAttentionDuringAtomicExecution(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 30, 0, 0, time.UTC)
	critical := record("critical", protocol.DispositionActionable, protocol.ImpactCritical, now)
	decision := Select([]observation.Record{critical}, func(observation.Record) (Rule, bool) {
		return Rule{
			Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention,
			BlockedByAtomicExecution: true,
		}, true
	}, presentation.History{LastShown: map[string]time.Time{}}, now)
	if decision.Candidate != nil {
		t.Fatalf("critical candidate crossed atomic boundary: %#v", decision.Candidate)
	}
	if got := reasonFor(decision.Evaluations, critical.ID()); got != ReasonBlockedByAtomicExecution {
		t.Fatalf("reason=%q, want %q", got, ReasonBlockedByAtomicExecution)
	}
}

func TestSelectReportsAssetsPendingAndForegroundInactive(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	pending := record("pending", protocol.DispositionNotable, protocol.ImpactNotable, now)
	inactive := record("inactive", protocol.DispositionSnapshot, protocol.ImpactNormal, now)
	rules := map[string]Rule{
		pending.ID():  {Enabled: true, AssetsReady: false, Policy: presentation.PolicyWhenRelevant},
		inactive.ID(): {Enabled: true, AssetsReady: true, Policy: presentation.PolicyInteractive, Foreground: false},
	}
	decision := Select([]observation.Record{pending, inactive}, func(record observation.Record) (Rule, bool) {
		return rules[record.ID()], true
	}, presentation.History{LastShown: map[string]time.Time{}}, now)
	if decision.Candidate != nil {
		t.Fatalf("winner = %#v, want none", decision.Candidate)
	}
	if got := reasonFor(decision.Evaluations, pending.ID()); got != ReasonAssetsPending {
		t.Fatalf("pending reason = %q", got)
	}
	if got := reasonFor(decision.Evaluations, inactive.ID()); got != ReasonForegroundInactive {
		t.Fatalf("inactive reason = %q", got)
	}
}

func TestSelectDerivesSemanticBandAndSnapshotEligibility(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		policy      presentation.Policy
		disposition protocol.Disposition
		impact      protocol.Impact
		foreground  bool
		wantBand    presentation.Band
		wantReason  Reason
	}{
		{name: "critical actionable", policy: presentation.PolicyAttention, disposition: protocol.DispositionActionable, impact: protocol.ImpactCritical, wantBand: presentation.BandCriticalActionable},
		{name: "ordinary actionable", policy: presentation.PolicyAttention, disposition: protocol.DispositionActionable, impact: protocol.ImpactNotable, wantBand: presentation.BandActionable},
		{name: "interactive snapshot", policy: presentation.PolicyInteractive, disposition: protocol.DispositionSnapshot, impact: protocol.ImpactLow, foreground: true, wantBand: presentation.BandInteractive},
		{name: "inactive interactive", policy: presentation.PolicyInteractive, disposition: protocol.DispositionSnapshot, impact: protocol.ImpactNormal, wantReason: ReasonForegroundInactive},
		{name: "relevant notable", policy: presentation.PolicyWhenRelevant, disposition: protocol.DispositionNotable, impact: protocol.ImpactNormal, wantBand: presentation.BandRelevant},
		{name: "when relevant snapshot", policy: presentation.PolicyWhenRelevant, disposition: protocol.DispositionSnapshot, impact: protocol.ImpactNormal, wantReason: ReasonNotRelevant},
		{name: "due rotation", policy: presentation.PolicyRotation, disposition: protocol.DispositionNotable, impact: protocol.ImpactLow, wantBand: presentation.BandRotation},
		{name: "rotation snapshot", policy: presentation.PolicyRotation, disposition: protocol.DispositionSnapshot, impact: protocol.ImpactNormal, wantReason: ReasonNotRelevant},
		{name: "attention snapshot", policy: presentation.PolicyAttention, disposition: protocol.DispositionSnapshot, impact: protocol.ImpactCritical, wantReason: ReasonNotActionableForAttention},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := record("value", test.disposition, test.impact, now)
			decision := Select([]observation.Record{value}, func(observation.Record) (Rule, bool) {
				return Rule{Enabled: true, AssetsReady: true, Policy: test.policy, Foreground: test.foreground, RotationIntervalMS: 10_000}, true
			}, presentation.History{LastShown: map[string]time.Time{}}, now)
			if test.wantReason != "" {
				if decision.Candidate != nil || reasonFor(decision.Evaluations, value.ID()) != test.wantReason {
					t.Fatalf("candidate = %#v, evaluations = %#v", decision.Candidate, decision.Evaluations)
				}
				return
			}
			if decision.Candidate == nil || decision.Candidate.Band != test.wantBand {
				t.Fatalf("candidate = %#v, want band %q", decision.Candidate, test.wantBand)
			}
		})
	}
}

func TestSelectUsesDaemonAdmissionFIFO(t *testing.T) {
	now := time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	first := record("z-first", protocol.DispositionActionable, protocol.ImpactNormal, now).Observation
	second := record("a-second", protocol.DispositionActionable, protocol.ImpactNormal, now).Observation
	source := observation.Source{PluginID: "plugin", Generation: 1}
	if err := store.Publish(source, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(source, second); err != nil {
		t.Fatal(err)
	}
	resolve := func(observation.Record) (Rule, bool) {
		return Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention}, true
	}
	decision := Select(store.Snapshot(), resolve, presentation.History{LastShown: map[string]time.Time{}}, now)
	if decision.Candidate == nil || decision.Candidate.Key != first.Key {
		t.Fatalf("FIFO winner = %#v, want first accepted revision", decision.Candidate)
	}
	if err := store.Publish(source, first); err != nil {
		t.Fatal(err)
	}
	first.Revision++
	first.UpdatedAt = now.Add(time.Second)
	if err := store.Publish(source, first); err != nil {
		t.Fatal(err)
	}
	decision = Select(store.Snapshot(), resolve, presentation.History{LastShown: map[string]time.Time{}}, now)
	if decision.Candidate == nil || decision.Candidate.Key != second.Key {
		t.Fatalf("winner after replacement = %#v, want earlier remaining admission", decision.Candidate)
	}
}

func TestSelectCarriesNativeBusyTimerToDeviceCandidate(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	meeting := record("meeting", protocol.DispositionNotable, protocol.ImpactNormal, now)
	meeting.Observation.Scene = nil
	meeting.Observation.BusyTimer = &protocol.BusyTimerPresentation{Theme: "meeting"}
	decision := Select([]observation.Record{meeting}, func(observation.Record) (Rule, bool) {
		return Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyWhenRelevant}, true
	}, presentation.History{LastShown: map[string]time.Time{}}, now)
	if decision.Candidate == nil || decision.Candidate.BusyTimer == nil || decision.Candidate.BusyTimer.Theme != "meeting" {
		t.Fatalf("selected candidate = %#v", decision.Candidate)
	}
}

func record(key string, disposition protocol.Disposition, impact protocol.Impact, now time.Time) observation.Record {
	return observation.Record{PluginID: "plugin", Generation: 1, AdmissionSequence: 1, Observation: protocol.Observation{
		Instance: protocol.InstanceRef{ID: "app", Generation: 1}, Channel: "main", Key: key, Revision: 1,
		Disposition: disposition, Impact: impact, ReasonCode: "test_state",
		ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(time.Minute),
		Scene: &presentation.Scene{Elements: []presentation.Element{{ID: "text", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: key, Font: "normal"}}}},
	}}
}

func reasonFor(values []Evaluation, id string) Reason {
	for _, value := range values {
		if value.ObservationID == id {
			return value.Reason
		}
	}
	return ""
}

func evaluationFor(values []Evaluation, id string) Evaluation {
	for _, value := range values {
		if value.ObservationID == id {
			return value
		}
	}
	return Evaluation{}
}
