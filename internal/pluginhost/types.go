package pluginhost

import (
	"encoding/json"

	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

// CheckpointRestore is a generation-scoped checkpoint supplied during desired
// instance replacement.
type CheckpointRestore struct {
	Generation uint64
	Data       json.RawMessage
}

// Instance is one enabled desired plugin instance. Disabled configuration is
// excluded before it reaches the plugin host.
type Instance struct {
	ID         string
	Generation uint64
	Config     json.RawMessage
	Secrets    map[string]string
	Policies   map[string]presentation.PolicyConfig
	Checkpoint *CheckpointRestore
}

func (instance Instance) Ref() protocol.InstanceRef {
	return protocol.InstanceRef{ID: instance.ID, Generation: instance.Generation}
}

func (instance Instance) Wire() protocol.Instance {
	result := protocol.Instance{
		ID: instance.ID, Generation: instance.Generation,
		Config: instance.Config, Secrets: instance.Secrets,
	}
	if instance.Checkpoint != nil {
		result.Checkpoint = instance.Checkpoint.Data
	}
	return result
}

// InvokeRequest is the host's pre-dispatch session command.
type InvokeRequest struct {
	InstanceID   string
	Generation   uint64
	Action       string
	Payload      json.RawMessage
	SessionToken string
	Trigger      *protocol.SessionTrigger
}

// EndSessionRequest identifies one exact active plugin session.
type EndSessionRequest struct {
	InstanceID   string
	Generation   uint64
	SessionToken string
}
