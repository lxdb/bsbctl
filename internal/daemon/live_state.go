package daemon

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/pluginhost"
)

// LiveState owns the in-memory snapshot accepted by reconciliation and the
// revision metadata that makes stale asynchronous work unable to win. It does
// not perform persistence, plugin, asset, or device I/O.
type LiveState struct {
	mu                    sync.RWMutex
	document              config.Document
	generations           Generations
	specs                 []pluginhost.Spec
	appReadiness          map[string]AppReadiness
	epoch                 uint64
	activeAttempt         uint64
	nextAttempt           uint64
	nextCandidateRevision uint64
	acceptedRevision      uint64
	finalizedRevision     uint64
	activeCancel          context.CancelFunc
	attempts              map[uint64]chan struct{}
	candidate             *reconcileCandidate
	correctionRevision    uint64
	correctionAttempt     int
	correctionRetryAt     time.Time
	closing               bool
	loaded                bool
}

func NewLiveState() *LiveState {
	return &LiveState{
		appReadiness: make(map[string]AppReadiness),
		attempts:     make(map[uint64]chan struct{}),
	}
}

func (s *LiveState) Generation(pluginID, instanceID string) (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generations.Lookup(pluginID, instanceID)
}

func (s *LiveState) Document() (config.Document, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneDocument(s.document), s.loaded
}

func (s *LiveState) AppReadiness() []AppReadiness {
	s.mu.RLock()
	result := make([]AppReadiness, 0, len(s.appReadiness))
	for _, status := range s.appReadiness {
		result = append(result, status)
	}
	s.mu.RUnlock()
	slices.SortFunc(result, func(left, right AppReadiness) int { return cmp.Compare(left.AppID, right.AppID) })
	return result
}
