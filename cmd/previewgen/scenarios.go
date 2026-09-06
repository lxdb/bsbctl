//go:build preview

package main

import (
	"fmt"
	"slices"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/plugins/calendar"
	"github.com/lxdb/bsbctl/plugins/codex"
	"github.com/lxdb/bsbctl/plugins/codexquota"
	"github.com/lxdb/bsbctl/plugins/githubnotifications"
	"github.com/lxdb/bsbctl/plugins/macresources"
	"github.com/lxdb/bsbctl/plugins/slack"
	"github.com/lxdb/bsbctl/sdk/protocol"
	busylib "github.com/lxdb/busylib-go"
)

const (
	previewApplication       = "bsbctl-preview"
	previewAssetPath         = "codex-mark.png"
	previewAssetSource       = "assets/codex-mark.png"
	githubPreviewAssetPath   = "github-mark.png"
	githubPreviewAssetSource = "assets/github-mark.png"
	slackPreviewAssetPath    = "slack-mark.png"
	slackPreviewAssetSource  = "assets/slack-mark.png"
)

type previewStep struct {
	Duration time.Duration
	Draw     busylib.DisplayElements
}

type previewScenario struct {
	Name           string
	File           string
	Capture        bool
	SampleInterval time.Duration
	Steps          []previewStep
	Duration       time.Duration
}

var capturedPreviewArtifactNames = []string{
	"calendar-front.gif",
	"codex-front.gif",
	"codex-quota-front.gif",
	"github-notifications-front.gif",
	"slack-front.gif",
}

func isCapturedPreviewArtifact(name string) bool {
	return slices.Contains(capturedPreviewArtifactNames, name)
}

func previewScenarios(now time.Time) ([]previewScenario, error) {
	codexScenes, err := codex.PreviewScenes(now)
	if err != nil {
		return nil, fmt.Errorf("build Codex mock scenes: %w", err)
	}
	definitions := []struct {
		name           string
		file           string
		capture        bool
		sampleInterval time.Duration
		scenes         []protocol.Scene
		durations      []time.Duration
	}{
		{name: "Calendar", file: "calendar-front.gif", capture: true, sampleInterval: 250 * time.Millisecond, scenes: calendar.PreviewScenes(now), durations: []time.Duration{6 * time.Second, 6 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}},
		{name: "Codex", file: "codex-front.gif", capture: true, sampleInterval: 300 * time.Millisecond, scenes: codexScenes, durations: repeatedDurations(len(codexScenes), 6*time.Second)},
		{name: "Codex quota", file: "codex-quota-front.gif", capture: true, sampleInterval: 250 * time.Millisecond, scenes: codexquota.PreviewScenes(now), durations: []time.Duration{2 * time.Second, 2 * time.Second}},
		{name: "GitHub notifications", file: "github-notifications-front.gif", capture: true, sampleInterval: 300 * time.Millisecond, scenes: githubnotifications.PreviewScenes(now), durations: []time.Duration{30 * time.Second, 30 * time.Second}},
		{name: "Mac resources", file: "mac-resources-front.gif", sampleInterval: 250 * time.Millisecond, scenes: macresources.PreviewScenes(), durations: []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second}},
		{name: "Slack", file: "slack-front.gif", capture: true, sampleInterval: 250 * time.Millisecond, scenes: slack.PreviewScenes(now), durations: []time.Duration{2 * time.Second, 2 * time.Second, 8 * time.Second, 8 * time.Second, 18 * time.Second, 8 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}},
	}
	result := make([]previewScenario, 0, len(definitions))
	for _, definition := range definitions {
		if len(definition.scenes) != len(definition.durations) {
			return nil, fmt.Errorf("%s preview scene duration set is incomplete", definition.name)
		}
		steps := make([]previewStep, 0, len(definition.scenes))
		total := time.Duration(0)
		for index, scene := range definition.scenes {
			background := "#071522FF"
			switch definition.name {
			case "Calendar":
				background = "#000000FF"
			}
			draw, err := compilePreviewScene(scene, background)
			if err != nil {
				return nil, fmt.Errorf("compile %s mock scene: %w", definition.name, err)
			}
			duration := definition.durations[index]
			if duration <= 0 || duration%(10*time.Millisecond) != 0 {
				return nil, fmt.Errorf("%s preview scene %d duration is invalid", definition.name, index)
			}
			steps = append(steps, previewStep{Duration: duration, Draw: draw})
			total += duration
		}
		result = append(result, previewScenario{
			Name: definition.name, File: definition.file, Capture: definition.capture,
			SampleInterval: definition.sampleInterval, Steps: steps, Duration: total,
		})
	}
	return result, nil
}

func repeatedDurations(count int, duration time.Duration) []time.Duration {
	result := make([]time.Duration, count)
	for index := range result {
		result[index] = duration
	}
	return result
}

func compilePreviewScene(scene protocol.Scene, background string) (busylib.DisplayElements, error) {
	resolved := presentation.ResolveScene(scene)
	resolved.Elements = append([]presentation.ResolvedElement{{Element: protocol.Element{
		ID:      "preview-front-background",
		Display: protocol.DisplayFront,
		X:       0,
		Y:       0,
		Rectangle: &protocol.RectangleElement{
			Width:  72,
			Height: 16,
			Color:  background,
		},
	}}}, resolved.Elements...)
	for index := range resolved.Elements {
		element := &resolved.Elements[index]
		switch {
		case element.Image != nil:
			if element.Image.Asset.StockName == "" {
				switch element.Image.Asset.PackagePath {
				case previewAssetSource:
					element.Path = previewAssetPath
				case githubPreviewAssetSource:
					element.Path = githubPreviewAssetPath
				case slackPreviewAssetSource:
					element.Path = slackPreviewAssetPath
				default:
					return busylib.DisplayElements{}, fmt.Errorf("unsupported preview package asset %q", element.Image.Asset.PackagePath)
				}
			} else {
				element.Path = assets.StockPath(element.Image.Asset.StockName, "image")
			}
		case element.Animation != nil:
			if element.Animation.Asset.StockName == "" {
				if element.Animation.Asset.PackagePath != previewAssetSource {
					return busylib.DisplayElements{}, fmt.Errorf("unsupported preview package asset %q", element.Animation.Asset.PackagePath)
				}
				element.Path = previewAssetPath
			} else {
				element.Path = assets.StockPath(element.Animation.Asset.StockName, "animation")
			}
		}
	}
	return presentation.CompileScene(previewApplication, 100, resolved)
}
