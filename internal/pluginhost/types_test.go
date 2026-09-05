package pluginhost

import (
	"encoding/json"
	"testing"

	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestInstanceRefAndWireExposeOnlyPluginOwnedState(t *testing.T) {
	instance := Instance{
		ID: "calendar", Generation: 7,
		Config: json.RawMessage(`{"calendar":"Work"}`), Secrets: map[string]string{"token": "resolved"},
		Policies:   map[string]presentation.PolicyConfig{"main": {Policy: presentation.PolicyInteractive}},
		Checkpoint: &CheckpointRestore{Generation: 6, Data: json.RawMessage(`{"cursor":2}`)},
	}
	if got, want := instance.Ref(), (protocol.InstanceRef{ID: "calendar", Generation: 7}); got != want {
		t.Fatalf("Ref = %#v, want %#v", got, want)
	}
	wire := instance.Wire()
	if wire.ID != "calendar" || wire.Generation != 7 || string(wire.Config) != `{"calendar":"Work"}` || wire.Secrets["token"] != "resolved" || string(wire.Checkpoint) != `{"cursor":2}` {
		t.Fatalf("Wire = %#v", wire)
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"id":"calendar","generation":7,"config":{"calendar":"Work"},"secrets":{"token":"resolved"},"checkpoint":{"cursor":2}}`; got != want {
		t.Fatalf("wire JSON = %s, want %s", got, want)
	}
}

func TestInstanceWireOmitsAbsentCheckpoint(t *testing.T) {
	wire := (Instance{ID: "app", Generation: 1, Config: json.RawMessage(`{}`)}).Wire()
	if wire.Checkpoint != nil {
		t.Fatalf("checkpoint = %s, want nil", wire.Checkpoint)
	}
}
