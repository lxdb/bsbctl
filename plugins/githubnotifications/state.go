package githubnotifications

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

type item struct {
	notification
	ID         string
	EpisodeID  string
	ObservedAt time.Time
	Handled    bool
	Revision   uint64
}
type handledEpisode struct {
	ID         string    `json:"id"`
	Reason     string    `json:"reason"`
	EpisodeID  string    `json:"episode_id"`
	ObservedAt time.Time `json:"observed_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	HandledAt  time.Time `json:"handled_at"`
	Uncertain  bool      `json:"uncertain,omitempty"`
}
type checkpoint struct {
	SchemaVersion int              `json:"schema_version"`
	Scope         string           `json:"scope"`
	Handled       []handledEpisode `json:"handled"`
	Truncated     bool             `json:"truncated"`
}
type state struct {
	config                                          Config
	identity                                        Identity
	scope                                           string
	items                                           map[string]item
	handled                                         map[string]handledEpisode
	baseline                                        bool
	lastModified                                    string
	lastSuccess, freshUntil                         time.Time
	effectiveInterval, serverInterval               time.Duration
	phase, lastError                                string
	truncated, checkpointTruncated, checkpointDirty bool
	revision                                        uint64
}

func hashKey(parts ...string) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return "item-" + hex.EncodeToString(sum[:])
}
func newState(c Config, identity Identity) *state {
	names := make([]string, 0, len(c.Repositories))
	for _, r := range c.Repositories {
		names = append(names, strings.ToLower(r.Name))
	}
	slices.Sort(names)
	scope := hashKey(append([]string{strconv.FormatInt(identity.ID, 10)}, names...)...)
	phase := "unconfigured"
	if c.Configured {
		phase = "syncing"
	}
	return &state{config: c, identity: identity, scope: scope, items: map[string]item{}, handled: map[string]handledEpisode{}, effectiveInterval: c.PollInterval, phase: phase, checkpointDirty: c.Configured}
}
func actionable(reason string) bool {
	switch reason {
	case "review_requested", "approval_requested", "assign", "mention", "team_mention", "security_alert", "invitation":
		return true
	}
	return false
}
func (s *state) fresh(now time.Time) bool { return !s.lastSuccess.IsZero() && now.Before(s.freshUntil) }
func (s *state) apply(result fetchResult, err error, now time.Time) (conflict bool) {
	now = now.UTC()
	if result.PollInterval > 0 {
		s.serverInterval = result.PollInterval
	}
	s.effectiveInterval = max(s.serverInterval, s.config.PollInterval)
	if result.NotModified && !s.baseline {
		err = &sourceError{Code: "baseline_missing"}
		result.Complete = false
	}
	complete := result.Complete && err == nil

	if s.pruneHandled(now) {
		s.checkpointDirty = true
	}
	// Resolve duplicates and conflicts before applying any authoritative absence.
	seen := map[string]bool{}
	incoming := map[string]notification{}
	conflicted := map[string]bool{}
	for _, n := range result.Items {
		id := hashKey(strconv.FormatInt(s.identity.ID, 10), n.ThreadID)
		seen[id] = true
		if previous, exists := incoming[id]; exists {
			if n.UpdatedAt.Before(previous.UpdatedAt) {
				continue
			}
			if n.UpdatedAt.Equal(previous.UpdatedAt) && n != previous {
				conflicted[id] = true
			}
		}
		incoming[id] = n
	}
	for id, n := range incoming {
		if old, exists := s.items[id]; exists && n.UpdatedAt.Equal(old.UpdatedAt) && n != old.notification {
			conflicted[id] = true
		}
	}
	conflict = len(conflicted) > 0
	if conflict {
		complete = false
	} // Only a complete unconditional read can resolve an ambiguous write.
	if complete && !result.NotModified {
		for id, marker := range s.handled {
			if marker.Uncertain {
				delete(s.handled, id)
				s.checkpointDirty = true
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(incoming)) {
		n := incoming[id]
		old, exists := s.items[id]
		if conflicted[id] || (exists && n.UpdatedAt.Before(old.UpdatedAt)) {
			continue
		}
		if !n.Unread {
			if complete {
				delete(s.items, id)
			}
			continue
		}
		next := item{notification: n, ID: id}
		if exists {
			next = old
			next.notification = n
		}
		next.Handled = false
		if exists && (old.Reason != n.Reason || n.UpdatedAt.After(old.UpdatedAt)) {
			if _, ok := s.handled[id]; ok {
				delete(s.handled, id)
				s.checkpointDirty = true
			}
		}
		if !exists || (old.Reason != n.Reason || n.UpdatedAt.After(old.UpdatedAt)) {
			next.EpisodeID = ""
			next.ObservedAt = time.Time{}
			next.Handled = false
			if s.config.matchesReason(n.Reason) {
				next.ObservedAt = now
				next.EpisodeID = hashKey(id, n.Reason, n.UpdatedAt.UTC().Format(time.RFC3339Nano))
			}
		}
		if marker, ok := s.handled[id]; ok && marker.Reason == n.Reason && n.UpdatedAt.Equal(marker.UpdatedAt) {
			next.Handled = true
			next.EpisodeID = marker.EpisodeID
			next.ObservedAt = marker.ObservedAt
		}
		if !exists || next.notification != old.notification || next.EpisodeID != old.EpisodeID || next.Handled != old.Handled {
			s.revision++
			next.Revision = s.revision
		}
		s.items[id] = next
	}
	if conflict {
		complete = false
		s.lastModified = ""
		err = &sourceError{Code: "version_conflict"}
	}
	if complete {
		if !result.NotModified {
			for id := range s.items {
				if !seen[id] {
					delete(s.items, id)
				}
			}
			for id := range s.handled {
				if !seen[id] {
					delete(s.handled, id)
					s.checkpointDirty = true
				}
			}
			s.lastModified = result.LastModified
			s.baseline = true
		}
		s.lastSuccess = now
		s.freshUntil = now.Add(2 * s.effectiveInterval)
		s.phase = "ready"
		s.lastError = ""
		if !result.NotModified {
			s.truncated = false
		}
	} else {
		s.phase = "degraded"
		s.lastError = ErrorCode(err)
		if s.lastError == "" {
			s.lastError = "coverage_incomplete"
		}
		if IsCredentialRejected(err) {
			s.phase = "auth_required"
		}
		s.truncated = true
	}
	// Retention is based on recency; presentation applies attention grouping afterward.
	retained := slices.Collect(maps.Values(s.items))
	slices.SortFunc(retained, func(a, b item) int { return cmp.Or(b.UpdatedAt.Compare(a.UpdatedAt), cmp.Compare(a.ID, b.ID)) })
	if len(retained) > 128 {
		s.truncated = true
		for _, v := range retained[128:] {
			delete(s.items, v.ID)
		}
	}
	return conflict
}
func (s *state) ordered() []item {
	values := slices.Collect(maps.Values(s.items))
	slices.SortFunc(values, func(a, b item) int {
		rank := func(i item) int {
			if s.config.matchesReason(i.Reason) {
				return 0
			}
			return 1
		}
		return cmp.Or(cmp.Compare(rank(a), rank(b)), b.UpdatedAt.Compare(a.UpdatedAt), cmp.Compare(a.ID, b.ID))
	})
	return values
}
func (s *state) attention(now time.Time) []item {
	out := []item{}
	if !s.fresh(now) {
		return out
	}
	for _, i := range s.ordered() {
		if i.EpisodeID != "" && !i.Handled && s.config.matchesReason(i.Reason) {
			out = append(out, i)
			if len(out) == 32 {
				break
			}
		}
	}
	return out
}
func (s *state) pruneHandled(now time.Time) bool {
	changed := false
	for id, v := range s.handled {
		if !v.HandledAt.After(now.Add(-7 * 24 * time.Hour)) {
			delete(s.handled, id)
			changed = true
		}
	}
	values := slices.Collect(maps.Values(s.handled))
	slices.SortFunc(values, func(a, b handledEpisode) int {
		return cmp.Or(b.HandledAt.Compare(a.HandledAt), cmp.Compare(a.ID, b.ID))
	})
	if len(values) > 128 {
		for _, v := range values[128:] {
			delete(s.handled, v.ID)
		}
		s.checkpointTruncated = true
		changed = true
	}
	return s.reconcileHandling() || changed
}

// Markers own local suppression; removal changes visibility, not the episode.
func (s *state) reconcileHandling() bool {
	changed := false
	for id, i := range s.items {
		if _, exists := s.handled[id]; !exists && i.Handled {
			i.Handled = false
			s.revision++
			i.Revision = s.revision
			s.items[id] = i
			changed = true
		}
	}
	return changed
}
func (s *state) checkpointData(now time.Time) json.RawMessage {
	s.pruneHandled(now)
	values := slices.Collect(maps.Values(s.handled))
	slices.SortFunc(values, func(a, b handledEpisode) int {
		return cmp.Or(b.HandledAt.Compare(a.HandledAt), cmp.Compare(a.ID, b.ID))
	})
	for {
		raw, _ := json.Marshal(checkpoint{SchemaVersion: 1, Scope: s.scope, Handled: values, Truncated: s.checkpointTruncated})
		if len(raw) <= 64<<10 {
			s.reconcileHandling()
			return raw
		}
		delete(s.handled, values[len(values)-1].ID)
		values = values[:len(values)-1]
		s.checkpointTruncated = true
	}
}
func (s *state) restore(raw json.RawMessage, now time.Time) string {
	if len(raw) == 0 {
		return ""
	}
	var saved checkpoint
	if len(raw) > 64<<10 || protocol.DecodeStrict(raw, &saved) != nil || saved.SchemaVersion != 1 || saved.Scope != s.scope {
		return "checkpoint_ignored"
	}
	s.checkpointTruncated = saved.Truncated
	for _, v := range saved.Handled {
		if len(v.ID) != 69 || len(v.EpisodeID) != 69 || !knownReason(v.Reason) || v.ObservedAt.IsZero() || v.UpdatedAt.IsZero() || v.HandledAt.After(now) || v.HandledAt.IsZero() {
			return "checkpoint_ignored"
		}
	}
	for _, v := range saved.Handled {
		s.handled[v.ID] = v
	}
	if s.pruneHandled(now) {
		s.checkpointDirty = true
	}
	return ""
}
