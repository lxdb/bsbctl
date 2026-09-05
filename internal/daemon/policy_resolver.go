package daemon

import (
	"errors"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type assetReadiness interface {
	ReadyFor(assets.Package) bool
}

// PolicyResolver translates authenticated observations and current daemon
// state into core-owned attention policy.
type PolicyResolver struct {
	live     *LiveState
	sessions *SessionCoordinator
	assets   assetReadiness
}

func NewPolicyResolver(live *LiveState, sessions *SessionCoordinator, assets assetReadiness) (*PolicyResolver, error) {
	if live == nil {
		return nil, errors.New("live state is required")
	}
	if sessions == nil {
		return nil, errors.New("session coordinator is required")
	}
	if assets == nil {
		return nil, errors.New("asset readiness is required")
	}
	return &PolicyResolver{live: live, sessions: sessions, assets: assets}, nil
}

func (r *PolicyResolver) Resolve(record observation.Record) (attention.Rule, bool) {
	r.live.mu.RLock()
	if !r.live.loaded {
		r.live.mu.RUnlock()
		return attention.Rule{}, false
	}
	app, exists := r.live.document.Apps[record.Observation.Instance.ID]
	if !exists || app.PluginID != record.PluginID {
		r.live.mu.RUnlock()
		return attention.Rule{}, false
	}
	policy, exists := app.Policies[record.Observation.Channel]
	if !exists {
		r.live.mu.RUnlock()
		return attention.Rule{}, false
	}
	generationCurrent := r.live.generations.matches(record.PluginID, app.ID, record.Generation)
	assetPackage, _ := assetPackage(r.live.document, app.PluginID)
	r.live.mu.RUnlock()
	foreground, suppressed := r.sessions.attentionState(record)
	return attention.Rule{
		Enabled: app.Enabled && generationCurrent && !suppressed, AssetsReady: r.assets.ReadyFor(assetPackage), Policy: policy.Policy,
		DevicePriority: policy.DevicePriority, HoldMS: policy.HoldMS, CooldownMS: policy.CooldownMS,
		RequiresAck: policy.RequiresAck, Foreground: foreground,
		RotationIntervalMS: policy.RotationIntervalMS, RotationJitterPercent: policy.RotationJitterPercent,
		BlockedByAtomicExecution: policy.Policy == presentation.PolicyAttention &&
			record.Observation.Disposition == protocol.DispositionActionable && record.Observation.Impact == protocol.ImpactCritical && r.sessions.blocksCritical(),
	}, true
}
