package githubnotifications

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const interactionTimeout = 5 * time.Second

type panelLevel int

const (
	panelList panelLevel = iota
	panelDetail
	panelConfirm
)

type interactionSession struct {
	token        string
	observedAt   time.Time
	selected     item
	level        panelLevel
	action       int
	lastSequence uint64
}

func openBrowser(ctx context.Context, target string) error {
	if !validBrowserURL(target) {
		return errors.New("unsafe browser URL")
	}
	if err := exec.CommandContext(ctx, "/usr/bin/open", target).Run(); err != nil {
		return errors.New("browser open failed")
	}
	return nil
}

// StartSession binds a panel to an exact observation or launcher; effects require
// a later input after core has promoted this session.
func (h *Handler) StartSession(ctx context.Context, r protocol.SessionStartRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Action != "open" || r.Trigger == nil {
		return errors.New("notification panel requires an open trigger")
	}
	w := h.findWorker(r.Instance)
	if w == nil {
		return errors.New("notification instance is not active")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.retiring.Load() {
		return errors.New("notification instance is retiring")
	}
	if w.session != nil {
		return errors.New("notification panel is already active")
	}
	s := &interactionSession{token: r.SessionToken, observedAt: w.now().UTC()}
	switch r.Trigger.Kind {
	case protocol.SessionTriggerLauncher:
		items := w.state.ordered()
		if len(items) > 0 {
			s.selected = items[0]
		}
	case protocol.SessionTriggerObservation:
		ref := r.Trigger.Observation
		o, ok := w.published[ref.Channel+"/"+ref.Key]
		if !ok || o.Revision != ref.Revision {
			return ErrStaleNotification
		}
		switch ref.Channel {
		case ChannelAttention:
			i, ok := w.state.items[ref.Key]
			if !ok || w.publishedItems[ref.Channel+"/"+ref.Key] != i.Revision || !w.state.fresh(w.now()) {
				return ErrStaleNotification
			}
			s.selected = i
			s.level = panelDetail
		case ChannelSummary, ChannelConnection:
			items := w.state.ordered()
			if len(items) > 0 {
				s.selected = items[0]
			}
		default:
			return errors.New("notification channel cannot activate")
		}
	}
	w.actionError = ""
	w.session = s
	if err := w.refreshPanel(ctx); err != nil {
		w.session = nil
		return err
	}
	return nil
}

func (h *Handler) EndSession(ctx context.Context, r protocol.SessionEndRequest) error {
	w := h.findWorker(r.Instance)
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.session == nil || w.session.token != r.SessionToken {
		return nil
	}
	w.session = nil
	return w.refreshPanel(ctx)
}

func (h *Handler) HandleSessionInput(ctx context.Context, r protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	if err := r.Validate(); err != nil {
		return inputResult(false), err
	}
	w := h.findWorker(r.Instance)
	if w == nil {
		return inputResult(false), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.session
	if s == nil || s.token != r.SessionToken {
		return inputResult(false), nil
	}
	if r.Sequence <= s.lastSequence {
		return inputResult(true), nil
	}
	s.lastSequence = r.Sequence
	if err := ctx.Err(); err != nil {
		return inputResult(true), err
	}
	if w.closed || w.retiring.Load() {
		return inputResult(true), ErrStaleNotification
	}
	effect := false
	if encoder := r.Input.Encoder; encoder != nil {
		switch s.level {
		case panelList:
			items := w.state.ordered()
			if len(items) > 0 {
				index := slices.IndexFunc(items, func(i item) bool { return i.ID == s.selected.ID })
				if index < 0 {
					index = 0
				}
				index = (index + int(encoder.Delta)%len(items) + len(items)) % len(items)
				s.selected = items[index]
			}
		case panelDetail:
			s.level = panelConfirm
			s.action = 1
		case panelConfirm:
			s.level = panelDetail
			s.action = 0
		}
	} else if button := r.Input.Button; button.Action == protocol.ButtonPress {
		switch button.Button {
		case protocol.ButtonBack:
			if s.level == panelList {
				return inputResult(false), nil
			}
			if s.level == panelConfirm {
				s.level = panelDetail
				s.action = 0
			} else {
				s.level--
			}
		case protocol.ButtonOK:
			if s.selected.ID == "" {
				return inputResult(true), nil
			}
			if s.level == panelList {
				s.level = panelDetail
			}
		case protocol.ButtonStart:
			if s.level == panelConfirm {
				s.action = 1
				effect = true
			} else if s.level == panelList || s.level == panelDetail {
				if s.selected.ID != "" {
					s.action = 0
					effect = true
				}
			}
		}
	} else {
		return inputResult(true), nil
	}
	if !effect {
		return inputResult(true), w.refreshPanel(ctx)
	}
	return inputResult(true), h.executeSelection(ctx, w, s)
}

// executeSelection holds the worker lock across resolution, admission, and the
// effect so a collector cannot replace a validated item in that interval.
func (h *Handler) executeSelection(ctx context.Context, w *worker, s *interactionSession) error {
	actionCtx, cancel := context.WithTimeout(ctx, interactionTimeout)
	defer cancel()
	if !w.selectionCurrent(s) {
		return ErrStaleNotification
	}
	target := ""
	if s.action == 0 {
		var err error
		target, _, err = w.source.resolveSubject(actionCtx, s.selected.notification)
		if err != nil {
			w.actionError = ErrorCode(err)
			_ = w.refreshPanel(actionCtx)
			return err
		}
		if !validBrowserURL(target) {
			return errors.New("unsafe browser URL")
		}

	}
	if !w.selectionCurrent(s) {
		return ErrStaleNotification
	}
	if err := actionCtx.Err(); err != nil {
		return err
	}
	if err := w.host.BeginSessionExecution(actionCtx, protocol.SessionExecutionRequest{Instance: w.ref, SessionToken: s.token}); err != nil {
		return err
	}
	// Completion is mandatory after a grant, including cancellation or uncertain
	// opener failure. A fresh bounded context reserves time inside the input budget.
	var effectErr error
	if !w.selectionCurrent(s) {
		effectErr = ErrStaleNotification
	} else if err := actionCtx.Err(); err != nil {
		effectErr = err
	} else {
		if s.action == 0 {
			if err := h.openURL(actionCtx, target); err != nil {
				effectErr = &sourceError{Code: "open_failed"}
				w.actionError = "open_failed"
			}
		}
		if effectErr == nil {
			w.writeEpoch++
			writeErr := w.source.markRead(actionCtx, s.selected.notification)
			w.state.lastModified = ""
			if writeErr == nil {
				delete(w.state.items, s.selected.ID)
				delete(w.state.handled, s.selected.ID)
				w.state.revision++
				w.state.checkpointDirty = true
			} else {
				w.actionError = ErrorCode(writeErr)
				if s.action == 0 && w.actionError != "read_unknown" {
					w.actionError = "opened_read_failed"
				}
				if ErrorCode(writeErr) == "read_unknown" {
					i := s.selected
					if i.EpisodeID == "" {
						i.EpisodeID = hashKey(i.ID, i.Reason, i.UpdatedAt.UTC().Format(time.RFC3339Nano))
						i.ObservedAt = w.now()
					}
					w.state.handled[i.ID] = handledEpisode{ID: i.ID, Reason: i.Reason, EpisodeID: i.EpisodeID, ObservedAt: i.ObservedAt, UpdatedAt: i.UpdatedAt, HandledAt: w.now(), Uncertain: true}
					i.Handled = true
					w.state.revision++
					i.Revision = w.state.revision
					w.state.items[i.ID] = i
					w.state.checkpointDirty = true
				}
				effectErr = writeErr
			}
		}
	}
	w.session = nil
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer finishCancel()
	if w.state.checkpointDirty {
		w.checkpointError = w.persistCheckpoint(finishCtx) != nil
	}
	completeErr := w.host.CompleteSession(finishCtx, protocol.CompleteSessionRequest{Instance: w.ref, SessionToken: s.token})
	return errors.Join(effectErr, completeErr, w.refreshPanel(finishCtx))
}
func (w *worker) selectionCurrent(s *interactionSession) bool {
	i, ok := w.state.items[s.selected.ID]
	return !w.closed && !w.retiring.Load() && w.session == s && ok && !w.state.handled[i.ID].Uncertain && i.Revision == s.selected.Revision && w.state.fresh(w.now())
}
func (w *worker) refreshPanel(ctx context.Context) error {
	w.lastPublishedAt = time.Time{}
	return w.publish(ctx)
}
func inputResult(consumed bool) protocol.SessionInputResult {
	if consumed {
		return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}
	}
	return protocol.SessionInputResult{Disposition: protocol.SessionInputNotConsumed}
}
