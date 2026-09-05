// Package protocol defines the public, exact bsbctl plugin protocol 1.0 application contract.
package protocol

import (
	"time"
)

// Protocol and payload limits are part of the exact v1 wire contract.
const (
	Version                  = "1.0"
	MaxMessageBytes          = 1 << 20
	MaxJSONObjectBytes       = 64 << 10
	MaxConfigObjectBytes     = MaxJSONObjectBytes
	MaxCheckpointObjectBytes = MaxJSONObjectBytes
	MaxOperationObjectBytes  = MaxJSONObjectBytes
	MaxSessionInputBytes     = 16 << 10
	MaxSceneElements         = 64
	MaxTextBytes             = 512
	MaxBusyTimerDuration     = 24 * time.Hour
)
