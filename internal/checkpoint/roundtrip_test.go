package checkpoint

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckpointEscapingPreservesSuccessfulSave(t *testing.T) {
	for _, character := range []string{"<", ">", "&", "\u2028", "\u2029"} {
		t.Run(character, func(t *testing.T) {
			root := t.TempDir()
			key := Key{PluginID: "dev.bsbctl.example", InstanceID: "main", Generation: 1}
			want := strings.Repeat(character, 12000)
			payload := json.RawMessage(`{"value":"` + want + `"}`)
			outcome, err := NewStore(root).Save(key, payload)
			if err != nil || !outcome.IsCommitted() {
				t.Fatalf("valid bounded checkpoint Save=%s error=%v", outcome, err)
			}
			data, exists, err := NewStore(root).Load(key)
			if err != nil || !exists {
				t.Fatalf("successfully committed checkpoint lost after reopening: exists=%t error=%v", exists, err)
			}
			var got struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(data, &got); err != nil || got.Value != want {
				t.Fatalf("checkpoint round trip changed data: length=%d error=%v", len(got.Value), err)
			}
		})
	}
}
