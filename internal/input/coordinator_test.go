package input

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/busylib-go/proto/inputpb"
)

type coordinatorActivator struct {
	calls     int
	activated bool
	err       error
}

func (a *coordinatorActivator) ActivateSelected(context.Context) (bool, error) {
	a.calls++
	return a.activated, a.err
}

type coordinatorSessions struct {
	instance string
	token    string
	critical bool
	sequence uint64
	cleared  []SessionTarget
}

func (s *coordinatorSessions) ForegroundSession() (string, string) { return s.instance, s.token }
func (s *coordinatorSessions) BeginLauncherAdmission() (uint64, bool) {
	return s.sequence, !s.critical
}
func (s *coordinatorSessions) LauncherAdmissionCurrent(sequence uint64) bool {
	return !s.critical && s.sequence == sequence
}
func (s *coordinatorSessions) ClearForegroundSessionContext(_ context.Context, instanceID, token string) {
	s.cleared = append(s.cleared, SessionTarget{InstanceID: instanceID, Token: token})
	if s.instance == instanceID && s.token == token {
		s.instance, s.token = "", ""
	}
}

type publishedSessionInput struct {
	target  SessionTarget
	payload protocol.SessionInput
}

func testBackHandling(publish SessionInputResultPublisher, consumed func(context.Context) error, fallback func(context.Context, string) error) BackHandling {
	if publish == nil {
		publish = func(context.Context, string, string, protocol.SessionInput, time.Time) (protocol.SessionInputResult, error) {
			return protocol.SessionInputResult{Disposition: protocol.SessionInputNotConsumed}, nil
		}
	}
	if consumed == nil {
		consumed = func(context.Context) error { return nil }
	}
	if fallback == nil {
		fallback = func(context.Context, string) error { return nil }
	}
	return BackHandling{
		Publish: publish,
		Begin: func() BackAttempt {
			return BackAttempt{Consumed: consumed, Fallback: fallback}
		},
	}
}

func TestCoordinatorMakesAppsExclusiveAndConsumesOpeningOK(t *testing.T) {
	launcherBackend := &fakeLauncher{apps: []App{{ID: "codex", Action: "open"}}}
	router := NewRouter(launcherBackend, func(protocol.Observation) error { return nil }, func() {}, time.Now)
	sessions := &coordinatorSessions{instance: "old", token: "session-old"}
	activator := &coordinatorActivator{activated: true}
	var published []publishedSessionInput
	coordinator := NewCoordinator(router, activator, sessions, func(instanceID, token string, payload protocol.SessionInput, _ time.Time) error {
		published = append(published, publishedSessionInput{target: SessionTarget{InstanceID: instanceID, Token: token}, payload: payload})
		return nil
	}, testBackHandling(nil, nil, nil), nil, nil, time.Now)

	if err := coordinator.Handle(context.Background(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	if len(sessions.cleared) != 1 || sessions.cleared[0] != (SessionTarget{InstanceID: "old", Token: "session-old"}) {
		t.Fatalf("cleared sessions = %#v", sessions.cleared)
	}
	if err := coordinator.Handle(context.Background(), buttonPress(inputpb.Button_OK)); err != nil {
		t.Fatal(err)
	}
	if launcherBackend.launched != "codex/open" {
		t.Fatalf("launcher invocation = %q", launcherBackend.launched)
	}
	if len(published) != 0 || activator.calls != 0 {
		t.Fatalf("opening OK leaked: published=%#v activations=%d", published, activator.calls)
	}
}

func TestCoordinatorDirectStartActivatesSelectedObservationWithoutSessionInput(t *testing.T) {
	router := NewRouter(&fakeLauncher{}, func(protocol.Observation) error { return nil }, func() {}, time.Now)
	sessions := &coordinatorSessions{}
	activator := &coordinatorActivator{activated: true}
	published := 0
	coordinator := NewCoordinator(router, activator, sessions, func(string, string, protocol.SessionInput, time.Time) error {
		published++
		return nil
	}, testBackHandling(nil, nil, nil), nil, nil, time.Now)
	if err := coordinator.Handle(context.Background(), buttonPress(inputpb.Button_START)); err != nil {
		t.Fatal(err)
	}
	if activator.calls != 1 || published != 0 {
		t.Fatalf("activation/published = %d/%d", activator.calls, published)
	}
	if err := coordinator.Handle(context.Background(), buttonPress(inputpb.Button_OK)); err != nil {
		t.Fatal(err)
	}
	if activator.calls != 1 {
		t.Fatalf("direct OK activated a card; calls = %d", activator.calls)
	}
}

func TestCoordinatorStartActivatesRenderedObservationBeforeForegroundSession(t *testing.T) {
	router := NewRouter(&fakeLauncher{}, func(protocol.Observation) error { return nil }, func() {}, time.Now)
	sessions := &coordinatorSessions{instance: "codex", token: "session-7"}
	activator := &coordinatorActivator{activated: true}
	var published []publishedSessionInput
	coordinator := NewCoordinator(router, activator, sessions, func(instanceID, token string, payload protocol.SessionInput, _ time.Time) error {
		published = append(published, publishedSessionInput{target: SessionTarget{InstanceID: instanceID, Token: token}, payload: payload})
		return nil
	}, testBackHandling(nil, nil, nil), nil, nil, time.Now)

	if err := coordinator.Handle(context.Background(), buttonPress(inputpb.Button_START)); err != nil {
		t.Fatal(err)
	}
	if activator.calls != 1 || len(published) != 0 {
		t.Fatalf("activation/published = %d/%d, want rendered card activation only", activator.calls, len(published))
	}
}

func TestCoordinatorStartIsConsumedWhileLauncherIsActive(t *testing.T) {
	launcherBackend := &fakeLauncher{apps: []App{{ID: "codex", Action: "open"}}}
	router := NewRouter(launcherBackend, func(protocol.Observation) error { return nil }, func() {}, time.Now)
	activator := &coordinatorActivator{activated: true}
	coordinator := NewCoordinator(router, activator, &coordinatorSessions{}, nil, testBackHandling(nil, nil, nil), nil, nil, time.Now)

	if err := coordinator.Handle(context.Background(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Handle(context.Background(), buttonPress(inputpb.Button_START)); err != nil {
		t.Fatal(err)
	}
	if activator.calls != 0 || launcherBackend.launched != "" || !router.Active() {
		t.Fatalf("activation/launcher/active = %d/%q/%v, want consumed Start and active launcher", activator.calls, launcherBackend.launched, router.Active())
	}
}

func TestCoordinatorDoesNotOpenLauncherWhileCriticalOwnsPresentation(t *testing.T) {
	launcherBackend := &fakeLauncher{apps: []App{{ID: "codex", Action: "open"}}}
	router := NewRouter(launcherBackend, func(protocol.Observation) error { return nil }, func() {}, time.Now)
	sessions := &coordinatorSessions{critical: true}
	coordinator := NewCoordinator(router, nil, sessions, nil, testBackHandling(nil, nil, nil), nil, nil, time.Now)

	if err := coordinator.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	if router.Active() || launcherBackend.launched != "" {
		t.Fatalf("critical ownership opened launcher: active=%t launched=%q", router.Active(), launcherBackend.launched)
	}
}

func TestCoordinatorClosesLauncherWhenCriticalInvalidatesAdmission(t *testing.T) {
	sessions := &coordinatorSessions{}
	backend := &admissionInvalidatingLauncher{
		apps: []App{{ID: "codex", Action: "open"}},
		invalidate: func() {
			sessions.sequence++
		},
	}
	withdrawn := 0
	router := NewRouter(backend, func(protocol.Observation) error { return nil }, func() { withdrawn++ }, time.Now)
	coordinator := NewCoordinator(router, nil, sessions, nil, testBackHandling(nil, nil, nil), nil, nil, time.Now)

	if err := coordinator.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	if router.Active() || withdrawn != 1 {
		t.Fatalf("invalidated launcher admission: active=%t withdrawals=%d", router.Active(), withdrawn)
	}
}

func TestCoordinatorInvalidatesCanvasBeforeHandlingSwitch(t *testing.T) {
	launcherBackend := &fakeLauncher{apps: []App{{ID: "codex", Action: "open"}}}
	var order []string
	router := NewRouter(launcherBackend, func(protocol.Observation) error {
		order = append(order, "publish")
		return nil
	}, func() { order = append(order, "withdraw") }, time.Now)
	coordinator := NewCoordinator(router, nil, &coordinatorSessions{}, nil, testBackHandling(nil, nil, nil), func() {
		order = append(order, "invalidate")
	}, func(context.Context) error {
		order = append(order, "reconcile")
		return nil
	}, time.Now)

	if err := coordinator.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(order); got != "[invalidate publish reconcile]" {
		t.Fatalf("APPS order = %s", got)
	}
	order = nil
	if err := coordinator.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_BUSY)); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(order); got != "[invalidate withdraw reconcile]" {
		t.Fatalf("BUSY order = %s", got)
	}
}

func TestCoordinatorBackWithoutSessionUsesFallbackAndClosesLauncher(t *testing.T) {
	var order []string
	router := NewRouter(&fakeLauncher{apps: []App{{ID: "codex", Action: "open"}}}, func(protocol.Observation) error {
		order = append(order, "publish")
		return nil
	}, func() { order = append(order, "withdraw") }, time.Now)
	coordinator := NewCoordinator(router, nil, &coordinatorSessions{}, nil, testBackHandling(nil, nil, func(context.Context, string) error {
		order = append(order, "fallback")
		return nil
	}), func() {
		order = append(order, "invalidate")
	}, func(context.Context) error {
		order = append(order, "reconcile")
		return nil
	}, time.Now)

	if err := coordinator.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	order = nil
	if err := coordinator.Handle(t.Context(), buttonPress(inputpb.Button_BACK)); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(order); got != "[fallback withdraw]" {
		t.Fatalf("BACK order = %s", got)
	}
}

func TestCoordinatorBackGivesForegroundPluginFirstRefusal(t *testing.T) {
	sessions := &coordinatorSessions{instance: "calendar", token: "session-7"}
	var published []publishedSessionInput
	invalidated, reconciled, fallback := 0, 0, 0
	back := testBackHandling(func(_ context.Context, instanceID, token string, payload protocol.SessionInput, _ time.Time) (protocol.SessionInputResult, error) {
		published = append(published, publishedSessionInput{target: SessionTarget{InstanceID: instanceID, Token: token}, payload: payload})
		return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, nil
	}, func(context.Context) error {
		invalidated++
		reconciled++
		return nil
	}, func(context.Context, string) error {
		fallback++
		return nil
	})
	coordinator := NewCoordinator(nil, nil, sessions, nil, back, func() { invalidated++ }, func(context.Context) error {
		reconciled++
		return nil
	}, time.Now)

	if err := coordinator.Handle(t.Context(), buttonPress(inputpb.Button_BACK)); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 || published[0].target != (SessionTarget{InstanceID: "calendar", Token: "session-7"}) || published[0].payload.Button == nil {
		t.Fatalf("published Back = %#v", published)
	}
	if fallback != 0 || len(sessions.cleared) != 0 || invalidated != 1 || reconciled != 1 {
		t.Fatalf("consumed Back state: fallback=%d cleared=%#v invalidated=%d reconciled=%d", fallback, sessions.cleared, invalidated, reconciled)
	}
}

func TestCoordinatorBackFallbackRunsExactlyOnce(t *testing.T) {
	for _, test := range []struct {
		name   string
		result protocol.SessionInputResult
		err    error
		reason string
	}{
		{name: "not consumed", result: protocol.SessionInputResult{Disposition: protocol.SessionInputNotConsumed}, reason: "back_not_consumed"},
		{name: "callback failure", err: errors.New("plugin unavailable"), reason: "back_session_input_failed"},
		{name: "malformed result", result: protocol.SessionInputResult{}, reason: "back_session_input_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sessions := &coordinatorSessions{instance: "codex", token: "session-1"}
			fallbacks := make([]string, 0, 1)
			back := testBackHandling(func(context.Context, string, string, protocol.SessionInput, time.Time) (protocol.SessionInputResult, error) {
				return test.result, test.err
			}, nil, func(_ context.Context, reason string) error {
				fallbacks = append(fallbacks, reason)
				return nil
			})
			coordinator := NewCoordinator(nil, nil, sessions, nil, back, nil, nil, time.Now)
			if err := coordinator.Handle(t.Context(), buttonPress(inputpb.Button_BACK)); err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(fallbacks); got != "["+test.reason+"]" {
				t.Fatalf("fallback reasons = %s", got)
			}
			if len(sessions.cleared) != 1 || sessions.cleared[0] != (SessionTarget{InstanceID: "codex", Token: "session-1"}) {
				t.Fatalf("cleared sessions = %#v", sessions.cleared)
			}
		})
	}
}

func TestCoordinatorLateBackFallbackCannotClearNewForegroundSession(t *testing.T) {
	sessions := &coordinatorSessions{instance: "calendar", token: "session-old"}
	var fallbackReason string
	back := testBackHandling(func(context.Context, string, string, protocol.SessionInput, time.Time) (protocol.SessionInputResult, error) {
		sessions.instance, sessions.token = "codex", "session-new"
		return protocol.SessionInputResult{Disposition: protocol.SessionInputNotConsumed}, nil
	}, nil, func(_ context.Context, reason string) error {
		fallbackReason = reason
		return nil
	})
	coordinator := NewCoordinator(nil, nil, sessions, nil, back, nil, nil, time.Now)

	if err := coordinator.Handle(t.Context(), buttonPress(inputpb.Button_BACK)); err != nil {
		t.Fatal(err)
	}
	if fallbackReason != "back_not_consumed" {
		t.Fatalf("fallback reason = %q", fallbackReason)
	}
	if sessions.instance != "codex" || sessions.token != "session-new" {
		t.Fatalf("late fallback cleared new foreground = %q/%q", sessions.instance, sessions.token)
	}
	if len(sessions.cleared) != 1 || sessions.cleared[0] != (SessionTarget{InstanceID: "calendar", Token: "session-old"}) {
		t.Fatalf("fallback clear target = %#v", sessions.cleared)
	}
}

func TestNewCoordinatorRequiresBackHandling(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewCoordinator accepted missing Back dependencies")
		}
	}()
	NewCoordinator(nil, nil, nil, nil, BackHandling{}, nil, nil, time.Now)
}

func TestCoordinatorBackReleaseDoesNotInvokePluginOrFallback(t *testing.T) {
	sessions := &coordinatorSessions{instance: "codex", token: "session-1"}
	called := 0
	coordinator := NewCoordinator(nil, nil, sessions, func(string, string, protocol.SessionInput, time.Time) error {
		called++
		return nil
	}, testBackHandling(func(context.Context, string, string, protocol.SessionInput, time.Time) (protocol.SessionInputResult, error) {
		called++
		return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, nil
	}, nil, func(context.Context, string) error {
		called++
		return nil
	}), nil, nil, time.Now)
	if err := coordinator.Handle(t.Context(), buttonRelease(inputpb.Button_BACK)); err != nil {
		t.Fatal(err)
	}
	if called != 0 || len(sessions.cleared) != 0 {
		t.Fatalf("Back release caused action: called=%d cleared=%#v", called, sessions.cleared)
	}
}

func TestCoordinatorStartFallsBackToExactForegroundWhenNoObservationActivates(t *testing.T) {
	router := NewRouter(&fakeLauncher{}, func(protocol.Observation) error { return nil }, func() {}, time.Now)
	sessions := &coordinatorSessions{instance: "codex", token: "session-7"}
	activator := &coordinatorActivator{}
	var published []publishedSessionInput
	coordinator := NewCoordinator(router, activator, sessions, func(instanceID, token string, payload protocol.SessionInput, _ time.Time) error {
		published = append(published, publishedSessionInput{target: SessionTarget{InstanceID: instanceID, Token: token}, payload: payload})
		return nil
	}, testBackHandling(nil, nil, nil), nil, nil, time.Now)

	if err := coordinator.Handle(context.Background(), buttonPress(inputpb.Button_START)); err != nil {
		t.Fatal(err)
	}
	if activator.calls != 1 || len(published) != 1 || published[0].target != (SessionTarget{InstanceID: "codex", Token: "session-7"}) {
		t.Fatalf("activation/published = %d/%#v, want exact foreground fallback", activator.calls, published)
	}
}

func TestCoordinatorRoutesInputToExactForegroundTokenAndModeSwitchClearsIt(t *testing.T) {
	router := NewRouter(&fakeLauncher{}, func(protocol.Observation) error { return nil }, func() {}, time.Now)
	sessions := &coordinatorSessions{instance: "codex", token: "session-7"}
	activator := &coordinatorActivator{}
	var published []publishedSessionInput
	coordinator := NewCoordinator(router, activator, sessions, func(instanceID, token string, payload protocol.SessionInput, _ time.Time) error {
		published = append(published, publishedSessionInput{target: SessionTarget{InstanceID: instanceID, Token: token}, payload: payload})
		return nil
	}, testBackHandling(nil, nil, nil), nil, nil, time.Now)
	if err := coordinator.Handle(context.Background(), encoder(1)); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 || published[0].target != (SessionTarget{InstanceID: "codex", Token: "session-7"}) || published[0].payload.Encoder == nil || published[0].payload.Encoder.Delta != 1 {
		t.Fatalf("published input = %#v", published)
	}
	if err := coordinator.Handle(context.Background(), switchPosition(inputpb.SwitchPosition_BUSY)); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 || len(sessions.cleared) != 1 || sessions.cleared[0].Token != "session-7" {
		t.Fatalf("mode switch state: published=%#v cleared=%#v", published, sessions.cleared)
	}
}

func TestCoordinatorModeSwitchClearsForegroundEvenWhileLauncherIsActive(t *testing.T) {
	router := NewRouter(&fakeLauncher{}, func(protocol.Observation) error { return nil }, func() {}, time.Now)
	sessions := &coordinatorSessions{}
	coordinator := NewCoordinator(router, nil, sessions, nil, testBackHandling(nil, nil, nil), nil, nil, time.Now)
	if err := coordinator.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_APPS)); err != nil {
		t.Fatal(err)
	}
	sessions.instance, sessions.token = "codex", "session-after-apps"
	if err := coordinator.Handle(t.Context(), switchPosition(inputpb.SwitchPosition_BUSY)); err != nil {
		t.Fatal(err)
	}
	if len(sessions.cleared) != 1 || sessions.cleared[0].Token != "session-after-apps" {
		t.Fatalf("mode switch did not clear foreground: %#v", sessions.cleared)
	}
}

func switchPosition(position inputpb.SwitchPosition) *inputpb.InputEvent {
	return &inputpb.InputEvent{Event: &inputpb.InputEvent_SwitchEvent{SwitchEvent: &inputpb.SwitchEvent{Position: position}}}
}

func buttonPress(button inputpb.Button) *inputpb.InputEvent {
	return &inputpb.InputEvent{Event: &inputpb.InputEvent_ButtonEvent{ButtonEvent: &inputpb.ButtonEvent{Button: button, Action: inputpb.ButtonAction_PRESS}}}
}

func buttonRelease(button inputpb.Button) *inputpb.InputEvent {
	return &inputpb.InputEvent{Event: &inputpb.InputEvent_ButtonEvent{ButtonEvent: &inputpb.ButtonEvent{Button: button, Action: inputpb.ButtonAction_RELEASE}}}
}

type admissionInvalidatingLauncher struct {
	apps       []App
	invalidate func()
}

func (l *admissionInvalidatingLauncher) Apps() []App {
	if l.invalidate != nil {
		l.invalidate()
	}
	return append([]App(nil), l.apps...)
}

func (*admissionInvalidatingLauncher) Launch(context.Context, string, string) error { return nil }
