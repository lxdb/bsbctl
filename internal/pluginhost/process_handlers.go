package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/lxdb/bsbctl/internal/identifier"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

func (p *Process) register(callbacks Callbacks) error {
	if err := p.peer.Handle("host.observation.publish", func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request protocol.PublishRequest
		if err := protocol.DecodeStrict(raw, &request); err != nil {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		if err := request.Observation.Validate(time.Now().UTC()); err != nil {
			return nil, domainHostError(protocol.ErrorInvalidArgument)
		}
		accepted, err := p.applyActiveEffect(request.Observation.Instance, request.Observation.Channel, func() error {
			if callbacks.Observe == nil {
				return nil
			}
			return callbacks.Observe(observation.Source{PluginID: p.spec.ID, Generation: request.Observation.Instance.Generation}, request.Observation)
		})
		if errors.Is(err, errHostEffectNotReady) {
			return nil, hostEffectNotReady()
		}
		if !accepted {
			return nil, domainHostError(protocol.ErrorGenerationConflict)
		}
		if err != nil {
			if domain, ok := errors.AsType[*protocol.DomainError](err); ok && domain.Data().Validate() == nil {
				return nil, domainHostError(domain.Kind())
			}
			return nil, domainHostError(protocol.ErrorNotReady)
		}
		return struct{}{}, nil
	}); err != nil {
		return err
	}
	if err := p.peer.Handle("host.observation.withdraw", func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request protocol.WithdrawRequest
		if err := protocol.DecodeStrict(raw, &request); err != nil || request.Validate() != nil {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		accepted, err := p.applyActiveEffect(request.Instance, request.Channel, func() error {
			if callbacks.Withdraw == nil {
				return nil
			}
			err := callbacks.Withdraw(p.spec.ID, request)
			if errors.Is(err, observation.ErrNotFound) {
				return nil
			}
			return err
		})
		if errors.Is(err, errHostEffectNotReady) {
			return nil, hostEffectNotReady()
		}
		if !accepted {
			return nil, domainHostError(protocol.ErrorGenerationConflict)
		}
		if err != nil {
			return nil, domainHostError(protocol.ErrorNotReady)
		}
		return struct{}{}, nil
	}); err != nil {
		return err
	}
	if err := p.peer.Handle("host.checkpoint.save", func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request protocol.CheckpointRequest
		if err := protocol.DecodeStrict(raw, &request); err != nil || request.Validate() != nil {
			return nil, &rpc.Error{Code: -32602, Message: "invalid checkpoint"}
		}
		accepted, err := p.applyActiveEffect(request.Instance, "", func() error {
			if callbacks.Checkpoint == nil {
				return nil
			}
			return callbacks.Checkpoint(p.spec.ID, request)
		})
		if errors.Is(err, errHostEffectNotReady) {
			return nil, hostEffectNotReady()
		}
		if !accepted {
			return nil, domainHostError(protocol.ErrorGenerationConflict)
		}
		if err != nil {
			return nil, domainHostError(protocol.ErrorNotReady)
		}
		return struct{}{}, nil
	}); err != nil {
		return err
	}
	if err := p.peer.Handle("host.session.complete", func(ctx context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request protocol.CompleteSessionRequest
		if protocol.DecodeStrict(raw, &request) != nil || request.Validate() != nil ||
			request.SessionToken == "" || identifier.Validate("session token", request.SessionToken) != nil || callbacks.CompleteSession == nil {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		accepted, err := p.applyActiveEffect(request.Instance, "", func() error {
			return callbacks.CompleteSession(context.WithoutCancel(ctx), p.spec.ID, request)
		})
		if errors.Is(err, errHostEffectNotReady) {
			return nil, hostEffectNotReady()
		}
		if !accepted {
			return nil, domainHostError(protocol.ErrorGenerationConflict)
		}
		if err != nil {
			return nil, domainHostError(protocol.ErrorNotReady)
		}
		return struct{}{}, nil
	}); err != nil {
		return err
	}
	if err := p.peer.Handle("host.session.execution.begin", func(ctx context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request protocol.SessionExecutionRequest
		if protocol.DecodeStrict(raw, &request) != nil || request.Validate() != nil || callbacks.BeginExecution == nil {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		accepted, err := p.applyActiveEffect(request.Instance, "", func() error {
			return callbacks.BeginExecution(ctx, p.spec.ID, request)
		})
		if errors.Is(err, errHostEffectNotReady) {
			return nil, domainHostError(protocol.ErrorSessionGenerationMismatch)
		}
		if !accepted {
			return nil, domainHostError(protocol.ErrorSessionGenerationMismatch)
		}
		if err != nil {
			if domain, ok := errors.AsType[*protocol.DomainError](err); ok && domain.Data().Validate() == nil {
				return nil, domainHostError(domain.Kind())
			}
			return nil, domainHostError(protocol.ErrorNotReady)
		}
		return struct{}{}, nil
	}); err != nil {
		return err
	}
	if err := p.peer.HandleLossyNotification("host.metric", func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var metric protocol.MetricNotification
		if protocol.DecodeStrict(raw, &metric) == nil && metric.Validate() == nil && callbacks.Metric != nil {
			callbacks.Metric(metric)
		}
		return struct{}{}, nil
	}); err != nil {
		return err
	}
	return p.peer.Handle("host.log", func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var notification protocol.LogNotification
		if err := protocol.DecodeStrict(raw, &notification); err != nil {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		if err := notification.Validate(); err != nil {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		if notification.Instance != (protocol.InstanceRef{}) && !p.hasInstanceRef(notification.Instance) {
			return nil, domainHostError(protocol.ErrorGenerationConflict)
		}
		if callbacks.Log != nil {
			callbacks.Log(p.spec.ID, notification)
		}
		return struct{}{}, nil
	})
}

func (p *Process) hasInstanceRef(ref protocol.InstanceRef) bool {
	p.policyMu.RLock()
	defer p.policyMu.RUnlock()
	instance, exists := p.instances[ref.ID]
	return exists && instance.Generation == ref.Generation
}

var errHostEffectNotReady = errors.New("host effect is not ready")

// applyActiveEffect keeps authorization valid until the admitted effect
// finishes. Proposed-generation effects retry after replacement commits.
func (p *Process) applyActiveEffect(ref protocol.InstanceRef, channel string, effect func() error) (bool, error) {
	p.effectMu.Lock()
	p.policyMu.RLock()
	if p.pending != nil {
		active := instanceMatches(p.instances, ref, channel)
		proposed := instanceMatches(p.pending, ref, channel)
		if active && proposed {
			p.policyMu.RUnlock()
			p.beginAdmittedEffectLocked(ref)
			p.effectMu.Unlock()
			return true, p.runAdmittedEffect(ref, effect)
		}
		p.policyMu.RUnlock()
		p.effectMu.Unlock()
		if active || proposed {
			return true, errHostEffectNotReady
		}
		return false, nil
	}
	if instanceMatches(p.instances, ref, channel) {
		p.policyMu.RUnlock()
		p.beginAdmittedEffectLocked(ref)
		p.effectMu.Unlock()
		return true, p.runAdmittedEffect(ref, effect)
	}
	p.policyMu.RUnlock()
	p.effectMu.Unlock()
	return false, nil
}

func (p *Process) beginAdmittedEffectLocked(ref protocol.InstanceRef) {
	if p.effectInFlight == nil {
		p.effectInFlight = make(map[protocol.InstanceRef]int)
	}
	p.effectInFlight[ref]++
}

func (p *Process) runAdmittedEffect(ref protocol.InstanceRef, effect func() error) (err error) {
	defer p.finishAdmittedEffect(ref)
	return effect()
}

func (p *Process) finishAdmittedEffect(ref protocol.InstanceRef) {
	p.effectMu.Lock()
	defer p.effectMu.Unlock()
	if p.effectInFlight[ref] == 1 {
		delete(p.effectInFlight, ref)
	} else {
		p.effectInFlight[ref]--
	}
	p.signalEffectChangeLocked()
}

func (p *Process) retiringEffectInFlightLocked(proposed map[string]Instance) bool {
	for ref, count := range p.effectInFlight {
		if count == 0 {
			continue
		}
		next, exists := proposed[ref.ID]
		if !exists || next.Generation != ref.Generation {
			return true
		}
	}
	return false
}

func (p *Process) effectChangedLocked() <-chan struct{} {
	if p.effectChanged == nil {
		p.effectChanged = make(chan struct{})
	}
	return p.effectChanged
}

func (p *Process) signalEffectChangeLocked() {
	if p.effectChanged != nil {
		close(p.effectChanged)
		p.effectChanged = nil
	}
}

func hostEffectNotReady() *rpc.Error {
	return domainHostError(protocol.ErrorNotReady)
}

func domainHostError(kind protocol.ErrorKind) *rpc.Error {
	data, _ := json.Marshal(protocol.ErrorData{Kind: kind})
	return &rpc.Error{Code: protocol.DomainErrorCode, Message: "bsbctl request failed", Data: data}
}

func instanceMatches(instances map[string]Instance, ref protocol.InstanceRef, channel string) bool {
	instance, exists := instances[ref.ID]
	if !exists || instance.Generation != ref.Generation {
		return false
	}
	if channel == "" {
		return true
	}
	_, exists = instance.Policies[channel]
	return exists
}

func (p *Process) instanceRef(instanceID string, generation uint64) (protocol.InstanceRef, bool) {
	p.policyMu.RLock()
	defer p.policyMu.RUnlock()
	instance, exists := p.instances[instanceID]
	if !exists || generation == 0 || generation != instance.Generation {
		return protocol.InstanceRef{}, false
	}
	return protocol.InstanceRef{ID: instanceID, Generation: instance.Generation}, true
}

func instanceMap(instances []Instance) map[string]Instance {
	result := make(map[string]Instance, len(instances))
	for _, instance := range instances {
		result[instance.ID] = instance
	}
	return result
}
