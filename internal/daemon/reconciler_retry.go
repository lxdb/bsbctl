package daemon

import (
	"maps"
	"slices"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/eventbus"
	"github.com/lxdb/bsbctl/internal/pluginhost"
)

func (s *Reconciler) wakeSecretRetry() {
	select {
	case s.retryWake <- struct{}{}:
	default:
	}
}

func (s *Reconciler) startSecretRetryLoop() {
	s.live.mu.Lock()
	if s.live.closing || s.retryStarted {
		s.live.mu.Unlock()
		return
	}
	s.retryStarted = true
	s.retryDone = make(chan struct{})
	done := s.retryDone
	s.live.mu.Unlock()
	go s.secretRetryLoop(done)
}

func (s *Reconciler) secretRetryLoop(done chan<- struct{}) {
	defer close(done)
	for {
		retryAt, pending := s.nextRetry()
		if !pending {
			select {
			case <-s.retryCtx.Done():
				return
			case <-s.retryWake:
				continue
			}
		}
		delay := time.Until(retryAt)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-s.retryCtx.Done():
			stopTimer(timer)
			return
		case <-s.retryWake:
			stopTimer(timer)
			continue
		case <-timer.C:
		}

		s.runDueRetry()
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (s *Reconciler) nextRetry() (time.Time, bool) {
	s.live.mu.RLock()
	defer s.live.mu.RUnlock()
	if s.live.activeAttempt != 0 {
		return time.Time{}, false
	}
	var earliest time.Time
	if s.live.correctionRevision != 0 && !s.live.correctionRetryAt.IsZero() {
		earliest = s.live.correctionRetryAt
	}
	for _, status := range s.live.appReadiness {
		if status.RetryAt.IsZero() {
			continue
		}
		if earliest.IsZero() || status.RetryAt.Before(earliest) {
			earliest = status.RetryAt
		}
	}
	return earliest, !earliest.IsZero()
}

func (s *Reconciler) runDueRetry() {
	now := time.Now()
	s.live.mu.RLock()
	if s.live.closing || !s.live.loaded || s.live.activeAttempt != 0 {
		s.live.mu.RUnlock()
		return
	}
	epoch := s.live.epoch
	document := cloneDocument(s.live.document)
	candidate := s.live.candidate
	corrective := candidate != nil && s.live.correctionRevision == candidate.revision && !s.live.correctionRetryAt.After(now)
	dueSecrets := make(map[string]bool)
	retryApply := false
	for _, status := range s.live.appReadiness {
		if status.RetryAt.IsZero() || status.RetryAt.After(now) {
			continue
		}
		if status.Phase == AppSecretPending {
			dueSecrets[status.AppID] = true
		}
		if status.Phase == AppReconcilePending && status.LastErrorCode == reconcileFailedCode {
			retryApply = true
		}
	}
	retainedSpecs := s.live.specs
	if candidate != nil && candidate.epoch == epoch {
		retainedSpecs = candidate.plan.Specs
	}
	retained := readyInstancesFromSpecs(retainedSpecs)
	var plan ReconciliationPlan
	if candidate != nil {
		plan = candidate.plan
	}
	s.live.mu.RUnlock()
	if len(dueSecrets) != 0 {
		_ = s.resolveAndApply(s.retryCtx, epoch, document, retained, func(appID string) bool { return dueSecrets[appID] }, dueSecrets, false)
		return
	}
	if retryApply && candidate == nil {
		_ = s.resolveAndApply(s.retryCtx, epoch, document, retained, nil, nil, false)
		return
	}
	if (retryApply || corrective) && candidate != nil && candidate.epoch == epoch {
		_ = s.applyPlan(s.retryCtx, epoch, candidate.revision, plan, false)
	}
}

func (s *Reconciler) acceptDocument(document config.Document) (uint64, map[string]pluginhost.Instance, bool) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	s.live.mu.Lock()
	if s.live.closing {
		s.live.mu.Unlock()
		return 0, nil, false
	}
	wasLoaded := s.live.loaded
	// Reconciliation may prune the child generation that received an Invoke
	// before Invoke returns. Invalidate only the pending admission here; an
	// already-promoted foreground remains owned until its exact app incarnation
	// becomes invalid or the supervisor reports its session invalidated.
	previousDocument := s.live.document
	retained := readyInstancesFromSpecs(s.live.specs)
	activeGenerations := s.live.generations
	if s.live.candidate != nil {
		retained = readyInstancesFromSpecs(s.live.candidate.plan.Specs)
	}
	if s.live.activeCancel != nil {
		s.live.activeCancel()
	}
	s.live.epoch++
	epoch := s.live.epoch
	s.live.document = cloneDocument(document)
	preservedGenerations := Generations{values: make(map[generationKey]uint64)}
	s.live.acceptedRevision = 0
	s.live.finalizedRevision = 0
	s.live.candidate = nil
	s.resetCorrectionLocked()
	s.live.loaded = true
	readiness := make(map[string]AppReadiness, len(document.Apps))
	for _, app := range document.Apps {
		phase := AppReconcilePending
		if !app.Enabled {
			phase = AppDisabled
		} else if activeGenerations.matches(app.PluginID, app.ID, app.Generation) {
			preservedGenerations.values[generationKey{pluginID: app.PluginID, instanceID: app.ID}] = app.Generation
		}
		previousApp, sameID := previousDocument.Apps[app.ID]
		if current, exists := s.live.appReadiness[app.ID]; exists && sameID && previousApp.PluginID == app.PluginID && previousApp.Generation == app.Generation {
			readiness[app.ID] = current
			continue
		}
		readiness[app.ID] = AppReadiness{AppID: app.ID, PluginID: app.PluginID, Phase: phase}
	}
	s.live.generations = preservedGenerations
	s.live.appReadiness = readiness
	attentionController := s.attentionController
	s.live.mu.Unlock()
	s.sessions.ApplyConfiguration(document)
	if wasLoaded && attentionController != nil {
		attentionController.Wake()
	}
	s.wakeSecretRetry()
	return epoch, retained, true
}

func sessionInputTargets(document config.Document) []eventbus.TargetSet {
	instances := make(map[string][]string)
	for _, app := range document.Apps {
		instances[app.PluginID] = append(instances[app.PluginID], app.ID)
	}
	pluginIDs := slices.Sorted(maps.Keys(document.Plugins))
	result := make([]eventbus.TargetSet, 0, len(pluginIDs))
	for _, id := range pluginIDs {
		slices.Sort(instances[id])
		result = append(result, eventbus.TargetSet{PluginID: id, InstanceIDs: slices.Clone(instances[id])})
	}
	return result
}
