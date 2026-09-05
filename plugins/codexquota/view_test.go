package codexquota

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestQuotaSceneFocusesOneWindowWithStaticCodexMarkAndComprehensiveBack(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.Local)
	config := defaultConfig("/tmp/codex")
	config.Label = "MAIN"
	config.Badge = "M"
	config.ShowBadge = true
	snapshot := Snapshot{UpdatedAt: now, Windows: []Window{
		{Duration: 5 * time.Hour, RemainingPercent: 62, ResetsAt: now.Add(2*time.Hour + 14*time.Minute)},
		{Duration: 7 * 24 * time.Hour, RemainingPercent: 24, ResetsAt: now.Add(6 * 24 * time.Hour)},
	}}
	scene := quotaScene(snapshot, snapshot.Windows[0], config, signalNone)
	assertQuotaScene(t, scene)
	assertQuotaText(t, scene, "front-window-label", "M 5H")
	assertQuotaText(t, scene, "front-window-state", "LEFT")
	assertQuotaText(t, scene, "front-window-value", "62%")
	value := quotaElement(t, scene, "front-window-value")
	if value.X != 70 || value.Text == nil || value.Text.Align != "top_right" {
		t.Fatalf("front value geometry = %#v, want one-pixel right inset", value)
	}
	logo := quotaElement(t, scene, "front-codex-mark")
	if logo.Image == nil || logo.Image.Asset.PackagePath != "assets/codex-mark.png" || logo.Image.Asset.StockName != "" {
		t.Fatalf("front logo = %#v, want one static logical image", logo)
	}
	if logo.X != 1 || logo.Y != 1 {
		t.Fatalf("front logo position = %v/%v, want 1/1", logo.X, logo.Y)
	}
	if quotaHasElement(scene, "front-window-border") {
		t.Fatalf("front scene retained a progress frame: %s", quotaIDs(scene))
	}
	track := quotaElement(t, scene, "front-window-track")
	if track.X != 35 || track.Y != 11 || track.Rectangle == nil || track.Rectangle.Width != 36 || track.Rectangle.Height != 4 || track.Rectangle.Color != border {
		t.Fatalf("front capacity track = %#v", track)
	}
	bar := quotaElement(t, scene, "front-window-fill")
	if bar.X != 35 || bar.Y != 11 || bar.Rectangle == nil || bar.Rectangle.Width != 22 || bar.Rectangle.Height != 4 {
		t.Fatalf("front progress = %#v", bar)
	}
	percentageCount := 0
	for _, element := range scene.Elements {
		if element.Display == protocol.DisplayFront && element.Text != nil && strings.Contains(element.Text.Value, "%") {
			percentageCount++
		}
	}
	if percentageCount != 1 {
		t.Fatalf("front percentage elements = %d, want 1", percentageCount)
	}
	if strings.Contains(quotaIDs(scene), "front-window-1") {
		t.Fatalf("focused scene contains a second front row: %s", quotaIDs(scene))
	}
	assertQuotaText(t, scene, "back-title", "CODEX QUOTA: MAIN")
	assertQuotaText(t, scene, "back-window-0-label", "5 HOURS")
	assertQuotaText(t, scene, "back-window-0-value", "62% LEFT")
	assertQuotaText(t, scene, "back-window-1-label", "WEEKLY")
	wantResetText := []string{"RESET IN", "RESET IN 6D"}
	for index, window := range snapshot.Windows {
		prefix := "back-window-" + strconv.Itoa(index)
		assertQuotaText(t, scene, prefix+"-reset", wantResetText[index])
		countdown := quotaElement(t, scene, prefix+"-reset-countdown")
		if countdown.Countdown == nil || countdown.Countdown.EndsAtUnixSeconds != window.ResetsAt.Unix() || countdown.Countdown.ShowHours != protocol.CountdownShowHoursWhenNonZero {
			t.Fatalf("%s reset countdown = %#v", prefix, countdown)
		}
		if index == 0 && countdown.Countdown.Color == canvas || index == 1 && countdown.Countdown.Color != canvas {
			t.Fatalf("%s reset countdown visibility color = %q", prefix, countdown.Countdown.Color)
		}
	}
	for _, id := range []string{"back-window-0-value", "back-window-1-value"} {
		value := quotaElement(t, scene, id)
		if value.X != 142 || value.Text == nil || value.Text.Align != "top_right" {
			t.Fatalf("%s geometry = %#v, want right edge before status rail", id, value)
		}
	}
	for _, id := range []string{"back-window-0-border", "back-window-0-track", "back-window-1-border", "back-window-1-track"} {
		bar := quotaElement(t, scene, id)
		if bar.Rectangle == nil || bar.X+bar.Rectangle.Width > 144 {
			t.Fatalf("%s extends into status rail: %#v", id, bar)
		}
	}
	if got := quotaElement(t, scene, "front-window-fill").Rectangle.Color; got != signature {
		t.Fatalf("normal fill = %q", got)
	}
	if got := quotaElement(t, scene, "front-background").Rectangle.Color; got != canvas {
		t.Fatalf("background = %q", got)
	}
}

func TestQuotaSceneMarksCriticalPressureWithoutColorOnly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	config := defaultConfig("/tmp/codex")
	snapshot := Snapshot{UpdatedAt: now, Windows: []Window{{
		Duration: 30 * 24 * time.Hour, RemainingPercent: 5, ResetsAt: now.Add(time.Hour),
	}}}
	scene := quotaScene(snapshot, snapshot.Windows[0], config, signalCritical)
	assertQuotaScene(t, scene)
	assertQuotaText(t, scene, "front-window-label", "1M")
	assertQuotaText(t, scene, "front-window-state", "CRIT")
	assertQuotaText(t, scene, "front-window-value", "5%!")
	if got := quotaElement(t, scene, "front-window-fill").Rectangle.Color; got != danger {
		t.Fatalf("critical fill = %q", got)
	}
}

func TestQuotaSceneUsesWarningAndFullValueContracts(t *testing.T) {
	t.Parallel()
	now := time.Now()
	config := defaultConfig("/tmp/codex")
	snapshot := Snapshot{UpdatedAt: now, Windows: []Window{
		{Duration: 5 * time.Hour, RemainingPercent: 100, ResetsAt: now.Add(time.Hour)},
		{Duration: 7 * 24 * time.Hour, RemainingPercent: 20, ResetsAt: now.Add(time.Hour)},
	}}
	scene := quotaScene(snapshot, snapshot.Windows[1], config, signalLow)
	assertQuotaText(t, scene, "front-window-label", "1W")
	assertQuotaText(t, scene, "front-window-state", "LOW")
	assertQuotaText(t, scene, "front-window-value", "20%")
	if got := quotaElement(t, scene, "front-window-fill").Rectangle.Color; got != warning {
		t.Fatalf("warning fill = %q", got)
	}
}

func TestQuotaSceneAlwaysIncludesFrontAndBackFillElements(t *testing.T) {
	t.Parallel()
	config := defaultConfig("/tmp/codex")
	now := time.Now()
	for _, remaining := range []int{0, 1, 99, 100} {
		windows := []Window{
			{Duration: 5 * time.Hour, RemainingPercent: remaining, ResetsAt: now.Add(time.Hour)},
			{Duration: 7 * 24 * time.Hour, RemainingPercent: remaining, ResetsAt: now.Add(time.Hour)},
		}
		scene := quotaScene(Snapshot{Windows: windows, UpdatedAt: now}, windows[0], config, signalNone)
		for _, id := range []string{"front-window-fill", "back-window-0-fill", "back-window-1-fill"} {
			if !quotaHasElement(scene, id) {
				t.Fatalf("remaining=%d omitted %s: %s", remaining, id, quotaIDs(scene))
			}
		}
	}
}

func TestQuotaLiveStatusScenesKeepStableTopologyAndUseTraceStatusColors(t *testing.T) {
	waiting := quotaStatusScene("WAITING", quotaWaitingColor)
	unavailable := quotaStatusScene("UNAVAILABLE", quotaUnavailableColor)
	if err := waiting.Validate(); err != nil {
		t.Fatalf("waiting status scene is invalid: %v", err)
	}
	if err := unavailable.Validate(); err != nil {
		t.Fatalf("unavailable status scene is invalid: %v", err)
	}
	if quotaIDs(waiting) != quotaIDs(unavailable) {
		t.Fatalf("status topology changed: waiting=%s unavailable=%s", quotaIDs(waiting), quotaIDs(unavailable))
	}
	for _, test := range []struct {
		scene protocol.Scene
		text  string
		color string
	}{
		{scene: waiting, text: "WAITING", color: "#F2B84BFF"},
		{scene: unavailable, text: "UNAVAILABLE", color: "#FF786FFF"},
	} {
		if got := quotaElement(t, test.scene, "front-status").Text.Value; got != test.text {
			t.Fatalf("front status = %q, want %q", got, test.text)
		}
		if got := quotaElement(t, test.scene, "front-status").Text.Color; got != test.color {
			t.Fatalf("front status color = %q, want %q", got, test.color)
		}
		if got := quotaElement(t, test.scene, "back-title").Text.Color; got != "#EAF4F2FF" {
			t.Fatalf("back title color = %q, want Trace primary text", got)
		}
		if got := quotaElement(t, test.scene, "back-background").Rectangle.Color; got != "#071522FF" {
			t.Fatalf("back background color = %q, want Trace canvas", got)
		}
		if got := quotaElement(t, test.scene, "back-accent").Rectangle.Color; got != test.color {
			t.Fatalf("back accent color = %q, want %q", got, test.color)
		}
	}
}

func assertQuotaScene(t *testing.T, scene presentation.Scene) {
	t.Helper()
	if len(scene.Elements) == 0 || len(scene.Elements) > 64 {
		t.Fatalf("element count = %d", len(scene.Elements))
	}
	seen := map[string]bool{}
	for _, element := range scene.Elements {
		if seen[element.ID] {
			t.Fatalf("duplicate element %q", element.ID)
		}
		seen[element.ID] = true
		if quotaElementKind(element) == "" {
			t.Fatalf("element %q has no v1 variant", element.ID)
		}
		if element.Image != nil && element.Image.Asset.PackagePath == "" {
			t.Fatalf("element %q has no logical asset", element.ID)
		}
		if element.Display != "front" && element.Display != "back" {
			t.Fatalf("element %q display = %q", element.ID, element.Display)
		}
	}
	for _, id := range []string{"front-window-label", "front-window-value"} {
		if got := quotaElement(t, scene, id).Text.Font; got != "normal" {
			t.Fatalf("front text %q font = %q, want normal", id, got)
		}
	}
	if state := quotaElement(t, scene, "front-window-state"); state.Text == nil || state.Text.Font != "tiny" || state.X != 18 || state.Y != 10 {
		t.Fatalf("front state = %#v", state)
	}
	if err := (presentation.Candidate{
		PluginID: PluginID, InstanceID: AppID, Channel: ChannelSummary, Key: observationKey,
		Revision: 1, Generation: 1, AdmissionSequence: 1, Policy: presentation.PolicyRotation, Band: presentation.BandRotation, Impact: protocol.ImpactNormal,
		CreatedAt: time.Now(), UpdatedAt: time.Now(), Scene: scene,
	}).Validate(); err != nil {
		t.Fatalf("scene validation: %v", err)
	}
}

func assertQuotaText(t *testing.T, scene presentation.Scene, id, want string) {
	t.Helper()
	got := quotaElement(t, scene, id)
	if got.Text == nil || got.Text.Value != want {
		t.Fatalf("%s text = %#v, want %q", id, got.Text, want)
	}
}

func quotaElementKind(element presentation.Element) string {
	switch {
	case element.Text != nil:
		return "text"
	case element.Image != nil:
		return "image"
	case element.Animation != nil:
		return "animation"
	case element.Rectangle != nil:
		return "rectangle"
	case element.Countdown != nil:
		return "countdown"
	default:
		return ""
	}
}

func quotaElement(t *testing.T, scene presentation.Scene, id string) presentation.Element {
	t.Helper()
	for _, element := range scene.Elements {
		if element.ID == id {
			return element
		}
	}
	t.Fatalf("missing element %q; ids include %s", id, quotaIDs(scene))
	return presentation.Element{}
}

func quotaHasElement(scene presentation.Scene, id string) bool {
	for _, element := range scene.Elements {
		if element.ID == id {
			return true
		}
	}
	return false
}

func quotaIDs(scene presentation.Scene) string {
	values := make([]string, 0, len(scene.Elements))
	for _, element := range scene.Elements {
		values = append(values, element.ID)
	}
	return strings.Join(values, ",")
}
