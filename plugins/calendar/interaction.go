package calendar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

type calendarInteractionSession struct {
	token    string
	eventKey string
	choices  []meetingChoice
	index    int
	applied  bool
	selected selectedEvents
	launcher bool
	card     *calendarCard
}

type calendarInteractionPublisher struct {
	mu        sync.Mutex
	host      observationHost
	instance  protocol.InstanceRef
	now       func() time.Time
	scene     func(calendarCard) protocol.Scene
	revisions map[string]uint64
}

func newCalendarInteractionPublisher(host observationHost, instance protocol.InstanceRef, now func() time.Time, scene func(calendarCard) protocol.Scene) *calendarInteractionPublisher {
	return &calendarInteractionPublisher{host: host, instance: instance, now: now, scene: scene, revisions: make(map[string]uint64)}
}

func (p *calendarInteractionPublisher) Publish(ctx context.Context, session *calendarInteractionSession) error {
	if p == nil || p.host == nil || p.instance.Validate() != nil || session == nil {
		return errors.New("Calendar interaction publisher is not configured")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := interactionKey(session.token)
	now := p.now().UTC()
	revision := p.revisions[key] + 1
	var scene protocol.Scene
	reasonCode := "meeting_attendance_choice"
	if session.launcher {
		reasonCode = "calendar_launcher"
		scene = calendarLauncherScene(session.card, p.scene)
	} else {
		scene = meetingInteractionScene(session)
	}
	observation := protocol.Observation{
		Instance: p.instance, Channel: ChannelInteraction, Key: key, Revision: revision,
		Disposition: protocol.DispositionSnapshot, Impact: protocol.ImpactNormal,
		ReasonCode: reasonCode, ObservedAt: now, UpdatedAt: now,
		ValidUntil: now.Add(15 * time.Minute), Scene: new(scene),
	}
	if err := p.host.PublishObservation(ctx, observation); err != nil {
		return err
	}
	p.revisions[key] = revision
	return nil
}

func (p *calendarInteractionPublisher) Resolve(ctx context.Context, token string) error {
	if p == nil || p.host == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := interactionKey(token)
	revision, exists := p.revisions[key]
	if !exists {
		return nil
	}
	now := p.now().UTC()
	observation := protocol.Observation{
		Instance: p.instance, Channel: ChannelInteraction, Key: key, Revision: revision + 1,
		Disposition: protocol.DispositionResolved, Impact: protocol.ImpactNormal,
		ReasonCode: "meeting_choice_closed", ObservedAt: now, UpdatedAt: now,
	}
	if err := p.host.PublishObservation(ctx, observation); err != nil {
		return err
	}
	delete(p.revisions, key)
	return nil
}

func interactionKey(token string) string {
	digest := sha256.Sum256([]byte(token))
	return "session-" + hex.EncodeToString(digest[:])
}

func meetingInteractionScene(session *calendarInteractionSession) protocol.Scene {
	lines := make([]string, 0, len(session.choices))
	for index, choice := range session.choices {
		prefix := "  "
		if index == session.index {
			prefix = "> "
		}
		lines = append(lines, prefix+strings.ToUpper(string(choice)))
	}
	return protocol.Scene{Elements: []protocol.Element{
		calendarRectangle("front-background", "front", 0, 0, 72, 16, calendarBlack),
		calendarText("front-choice", "front", strings.ToUpper(string(session.choices[session.index])), "small", calendarWhite, 2, 2, ""),
		calendarRectangle("back-background", "back", 0, 0, 160, 80, calendarBlack),
		calendarText("back-title", "back", "EVENT OPTIONS", "normal", calendarWhite, 9, 8, ""),
		calendarText("back-options", "back", strings.Join(lines, " / "), "small", calendarAccent, 9, 36, ""),
		calendarText("back-help", "back", "OK CONFIRM / BACK CANCEL", "tiny", calendarSecondary, 9, 62, ""),
	}}
}

func calendarLauncherScene(card *calendarCard, render func(calendarCard) protocol.Scene) protocol.Scene {
	if card != nil && render != nil {
		scene := render(*card)
		for index := range scene.Elements {
			if scene.Elements[index].ID == "back-action" && scene.Elements[index].Text != nil {
				scene.Elements[index].Text.Value = "BACK TO CLOSE"
			}
		}
		return scene
	}
	return protocol.Scene{Elements: []protocol.Element{
		calendarRectangle("front-background", "front", 0, 0, 72, 16, calendarBlack),
		calendarText("front-calendar", "front", "CALENDAR", "normal", calendarWhite, 36, 0, "top_mid"),
		calendarText("front-empty", "front", "NO EVENTS", "tiny", calendarSecondary, 36, 15, "bottom_mid"),
		calendarRectangle("back-background", "back", 0, 0, 160, 80, calendarBlack),
		calendarText("back-title", "back", "CALENDAR", "large", calendarWhite, 8, 12, ""),
		calendarText("back-empty", "back", "NO ACTIVE OR UPCOMING EVENTS", "small", calendarSecondary, 8, 40, ""),
		calendarText("back-action", "back", "BACK TO CLOSE", "small", calendarAccent, 8, 66, ""),
	}}
}

func (h *Handler) HandleSessionInput(ctx context.Context, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	h.mu.RLock()
	worker := h.worker
	h.mu.RUnlock()
	if worker == nil || request.Instance != (protocol.InstanceRef{ID: worker.instanceID, Generation: worker.generation}) || request.SessionToken == "" {
		return calendarInputResult(false), nil
	}
	input := &request.Input
	worker.actionMu.Lock()
	defer worker.actionMu.Unlock()
	session := worker.session
	if session == nil || session.token != request.SessionToken {
		return calendarInputResult(false), nil
	}
	if session.launcher {
		button := input.Button
		if button == nil || button.Button != protocol.ButtonBack || button.Action != protocol.ButtonPress {
			return calendarInputResult(false), nil
		}
		return calendarInputResult(true), worker.finishInteraction(ctx, session.token, true)
	}
	if encoder := input.Encoder; encoder != nil {
		if !session.applied && len(session.choices) > 0 && encoder.Delta != 0 {
			session.index = wrappedCalendarIndex(session.index+int(encoder.Delta), len(session.choices))
			return calendarInputResult(true), worker.interaction.Publish(ctx, session)
		}
		return calendarInputResult(false), nil
	}
	button := input.Button
	if button == nil || button.Action != protocol.ButtonPress {
		return calendarInputResult(false), nil
	}
	switch button.Button {
	case protocol.ButtonBack:
		return calendarInputResult(true), worker.finishInteraction(ctx, session.token, true)
	case protocol.ButtonOK:
		if !session.applied {
			executionGranted := false
			selected, err := worker.state.DecideWithGrant(ctx, session.eventKey, session.choices[session.index], func() error {
				if worker.host == nil {
					return errors.New("Calendar host is unavailable")
				}
				err := worker.host.BeginSessionExecution(ctx, protocol.SessionExecutionRequest{
					Instance: protocol.InstanceRef{ID: worker.instanceID, Generation: worker.generation}, SessionToken: session.token,
				})
				executionGranted = err == nil
				return err
			})
			if err != nil {
				worker.log(ctx, protocol.LogLevelWarn, "calendar_decision_failed", "Calendar meeting decision failed")
				if executionGranted {
					return calendarInputResult(true), errors.Join(err, worker.finishInteraction(ctx, session.token, true))
				}
				return calendarInputResult(true), err
			}
			session.applied = true
			session.selected = selected
			worker.checkpointDirty = true
		}
		if err := worker.persistCheckpoint(ctx); err != nil {
			worker.state.RetryAfter(calendarPublicationRetry)
			worker.owner.setHealth(worker, false, "calendar_checkpoint_failed", "Calendar attendance checkpoint failed")
			return calendarInputResult(true), errors.Join(err, worker.finishInteraction(ctx, session.token, true))
		}
		if err := worker.publisher.Publish(ctx, session.selected); err != nil {
			return calendarInputResult(true), errors.Join(err, worker.finishInteraction(ctx, session.token, true))
		}
		return calendarInputResult(true), worker.finishInteraction(ctx, session.token, true)
	default:
		return calendarInputResult(false), nil
	}
}

func calendarInputResult(consumed bool) protocol.SessionInputResult {
	if consumed {
		return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}
	}
	return protocol.SessionInputResult{Disposition: protocol.SessionInputNotConsumed}
}

func (w *calendarWorker) persistCheckpoint(ctx context.Context) error {
	if w.host == nil {
		return errors.New("Calendar host is unavailable")
	}
	data, err := w.state.Checkpoint()
	if err != nil {
		return err
	}
	if err := w.host.SaveCheckpoint(ctx, protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, Data: data}); err != nil {
		return fmt.Errorf("persist Calendar attendance: %w", err)
	}
	w.checkpointDirty = false
	return nil
}

func (w *calendarWorker) finishInteraction(ctx context.Context, token string, notify bool) error {
	if w.session == nil || w.session.token != token {
		return nil
	}
	w.session = nil
	resolveErr := w.interaction.Resolve(ctx, token)
	if !notify || w.host == nil {
		return resolveErr
	}
	completeErr := w.host.CompleteSession(ctx, protocol.CompleteSessionRequest{Instance: protocol.InstanceRef{ID: w.instanceID, Generation: w.generation}, SessionToken: token})
	return errors.Join(resolveErr, completeErr)
}

func wrappedCalendarIndex(index, length int) int {
	if length <= 0 {
		return 0
	}
	index %= length
	if index < 0 {
		index += length
	}
	return index
}
