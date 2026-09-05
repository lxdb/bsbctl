package pluginhost

import (
	"context"
	"errors"
	"math"
	"reflect"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

func (s *supervisor) handleHealth() {
	if !s.desired || s.child == nil {
		return
	}
	s.retryPendingSessionCleanup(context.Background())
	if s.child == nil || s.stopping {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	result, err := s.child.Ping(ctx)
	cancel()
	if err == nil {
		s.healthMisses = 0
		s.retryAttempt = 0
		s.healthy = result.Healthy
		if result.Healthy {
			s.phase = PhaseRunning
			if s.lastErrorCode == ErrorCodeHealthReported {
				s.lastErrorCode = ""
				s.lastErrorAt = time.Time{}
			}
		} else {
			s.phase = PhaseUnhealthy
			s.lastErrorCode = ErrorCodeHealthReported
			s.lastErrorAt = s.manager.options.clock.Now()
		}
		s.scheduleHealth()
		s.publish()
		return
	}
	s.healthMisses++
	s.healthy = false
	if s.healthMisses < 3 {
		s.scheduleHealth()
		s.publish()
		return
	}
	s.phase = PhaseUnhealthy
	s.publish()
	s.invalidateSessions(SessionInvalidatedHealth)
	_ = s.stopCurrent(context.Background())
	s.recordFailure(ErrorCodeHealthTimeout)
}

func (s *supervisor) handleRetry() {
	s.retryAt = time.Time{}
	if !s.desired || s.phase == PhaseQuarantined {
		return
	}
	if s.stopping {
		s.queuePostStop(postStopRetry)
		s.phase = PhaseStopping
		s.publish()
		return
	}
	if s.child != nil && !reflect.DeepEqual(s.childSpec.Instances, s.spec.Instances) {
		ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
		err := s.replaceChildInstances(ctx)
		cancel()
		if err != nil {
			if errors.Is(err, rpc.ErrOutcomeUnknown) {
				s.failAmbiguousLifecycle(ErrorCodeReconcileFailed, SessionInvalidatedReplaced)
			} else if isPermanentConfiguration(err) {
				s.quarantine(ErrorCodeConfigurationRejected)
			} else {
				s.recordFailure(ErrorCodeReconcileFailed)
			}
			return
		}
		s.clearFailures()
		s.retryPendingSessionCleanup(context.Background())
		return
	}
	if s.wantsChild() {
		s.startChild(context.Background())
		return
	}
	s.phase = PhaseStopped
	s.publish()
}

func (s *supervisor) replaceChildInstances(ctx context.Context) error {
	if err := s.child.ReplaceInstances(ctx, cloneInstances(s.spec.Instances)); err != nil {
		return err
	}
	s.withdrawReplacedInstances(s.childSpec, s.spec)
	s.childSpec = cloneSpec(s.spec)
	return nil
}

func (s *supervisor) recordFailure(code string) {
	now := s.manager.options.clock.Now()
	s.lastErrorCode, s.lastErrorAt = code, now
	s.cancelRetry()
	if s.stopping {
		s.queuePostStop(postStopRetry)
		s.phase = PhaseStopping
		s.retryAt = time.Time{}
		s.publish()
		return
	}
	s.scheduleRetry()
}

func isPermanentConfiguration(err error) bool {
	domain, ok := errors.AsType[*protocol.DomainError](err)
	return ok && domain.Kind() == protocol.ErrorInvalidArgument
}

func (s *supervisor) quarantine(code string) {
	s.cancelRetry()
	s.lastErrorCode = code
	s.lastErrorAt = s.manager.options.clock.Now()
	s.phase = PhaseQuarantined
	s.retryAt = time.Time{}
	s.publish()
}

func (s *supervisor) scheduleRetry() {
	s.retryAttempt++
	delay := jitteredDelay(backoffDelay(s.retryAttempt), s.manager.options.jitter())
	s.phase = PhaseBackoff
	s.retryAt = s.manager.options.clock.Now().Add(delay)
	s.retryTimer = s.manager.options.clock.NewTimer(delay)
	s.publish()
}

func (s *supervisor) clearFailures() {
	s.retryAttempt = 0
	s.clearExits()
	s.lastErrorCode = ""
	s.lastErrorAt = time.Time{}
	s.healthMisses = 0
	s.cancelRetry()
	if s.stopping {
		s.phase = PhaseStopping
	} else if s.child != nil {
		s.phase = PhaseRunning
	} else {
		s.phase = PhaseStopped
	}
	s.retryAt = time.Time{}
	s.publish()
}

func (s *supervisor) setError(code string) {
	s.lastErrorCode, s.lastErrorAt = code, s.manager.options.clock.Now()
	s.publish()
}

func (s *supervisor) scheduleHealth() {
	s.cancelHealth()
	s.healthTimer = s.manager.options.clock.NewTimer(s.manager.options.healthInterval)
}
func (s *supervisor) cancelHealth() {
	if s.healthTimer != nil {
		s.healthTimer.Stop()
		s.healthTimer = nil
	}
}
func (s *supervisor) cancelRetry() {
	if s.retryTimer != nil {
		s.retryTimer.Stop()
		s.retryTimer = nil
	}
}
func (s *supervisor) cancelTimers() { s.cancelHealth(); s.cancelRetry() }
func (s *supervisor) addExit(at time.Time) int {
	s.statusMu.Lock()
	cutoff := at.Add(-failureWindow)
	kept := s.exits[:0]
	for _, value := range s.exits {
		if !value.Before(cutoff) {
			kept = append(kept, value)
		}
	}
	s.exits = kept
	s.exits = append(s.exits, at)
	count := len(s.exits)
	s.statusMu.Unlock()
	return count
}
func (s *supervisor) clearExits() {
	s.statusMu.Lock()
	s.exits = nil
	s.statusMu.Unlock()
}
func backoffDelay(failure int) time.Duration {
	if failure <= 1 {
		return time.Second
	}
	if failure >= 6 {
		return 30 * time.Second
	}
	delay := time.Second << (failure - 1)
	return delay
}
func jitteredDelay(delay time.Duration, factor float64) time.Duration {
	if math.IsNaN(factor) {
		factor = 1
	}
	if factor < 0.8 {
		factor = 0.8
	}
	if factor > 1.2 {
		factor = 1.2
	}
	if delay <= 0 {
		return 0
	}
	if float64(delay)*factor >= float64(30*time.Second) {
		return 30 * time.Second
	}
	return time.Duration(float64(delay) * factor)
}
