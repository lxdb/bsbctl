package slack

import (
	"cmp"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"time"

	protocoljson "github.com/lxdb/bsbctl/sdk/protocol"
)

var (
	errCheckpoint    = errors.New("incompatible Slack checkpoint")
	errStaleActivity = errors.New("Slack activity selection is stale")
	hashPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const checkpointLimit = 64 * 1024

type checkpointJSON struct {
	SchemaVersion int                  `json:"schema_version"`
	Scope         string               `json:"scope"`
	Fingerprints  []messageFingerprint `json:"fingerprints"`
	Watches       []watch              `json:"watches"`
	Handled       []handledEpisode     `json:"handled"`
	Truncated     bool                 `json:"truncated,omitzero"`
	Revision      uint64               `json:"revision"`
}

// proposeHandle does not mutate the receiver. Save raw successfully before
// replacing the worker's state with proposed; discard it on failure. The worker
// must serialize reduction throughout save-and-replace to avoid losing arrivals.
func (s *state) proposeHandle(id string, revision uint64, fingerprint string, now time.Time) (*state, json.RawMessage, error) {
	item, ok := s.aggregates[id]
	if !ok || item.Revision != revision || item.Fingerprint != fingerprint {
		return nil, nil, errStaleActivity
	}
	proposed := *s
	proposed.messages = maps.Clone(s.messages)
	proposed.aggregates = maps.Clone(s.aggregates)
	proposed.fingerprints = maps.Clone(s.fingerprints)
	proposed.watches = maps.Clone(s.watches)
	proposed.handled = maps.Clone(s.handled)
	proposed.callbacks = maps.Clone(s.callbacks)
	proposed.callbackFIFO = slices.Clone(s.callbackFIFO)
	proposed.prune(now)
	proposed.revision++
	item.Revision = proposed.revision
	proposed.aggregates[id] = item
	proposed.handled[fingerprint] = handledEpisode{Fingerprint: fingerprint, ObservedAt: item.ObservedAt, HandledAt: now.UTC()}
	for len(proposed.handled) > maxRetained {
		oldest := ""
		for key, value := range proposed.handled {
			other := proposed.handled[oldest]
			if oldest == "" || value.HandledAt.Before(other.HandledAt) || (value.HandledAt.Equal(other.HandledAt) && key < oldest) {
				oldest = key
			}
		}
		delete(proposed.handled, oldest)
		proposed.truncated = true
	}
	raw, err := proposed.checkpoint(now)
	if err != nil {
		return nil, nil, err
	}
	return &proposed, raw, nil
}

func (s *state) checkpoint(now time.Time) (json.RawMessage, error) {
	wire := checkpointJSON{SchemaVersion: 1, Scope: s.scope, Truncated: s.truncated, Revision: s.revision}
	for _, value := range s.fingerprints {
		wire.Fingerprints = append(wire.Fingerprints, value)
	}
	for _, value := range s.watches {
		if now.Before(value.LastActivity.Add(watchLifetime)) {
			wire.Watches = append(wire.Watches, value)
		}
	}
	for _, value := range s.handled {
		if now.Before(value.HandledAt.Add(handledLifetime)) {
			wire.Handled = append(wire.Handled, value)
		}
	}
	slices.SortFunc(wire.Fingerprints, func(a, b messageFingerprint) int {
		return cmp.Or(a.SeenAt.Compare(b.SeenAt), cmp.Compare(a.Key, b.Key))
	})
	slices.SortFunc(wire.Watches, func(a, b watch) int { return cmp.Or(a.LastActivity.Compare(b.LastActivity), cmp.Compare(a.Key, b.Key)) })
	slices.SortFunc(wire.Handled, func(a, b handledEpisode) int {
		return cmp.Or(a.HandledAt.Compare(b.HandledAt), cmp.Compare(a.Fingerprint, b.Fingerprint))
	})
	for {
		raw, err := json.Marshal(wire)
		if err != nil {
			return nil, errCheckpoint
		}
		if len(raw) < checkpointLimit {
			return raw, nil
		}
		// Prefer preserving acknowledged intent. Replay/watch suppression is bounded
		// recovery, so discard its oldest records before durable handling entries.
		wire.Truncated = true
		s.truncated = true
		switch {
		case len(wire.Fingerprints) > 0:
			wire.Fingerprints = wire.Fingerprints[1:]
		case len(wire.Watches) > 0:
			wire.Watches = wire.Watches[1:]
		case len(wire.Handled) > 0:
			wire.Handled = wire.Handled[1:]
		default:
			return nil, errCheckpoint
		}
	}
}

func (s *state) restoreCheckpoint(raw json.RawMessage, now time.Time) error {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) >= checkpointLimit {
		return errCheckpoint
	}
	var wire checkpointJSON
	if err := protocoljson.DecodeStrict(raw, &wire); err != nil || wire.SchemaVersion != 1 || wire.Scope != s.scope || len(wire.Fingerprints) > maxRetained || len(wire.Watches)+len(s.explicitWatches) > maxRetained || len(wire.Handled) > maxRetained {
		return errCheckpoint
	}
	fingerprints := make(map[string]messageFingerprint, len(wire.Fingerprints))
	watches := make(map[string]watch, len(wire.Watches))
	handled := make(map[string]handledEpisode, len(wire.Handled))
	validTime := func(value time.Time) bool { return !value.IsZero() && !value.After(now.Add(5*time.Minute)) }
	for _, value := range wire.Fingerprints {
		if !hashPattern.MatchString(value.Key) || !hashPattern.MatchString(value.Digest) || !timestampPattern.MatchString(value.Version) || !validTime(value.SeenAt) {
			return errCheckpoint
		}
		if _, exists := fingerprints[value.Key]; exists {
			return errCheckpoint
		}
		fingerprints[value.Key] = value
	}
	seenWatches := make(map[string]bool)
	for _, value := range wire.Watches {
		if !hashPattern.MatchString(value.Key) || !validTime(value.LastActivity) || seenWatches[value.Key] {
			return errCheckpoint
		}
		seenWatches[value.Key] = true
		if now.Before(value.LastActivity.Add(watchLifetime)) && !s.explicitWatches[value.Key] {
			watches[value.Key] = value
		}
	}
	seenHandled := make(map[string]bool)
	for _, value := range wire.Handled {
		if !hashPattern.MatchString(value.Fingerprint) || !validTime(value.ObservedAt) || !validTime(value.HandledAt) || value.HandledAt.Before(value.ObservedAt) || seenHandled[value.Fingerprint] {
			return errCheckpoint
		}
		seenHandled[value.Fingerprint] = true
		if now.Before(value.HandledAt.Add(handledLifetime)) {
			handled[value.Fingerprint] = value
		}
	}
	s.fingerprints = fingerprints
	s.watches = watches
	s.handled = handled
	s.truncated = wire.Truncated
	s.revision = wire.Revision
	return nil
}
