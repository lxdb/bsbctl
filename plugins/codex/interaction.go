package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/plugins/codex/internal/appserver"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

const interactionTimeout = 5 * time.Second

type interactionSession struct {
	token         string
	detailKey     string
	card          Card
	request       *pendingRequest
	requestKey    string
	threadID      string
	turnID        string
	actions       []string
	actionIndex   int
	questionIndex int
	choiceIndex   int
	answers       map[string]string
	staged        bool
	processing    bool
	connection    appserver.Connection
	sensitive     bool
	launcher      bool
}

type interactionEffect struct {
	answerInCodex bool
	requestID     appserver.RawID
	result        any
	threadID      string
	turnID        string
}

func (h *Handler) StartSession(ctx context.Context, request protocol.SessionStartRequest) error {
	if request.Action != "open" || request.Instance.ID == "" || request.SessionToken == "" || request.Trigger == nil {
		return errors.New("Codex interaction requires an open trigger and session token")
	}
	h.mu.RLock()
	worker := h.worker
	h.mu.RUnlock()
	if worker == nil || request.Instance != (protocol.InstanceRef{ID: worker.instanceID, Generation: worker.generation}) {
		return errors.New("Codex instance is not active")
	}
	var card Card
	switch request.Trigger.Kind {
	case protocol.SessionTriggerLauncher:
		if request.Trigger.Observation != nil {
			return errors.New("Codex launcher trigger is invalid")
		}
		worker.stateMu.Lock()
		card = worker.reducer.LiveCard()
		worker.stateMu.Unlock()
	case protocol.SessionTriggerObservation:
		trigger := request.Trigger.Observation
		if trigger == nil || trigger.Revision == 0 {
			return errors.New("Codex interaction requires an exact observation trigger")
		}
		if trigger.Channel == ChannelDetail {
			return errors.New("Codex detail observations cannot recursively activate")
		}
		var ok bool
		card, ok = worker.publisher.Lookup(trigger.Channel, trigger.Key, trigger.Revision)
		if !ok {
			return errors.New("Codex observation trigger is stale")
		}
		if card.Channel == ChannelGuidance {
			return errors.New("Codex guidance observations cannot activate")
		}
	default:
		return errors.New("Codex interaction trigger is unsupported")
	}

	session := &interactionSession{
		token: request.SessionToken, detailKey: observationKey("session", request.SessionToken), card: card,
		answers: make(map[string]string), sensitive: worker.config.ShowSensitiveRequestDetails,
		launcher: request.Trigger.Kind == protocol.SessionTriggerLauncher,
	}
	worker.stateMu.Lock()
	if worker.session != nil {
		worker.stateMu.Unlock()
		return errors.New("Codex interaction session is already active")
	}
	session.connection = worker.reducer.connection
	if request.Trigger.Kind == protocol.SessionTriggerObservation {
		if pending, exists := worker.reducer.PendingRequest(card.Key); exists {
			session.request = &pending
			session.requestKey = pending.Key
			if pending.Interactive {
				session.actions = slices.Clone(pending.Actions)
			}
		} else if threadID, turnID, exists := worker.reducer.InterruptTarget(card.Key); exists {
			session.threadID, session.turnID = threadID, turnID
			session.actions = []string{"interrupt"}
		} else if card.Disposition == protocol.DispositionActionable && !displayOnlyActionable(card.ReasonCode) {
			worker.stateMu.Unlock()
			return errors.New("Codex actionable observation no longer has an exact target")
		}
	}
	worker.session = session
	detail := session.detailCard(worker.owner.now())
	worker.stateMu.Unlock()
	if err := worker.publisher.PublishDetail(ctx, detail); err != nil {
		worker.stateMu.Lock()
		if worker.session == session {
			worker.session = nil
		}
		worker.stateMu.Unlock()
		return err
	}
	return nil
}

func displayOnlyActionable(reasonCode string) bool {
	return reasonCode == "codex_system_error" || reasonCode == "codex_turn_failed"
}

func (h *Handler) EndSession(ctx context.Context, request protocol.SessionEndRequest) error {
	h.mu.RLock()
	worker := h.worker
	h.mu.RUnlock()
	if worker == nil || request.Instance != (protocol.InstanceRef{ID: worker.instanceID, Generation: worker.generation}) || request.SessionToken == "" {
		return nil
	}
	return worker.finishSession(ctx, request.SessionToken, false)
}

func (h *Handler) HandleSessionInput(ctx context.Context, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	h.mu.RLock()
	worker := h.worker
	h.mu.RUnlock()
	if worker == nil || request.Instance != (protocol.InstanceRef{ID: worker.instanceID, Generation: worker.generation}) || request.SessionToken == "" {
		return codexInputResult(false), nil
	}
	return worker.handleInput(ctx, request.SessionToken, &request.Input)
}

func (w *codexWorker) handleInput(ctx context.Context, token string, input *protocol.SessionInput) (protocol.SessionInputResult, error) {
	w.stateMu.Lock()
	session := w.session
	if session == nil || session.token != token || session.processing {
		w.stateMu.Unlock()
		return codexInputResult(false), nil
	}
	refresh, closeSession := false, false
	consumed := false
	var effect *interactionEffect
	if encoder := input.Encoder; encoder != nil {
		refresh = session.navigate(int(encoder.Delta))
		consumed = refresh
	} else if button := input.Button; button != nil && button.Action == protocol.ButtonPress {
		switch button.Button {
		case protocol.ButtonBack:
			consumed = true
			if session.staged {
				session.staged = false
				refresh = true
			} else {
				closeSession = true
			}
		case protocol.ButtonOK:
			effect, refresh = session.ok()
			consumed = effect != nil || refresh
			if effect != nil {
				session.processing = true
			}
		case protocol.ButtonStart:
			effect, refresh = session.start()
			consumed = effect != nil || refresh
			if effect != nil {
				session.processing = true
			}
		}
	}
	detail := session.detailCard(w.owner.now())
	w.stateMu.Unlock()
	if closeSession {
		return codexInputResult(true), w.finishSession(ctx, token, true)
	}
	if refresh {
		if err := w.publisher.PublishDetail(ctx, detail); err != nil {
			return codexInputResult(consumed), err
		}
	}
	if effect == nil {
		return codexInputResult(consumed), nil
	}
	if effect.answerInCodex {
		return codexInputResult(true), w.finishSession(ctx, token, true)
	}
	actionCtx, cancel := context.WithTimeout(ctx, interactionTimeout)
	defer cancel()
	if w.host == nil {
		return codexInputResult(true), errors.New("Codex host is unavailable")
	}
	if err := w.host.BeginSessionExecution(actionCtx, protocol.SessionExecutionRequest{
		Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, SessionToken: token,
	}); err != nil {
		w.stateMu.Lock()
		if w.session == session {
			session.processing = false
		}
		w.stateMu.Unlock()
		return codexInputResult(true), err
	}
	w.stateMu.Lock()
	current := w.session == session && w.reducer.connected && w.reducer.connection == session.connection && w.sessionTargetCurrentLocked(session)
	w.stateMu.Unlock()
	if !current {
		return codexInputResult(true), errors.Join(errors.New("Codex interaction target is no longer active"), w.finishSession(ctx, token, true))
	}
	var err error
	if effect.requestID.Valid() {
		err = w.client.Respond(actionCtx, effect.requestID, effect.result)
	} else {
		err = w.client.Interrupt(actionCtx, session.connection, effect.threadID, effect.turnID)
	}
	if err != nil {
		effectErr := errors.New("Codex app-server action failed")
		return codexInputResult(true), errors.Join(effectErr, w.finishSession(ctx, token, true))
	}
	return codexInputResult(true), w.finishSession(ctx, token, true)
}

func codexInputResult(consumed bool) protocol.SessionInputResult {
	if consumed {
		return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}
	}
	return protocol.SessionInputResult{Disposition: protocol.SessionInputNotConsumed}
}

func (s *interactionSession) navigate(delta int) bool {
	if delta == 0 || s.staged {
		return false
	}
	if s.request != nil && s.request.Kind == requestQuestion && s.request.Interactive {
		options := s.request.Questions[s.questionIndex].Options
		s.choiceIndex = wrappedIndex(s.choiceIndex+delta, len(options))
		return true
	}
	if len(s.actions) != 0 {
		s.actionIndex = wrappedIndex(s.actionIndex+delta, len(s.actions))
		return true
	}
	return false
}

func (s *interactionSession) ok() (*interactionEffect, bool) {
	if s.request != nil && s.request.Kind == requestQuestion && s.request.Interactive {
		question := s.request.Questions[s.questionIndex]
		option := question.Options[s.choiceIndex]
		if option.AnswerInCodex {
			return &interactionEffect{answerInCodex: true}, false
		}
		s.answers[question.ID] = option.Label
		if s.questionIndex < len(s.request.Questions)-1 {
			s.questionIndex++
			s.choiceIndex = 0
			return nil, true
		}
		return s.responseEffect(), false
	}
	if len(s.actions) == 0 {
		return nil, false
	}
	if !s.staged {
		s.staged = true
		return nil, true
	}
	if s.threadID != "" {
		return &interactionEffect{threadID: s.threadID, turnID: s.turnID}, false
	}
	return s.responseEffect(), false
}

func (s *interactionSession) start() (*interactionEffect, bool) {
	if s.request != nil && s.request.Kind == requestQuestion && s.request.Interactive {
		return nil, false
	}
	return s.ok()
}

func (s *interactionSession) responseEffect() *interactionEffect {
	if s.request == nil {
		return nil
	}
	var result any
	switch s.request.Kind {
	case requestCommand, requestFile:
		result = map[string]string{"decision": s.actions[s.actionIndex]}
	case requestPermission:
		permissions := json.RawMessage(`{}`)
		if s.actions[s.actionIndex] == "grantTurn" {
			permissions = append(json.RawMessage(nil), s.request.Permissions...)
		}
		result = struct {
			Permissions json.RawMessage `json:"permissions"`
			Scope       string          `json:"scope"`
		}{Permissions: permissions, Scope: "turn"}
	case requestQuestion:
		answers := make(map[string]struct {
			Answers []string `json:"answers"`
		}, len(s.answers))
		for id, label := range s.answers {
			answers[id] = struct {
				Answers []string `json:"answers"`
			}{Answers: []string{label}}
		}
		result = struct {
			Answers map[string]struct {
				Answers []string `json:"answers"`
			} `json:"answers"`
		}{Answers: answers}
	}
	return &interactionEffect{requestID: s.request.ID, result: result}
}

func (s *interactionSession) detailCard(now time.Time) Card {
	card := Card{
		Channel: ChannelDetail, Key: s.detailKey, StateWord: s.card.StateWord,
		ContextLine: s.card.ContextLine, SessionLine: s.card.SessionLine, ProjectLine: s.card.ProjectLine,
		DetailLine:  "Display only",
		Disposition: protocol.DispositionSnapshot, Impact: s.card.Impact,
		ReasonCode: "codex_detail", ObservedAt: now.UTC(), ValidUntil: now.UTC().Add(24 * time.Hour),
	}
	if s.request != nil {
		card.ContextLine = requestContext(*s.request, s.sensitive)
		if s.request.Kind == requestQuestion && s.request.Interactive {
			question := s.request.Questions[s.questionIndex]
			option := question.Options[s.choiceIndex]
			questionPosition := fmt.Sprintf("QUESTION %d/%d", s.questionIndex+1, len(s.request.Questions))
			optionPosition := fmt.Sprintf("OPTION %d/%d", s.choiceIndex+1, len(question.Options))
			card.DetailLine = optionPosition
			card.Disposition = protocol.DispositionActionable
			card.Scene = typedQuestionScene(card, questionPosition, question.Question, optionPosition, option)
		} else if s.request.Interactive && len(s.actions) != 0 {
			card.DetailLine = actionLabel(s.actions[s.actionIndex])
			card.Disposition = protocol.DispositionActionable
		} else {
			card.DetailLine = "Use Codex"
		}
	} else if len(s.actions) != 0 {
		card.DetailLine = actionLabel(s.actions[s.actionIndex])
		card.Disposition = protocol.DispositionActionable
	}
	if s.staged {
		card.DetailLine = "OK CONFIRM / BACK"
		card.Impact = protocol.ImpactCritical
	}
	return card
}

func requestContext(request pendingRequest, sensitive bool) string {
	if request.Kind == requestQuestion && len(request.Questions) != 0 {
		return request.Questions[0].Question
	}
	if !sensitive {
		return "Codex request"
	}
	var params struct {
		Command   string `json:"command"`
		Reason    string `json:"reason"`
		GrantRoot string `json:"grantRoot"`
	}
	if json.Unmarshal(request.Params, &params) == nil {
		for _, value := range []string{params.Command, params.Reason, params.GrantRoot} {
			if value := safeLine(value); value != "" {
				return value
			}
		}
	}
	return "Codex request"
}

func actionLabel(action string) string {
	switch action {
	case "grantTurn":
		return "GRANT TURN"
	default:
		return strings.ToUpper(action)
	}
}

func (w *codexWorker) finishSession(ctx context.Context, expectedToken string, notifyCore bool) error {
	w.stateMu.Lock()
	session := w.session
	if session == nil || (expectedToken != "" && session.token != expectedToken) {
		w.stateMu.Unlock()
		return nil
	}
	w.session = nil
	w.stateMu.Unlock()
	resolveErr := w.publisher.ResolveDetail(ctx, session.detailKey)
	if !notifyCore || w.host == nil {
		return resolveErr
	}
	completeErr := w.host.CompleteSession(ctx, protocol.CompleteSessionRequest{
		Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, SessionToken: session.token,
	})
	return errors.Join(resolveErr, completeErr)
}

func (w *codexWorker) staleSessionTokenLocked(disconnected bool) string {
	if w.session == nil {
		return ""
	}
	if disconnected && !w.session.launcher {
		return w.session.token
	}
	if !w.session.processing && !w.sessionTargetCurrentLocked(w.session) {
		return w.session.token
	}
	return ""
}

func (w *codexWorker) sessionTargetCurrentLocked(session *interactionSession) bool {
	if session.requestKey != "" {
		pending, exists := w.reducer.PendingRequest(session.requestKey)
		if !exists || session.request == nil || !samePendingRequestIdentity(pending, *session.request) {
			return false
		}
		thread := w.reducer.threads[session.request.ThreadID]
		if thread != nil && thread.LatestTurn != nil && thread.LatestTurn.ID != "" && thread.LatestTurn.ID != session.request.TurnID {
			return false
		}
	}
	if session.threadID != "" {
		if threadID, turnID, exists := w.reducer.InterruptTarget(session.card.Key); !exists || threadID != session.threadID || turnID != session.turnID {
			return false
		}
	}
	return true
}

func samePendingRequestIdentity(current, captured pendingRequest) bool {
	return current.Key == captured.Key &&
		current.ID.Equal(captured.ID) &&
		current.Kind == captured.Kind &&
		current.ThreadID == captured.ThreadID &&
		current.TurnID == captured.TurnID &&
		current.ItemID == captured.ItemID
}

func wrappedIndex(index, length int) int {
	if length <= 0 {
		return 0
	}
	index %= length
	if index < 0 {
		index += length
	}
	return index
}
