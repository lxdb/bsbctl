//go:build preview

package main

import (
	"path/filepath"
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
		{name: "GitHub notifications", file: "github-notifications-front.gif", capture: true, sampleInterval: 300 * time.Millisecond, durations: []time.Duration{30 * time.Second, 30 * time.Second}},
		{name: "Mac resources", file: "mac-resources-front.gif", sampleInterval: 250 * time.Millisecond, durations: []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second}},
		{name: "Slack", file: "slack-front.gif", capture: true, sampleInterval: 250 * time.Millisecond, durations: []time.Duration{2 * time.Second, 2 * time.Second, 8 * time.Second, 8 * time.Second, 18 * time.Second, 8 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}},
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
			switch scenario.Name {
			case "Calendar":
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

func TestEveryCompiledPackageImageHasACaptureAsset(t *testing.T) {
	scenarios, err := previewScenarios(time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	uploads := make(map[string]string, len(previewCaptureAssets))
	for _, asset := range previewCaptureAssets {
		uploads[asset.devicePath] = filepath.ToSlash(asset.sourcePath)
	}
	want := map[string]string{
		previewAssetPath:       "plugins/codex/assets/codex-mark.png",
		githubPreviewAssetPath: "plugins/githubnotifications/assets/github-mark.png",
		slackPreviewAssetPath:  "plugins/slack/assets/slack-mark.png",
	}
	for path, source := range want {
		if uploads[path] != source {
			t.Errorf("capture asset %q source = %q, want %q", path, uploads[path], source)
		}
	}
	for _, scenario := range scenarios {
		for _, step := range scenario.Steps {
			for _, element := range step.Draw.Elements {
				imageElement, ok := element.(busylib.ImageElement)
				if ok && imageElement.Path != "" && uploads[imageElement.Path] == "" {
					t.Errorf("%s image %q has no capture upload", scenario.Name, imageElement.Path)
				}
			}
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
