//go:build preview

package githubnotifications

import (
	"strings"
	"testing"
	"time"
)

func TestPreviewScenesPrioritizeAttentionStates(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	scenes := PreviewScenes(now)
	if len(scenes) != 2 {
		t.Fatalf("preview scenes = %d, want 2", len(scenes))
	}
	wantText := []string{"Review requested: GitHub Notifications release", "Mentioned: GitHub Notifications release"}
	for index, scene := range scenes {
		if err := scene.Validate(); err != nil {
			t.Fatalf("scene %d is invalid: %v", index, err)
		}
		var text strings.Builder
		for _, element := range scene.Elements {
			if element.Text != nil {
				text.WriteString(element.Text.Value)
				text.WriteByte('\n')
			}
		}
		if !strings.Contains(text.String(), wantText[index]) {
			t.Fatalf("scene %d text %q does not contain %q", index, text.String(), wantText[index])
		}
	}
}

func TestPreviewUsesOnlySyntheticPublicSubjects(t *testing.T) {
	scenes := PreviewScenes(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	for _, scene := range scenes {
		if err := scene.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if got := scenes[0].Elements[len(scenes[0].Elements)-1].Text.Value; got != "lxdb/bsbctl" {
		t.Fatalf("review-request repository = %q, want lxdb/bsbctl", got)
	}
	for index, scene := range scenes {
		if got := scene.Elements[1].Text.Value; !strings.HasSuffix(got, "GitHub Notifications release") {
			t.Fatalf("scene %d subject = %q, want the provider pull-request title", index, got)
		}
	}
}
