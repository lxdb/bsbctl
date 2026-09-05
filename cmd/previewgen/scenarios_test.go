//go:build preview

package main

import (
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
	busylib "github.com/lxdb/busylib-go"
)

func TestScenariosCompileTheProductionScenesWithMockAssets(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	scenarios, err := previewScenarios(now)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name           string
		file           string
		capture        bool
		sampleInterval time.Duration
		durations      []time.Duration
	}{
		{name: "Calendar", file: "calendar-front.gif", capture: true, sampleInterval: 250 * time.Millisecond, durations: []time.Duration{6 * time.Second, 6 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}},
		{name: "Codex", file: "codex-front.gif", capture: true, sampleInterval: 300 * time.Millisecond, durations: []time.Duration{
			6 * time.Second, 6 * time.Second, 6 * time.Second, 6 * time.Second, 6 * time.Second,
			6 * time.Second, 6 * time.Second, 6 * time.Second, 6 * time.Second, 6 * time.Second,
			6 * time.Second, 6 * time.Second, 6 * time.Second, 6 * time.Second, 6 * time.Second,
		}},
		{name: "Codex quota", file: "codex-quota-front.gif", capture: true, sampleInterval: 250 * time.Millisecond, durations: []time.Duration{2 * time.Second, 2 * time.Second}},
		{name: "Mac resources", file: "mac-resources-front.gif", sampleInterval: 250 * time.Millisecond, durations: []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second}},
	}
	if len(scenarios) != len(want) {
		t.Fatalf("scenarios = %d, want %d", len(scenarios), len(want))
	}
	for index, expected := range want {
		scenario := scenarios[index]
		if scenario.Name != expected.name || scenario.File != expected.file || scenario.Capture != expected.capture || scenario.SampleInterval != expected.sampleInterval || len(scenario.Steps) != len(expected.durations) {
			t.Fatalf("scenario %d = %#v, want %#v", index, scenario, expected)
		}
		var duration time.Duration
		for stepIndex, step := range scenario.Steps {
			if step.Duration != expected.durations[stepIndex] {
				t.Fatalf("%s step %d duration = %v, want %v", scenario.Name, stepIndex, step.Duration, expected.durations[stepIndex])
			}
			duration += step.Duration
			if step.Draw.ApplicationName != previewApplication || step.Draw.Priority != 100 {
				t.Fatalf("%s draw ownership = %q/%d", scenario.Name, step.Draw.ApplicationName, step.Draw.Priority)
			}
			for _, element := range step.Draw.Elements {
				if imageElement, ok := element.(busylib.ImageElement); ok && imageElement.Path == previewAssetPath && imageElement.StockPath != "" {
					t.Fatalf("%s mock mark has competing stock path", scenario.Name)
				}
			}
			wantBackground := "#071522FF"
			if scenario.Name == "Calendar" {
				wantBackground = "#000000FF"
			}
			background, ok := step.Draw.Elements[0].(busylib.RectangleElement)
			if !ok || background.ID != "preview-front-background" || background.Width != 72 || background.Height != 16 || len(background.FillColors) != 1 || background.FillColors[0] != wantBackground {
				t.Fatalf("%s preview background = %#v, want full front canvas %s", scenario.Name, step.Draw.Elements[0], wantBackground)
			}
		}
		if scenario.Duration != duration {
			t.Fatalf("%s duration = %v, want step total %v", scenario.Name, scenario.Duration, duration)
		}
	}
}

func TestCompilePreviewSceneRejectsAnUnreviewedPackageAsset(t *testing.T) {
	t.Parallel()
	scene := protocol.Scene{Elements: []protocol.Element{{
		ID: "unexpected", Display: protocol.DisplayFront,
		Image: &protocol.ImageElement{Asset: protocol.AssetRef{PackagePath: "assets/private.png"}},
	}}}

	if _, err := compilePreviewScene(scene, "#071522FF"); err == nil {
		t.Fatal("unreviewed package asset was mapped to the preview Codex mark")
	}
}
