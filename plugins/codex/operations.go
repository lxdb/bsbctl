package codex

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

type pinRequest struct {
	ThreadID string `json:"thread_id"`
}

type codexCheckpoint struct {
	SchemaVersion  int    `json:"schema_version"`
	PinnedThreadID string `json:"pinned_thread_id,omitempty"`
}

const codexCheckpointSchemaVersion = 1

func (h *Handler) InvokeOperation(ctx context.Context, request protocol.OperationRequest) (protocol.OperationResult, error) {
	h.mu.RLock()
	worker := h.worker
	h.mu.RUnlock()
	if worker == nil || request.Instance != (protocol.InstanceRef{ID: worker.instanceID, Generation: worker.generation}) {
		return protocol.OperationResult{}, errors.New("Codex instance is not active")
	}
	worker.stateMu.Lock()
	mutated := false
	previousPinned := worker.reducer.PinnedThread()
	switch {
	case request.Operation == OperationSessions:
		result, err := json.Marshal(struct {
			Sessions []ThreadSummary `json:"sessions"`
		}{Sessions: worker.reducer.ThreadSummaries()})
		worker.stateMu.Unlock()
		return protocol.OperationResult{Payload: result}, err
	case request.Operation == OperationPin:
		var value pinRequest
		if decodeOperationPayload(request.Payload, &value) != nil || !safeThreadID(value.ThreadID) || !worker.reducer.PinThread(value.ThreadID) {
			worker.stateMu.Unlock()
			return protocol.OperationResult{}, errors.New("Codex thread cannot be pinned")
		}
		mutated = true
	case request.Operation == OperationUnpin:
		if len(request.Payload) != 0 && decodeOperationPayload(request.Payload, &struct{}{}) != nil {
			worker.stateMu.Unlock()
			return protocol.OperationResult{}, errors.New("Codex unpin payload is invalid")
		}
		worker.reducer.UnpinThread()
		mutated = true
	default:
		worker.stateMu.Unlock()
		return protocol.OperationResult{}, errors.New("Codex operation is unsupported")
	}
	pinned := worker.reducer.PinnedThread()
	if mutated {
		if err := worker.publishCheckpoint(ctx, pinned); err != nil {
			worker.reducer.RestorePinnedThread(previousPinned)
			worker.stateMu.Unlock()
			return protocol.OperationResult{}, err
		}
		cards := worker.reducer.Cards()
		worker.stateMu.Unlock()
		if err := worker.publisher.Publish(ctx, cards); err != nil {
			return protocol.OperationResult{}, err
		}
	} else {
		worker.stateMu.Unlock()
	}
	payload, err := json.Marshal(struct {
		PinnedThreadID *string `json:"pinned_thread_id"`
	}{PinnedThreadID: optionalString(pinned)})
	return protocol.OperationResult{Payload: payload}, err
}

func (w *codexWorker) publishCheckpoint(ctx context.Context, pinned string) error {
	data, err := json.Marshal(codexCheckpoint{SchemaVersion: codexCheckpointSchemaVersion, PinnedThreadID: pinned})
	if err != nil {
		return err
	}
	return w.host.SaveCheckpoint(ctx, protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, Data: data})
}

func decodePinnedCheckpoint(raw json.RawMessage) string {
	var checkpoint codexCheckpoint
	if decodeOperationPayload(raw, &checkpoint) != nil ||
		checkpoint.SchemaVersion != codexCheckpointSchemaVersion ||
		(checkpoint.PinnedThreadID != "" && !safeThreadID(checkpoint.PinnedThreadID)) {
		return ""
	}
	return checkpoint.PinnedThreadID
}

func decodeOperationPayload(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	return protocol.DecodeStrict(raw, target)
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
