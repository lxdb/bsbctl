package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	pluginID = "example.bsbctl.hello"
	channel  = "main"
	key      = "message"
)

type handler struct {
	host     *plugin.Host
	revision atomic.Uint64

	mu     sync.RWMutex
	active map[string]protocol.InstanceRef
}

func newHandler(host *plugin.Host) *handler {
	return &handler{host: host, active: make(map[string]protocol.InstanceRef)}
}

func (h *handler) ReplaceInstances(_ context.Context, instances []protocol.Instance) error {
	next := make(map[string]protocol.InstanceRef, len(instances))
	for _, instance := range instances {
		next[instance.ID] = instance.Ref()
	}
	h.mu.Lock()
	h.active = next
	h.mu.Unlock()
	return nil
}

func (h *handler) StartSession(ctx context.Context, request protocol.SessionStartRequest) error {
	if request.Action != "" && request.Action != "show" {
		return errors.New("unsupported action")
	}
	h.mu.RLock()
	active, exists := h.active[request.Instance.ID]
	h.mu.RUnlock()
	if !exists || active != request.Instance {
		return errors.New("instance generation is not active")
	}

	now := time.Now().UTC()
	x, y := 1, 0
	observation := protocol.Observation{
		Instance:    request.Instance,
		Channel:     channel,
		Key:         key,
		Revision:    h.revision.Add(1),
		Disposition: protocol.DispositionSnapshot,
		Impact:      protocol.ImpactNormal,
		ReasonCode:  "example.invoked",
		ObservedAt:  now,
		UpdatedAt:   now,
		ValidUntil:  now.Add(time.Minute),
		Scene: &protocol.Scene{Elements: []protocol.Element{{
			ID: "message", Display: protocol.DisplayFront, X: x, Y: y,
			Text: &protocol.TextElement{Value: "HELLO", Font: "normal", Color: "#EAF4F2FF"},
		}}},
	}
	if err := observation.Validate(now); err != nil {
		return fmt.Errorf("validate observation: %w", err)
	}
	return h.host.PublishObservation(ctx, observation)
}

func (h *handler) HandleSessionInput(_ context.Context, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	h.mu.RLock()
	active, exists := h.active[request.Instance.ID]
	h.mu.RUnlock()
	if !exists || active != request.Instance {
		return protocol.SessionInputResult{}, errors.New("instance generation is not active")
	}
	return protocol.SessionInputResult{Disposition: protocol.SessionInputNotConsumed}, nil
}

func (h *handler) EndSession(ctx context.Context, request protocol.SessionEndRequest) error {
	return h.host.WithdrawObservation(ctx, protocol.WithdrawRequest{
		Instance: request.Instance,
		Channel:  channel,
		Key:      key,
	})
}

func (*handler) Health(context.Context) protocol.HealthResult {
	return protocol.HealthResult{Healthy: true, ObservedAt: time.Now().UTC()}
}

var _ plugin.Plugin = (*handler)(nil)
var _ plugin.SessionHandler = (*handler)(nil)
var _ plugin.HealthReporter = (*handler)(nil)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	err := plugin.Run(ctx, plugin.Definition{
		ID:      pluginID,
		Version: "0.1.0",
		Contract: plugin.Contract{
			ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
			Channels:       []protocol.Channel{{ID: channel}},
		},
		New: func(host *plugin.Host) plugin.Plugin {
			return newHandler(host)
		},
	})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "bsbctl example plugin:", err)
		os.Exit(1)
	}
}
