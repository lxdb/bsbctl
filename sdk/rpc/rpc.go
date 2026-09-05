// Package rpc implements bounded bidirectional JSON-RPC 2.0 over a byte stream.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	Version               = "2.0"
	MaxMessageBytes       = 1 << 20
	maxHandlers           = 32
	inboundQueueCapacity  = maxHandlers
	outboundQueueCapacity = 128
	handlerWriteTimeout   = 2 * time.Second
)

var (
	ErrClosed           = errors.New("json-rpc connection closed")
	ErrInboundSaturated = errors.New("json-rpc inbound method capacity exhausted")
	ErrOutcomeUnknown   = errors.New("json-rpc call outcome unknown")
	ErrProtocol         = errors.New("json-rpc protocol violation")
	errMessageTooLarge  = errors.New("json-rpc message exceeds 1 MiB")
)

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// Handler processes one inbound request.
type Handler func(context.Context, json.RawMessage) (any, *Error)

type message struct {
	JSONRPC  string          `json:"jsonrpc"`
	ID       json.RawMessage `json:"id,omitempty"`
	Method   string          `json:"method,omitempty"`
	Params   json.RawMessage `json:"params,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    *Error          `json:"error,omitempty"`
	Metadata *metadata       `json:"bsbctl,omitempty"`

	hasID       bool
	hasParams   bool
	hasResult   bool
	hasError    bool
	hasMetadata bool
}

type metadata struct {
	DeadlineUnixMilliseconds int64 `json:"deadline_unix_milliseconds,omitzero"`
}

type cancelParams struct {
	ID string `json:"id"`
}

type outboundWrite struct {
	ctx     context.Context
	data    []byte
	result  chan error
	state   *atomic.Uint32
	timeout time.Duration
}

const (
	outboundWriteQueued uint32 = iota
	outboundWriteStarted
	outboundWriteCanceled
)

type inboundMethod struct {
	ctx      context.Context
	writeCtx context.Context
	msg      message
	cancel   context.CancelFunc
}

// Peer owns one bidirectional JSON-RPC connection. Serve must run while calls are made.
type Peer struct {
	conn net.Conn

	mu               sync.Mutex
	callGate         chan struct{}
	handlers         map[string]Handler
	lossyMethods     map[string]struct{}
	pending          map[string]chan message
	inflight         map[string]context.CancelFunc
	err              error
	done             chan struct{}
	writes           chan outboundWrite
	writerDone       chan struct{}
	methods          chan inboundMethod
	methodDone       chan struct{}
	once             sync.Once
	nextID           atomic.Uint64
	highestWrittenID atomic.Uint64
	droppedInbound   atomic.Uint64
	handlerSlots     chan struct{}
}
