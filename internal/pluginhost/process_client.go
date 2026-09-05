package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"syscall"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

func (p *Process) Invoke(ctx context.Context, request InvokeRequest) error {
	ref, ok := p.instanceRef(request.InstanceID, request.Generation)
	if !ok {
		return newPluginRPCError("plugin_generation_conflict", "plugin instance generation is unavailable", nil)
	}
	if request.SessionToken == "" {
		return newPluginRPCError("plugin_invoke_failed", "plugin invocation failed", nil)
	}
	wire := protocol.SessionStartRequest{Instance: ref, Action: request.Action, Payload: request.Payload, SessionToken: request.SessionToken, Trigger: request.Trigger}
	if err := wire.Validate(); err != nil {
		return newPluginRPCError("plugin_invoke_failed", "plugin invocation failed", nil)
	}
	return mapPluginRPCError(p.peer, p.peer.CallEmpty(ctx, "plugin.session.start", wire), "plugin_invoke_failed", "plugin invocation failed")
}

// EndSession tells the plugin that one interactive instance session has ended.
func (p *Process) EndSession(ctx context.Context, request EndSessionRequest) error {
	ref, ok := p.instanceRef(request.InstanceID, request.Generation)
	if !ok {
		return nil
	}
	wire := protocol.SessionEndRequest{Instance: ref, SessionToken: request.SessionToken}
	if err := wire.Validate(); err != nil {
		return newPluginRPCError("plugin_end_session_failed", "plugin end session failed", nil)
	}
	return mapPluginRPCError(p.peer, p.peer.CallEmpty(ctx, "plugin.session.end", wire), "plugin_end_session_failed", "plugin end session failed")
}

// ReplaceInstances atomically supplies the plugin's complete desired state.
func (p *Process) ReplaceInstances(ctx context.Context, instances []Instance) error {
	if err := validateDesiredInstances(instances); err != nil {
		return newPluginRPCError("plugin_reconcile_failed", "plugin reconciliation failed", nil)
	}
	p.replaceMu.Lock()
	defer p.replaceMu.Unlock()
	wire := make([]protocol.Instance, len(instances))
	for index, instance := range instances {
		wire[index] = instance.Wire()
	}
	replacement := protocol.ReplaceInstancesRequest{Instances: wire}
	proposed := instanceMap(instances)
	p.effectMu.Lock()
	p.policyMu.Lock()
	p.pending = proposed
	p.policyMu.Unlock()
	for p.retiringEffectInFlightLocked(proposed) {
		changed := p.effectChangedLocked()
		p.effectMu.Unlock()
		select {
		case <-ctx.Done():
			p.effectMu.Lock()
			p.policyMu.Lock()
			p.pending = nil
			p.policyMu.Unlock()
			p.signalEffectChangeLocked()
			p.effectMu.Unlock()
			return mapPluginRPCError(p.peer, ctx.Err(), "plugin_reconcile_failed", "plugin reconciliation failed")
		case <-changed:
		}
		p.effectMu.Lock()
	}
	p.effectMu.Unlock()
	if err := mapPluginRPCError(p.peer, p.peer.CallEmpty(ctx, "plugin.instances.replace", replacement), "plugin_reconcile_failed", "plugin reconciliation failed"); err != nil {
		p.effectMu.Lock()
		p.policyMu.Lock()
		p.pending = nil
		p.policyMu.Unlock()
		p.signalEffectChangeLocked()
		p.effectMu.Unlock()
		return err
	}
	p.effectMu.Lock()
	p.policyMu.Lock()
	p.instances = proposed
	p.pending = nil
	p.policyMu.Unlock()
	p.signalEffectChangeLocked()
	p.effectMu.Unlock()
	return nil
}

// SessionInput delivers one exact interactive input and returns the plugin's
// strict consumed/not-consumed disposition.
func (p *Process) SessionInput(ctx context.Context, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	ref, ok := p.instanceRef(request.Instance.ID, request.Instance.Generation)
	if !ok || ref != request.Instance {
		return protocol.SessionInputResult{}, newPluginRPCError("plugin_generation_conflict", "plugin instance generation is unavailable", nil)
	}
	if err := request.Validate(); err != nil {
		return protocol.SessionInputResult{}, newPluginRPCError("plugin_session_input_failed", "plugin session input failed", nil)
	}
	var raw json.RawMessage
	err := mapPluginRPCError(p.peer, p.peer.Call(ctx, "plugin.session.input", request, &raw), "plugin_session_input_failed", "plugin session input failed")
	if err != nil {
		return protocol.SessionInputResult{}, err
	}
	var result protocol.SessionInputResult
	if err := protocol.DecodeStrict(raw, &result); err != nil || result.Validate() != nil {
		return protocol.SessionInputResult{}, newPluginRPCError("plugin_session_input_failed", "plugin session input failed", nil)
	}
	return result, nil
}

func (p *Process) Operation(ctx context.Context, request protocol.OperationRequest) (protocol.OperationResult, error) {
	if err := request.Validate(); err != nil {
		return protocol.OperationResult{}, newPluginRPCError("plugin_operation_failed", "plugin operation failed", nil)
	}
	var raw json.RawMessage
	err := mapPluginRPCError(p.peer, p.peer.Call(ctx, "plugin.operation.invoke", request, &raw), "plugin_operation_failed", "plugin operation failed")
	if err != nil {
		return protocol.OperationResult{}, err
	}
	var result protocol.OperationResult
	if err := protocol.DecodeStrict(raw, &result); err != nil {
		return protocol.OperationResult{}, newPluginRPCError("plugin_operation_failed", "plugin operation failed", nil)
	}
	if err := result.Validate(); err != nil {
		return protocol.OperationResult{}, newPluginRPCError("plugin_operation_failed", "plugin operation failed", nil)
	}
	return result, nil
}

// Ping checks plugin responsiveness.
func (p *Process) Ping(ctx context.Context) (protocol.HealthResult, error) {
	var raw json.RawMessage
	if err := mapPluginRPCError(p.peer, p.peer.Call(ctx, "plugin.health", nil, &raw), "plugin_ping_failed", "plugin health check failed"); err != nil {
		return protocol.HealthResult{}, err
	}
	var result protocol.HealthResult
	if err := protocol.DecodeStrict(raw, &result); err != nil || result.Validate() != nil {
		return protocol.HealthResult{}, newPluginRPCError("plugin_ping_failed", "plugin health check failed", nil)
	}
	return result, nil
}

// Done closes after the child has been reaped.
func (p *Process) Done() <-chan error { return p.done }

// Stop performs bounded graceful shutdown followed by process-group signals.
func (p *Process) Stop(ctx context.Context) error {
	p.stopOnce.Do(func() {
		go func() {
			p.stopErr = p.stop()
			close(p.stopDone)
		}()
	})
	select {
	case <-p.stopDone:
		return p.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Process) stop() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	shutdownErr := mapPluginRPCError(p.peer, p.peer.CallEmpty(shutdownCtx, "plugin.shutdown", nil), "plugin_shutdown_failed", "plugin shutdown failed")
	cancel()
	if waitForReap(p.reaped, p.shutdownGrace) {
		return errors.Join(ignoreExitSignal(p.waitErr), shutdownErr)
	}
	termErr := p.signalGroup(p.cmd.Process.Pid, syscall.SIGTERM)
	if waitForReap(p.reaped, p.termGrace) {
		return errors.Join(ignoreExitSignal(p.waitErr), shutdownErr, ignoreNoProcess(termErr))
	}
	killErr := p.signalGroup(p.cmd.Process.Pid, syscall.SIGKILL)
	<-p.reaped
	return errors.Join(ignoreExitSignal(p.waitErr), shutdownErr, ignoreNoProcess(termErr), ignoreNoProcess(killErr))
}

type pluginRPCError struct {
	code    string
	message string
	cause   error
}

func (e *pluginRPCError) Error() string { return e.code + ": " + e.message }
func (e *pluginRPCError) Unwrap() error { return e.cause }

func newPluginRPCError(code, message string, cause error) error {
	return &pluginRPCError{code: code, message: message, cause: cause}
}

func mapPluginRPCError(peer *rpc.Peer, err error, code, message string) error {
	if err == nil {
		return nil
	}
	if remote, ok := errors.AsType[*rpc.Error](err); ok {
		kind, domain, decodeErr := protocol.DecodeRemoteError(remote.Code, remote.Data)
		if decodeErr != nil {
			peer.TerminateProtocol(nil)
			return newPluginRPCError(code, message, rpc.ErrProtocol)
		}
		if domain {
			return newPluginRPCError(code, message, protocol.NewDomainError(kind, remote))
		}
	}
	if errors.Is(err, rpc.ErrOutcomeUnknown) {
		return newPluginRPCError(code, message, err)
	}
	if errors.Is(err, context.Canceled) {
		return newPluginRPCError(code, message, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return newPluginRPCError(code, message, context.DeadlineExceeded)
	}
	return newPluginRPCError(code, message, nil)
}

func mapInitializeRPCError(peer *rpc.Peer, err error) error {
	mapped := mapPluginRPCError(peer, err, "plugin_initialize_failed", "plugin initialization failed")
	if rpcErr, ok := errors.AsType[*rpc.Error](err); ok {
		kind, domain, decodeErr := protocol.DecodeRemoteError(rpcErr.Code, rpcErr.Data)
		if decodeErr == nil && domain && kind == protocol.ErrorInvalidArgument {
			return PermanentStart(mapped)
		}
	}
	return mapped
}

func waitForReap(reaped <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-reaped:
		return true
	case <-timer.C:
		return false
	}
}

func (p *Process) stopWithoutRPC() error {
	_ = p.signalGroup(p.cmd.Process.Pid, syscall.SIGKILL)
	<-p.reaped
	return p.waitErr
}
