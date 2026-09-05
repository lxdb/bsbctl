package daemon

import (
	"context"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func (s *Reconciler) AttentionRule(record observation.Record) (attention.Rule, bool) {
	return s.policy.Resolve(record)
}

func (s *Reconciler) Foreground() string {
	return s.sessions.Foreground()
}

func (s *Reconciler) ForegroundSession() (string, string) {
	return s.sessions.ForegroundSession()
}

func (s *Reconciler) ForegroundSessionRef() (protocol.InstanceRef, string) {
	return s.sessions.ForegroundSessionRef()
}

func (s *Reconciler) BeginLauncherAdmission() (uint64, bool) {
	return s.sessions.BeginLauncherAdmission()
}

func (s *Reconciler) LauncherAdmissionCurrent(sequence uint64) bool {
	return s.sessions.LauncherAdmissionCurrent(sequence)
}

// AcquireCritical revokes non-atomic foreground ownership before a critical
// actionable observation reaches the physical renderer.
func (s *Reconciler) AcquireCritical(ctx context.Context, candidate presentation.Candidate) bool {
	return s.sessions.AcquireCritical(ctx, candidate)
}

func (s *Reconciler) ReleaseCritical() { s.sessions.ReleaseCritical() }

func (s *Reconciler) CriticalPresentationOwned() bool {
	return s.sessions.CriticalPresentationOwned()
}

// ClearForeground cancels the active session. When expected is non-empty it
// only clears that exact instance, which makes broker-overrun handling race-safe.
func (s *Reconciler) ClearForeground(expected string) {
	s.ClearForegroundContext(context.Background(), expected)
}

func (s *Reconciler) ClearForegroundContext(ctx context.Context, expected string) {
	s.clearForegroundContext(ctx, expected, "")
}

// ClearForegroundSessionContext invalidates only the exact foreground token.
func (s *Reconciler) ClearForegroundSessionContext(ctx context.Context, expected, token string) {
	if expected == "" || token == "" {
		return
	}
	s.clearForegroundContext(ctx, expected, pluginhost.SessionToken(token))
}

func (s *Reconciler) clearForegroundContext(ctx context.Context, expected string, expectedToken pluginhost.SessionToken) {
	changed := s.sessions.ClearForeground(ctx, expected, expectedToken)
	s.live.mu.RLock()
	controller := s.attentionController
	s.live.mu.RUnlock()
	if changed {
		if controller != nil {
			controller.Wake()
		}
	}
}

func (s *Reconciler) endSession(ctx context.Context, pluginID string, target protocol.InstanceRef, token pluginhost.SessionToken) error {
	return s.sessions.EndSession(ctx, sessionTermination{
		pluginID: pluginID, instanceID: target.ID, generation: target.Generation, token: token,
	})
}
