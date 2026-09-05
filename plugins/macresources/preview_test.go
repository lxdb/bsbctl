//go:build preview

package macresources

import (
	"reflect"
	"testing"
)

func TestPreviewScenesUseOnlyFixedMockReadings(t *testing.T) {
	t.Parallel()
	scenes := PreviewScenes()
	if len(scenes) != 3 {
		t.Fatalf("preview scenes = %d, want 3", len(scenes))
	}
	want := []map[string]string{
		{"front-cpu-label": "CPU", "front-cpu-value": "24%", "front-mem-label": "MEM", "front-mem-value": "51%", "front-net-label": "NET", "front-net-value": "900K"},
		{"front-cpu-label": "CPU", "front-cpu-value": "47%", "front-mem-label": "MEM", "front-mem-value": "58%", "front-net-label": "NET", "front-net-value": "3.0M"},
		{"front-cpu-label": "CPU", "front-cpu-value": "68%", "front-mem-label": "MEM", "front-mem-value": "63%", "front-net-label": "NET", "front-net-value": "5.3M"},
	}
	for index, scene := range scenes {
		got := make(map[string]string)
		for _, element := range scene.Elements {
			if _, expected := want[index][element.ID]; expected && element.Text != nil {
				got[element.ID] = element.Text.Value
			}
		}
		if !reflect.DeepEqual(got, want[index]) {
			t.Fatalf("scene %d front readings = %v, want %v", index, got, want[index])
		}
	}
}
