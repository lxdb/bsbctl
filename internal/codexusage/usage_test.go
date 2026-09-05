package codexusage

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestNormalizeWindowsClampsSortsAndRejectsIncompleteData(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	snapshot, err := NormalizeWindows([]RawWindow{
		{UsedPercent: 120, Duration: 7 * 24 * time.Hour, ResetsAt: now.Add(7 * 24 * time.Hour)},
		{UsedPercent: -5, Duration: 5 * time.Hour, ResetsAt: now.Add(5 * time.Hour)},
		{UsedPercent: 50},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Windows) != 2 || snapshot.Windows[0].Duration != 5*time.Hour || snapshot.Windows[0].RemainingPercent != 100 ||
		snapshot.Windows[1].Duration != 7*24*time.Hour || snapshot.Windows[1].RemainingPercent != 0 {
		t.Fatalf("normalized windows = %#v", snapshot.Windows)
	}
	if _, err := NormalizeWindows([]RawWindow{{UsedPercent: 50}}, now); err == nil {
		t.Fatal("incomplete windows were accepted")
	}
}

func TestNormalizeWindowsPreservesUsableWindowsWithoutResetTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	snapshot, err := NormalizeWindows([]RawWindow{
		{UsedPercent: 25, Duration: 5 * time.Hour},
		{UsedPercent: 50, Duration: 7 * 24 * time.Hour, ResetsAt: time.Unix(-1, 0)},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Windows) != 2 {
		t.Fatalf("normalized windows = %#v, want both usable durations", snapshot.Windows)
	}
	for _, window := range snapshot.Windows {
		if !window.ResetsAt.IsZero() {
			t.Fatalf("missing reset became %v, want zero time", window.ResetsAt)
		}
	}
}

func elementByID(scene protocol.Scene, id string) (protocol.Element, bool) {
	for _, element := range scene.Elements {
		if element.ID == id {
			return element, true
		}
	}
	return protocol.Element{}, false
}

func TestSceneUsesCallerAssetAndMatchesQuotaGeometry(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	snapshot := Snapshot{UpdatedAt: now, Windows: []Window{
		{UsedPercent: 13, RemainingPercent: 87, Duration: 5 * time.Hour, ResetsAt: now.Add(4 * time.Hour)},
		{UsedPercent: 41, RemainingPercent: 59, Duration: 7 * 24 * time.Hour, ResetsAt: now.Add(6 * 24 * time.Hour)},
	}}
	scene := Scene(snapshot, snapshot.Windows[1], PresentationConfig{Label: "MAIN", WarningRemainingPercent: 20, CriticalRemainingPercent: 5}, SignalNone, "assets/codex-mark.png")
	mark, ok := elementByID(scene, "front-codex-mark")
	if !ok || mark.Image == nil || mark.Image.Asset.PackagePath != "assets/codex-mark.png" || mark.Image.Asset.StockName != "" || mark.X != 1 || mark.Y != 1 {
		t.Fatalf("static mark = %#v/%v", mark, ok)
	}
	value, ok := elementByID(scene, "front-window-value")
	if !ok || value.Text == nil || value.Text.Value != "59%" || value.X != 70 || value.Y != 1 {
		t.Fatalf("front quota value = %#v/%v", value, ok)
	}
	label, _ := elementByID(scene, "front-window-label")
	if label.X != 18 || label.Y != 1 {
		t.Fatalf("front quota label = %#v, want (18,1)", label)
	}
	title, _ := elementByID(scene, "back-title")
	if title.X != 74 {
		t.Fatalf("back title anchor = %d, want 74", title.X)
	}
	assertQuotaSafeMargins(t, scene)
	if got := SignalFor(snapshot, PresentationConfig{WarningRemainingPercent: 60, CriticalRemainingPercent: 5}); got != SignalLow {
		t.Fatalf("quota signal = %v, want low", got)
	}
}

func TestSingleWindowSceneKeepsResetCountdownBeforeFirmwareRail(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	window := Window{RemainingPercent: 75, Duration: 5 * time.Hour, ResetsAt: now.Add(time.Hour)}
	scene := Scene(Snapshot{UpdatedAt: now, Windows: []Window{window}}, window, PresentationConfig{Label: "MAIN"}, SignalNone, "codex-mark")
	countdown, _ := elementByID(scene, "back-window-0-reset-countdown")
	if countdown.X != 142 {
		t.Fatalf("single-window countdown anchor = %d, want 142", countdown.X)
	}
	assertQuotaSafeMargins(t, scene)
}

func TestSceneUsesNativeCountdownAndDeterministicResetFallbacks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	config := PresentationConfig{Label: "MAIN", WarningRemainingPercent: 20, CriticalRemainingPercent: 5}
	tests := []struct {
		name          string
		reset         time.Time
		wantText      string
		wantTimestamp string
		wantColor     string
	}{
		{name: "future", reset: now.Add(90 * time.Minute), wantText: "RESET IN", wantTimestamp: strconv.FormatInt(now.Add(90*time.Minute).Unix(), 10), wantColor: SecondaryColor},
		{name: "equal", reset: now, wantText: "RESETTING", wantTimestamp: strconv.FormatInt(now.Unix(), 10), wantColor: CanvasColor},
		{name: "expired", reset: now.Add(-time.Second), wantText: "RESETTING", wantTimestamp: strconv.FormatInt(now.Add(-time.Second).Unix(), 10), wantColor: CanvasColor},
		{name: "missing", wantText: "RESET UNKNOWN", wantTimestamp: "1", wantColor: CanvasColor},
	}
	var wantTopology []string
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			window := Window{Duration: 5 * time.Hour, RemainingPercent: 75, ResetsAt: test.reset}
			scene := Scene(Snapshot{UpdatedAt: now, Windows: []Window{window}}, window, config, SignalNone, "codex-mark")
			label, ok := elementByID(scene, "back-window-0-reset")
			if !ok || label.Text == nil || label.Text.Value != test.wantText {
				t.Fatalf("reset label = %#v/%v, want %q", label, ok, test.wantText)
			}
			countdown, ok := elementByID(scene, "back-window-0-reset-countdown")
			if !ok || countdown.Countdown == nil || strconv.FormatInt(countdown.Countdown.EndsAtUnixSeconds, 10) != test.wantTimestamp || countdown.Countdown.ShowHours != protocol.CountdownShowHoursWhenNonZero || countdown.Countdown.Color != test.wantColor {
				t.Fatalf("reset countdown = %#v/%v", countdown, ok)
			}
			topology := quotaSceneTopology(scene)
			if wantTopology == nil {
				wantTopology = topology
			} else if !slices.Equal(topology, wantTopology) {
				t.Fatalf("reset topology = %v, want %v", topology, wantTopology)
			}
		})
	}
}

func TestSceneUsesNativeCountdownOnlyBelowSixtyHours(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	config := PresentationConfig{Label: "MAIN", WarningRemainingPercent: 20, CriticalRemainingPercent: 5}
	tests := []struct {
		name      string
		remaining time.Duration
		wantText  string
		wantColor string
	}{
		{name: "below boundary", remaining: 59*time.Hour + 59*time.Minute, wantText: "RESET IN", wantColor: SecondaryColor},
		{name: "at boundary", remaining: 60 * time.Hour, wantText: "RESET IN 2D 12H", wantColor: CanvasColor},
		{name: "above boundary exact days", remaining: 72 * time.Hour, wantText: "RESET IN 3D", wantColor: CanvasColor},
	}
	var wantTopology []string
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reset := now.Add(test.remaining)
			window := Window{Duration: 7 * 24 * time.Hour, RemainingPercent: 75, ResetsAt: reset}
			scene := Scene(Snapshot{UpdatedAt: now, Windows: []Window{window}}, window, config, SignalNone, "codex-mark")
			label, _ := elementByID(scene, "back-window-0-reset")
			countdown, _ := elementByID(scene, "back-window-0-reset-countdown")
			if label.Text.Value != test.wantText || countdown.Countdown.EndsAtUnixSeconds != reset.Unix() || countdown.Countdown.Color != test.wantColor {
				t.Fatalf("remaining=%v reset row = %#v / %#v", test.remaining, label, countdown)
			}
			topology := quotaSceneTopology(scene)
			if wantTopology == nil {
				wantTopology = topology
			} else if !slices.Equal(topology, wantTopology) {
				t.Fatalf("remaining=%v topology = %v, want %v", test.remaining, topology, wantTopology)
			}
		})
	}
}

func TestSceneKeepsTwoWindowResetTopologyStable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	config := PresentationConfig{Label: "MAIN", WarningRemainingPercent: 20, CriticalRemainingPercent: 5}
	var wantTopology []string
	for _, reset := range []time.Time{now.Add(time.Hour), now, now.Add(-time.Second), {}} {
		windows := []Window{
			{Duration: 5 * time.Hour, RemainingPercent: 75, ResetsAt: reset},
			{Duration: 7 * 24 * time.Hour, RemainingPercent: 50, ResetsAt: reset},
		}
		topology := quotaSceneTopology(Scene(Snapshot{UpdatedAt: now, Windows: windows}, windows[0], config, SignalNone, "codex-mark"))
		if wantTopology == nil {
			wantTopology = topology
		} else if !slices.Equal(topology, wantTopology) {
			t.Fatalf("reset=%v topology = %v, want %v", reset, topology, wantTopology)
		}
	}
}

func quotaSceneTopology(scene protocol.Scene) []string {
	result := make([]string, 0, len(scene.Elements))
	for _, element := range scene.Elements {
		result = append(result, element.ID+"/"+elementKind(element)+"/"+string(element.Display))
	}
	return result
}

func elementKind(element protocol.Element) string {
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

func assertQuotaSafeMargins(t *testing.T, scene protocol.Scene) {
	t.Helper()
	for _, element := range scene.Elements {
		if element.ID == "front-background" || element.ID == "back-background" {
			continue
		}
		maxX, maxY := 70, 14
		if element.Display == protocol.DisplayBack {
			maxX, maxY = 146, 78
		}
		if element.X < 1 || element.X > maxX || element.Y < 1 || element.Y > maxY {
			t.Fatalf("%s origin = (%d,%d), outside safe %s area", element.ID, element.X, element.Y, element.Display)
		}
		if element.Rectangle != nil && (element.X+element.Rectangle.Width-1 > maxX || element.Y+element.Rectangle.Height-1 > maxY) {
			t.Fatalf("%s rectangle reaches (%d,%d), outside safe %s area", element.ID, element.X+element.Rectangle.Width-1, element.Y+element.Rectangle.Height-1, element.Display)
		}
	}
}
