//go:build preview

package slack

import (
	"testing"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestPreviewScenesUseProductionBounds(t *testing.T) {
	scenes := PreviewScenes(fixtureNow)
	if len(scenes) != 10 {
		t.Fatalf("preview count=%d", len(scenes))
	}
	for _, s := range scenes {
		assertSceneBounds(t, s)
	}
	assertSlackPreviewText(t, scenes[2], "front-label", "Mentioned")
	assertSlackPreviewText(t, scenes[2], "front-context", "BUILD | START OPEN")
	assertSlackPreviewText(t, scenes[3], "front-label", "Direct message")
	assertSlackPreviewText(t, scenes[3], "front-context", "DIRECT | START OPEN")
	assertSlackPreviewText(t, scenes[4], "front-label", "Direct message: Release checklist is ready")
	assertSlackPreviewText(t, scenes[4], "front-context", "DIRECT | START OPEN")
	assertSlackPreviewText(t, scenes[5], "back-position", "ITEM 1 OF 2 / TURN SELECT")
}

func assertSlackPreviewText(t *testing.T, scene protocol.Scene, id, want string) {
	t.Helper()
	for _, element := range scene.Elements {
		if element.ID == id && element.Text != nil {
			if element.Text.Value != want {
				t.Fatalf("%s = %q, want %q", id, element.Text.Value, want)
			}
			return
		}
	}
	t.Fatalf("scene is missing %s", id)
}
