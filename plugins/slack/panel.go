package slack

import (
	"context"
	"errors"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

type panelLevel int

const (
	panelList panelLevel = iota
	panelDetail
	panelDismiss
)

type panelSession struct {
	token        string
	started      time.Time
	level        panelLevel
	index        int
	target       activity
	page         int
	failure      string
	lastSequence uint64
}

func wrapIndex(i, n int) int {
	if n == 0 {
		return 0
	}
	return (i%n + n) % n
}
func inputResult(consumed bool) protocol.SessionInputResult {
	if consumed {
		return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}
	}
	return protocol.SessionInputResult{Disposition: protocol.SessionInputNotConsumed}
}

func (h *Handler) StartSession(ctx context.Context, r protocol.SessionStartRequest) error {
	if r.Validate() != nil || r.Action != "open" || r.Trigger == nil {
		return errStaleActivity
	}
	w, err := h.lookup(r.Instance)
	if err != nil {
		return err
	}
	w.panelMu.Lock()
	defer w.panelMu.Unlock()
	if w.ctx.Err() != nil || ctx.Err() != nil {
		return errStaleActivity
	}
	if w.panel != nil {
		return errors.New("Slack panel is already active")
	}
	p := &panelSession{token: r.SessionToken, started: w.now().UTC()}
	if r.Trigger.Kind == protocol.SessionTriggerObservation {
		ref := r.Trigger.Observation
		w.publications.mu.Lock()
		published, ok := w.publications.current[ref.Channel+"/"+ref.Key]
		w.publications.mu.Unlock()
		if !ok || !published.confirmed || published.observation.Revision != ref.Revision || !published.observation.ValidUntil.After(w.now()) {
			return errStaleActivity
		}
		if ref.Channel == ChannelAttention {
			p.level = panelDetail
			p.target = published.target
			w.mu.Lock()
			err = w.validateTargetLocked(p.target)
			w.mu.Unlock()
			if err != nil {
				return err
			}
		} else if ref.Channel != ChannelSummary && ref.Channel != ChannelConnection {
			return errStaleActivity
		}
	}
	w.panel = p
	if err := w.publishPanel(ctx); err != nil {
		w.panel = nil
		w.updatePanelSnapshot()
		return err
	}
	return nil
}
func (h *Handler) HandleSessionInput(ctx context.Context, r protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	if err := r.Validate(); err != nil {
		return inputResult(false), err
	}
	w, err := h.lookup(r.Instance)
	if err != nil {
		return inputResult(false), err
	}
	w.panelMu.Lock()
	defer w.panelMu.Unlock()
	p := w.panel
	if p == nil || p.token != r.SessionToken || ctx.Err() != nil || w.ctx.Err() != nil {
		return inputResult(false), nil
	}
	if r.Sequence <= p.lastSequence {
		return inputResult(true), nil
	}
	p.lastSequence = r.Sequence
	if e := r.Input.Encoder; e != nil {
		switch p.level {
		case panelList:
			p.index = wrapIndex(p.index+int(e.Delta), len(pendingItems(w.snapshot().Items)))
		case panelDismiss:
			p.level = panelDetail
		case panelDetail:
			p.level = panelDismiss
		}
		return inputResult(true), w.publishPanel(ctx)
	}
	b := r.Input.Button
	if b == nil || b.Action != protocol.ButtonPress {
		return inputResult(false), nil
	}
	switch b.Button {
	case protocol.ButtonBack:
		if p.level == panelList {
			return inputResult(false), nil
		}
		p.level--
		p.failure = ""
		return inputResult(true), w.publishPanel(ctx)
	case protocol.ButtonOK:
		switch p.level {
		case panelList:
			s := w.snapshot()
			s.Items = pendingItems(s.Items)
			if len(s.Items) == 0 {
				return inputResult(true), nil
			}
			p.index = wrapIndex(p.index, len(s.Items))
			p.target = s.Items[p.index]
			p.level = panelDetail
			p.page = 0
			p.failure = ""
		case panelDetail:
			if w.cfg.rearDetails {
				p.page++
			}
		}
		return inputResult(true), w.publishPanel(ctx)
	case protocol.ButtonStart:
		if p.level == panelDetail || p.level == panelDismiss {
			return inputResult(true), h.execute(ctx, w, p, p.level == panelDismiss)
		}
	}
	return inputResult(false), nil
}
func (w *worker) validateTargetLocked(target activity) error {
	if w.ctx.Err() != nil || !w.snapshot().Fresh {
		return errStaleActivity
	}
	a, ok := w.state.aggregates[target.ID]
	_, handled := w.state.handled[target.Fingerprint]
	if !ok || handled || a.Revision != target.Revision || a.Fingerprint != target.Fingerprint {
		return errStaleActivity
	}
	return nil
}

// panelMu joins actions at retirement. mu freezes the committed domain target
// through grant and save; transport freshness and cancellation are rechecked
// after the blocking grant, immediately before the actual effect.
func (h *Handler) execute(ctx context.Context, w *worker, p *panelSession, handle bool) error {
	w.mu.Lock()
	if err := w.validateTargetLocked(p.target); err != nil {
		w.mu.Unlock()
		p.failure = "changed"
		_ = w.publishPanel(ctx)
		return err
	}
	target, err := nativeTarget(w.cfg.workspaceID, p.target)
	if err != nil {
		w.mu.Unlock()
		return err
	}
	effect, cancel := context.WithTimeout(ctx, 5*time.Second)
	stop := context.AfterFunc(w.ctx, cancel)
	defer func() { stop(); cancel() }()
	if effect.Err() != nil {
		w.mu.Unlock()
		return effect.Err()
	}
	err = w.host.BeginSessionExecution(effect, protocol.SessionExecutionRequest{Instance: w.instance.Ref(), SessionToken: p.token})
	if err != nil {
		w.mu.Unlock()
		return errors.New("Slack execution rejected")
	}
	err = w.validateTargetLocked(p.target)
	if err == nil {
		err = effect.Err()
	}
	if err == nil {
		if handle {
			err = w.handleLocked(effect, p.target.ID, p.target.Revision, p.target.Fingerprint)
		} else if h.open(effect, target) != nil {
			err = errOpen
		} else {
			err = w.openedLocked(effect, p.target)
		}
	}
	w.mu.Unlock()
	// Remove local admission before any completion callback. A lost completion
	// response must never permit another desktop launch for this token.
	w.panel = nil
	w.updatePanelSnapshot()
	cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cleanupCancel()
	call, done := w.hostContext(cleanup, time.Time{})
	completeErr := w.host.CompleteSession(call, protocol.CompleteSessionRequest{Instance: w.instance.Ref(), SessionToken: p.token})
	done()
	if completeErr != nil {
		completeErr = errors.New("Slack session completion failed")
	}
	withdrawErr := w.publishCurrentPanel(ctx)
	if err != nil && w.ctx.Err() == nil {
		reason, message := "open_failed", "Slack did not open - press START to try again"
		if handle {
			reason, message = "handle_failed", "Slack item is not saved as dismissed - press START to try again"
		}
		snap := w.snapshot()
		if snap.OpenUnsaved {
			reason, message = "open_unsaved", "Slack opened; local pending state is not saved yet"
		}
		now := w.now().UTC()
		key := w.observationKey("connection")
		scene := textScene(message, []string{"SLACK", "NO AUTOMATIC RETRY", coverage(snap)}, snap)
		_ = w.publish(ctx, ChannelConnection+"/"+key, publishedItem{observation: protocol.Observation{Instance: w.instance.Ref(), Channel: ChannelConnection, Key: key, Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNotable, ReasonCode: reason, ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(sceneTTL), Scene: new(scene)}})
	}
	return errors.Join(err, completeErr, withdrawErr)
}
func (h *Handler) EndSession(ctx context.Context, r protocol.SessionEndRequest) error {
	w, err := h.lookup(r.Instance)
	if err != nil {
		return nil
	}
	w.panelMu.Lock()
	defer w.panelMu.Unlock()
	if w.panel == nil || w.panel.token != r.SessionToken {
		return nil
	}
	w.panel = nil
	return w.publishPanel(ctx)
}
