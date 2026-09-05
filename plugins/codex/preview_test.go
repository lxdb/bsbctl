//go:build preview

package codex

import (
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestPreviewScenesShowTheCodexFeatureTourWithProjectExamples(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	scenes, err := PreviewScenes(now)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		id    string
		value string
	}{
		{id: "front-state", value: "RUN"},
		{id: "front-state", value: "2 ACT"},
		{id: "front-state", value: "PIN"},
		{id: "front-state", value: "PLAN 1/3"},
		{id: "front-state", value: "PLAN READY"},
		{id: "front-state", value: "WAIT CMD"},
		{id: "front-state", value: "WAIT FILE"},
		{id: "front-state", value: "WAIT PERM"},
		{id: "front-state", value: "ASK"},
		{id: "front-option-label", value: "Codex and Calendar"},
		{id: "front-state", value: "COMPACT"},
		{id: "front-state", value: "COMPACTED"},
		{id: "front-state", value: "DONE"},
		{id: "front-state", value: "STOP"},
		{id: "front-state", value: "FAIL"},
	}
	if len(scenes) != len(want) {
		t.Fatalf("preview scenes = %d, want %d feature scenes", len(scenes), len(want))
	}
	for index, expected := range want {
		if got := previewText(scenes[index], expected.id); got != expected.value {
			t.Fatalf("scene %d %s = %q, want %q", index, expected.id, got, expected.value)
		}
	}

	if got := previewText(scenes[0], "back-session"); got != "Preview GIF refresh" {
		t.Fatalf("run session = %q, want project-related example", got)
	}
	if got := previewText(scenes[0], "back-workdir"); got != "bsbctl" {
		t.Fatalf("run workdir = %q, want production basename projection", got)
	}
	wantDetails := []string{
		"Active",
		"2 active",
		"Pinned focus",
		"Plan progress",
		"Ready in Codex",
		"Command approval",
		"File approval",
		"Permission approval",
		"START TO ANSWER",
		"",
		"Compacting context",
		"Context compacted",
		"Completed",
		"Interrupted",
		"Failed",
	}
	for index, want := range wantDetails {
		if got := previewText(scenes[index], "back-detail"); got != want {
			t.Fatalf("scene %d production detail = %q, want %q", index, got, want)
		}
	}
	if got := previewText(scenes[9], "back-question"); got != "Which previews should be refreshed?" {
		t.Fatalf("question = %q, want preview task question", got)
	}
	if got := previewText(scenes[9], "back-option-label"); got != "Codex and Calendar" {
		t.Fatalf("question option = %q, want project feature choice", got)
	}
	if got := previewText(scenes[9], "back-option-description"); got != "Update both feature tours" {
		t.Fatalf("question option description = %q, want concrete outcome", got)
	}
	if got := previewText(scenes[9], "front-option-position"); got != "OPTION 1/2" {
		t.Fatalf("front question option position = %q, want selected answer position", got)
	}
}

func previewText(scene protocol.Scene, id string) string {
	for _, element := range scene.Elements {
		if element.ID == id && element.Text != nil {
			return element.Text.Value
		}
	}
	return ""
}
