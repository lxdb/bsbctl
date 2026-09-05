// Package input maps BUSY Bar physical controls to core-owned interaction state.
package input

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/busylib-go/proto/inputpb"
)

type App struct {
	ID          string
	DisplayName string
	Action      string
}

const (
	launcherCanvas    = "#071522FF"
	launcherSurface   = "#171A21FF"
	launcherBorder    = "#2B3940FF"
	launcherText      = "#EAF4F2FF"
	launcherSecondary = "#9AAFB2FF"
	launcherAccent    = "#2AC7B5FF"
)

type Launcher interface {
	Apps() []App
	Launch(context.Context, string, string) error
}

type Router struct {
	mu       sync.Mutex
	launcher Launcher
	publish  func(protocol.Observation) error
	withdraw func()
	now      func() time.Time
	active   bool
	apps     []App
	selected int
	revision uint64
	created  time.Time
}

func NewRouter(launcher Launcher, publish func(protocol.Observation) error, withdraw func(), now func() time.Time) *Router {
	if now == nil {
		now = time.Now
	}
	return &Router{launcher: launcher, publish: publish, withdraw: withdraw, now: now}
}

func (r *Router) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

// Close withdraws the launcher without changing any plugin session.
func (r *Router) Close() { _ = r.close() }

// Handle mutates only core menu state under mu. Launcher and observation
// callbacks always run after unlocking so a slow plugin cannot stop input drain.
func (r *Router) Handle(ctx context.Context, event *inputpb.InputEvent) error {
	if event == nil {
		return nil
	}
	switch value := event.Event.(type) {
	case *inputpb.InputEvent_SwitchEvent:
		if value.SwitchEvent.GetPosition() == inputpb.SwitchPosition_APPS {
			apps := r.launcher.Apps()
			r.mu.Lock()
			r.active = true
			r.apps = apps
			r.selected = 0
			r.created = r.now().UTC()
			observation, token := r.showLocked()
			r.mu.Unlock()
			return r.publishCurrent(observation, token)
		}
		return r.close()
	case *inputpb.InputEvent_EncoderEvent:
		r.mu.Lock()
		if !r.active || len(r.apps) == 0 || value.EncoderEvent.GetDelta() == 0 {
			r.mu.Unlock()
			return nil
		}
		r.selected = wrap(r.selected+int(value.EncoderEvent.GetDelta()), len(r.apps))
		observation, token := r.showLocked()
		r.mu.Unlock()
		return r.publishCurrent(observation, token)
	case *inputpb.InputEvent_ButtonEvent:
		button := value.ButtonEvent
		if button.GetAction() != inputpb.ButtonAction_PRESS {
			return nil
		}
		switch button.GetButton() {
		case inputpb.Button_START:
			return nil
		case inputpb.Button_OK:
			r.mu.Lock()
			if !r.active || len(r.apps) == 0 {
				r.mu.Unlock()
				return nil
			}
			selected, token := r.apps[r.selected], r.revision
			r.mu.Unlock()
			if err := r.launcher.Launch(ctx, selected.ID, selected.Action); err != nil {
				return err
			}
			return r.closeIfCurrent(token)
		}
	}
	return nil
}

func (r *Router) showLocked() (protocol.Observation, uint64) {
	r.revision++
	return r.observationLocked(), r.revision
}

func (r *Router) observationLocked() protocol.Observation {
	text := "No available apps"
	position := "0 / 0"
	if len(r.apps) > 0 {
		app := r.apps[r.selected]
		text = strings.TrimSpace(app.DisplayName)
		if text == "" {
			text = app.ID
		}
		text = strings.ToUpper(strings.ReplaceAll(text, "-", " "))
		position = fmt.Sprintf("%d / %d", r.selected+1, len(r.apps))
	}
	now := r.now().UTC()
	return protocol.Observation{
		Instance: protocol.InstanceRef{ID: "launcher", Generation: 1}, Channel: "apps", Key: "menu", Revision: r.revision,
		Disposition: protocol.DispositionSnapshot, Impact: protocol.ImpactNormal,
		ReasonCode: "launcher_active", ObservedAt: r.created, UpdatedAt: now, ValidUntil: now.Add(24 * time.Hour),
		Scene: launcherScene(text, position),
	}
}

func launcherScene(app, position string) *presentation.Scene {
	return &presentation.Scene{Elements: []presentation.Element{
		launcherRectangle("front-background", protocol.DisplayFront, 0, 0, 72, 16, launcherCanvas),
		launcherRectangle("front-accent", protocol.DisplayFront, 1, 1, 3, 14, launcherAccent),
		launcherMarquee("front-app", protocol.DisplayFront, app, "normal", launcherText, 36, 0, 64, "top_mid"),
		launcherTextElement("front-position", protocol.DisplayFront, position, "tiny", launcherAccent, 36, 15, "bottom_mid"),

		launcherRectangle("back-background", protocol.DisplayBack, 0, 0, 160, 80, launcherCanvas),
		launcherRectangle("back-surface", protocol.DisplayBack, 4, 13, 140, 60, launcherSurface),
		launcherRectangle("back-accent", protocol.DisplayBack, 1, 1, 4, 78, launcherAccent),
		launcherTextElement("back-eyebrow", protocol.DisplayBack, "APPS / LAUNCHER", "tiny", launcherSecondary, 8, 3, ""),
		launcherMarquee("back-app", protocol.DisplayBack, app, "large", launcherText, 8, 20, 132, ""),
		launcherRectangle("back-divider", protocol.DisplayBack, 8, 45, 132, 1, launcherBorder),
		launcherTextElement("back-position", protocol.DisplayBack, position, "small", launcherSecondary, 8, 52, ""),
		launcherTextElement("back-action", protocol.DisplayBack, "OK TO OPEN", "small", launcherAccent, 140, 65, "top_right"),
	}}
}

func launcherTextElement(id string, display protocol.Display, value, font, color string, x, y int, align string) presentation.Element {
	return presentation.Element{ID: id, Display: display, X: x, Y: y, Text: &protocol.TextElement{Value: value, Font: font, Color: color, Align: align}}
}

func launcherMarquee(id string, display protocol.Display, value, font, color string, x, y, width int, align string) presentation.Element {
	element := launcherTextElement(id, display, value, font, color, x, y, align)
	element.Text.Width = width
	element.Text.Marquee = &protocol.Marquee{PixelsPerMinute: 1000, StartDelayMilliseconds: 1000, RepeatDelayMilliseconds: 2500}
	return element
}

func launcherRectangle(id string, display protocol.Display, x, y, width, height int, color string) presentation.Element {
	return presentation.Element{ID: id, Display: display, X: x, Y: y, Rectangle: &protocol.RectangleElement{Width: width, Height: height, Color: color}}
}

func (r *Router) publishCurrent(value protocol.Observation, token uint64) error {
	if r.publish == nil {
		return errors.New("launcher publisher is not configured")
	}
	var firstErr error
	for {
		publishErr := r.publish(value)
		if publishErr != nil && firstErr == nil {
			firstErr = publishErr
		}
		r.mu.Lock()
		if r.active && r.revision == token {
			if publishErr != nil {
				r.active = false
				r.apps = nil
				r.selected = 0
				r.revision++
				r.mu.Unlock()
				if r.withdraw != nil {
					r.withdraw()
				}
				return firstErr
			}
			r.mu.Unlock()
			return firstErr
		}
		if !r.active {
			r.mu.Unlock()
			if r.withdraw != nil {
				r.withdraw()
			}
			return firstErr
		}
		value, token = r.observationLocked(), r.revision
		r.mu.Unlock()
	}
}

func (r *Router) close() error {
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return nil
	}
	r.active = false
	r.apps = nil
	r.selected = 0
	r.revision++
	r.mu.Unlock()
	if r.withdraw != nil {
		r.withdraw()
	}
	return nil
}

func (r *Router) closeIfCurrent(token uint64) error {
	r.mu.Lock()
	if !r.active || r.revision != token {
		r.mu.Unlock()
		return nil
	}
	r.active = false
	r.apps = nil
	r.selected = 0
	r.revision++
	r.mu.Unlock()
	if r.withdraw != nil {
		r.withdraw()
	}
	return nil
}

func wrap(value, size int) int {
	value %= size
	if value < 0 {
		value += size
	}
	return value
}
