package input

import (
	"context"
	"errors"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/busylib-go/proto/inputpb"
)

type ObservationActivator interface {
	ActivateSelected(context.Context, protocol.SessionInput) (ActivationResult, error)
}

// ActivationResult binds optional activation input to the session actually promoted.
// An empty InputTarget preserves activation without forwarding the press.
type ActivationResult struct {
	Activated   bool
	InputTarget SessionTarget
}

type SessionController interface {
	ForegroundSession() (instanceID, token string)
	ClearForegroundSessionContext(context.Context, string, string)
	BeginLauncherAdmission() (uint64, bool)
	LauncherAdmissionCurrent(uint64) bool
}

type SessionTarget struct {
	InstanceID string
	Token      string
}

type SessionInputPublisher func(instanceID, token string, payload protocol.SessionInput, occurredAt time.Time) error
type SessionInputResultPublisher func(context.Context, string, string, protocol.SessionInput, time.Time) (protocol.SessionInputResult, error)

// BackAttempt binds the eventual callback result to the presentation context
// that existed when the physical Back press was accepted.
type BackAttempt struct {
	Consumed func(context.Context) error
	Fallback func(context.Context, string) error
}

// BackHandling is a required dependency because Back has synchronous
// first-refusal semantics that ordinary input does not share.
type BackHandling struct {
	Publish SessionInputResultPublisher
	Begin   func() BackAttempt
}

// Coordinator is the single ordered decision point for physical input. Switch
// transitions are handled first; START then activates the exact rendered card
// before input falls back to the APPS menu or one foreground session token.
type Coordinator struct {
	launcher   *Router
	activate   ObservationActivator
	sessions   SessionController
	publish    SessionInputPublisher
	back       BackHandling
	invalidate func()
	reconcile  func(context.Context) error
	now        func() time.Time
}

func NewCoordinator(
	launcher *Router,
	activate ObservationActivator,
	sessions SessionController,
	publish SessionInputPublisher,
	back BackHandling,
	invalidate func(),
	reconcile func(context.Context) error,
	now func() time.Time,
) *Coordinator {
	if back.Publish == nil || back.Begin == nil {
		panic("input coordinator requires Back handling")
	}
	if now == nil {
		now = time.Now
	}
	return &Coordinator{launcher: launcher, activate: activate, sessions: sessions, publish: publish, back: back, invalidate: invalidate, reconcile: reconcile, now: now}
}

func (c *Coordinator) Handle(ctx context.Context, event *inputpb.InputEvent) error {
	if event == nil {
		return nil
	}
	if isBackPress(event) {
		return c.handleBack(ctx, event)
	}
	if isBackRelease(event) {
		return nil
	}
	canvasLost := closesFirmwareCanvas(event)
	if canvasLost && c.invalidate != nil {
		c.invalidate()
	}
	err := c.handle(ctx, event)
	if canvasLost && c.reconcile != nil {
		err = errors.Join(err, c.reconcile(ctx))
	}
	return err
}

func (c *Coordinator) handleBack(ctx context.Context, event *inputpb.InputEvent) error {
	payload, ok := sessionInput(event)
	if !ok {
		return nil
	}
	instanceID, token := c.foreground()
	launcherActive := c.launcher != nil && c.launcher.Active()
	attempt := c.back.Begin()
	if attempt.Consumed == nil || attempt.Fallback == nil {
		panic("input coordinator Back attempt is incomplete")
	}
	reason := "back_no_session"
	if instanceID != "" && token != "" {
		result, err := c.back.Publish(ctx, instanceID, token, payload, c.now().UTC())
		if err == nil {
			err = result.Validate()
		}
		if err == nil && result.Disposition == protocol.SessionInputConsumed {
			return attempt.Consumed(ctx)
		}
		if err == nil {
			reason = "back_not_consumed"
		} else {
			reason = "back_session_input_failed"
		}
	}

	err := attempt.Fallback(ctx, reason)
	if instanceID != "" && token != "" && c.sessions != nil {
		c.sessions.ClearForegroundSessionContext(ctx, instanceID, token)
	}
	if launcherActive {
		c.launcher.Close()
	}
	return err
}

func isBackPress(event *inputpb.InputEvent) bool {
	button := event.GetButtonEvent()
	return button != nil && button.GetButton() == inputpb.Button_BACK && button.GetAction() == inputpb.ButtonAction_PRESS
}

func isBackRelease(event *inputpb.InputEvent) bool {
	button := event.GetButtonEvent()
	return button != nil && button.GetButton() == inputpb.Button_BACK && button.GetAction() == inputpb.ButtonAction_RELEASE
}

func (c *Coordinator) handle(ctx context.Context, event *inputpb.InputEvent) error {
	if switchEvent := event.GetSwitchEvent(); switchEvent != nil {
		if switchEvent.GetPosition() == inputpb.SwitchPosition_APPS {
			c.clearForeground(ctx)
			var admission uint64
			if c.sessions != nil {
				var admitted bool
				admission, admitted = c.sessions.BeginLauncherAdmission()
				if !admitted {
					if c.launcher != nil {
						c.launcher.Close()
					}
					return nil
				}
			}
			if c.launcher == nil {
				return nil
			}
			err := c.launcher.Handle(ctx, event)
			if c.sessions != nil && !c.sessions.LauncherAdmissionCurrent(admission) {
				if c.launcher != nil {
					c.launcher.Close()
				}
			}
			return err
		}
		c.clearForeground(ctx)
		if c.launcher != nil && c.launcher.Active() {
			return c.launcher.Handle(ctx, event)
		}
		return nil
	}
	if c.launcher != nil && c.launcher.Active() {
		return c.launcher.Handle(ctx, event)
	}
	payload, valid := sessionInput(event)
	if !valid {
		return nil
	}
	instanceID, token := c.foreground()
	if instanceID != "" && token != "" {
		if c.publish == nil {
			return nil
		}
		return c.publish(instanceID, token, payload, c.now().UTC())
	}
	start := payload.Button != nil && payload.Button.Button == protocol.ButtonStart && payload.Button.Action == protocol.ButtonPress
	if (start || payload.Encoder != nil) && c.activate != nil {
		occurredAt := c.now().UTC()
		result, err := c.activate.ActivateSelected(ctx, payload)
		if err != nil {
			return err
		}
		if result.Activated {
			target := result.InputTarget
			if target.InstanceID != "" && target.Token != "" && c.publish != nil {
				return c.publish(target.InstanceID, target.Token, payload, occurredAt)
			}
		}
	}
	return nil
}

func closesFirmwareCanvas(event *inputpb.InputEvent) bool {
	return event.GetSwitchEvent() != nil
}

func sessionInput(event *inputpb.InputEvent) (protocol.SessionInput, bool) {
	if button := event.GetButtonEvent(); button != nil {
		buttons := map[inputpb.Button]protocol.Button{inputpb.Button_OK: protocol.ButtonOK, inputpb.Button_BACK: protocol.ButtonBack, inputpb.Button_START: protocol.ButtonStart}
		actions := map[inputpb.ButtonAction]protocol.ButtonAction{inputpb.ButtonAction_PRESS: protocol.ButtonPress, inputpb.ButtonAction_RELEASE: protocol.ButtonRelease}
		mappedButton, buttonOK := buttons[button.GetButton()]
		mappedAction, actionOK := actions[button.GetAction()]
		if !buttonOK || !actionOK {
			return protocol.SessionInput{}, false
		}
		return protocol.SessionInput{Button: &protocol.ButtonInput{Button: mappedButton, Action: mappedAction}}, true
	}
	if encoder := event.GetEncoderEvent(); encoder != nil && encoder.GetDelta() != 0 {
		return protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: encoder.GetDelta()}}, true
	}
	return protocol.SessionInput{}, false
}

func (c *Coordinator) foreground() (string, string) {
	if c.sessions == nil {
		return "", ""
	}
	return c.sessions.ForegroundSession()
}

func (c *Coordinator) clearForeground(ctx context.Context) {
	instanceID, token := c.foreground()
	if instanceID != "" && token != "" {
		c.sessions.ClearForegroundSessionContext(ctx, instanceID, token)
	}
}
