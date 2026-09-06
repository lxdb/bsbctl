package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	ChannelSummary    = "summary"
	ChannelAttention  = "attention"
	ChannelConnection = "connection"
	ChannelLive       = "live"
	sceneRefresh      = 15 * time.Second
	sceneTTL          = 45 * time.Second
)

var errPublication = errors.New("Slack publication failed")

type publishedItem struct {
	observation       protocol.Observation
	target            activity
	confirmed         bool
	fresh             bool
	residentSignature string
	panelRevision     uint64
}

// A global counter avoids retaining revision tombstones for evicted items.
// current is bounded by the retained-message and attention limits.
type publisher struct {
	mu            sync.Mutex
	calls         chan struct{}
	panel         *panelSession
	panelRevision uint64
	revision      uint64
	current       map[string]publishedItem
	liveSignature string
	liveRefreshed time.Time
	signature     string
	refreshed     time.Time
}

func (w *worker) observationKey(kind string) string {
	return "item-" + hashParts(w.cfg.workspaceID, w.cfg.userID, kind)
}
func publicationSignature(s workerSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s/%t/%t/%t", s.Phase, s.ErrorCode, s.Fresh, s.Gap, s.Truncated)
	for _, a := range s.Items {
		fmt.Fprintf(&b, "/%s:%d:%t", a.ID, a.Revision, a.Handled)
	}
	return b.String()
}
func (w *worker) runPublisher() {
	for w.ctx.Err() == nil {
		_ = w.publishResident(w.ctx)
		_ = w.publishCurrentPanel(w.ctx)
		delay := sceneRefresh
		s := w.snapshot()
		if s.Fresh {
			delay = min(delay, s.FreshUntil.Sub(w.now()))
		}
		delay = max(delay, time.Millisecond)
		timer := time.NewTimer(delay)
		select {
		case <-w.ctx.Done():
			timer.Stop()
			return
		case <-w.changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
func (w *worker) publishResident(ctx context.Context) error {
	p := &w.publications
	if w.ctx.Err() != nil {
		return w.ctx.Err()
	}
	if w.host == nil {
		return nil
	}
	now := w.now().UTC()
	s := w.snapshot()
	signature := publicationSignature(s)
	p.mu.Lock()
	if signature == p.signature && now.Before(p.refreshed.Add(sceneRefresh)) {
		p.mu.Unlock()
		return nil
	}
	// Rate-limit failed attempts too; transport heartbeats are not domain changes.
	p.signature = signature
	p.refreshed = now
	p.mu.Unlock()
	desired := make(map[string]publishedItem)
	add := func(channel, key, reason string, impact protocol.Impact, scene protocol.Scene, target activity) {
		disposition := protocol.DispositionNotable
		observed := now
		expires := now.Add(sceneTTL)
		if channel == ChannelAttention {
			disposition = protocol.DispositionActionable
			observed = target.ObservedAt
		}
		if channel == ChannelAttention {
			expires = minTime(expires, s.FreshUntil)
		}
		desired[channel+"/"+key] = publishedItem{observation: protocol.Observation{Instance: w.instance.Ref(), Channel: channel, Key: key, Disposition: disposition, Impact: impact, ReasonCode: reason, ObservedAt: observed, UpdatedAt: now, ValidUntil: expires, Scene: new(scene)}, target: target, fresh: channel == ChannelAttention, residentSignature: signature}
	}
	if s.Fresh {
		n := 0
		for _, a := range attentionItems(s.Items) {
			if n == 32 {
				break
			}
			n++
			add(ChannelAttention, a.ID, a.Kind, protocol.ImpactNotable, detailScene(w.cfg, s, a, 0, now), a)
		}
	}
	if w.cfg.configured && (s.Phase != "ready" || s.Gap || s.Truncated || len(attentionItems(s.Items)) > 32) {
		add(ChannelConnection, w.observationKey("connection"), "coverage", protocol.ImpactNotable, connectionScene(s), activity{})
	}
	// Remove obsolete slots before admitting new cards; even failed withdrawals
	// cannot allow a stream of replacements to exceed the attention bound.
	var obsolete []string
	p.mu.Lock()
	for id, old := range p.current {
		if old.observation.Channel != ChannelLive {
			if _, ok := desired[id]; !ok {
				obsolete = append(obsolete, id)
			}
		}
	}
	p.mu.Unlock()
	for _, id := range obsolete {
		if err := w.withdraw(ctx, id); err != nil {
			return err
		}
	}
	for id, item := range desired {
		if err := w.publish(ctx, id, item); err != nil {
			return err
		}
	}

	return nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (w *worker) hostContext(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	child, cancel := context.WithTimeout(ctx, 2*time.Second)
	stop := context.AfterFunc(w.ctx, cancel)
	if !deadline.IsZero() {
		bounded, done := context.WithTimeout(child, deadline.Sub(w.now()))
		return bounded, func() { done(); stop(); cancel() }
	}
	return child, func() { stop(); cancel() }
}

// Host requests are serialized individually. The metadata mutex never spans
// I/O, and foreground callers can stop waiting at their own deadline.
func (w *worker) acquirePublication(ctx context.Context) error {
	p := &w.publications
	p.mu.Lock()
	if p.calls == nil {
		p.calls = make(chan struct{}, 1)
	}
	calls := p.calls
	p.mu.Unlock()
	select {
	case calls <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-calls
			return err
		}
		if err := w.ctx.Err(); err != nil {
			<-calls
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
}
func (w *worker) publish(ctx context.Context, id string, item publishedItem) error {
	if err := w.acquirePublication(ctx); err != nil {
		return err
	}
	defer func() { <-w.publications.calls }()
	return w.publishLocked(ctx, id, item)
}
func (w *worker) withdraw(ctx context.Context, id string) error {
	if err := w.acquirePublication(ctx); err != nil {
		return err
	}
	defer func() { <-w.publications.calls }()
	return w.withdrawLocked(ctx, id)
}

// Caller owns the publication request slot, never the reducer or metadata mutex.
func (w *worker) publishLocked(ctx context.Context, id string, item publishedItem) error {
	if w.ctx.Err() != nil {
		return w.ctx.Err()
	}
	if w.host == nil {
		return errPublication
	}
	if !w.publicationCurrent(item) {
		return errPublication
	}
	if item.fresh {
		current := w.snapshot()
		if !current.Fresh {
			return errPublication
		}
		item.observation.ValidUntil = minTime(item.observation.ValidUntil, current.FreshUntil)
	}
	if !item.observation.ValidUntil.After(w.now()) {
		return errPublication
	}
	p := &w.publications
	p.mu.Lock()
	p.revision++
	item.observation.Revision = p.revision
	// Record ambiguous host failures too, so later stale/handled states withdraw
	// anything the host may have accepted before the response was lost.
	p.current[id] = item
	p.mu.Unlock()
	deadline := time.Time{}
	if item.fresh {
		deadline = item.observation.ValidUntil
	}
	call, cancel := w.hostContext(ctx, deadline)
	defer cancel()
	if call.Err() != nil {
		return call.Err()
	}
	publishErr := w.host.PublishObservation(call, item.observation)
	if !w.publicationCurrent(item) {
		_ = w.withdrawLocked(ctx, id)
		return errPublication
	}
	if publishErr != nil {
		return errPublication
	}
	item.confirmed = true
	p.mu.Lock()
	p.current[id] = item
	p.mu.Unlock()
	return nil
}
func (w *worker) withdrawLocked(ctx context.Context, id string) error {
	if w.ctx.Err() != nil {
		return w.ctx.Err()
	}
	p := &w.publications
	p.mu.Lock()
	item, ok := p.current[id]
	p.mu.Unlock()
	if !ok {
		return nil
	}
	call, cancel := w.hostContext(ctx, time.Time{})
	defer cancel()
	if err := w.host.WithdrawObservation(call, protocol.WithdrawRequest{Instance: w.instance.Ref(), Channel: item.observation.Channel, Key: item.observation.Key}); err != nil {
		return errPublication
	}
	p.mu.Lock()
	delete(p.current, id)
	p.mu.Unlock()
	return nil
}

// Caller owns panelMu. Background refresh reads only this immutable copy,
// so host I/O cannot make an unrelated callback wait for the panel mutex.
func (w *worker) updatePanelSnapshot() {
	p := &w.publications
	p.mu.Lock()
	p.panelRevision++
	p.panel = nil
	if w.panel != nil {
		p.panel = new(*w.panel)
	}
	p.mu.Unlock()
	w.notify()
}
func (w *worker) publishPanel(ctx context.Context) error {
	w.updatePanelSnapshot()
	return w.publishCurrentPanel(ctx)
}

// A stale panel has a short diagnostic lease; retained data is never presented
// as fresh after the source deadline. Session/navigation revisions prevent an
// older background copy from overwriting a newer input or completed session.
func (w *worker) publishCurrentPanel(ctx context.Context) error {
	if err := w.acquirePublication(ctx); err != nil {
		return err
	}
	defer func() { <-w.publications.calls }()
	p := &w.publications
	p.mu.Lock()
	panel, revision := p.panel, p.panelRevision
	p.mu.Unlock()
	key := w.observationKey("live")
	if panel == nil {
		return w.withdrawLocked(ctx, ChannelLive+"/"+key)
	}
	now := w.now().UTC()
	snap := w.snapshot()
	signature := fmt.Sprintf("%s/%d", publicationSignature(snap), revision)
	p.mu.Lock()
	_, exists := p.current[ChannelLive+"/"+key]
	if exists && signature == p.liveSignature && now.Before(p.liveRefreshed.Add(sceneRefresh)) {
		p.mu.Unlock()
		return nil
	}
	p.liveSignature = signature
	p.liveRefreshed = now
	p.mu.Unlock()
	until := now.Add(sceneTTL)
	if snap.Fresh {
		until = minTime(until, snap.FreshUntil)
	}
	return w.publishLocked(ctx, ChannelLive+"/"+key, publishedItem{observation: protocol.Observation{Instance: w.instance.Ref(), Channel: ChannelLive, Key: key, Disposition: protocol.DispositionSnapshot, Impact: protocol.ImpactNormal, ReasonCode: "panel", ObservedAt: panel.started, UpdatedAt: now, ValidUntil: until, Scene: new(panelScene(w.cfg, snap, panel, now))}, fresh: snap.Fresh, panelRevision: revision})
}

// A reducer commit invalidates the remainder of a resident batch. In-flight
// acceptance is retracted before another item can be sent, including after an
// ambiguous host error. Cached snapshots keep this independent of reducer I/O.
func (w *worker) publicationCurrent(item publishedItem) bool {
	if item.panelRevision != 0 {
		p := &w.publications
		p.mu.Lock()
		current := item.panelRevision == p.panelRevision
		p.mu.Unlock()
		if !current {
			return false
		}
	}
	snap := w.snapshot()
	if item.fresh && !snap.Fresh {
		return false
	}
	if item.residentSignature != "" && item.residentSignature != publicationSignature(snap) {
		return false
	}
	if item.observation.Channel == ChannelAttention {
		for _, a := range snap.Items {
			if a.ID == item.target.ID {
				return !a.Handled && a.Revision == item.target.Revision && a.Fingerprint == item.target.Fingerprint
			}
		}
		return false
	}
	return true
}
