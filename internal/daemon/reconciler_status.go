package daemon

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func (s *Reconciler) AttentionSnapshot() (attention.Trace, bool) {
	s.live.mu.RLock()
	controller := s.attentionController
	s.live.mu.RUnlock()
	if controller == nil {
		return attention.Trace{}, false
	}
	return controller.AttentionSnapshot()
}
func (s *Reconciler) AttentionExplain(id string) (attention.Evaluation, bool) {
	s.live.mu.RLock()
	controller := s.attentionController
	s.live.mu.RUnlock()
	if controller == nil {
		return attention.Evaluation{}, false
	}
	return controller.AttentionExplain(id)
}
func (s *Reconciler) AttentionHistory(limit int, since time.Time) []attention.Trace {
	s.live.mu.RLock()
	controller := s.attentionController
	s.live.mu.RUnlock()
	if controller == nil {
		return nil
	}
	return controller.AttentionHistory(limit, since)
}

func (s *Reconciler) RecorderStatus() attention.RecorderStatus {
	s.live.mu.RLock()
	controller := s.attentionController
	s.live.mu.RUnlock()
	if controller == nil {
		return attention.RecorderStatus{Phase: attention.RecorderUnavailable}
	}
	return controller.RecorderStatus()
}

func (s *Reconciler) AcknowledgeAttention(id string) error {
	s.live.mu.RLock()
	controller := s.attentionController
	s.live.mu.RUnlock()
	if controller == nil {
		return errors.New("attention controller is not configured")
	}
	return controller.AcknowledgeAttention(id)
}

func (s *Reconciler) Wake() {
	s.live.mu.RLock()
	controller := s.attentionController
	s.live.mu.RUnlock()
	if controller != nil {
		controller.Wake()
	}
}

func (s *Reconciler) Reconcile(ctx context.Context) error {
	s.live.mu.RLock()
	controller := s.attentionController
	s.live.mu.RUnlock()
	if controller == nil {
		return errors.New("attention controller is not configured")
	}
	return controller.Reconcile(ctx)
}

func (s *Reconciler) AttentionConfigured() bool {
	s.live.mu.RLock()
	defer s.live.mu.RUnlock()
	return s.attentionController != nil
}

// LaunchableApps returns ready interactive app instances with an explicit launcher action.
func (s *Reconciler) LaunchableApps() []LaunchableApp {
	s.live.mu.RLock()
	defer s.live.mu.RUnlock()
	result := make([]LaunchableApp, 0, len(s.live.document.Apps))
	for _, app := range s.live.document.Apps {
		if !app.Enabled {
			continue
		}
		generation, ready := s.live.generations.Lookup(app.PluginID, app.ID)
		if !ready || generation != app.Generation {
			continue
		}
		plugin := s.live.document.Plugins[app.PluginID]
		interactive := false
		for _, mode := range plugin.ExecutionModes {
			if mode == protocol.ExecutionModeInteractive {
				interactive = true
				break
			}
		}
		interactivePolicy := false
		for _, policy := range app.Policies {
			if policy.Policy == presentation.PolicyInteractive {
				interactivePolicy = true
				break
			}
		}
		if interactive && interactivePolicy && app.LaunchAction != "" {
			result = append(result, LaunchableApp{ID: app.ID, PluginID: app.PluginID, Action: app.LaunchAction})
		}
	}
	slices.SortFunc(result, func(left, right LaunchableApp) int { return cmp.Compare(left.ID, right.ID) })
	return result
}

func (s *Reconciler) Generation(pluginID, instanceID string) (uint64, bool) {
	return s.live.Generation(pluginID, instanceID)
}

func (s *Reconciler) AppReadiness() []AppReadiness {
	return s.live.AppReadiness()
}

func (s *Reconciler) Document() (config.Document, bool) {
	return s.live.Document()
}

func (s *Reconciler) Status() []pluginhost.PluginStatus { return s.plugins.Status() }

func (s *Reconciler) PresentationCooldownStatus() PresentationCooldownDiagnostics {
	s.live.mu.RLock()
	controller := s.attentionController
	s.live.mu.RUnlock()
	if controller != nil {
		return controller.PresentationCooldownStatus()
	}
	return PresentationCooldownDiagnostics{}
}

func (s *Reconciler) ObservationStatus() observation.StoreDiagnostics {
	s.live.mu.RLock()
	controller := s.attentionController
	s.live.mu.RUnlock()
	if controller != nil {
		return controller.ObservationDiagnostics()
	}
	return observation.StoreDiagnostics{}
}

func (s *Reconciler) AttentionStateStatus() AttentionStateDiagnostics {
	s.live.mu.RLock()
	controller := s.attentionController
	s.live.mu.RUnlock()
	if controller != nil {
		return controller.AttentionStateStatus()
	}
	return AttentionStateDiagnostics{}
}

func (s *Reconciler) ObservationDiagnostics() observation.StoreDiagnostics {
	return s.ObservationStatus()
}
