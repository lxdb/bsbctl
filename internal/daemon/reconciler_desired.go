package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func (s *Reconciler) Load(ctx context.Context) error {
	s.desired.mu.Lock()
	document, err := s.desired.store.Load()
	if err != nil {
		s.desired.mu.Unlock()
		return err
	}
	s.live.mu.RLock()
	validate := s.desired.validator
	s.live.mu.RUnlock()
	if err := validateDesiredState(document, validate); err != nil {
		s.desired.mu.Unlock()
		return fmt.Errorf("validate desired state: %w", err)
	}
	epoch, retained, accepted := s.acceptDocument(document)
	s.desired.mu.Unlock()
	if !accepted {
		return errors.New("daemon is closing")
	}
	s.startSecretRetryLoop()
	s.reconcileEnabledAssetsForEpoch(ctx, document, epoch)
	err = s.resolveAndApply(ctx, epoch, document, retained, nil, nil, true)
	// A valid desired state remains controllable while runtime reconciliation
	// retries in the background.
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return nil
}

func (s *Reconciler) SetEnabled(ctx context.Context, appID string, enabled bool) (EnableResult, error) {
	if err := s.lockDesiredApp(ctx, appID); err != nil {
		return EnableResult{}, err
	}
	s.live.mu.RLock()
	if !s.live.loaded {
		s.live.mu.RUnlock()
		s.desired.mu.Unlock()
		return EnableResult{}, errors.New("daemon configuration is not loaded")
	}
	current := cloneDocument(s.live.document)
	expectedGeneration := s.live.document.Generation
	retained := readyInstancesFromSpecs(s.live.specs)
	validate := s.desired.validator
	s.live.mu.RUnlock()
	app, exists := current.Apps[appID]
	if !exists {
		s.desired.mu.Unlock()
		return EnableResult{}, fmt.Errorf("%w: %s", ErrAppNotFound, appID)
	}
	if app.Enabled == enabled {
		s.desired.mu.Unlock()
		return EnableResult{Document: current, Outcome: localstate.NotCommitted}, nil
	}
	candidate := cloneDocument(current)
	app.Enabled = enabled
	candidate.Apps[appID] = app
	candidate.Generation++
	if err := validateDesiredState(candidate, validate); err != nil {
		s.desired.mu.Unlock()
		return EnableResult{}, fmt.Errorf("%w", ErrInvalidAppConfiguration)
	}
	next, outcome, commitErr := s.desired.store.Update(expectedGeneration, func(document *config.Document) error {
		app, exists := document.Apps[appID]
		if !exists {
			return fmt.Errorf("%w: %s", ErrAppNotFound, appID)
		}
		app.Enabled = enabled
		document.Apps[appID] = app
		return nil
	})
	if !outcome.IsCommitted() {
		s.desired.mu.Unlock()
		return EnableResult{Document: next, Outcome: outcome}, commitErr
	}
	result := EnableResult{Document: cloneDocument(next), Changed: true, Outcome: outcome}
	epoch, _, accepted := s.acceptDocument(next)
	s.desired.recordOutcome(outcome)
	var terminations []sessionTermination
	var wake AttentionController
	if !enabled {
		terminations, wake = s.detachAppSessions(app)
	}
	for _, termination := range terminations {
		_ = s.endSession(ctx, termination.pluginID, protocol.InstanceRef{ID: termination.instanceID, Generation: termination.generation}, termination.token)
	}
	if wake != nil {
		wake.Wake()
	}
	s.desired.mu.Unlock()
	if !accepted {
		result.ReconciliationError = errors.New("desired state persisted but daemon is closing")
		return result, nil
	}
	// Enables need their assets before the child becomes eligible. Disables do
	// the inverse in finalizeAccepted: withdraw, clear/draw, then remove.
	if enabled {
		s.reconcileEnabledAssetsForEpoch(ctx, next, epoch)
	}
	attempted := map[string]bool{}
	if enabled {
		attempted[appID] = true
	}
	err := s.resolveAndApply(ctx, epoch, next, retained, func(candidate string) bool { return attempted[candidate] }, attempted, true)
	if err = s.operationReconciliationError(ctx, epoch, err); err != nil {
		result.ReconciliationError = fmt.Errorf("desired state persisted but plugin reconciliation failed: %w", err)
	}
	return result, nil
}

// CreateAppInstance atomically adds one complete app definition and reconciles
// the exact committed generation. Plugin package metadata remains immutable.
func (s *Reconciler) CreateAppInstance(ctx context.Context, app config.App) (AppInstanceResult, error) {
	app.Generation = 0
	if err := s.lockDesiredApp(ctx, app.ID); err != nil {
		return AppInstanceResult{}, err
	}
	s.live.mu.RLock()
	if !s.live.loaded {
		s.live.mu.RUnlock()
		s.desired.mu.Unlock()
		return AppInstanceResult{}, errors.New("daemon configuration is not loaded")
	}
	current := cloneDocument(s.live.document)
	expectedGeneration := s.live.document.Generation
	retained := readyInstancesFromSpecs(s.live.specs)
	validate := s.desired.validator
	s.live.mu.RUnlock()
	if _, exists := current.Apps[app.ID]; exists {
		s.desired.mu.Unlock()
		return AppInstanceResult{}, fmt.Errorf("%w: %s", ErrAppAlreadyExists, app.ID)
	}
	candidate := cloneDocument(current)
	app.Config = append(json.RawMessage(nil), app.Config...)
	app.Secrets = cloneStringMap(app.Secrets)
	app.Policies = clonePolicies(app.Policies)
	candidate.Apps[app.ID] = app
	candidate.Generation++
	candidateApp := candidate.Apps[app.ID]
	candidateApp.Generation = candidate.Generation
	candidate.Apps[app.ID] = candidateApp
	if err := validateDesiredState(candidate, validate); err != nil {
		s.desired.mu.Unlock()
		return AppInstanceResult{}, fmt.Errorf("%w", ErrInvalidAppConfiguration)
	}
	next, outcome, commitErr := s.desired.store.Update(expectedGeneration, func(document *config.Document) error {
		if _, exists := document.Apps[app.ID]; exists {
			return fmt.Errorf("%w: %s", ErrAppAlreadyExists, app.ID)
		}
		document.Apps[app.ID] = app
		return nil
	})
	if !outcome.IsCommitted() {
		s.desired.mu.Unlock()
		return AppInstanceResult{Document: next, AppID: app.ID, PluginID: app.PluginID, Enabled: app.Enabled, Outcome: outcome}, commitErr
	}
	result := AppInstanceResult{
		Document: cloneDocument(next), AppID: app.ID, PluginID: app.PluginID, Enabled: app.Enabled, Outcome: outcome,
	}
	epoch, _, accepted := s.acceptDocument(next)
	s.desired.recordOutcome(outcome)
	s.desired.mu.Unlock()
	if !accepted {
		result.ReconciliationError = errors.New("desired state persisted but daemon is closing")
		return result, nil
	}
	if app.Enabled {
		s.reconcileEnabledAssetsForEpoch(ctx, next, epoch)
	}
	attempted := map[string]bool{}
	if app.Enabled {
		attempted[app.ID] = true
	}
	err := s.resolveAndApply(ctx, epoch, next, retained, func(candidate string) bool { return attempted[candidate] }, attempted, true)
	if err = s.operationReconciliationError(ctx, epoch, err); err != nil {
		result.ReconciliationError = fmt.Errorf("desired state persisted but plugin reconciliation failed: %w", err)
	}
	return result, nil
}

// DeleteAppInstance atomically removes one app definition, reconciles child
// configuration, and drops retained per-instance runtime state.
func (s *Reconciler) DeleteAppInstance(ctx context.Context, appID string) (AppInstanceResult, error) {
	if err := s.lockDesiredApp(ctx, appID); err != nil {
		return AppInstanceResult{}, err
	}
	s.live.mu.RLock()
	if !s.live.loaded {
		s.live.mu.RUnlock()
		s.desired.mu.Unlock()
		return AppInstanceResult{}, errors.New("daemon configuration is not loaded")
	}
	current := cloneDocument(s.live.document)
	expectedGeneration := s.live.document.Generation
	retained := readyInstancesFromSpecs(s.live.specs)
	validate := s.desired.validator
	s.live.mu.RUnlock()
	app, exists := current.Apps[appID]
	if !exists {
		s.desired.mu.Unlock()
		return AppInstanceResult{}, fmt.Errorf("%w: %s", ErrAppNotFound, appID)
	}
	candidate := cloneDocument(current)
	delete(candidate.Apps, appID)
	candidate.Generation++
	if err := validateDesiredState(candidate, validate); err != nil {
		s.desired.mu.Unlock()
		return AppInstanceResult{}, fmt.Errorf("%w", ErrInvalidAppConfiguration)
	}
	next, outcome, commitErr := s.desired.store.Update(expectedGeneration, func(document *config.Document) error {
		if _, exists := document.Apps[appID]; !exists {
			return fmt.Errorf("%w: %s", ErrAppNotFound, appID)
		}
		delete(document.Apps, appID)
		return nil
	})
	if !outcome.IsCommitted() {
		s.desired.mu.Unlock()
		return AppInstanceResult{Document: next, AppID: app.ID, PluginID: app.PluginID, Enabled: app.Enabled, Outcome: outcome}, commitErr
	}
	result := AppInstanceResult{
		Document: cloneDocument(next), AppID: app.ID, PluginID: app.PluginID, Enabled: app.Enabled, Outcome: outcome,
	}
	finishRetirement := s.appRetirements.begin(appID)
	defer finishRetirement()
	epoch, _, accepted := s.acceptDocument(next)
	s.desired.recordOutcome(outcome)
	s.desired.mu.Unlock()
	terminations, wake := s.detachAppSessions(app)
	for _, termination := range terminations {
		_ = s.endSession(ctx, termination.pluginID, protocol.InstanceRef{ID: termination.instanceID, Generation: termination.generation}, termination.token)
	}
	if wake != nil {
		wake.Wake()
	}
	if !accepted {
		result.ReconciliationError = errors.New("desired state persisted but daemon is closing")
		return result, nil
	}
	err := s.operationReconciliationError(ctx, epoch, s.resolveAndApply(ctx, epoch, next, retained, nil, nil, true))
	if err != nil {
		result.ReconciliationError = fmt.Errorf("desired state persisted but plugin reconciliation failed: %w", err)
	}
	return result, nil
}

func validateDesiredState(document config.Document, validate DesiredStateValidator) error {
	if err := document.Validate(); err != nil {
		return err
	}
	if validate != nil {
		return validate(document)
	}
	return nil
}

// detachAppSessions atomically invalidates admission and foreground ownership.
// A pending Invoke performs its compensating EndSession after it returns;
// already-active sessions are returned for termination outside the LiveState lock.
func (s *Reconciler) detachAppSessions(app config.App) ([]sessionTermination, AttentionController) {
	termination, changed := s.sessions.detach(app)
	terminations := make([]sessionTermination, 0, 1)
	if termination.token != "" {
		terminations = append(terminations, termination)
	}
	s.live.mu.RLock()
	controller := s.attentionController
	s.live.mu.RUnlock()
	if !changed {
		return nil, nil
	}
	return terminations, controller
}

func (s *Reconciler) operationReconciliationError(ctx context.Context, epoch uint64, reconcileErr error) error {
	ctxErr := ctx.Err()
	if reconcileErr == nil && ctxErr == nil {
		return nil
	}
	s.live.mu.Lock()
	if s.live.closing || s.live.epoch != epoch {
		s.live.mu.Unlock()
		return nil
	}
	if ctxErr != nil {
		if s.live.candidate != nil && s.live.candidate.epoch == epoch {
			retryOwned := s.live.correctionRevision == s.live.candidate.revision && !s.live.correctionRetryAt.IsZero()
			for _, status := range s.live.appReadiness {
				retryOwned = retryOwned || !status.RetryAt.IsZero()
			}
			if !retryOwned {
				s.scheduleCorrectionLocked(s.live.candidate.revision)
			}
		} else {
			now := time.Now()
			for appID, status := range s.live.appReadiness {
				app, exists := s.live.document.Apps[appID]
				if !exists || !app.Enabled || !status.RetryAt.IsZero() {
					continue
				}
				status.Phase = AppReconcilePending
				status.Attempt++
				status.RetryAt = now
				status.LastErrorCode = reconcileFailedCode
				s.live.appReadiness[appID] = status
			}
		}
	}
	s.live.mu.Unlock()
	s.wakeSecretRetry()
	if ctxErr != nil {
		return ctxErr
	}
	return reconcileErr
}

// ReplaceAppConfiguration atomically replaces all configurable fields for one
// app and reconciles the exact committed generation.
func (s *Reconciler) ReplaceAppConfiguration(ctx context.Context, appID string, replacement AppConfiguration) (config.Document, localstate.CommitOutcome, error) {
	if err := s.lockDesiredApp(ctx, appID); err != nil {
		return config.Document{}, localstate.NotCommitted, err
	}
	s.live.mu.RLock()
	if !s.live.loaded {
		s.live.mu.RUnlock()
		s.desired.mu.Unlock()
		return config.Document{}, localstate.NotCommitted, errors.New("daemon configuration is not loaded")
	}
	current := cloneDocument(s.live.document)
	expectedGeneration := s.live.document.Generation
	retained := readyInstancesFromSpecs(s.live.specs)
	validate := s.desired.validator
	s.live.mu.RUnlock()
	if _, exists := current.Apps[appID]; !exists {
		s.desired.mu.Unlock()
		return config.Document{}, localstate.NotCommitted, fmt.Errorf("%w: %s", ErrAppNotFound, appID)
	}
	candidate := cloneDocument(current)
	applyAppConfiguration(&candidate, appID, replacement)
	candidate.Generation++
	if err := validateDesiredState(candidate, validate); err != nil {
		s.desired.mu.Unlock()
		return config.Document{}, localstate.NotCommitted, fmt.Errorf("%w", ErrInvalidAppConfiguration)
	}
	next, outcome, commitErr := s.desired.store.Update(expectedGeneration, func(document *config.Document) error {
		if _, exists := document.Apps[appID]; !exists {
			return fmt.Errorf("%w: %s", ErrAppNotFound, appID)
		}
		applyAppConfiguration(document, appID, replacement)
		return nil
	})
	if !outcome.IsCommitted() {
		s.desired.mu.Unlock()
		return config.Document{}, outcome, commitErr
	}
	epoch, _, accepted := s.acceptDocument(next)
	s.desired.recordOutcome(outcome)
	s.desired.mu.Unlock()
	if !accepted {
		return cloneDocument(next), outcome, errors.New("desired state persisted but daemon is closing")
	}
	attempted := map[string]bool{appID: true}
	if err := s.operationReconciliationError(ctx, epoch, s.resolveAndApply(ctx, epoch, next, retained, func(candidate string) bool { return attempted[candidate] }, attempted, true)); err != nil {
		return cloneDocument(next), outcome, fmt.Errorf("desired state persisted but plugin reconciliation failed: %w", err)
	}
	return cloneDocument(next), outcome, nil
}

func applyAppConfiguration(document *config.Document, appID string, replacement AppConfiguration) {
	app := document.Apps[appID]
	app.Config = append(json.RawMessage(nil), replacement.Config...)
	app.Secrets = cloneStringMap(replacement.Secrets)
	app.Policies = clonePolicies(replacement.Policies)
	app.LaunchAction = replacement.LaunchAction
	document.Apps[appID] = app
}

// DesiredPlugin returns the exact current verified package configuration.
func (s *Reconciler) DesiredPlugin(_ context.Context, pluginID string) (*config.Plugin, error) {
	s.live.mu.RLock()
	defer s.live.mu.RUnlock()
	if !s.live.loaded {
		return nil, errors.New("daemon configuration is not loaded")
	}
	plugin, exists := s.live.document.Plugins[pluginID]
	if !exists {
		return nil, nil
	}
	copy := clonePlugin(plugin)
	return &copy, nil
}

// ActivatePlugin atomically changes only one verified package and reconciles
// app instances against the committed desired state.
func (s *Reconciler) ActivatePlugin(ctx context.Context, plugin config.Plugin) (localstate.CommitOutcome, error) {
	s.desired.mu.Lock()
	s.live.mu.RLock()
	if !s.live.loaded {
		s.live.mu.RUnlock()
		s.desired.mu.Unlock()
		return localstate.NotCommitted, errors.New("daemon configuration is not loaded")
	}
	expectedGeneration := s.live.document.Generation
	retained := readyInstancesFromSpecs(s.live.specs)
	s.live.mu.RUnlock()
	next, outcome, commitErr := s.desired.store.Update(expectedGeneration, func(document *config.Document) error {
		document.Plugins[plugin.ID] = clonePlugin(plugin)
		return nil
	})
	if !outcome.IsCommitted() {
		s.desired.mu.Unlock()
		return outcome, commitErr
	}
	epoch, _, accepted := s.acceptDocument(next)
	s.desired.recordOutcome(outcome)
	s.desired.mu.Unlock()
	if !accepted {
		return outcome, errors.New("desired state persisted but daemon is closing")
	}
	packages := assetPackages(next)
	if err := s.quiesceUnreadyPackagePlugins(ctx, epoch, packages); err != nil {
		return outcome, fmt.Errorf("desired state persisted but plugin quiescence failed: %w", err)
	}
	s.reconcileEnabledAssetsForEpoch(ctx, next, epoch)
	if err := s.operationReconciliationError(ctx, epoch, s.resolveAndApply(ctx, epoch, next, retained, nil, nil, true)); err != nil {
		return outcome, fmt.Errorf("desired state persisted but plugin reconciliation failed: %w", err)
	}
	s.collectAssetGarbage(ctx, next)
	return outcome, commitErr
}
