package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

func (s *supervisor) invoke(ctx context.Context, request InvokeRequest, kind InvocationKind, token SessionToken) error {
	if !s.desired {
		return ErrPluginNotConfigured
	}
	if kind != InvocationInteractive {
		return fmt.Errorf("unsupported invocation kind %q", kind)
	}
	if token == "" {
		return errors.New("interactive invocation requires a session token")
	}
	request.SessionToken = string(token)
	if s.phase == PhaseBackoff || s.phase == PhaseQuarantined || s.phase == PhaseStopping {
		return ErrPluginUnavailable
	}
	generation, exists := s.instanceGeneration(request.InstanceID)
	if !exists || request.Generation == 0 || request.Generation != generation {
		return ErrPluginNotConfigured
	}
	if s.child == nil && !s.startChild(ctx) {
		return ErrPluginUnavailable
	}
	previous, hadPrevious := s.sessions[request.InstanceID]
	invokeCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	err := s.child.Invoke(invokeCtx, request)
	cancel()
	if err != nil {
		if errors.Is(err, rpc.ErrOutcomeUnknown) {
			s.failAmbiguousLifecycle(ErrorCodeInvokeFailed, SessionInvalidatedReplaced)
		} else {
			s.setError(ErrorCodeInvokeFailed)
		}
	} else {
		current := interactiveSession{generation: generation, token: token}
		if hadPrevious && previous != current {
			cleanupErr := s.deliverSessionCleanup(ctx, pendingSessionCleanup{instanceID: request.InstanceID, generation: previous.generation, token: previous.token})
			if errors.Is(cleanupErr, errSessionCleanupLimit) {
				return ErrPluginUnavailable
			}
		}
		s.sessions[request.InstanceID] = current
	}
	if err != nil {
		if !s.resident() && len(s.sessions) == 0 {
			_ = s.stopCurrent(ctx)
		}
	}
	return err
}

func (s *supervisor) endSession(ctx context.Context, target protocol.InstanceRef, token SessionToken) error {
	var endErr error
	if current, exists := s.sessions[target.ID]; exists && current.generation == target.Generation && current.token == token {
		delete(s.sessions, target.ID)
		if s.child != nil {
			cleanup := pendingSessionCleanup{instanceID: target.ID, generation: target.Generation, token: token}
			if ctxErr := ctx.Err(); ctxErr != nil {
				endErr = errors.Join(ctxErr, s.enqueueSessionCleanup(cleanup))
			} else {
				endErr = s.deliverSessionCleanup(ctx, cleanup)
			}
		}
	}
	if s.child != nil && !s.resident() && len(s.sessions) == 0 {
		return errors.Join(endErr, s.stopCurrent(ctx))
	}
	s.publish()
	return endErr
}

func (s *supervisor) completeSession(ctx context.Context, target protocol.InstanceRef, token SessionToken) (bool, error) {
	if token == "" {
		return false, errors.New("session completion requires a token")
	}
	changed := false
	if current, exists := s.sessions[target.ID]; exists && current.generation == target.Generation && current.token == token {
		delete(s.sessions, target.ID)
		changed = true
	}
	if s.child != nil && !s.resident() && len(s.sessions) == 0 {
		return changed, s.stopCurrent(ctx)
	}
	s.publish()
	return changed, nil
}

func (s *supervisor) handleCompletion(value completeSessionCommand) error {
	if value.ctx == nil {
		value.ctx = context.Background()
	}
	changed, err := s.completeSession(value.ctx, value.target, value.token)
	if changed && s.manager.callbacks.SessionCompleted != nil {
		s.manager.callbacks.SessionCompleted(s.id, protocol.CompleteSessionRequest{Instance: value.target, SessionToken: string(value.token)})
	}
	return err
}

func (s *supervisor) restart(ctx context.Context) error {
	if !s.desired {
		return ErrPluginNotConfigured
	}
	if s.phase == PhaseQuarantined && s.lastErrorCode == ErrorCodeUnsupportedPlatform {
		return ErrPluginUnavailable
	}
	s.clearFailures()
	s.invalidateSessions(SessionInvalidatedReplaced)
	wanted := s.resident()
	if s.child != nil {
		if wanted {
			s.postStop = postStopRestart
		}
		if err := s.stopCurrent(ctx); err != nil {
			return err
		}
	}
	if wanted {
		s.startChild(ctx)
	} else {
		s.phase = PhaseStopped
		s.publish()
	}
	return nil
}

func (s *supervisor) sessionInput(ctx context.Context, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	if !s.desired || s.child == nil {
		return protocol.SessionInputResult{}, ErrPluginNotConfigured
	}
	if s.phase == PhaseStopping {
		return protocol.SessionInputResult{}, ErrPluginUnavailable
	}
	session, exists := s.sessions[request.Instance.ID]
	if !exists || session.generation != request.Instance.Generation || session.token != SessionToken(request.SessionToken) {
		return protocol.SessionInputResult{}, ErrPluginNotConfigured
	}
	callCtx, cancel := context.WithTimeout(ctx, sessionInputTimeout(request))
	result, err := s.child.SessionInput(callCtx, request)
	cancel()
	if err != nil {
		if errors.Is(err, rpc.ErrOutcomeUnknown) {
			s.failAmbiguousLifecycle(ErrorCodeSessionInputFailed, SessionInvalidatedInput)
		} else {
			s.setError(ErrorCodeSessionInputFailed)
			s.invalidateSession(request.Instance.ID, session, SessionInvalidatedInput)
			if !s.resident() {
				_ = s.stopCurrent(ctx)
			}
		}
	}
	return result, err
}

func sessionInputTimeout(request protocol.SessionInputRequest) time.Duration {
	button := request.Input.Button
	if button != nil && button.Action == protocol.ButtonPress &&
		(button.Button == protocol.ButtonOK || button.Button == protocol.ButtonStart) {
		return sessionActionTimeout
	}
	return eventTimeout
}

func (s *supervisor) operation(ctx context.Context, request protocol.OperationRequest) (protocol.OperationResult, error) {
	if !s.desired || s.phase == PhaseBackoff || s.phase == PhaseQuarantined || s.phase == PhaseStopping {
		return protocol.OperationResult{}, ErrPluginUnavailable
	}
	generation, exists := s.instanceGeneration(request.Instance.ID)
	if !exists || !operationDeclared(s.spec.Operations, request.Operation) {
		return protocol.OperationResult{}, ErrPluginNotConfigured
	}
	if request.Instance.Generation != generation {
		return protocol.OperationResult{}, ErrPluginNotConfigured
	}
	if s.child == nil && !s.startChild(ctx) {
		return protocol.OperationResult{}, ErrPluginUnavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	result, err := s.child.Operation(callCtx, request)
	cancel()
	if err != nil {
		s.setError(ErrorCodeOperationFailed)
	}
	if !s.resident() && len(s.sessions) == 0 {
		err = errors.Join(err, s.stopCurrent(ctx))
	}
	return result, err
}

func operationDeclared(descriptors []protocol.OperationDescriptor, id string) bool {
	for _, descriptor := range descriptors {
		if descriptor.ID == id {
			return true
		}
	}
	return false
}

func (s *supervisor) deliverSessionCleanup(ctx context.Context, cleanup pendingSessionCleanup) error {
	if s.child == nil {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	err := s.child.EndSession(callCtx, EndSessionRequest{InstanceID: cleanup.instanceID, Generation: cleanup.generation, SessionToken: string(cleanup.token)})
	cancel()
	if err == nil {
		s.removePendingSessionCleanup(cleanup)
		if len(s.pendingCleanups) == 0 {
			s.clearSessionCleanupLifecycle()
		}
		s.publish()
		s.emitSessionCleanup()
		return nil
	}
	if errors.Is(err, rpc.ErrOutcomeUnknown) {
		s.failAmbiguousLifecycle(ErrorCodeEndSessionFailed, SessionInvalidatedReplaced)
		return err
	}
	if !s.hasPendingSessionCleanup(cleanup) {
		if queueErr := s.enqueueSessionCleanup(cleanup); queueErr != nil {
			return queueErr
		}
		return err
	}
	s.setSessionCleanupError()
	s.publish()
	return err
}

func (s *supervisor) enqueueSessionCleanup(cleanup pendingSessionCleanup) error {
	if s.child == nil {
		return nil
	}
	if s.hasPendingSessionCleanup(cleanup) {
		s.setSessionCleanupError()
		s.publish()
		s.emitSessionCleanup()
		return nil
	}
	if len(s.pendingCleanups) >= maxPendingSessionCleanups {
		s.setSessionCleanupError()
		s.publish()
		s.emitSessionCleanup()
		s.failClosedSessionCleanup()
		return errSessionCleanupLimit
	}
	s.pendingCleanups = append(s.pendingCleanups, cleanup)
	s.setSessionCleanupError()
	s.publish()
	s.emitSessionCleanup()
	return nil
}

func (s *supervisor) retryPendingSessionCleanup(ctx context.Context) {
	if len(s.pendingCleanups) == 0 {
		return
	}
	if s.child == nil {
		s.clearSessionCleanupState()
		s.publish()
		s.emitSessionCleanup()
		return
	}
	cleanup := s.pendingCleanups[0]
	callCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	err := s.child.EndSession(callCtx, EndSessionRequest{InstanceID: cleanup.instanceID, Generation: cleanup.generation, SessionToken: string(cleanup.token)})
	cancel()
	if err != nil {
		if errors.Is(err, rpc.ErrOutcomeUnknown) {
			s.failAmbiguousLifecycle(ErrorCodeEndSessionFailed, SessionInvalidatedReplaced)
			return
		}
		if len(s.pendingCleanups) > 1 {
			copy(s.pendingCleanups, s.pendingCleanups[1:])
			s.pendingCleanups[len(s.pendingCleanups)-1] = cleanup
		}
		s.setSessionCleanupError()
		s.publish()
		s.emitSessionCleanup()
		return
	}
	s.pendingCleanups = s.pendingCleanups[1:]
	if len(s.pendingCleanups) == 0 {
		s.clearSessionCleanupLifecycle()
	}
	s.publish()
	s.emitSessionCleanup()
}

func (s *supervisor) failAmbiguousLifecycle(code string, reason SessionInvalidationReason) {
	s.invalidateSessions(reason)
	_ = s.stopCurrent(context.Background())
	s.recordFailure(code)
}

func (s *supervisor) failClosedSessionCleanup() {
	s.invalidateSessions(SessionInvalidatedReplaced)
	wanted := s.resident()
	if wanted {
		s.queuePostStop(postStopRestart)
	}
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	if err := s.stopCurrent(ctx); err != nil {
		return
	}
	if wanted {
		s.startChild(ctx)
	}
}

func (s *supervisor) hasPendingSessionCleanup(cleanup pendingSessionCleanup) bool {
	for _, pending := range s.pendingCleanups {
		if pending == cleanup {
			return true
		}
	}
	return false
}

func (s *supervisor) removePendingSessionCleanup(cleanup pendingSessionCleanup) {
	for index, pending := range s.pendingCleanups {
		if pending != cleanup {
			continue
		}
		copy(s.pendingCleanups[index:], s.pendingCleanups[index+1:])
		s.pendingCleanups = s.pendingCleanups[:len(s.pendingCleanups)-1]
		return
	}
}

func (s *supervisor) setSessionCleanupError() {
	s.sessionErrorCode = ErrorCodeEndSessionFailed
	s.sessionErrorAt = s.manager.options.clock.Now()
}

func (s *supervisor) clearSessionCleanupLifecycle() {
	s.sessionErrorCode = ""
	s.sessionErrorAt = time.Time{}
}

func (s *supervisor) clearSessionCleanupState() {
	s.pendingCleanups = nil
	s.clearSessionCleanupLifecycle()
}

func (s *supervisor) emitSessionCleanup() {
	if s.manager.callbacks.SessionCleanup == nil {
		return
	}
	s.manager.callbacks.SessionCleanup(SessionCleanup{
		PluginID:     s.id,
		Sequence:     s.manager.sessionCleanupSequence.Add(1),
		At:           s.manager.options.clock.Now().UTC(),
		PendingCount: len(s.pendingCleanups),
		ErrorCode:    s.sessionErrorCode,
	})
}
func (s *supervisor) resident() bool {
	return hasExecutionMode(s.spec.ExecutionModes, protocol.ExecutionModeResident)
}
func (s *supervisor) wantsChild() bool { return s.resident() || len(s.sessions) > 0 }

func (s *supervisor) instanceGeneration(id string) (uint64, bool) {
	for _, instance := range s.spec.Instances {
		if instance.ID == id {
			return instance.Generation, true
		}
	}
	return 0, false
}
func (s *supervisor) pruneSessions() {
	for id, session := range s.sessions {
		current, exists := s.instanceGeneration(id)
		if !exists || current != session.generation {
			s.invalidateSession(id, session, SessionInvalidatedGeneration)
		}
	}
}
func (s *supervisor) invalidateSessions(reason SessionInvalidationReason) {
	for id, session := range s.sessions {
		s.invalidateSession(id, session, reason)
	}
}
func (s *supervisor) invalidateSession(id string, session interactiveSession, reason SessionInvalidationReason) {
	delete(s.sessions, id)
	if callback := s.manager.callbacks.SessionInvalidated; callback != nil {
		callback(SessionInvalidation{PluginID: s.id, InstanceID: id, Generation: session.generation, Token: session.token, Reason: reason})
	}
}
