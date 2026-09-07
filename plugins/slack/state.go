package slack

import (
	"cmp"
	"maps"
	"slices"
	"time"
)

const allChannelsAlias = "CHANNEL"

const (
	maxRetained     = 128
	maxCallbacks    = 1024
	watchLifetime   = 72 * time.Hour
	handledLifetime = 7 * 24 * time.Hour
)

type activity struct {
	ActivityTS  string
	ID          string
	ChannelID   string
	RootTS      string
	MessageTS   string
	Alias       string
	Kind        string
	Mention     bool
	Count       int
	Preview     string
	Fingerprint string
	Revision    uint64
	ObservedAt  time.Time
	UpdatedAt   time.Time
	Handled     bool
}

type retainedMessage struct {
	key         string
	aggregateID string
	event       normalizedEvent
	kind        string
	updatedAt   time.Time
}

type messageFingerprint struct {
	Key     string    `json:"key"`
	Version string    `json:"version"`
	Digest  string    `json:"digest"`
	SeenAt  time.Time `json:"seen_at"`
}

type watch struct {
	Key          string    `json:"key"`
	LastActivity time.Time `json:"last_activity"`
}

type handledEpisode struct {
	Fingerprint string    `json:"fingerprint"`
	ObservedAt  time.Time `json:"observed_at"`
	HandledAt   time.Time `json:"handled_at"`
}

// state owns the entire pure reducer. The worker must serialize all calls,
// including a handling proposal's save and replacement of the committed state.
// Snapshots returned by items contain no references to reducer maps.
type state struct {
	revision             uint64
	config               config
	userID               string
	scope                string
	messages             map[string]retainedMessage
	aggregates           map[string]activity
	fingerprints         map[string]messageFingerprint
	watches              map[string]watch
	explicitWatches      map[string]bool
	handled              map[string]handledEpisode
	callbacks            map[string]bool
	callbackFIFO         []string
	channelNames         map[string]string
	truncated            bool
	unresolvedMembership bool
	coverageIncomplete   bool
}

func newState(cfg config, userID string) *state {
	s := &state{
		config: cfg, userID: userID, scope: hashParts(cfg.workspaceID, userID),
		messages: make(map[string]retainedMessage), aggregates: make(map[string]activity),
		fingerprints: make(map[string]messageFingerprint), watches: make(map[string]watch),
		explicitWatches: make(map[string]bool), handled: make(map[string]handledEpisode),
		callbacks: make(map[string]bool), channelNames: make(map[string]string),
	}
	s.config.channels = maps.Clone(cfg.channels)
	s.config.watchedThreads = slices.Clone(cfg.watchedThreads)
	for _, root := range cfg.watchedThreads {
		s.explicitWatches[s.rootKey(root.ChannelID, root.ThreadTS)] = true
	}
	return s
}

func (s *state) rootKey(channel, ts string) string {
	return hashParts(s.config.workspaceID, s.userID, channel, ts)
}

func (s *state) applyNormalized(event normalizedEvent, now time.Time) bool {
	if !s.config.configured {
		return false
	}
	if !s.acceptCallback(event.callbackID) {
		return false
	}
	pruned := s.prune(now)
	if !s.admits(event) {
		return pruned
	}
	key := hashParts(s.config.workspaceID, event.channelID, event.ts)
	old, existed := s.messages[key]
	previous, seen := s.fingerprints[key]
	if !seen && existed {
		previous = messageFingerprint{Version: old.event.version, Digest: old.event.digest}
		seen = true
	}
	if seen {
		order := compareTS(event.version, previous.Version)
		if order < 0 || (order == 0 && event.digest == previous.Digest) {
			return pruned
		}
		// Conflicting payloads at one provider version have no authoritative order.
		// Preserve the accepted version, avoiding retry-induced oscillation.
		if order == 0 {
			return pruned
		}
	}
	if event.deleted {
		s.remember(key, event, now)
		if !existed {
			return true
		}
		delete(s.messages, key)
		s.rebuild(old.aggregateID, now, false, "")
		return true
	}
	root := s.rootKey(event.channelID, event.rootTS)
	_, watched := s.watches[root]
	watched = watched || s.explicitWatches[root]
	// Watch expiry limits admission of new replies, not updates to retained ones.
	if existed && old.kind == "thread" && old.event.rootTS == event.rootTS {
		watched = true
	}
	if s.config.watchParticipatedThreads && (event.own || event.mention) {
		s.touchWatch(root, now)
		watched = true
	}
	if event.own {
		return pruned || (s.config.watchParticipatedThreads && !s.explicitWatches[root])
	}
	kind := ""
	dm := event.channelType == "im" || event.channelType == "mpim"
	switch {
	case event.ts != event.rootTS && (watched || event.mention || dm):
		kind = "thread"
	case dm:
		kind = "dm"
	case event.ts == event.rootTS:
		kind = "channel"
	}

	if kind == "" && !existed {
		return pruned
	}
	if watched {
		s.touchWatch(root, now)
	}
	s.remember(key, event, now)
	if kind == "" {
		delete(s.messages, key)
		s.rebuild(old.aggregateID, now, false, "")
		return true
	}
	aggregateID := "item-" + root
	newEpisode := !existed || (!old.event.mention && event.mention)
	s.messages[key] = retainedMessage{key: key, aggregateID: aggregateID, event: event, kind: kind, updatedAt: now.UTC()}
	if existed && old.aggregateID != aggregateID {
		s.rebuild(old.aggregateID, now, false, "")
	}
	s.rebuild(aggregateID, now, newEpisode, hashParts(key, event.version, event.digest))
	s.boundMessages(now)
	return true
}

func (s *state) acceptCallback(callbackID string) bool {
	if s.callbacks[callbackID] {
		return false
	}
	s.callbacks[callbackID] = true
	s.callbackFIFO = append(s.callbackFIFO, callbackID)
	if len(s.callbackFIFO) > maxCallbacks {
		delete(s.callbacks, s.callbackFIFO[0])
		s.callbackFIFO = s.callbackFIFO[1:]
	}
	return true
}

func (s *state) hasAcceptedCallback(callbackID string) bool {
	return s.callbacks[callbackID]
}

func (s *state) removeChannel(channelID string, now time.Time) bool {
	aggregates := make(map[string]bool)
	for key, message := range s.messages {
		if message.event.channelID != channelID {
			continue
		}
		aggregates[message.aggregateID] = true
		delete(s.messages, key)
	}
	delete(s.channelNames, channelID)
	for id := range aggregates {
		s.rebuild(id, now, false, "")
	}
	return len(aggregates) != 0
}

func (s *state) admits(event normalizedEvent) bool {
	switch event.channelType {
	case "im":
		return s.config.directMessages && validID(event.channelID, "D")
	case "mpim":
		return s.config.groupDirectMessages && validID(event.channelID, "G")
	case "channel", "group":
		_, ok := s.config.channels[event.channelID]
		return s.config.allChannels || ok
	default:
		return false
	}
}

func (s *state) remember(key string, event normalizedEvent, now time.Time) {
	s.fingerprints[key] = messageFingerprint{Key: key, Version: event.version, Digest: event.digest, SeenAt: now.UTC()}
	for len(s.fingerprints) > maxRetained {
		oldest := ""
		for key, value := range s.fingerprints {
			other := s.fingerprints[oldest]
			if oldest == "" || value.SeenAt.Before(other.SeenAt) || (value.SeenAt.Equal(other.SeenAt) && key < oldest) {
				oldest = key
			}
		}
		delete(s.fingerprints, oldest)
		s.truncated = true
	}
}

func (s *state) touchWatch(key string, now time.Time) {
	if s.explicitWatches[key] {
		return
	}
	s.watches[key] = watch{Key: key, LastActivity: now.UTC()}
	for len(s.watches)+len(s.explicitWatches) > maxRetained {
		oldest := ""
		for key, value := range s.watches {
			other := s.watches[oldest]
			if oldest == "" || value.LastActivity.Before(other.LastActivity) || (value.LastActivity.Equal(other.LastActivity) && key < oldest) {
				oldest = key
			}
		}
		delete(s.watches, oldest)
		s.truncated = true
	}
}

// prune expires bounded metadata without provider I/O. A true result requests
// checkpoint persistence and fresh snapshots (handling expiry changes a card).
func (s *state) prune(now time.Time) bool {
	beforeWatches, beforeHandled := len(s.watches), len(s.handled)
	maps.DeleteFunc(s.watches, func(_ string, w watch) bool { return !now.Before(w.LastActivity.Add(watchLifetime)) })
	for fingerprint, h := range s.handled {
		if now.Before(h.HandledAt.Add(handledLifetime)) {
			continue
		}
		delete(s.handled, fingerprint)
		for id, item := range s.aggregates {
			if item.Fingerprint == fingerprint {
				s.revision++
				item.Revision = s.revision
				s.aggregates[id] = item
			}
		}
	}
	return beforeWatches != len(s.watches) || beforeHandled != len(s.handled)
}

func (s *state) rebuild(id string, now time.Time, newEpisode bool, trigger string) {
	old := s.aggregates[id]
	s.revision++
	item := activity{ID: id, Kind: "thread", Fingerprint: old.Fingerprint, Revision: s.revision, ObservedAt: old.ObservedAt, UpdatedAt: now.UTC()}
	var newest retainedMessage
	for _, msg := range s.messages {
		if msg.aggregateID != id {
			continue
		}
		item.Count++
		if compareTS(msg.event.version, item.ActivityTS) > 0 {
			item.ActivityTS = msg.event.version
		}
		item.Mention = item.Mention || msg.event.mention
		if newest.key == "" || compareTS(msg.event.ts, newest.event.ts) > 0 {
			newest = msg
		}
	}
	if item.Count == 0 {
		delete(s.aggregates, id)
		return
	}
	item.Kind = newest.kind
	item.ChannelID = newest.event.channelID
	item.RootTS = newest.event.rootTS
	item.MessageTS = newest.event.ts
	item.Preview = newest.event.preview
	item.Alias = s.config.channels[item.ChannelID]
	if item.Alias == "" {
		item.Alias = s.channelNames[item.ChannelID]
	}
	if item.Alias == "" && s.config.allChannels && (newest.event.channelType == "channel" || newest.event.channelType == "group") {
		item.Alias = allChannelsAlias
	}
	if newEpisode {
		item.Fingerprint = hashParts(old.Fingerprint, trigger)
		item.ObservedAt = now.UTC()
		if !item.ObservedAt.After(old.ObservedAt) {
			item.ObservedAt = old.ObservedAt.Add(time.Nanosecond)
		}
	}
	s.aggregates[id] = item
}

func (s *state) setChannelName(channelID, name string) bool {
	maps.DeleteFunc(s.channelNames, func(id, _ string) bool { return !s.hasChannel(id) })
	if !s.hasChannel(channelID) || s.config.channels[channelID] != "" || !validID(channelID, "CG") || !validLabel(name) || s.channelNames[channelID] == name {
		return false
	}
	s.channelNames[channelID] = name
	changed := false
	for id, item := range s.aggregates {
		if item.ChannelID != channelID || item.Alias == name {
			continue
		}
		s.revision++
		item.Alias = name
		item.Revision = s.revision
		s.aggregates[id] = item
		changed = true
	}
	return changed
}

func (s *state) hasChannel(channelID string) bool {
	for _, item := range s.aggregates {
		if item.ChannelID == channelID {
			return true
		}
	}
	return false
}

func (s *state) boundMessages(now time.Time) {
	for len(s.messages) > maxRetained {
		oldest := ""
		for key, msg := range s.messages {
			other := s.messages[oldest]
			if oldest == "" || msg.updatedAt.Before(other.updatedAt) || (msg.updatedAt.Equal(other.updatedAt) && key < oldest) {
				oldest = key
			}
		}
		id := s.messages[oldest].aggregateID
		delete(s.messages, oldest)
		s.rebuild(id, now, false, "")
		s.truncated = true
	}
}

func (s *state) items() []activity {
	result := make([]activity, 0, len(s.aggregates))
	for _, item := range s.aggregates {
		_, item.Handled = s.handled[item.Fingerprint]
		result = append(result, item)
	}
	slices.SortFunc(result, func(a, b activity) int {
		if a.Mention != b.Mention {
			if a.Mention {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(kindRank(a.Kind), kindRank(b.Kind)), compareTS(b.ActivityTS, a.ActivityTS), cmp.Compare(a.ID, b.ID))
	})
	return result
}

func kindRank(kind string) int {
	switch kind {
	case "dm":
		return 0
	case "channel":
		return 1
	default:
		return 2
	}
}

// pendingItems projects the single retained index onto actionable local episodes.
func pendingItems(items []activity) []activity {
	pending := make([]activity, 0, len(items))
	for _, item := range items {
		if !item.Handled {
			pending = append(pending, item)
		}
	}
	return pending
}

// attentionItems keeps ambient channel and watched-thread activity in
// the manual pending list. Mentions and direct conversations may interrupt.
func attentionItems(items []activity) []activity {
	result := make([]activity, 0, len(items))
	for _, item := range items {
		if item.Handled {
			continue
		}
		directConversation := item.Kind == "dm" || (item.Kind == "thread" && item.Alias == "")
		if item.Mention || directConversation {
			result = append(result, item)
		}
	}
	return result
}
