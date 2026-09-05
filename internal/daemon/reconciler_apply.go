package daemon

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/pluginhost"
)

func (s *Reconciler) resolveAndApply(parent context.Context, epoch uint64, document config.Document, retained map[string]pluginhost.Instance, shouldResolve func(string) bool, attempted map[string]bool, replace bool) error {
	ctx, attemptID, finish, ok := s.beginAttempt(parent, epoch, replace)
	if !ok {
		return context.Canceled
	}
	defer finish()
	plan, err := buildPlan(ctx, document, s.resolver, retained, shouldResolve)
	if err != nil {
		return err
	}
	s.checkpoints.attach(&plan)
	revision, ok := s.stagePlan(epoch, attemptID, plan, attempted)
	if !ok {
		return context.Canceled
	}
	return s.applyPlanForAttempt(ctx, epoch, revision, attemptID, plan)
}

func (s *Reconciler) applyPlan(parent context.Context, epoch, revision uint64, plan ReconciliationPlan, replace bool) error {
	ctx, attemptID, finish, ok := s.beginAttempt(parent, epoch, replace)
	if !ok {
		return context.Canceled
	}
	defer finish()
	return s.applyPlanForAttempt(ctx, epoch, revision, attemptID, plan)
}

func (s *Reconciler) beginAttempt(parent context.Context, epoch uint64, replace bool) (context.Context, uint64, func(), bool) {
	ctx, cancel := context.WithCancel(s.retryCtx)
	stopParent := context.AfterFunc(parent, cancel)
	s.live.mu.Lock()
	if s.live.closing || s.live.epoch != epoch {
		s.live.mu.Unlock()
		stopParent()
		cancel()
		return nil, 0, func() {}, false
	}
	if s.live.activeAttempt != 0 && !replace {
		s.live.mu.Unlock()
		stopParent()
		cancel()
		return nil, 0, func() {}, false
	}
	if s.live.activeCancel != nil {
		s.live.activeCancel()
	}
	s.live.nextAttempt++
	id := s.live.nextAttempt
	s.live.activeAttempt = id
	s.live.activeCancel = cancel
	done := make(chan struct{})
	s.live.attempts[id] = done
	s.live.mu.Unlock()
	finish := func() {
		stopParent()
		cancel()
		s.live.mu.Lock()
		if owned, exists := s.live.attempts[id]; exists {
			delete(s.live.attempts, id)
			close(owned)
		}
		if s.live.activeAttempt == id {
			s.live.activeAttempt = 0
			s.live.activeCancel = nil
		}
		s.live.mu.Unlock()
		s.wakeSecretRetry()
	}
	return ctx, id, finish, true
}

func (s *Reconciler) stagePlan(epoch, attemptID uint64, plan ReconciliationPlan, attempted map[string]bool) (uint64, bool) {
	now := time.Now()
	s.live.mu.Lock()
	defer s.live.mu.Unlock()
	if s.live.closing || s.live.epoch != epoch || s.live.activeAttempt != attemptID {
		return 0, false
	}
	readiness := make(map[string]AppReadiness, len(plan.Readiness))
	for _, status := range plan.Readiness {
		if status.Phase == AppSecretPending {
			if previous, exists := s.live.appReadiness[status.AppID]; exists && previous.Phase == AppSecretPending && attempted != nil && !attempted[status.AppID] {
				readiness[status.AppID] = previous
				continue
			}
			attempt := 1
			if previous, exists := s.live.appReadiness[status.AppID]; exists && previous.Phase == AppSecretPending {
				attempt = previous.Attempt + 1
			}
			status.Attempt = attempt
			status.RetryAt = now.Add(s.retryDelay(attempt))
			status.LastErrorCode = secretUnavailableCode
		} else if status.Phase == AppReady {
			if previous, exists := s.live.appReadiness[status.AppID]; exists && previous.Phase == AppReady {
				if _, accepted := s.live.generations.Lookup(status.PluginID, status.AppID); accepted {
					status = previous
				} else {
					status.Phase = AppReconcilePending
				}
			} else {
				status.Phase = AppReconcilePending
			}
		}
		readiness[status.AppID] = status
	}
	s.live.nextCandidateRevision++
	revision := s.live.nextCandidateRevision
	s.live.candidate = &reconcileCandidate{epoch: epoch, revision: revision, plan: plan}
	s.resetCorrectionLocked()
	s.live.appReadiness = readiness
	s.wakeSecretRetry()
	return revision, true
}

func (s *Reconciler) applyPlanForAttempt(ctx context.Context, epoch, revision, attemptID uint64, plan ReconciliationPlan) error {
	specs := s.readySpecs(plan.Specs)
	if !s.attemptCurrent(epoch, revision, attemptID) {
		s.wakeSecretRetry()
		return context.Canceled
	}
	err := s.plugins.Apply(ctx, specs)
	s.live.mu.Lock()
	if s.live.closing || s.live.epoch != epoch || s.live.activeAttempt != attemptID || s.live.candidate == nil || s.live.candidate.revision != revision {
		if !s.live.closing && s.live.candidate != nil && s.live.acceptedRevision == s.live.candidate.revision {
			s.scheduleCorrectionLocked(s.live.candidate.revision)
		}
		s.live.mu.Unlock()
		s.wakeSecretRetry()
		return context.Canceled
	}
	if err != nil {
		if s.live.acceptedRevision == revision {
			if s.live.correctionRevision != revision {
				s.live.correctionRevision = revision
				s.live.correctionAttempt = 0
			}
			s.live.correctionAttempt++
			s.live.correctionRetryAt = time.Now().Add(s.retryDelay(s.live.correctionAttempt))
			s.live.mu.Unlock()
			s.wakeSecretRetry()
			return err
		}
		now := time.Now()
		for id, status := range s.live.appReadiness {
			if status.Phase != AppReconcilePending {
				continue
			}
			status.Attempt++
			status.RetryAt = now.Add(s.retryDelay(status.Attempt))
			status.LastErrorCode = reconcileFailedCode
			s.live.appReadiness[id] = status
		}
		s.live.mu.Unlock()
		s.wakeSecretRetry()
		return err
	}
	accepted := make(map[string]bool)
	generations := Generations{values: make(map[generationKey]uint64)}
	for _, spec := range specs {
		for _, instance := range spec.Instances {
			accepted[instance.ID] = true
			generations.values[generationKey{pluginID: spec.ID, instanceID: instance.ID}] = instance.Generation
		}
	}
	readiness := make(map[string]AppReadiness, len(plan.Readiness))
	for _, status := range plan.Readiness {
		if status.Phase == AppReady {
			if accepted[status.AppID] {
				status = AppReadiness{AppID: status.AppID, PluginID: status.PluginID, Phase: AppReady}
			} else {
				status.Phase = AppReconcilePending
			}
		} else if previous, exists := s.live.appReadiness[status.AppID]; exists && status.Phase == AppSecretPending {
			status = previous
		}
		readiness[status.AppID] = status
	}
	s.live.generations = generations
	s.live.acceptedRevision = revision
	s.resetCorrectionLocked()
	s.live.appReadiness = readiness
	s.live.specs = slices.Clone(specs)
	document := cloneDocument(s.live.document)
	s.live.mu.Unlock()
	s.checkpoints.reconcile(document)
	if err := s.finalizeAccepted(ctx, epoch, revision); err != nil {
		s.live.mu.Lock()
		if !s.live.closing && s.live.epoch == epoch && s.live.acceptedRevision == revision {
			s.scheduleCorrectionLocked(revision)
		}
		s.live.mu.Unlock()
		s.wakeSecretRetry()
		return err
	}
	s.wakeSecretRetry()
	return nil
}

// finalizeAccepted is the post-plugin transaction boundary. It never holds
// the LiveState lock across attention or device I/O and rechecks ownership before
// asset reconciliation so an older disable cannot remove a newer enable's
// assets.
func (s *Reconciler) finalizeAccepted(ctx context.Context, epoch, revision uint64) error {
	s.live.mu.RLock()
	if s.live.closing || s.live.epoch != epoch || s.live.acceptedRevision != revision {
		s.live.mu.RUnlock()
		return context.Canceled
	}
	controller := s.attentionController
	document := cloneDocument(s.live.document)
	s.live.mu.RUnlock()
	if controller != nil {
		if err := controller.Reconcile(ctx); err != nil {
			return fmt.Errorf("reconcile attention after plugin update: %w", err)
		}
	}
	s.assetMu.Lock()
	defer s.assetMu.Unlock()
	s.live.mu.Lock()
	current := !s.live.closing && s.live.epoch == epoch && s.live.acceptedRevision == revision
	assetController := s.assetController
	s.live.mu.Unlock()
	if !current {
		return context.Canceled
	}
	if assetController != nil {
		assetController.Reconcile(ctx, assetPackages(document))
	}
	s.live.mu.Lock()
	if !s.live.closing && s.live.epoch == epoch && s.live.acceptedRevision == revision {
		s.live.finalizedRevision = revision
	}
	current = s.live.finalizedRevision == revision
	s.live.mu.Unlock()
	if !current {
		return context.Canceled
	}
	return nil
}

func (s *Reconciler) attemptCurrent(epoch, revision, attemptID uint64) bool {
	s.live.mu.RLock()
	defer s.live.mu.RUnlock()
	return !s.live.closing && s.live.epoch == epoch && s.live.activeAttempt == attemptID && s.live.candidate != nil && s.live.candidate.revision == revision
}

func (s *Reconciler) resetCorrectionLocked() {
	s.live.correctionRevision = 0
	s.live.correctionAttempt = 0
	s.live.correctionRetryAt = time.Time{}
}

func (s *Reconciler) scheduleCorrectionLocked(revision uint64) {
	if s.live.correctionRevision == revision && !s.live.correctionRetryAt.IsZero() {
		return
	}
	s.live.correctionRevision = revision
	s.live.correctionAttempt = 0
	s.live.correctionRetryAt = time.Now()
}
