package presentation

import (
	"slices"
	"time"
)

// History is the minimum state required to enforce holds, cooldowns, and fairness.
type History struct {
	CurrentID    string
	CurrentSince time.Time
	LastShown    map[string]time.Time
}

// Decision describes the current winner. A nil candidate means bsbctl should clear.
type Decision struct {
	Candidate *Candidate
}

// Select deterministically chooses one eligible candidate.
func Select(candidates []Candidate, history History, now time.Time) Decision {
	current, currentOK := findEligible(candidates, history.CurrentID, now)
	eligible := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !candidate.Eligible(now) {
			continue
		}
		lastShown := history.LastShown[candidate.ID()]
		if candidate.ID() != history.CurrentID && !lastShown.IsZero() && now.Before(lastShown.Add(candidate.Cooldown())) {
			continue
		}
		eligible = append(eligible, candidate)
	}
	if len(eligible) == 0 {
		return Decision{}
	}

	slices.SortStableFunc(eligible, func(left, right Candidate) int {
		if better(left, right) {
			return -1
		}
		if better(right, left) {
			return 1
		}
		return 0
	})
	winner := eligible[0]
	if currentOK && now.Before(history.CurrentSince.Add(current.Hold())) && !mayInterrupt(winner, current) {
		winner = current
	}
	return Decision{Candidate: &winner}
}

func findEligible(candidates []Candidate, id string, now time.Time) (Candidate, bool) {
	for _, candidate := range candidates {
		if candidate.ID() == id && candidate.Eligible(now) {
			return candidate, true
		}
	}
	return Candidate{}, false
}

func better(a, b Candidate) bool {
	if order := CompareBand(a.Band, b.Band); order != 0 {
		return order > 0
	}
	if order := CompareImpact(a.Impact, b.Impact); order != 0 {
		return order > 0
	}
	if a.AdmissionSequence != b.AdmissionSequence {
		return a.AdmissionSequence < b.AdmissionSequence
	}
	return a.ID() < b.ID()
}

func mayInterrupt(next, current Candidate) bool {
	if order := CompareBand(next.Band, current.Band); order != 0 {
		return order > 0
	}
	return next.Band == BandActionable && CompareImpact(next.Impact, current.Impact) > 0
}
