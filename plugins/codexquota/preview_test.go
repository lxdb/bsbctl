//go:build preview

package codexquota

import (
	"testing"
	"time"
)

func TestPreviewScenesUseOnlyNormalAndCriticalMockQuota(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	scenes := PreviewScenes(now)
	if len(scenes) != 2 {
		t.Fatalf("preview scenes = %d, want normal and critical quota", len(scenes))
	}

	for index, want := range []map[string]string{
		{
			"front-window-label": "1W",
			"front-window-value": "72%",
			"front-window-state": "LEFT",
			"back-title":         "CODEX QUOTA: MOCK",
		},
		{
			"front-window-label": "5H",
			"front-window-value": "5%!",
			"front-window-state": "CRIT",
			"back-title":         "CODEX QUOTA: MOCK",
		},
	} {
		for _, element := range scenes[index].Elements {
			if expected, ok := want[element.ID]; ok {
				if element.Text == nil || element.Text.Value != expected {
					t.Fatalf("scene %d %s = %#v, want %q", index, element.ID, element.Text, expected)
				}
				delete(want, element.ID)
			}
		}
		if len(want) != 0 {
			t.Fatalf("scene %d missing mock quota text elements: %v", index, want)
		}
	}
}
