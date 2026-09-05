package pluginhost

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

func (s *supervisor) apply(ctx context.Context, spec Spec) error {
	changed := !s.desired || !reflect.DeepEqual(s.spec, spec)
	old := s.spec
	if changed {
		s.clearFailures()
	}
	s.desired = true
	s.spec = cloneSpec(spec)
	s.pruneSessions()
	if s.stopping {
		if s.wantsChild() {
			s.queuePostStop(postStopRestart)
		}
		if err := s.stopCurrent(ctx); err != nil {
			return err
		}
	}
	if s.child != nil && startupChanged(old, spec) {
		s.invalidateSessions(SessionInvalidatedReplaced)
		if s.wantsChild() {
			s.queuePostStop(postStopRestart)
		}
		if err := s.stopCurrent(ctx); err != nil {
			return err
		}
	}
	if s.child != nil && !reflect.DeepEqual(s.childSpec.Instances, spec.Instances) {
		callCtx, cancel := context.WithTimeout(ctx, operationTimeout)
		err := s.replaceChildInstances(callCtx)
		cancel()
		if err != nil {
			if errors.Is(err, rpc.ErrOutcomeUnknown) {
				s.failAmbiguousLifecycle(ErrorCodeReconcileFailed, SessionInvalidatedReplaced)
			} else if isPermanentConfiguration(err) {
				s.quarantine(ErrorCodeConfigurationRejected)
			} else {
				s.recordFailure(ErrorCodeReconcileFailed)
			}
			return err
		}
		s.clearFailures()
	}
	if s.child != nil {
		s.retryPendingSessionCleanup(ctx)
	}
	if s.child != nil && !s.resident() && len(s.sessions) == 0 {
		return s.stopCurrent(ctx)
	}
	if s.child == nil && s.wantsChild() && s.phase != PhaseBackoff && s.phase != PhaseQuarantined {
		s.startChild(ctx)
	} else {
		s.publish()
	}
	return nil
}

func (s *supervisor) startChild(ctx context.Context) bool {
	if !s.desired || s.child != nil || s.stopping || s.phase == PhaseQuarantined {
		return false
	}
	s.phase = PhaseStarting
	s.retryAt = time.Time{}
	s.publish()
	callbacks := s.manager.callbacks
	callbacks.CompleteSession = func(ctx context.Context, pluginID string, request protocol.CompleteSessionRequest) error {
		if pluginID != s.id {
			return ErrPluginNotConfigured
		}
		return s.admitCompletion(ctx, completeSessionCommand{
			target: request.Instance,
			token:  SessionToken(request.SessionToken),
		})
	}
	callCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	child, err := s.manager.start(callCtx, s.manager.coreVersion, cloneSpec(s.spec), callbacks)
	cancel()
	if err != nil {
		if errors.Is(err, ErrPermanentStart) {
			code := ErrorCodeUnsupportedPlatform
			if isPermanentConfiguration(err) {
				code = ErrorCodeConfigurationRejected
			}
			s.quarantine(code)
			return false
		}
		s.recordFailure(ErrorCodeStartFailed)
		return false
	}
	s.child = child
	reaped := make(chan struct{})
	s.childReaped = reaped
	s.clearSessionCleanupState()
	s.emitSessionCleanup()
	s.childSpec = cloneSpec(s.spec)
	s.runID++
	runID := s.runID
	s.phase = PhaseRunning
	s.healthy = true
	s.healthMisses = 0
	s.retryAt = time.Time{}
	s.scheduleHealth()
	s.publish()
	incarnation := s.manager.childIncarnation.Add(1)
	if s.manager.callbacks.Started != nil {
		s.manager.callbacks.Started(s.id, incarnation)
	}
	s.observers.Go(func() {
		err, ok := <-child.Done()
		if !ok {
			err = nil
		}
		close(reaped)
		select {
		case s.commands <- exitCommand{runID: runID, err: err}:
		case <-s.done:
		}
	})
	return true
}

func (s *supervisor) stopCurrent(ctx context.Context) error {
	if s.child == nil {
		return nil
	}
	if !s.stopping {
		s.cancelHealth()
		s.stopping = true
		s.phase = PhaseStopping
		s.publish()
	}
	callCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	err := s.child.Stop(callCtx)
	if err == nil {
		err = s.awaitStop(callCtx)
	} else {
		select {
		case <-s.childReaped:
			s.continueAfterStop(s.finishStop())
		default:
		}
	}
	cancel()
	if err != nil {
		s.setError(ErrorCodeStopFailed)
	}
	return err
}

func (s *supervisor) awaitStop(ctx context.Context) error {
	if !s.stopping || s.child == nil {
		return nil
	}
	select {
	case <-s.childReaped:
		s.finishStop()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *supervisor) queuePostStop(action postStopAction) {
	if action == postStopRetry || s.postStop == postStopNone {
		s.postStop = action
	}
}

func (s *supervisor) clearPostStop() {
	s.postStop = postStopNone
}

func (s *supervisor) finishStop() postStopAction {
	if s.child == nil {
		return postStopNone
	}
	action := s.postStop
	stoppedSpec := s.childSpec
	s.runID++
	s.child = nil
	s.childReaped = nil
	s.stopping = false
	s.clearPostStop()
	s.healthy = false
	s.clearSessionCleanupState()
	s.emitSessionCleanup()
	s.withdraw(stoppedSpec)
	s.phase = PhaseStopped
	s.publish()
	return action
}

func (s *supervisor) exited(value exitCommand) {
	if s.child == nil || value.runID != s.runID {
		return
	}
	if s.stopping {
		s.continueAfterStop(s.finishStop())
		return
	}
	s.cancelHealth()
	s.child = nil
	s.healthy = false
	s.clearSessionCleanupState()
	s.emitSessionCleanup()
	s.withdraw(s.childSpec)
	exitCount := s.addExit(s.manager.options.clock.Now())
	s.invalidateSessions(SessionInvalidatedExit)
	if !s.desired {
		s.phase = PhaseStopped
		s.publish()
		return
	}
	s.lastErrorCode = ErrorCodeUnexpectedExit
	s.lastErrorAt = s.manager.options.clock.Now()
	s.cancelRetry()
	if exitCount >= 5 {
		s.phase = PhaseQuarantined
		s.retryAt = time.Time{}
		s.publish()
		return
	}
	s.scheduleRetry()
	_ = value.err
}

func (s *supervisor) continueAfterStop(action postStopAction) {
	if !s.desired || !s.wantsChild() {
		return
	}
	if action == postStopRetry {
		s.scheduleRetry()
		return
	}
	if action == postStopRestart {
		s.startChild(context.Background())
	}
}

func (s *supervisor) withdraw(spec Spec) {
	if s.manager.callbacks.WithdrawGeneration == nil {
		return
	}
	seen := make(map[uint64]struct{})
	for _, instance := range spec.Instances {
		if _, exists := seen[instance.Generation]; exists {
			continue
		}
		seen[instance.Generation] = struct{}{}
		s.manager.callbacks.WithdrawGeneration(s.id, instance.Generation)
	}
}

func (s *supervisor) withdrawReplacedInstances(previous, next Spec) {
	if s.manager.callbacks.WithdrawGeneration == nil && s.manager.callbacks.WithdrawInstance == nil {
		return
	}
	remaining := make(map[string]uint64)
	remainingGenerations := make(map[uint64]struct{})
	for _, instance := range next.Instances {
		remaining[instance.ID] = instance.Generation
		remainingGenerations[instance.Generation] = struct{}{}
	}
	removed := make(map[uint64]struct{})
	for _, instance := range previous.Instances {
		if generation, remains := remaining[instance.ID]; remains && generation == instance.Generation {
			continue
		}
		if s.manager.callbacks.WithdrawInstance != nil {
			s.manager.callbacks.WithdrawInstance(s.id, instance.ID, instance.Generation)
			continue
		}
		if _, remains := remainingGenerations[instance.Generation]; remains {
			continue
		}
		removed[instance.Generation] = struct{}{}
	}
	for generation := range removed {
		s.manager.callbacks.WithdrawGeneration(s.id, generation)
	}
}
