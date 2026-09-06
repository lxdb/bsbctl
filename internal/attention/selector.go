// Package attention owns core-only eligibility, arbitration, and explanations.
package attention

import (
	"hash/fnv"
	"time"

	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

// Reason is a stable, non-sensitive explanation for one evaluation.
type Reason string

const (
	ReasonSelected                  Reason = "selected"
	ReasonDisabled                  Reason = "disabled"
	ReasonAssetsPending             Reason = "assets_pending"
	ReasonStale                     Reason = "stale"
	ReasonExpired                   Reason = "expired"
	ReasonResolved                  Reason = "resolved"
	ReasonNotActionableForAttention Reason = "not_actionable_for_attention"
	ReasonNotRelevant               Reason = "not_relevant"
	ReasonNotDue                    Reason = "not_due"
	ReasonAcknowledged              Reason = "acknowledged"
	ReasonCooldownUntil             Reason = "cooldown_until"
	ReasonHeldByCurrent             Reason = "held_by_current"
	ReasonLowerBand                 Reason = "lower_band"
	ReasonLowerImpact               Reason = "lower_impact"
	ReasonFairnessWait              Reason = "fairness_wait"
	ReasonForegroundInactive        Reason = "foreground_inactive"
	ReasonBlockedByAtomicExecution  Reason = "blocked_by_atomic_execution"
)

// Rule is core-owned configuration and runtime state for one observation.
type Rule struct {
	Enabled                  bool
	AssetsReady              bool
	Policy                   presentation.Policy
	DevicePriority           int
	HoldMS                   int
	CooldownMS               int
	RequiresAck              bool
	Acknowledged             bool
	Foreground               bool
	RotationIntervalMS       int
	RotationJitterPercent    int
	BlockedByAtomicExecution bool
}

type Resolver func(observation.Record) (Rule, bool)

// Evaluation is safe to expose through status and persist in decision history.
type Evaluation struct {
	ObservationID string               `json:"observation_id"`
	PluginID      string               `json:"plugin_id"`
	InstanceID    string               `json:"instance_id"`
	Channel       string               `json:"channel"`
	Policy        presentation.Policy  `json:"policy,omitempty"`
	Disposition   protocol.Disposition `json:"disposition"`
	Impact        protocol.Impact      `json:"impact"`
	Band          presentation.Band    `json:"band,omitempty"`
	ReasonCode    string               `json:"reason_code"`
	Reason        Reason               `json:"reason"`
	EvaluatedAt   time.Time            `json:"evaluated_at"`
	CooldownUntil time.Time            `json:"cooldown_until,omitempty"`
	NextDue       time.Time            `json:"next_due,omitempty"`
}

type Decision struct {
	Candidate   *presentation.Candidate
	Evaluations []Evaluation
}

type eligibleValue struct {
	record    observation.Record
	rule      Rule
	candidate presentation.Candidate
	index     int
}

// Select applies mode eligibility before delegating deterministic ordering and
// anti-flap mechanics to presentation.Select.
func Select(records []observation.Record, resolve Resolver, history presentation.History, now time.Time) Decision {
	evaluations := make([]Evaluation, len(records))
	eligible := make([]eligibleValue, 0, len(records))
	candidates := make([]presentation.Candidate, 0, len(records))
	for index, record := range records {
		value := record.Observation
		evaluation := Evaluation{
			ObservationID: record.ID(), PluginID: record.PluginID, InstanceID: value.Instance.ID,
			Channel: value.Channel, Disposition: value.Disposition, Impact: value.Impact,
			ReasonCode: value.ReasonCode, EvaluatedAt: now,
		}
		rule, exists := resolve(record)
		evaluation.Policy = rule.Policy
		if !exists || !rule.Enabled {
			evaluation.Reason = ReasonDisabled
			evaluations[index] = evaluation
			continue
		}
		if !rule.AssetsReady {
			evaluation.Reason = ReasonAssetsPending
			evaluations[index] = evaluation
			continue
		}
		if value.Disposition == protocol.DispositionResolved {
			evaluation.Reason = ReasonResolved
			evaluations[index] = evaluation
			continue
		}
		if value.ValidUntil.IsZero() || !now.Before(value.ValidUntil) {
			evaluation.Reason = ReasonExpired
			evaluations[index] = evaluation
			continue
		}
		if rule.RequiresAck && rule.Acknowledged {
			evaluation.Reason = ReasonAcknowledged
			evaluations[index] = evaluation
			continue
		}
		if rule.BlockedByAtomicExecution {
			evaluation.Reason = ReasonBlockedByAtomicExecution
			evaluations[index] = evaluation
			continue
		}
		if reason := ineligibleReason(rule, value.Disposition); reason != "" {
			evaluation.Reason = reason
			evaluations[index] = evaluation
			continue
		}
		candidate := derive(record, rule)
		evaluation.Band = candidate.Band
		lastShown := history.LastShown[candidate.ID()]
		if rule.Policy == presentation.PolicyRotation {
			evaluation.NextDue = rotationNextDue(candidate.ID(), lastShown, rule.RotationIntervalMS, rule.RotationJitterPercent)
			withinCurrentHold := candidate.ID() == history.CurrentID && now.Before(history.CurrentSince.Add(candidate.Hold()))
			if !withinCurrentHold && !evaluation.NextDue.IsZero() && now.Before(evaluation.NextDue) {
				evaluation.Reason = ReasonNotDue
				evaluations[index] = evaluation
				continue
			}
		}
		if candidate.ID() != history.CurrentID && !lastShown.IsZero() && now.Before(lastShown.Add(candidate.Cooldown())) {
			evaluation.Reason = ReasonCooldownUntil
			evaluation.CooldownUntil = lastShown.Add(candidate.Cooldown())
			evaluations[index] = evaluation
			continue
		}
		evaluations[index] = evaluation
		eligible = append(eligible, eligibleValue{record: record, rule: rule, candidate: candidate, index: index})
		candidates = append(candidates, candidate)
	}
	selected := presentation.Select(candidates, history, now).Candidate
	if selected == nil {
		return Decision{Evaluations: evaluations}
	}
	for _, value := range eligible {
		reason := ReasonFairnessWait
		if value.candidate.ID() == selected.ID() {
			reason = ReasonSelected
		} else if presentation.CompareBand(value.candidate.Band, selected.Band) < 0 {
			reason = ReasonLowerBand
		} else if presentation.CompareImpact(value.candidate.Impact, selected.Impact) < 0 {
			reason = ReasonLowerImpact
		} else if selected.ID() == history.CurrentID && now.Before(history.CurrentSince.Add(selected.Hold())) {
			reason = ReasonHeldByCurrent
		}
		evaluations[value.index].Reason = reason
	}
	return Decision{Candidate: selected, Evaluations: evaluations}
}

func ineligibleReason(rule Rule, disposition protocol.Disposition) Reason {
	switch rule.Policy {
	case presentation.PolicyAttention:
		if disposition != protocol.DispositionActionable {
			return ReasonNotActionableForAttention
		}
	case presentation.PolicyInteractive:
		if !rule.Foreground {
			return ReasonForegroundInactive
		}
	case presentation.PolicyWhenRelevant:
		if disposition != protocol.DispositionNotable && disposition != protocol.DispositionActionable {
			return ReasonNotRelevant
		}
	case presentation.PolicyRotation:
		if disposition != protocol.DispositionNotable && disposition != protocol.DispositionActionable {
			return ReasonNotRelevant
		}
	default:
		return ReasonDisabled
	}
	return ""
}

func rotationNextDue(id string, lastShown time.Time, intervalMS, jitterPercent int) time.Time {
	if lastShown.IsZero() {
		return time.Time{}
	}
	interval := time.Duration(intervalMS) * time.Millisecond
	if interval <= 0 {
		return lastShown
	}
	if jitterPercent < 0 {
		jitterPercent = 0
	}
	if jitterPercent > 50 {
		jitterPercent = 50
	}
	if jitterPercent == 0 {
		return lastShown.Add(interval)
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(id))
	maximumBasisPoints := int64(jitterPercent * 100)
	span := uint64(2*maximumBasisPoints + 1)
	offsetBasisPoints := int64(hash.Sum64()%span) - maximumBasisPoints
	jitter := (interval / 10_000) * time.Duration(offsetBasisPoints)
	return lastShown.Add(interval + jitter)
}

func derive(record observation.Record, rule Rule) presentation.Candidate {
	value := record.Observation
	var scene presentation.Scene
	if value.Scene != nil {
		scene = *value.Scene
	}
	return presentation.Candidate{
		PluginID: record.PluginID, InstanceID: value.Instance.ID, Channel: value.Channel, Key: value.Key,
		Revision: value.Revision, Generation: record.Generation, Policy: rule.Policy,
		Band: presentation.BandFor(rule.Policy, value.Impact), Impact: value.Impact, AdmissionSequence: record.AdmissionSequence,
		Reason:    value.ReasonCode,
		CreatedAt: value.ObservedAt, UpdatedAt: value.UpdatedAt, ExpiresAt: value.ValidUntil,
		HoldMS: rule.HoldMS, CooldownMS: rule.CooldownMS, RequiresAck: rule.RequiresAck,
		Acknowledged: rule.Acknowledged, DevicePriority: rule.DevicePriority, Scene: scene,
		BusyTimer: value.BusyTimer,
		AudioCue:  value.Audio,
	}
}
