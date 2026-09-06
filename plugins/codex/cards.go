package codex

import (
	"fmt"
	"slices"
	"time"

	"github.com/lxdb/bsbctl/internal/codexusage"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

const quotaPressureVisibilityWindow = 30 * time.Second

func (r *Reducer) Cards() []Card {
	now := r.now().UTC()
	if !r.connected {
		deadline := r.disconnectedAt.Add(reconnectGrace)
		if !r.disconnectedAt.IsZero() && now.Before(deadline) {
			result := []Card{reconnectingCard(r.disconnectedAt, deadline)}
			return append(result, r.threadCards(now, true)...)
		}
		return []Card{connectionCard(false, now)}
	}
	result := []Card{connectionCard(true, now)}
	if r.awaitingReconcile {
		return result
	}
	result = append(result, r.threadCards(now, false)...)
	result = append(result, r.quotaCards(now)...)
	return result
}

func (r *Reducer) threadCards(now time.Time, reconnecting bool) []Card {
	result := make([]Card, 0, len(r.threads)*3+1)
	keys := make([]string, 0, len(r.pending))
	if !reconnecting {
		for key := range r.pending {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			request := r.pending[key]
			result = append(result, requestCard(request, r.threads[request.ThreadID], now))
		}
	}
	threadIDs := make([]string, 0, len(r.threads))
	for threadID := range r.threads {
		threadIDs = append(threadIDs, threadID)
	}
	slices.Sort(threadIDs)
	activeCount := 0
	for _, threadID := range threadIDs {
		thread := r.threads[threadID]
		if thread.Status.Type == "active" || (thread.LatestTurn != nil && thread.LatestTurn.Status == "inProgress") {
			activeCount++
		}
		candidates := r.threadCardCandidates(thread, now, backgroundRun)
		for _, candidate := range []cardCandidate{candidates.outcome, candidates.compaction, candidates.plan, candidates.activity} {
			if candidate.present && (!reconnecting || safeDuringReconnect(candidate.card)) {
				result = append(result, candidate.card)
			}
		}
	}
	if activeCount > 1 {
		result = append(result, Card{
			Channel: ChannelOverview, Key: "active", StateWord: fmt.Sprintf("%d ACT", activeCount),
			ContextLine: "Codex threads", DetailLine: fmt.Sprintf("%d active", activeCount),
			Disposition: protocol.DispositionNotable, Impact: protocol.ImpactLow,
			ReasonCode: "codex_active_count", ObservedAt: now, ValidUntil: now.Add(45 * time.Second),
		})
	}
	return result
}

func safeDuringReconnect(card Card) bool {
	switch card.Channel {
	case ChannelActivity, ChannelProgress, ChannelOverview, ChannelOutcome:
		return true
	default:
		return false
	}
}

func (r *Reducer) ReconnectDeadline() (time.Time, bool) {
	if r.connected || r.disconnectedAt.IsZero() {
		return time.Time{}, false
	}
	deadline := r.disconnectedAt.Add(reconnectGrace)
	if !deadline.After(r.now().UTC()) {
		return time.Time{}, false
	}
	return deadline, true
}

func (r *Reducer) quotaCards(now time.Time) []Card {
	if !r.quotaOptions.Enabled || r.quota == nil || len(r.quota.Windows) == 0 {
		return nil
	}
	validUntil := r.quota.UpdatedAt.Add(6 * time.Minute)
	if !validUntil.After(now) {
		return nil
	}
	result := make([]Card, 0, len(r.quota.Windows)+1)
	for _, window := range r.quota.Windows {
		result = append(result, Card{
			Channel: ChannelQuotaSummary, Key: codexusage.SummaryKey(window.Duration),
			Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNormal, ReasonCode: "codex_quota_summary",
			ObservedAt: r.quota.UpdatedAt, ValidUntil: validUntil,
			Scene: codexusage.Scene(*r.quota, window, r.quotaOptions.Presentation, codexusage.SignalNone, r.quotaOptions.AssetPath),
		})
	}
	signal := r.quotaSignal
	if signal == codexusage.SignalNone || !now.Before(r.quotaPressureUntil) {
		return result
	}
	pressureUntil := min(validUntil, r.quotaPressureUntil)
	pressure := Card{
		Channel: ChannelQuotaPressure, Key: "quota",
		Disposition: protocol.DispositionNotable, Impact: protocol.ImpactLow, ReasonCode: "codex_quota_low",
		ObservedAt: r.quota.UpdatedAt, ValidUntil: pressureUntil,
		Scene: codexusage.Scene(*r.quota, codexusage.MostConstrained(*r.quota), r.quotaOptions.Presentation, signal, r.quotaOptions.AssetPath),
	}
	if signal == codexusage.SignalCritical {
		pressure.Disposition = protocol.DispositionActionable
		pressure.Impact = protocol.ImpactCritical
		pressure.ReasonCode = "codex_quota_critical"
	}
	return append(result, pressure)
}

func min(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

func (r *Reducer) LauncherCard() Card {
	now := r.now().UTC()
	detail := "App server disconnected"
	if r.connected {
		detail = fmt.Sprintf("%d loaded sessions", len(r.threads))
	}
	return Card{
		Channel: ChannelDetail, Key: "launcher", StateWord: "CODEX",
		ContextLine: "Codex sessions", DetailLine: detail,
		Disposition: protocol.DispositionSnapshot, Impact: protocol.ImpactNormal,
		ReasonCode: "codex_launcher", ObservedAt: now, ValidUntil: now.Add(24 * time.Hour),
	}
}

// LiveCard returns the current foreground view without background visibility throttles.
func (r *Reducer) LiveCard() Card {
	now := r.now().UTC()
	if !r.connected {
		deadline := r.disconnectedAt.Add(reconnectGrace)
		if !r.disconnectedAt.IsZero() && now.Before(deadline) {
			return reconnectingCard(r.disconnectedAt, deadline)
		}
		return connectionCard(false, now)
	}
	latest := r.latestLiveThread()
	if latest == nil {
		return r.LauncherCard()
	}
	if pending, ok := r.latestPendingCard(latest, now); ok {
		return pending
	}
	candidates := r.threadCardCandidates(latest, now, foregroundRun)
	if latest.LatestTurn != nil && terminalTurnStatus(latest.LatestTurn.Status) {
		if candidates.outcome.present {
			return candidates.outcome.card
		}
		if candidates.plan.present {
			return candidates.plan.card
		}
		if candidates.compaction.present {
			return candidates.compaction.card
		}
	}
	if latest.CompactionItemID != "" && candidates.activity.present {
		return candidates.activity.card
	}
	if latest.PlanTotal != 0 && candidates.plan.present {
		return candidates.plan.card
	}
	if candidates.activity.present {
		return candidates.activity.card
	}
	if candidates.outcome.present {
		return candidates.outcome.card
	}
	return r.LauncherCard()
}

func (r *Reducer) latestLiveThread() *threadState {
	var latest *threadState
	for _, thread := range r.threads {
		if latest == nil || thread.liveSequence > latest.liveSequence {
			latest = thread
		}
	}
	return latest
}

func (r *Reducer) latestPendingCard(thread *threadState, now time.Time) (Card, bool) {
	var latest *pendingRequest
	for key, request := range r.pending {
		if request.ThreadID != thread.ID {
			continue
		}
		if latest == nil || request.ArrivalSequence > latest.ArrivalSequence || (request.ArrivalSequence == latest.ArrivalSequence && key > latest.ID.Key()) {
			latest = request
		}
	}
	if latest == nil {
		return Card{}, false
	}
	return requestCard(latest, thread, now), true
}

type cardCandidate struct {
	card    Card
	present bool
}

type threadCandidates struct {
	outcome    cardCandidate
	compaction cardCandidate
	plan       cardCandidate
	activity   cardCandidate
}

func (r *Reducer) threadCardCandidates(thread *threadState, now time.Time, runPolicy runVisibilityPolicy) threadCandidates {
	outcome, hasOutcome := outcomeCard(thread, now)
	compaction, hasCompaction := compactionOutcomeCard(thread, now)
	plan, hasPlan := planCard(thread, now)
	activity, hasActivity := r.threadCard(thread, now, runPolicy)
	return threadCandidates{
		outcome:    cardCandidate{card: outcome, present: hasOutcome},
		compaction: cardCandidate{card: compaction, present: hasCompaction},
		plan:       cardCandidate{card: plan, present: hasPlan},
		activity:   cardCandidate{card: activity, present: hasActivity},
	}
}

func outcomeCard(thread *threadState, now time.Time) (Card, bool) {
	if thread == nil || thread.LatestTurn == nil || !terminalTurnStatus(thread.LatestTurn.Status) || thread.OutcomeAt.IsZero() {
		return Card{}, false
	}
	if thread.LatestTurn.Status == "completed" && thread.CompletedPlanTurnID == thread.LatestTurn.ID {
		return Card{}, false
	}
	expires := thread.OutcomeAt.Add(outcomeVisibilityWindow)
	if !expires.After(now) {
		return Card{}, false
	}
	card := Card{
		Channel: ChannelOutcome, Key: observationKey("outcome", thread.ID), ContextLine: threadContext(thread),
		Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNormal,
		ObservedAt: thread.OutcomeAt, ValidUntil: expires,
	}
	applyThreadIdentity(&card, thread)
	switch thread.LatestTurn.Status {
	case "completed":
		card.StateWord, card.DetailLine, card.ReasonCode = "DONE", "Completed", "codex_turn_completed"
	case "interrupted":
		card.StateWord, card.DetailLine, card.ReasonCode = "STOP", "Interrupted", "codex_turn_interrupted"
		card.Impact = protocol.ImpactNotable
	case "failed":
		card.StateWord, card.DetailLine, card.ReasonCode = "FAIL", "Failed", "codex_turn_failed"
		card.Disposition, card.Impact = protocol.DispositionActionable, protocol.ImpactCritical
	}
	return card, true
}

func compactionOutcomeCard(thread *threadState, now time.Time) (Card, bool) {
	if thread == nil || thread.CompletedCompactionItemID == "" || thread.CompactionCompletedAt.IsZero() {
		return Card{}, false
	}
	expires := thread.CompactionCompletedAt.Add(30 * time.Second)
	if !expires.After(now) {
		return Card{}, false
	}
	card := Card{
		Channel: ChannelOutcome, Key: observationKey("compaction", thread.ID),
		StateWord: "COMPACTED", ContextLine: threadContext(thread), DetailLine: "Context compacted",
		Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNormal,
		ReasonCode: "codex_compacted", ObservedAt: thread.CompactionCompletedAt, ValidUntil: expires,
	}
	applyThreadIdentity(&card, thread)
	return card, true
}

func planCard(thread *threadState, now time.Time) (Card, bool) {
	if thread == nil || (thread.CompletedPlanTurnID == "" && thread.PlanTotal == 0) {
		return Card{}, false
	}
	card := Card{
		Channel: ChannelProgress, Key: observationKey("plan", thread.ID),
		StateWord:   fmt.Sprintf("PLAN %d/%d", thread.PlanDone, thread.PlanTotal),
		ContextLine: threadContext(thread), DetailLine: "Plan progress",
		Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNormal,
		ReasonCode: "codex_plan_progress", ObservedAt: now, ValidUntil: now.Add(45 * time.Second),
	}
	applyThreadIdentity(&card, thread)
	if thread.CompletedPlanTurnID != "" {
		if thread.LatestTurn != nil && thread.LatestTurn.Status == "completed" && !thread.OutcomeAt.IsZero() {
			card.ObservedAt = thread.OutcomeAt
			card.ValidUntil = thread.OutcomeAt.Add(outcomeVisibilityWindow)
			if !card.ValidUntil.After(now) {
				return Card{}, false
			}
		}
		card.Channel, card.StateWord, card.DetailLine = ChannelOutcome, "PLAN READY", "Ready in Codex"
		card.Disposition, card.Impact, card.ReasonCode = protocol.DispositionNotable, protocol.ImpactNotable, "codex_plan_ready"
	}
	return card, true
}

type runVisibilityPolicy uint8

const (
	backgroundRun runVisibilityPolicy = iota
	foregroundRun
)

func (r *Reducer) threadCard(thread *threadState, now time.Time, runPolicy runVisibilityPolicy) (Card, bool) {
	if thread == nil {
		return Card{}, false
	}
	card := Card{
		Channel: ChannelActivity, Key: observationKey("thread", thread.ID), StateWord: "RUN",
		ContextLine: threadContext(thread), DetailLine: "Active",
		Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNormal,
		ReasonCode: "codex_running", ObservedAt: now, ValidUntil: now.Add(45 * time.Second),
	}
	applyThreadIdentity(&card, thread)
	if thread.Status.Type == "systemError" {
		card.Channel, card.StateWord, card.DetailLine = ChannelOutcome, "FAIL", "System error"
		card.Disposition, card.Impact, card.ReasonCode = protocol.DispositionActionable, protocol.ImpactCritical, "codex_system_error"
		return card, true
	}
	if !r.hasPendingForThread(thread.ID) {
		if contains(thread.Status.ActiveFlags, "waitingOnApproval") {
			card.Channel, card.StateWord, card.DetailLine = ChannelAttention, "WAIT", "Approval required"
			card.Disposition, card.Impact, card.ReasonCode = protocol.DispositionActionable, protocol.ImpactNotable, "codex_waiting_approval"
			return card, true
		}
		if contains(thread.Status.ActiveFlags, "waitingOnUserInput") {
			card.Channel, card.StateWord, card.DetailLine = ChannelGuidance, "OPEN CODEX", "Use Codex"
			card.Disposition, card.Impact, card.ReasonCode = protocol.DispositionNotable, protocol.ImpactNotable, "codex_waiting_input"
			return card, true
		}
	}
	if thread.CompactionItemID != "" && !thread.CompactionStartedAt.IsZero() {
		card.Channel = ChannelProgress
		card.Key = observationKey("compaction-active", thread.ID+"\x00"+thread.CompactionItemID)
		card.StateWord, card.DetailLine, card.ReasonCode = "COMPACT", "Compacting context", "codex_compacting"
		card.Disposition = protocol.DispositionNotable
		card.ObservedAt = thread.CompactionStartedAt
		return card, true
	}
	active := thread.Status.Type == "active" || (thread.LatestTurn != nil && thread.LatestTurn.Status == "inProgress")
	if active && thread.ID == r.pinned {
		card.StateWord, card.DetailLine = "PIN", "Pinned focus"
		card.Disposition, card.Impact, card.ReasonCode = protocol.DispositionNotable, protocol.ImpactNotable, "codex_thread_pinned"
		return card, true
	}
	if !active || thread.LatestTurn == nil || thread.LatestTurn.ID == "" || thread.RunStartedAt.IsZero() {
		return Card{}, false
	}
	expires := thread.RunStartedAt.Add(runVisibilityWindow)
	if !expires.After(now) && runPolicy == backgroundRun {
		return Card{}, false
	}
	if runPolicy == foregroundRun && !expires.After(now) {
		expires = now.Add(45 * time.Second)
	}
	card.Key = runObservationKey(thread)
	card.ObservedAt = thread.RunStartedAt
	card.ValidUntil = expires
	return card, true
}
