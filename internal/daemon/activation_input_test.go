package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/eventbus"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	busyinput "github.com/lxdb/bsbctl/internal/input"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/busylib-go/proto/inputpb"
)

func TestActivationInputWaitsForExactPromotionAndBrokerSequence(t *testing.T) {
	for _, transition := range []string{"promote", "critical", "replace", "encoder_promote", "encoder_critical", "encoder_replace"} {
		t.Run(transition, func(t *testing.T) {
			encoder := strings.HasPrefix(transition, "encoder_")
			transition = strings.TrimPrefix(transition, "encoder_")
			store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
			document := serviceDocument(true)
			app := document.Apps["ball8"]
			app.Policies["answer"] = presentation.PolicyConfig{Policy: presentation.PolicyAttention, ActivationAction: "open", ActivationInput: "start"}
			if encoder {
				policy := app.Policies["answer"]
				policy.ActivationInput = "start_or_encoder"
				app.Policies["answer"] = policy
			}
			document.Apps["ball8"] = app
			if _, err := store.ReplaceWithOutcome(0, document); err != nil {
				t.Fatal(err)
			}
			plugins := newAdmissionRacePluginController()
			selected := observation.Record{PluginID: "plugin", Generation: 1, Observation: protocol.Observation{Instance: protocol.InstanceRef{ID: "ball8", Generation: 1}, Channel: "answer", Key: "notification", Revision: 7}}
			service := newTestReconcilerWithAttention(t, store, nil, plugins, &observationDiagnostics{selected: selected})
			t.Cleanup(func() { _ = service.Close(context.Background()) })
			if err := service.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			delivered := make(chan protocol.SessionInputRequest, 2)
			broker := eventbus.New(func(_ context.Context, _ string, r protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
				delivered <- r
				return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, nil
			}, nil)
			t.Cleanup(broker.Close)
			broker.Apply([]eventbus.TargetSet{{PluginID: "plugin", InstanceIDs: []string{"ball8"}}})
			occurred := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			published := 0
			coordinator := busyinput.NewCoordinator(nil, service, service, func(id, token string, p protocol.SessionInput, at time.Time) error {
				published++
				ref, current := service.ForegroundSessionRef()
				if id != ref.ID || current != token {
					return errors.New("no longer foreground")
				}
				return broker.PublishSessionInput(ref, token, &p, at)
			}, busyinput.BackHandling{Publish: func(context.Context, string, string, protocol.SessionInput, time.Time) (protocol.SessionInputResult, error) {
				return protocol.SessionInputResult{Disposition: protocol.SessionInputNotConsumed}, nil
			}, Begin: func() busyinput.BackAttempt { return busyinput.BackAttempt{} }}, nil, nil, func() time.Time { return occurred })
			done := make(chan error, 1)
			go func() {
				event := &inputpb.InputEvent{Event: &inputpb.InputEvent_ButtonEvent{ButtonEvent: &inputpb.ButtonEvent{Button: inputpb.Button_START, Action: inputpb.ButtonAction_PRESS}}}
				if encoder {
					event = &inputpb.InputEvent{Event: &inputpb.InputEvent_EncoderEvent{EncoderEvent: &inputpb.EncoderEvent{Delta: -3}}}
				}
				done <- coordinator.Handle(t.Context(), event)
			}()
			awaitServiceSignal(t, plugins.invokeStarted, "pending activation")
			select {
			case r := <-delivered:
				t.Fatalf("input before promotion: %#v", r)
			default:
			}
			switch transition {
			case "critical":
				if !service.AcquireCritical(t.Context(), presentation.Candidate{PluginID: "alerts", InstanceID: "critical", Channel: "main", Key: "alert"}) {
					t.Fatal("critical admission rejected")
				}
			case "replace":
				if _, _, err := service.ReplaceAppConfiguration(t.Context(), "ball8", AppConfiguration{Config: json.RawMessage(`{"revision":2}`), Policies: app.Policies, LaunchAction: "open"}); err != nil {
					t.Fatal(err)
				}
			}
			close(plugins.invokeRelease)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if transition != "promote" {
				if published != 0 {
					t.Fatal("input forwarded for canceled pending session")
				}
				assertPendingAdmissionCompensatedOnce(t, service, plugins)
				return
			}
			select {
			case r := <-delivered:
				_, token, _ := plugins.snapshot()
				inputMatches := r.Input.Button != nil && r.Input.Button.Button == protocol.ButtonStart
				if encoder {
					inputMatches = r.Input.Encoder != nil && r.Input.Encoder.Delta == -3
				}
				if r.Instance != selected.Observation.Instance || r.SessionToken != string(token) || r.Sequence != 1 || r.OccurredAt != occurred || !inputMatches || published != 1 {
					t.Fatalf("wrong activated input: %#v publishes=%d", r, published)
				}
			case <-time.After(time.Second):
				t.Fatal("promoted activation input not delivered")
			}
		})
	}
}

func TestUnsetActivationInputActivatesWithoutForwardingCalendarOrCodexInput(t *testing.T) {
	for _, appID := range []string{"calendar", "codex"} {
		t.Run(appID, func(t *testing.T) {
			descriptor, ok := firstpartyplugins.LookupAppID(appID)
			if !ok {
				t.Fatal("default missing")
			}
			app := descriptor.DefaultApp
			app.Generation = 1
			definition := descriptor.DefinitionForVersion(descriptor.DevelopmentVersion)
			var channel, action string
			for id, p := range app.Policies {
				if p.ActivationInput != "" {
					t.Fatal("default app policy forwards activation input")
				}
				if p.ActivationAction != "" {
					channel, action = id, p.ActivationAction
					break
				}
			}
			if channel == "" {
				t.Fatal("activation policy missing")
			}
			store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
			document := config.Document{Version: config.CurrentVersion, Generation: 1, Plugins: map[string]config.Plugin{descriptor.ID: {ID: descriptor.ID, Version: definition.Version, Executable: "/bin/true", ProtocolVersion: protocol.Version, ExecutionModes: definition.Contract.ExecutionModes, Channels: definition.Contract.Channels, Operations: definition.Contract.Operations}}, Apps: map[string]config.App{app.ID: app}}
			if _, err := store.ReplaceWithOutcome(0, document); err != nil {
				t.Fatal(err)
			}
			plugins := &fakePluginController{}
			selected := observation.Record{PluginID: descriptor.ID, Generation: 1, Observation: protocol.Observation{Instance: protocol.InstanceRef{ID: app.ID, Generation: 1}, Channel: channel, Key: "item", Revision: 1}}
			service := newTestReconcilerWithAttention(t, store, nil, plugins, &observationDiagnostics{selected: selected})
			t.Cleanup(func() { _ = service.Close(context.Background()) })
			if err := service.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			result, err := service.ActivateSelected(t.Context(), protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonStart, Action: protocol.ButtonPress}})
			if err != nil || !result.Activated || result.InputTarget != (busyinput.SessionTarget{}) || plugins.invoked.Action != action {
				t.Fatalf("activation without forwarded input: %#v %v action=%s", result, err, plugins.invoked.Action)
			}
		})
	}
}
