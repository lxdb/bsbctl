package daemon

import (
	"context"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

// Constructor contracts must guarantee these capabilities before any app is
// loaded; an incomplete controller must fail compilation rather than a request.
type operationCapability interface {
	Operation(context.Context, string, protocol.OperationRequest) (protocol.OperationResult, error)
}

type selectedObservationCapability interface {
	SelectedObservation() (observation.Record, bool)
}

type assetGarbageCollectionCapability interface {
	CollectGarbage(context.Context, []assets.Package)
}

var (
	_ operationCapability              = PluginController(nil)
	_ selectedObservationCapability    = AttentionController(nil)
	_ assetGarbageCollectionCapability = AssetController(nil)
)
