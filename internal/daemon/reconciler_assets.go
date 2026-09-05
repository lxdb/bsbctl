package daemon

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/eventbus"
	"github.com/lxdb/bsbctl/internal/pluginhost"
)

func (s *Reconciler) AssetStatus() []assets.State {
	return s.assetController.Status()
}

func (s *Reconciler) SessionInputStatus() []eventbus.Status {
	return s.sessions.InputStatus()
}

// ReconcileAssets retries desired asset state and starts newly ready children.
func (s *Reconciler) ReconcileAssets(ctx context.Context) error {
	s.live.mu.RLock()
	document := cloneDocument(s.live.document)
	loaded := s.live.loaded
	epoch := s.live.epoch
	s.live.mu.RUnlock()
	if !loaded {
		return nil
	}
	packages := assetPackages(document)
	if err := s.quiesceUnreadyPackagePlugins(ctx, epoch, packages); err != nil {
		return err
	}
	s.assetMu.Lock()
	s.live.mu.RLock()
	current := s.live.epoch == epoch
	finalized := s.live.acceptedRevision != 0 && s.live.finalizedRevision == s.live.acceptedRevision
	controller := s.assetController
	s.live.mu.RUnlock()
	if current {
		if !finalized {
			packages = enabledPackages(packages)
		}
		controller.Reconcile(ctx, packages)
	}
	s.assetMu.Unlock()
	// Asset I/O may have overlapped secret recovery. Snapshot only after that
	// I/O and never replace an in-flight resolution attempt.
	s.live.mu.RLock()
	candidate := cloneCandidate(s.live.candidate)
	busy := s.live.activeAttempt != 0
	s.live.mu.RUnlock()
	if candidate == nil || busy {
		return nil
	}
	if err := s.applyPlan(ctx, candidate.epoch, candidate.revision, candidate.plan, false); err != nil {
		return err
	}
	s.collectAssetGarbage(ctx, document)
	return nil
}

func (s *Reconciler) collectAssetGarbage(ctx context.Context, document config.Document) {
	s.assetController.CollectGarbage(ctx, assetPackages(document))
}

func (s *Reconciler) reconcileEnabledAssetsForEpoch(ctx context.Context, document config.Document, epoch uint64) {
	s.assetMu.Lock()
	defer s.assetMu.Unlock()
	s.live.mu.RLock()
	controller := s.assetController
	current := s.live.epoch == epoch
	s.live.mu.RUnlock()
	if controller != nil && current {
		controller.Reconcile(ctx, enabledPackages(assetPackages(document)))
	}
}

func enabledPackages(packages []assets.Package) []assets.Package {
	result := packages[:0]
	for _, value := range packages {
		if value.Enabled {
			result = append(result, value)
		}
	}
	return result
}

func (s *Reconciler) readySpecs(specs []pluginhost.Spec) []pluginhost.Spec {
	s.live.mu.RLock()
	controller := s.assetController
	document := cloneDocument(s.live.document)
	s.live.mu.RUnlock()
	result := make([]pluginhost.Spec, 0, len(specs))
	packages := make(map[string]assets.Package)
	for _, value := range assetPackages(document) {
		packages[value.PluginID] = value
	}
	for _, spec := range specs {
		value, exists := packages[spec.ID]
		if exists && controller.ReadyFor(value) {
			result = append(result, spec)
		}
	}
	return result
}

func (s *Reconciler) quiesceUnreadyPackagePlugins(ctx context.Context, epoch uint64, packages []assets.Package) error {
	s.live.mu.RLock()
	controller := s.assetController
	currentSpecs := slices.Clone(s.live.specs)
	current := !s.live.closing && s.live.epoch == epoch
	attentionController := s.attentionController
	s.live.mu.RUnlock()
	if !current {
		return nil
	}
	blocked := make(map[string]struct{})
	for _, value := range packages {
		if value.Enabled && len(value.Assets) > 0 && !controller.ReadyFor(value) {
			blocked[value.PluginID] = struct{}{}
		}
	}
	if len(blocked) == 0 {
		return nil
	}
	filtered := make([]pluginhost.Spec, 0, len(currentSpecs))
	changed := false
	for _, spec := range currentSpecs {
		if _, mustQuiesce := blocked[spec.ID]; mustQuiesce {
			changed = true
			continue
		}
		filtered = append(filtered, spec)
	}
	if !changed {
		return nil
	}
	if err := s.plugins.Apply(ctx, filtered); err != nil {
		return fmt.Errorf("quiesce plugin before asset reconciliation: %w", err)
	}
	s.live.mu.Lock()
	if s.live.closing || s.live.epoch != epoch {
		s.live.mu.Unlock()
		return context.Canceled
	}
	s.live.specs = slices.Clone(filtered)
	s.live.generations = generationsFromSpecs(filtered)
	for appID, status := range s.live.appReadiness {
		if _, mustQuiesce := blocked[status.PluginID]; mustQuiesce {
			status.Phase = AppReconcilePending
			s.live.appReadiness[appID] = status
		}
	}
	s.live.mu.Unlock()
	if attentionController != nil {
		if err := attentionController.Reconcile(ctx); err != nil {
			return fmt.Errorf("reconcile attention after asset quiescence: %w", err)
		}
	}
	return nil
}

func generationsFromSpecs(specs []pluginhost.Spec) Generations {
	result := Generations{values: make(map[generationKey]uint64)}
	for _, spec := range specs {
		for _, instance := range spec.Instances {
			result.values[generationKey{pluginID: spec.ID, instanceID: instance.ID}] = instance.Generation
		}
	}
	return result
}

func assetPackages(document config.Document) []assets.Package {
	result := make([]assets.Package, 0, len(document.Plugins))
	pluginIDs := slices.Sorted(maps.Keys(document.Plugins))
	for _, id := range pluginIDs {
		value, _ := assetPackage(document, id)
		result = append(result, value)
	}
	return result
}

func assetPackage(document config.Document, pluginID string) (assets.Package, bool) {
	plugin, exists := document.Plugins[pluginID]
	if !exists {
		return assets.Package{}, false
	}
	enabled := false
	for _, app := range document.Apps {
		if app.PluginID == pluginID && app.Enabled {
			enabled = true
			break
		}
	}
	root := plugin.PackageRoot
	if root == "" {
		root = filepath.Dir(plugin.Executable)
	}
	return assets.Package{
		PluginID: plugin.ID, Version: plugin.Version, Root: root, Enabled: enabled,
		Assets: slices.Clone(plugin.Assets),
	}, true
}
