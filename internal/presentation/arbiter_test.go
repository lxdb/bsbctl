package presentation

import (
	"errors"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestValidateObservationRejectsBusylibInvalidPresentation(t *testing.T) {
	observation := protocol.Observation{
		Disposition: protocol.DispositionSnapshot,
		Scene: &protocol.Scene{Elements: []protocol.Element{{
			ID: "title", Display: protocol.DisplayFront,
			Text: &protocol.TextElement{Value: "Ready", Font: "not-a-firmware-font"},
		}}},
	}
	if err := ValidateObservation("dev.bsbctl.test", observation, nil); !errors.Is(err, ErrInvalidPresentation) {
		t.Fatalf("ValidateObservation error = %v, want ErrInvalidPresentation", err)
	}
}

func TestValidateObservationRejectsUnresolvedAudioBeforeDeviceIO(t *testing.T) {
	observation := protocol.Observation{
		Disposition: protocol.DispositionSnapshot,
		BusyTimer:   &protocol.BusyTimerPresentation{Theme: "busy"},
		Audio: &protocol.AudioCue{
			ID: "cue", Asset: protocol.AssetRef{StockName: "calendar_event_starts.snd"},
		},
	}
	if err := ValidateObservation("dev.bsbctl.test", observation, nil); !errors.Is(err, ErrInvalidPresentation) {
		t.Fatalf("ValidateObservation error = %v, want ErrInvalidPresentation", err)
	}
}

func TestValidateObservationCompilesBusyTimerBeforeDeviceIO(t *testing.T) {
	observation := protocol.Observation{
		Disposition: protocol.DispositionSnapshot,
		BusyTimer:   &protocol.BusyTimerPresentation{Theme: string(make([]byte, 65))},
	}
	if err := ValidateObservation("dev.bsbctl.test", observation, nil); !errors.Is(err, ErrInvalidPresentation) {
		t.Fatalf("ValidateObservation error = %v, want ErrInvalidPresentation", err)
	}
}

func TestCompileSceneRejectsEveryNonRepresentableBusyField(t *testing.T) {
	validText := func() ResolvedScene {
		return ResolvedScene{Elements: []ResolvedElement{{Element: protocol.Element{
			ID: "title", Display: protocol.DisplayFront, X: 0, Y: 0,
			Text: &protocol.TextElement{Value: "Ready", Font: "normal", Color: "#FFFFFFFF", Align: "center", Width: 10},
		}}}}
	}
	tests := []struct {
		name   string
		mutate func(*ResolvedScene)
	}{
		{name: "font", mutate: func(scene *ResolvedScene) { scene.Elements[0].Text.Font = "unknown" }},
		{name: "color", mutate: func(scene *ResolvedScene) { scene.Elements[0].Text.Color = "#fff" }},
		{name: "align", mutate: func(scene *ResolvedScene) { scene.Elements[0].Text.Align = "middle" }},
		{name: "x", mutate: func(scene *ResolvedScene) { scene.Elements[0].X = -1 }},
		{name: "y", mutate: func(scene *ResolvedScene) { scene.Elements[0].Y = 16 }},
		{name: "width", mutate: func(scene *ResolvedScene) { scene.Elements[0].Text.Width = -1 }},
		{name: "back coordinates", mutate: func(scene *ResolvedScene) {
			scene.Elements[0].Display = protocol.DisplayBack
			scene.Elements[0].X = 160
		}},
		{name: "rectangle width", mutate: func(scene *ResolvedScene) {
			scene.Elements[0].Text = nil
			scene.Elements[0].Rectangle = &protocol.RectangleElement{Width: 0, Height: 1, Color: "#FFFFFFFF"}
		}},
		{name: "rectangle height", mutate: func(scene *ResolvedScene) {
			scene.Elements[0].Text = nil
			scene.Elements[0].Rectangle = &protocol.RectangleElement{Width: 1, Height: 0, Color: "#FFFFFFFF"}
		}},
		{name: "rectangle color", mutate: func(scene *ResolvedScene) {
			scene.Elements[0].Text = nil
			scene.Elements[0].Rectangle = &protocol.RectangleElement{Width: 1, Height: 1, Color: "#fff"}
		}},
		{name: "countdown color", mutate: func(scene *ResolvedScene) {
			scene.Elements[0].Text = nil
			scene.Elements[0].Countdown = &protocol.CountdownElement{EndsAtUnixSeconds: 1, ShowHours: protocol.CountdownShowHoursAlways, Color: "#fff"}
		}},
		{name: "countdown align", mutate: func(scene *ResolvedScene) {
			scene.Elements[0].Text = nil
			scene.Elements[0].Countdown = &protocol.CountdownElement{EndsAtUnixSeconds: 1, ShowHours: protocol.CountdownShowHoursAlways, Color: "#FFFFFFFF", Align: "middle"}
		}},
		{name: "countdown", mutate: func(scene *ResolvedScene) {
			scene.Elements[0].Text = nil
			scene.Elements[0].Countdown = &protocol.CountdownElement{EndsAtUnixSeconds: 1, ShowHours: "sometimes", Color: "#FFFFFFFF"}
		}},
		{name: "display", mutate: func(scene *ResolvedScene) { scene.Elements[0].Display = "side" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scene := validText()
			test.mutate(&scene)
			if _, err := CompileScene("bsbctl", 100, scene); !errors.Is(err, ErrInvalidPresentation) {
				t.Fatalf("CompileScene error = %v, want ErrInvalidPresentation", err)
			}
		})
	}
}

func TestSelectSemanticBandOrderAndHold(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	rotation := candidate("rotation", PolicyRotation, BandRotation, protocol.ImpactCritical, 1, now)
	relevant := candidate("relevant", PolicyWhenRelevant, BandRelevant, protocol.ImpactNotable, 2, now)
	actionable := candidate("actionable", PolicyAttention, BandActionable, protocol.ImpactNotable, 3, now)
	interactive := candidate("interactive", PolicyInteractive, BandInteractive, protocol.ImpactLow, 4, now)
	critical := candidate("critical", PolicyAttention, BandCriticalActionable, protocol.ImpactCritical, 5, now)

	decision := Select([]Candidate{rotation, relevant, actionable, interactive}, History{}, now)
	if decision.Candidate == nil || decision.Candidate.ID() != interactive.ID() {
		t.Fatalf("winner = %#v, want interactive", decision.Candidate)
	}

	history := History{CurrentID: interactive.ID(), CurrentSince: now, LastShown: map[string]time.Time{interactive.ID(): now}}
	decision = Select([]Candidate{interactive, actionable}, history, now.Add(time.Second))
	if decision.Candidate == nil || decision.Candidate.ID() != interactive.ID() {
		t.Fatalf("held winner = %#v, want interactive", decision.Candidate)
	}

	decision = Select([]Candidate{interactive, critical}, history, now.Add(2*time.Second))
	if decision.Candidate == nil || decision.Candidate.ID() != critical.ID() {
		t.Fatalf("critical winner = %#v, want critical actionable", decision.Candidate)
	}
}

func TestSelectImpactAdmissionFIFOAndStableTieBreak(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	low := candidate("low", PolicyWhenRelevant, BandRelevant, protocol.ImpactLow, 1, now)
	normal := candidate("normal", PolicyWhenRelevant, BandRelevant, protocol.ImpactNormal, 2, now)
	notable := candidate("notable", PolicyWhenRelevant, BandRelevant, protocol.ImpactNotable, 3, now)
	critical := candidate("critical", PolicyWhenRelevant, BandRelevant, protocol.ImpactCritical, 4, now)

	decision := Select([]Candidate{low, normal, notable, critical}, History{}, now)
	if decision.Candidate == nil || decision.Candidate.ID() != critical.ID() {
		t.Fatalf("impact winner = %#v, want critical", decision.Candidate)
	}

	first := candidate("z-first", PolicyAttention, BandActionable, protocol.ImpactNormal, 10, now)
	second := candidate("a-second", PolicyAttention, BandActionable, protocol.ImpactNormal, 11, now.Add(time.Hour))
	decision = Select([]Candidate{second, first}, History{}, now)
	if decision.Candidate == nil || decision.Candidate.ID() != first.ID() {
		t.Fatalf("FIFO winner = %#v, want first admission", decision.Candidate)
	}

	a := candidate("a", PolicyAttention, BandActionable, protocol.ImpactNormal, 12, now)
	b := candidate("b", PolicyAttention, BandActionable, protocol.ImpactNormal, 12, now)
	decision = Select([]Candidate{b, a}, History{}, now)
	if decision.Candidate == nil || decision.Candidate.ID() != a.ID() {
		t.Fatalf("stable identity winner = %#v, want a", decision.Candidate)
	}
}

func TestSelectInterruptsHoldOnlyForHigherBandOrHigherActionableImpact(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	current := candidate("current", PolicyAttention, BandActionable, protocol.ImpactNormal, 1, now)
	current.HoldMS = 30_000
	history := History{CurrentID: current.ID(), CurrentSince: now, LastShown: map[string]time.Time{current.ID(): now}}

	sameImpact := candidate("same", PolicyAttention, BandActionable, protocol.ImpactNormal, 2, now)
	decision := Select([]Candidate{current, sameImpact}, history, now.Add(time.Second))
	if decision.Candidate == nil || decision.Candidate.ID() != current.ID() {
		t.Fatalf("same-band peer interrupted hold: %#v", decision.Candidate)
	}

	higherImpact := candidate("higher", PolicyAttention, BandActionable, protocol.ImpactNotable, 3, now)
	decision = Select([]Candidate{current, higherImpact}, history, now.Add(time.Second))
	if decision.Candidate == nil || decision.Candidate.ID() != higherImpact.ID() {
		t.Fatalf("higher actionable impact did not interrupt: %#v", decision.Candidate)
	}
}

func TestCandidateValidate(t *testing.T) {
	valid := candidate("valid", PolicyRotation, BandRotation, protocol.ImpactNormal, 1, time.Now())
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid candidate: %v", err)
	}
	invalid := valid
	invalid.Scene.Elements = append(invalid.Scene.Elements, invalid.Scene.Elements[0])
	if err := invalid.Validate(); err == nil {
		t.Fatal("duplicate element unexpectedly validated")
	}
	invalid = candidate("invalid-display", PolicyRotation, BandRotation, protocol.ImpactNormal, 1, valid.UpdatedAt)
	invalid.Scene.Elements[0].Display = protocol.Display("side")
	if err := invalid.Validate(); err == nil {
		t.Fatal("candidate accepted a scene rejected by the canonical protocol validator")
	}
	invalid = candidate("invalid-audio", PolicyRotation, BandRotation, protocol.ImpactNormal, 1, valid.UpdatedAt)
	invalid.ExpiresAt = valid.UpdatedAt.Add(time.Minute)
	invalid.AudioCue = &protocol.AudioCue{
		ID: "alert", Asset: protocol.AssetRef{PackagePath: "INVALID ASSET"},
		ExpiresAt: valid.UpdatedAt.Add(30 * time.Second),
	}
	if err := invalid.Validate(); err == nil {
		t.Fatal("candidate accepted an audio cue rejected by the canonical protocol validator")
	}
}

func TestCandidateValidateRejectsPolicyBandMismatch(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		policy Policy
		band   Band
		impact protocol.Impact
	}{
		{name: "critical attention as ordinary actionable", policy: PolicyAttention, band: BandActionable, impact: protocol.ImpactCritical},
		{name: "ordinary attention as critical", policy: PolicyAttention, band: BandCriticalActionable, impact: protocol.ImpactNormal},
		{name: "interactive as actionable", policy: PolicyInteractive, band: BandActionable, impact: protocol.ImpactNormal},
		{name: "relevant as rotation", policy: PolicyWhenRelevant, band: BandRotation, impact: protocol.ImpactNotable},
		{name: "rotation as critical", policy: PolicyRotation, band: BandCriticalActionable, impact: protocol.ImpactCritical},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := candidate("mismatch", test.policy, test.band, test.impact, 1, now)
			if err := value.Validate(); err == nil {
				t.Fatal("semantically impossible candidate unexpectedly validated")
			}
		})
	}
}

func TestCandidateValidateAcceptsNativeBusyTimerInsteadOfScene(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	value := candidate("meeting", PolicyWhenRelevant, BandRelevant, protocol.ImpactNormal, 1, now)
	value.Scene = Scene{}
	value.BusyTimer = &protocol.BusyTimerPresentation{Theme: "meeting"}
	value.ExpiresAt = now.Add(30 * time.Minute)
	if err := value.Validate(); err != nil {
		t.Fatalf("valid busy timer candidate: %v", err)
	}

	value.Scene = Scene{Elements: []Element{{ID: "text", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: "duplicate presentation"}}}}
	if err := value.Validate(); err == nil {
		t.Fatal("candidate with both scene and busy timer was accepted")
	}
}

func candidate(key string, policy Policy, band Band, impact protocol.Impact, admission uint64, now time.Time) Candidate {
	return Candidate{
		PluginID: "dev.bsbctl.test", InstanceID: "test", Channel: "main", Key: key,
		Revision: 1, Generation: 1, Policy: policy, Band: band, Impact: impact, AdmissionSequence: admission,
		CreatedAt: now, UpdatedAt: now,
		Scene: Scene{Elements: []Element{{ID: "text", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: key, Font: "normal"}}}},
	}
}
