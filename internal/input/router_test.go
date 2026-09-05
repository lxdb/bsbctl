package input

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/busylib-go/proto/inputpb"
)

func TestRouterBlockedLaunchDoesNotHoldStateLockOrCloseNewMenu(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	backend := &blockingLauncher{started: started, release: release}
	var withdrawals atomic.Int32
	router := NewRouter(backend, func(protocol.Observation) error { return nil }, func() { withdrawals.Add(1) }, time.Now)
	apps := &inputpb.InputEvent{Event: &inputpb.InputEvent_SwitchEvent{SwitchEvent: &inputpb.SwitchEvent{Position: inputpb.SwitchPosition_APPS}}}
	if err := router.Handle(context.Background(), apps); err != nil {
		t.Fatal(err)
	}
	launchDone := make(chan error, 1)
	go func() {
		launchDone <- router.Handle(context.Background(), &inputpb.InputEvent{Event: &inputpb.InputEvent_ButtonEvent{
			ButtonEvent: &inputpb.ButtonEvent{Button: inputpb.Button_OK, Action: inputpb.ButtonAction_PRESS},
		}})
	}()
	<-started
	if err := router.Handle(context.Background(), &inputpb.InputEvent{Event: &inputpb.InputEvent_SwitchEvent{
		SwitchEvent: &inputpb.SwitchEvent{Position: inputpb.SwitchPosition_BUSY},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(context.Background(), apps); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-launchDone; err != nil {
		t.Fatal(err)
	}
	if withdrawals.Load() != 1 {
		t.Fatalf("stale launch closed the new menu: withdrawals=%d", withdrawals.Load())
	}
}

func TestRouterStaleBlockedPublishRestoresNewerMenu(t *testing.T) {
	backend := &fakeLauncher{apps: []App{{ID: "ball8", Action: "ask"}}}
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var latest atomic.Uint64
	router := NewRouter(backend, func(value protocol.Observation) error {
		call := calls.Add(1)
		if call == 1 {
			close(firstStarted)
			<-release
		}
		latest.Store(value.Revision)
		return nil
	}, func() { latest.Store(0) }, time.Now)
	apps := &inputpb.InputEvent{Event: &inputpb.InputEvent_SwitchEvent{SwitchEvent: &inputpb.SwitchEvent{Position: inputpb.SwitchPosition_APPS}}}
	firstDone := make(chan error, 1)
	go func() { firstDone <- router.Handle(context.Background(), apps) }()
	<-firstStarted
	if err := router.Handle(context.Background(), &inputpb.InputEvent{Event: &inputpb.InputEvent_SwitchEvent{SwitchEvent: &inputpb.SwitchEvent{Position: inputpb.SwitchPosition_BUSY}}}); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(context.Background(), apps); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if latest.Load() != 3 {
		t.Fatalf("latest revision = %d, want restored revision 3", latest.Load())
	}
}

func TestRouterCurrentPublishFailureDeactivatesInvisibleMenu(t *testing.T) {
	wantErr := errors.New("publish failed")
	withdrawals := 0
	router := NewRouter(&fakeLauncher{apps: []App{{ID: "calendar", Action: "open"}}}, func(protocol.Observation) error {
		return wantErr
	}, func() { withdrawals++ }, time.Now)

	if err := router.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)); !errors.Is(err, wantErr) {
		t.Fatalf("publish error = %v, want %v", err, wantErr)
	}
	if router.Active() || withdrawals != 1 {
		t.Fatalf("failed menu state: active=%t withdrawals=%d", router.Active(), withdrawals)
	}
	if err := router.Handle(t.Context(), buttonPress(inputpb.Button_OK)); err != nil {
		t.Fatal(err)
	}
}

func TestRouterStalePublishFailureDoesNotCloseNewerMenu(t *testing.T) {
	backend := &fakeLauncher{apps: []App{{ID: "calendar", Action: "open"}}}
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	wantErr := errors.New("stale publish failed")
	var calls atomic.Int32
	var withdrawals atomic.Int32
	router := NewRouter(backend, func(protocol.Observation) error {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-release
			return wantErr
		}
		return nil
	}, func() { withdrawals.Add(1) }, time.Now)

	firstDone := make(chan error, 1)
	go func() { firstDone <- router.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)) }()
	<-firstStarted
	if err := router.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-firstDone; !errors.Is(err, wantErr) {
		t.Fatalf("stale publish error = %v, want %v", err, wantErr)
	}
	if !router.Active() || withdrawals.Load() != 0 {
		t.Fatalf("newer menu state: active=%t withdrawals=%d", router.Active(), withdrawals.Load())
	}
}

func TestRouterOpensAppsNavigatesAndLaunchesOnOKPress(t *testing.T) {
	t.Parallel()
	backend := &fakeLauncher{apps: []App{
		{ID: "ball8", DisplayName: "Magic 8 Ball", Action: "ask"},
		{ID: "calendar-work", DisplayName: "Calendar", Action: "open"},
	}}
	var shown protocol.Observation
	withdrawn := false
	router := NewRouter(backend, func(value protocol.Observation) error {
		shown = value
		return nil
	}, func() { withdrawn = true }, func() time.Time {
		return time.Date(2026, 8, 20, 6, 0, 0, 0, time.UTC)
	})

	if err := router.Handle(context.Background(), &inputpb.InputEvent{Event: &inputpb.InputEvent_SwitchEvent{
		SwitchEvent: &inputpb.SwitchEvent{Position: inputpb.SwitchPosition_APPS},
	}}); err != nil {
		t.Fatalf("APPS: %v", err)
	}
	if launcherElementText(t, shown, "front-app") != "MAGIC 8 BALL" || shown.Disposition != protocol.DispositionSnapshot {
		t.Fatalf("initial launcher = %#v", shown)
	}
	if err := router.Handle(context.Background(), &inputpb.InputEvent{Event: &inputpb.InputEvent_EncoderEvent{
		EncoderEvent: &inputpb.EncoderEvent{Delta: 1},
	}}); err != nil {
		t.Fatalf("encoder: %v", err)
	}
	if selected := launcherElementText(t, shown, "front-app"); selected != "CALENDAR" {
		t.Fatalf("selected text = %q", selected)
	}
	if err := router.Handle(context.Background(), &inputpb.InputEvent{Event: &inputpb.InputEvent_ButtonEvent{
		ButtonEvent: &inputpb.ButtonEvent{Button: inputpb.Button_OK, Action: inputpb.ButtonAction_PRESS},
	}}); err != nil {
		t.Fatalf("OK: %v", err)
	}
	if backend.launched != "calendar-work/open" || !withdrawn {
		t.Fatalf("launched/withdrawn = %q/%v", backend.launched, withdrawn)
	}
}

func TestRouterEncoderWrapsInBothDirections(t *testing.T) {
	t.Parallel()
	backend := &fakeLauncher{apps: []App{
		{ID: "calendar", DisplayName: "Calendar", Action: "open"},
		{ID: "codex", DisplayName: "Codex", Action: "open"},
		{ID: "quota", DisplayName: "Codex Quota", Action: "open"},
	}}
	var shown protocol.Observation
	router := NewRouter(backend, func(value protocol.Observation) error {
		shown = value
		return nil
	}, func() {}, time.Now)
	if err := router.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(t.Context(), encoder(-1)); err != nil {
		t.Fatal(err)
	}
	if got := launcherElementText(t, shown, "front-app"); got != "CODEX QUOTA" {
		t.Fatalf("counter-clockwise wrap selected %q", got)
	}
	if err := router.Handle(t.Context(), encoder(1)); err != nil {
		t.Fatal(err)
	}
	if got := launcherElementText(t, shown, "front-app"); got != "CALENDAR" {
		t.Fatalf("clockwise wrap selected %q", got)
	}
}

func TestRouterUsesStableAppIDWhenDisplayNameIsUnavailable(t *testing.T) {
	t.Parallel()
	backend := &fakeLauncher{apps: []App{{ID: "custom-app", Action: "open"}}}
	var shown protocol.Observation
	router := NewRouter(backend, func(value protocol.Observation) error {
		shown = value
		return nil
	}, func() {}, time.Now)

	if err := router.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	if got := launcherElementText(t, shown, "front-app"); got != "CUSTOM APP" {
		t.Fatalf("fallback launcher label = %q, want %q", got, "CUSTOM APP")
	}
}

func TestRouterReportsNoAvailableApps(t *testing.T) {
	t.Parallel()
	var shown protocol.Observation
	router := NewRouter(&fakeLauncher{}, func(value protocol.Observation) error {
		shown = value
		return nil
	}, func() {}, time.Now)

	if err := router.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	if got := launcherElementText(t, shown, "front-app"); got != "No available apps" {
		t.Fatalf("empty launcher message = %q, want %q", got, "No available apps")
	}
}

func launcherElementText(t *testing.T, value protocol.Observation, id string) string {
	t.Helper()
	for _, element := range value.Scene.Elements {
		if element.ID == id && element.Text != nil {
			return element.Text.Value
		}
	}
	t.Fatalf("launcher element %q not found", id)
	return ""
}

func TestRouterLauncherSceneUsesBothDisplaysWithExplicitLayout(t *testing.T) {
	t.Parallel()
	backend := &fakeLauncher{apps: []App{{ID: "codex-quota", Action: "open"}}}
	var shown protocol.Observation
	router := NewRouter(backend, func(value protocol.Observation) error {
		shown = value
		return nil
	}, func() {}, time.Now)

	if err := router.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	if shown.Scene == nil {
		t.Fatal("launcher scene is nil")
	}
	if _, err := presentation.CompileScene("bsbctl", 100, presentation.ResolveScene(*shown.Scene)); err != nil {
		t.Fatalf("compile launcher scene: %v", err)
	}
	displays := map[protocol.Display]bool{}
	for _, element := range shown.Scene.Elements {
		displays[element.Display] = true
		if element.ID == "front-background" || element.ID == "back-background" || element.ID == "front-app" || element.ID == "front-position" {
			continue
		}
		maxX, maxY := 70, 14
		if element.Display == protocol.DisplayBack {
			maxX, maxY = 146, 78
		}
		if element.X < 1 || element.X > maxX || element.Y < 1 || element.Y > maxY {
			t.Fatalf("%s origin = (%d,%d), outside safe %s area", element.ID, element.X, element.Y, element.Display)
		}
		if element.Rectangle != nil && (element.X+element.Rectangle.Width-1 > maxX || element.Y+element.Rectangle.Height-1 > maxY) {
			t.Fatalf("%s rectangle reaches (%d,%d), outside safe %s area", element.ID, element.X+element.Rectangle.Width-1, element.Y+element.Rectangle.Height-1, element.Display)
		}
	}
	if !displays[protocol.DisplayFront] || !displays[protocol.DisplayBack] {
		t.Fatalf("launcher displays = %#v, want front and back", displays)
	}
	for id, geometry := range map[string][4]int{
		"front-accent":   {1, 1, 3, 14},
		"front-app":      {36, 0, 64, 0},
		"front-position": {36, 15, 0, 0},
		"back-accent":    {1, 1, 4, 78},
		"back-surface":   {4, 13, 140, 60},
		"back-app":       {8, 20, 132, 0},
		"back-divider":   {8, 45, 132, 1},
		"back-action":    {140, 65, 0, 0},
	} {
		element := launcherElement(t, shown, id)
		width, height := 0, 0
		if element.Rectangle != nil {
			width, height = element.Rectangle.Width, element.Rectangle.Height
		} else if element.Text != nil {
			width = element.Text.Width
		}
		if got := [4]int{element.X, element.Y, width, height}; got != geometry {
			t.Fatalf("%s geometry = %v, want %v", id, got, geometry)
		}
	}
	if got := launcherElement(t, shown, "front-app").Text.Align; got != "top_mid" {
		t.Fatalf("front app alignment = %q, want top_mid", got)
	}
	if got := launcherElement(t, shown, "front-position").Text.Align; got != "bottom_mid" {
		t.Fatalf("front position alignment = %q, want bottom_mid", got)
	}
	if got := launcherElementText(t, shown, "back-action"); got != "OK TO OPEN" {
		t.Fatalf("back action = %q, want OK TO OPEN", got)
	}
}

func launcherElement(t *testing.T, value protocol.Observation, id string) presentation.Element {
	t.Helper()
	for _, element := range value.Scene.Elements {
		if element.ID == id {
			return element
		}
	}
	t.Fatalf("launcher element %q not found", id)
	return presentation.Element{}
}

func TestRouterConsumesStartWithoutChangingLauncher(t *testing.T) {
	t.Parallel()
	backend := &fakeLauncher{apps: []App{{ID: "calendar", Action: "open"}}}
	withdrawn := false
	router := NewRouter(backend, func(protocol.Observation) error { return nil }, func() { withdrawn = true }, time.Now)

	if err := router.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(t.Context(), buttonPress(inputpb.Button_START)); err != nil {
		t.Fatal(err)
	}
	if backend.launched != "" || withdrawn || !router.Active() {
		t.Fatalf("launched/withdrawn/active = %q/%v/%v, want no launch, no withdrawal, active", backend.launched, withdrawn, router.Active())
	}
}

func TestRouterDoesNotOwnPhysicalBackPolicy(t *testing.T) {
	t.Parallel()
	withdrawn := false
	router := NewRouter(&fakeLauncher{apps: []App{{ID: "calendar", Action: "open"}}}, func(protocol.Observation) error { return nil }, func() {
		withdrawn = true
	}, time.Now)
	if err := router.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(t.Context(), buttonPress(inputpb.Button_BACK)); err != nil {
		t.Fatal(err)
	}
	if withdrawn || !router.Active() {
		t.Fatalf("raw Back bypassed coordinator policy: withdrawn=%t active=%t", withdrawn, router.Active())
	}
}

func TestRouterIgnoresButtonReleaseAndNonAppsInput(t *testing.T) {
	t.Parallel()
	backend := &fakeLauncher{apps: []App{{ID: "ball8", Action: "ask"}}}
	router := NewRouter(backend, func(protocol.Observation) error { return nil }, func() {}, time.Now)
	if err := router.Handle(context.Background(), &inputpb.InputEvent{Event: &inputpb.InputEvent_ButtonEvent{
		ButtonEvent: &inputpb.ButtonEvent{Button: inputpb.Button_OK, Action: inputpb.ButtonAction_RELEASE},
	}}); err != nil {
		t.Fatal(err)
	}
	if backend.launched != "" {
		t.Fatalf("release launched %q", backend.launched)
	}
}

type fakeLauncher struct {
	apps     []App
	launched string
}

type blockingLauncher struct {
	started chan struct{}
	release chan struct{}
}

func (*blockingLauncher) Apps() []App { return []App{{ID: "ball8", Action: "ask"}} }
func (b *blockingLauncher) Launch(context.Context, string, string) error {
	close(b.started)
	<-b.release
	return nil
}

func (f *fakeLauncher) Apps() []App { return append([]App(nil), f.apps...) }
func (f *fakeLauncher) Launch(_ context.Context, appID, action string) error {
	f.launched = appID + "/" + action
	return nil
}
