package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	busylib "github.com/lxdb/busylib-go"
)

func newTestEngine(store *observation.Store, resolve attention.Resolver, renderer Renderer) *Engine {
	return newEngineWithRetry(store, resolve, renderer, 2*time.Second)
}

func newTestEngineWithRetry(store *observation.Store, resolve attention.Resolver, renderer Renderer, retry time.Duration) *Engine {
	return newEngineWithRetry(store, resolve, renderer, retry)
}

func setTestEngineRecorder(engine *Engine, recorder *attention.Recorder) {
	engine.mu.Lock()
	engine.recorder = recorder
	engine.mu.Unlock()
}

func setTestForegroundCoordinator(engine *Engine, coordinator ForegroundCoordinator) {
	engine.mu.Lock()
	engine.foreground = coordinator
	engine.mu.Unlock()
}

func TestEngineConstructorRejectsEveryMissingRequiredDependency(t *testing.T) {
	store := observation.NewStore(func(string, string) (uint64, bool) { return 1, true }, time.Now)
	resolve := attention.Resolver(func(observation.Record) (attention.Rule, bool) { return attention.Rule{}, false })
	renderer := Renderer(&recordingRenderer{})
	foreground := ForegroundCoordinator(&recordingForegroundCoordinator{})
	stateStore := attentionStatePersistence(&fakeAttentionStateStore{})
	generation := observation.Generation(func(string, string) (uint64, bool) { return 1, true })
	tests := []struct {
		name    string
		options EngineOptions
	}{
		{name: "store", options: EngineOptions{Resolve: resolve, Renderer: renderer, Foreground: foreground, StateStore: stateStore, Generation: generation}},
		{name: "resolver", options: EngineOptions{Store: store, Renderer: renderer, Foreground: foreground, StateStore: stateStore, Generation: generation}},
		{name: "renderer", options: EngineOptions{Store: store, Resolve: resolve, Foreground: foreground, StateStore: stateStore, Generation: generation}},
		{name: "foreground", options: EngineOptions{Store: store, Resolve: resolve, Renderer: renderer, StateStore: stateStore, Generation: generation}},
		{name: "state store", options: EngineOptions{Store: store, Resolve: resolve, Renderer: renderer, Foreground: foreground, Generation: generation}},
		{name: "generation", options: EngineOptions{Store: store, Resolve: resolve, Renderer: renderer, Foreground: foreground, StateStore: stateStore}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewEngine(test.options); err == nil {
				t.Fatal("NewEngine accepted a missing required dependency")
			}
		})
	}
}

func TestEngineSelectsAttentionThenClearsAtExpiry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 20, 4, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	renderer := &recordingRenderer{}
	rules := map[string]attention.Rule{}
	resolve := func(record observation.Record) (attention.Rule, bool) {
		rule, ok := rules[record.ID()]
		return rule, ok
	}
	engine := newTestEngine(store, resolve, renderer)
	recorder, err := attention.NewRecorder(t.TempDir()+"/attention.jsonl", 16, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	setTestEngineRecorder(engine, recorder)
	rotation := engineObservation("rotation", protocol.DispositionNotable, protocol.ImpactNormal, now)
	alert := engineObservation("attention", protocol.DispositionActionable, protocol.ImpactNotable, now)
	rules[recordID(rotation)] = attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation, RotationIntervalMS: 10_000}
	rules[recordID(alert)] = attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention}
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, rotation); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, alert); err != nil {
		t.Fatal(err)
	}

	if err := engine.Step(context.Background(), now); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if got := renderer.lastID(); got != recordID(alert) {
		t.Fatalf("selected = %q, want attention", got)
	}
	trace, ok := recorder.Snapshot()
	if !ok || trace.SelectedID != recordID(alert) || trace.Outcome != attention.OutcomeDrawn {
		t.Fatalf("trace = %#v/%v", trace, ok)
	}
	if deadline, want := engine.NextDeadline(now), now.Add(8*time.Second); !deadline.Equal(want) {
		t.Fatalf("deadline = %v, want hold boundary %v", deadline, want)
	}

	store.WithdrawGeneration("plugin", 1)
	if err := engine.Step(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatalf("clear Step: %v", err)
	}
	if !renderer.cleared {
		t.Fatal("engine did not clear after all candidates were withdrawn")
	}
}

func TestEngineSelectedObservationMatchesTheExactRenderedRevision(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	value := engineObservation("request", protocol.DispositionActionable, protocol.ImpactCritical, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention}, true
	}, &recordingRenderer{})
	if err := engine.Step(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	selected, ok := engine.SelectedObservation()
	if !ok || selected.PluginID != "plugin" || selected.Generation != 1 || selected.Observation.Key != "request" || selected.Observation.Revision != 1 {
		t.Fatalf("selected observation = %#v/%v", selected, ok)
	}

	newer := value
	newer.Revision = 2
	newer.UpdatedAt = now.Add(time.Second)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, newer); err != nil {
		t.Fatal(err)
	}
	if selected, ok := engine.SelectedObservation(); ok {
		t.Fatalf("unrendered replacement appeared selected: %#v", selected)
	}
	if err := engine.Step(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	selected, ok = engine.SelectedObservation()
	if !ok || selected.Observation.Revision != 2 {
		t.Fatalf("selected replacement = %#v/%v", selected, ok)
	}
}

func TestEngineInvalidatedRenderedSelectionRedrawsSameRevision(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	renderer := &recordingRenderer{}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyInteractive, Foreground: true}, true
	}, renderer)
	value := engineObservation("menu", protocol.DispositionSnapshot, protocol.ImpactNormal, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if len(renderer.values) != 1 {
		t.Fatalf("initial renders = %d, want 1", len(renderer.values))
	}

	engine.InvalidateRenderedSelection()
	if _, ok := engine.SelectedObservation(); ok {
		t.Fatal("invalidated physical delivery remained selected")
	}
	if err := engine.Step(t.Context(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(renderer.values) != 2 || renderer.values[1] == nil || renderer.values[1].Revision != value.Revision {
		t.Fatalf("renders after invalidation = %#v", renderer.values)
	}
}

func TestEngineBackCooldownTombstonesExactRevisionAndAllowsCriticalBypass(t *testing.T) {
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	clock := base
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return clock })
	rules := map[string]attention.Rule{}
	resolve := func(record observation.Record) (attention.Rule, bool) {
		rule, ok := rules[record.ID()]
		return rule, ok
	}
	renderer := &recordingRenderer{}
	engine := newTestEngine(store, resolve, renderer)
	engine.now = func() time.Time { return clock }

	normal := engineObservation("normal", protocol.DispositionActionable, protocol.ImpactNotable, base)
	rules[recordID(normal)] = attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention}
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, normal); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	captured := engine.CaptureBackPresentation()
	if err := engine.DismissForBack(t.Context(), captured, "back_not_consumed"); err != nil {
		t.Fatal(err)
	}
	if len(renderer.values) != 2 || renderer.values[1] != nil {
		t.Fatalf("Back did not clear the presentation: %#v", renderer.values)
	}
	status := engine.PresentationCooldownStatus()
	if !status.Active || status.Reason != "back_not_consumed" || !status.Until.Equal(base.Add(30*time.Second)) || status.RemainingMS != 30_000 {
		t.Fatalf("cooldown status = %#v", status)
	}
	if deadline := engine.NextDeadline(base); !deadline.Equal(base.Add(30 * time.Second)) {
		t.Fatalf("cooldown deadline = %v", deadline)
	}

	clock = base.Add(5 * time.Second)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, normal); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	if len(renderer.values) != 2 {
		t.Fatalf("dismissed revision was replayed during cooldown: %#v", renderer.values)
	}

	newer := normal
	newer.Revision = 2
	newer.UpdatedAt = clock
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, newer); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	if len(renderer.values) != 2 {
		t.Fatalf("non-critical revision bypassed cooldown: %#v", renderer.values)
	}

	restartedRenderer := &recordingRenderer{}
	restarted := newTestEngine(store, resolve, restartedRenderer)
	if err := restarted.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	if restartedRenderer.lastID() != recordID(newer) {
		t.Fatalf("process-local cooldown survived engine restart: %#v", restartedRenderer.values)
	}

	critical := engineObservation("critical", protocol.DispositionActionable, protocol.ImpactCritical, base)
	critical.UpdatedAt = clock
	rules[recordID(critical)] = attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention}
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, critical); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	if renderer.lastID() != recordID(critical) {
		t.Fatalf("critical did not bypass cooldown: %#v", renderer.values)
	}

	clock = base.Add(10 * time.Second)
	if err := store.Withdraw("plugin", "app", "main", "critical"); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	if renderer.values[len(renderer.values)-1] != nil {
		t.Fatalf("non-critical candidate replaced critical during cooldown: %#v", renderer.values)
	}

	clock = base.Add(31 * time.Second)
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	if renderer.lastID() != recordID(newer) {
		t.Fatalf("fresh revision was not evaluated after cooldown: %#v", renderer.values)
	}
	if status := engine.PresentationCooldownStatus(); status.Active || status.Reason != "" || status.RemainingMS != 0 {
		t.Fatalf("expired cooldown status = %#v", status)
	}
}

func TestEngineLateBackFallbackPreservesNewerPresentation(t *testing.T) {
	base := time.Date(2026, 9, 4, 16, 0, 0, 0, time.UTC)
	clock := base
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return clock })
	renderer := &recordingRenderer{}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention}, true
	}, renderer)
	engine.now = func() time.Time { return clock }
	first := engineObservation("normal", protocol.DispositionActionable, protocol.ImpactNotable, base)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, first); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	captured := engine.CaptureBackPresentation()
	newer := first
	newer.Revision++
	newer.UpdatedAt = base.Add(time.Second)
	clock = newer.UpdatedAt
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, newer); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	if err := engine.DismissForBack(t.Context(), captured, "back_not_consumed"); err != nil {
		t.Fatal(err)
	}
	if len(renderer.values) != 2 || renderer.values[1] == nil || renderer.values[1].Revision != newer.Revision {
		t.Fatalf("late fallback cleared or replaced newer presentation: %#v", renderer.values)
	}
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	if len(renderer.values) != 2 {
		t.Fatalf("cooldown reevaluation cleared preserved presentation: %#v", renderer.values)
	}
}

func TestEngineLateBackFallbackPreservesCriticalOwner(t *testing.T) {
	base := time.Date(2026, 9, 4, 17, 0, 0, 0, time.UTC)
	clock := base
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return clock })
	renderer := &recordingRenderer{}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention}, true
	}, renderer)
	engine.now = func() time.Time { return clock }
	normal := engineObservation("normal", protocol.DispositionActionable, protocol.ImpactNotable, base)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, normal); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	captured := engine.CaptureBackPresentation()
	critical := engineObservation("critical", protocol.DispositionActionable, protocol.ImpactCritical, base)
	clock = base.Add(time.Second)
	critical.UpdatedAt = clock
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, critical); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	if err := engine.DismissForBack(t.Context(), captured, "back_session_input_failed"); err != nil {
		t.Fatal(err)
	}
	if len(renderer.values) != 2 || renderer.lastID() != recordID(critical) || !engine.criticalOwned {
		t.Fatalf("late fallback displaced critical ownership: values=%#v critical=%t", renderer.values, engine.criticalOwned)
	}
}

func TestEngineBackTombstoneSurvivesProducerWithdrawal(t *testing.T) {
	base := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	clock := base
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return clock })
	renderer := &recordingRenderer{}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention}, true
	}, renderer)
	engine.now = func() time.Time { return clock }
	value := engineObservation("normal", protocol.DispositionActionable, protocol.ImpactNotable, base)
	source := observation.Source{PluginID: "plugin", Generation: 1}
	if err := store.Publish(source, value); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	captured := engine.CaptureBackPresentation()
	if err := store.Withdraw("plugin", value.Instance.ID, value.Channel, value.Key); err != nil {
		t.Fatal(err)
	}
	if err := engine.DismissForBack(t.Context(), captured, "back_session_input_failed"); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(source, value); err != nil {
		t.Fatal(err)
	}
	clock = base.Add(31 * time.Second)
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	if len(renderer.values) != 2 {
		t.Fatalf("same tuple replayed after withdrawal and cooldown: %#v", renderer.values)
	}
	newer := value
	newer.Revision++
	newer.UpdatedAt = clock
	if err := store.Publish(source, newer); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), clock); err != nil {
		t.Fatal(err)
	}
	if renderer.lastID() != recordID(newer) || renderer.values[len(renderer.values)-1].Revision != newer.Revision {
		t.Fatalf("new revision remained excluded: %#v", renderer.values)
	}
}

func TestEngineTreatsFirmwareSuppressionAsTerminalWithoutPresentationCredit(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	value := engineObservation("suppressed", protocol.DispositionNotable, protocol.ImpactNormal, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	renderer := &recordingRenderer{outcome: attention.OutcomeFirmwareSuppressed}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation}, true
	}, renderer)

	if err := engine.Step(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if len(renderer.values) != 1 {
		t.Fatalf("renders = %d, want one terminal attempt", len(renderer.values))
	}
	if got := store.Snapshot(); len(got) != 0 {
		t.Fatalf("suppressed revision remained eligible: %#v", got)
	}
	if engine.history.CurrentID != "" || engine.lastGeneration != 0 || engine.lastRevision != 0 {
		t.Fatalf("suppression was recorded as delivered: history=%#v generation=%d revision=%d", engine.history, engine.lastGeneration, engine.lastRevision)
	}
	if _, shown := engine.history.LastShown[recordID(value)]; shown {
		t.Fatal("suppression received LastShown credit")
	}
}

func TestEngineGatewayStoreAndRecorderTreatFirmwareConflictAsTerminalForExactRevision(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 30, 0, 0, time.UTC)
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	display := &firmwareConflictDisplay{conflictAt: 2}
	gateway, err := device.NewGateway(display, assets.NewReconciler(nil))
	if err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation}, true
	}, gateway)
	recorder, err := attention.NewRecorder(t.TempDir()+"/attention.jsonl", 16, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	setTestEngineRecorder(engine, recorder)

	first := engineObservation("firmware-conflict", protocol.DispositionNotable, protocol.ImpactNormal, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, first); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	shownAt := engine.history.LastShown[recordID(first)]
	if shownAt.IsZero() {
		t.Fatal("first revision did not receive presentation credit")
	}

	suppressed := first
	suppressed.Revision = 2
	suppressed.UpdatedAt = now.Add(time.Second)
	suppressed.Scene.Elements[0].Text.Value = "firmware conflict"
	suppressed.Audio = &protocol.AudioCue{
		ID: "not-played", Asset: protocol.AssetRef{StockName: "calendar_event_starts.snd"}, ExpiresAt: now.Add(30 * time.Second),
	}
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, suppressed); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), suppressed.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if display.draws != 2 || display.clears != 0 || display.audio != 0 {
		t.Fatalf("suppressed I/O = draws %d clears %d audio %d, want 2/0/0", display.draws, display.clears, display.audio)
	}
	if got := store.Snapshot(); len(got) != 0 {
		t.Fatalf("suppressed revision remained eligible: %#v", got)
	}
	if engine.history.CurrentID != "" || engine.lastGeneration != 0 || engine.lastRevision != 0 {
		t.Fatalf("suppressed revision became current: history=%#v generation=%d revision=%d", engine.history, engine.lastGeneration, engine.lastRevision)
	}
	if got := engine.history.LastShown[recordID(first)]; !got.Equal(shownAt) {
		t.Fatalf("suppression changed prior LastShown credit: got %v want %v", got, shownAt)
	}
	trace, ok := recorder.Snapshot()
	if !ok || trace.SelectedID != recordID(suppressed) || trace.Outcome != attention.OutcomeFirmwareSuppressed {
		t.Fatalf("suppression trace = %#v/%v", trace, ok)
	}
	if err := engine.Step(t.Context(), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if display.draws != 2 {
		t.Fatalf("suppressed revision retried without a new publication: draws=%d", display.draws)
	}

	newer := suppressed
	newer.Revision = 3
	newer.UpdatedAt = now.Add(3 * time.Second)
	newer.Scene.Elements[0].Text.Value = "accepted revision"
	newer.Audio = nil
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, newer); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), newer.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if display.draws != 3 || engine.lastRevision != 3 || engine.history.CurrentID != recordID(newer) {
		t.Fatalf("newer revision was not delivered: draws=%d history=%#v revision=%d", display.draws, engine.history, engine.lastRevision)
	}
}

func TestEngineRunDoesNotReevaluateFromBufferedNotificationAfterFirmwareSuppression(t *testing.T) {
	now := time.Now().UTC()
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, time.Now)
	fallback := engineObservation("fallback", protocol.DispositionNotable, protocol.ImpactNormal, now)
	primary := engineObservation("primary", protocol.DispositionActionable, protocol.ImpactCritical, now)
	rules := map[string]attention.Rule{
		recordID(fallback): {Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation},
		recordID(primary):  {Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention},
	}
	renderer := &runRecordingRenderer{
		outcomes: []attention.DeliveryOutcome{attention.OutcomeFirmwareSuppressed, attention.OutcomeDrawn},
		calls:    make(chan string, 4),
	}
	engine := newTestEngine(store, func(record observation.Record) (attention.Rule, bool) {
		rule, ok := rules[record.ID()]
		return rule, ok
	}, renderer)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, fallback); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, primary); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	runDone := make(chan error, 1)
	go func() { runDone <- engine.Run(ctx) }()
	if got := awaitEngineRender(t, renderer.calls); got != recordID(primary) {
		t.Fatalf("first render = %q, want primary", got)
	}
	select {
	case got := <-renderer.calls:
		t.Fatalf("buffered publication notification caused fallback render %q", got)
	case <-time.After(150 * time.Millisecond):
	}
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, primary); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-renderer.calls:
		t.Fatalf("idempotent republication revived suppressed revision as %q", got)
	case <-time.After(100 * time.Millisecond):
	}
	fallback.Revision++
	fallback.UpdatedAt = time.Now().UTC()
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, fallback); err != nil {
		t.Fatal(err)
	}
	if got := awaitEngineRender(t, renderer.calls); got != recordID(fallback) {
		t.Fatalf("render after new publication = %q, want fallback", got)
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestEngineRunObservesMutationAcceptedWhilePreviousStepIsRendering(t *testing.T) {
	now := time.Now().UTC()
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, time.Now)
	value := engineObservation("boundary", protocol.DispositionActionable, protocol.ImpactNormal, now)
	renderer := &boundaryRenderer{calls: make(chan uint64, 2), releaseFirst: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-renderer.releaseFirst:
		default:
			close(renderer.releaseFirst)
		}
	})
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention}, true
	}, renderer)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	runDone := make(chan error, 1)
	go func() { runDone <- engine.Run(ctx) }()
	if got := awaitEngineRevision(t, renderer.calls); got != 1 {
		t.Fatalf("first render revision = %d, want 1", got)
	}
	value.Revision = 2
	value.UpdatedAt = time.Now().UTC()
	value.Scene.Elements[0].Text.Value = "new revision"
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	close(renderer.releaseFirst)
	if got := awaitEngineRevision(t, renderer.calls); got != 2 {
		t.Fatalf("render after boundary publication = %d, want 2", got)
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestEngineRunObservesLocalWakeEvenWhenStoreNotificationIsAlreadyBuffered(t *testing.T) {
	now := time.Now().UTC()
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, time.Now)
	value := engineObservation("local-wake", protocol.DispositionActionable, protocol.ImpactNormal, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	var enabled atomic.Bool
	resolved := make(chan struct{}, 1)
	renderer := &runRecordingRenderer{calls: make(chan string, 2)}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		current := enabled.Load()
		select {
		case resolved <- struct{}{}:
		default:
		}
		return attention.Rule{Enabled: current, AssetsReady: true, Policy: presentation.PolicyAttention}, true
	}, renderer)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	runDone := make(chan error, 1)
	go func() { runDone <- engine.Run(ctx) }()
	select {
	case <-resolved:
	case <-time.After(time.Second):
		t.Fatal("initial evaluation did not resolve observation")
	}
	enabled.Store(true)
	engine.Wake()
	if got := awaitEngineRender(t, renderer.calls); got != recordID(value) {
		t.Fatalf("render after local wake = %q, want %q", got, recordID(value))
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestEngineRendersSameIdentityAndRevisionForNewGeneration(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	activeGeneration := uint64(1)
	store := observation.NewStore(func(_, _ string) (uint64, bool) {
		return activeGeneration, true
	}, func() time.Time { return now })
	renderer := &recordingRenderer{}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention}, true
	}, renderer)

	first := engineObservation("same", protocol.DispositionActionable, protocol.ImpactNotable, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, first); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), now); err != nil {
		t.Fatal(err)
	}

	store.WithdrawGeneration("plugin", 1)
	activeGeneration = 2
	second := first
	second.Instance.Generation = 2
	second.ObservedAt = now.Add(time.Second)
	second.UpdatedAt = second.ObservedAt
	second.Scene.Elements[0].Text.Value = "new content-derived asset generation"
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 2}, second); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := len(renderer.values); got != 2 {
		t.Fatalf("render count = %d, want 2 for the new generation", got)
	}
	if got := renderer.values[1].Generation; got != 2 {
		t.Fatalf("rendered generation = %d, want 2", got)
	}
}

func TestEngineFallsBackImmediatelyAfterDeterministicPresentationFailure(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	invalid := engineObservation("invalid", protocol.DispositionActionable, protocol.ImpactCritical, now)
	fallback := engineObservation("fallback", protocol.DispositionNotable, protocol.ImpactNormal, now)
	for _, value := range []protocol.Observation{invalid, fallback} {
		if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
			t.Fatal(err)
		}
	}
	renderer := &invalidPresentationRenderer{}
	engine := newTestEngine(store, func(record observation.Record) (attention.Rule, bool) {
		if record.Observation.Key == "invalid" {
			return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention}, true
		}
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation, RotationIntervalMS: 10_000}, true
	}, renderer)
	if err := engine.Step(t.Context(), now); err != nil {
		t.Fatalf("Step returned deterministic presentation failure: %v", err)
	}
	if got, want := renderer.attempted, []string{recordID(invalid), recordID(fallback)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("render attempts = %#v, want %#v", got, want)
	}
}

func TestEngineRunRetriesRendererFailureWithoutExiting(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, time.Now)
	value := engineObservation("rotation", protocol.DispositionNotable, protocol.ImpactNormal, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	renderer := &flakyRenderer{attempts: make(chan int, 2)}
	engine := newTestEngineWithRetry(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation, RotationIntervalMS: 10_000}, true
	}, renderer, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	for wanted := 1; wanted <= 2; wanted++ {
		select {
		case got := <-renderer.attempts:
			if got != wanted {
				t.Fatalf("attempt = %d, want %d", got, wanted)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for render attempt %d", wanted)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestEngineAcknowledgementIsCoreOwnedAndRemovesAttention(t *testing.T) {
	now := time.Date(2026, 8, 20, 7, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	value := engineObservation("alert", protocol.DispositionActionable, protocol.ImpactNotable, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	renderer := &recordingRenderer{}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention, RequiresAck: true}, true
	}, renderer)
	if err := engine.Step(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := engine.Acknowledge(recordID(value)); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if !renderer.cleared {
		t.Fatal("acknowledged attention was not cleared")
	}
}

func TestEngineAcknowledgementReturnsTypedRejections(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	value := engineObservation("notice", protocol.DispositionNotable, protocol.ImpactNormal, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyWhenRelevant}, true
	}, &recordingRenderer{})
	if err := engine.Acknowledge(recordID(value)); !errors.Is(err, ErrObservationNotAcknowledgeable) {
		t.Fatalf("non-acknowledgeable error = %v", err)
	}
	if err := engine.Acknowledge("plugin/app/channel/missing"); !errors.Is(err, ErrObservationNotFound) {
		t.Fatalf("missing error = %v", err)
	}
}

type fakeAttentionStateStore struct {
	document attention.StateDocument
	loadErr  error
	saveErr  error
	outcome  localstate.CommitOutcome
	status   attention.StateStoreStatus
	saves    int
}

func (s *fakeAttentionStateStore) Load() (attention.StateDocument, error) {
	if s.loadErr != nil {
		s.status = attention.StateStoreStatus{Phase: "degraded", LastErrorCode: "attention_state_corrupt", Failures: 1}
		return attention.StateDocument{}, s.loadErr
	}
	s.status.Phase = "loaded"
	return s.document, nil
}

func (s *fakeAttentionStateStore) Save(document attention.StateDocument) (localstate.CommitOutcome, error) {
	s.saves++
	if s.saveErr != nil {
		code := "attention_state_write_failed"
		if s.outcome == localstate.CommittedDurabilityUncertain {
			code = "attention_state_durability_uncertain"
		}
		s.status = attention.StateStoreStatus{Phase: "degraded", LastErrorCode: code, Failures: 1}
		return s.outcome, s.saveErr
	}
	s.document = document
	s.status.Phase = "loaded"
	if s.outcome == "" {
		return localstate.Committed, nil
	}
	return s.outcome, nil
}

func (s *fakeAttentionStateStore) Status() attention.StateStoreStatus { return s.status }

func TestEngineRestoresOnlyCurrentUnexpiredAttentionState(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	identity := func(instance string, generation uint64) attention.StateIdentity {
		return attention.StateIdentity{PluginID: "plugin", InstanceID: instance, Generation: generation, Channel: "main", Key: "alert"}
	}
	stateStore := &fakeAttentionStateStore{document: attention.StateDocument{
		Version: attention.StateVersion,
		Acknowledgements: []attention.AcknowledgementState{
			{Identity: identity("current", 1), ObservedAt: now.Add(-time.Hour), TouchedAt: now.Add(-time.Minute)},
			{Identity: identity("changed", 1), ObservedAt: now.Add(-time.Hour), TouchedAt: now.Add(-time.Minute)},
		},
		LastShown: []attention.LastShownState{
			{Identity: identity("current", 1), ShownAt: now.Add(-time.Hour)},
			{Identity: identity("removed", 1), ShownAt: now.Add(-time.Hour)},
			{Identity: identity("expired", 1), ShownAt: now.Add(-48 * time.Hour)},
		},
	}}
	store := observation.NewStore(nil, func() time.Time { return now })
	engine := newTestEngine(store, nil, &recordingRenderer{})
	engine.now = func() time.Time { return now }
	engine.restoreAttentionState(stateStore, func(pluginID, instanceID string) (uint64, bool) {
		if pluginID == "plugin" && (instanceID == "current" || instanceID == "expired") {
			return 1, true
		}
		if pluginID == "plugin" && instanceID == "changed" {
			return 2, true
		}
		return 0, false
	})
	currentID := stateIdentityID(identity("current", 1))
	if len(engine.history.LastShown) != 1 || len(engine.acknowledged) != 1 || engine.history.CurrentID != "" {
		t.Fatalf("restored history/acks/current = %v/%v/%q", engine.history.LastShown, engine.acknowledged, engine.history.CurrentID)
	}
	if _, exists := engine.history.LastShown[currentID]; !exists {
		t.Fatal("current last-shown state was not restored")
	}
	diagnostics := engine.AttentionStateStatus()
	if diagnostics.RestoredEntries != 2 || diagnostics.DiscardedEntries != 2 || diagnostics.PrunedEntries != 1 || diagnostics.Phase != "loaded" {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if stateStore.saves != 1 {
		t.Fatalf("reconciled state saves = %d, want 1", stateStore.saves)
	}
}

func TestEngineRestoredAcknowledgementAndLastShownAffectFirstArbitration(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	identity := func(key string) attention.StateIdentity {
		return attention.StateIdentity{PluginID: "plugin", InstanceID: "app", Generation: 1, Channel: "main", Key: key}
	}
	acknowledged := engineObservation("acknowledged", protocol.DispositionActionable, protocol.ImpactNotable, now.Add(-time.Minute))
	acknowledged.ValidUntil = now.Add(time.Hour)
	cooling := engineObservation("cooling", protocol.DispositionNotable, protocol.ImpactNormal, now.Add(-time.Minute))
	cooling.ValidUntil = now.Add(time.Hour)
	store := observation.NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	for _, value := range []protocol.Observation{acknowledged, cooling} {
		if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
			t.Fatal(err)
		}
	}
	renderer := &recordingRenderer{}
	engine := newTestEngine(store, func(record observation.Record) (attention.Rule, bool) {
		if record.Observation.Key == acknowledged.Key {
			return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention, RequiresAck: true}, true
		}
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyWhenRelevant}, true
	}, renderer)
	engine.now = func() time.Time { return now }
	engine.restoreAttentionState(&fakeAttentionStateStore{document: attention.StateDocument{
		Version: attention.StateVersion,
		Acknowledgements: []attention.AcknowledgementState{{
			Identity: identity(acknowledged.Key), ObservedAt: acknowledged.ObservedAt, TouchedAt: now.Add(-time.Minute),
		}},
		LastShown: []attention.LastShownState{{Identity: identity(cooling.Key), ShownAt: now.Add(-time.Second)}},
	}}, func(string, string) (uint64, bool) { return 1, true })
	if err := engine.Step(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if len(renderer.values) != 0 {
		t.Fatalf("first arbitration redrew restored state: %v", renderer.values)
	}
}

func TestEngineDoesNotPersistInteractivePresentationState(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	value := engineObservation("interactive", protocol.DispositionSnapshot, protocol.ImpactNormal, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	stateStore := &fakeAttentionStateStore{}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyInteractive, Foreground: true}, true
	}, &recordingRenderer{})
	engine.restoreAttentionState(stateStore, func(string, string) (uint64, bool) { return 1, true })
	if err := engine.Step(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if len(engine.history.LastShown) != 1 {
		t.Fatal("interactive in-process history was not retained")
	}
	if len(stateStore.document.LastShown) != 0 || len(stateStore.document.Acknowledgements) != 0 {
		t.Fatalf("interactive state was persisted: %#v", stateStore.document)
	}
}

func TestEngineAcknowledgementRequiresConfirmedDurableCommit(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	value := engineObservation("alert", protocol.DispositionActionable, protocol.ImpactNotable, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	stateStore := &fakeAttentionStateStore{saveErr: errors.New("sync failed"), outcome: localstate.CommittedDurabilityUncertain}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention, RequiresAck: true}, true
	}, &recordingRenderer{})
	engine.now = func() time.Time { return now }
	engine.restoreAttentionState(stateStore, func(string, string) (uint64, bool) { return 1, true })
	if err := engine.Acknowledge(recordID(value)); err == nil {
		t.Fatal("durability-uncertain acknowledgement returned success")
	}
	if len(engine.acknowledged) != 0 {
		t.Fatalf("uncommitted acknowledgement entered memory: %v", engine.acknowledged)
	}
	if diagnostics := engine.AttentionStateStatus(); diagnostics.Phase != "degraded" || diagnostics.LastErrorCode != "attention_state_durability_uncertain" {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestEngineFailedAcknowledgementDoesNotEvictLastShown(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	value := engineObservation("alert", protocol.DispositionActionable, protocol.ImpactNotable, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	stateStore := &fakeAttentionStateStore{saveErr: errors.New("write failed"), outcome: localstate.NotCommitted}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention, RequiresAck: true}, true
	}, &recordingRenderer{})
	engine.now = func() time.Time { return now }
	engine.restoreAttentionState(stateStore, func(string, string) (uint64, bool) { return 1, true })
	engine.stateCapacity = 1
	lastShownID := "plugin/old/main/notice"
	lastShownIdentity := attention.StateIdentity{
		PluginID: "plugin", InstanceID: "old", Generation: 1, Channel: "main", Key: "notice",
	}
	engine.history.LastShown[lastShownID] = now.Add(-time.Minute)
	engine.lastShownState[lastShownID] = lastShownMetadata{identity: lastShownIdentity, persistent: true}

	if err := engine.Acknowledge(recordID(value)); err == nil {
		t.Fatal("failed attention-state write returned acknowledgement success")
	}
	if len(engine.acknowledged) != 0 {
		t.Fatalf("failed acknowledgement entered memory: %v", engine.acknowledged)
	}
	if shownAt, exists := engine.history.LastShown[lastShownID]; !exists || !shownAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("failed acknowledgement mutated last-shown state: %v", engine.history.LastShown)
	}
	if metadata, exists := engine.lastShownState[lastShownID]; !exists || metadata.identity != lastShownIdentity || !metadata.persistent {
		t.Fatalf("failed acknowledgement mutated last-shown metadata: %#v", engine.lastShownState)
	}
	if diagnostics := engine.AttentionStateStatus(); diagnostics.LastShownCapacityEvictions != 0 || diagnostics.AcknowledgementCapacityEvictions != 0 {
		t.Fatalf("failed acknowledgement credited evictions: %#v", diagnostics)
	}
}

func TestEngineRunExpiresAcknowledgementWithoutExternalWake(t *testing.T) {
	now := time.Now().UTC()
	store := observation.NewStore(func(string, string) (uint64, bool) { return 1, true }, time.Now)
	value := engineObservation("alert", protocol.DispositionActionable, protocol.ImpactNotable, now)
	value.ValidUntil = now.Add(time.Hour)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	renderer := &runRecordingRenderer{calls: make(chan string, 1)}
	stateStore := &fakeAttentionStateStore{}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention, RequiresAck: true}, true
	}, renderer)
	engine.stateTTL = 50 * time.Millisecond
	engine.restoreAttentionState(stateStore, func(string, string) (uint64, bool) { return 1, true })
	if err := engine.Acknowledge(recordID(value)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	if got := awaitEngineRender(t, renderer.calls); got != recordID(value) {
		t.Fatalf("render after acknowledgement expiry = %q", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(stateStore.document.Acknowledgements) != 0 {
		t.Fatalf("expired acknowledgement remained durable: %#v", stateStore.document.Acknowledgements)
	}
}

func TestEngineKeepsLastShownInMemoryWhenPersistenceFails(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	value := engineObservation("notice", protocol.DispositionNotable, protocol.ImpactNormal, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	renderer := &recordingRenderer{}
	stateStore := &fakeAttentionStateStore{saveErr: errors.New("write failed"), outcome: localstate.NotCommitted}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyWhenRelevant}, true
	}, renderer)
	engine.now = func() time.Time { return now }
	engine.restoreAttentionState(stateStore, func(string, string) (uint64, bool) { return 1, true })
	if err := engine.Step(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(t.Context(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(renderer.values) != 1 || len(engine.history.LastShown) != 1 {
		t.Fatalf("renders/history = %d/%v", len(renderer.values), engine.history.LastShown)
	}
	if diagnostics := engine.AttentionStateStatus(); diagnostics.Phase != "degraded" || diagnostics.LastErrorCode != "attention_state_write_failed" {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestEngineBoundsLastShownAndAcknowledgementStateUnderIdentityChurn(t *testing.T) {
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	clock := now
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return clock })
	engine := newTestEngine(store, func(record observation.Record) (attention.Rule, bool) {
		return attention.Rule{
			Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation,
			RotationIntervalMS: 1, RequiresAck: record.Observation.Disposition == protocol.DispositionActionable,
		}, true
	}, &recordingRenderer{})
	engine.stateCapacity = 4
	engine.stateTTL = 24 * time.Hour
	engine.now = func() time.Time { return clock }

	for index := 0; index < 12; index++ {
		clock = now.Add(time.Duration(index) * time.Second)
		value := engineObservation(fmt.Sprintf("shown-%02d", index), protocol.DispositionNotable, protocol.ImpactNormal, clock)
		value.ValidUntil = clock.Add(time.Hour)
		if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
			t.Fatal(err)
		}
		if err := engine.Step(context.Background(), clock); err != nil {
			t.Fatal(err)
		}
		store.WithdrawGeneration("plugin", 1)
		if err := engine.Step(context.Background(), clock.Add(time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}

	for index := 0; index < 12; index++ {
		clock = now.Add(time.Minute + time.Duration(index)*time.Second)
		value := engineObservation(fmt.Sprintf("ack-%02d", index), protocol.DispositionActionable, protocol.ImpactNotable, clock)
		value.ValidUntil = clock.Add(time.Hour)
		if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
			t.Fatal(err)
		}
		if err := engine.Acknowledge(recordID(value)); err != nil {
			t.Fatal(err)
		}
		store.WithdrawGeneration("plugin", 1)
	}

	diagnostics := engine.AttentionStateStatus()
	if diagnostics.LastShownEntries > engine.stateCapacity || diagnostics.AcknowledgementEntries > engine.stateCapacity {
		t.Fatalf("state exceeded bound: %#v", diagnostics)
	}
	if diagnostics.LastShownCapacityEvictions == 0 || diagnostics.AcknowledgementCapacityEvictions == 0 {
		t.Fatalf("identity churn was not observable: %#v", diagnostics)
	}
}

func TestEngineStatePruningPreservesLiveAndRecentSemantics(t *testing.T) {
	now := time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC)
	clock := now
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return clock })
	renderer := &recordingRenderer{}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation, RotationIntervalMS: int((10 * time.Hour) / time.Millisecond)}, true
	}, renderer)
	engine.stateCapacity = 4
	engine.stateTTL = time.Hour
	engine.now = func() time.Time { return clock }

	value := engineObservation("rotation", protocol.DispositionNotable, protocol.ImpactNormal, now)
	value.ValidUntil = now.Add(24 * time.Hour)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	store.WithdrawGeneration("plugin", 1)
	if err := engine.Step(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	clock = now.Add(30 * time.Minute)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(context.Background(), clock); err != nil {
		t.Fatal(err)
	}
	if got := len(renderer.values); got != 2 {
		t.Fatalf("recent identity ignored preserved rotation schedule: %d renderer writes", got)
	}
	store.WithdrawGeneration("plugin", 1)
	clock = now.Add(2 * time.Hour)
	if err := engine.Step(context.Background(), clock); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(context.Background(), clock); err != nil {
		t.Fatal(err)
	}
	if got := renderer.lastID(); got != recordID(value) {
		t.Fatalf("expired history still suppressed identity: last render %q", got)
	}
	if diagnostics := engine.AttentionStateStatus(); diagnostics.LastShownTTLPruned == 0 {
		t.Fatalf("TTL pruning was not observable: %#v", diagnostics)
	}
}

func TestEngineAcknowledgementPruningExpiresLiveStateAndDropsSupersededState(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	clock := now
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return clock })
	renderer := &recordingRenderer{}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention, RequiresAck: true}, true
	}, renderer)
	engine.stateTTL = time.Hour
	engine.now = func() time.Time { return clock }

	value := engineObservation("alert", protocol.DispositionActionable, protocol.ImpactNotable, now)
	value.ValidUntil = now.Add(24 * time.Hour)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	if err := engine.Acknowledge(recordID(value)); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Hour)
	if err := engine.Step(context.Background(), clock); err != nil {
		t.Fatal(err)
	}
	if diagnostics := engine.AttentionStateStatus(); diagnostics.AcknowledgementEntries != 0 || diagnostics.AcknowledgementTTLPruned != 1 {
		t.Fatalf("live acknowledgement exceeded the hard retention window: %#v", diagnostics)
	}
	if err := engine.Acknowledge(recordID(value)); err != nil {
		t.Fatal(err)
	}

	replacement := value
	replacement.Revision = 2
	replacement.ObservedAt = clock
	replacement.UpdatedAt = clock
	replacement.ValidUntil = clock.Add(time.Hour)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, replacement); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(context.Background(), clock); err != nil {
		t.Fatal(err)
	}
	if got := renderer.lastID(); got != recordID(replacement) {
		t.Fatalf("superseded acknowledgement hid replacement: %q", got)
	}
	diagnostics := engine.AttentionStateStatus()
	if diagnostics.AcknowledgementEntries != 0 || diagnostics.SupersededAcknowledgementsPruned != 1 {
		t.Fatalf("superseded acknowledgement diagnostics = %#v", diagnostics)
	}
}

func TestEngineRemoveInstanceDropsOnlyDeletedIdentityRuntimeState(t *testing.T) {
	now := time.Date(2026, 8, 22, 13, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention, RequiresAck: true}, true
	}, &recordingRenderer{})
	deleted := engineObservation("deleted", protocol.DispositionActionable, protocol.ImpactNotable, now)
	deleted.Instance.ID = "deleted"
	retained := engineObservation("retained", protocol.DispositionActionable, protocol.ImpactNotable, now)
	retained.Instance.ID = "retained"
	for _, value := range []protocol.Observation{deleted, retained} {
		if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
			t.Fatal(err)
		}
		if err := engine.Acknowledge(recordID(value)); err != nil {
			t.Fatal(err)
		}
		engine.history.LastShown[recordID(value)] = now
	}
	engine.acknowledged["plugin/deleted/older"] = ackState{generation: 0, observedAt: now, touchedAt: now}

	engine.RemoveInstance("plugin", "deleted", 1)
	if _, exists := engine.history.LastShown[recordID(deleted)]; exists {
		t.Fatal("deleted instance retained last-shown state")
	}
	if _, exists := engine.acknowledged[recordID(deleted)]; exists {
		t.Fatal("deleted instance retained acknowledgement state")
	}
	if _, exists := engine.acknowledged["plugin/deleted/older"]; exists {
		t.Fatal("deleted instance retained acknowledgement from an older generation")
	}
	if _, exists := engine.history.LastShown[recordID(retained)]; !exists {
		t.Fatal("unrelated instance lost last-shown state")
	}
	if _, exists := engine.acknowledged[recordID(retained)]; !exists {
		t.Fatal("unrelated instance lost acknowledgement state")
	}
}

func TestEngineRemoveInstancePreservesNewerGenerationLastShown(t *testing.T) {
	now := time.Date(2026, time.September, 4, 13, 0, 0, 0, time.UTC)
	engine := newTestEngine(observation.NewStore(nil, time.Now), nil, &recordingRenderer{})
	oldIdentity := attention.StateIdentity{PluginID: "plugin", InstanceID: "app", Generation: 1, Channel: "main", Key: "old"}
	newIdentity := attention.StateIdentity{PluginID: "plugin", InstanceID: "app", Generation: 2, Channel: "main", Key: "new"}
	oldID := stateIdentityID(oldIdentity)
	newID := stateIdentityID(newIdentity)
	engine.history.LastShown[oldID] = now.Add(-time.Minute)
	engine.lastShownState[oldID] = lastShownMetadata{identity: oldIdentity, persistent: true}
	engine.history.LastShown[newID] = now
	engine.lastShownState[newID] = lastShownMetadata{identity: newIdentity, persistent: true}

	engine.RemoveInstance("plugin", "app", 1)
	if _, exists := engine.history.LastShown[oldID]; exists {
		t.Fatal("retired generation retained last-shown state")
	}
	if _, exists := engine.history.LastShown[newID]; !exists {
		t.Fatal("late retirement removed newer-generation last-shown state")
	}
}

func TestEngineRecorderFailureDegradesDiagnosticsWithoutBecomingRenderFailure(t *testing.T) {
	now := time.Now().UTC()
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) { return attention.Rule{}, false }, &recordingRenderer{})
	recorder, err := attention.NewRecorder(t.TempDir()+"/attention.jsonl", 4, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	setTestEngineRecorder(engine, recorder)

	if err := engine.Step(context.Background(), now); err != nil {
		t.Fatalf("recorder failure escaped as Step/render failure: %v", err)
	}
	status := engine.RecorderStatus()
	if status.Phase != attention.RecorderDegraded || status.LastErrorCode != "closed" || status.LastErrorAt.IsZero() {
		t.Fatalf("status = %#v", status)
	}
	if _, ok := engine.AttentionSnapshot(); ok {
		t.Fatal("failed decision appeared in history")
	}
}

func TestEngineRecorderStatusIsSafeWhileRunRecords(t *testing.T) {
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, time.Now)
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) { return attention.Rule{}, false }, &recordingRenderer{})
	recorder, err := attention.NewRecorder(t.TempDir()+"/attention.jsonl", 8, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	setTestEngineRecorder(engine, recorder)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	for index := 0; index < 100; index++ {
		_ = engine.RecorderStatus()
		engine.Wake()
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEngineSerializesRunStepNextDeadlineAndRender(t *testing.T) {
	now := time.Now().UTC()
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, time.Now)
	value := engineObservation("rotation", protocol.DispositionNotable, protocol.ImpactNormal, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	renderer := &gatedRenderer{entered: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation, RotationIntervalMS: 10_000}, true
	}, renderer)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- engine.Run(ctx) }()
	select {
	case <-renderer.entered:
	case <-time.After(time.Second):
		t.Fatal("Run did not enter renderer")
	}
	stepDone := make(chan error, 1)
	go func() { stepDone <- engine.Step(context.Background(), now.Add(time.Second)) }()
	deadlineDone := make(chan struct{})
	go func() { _ = engine.NextDeadline(now); close(deadlineDone) }()
	select {
	case <-renderer.entered:
		t.Fatal("concurrent Step entered renderer")
	case <-deadlineDone:
		t.Fatal("NextDeadline observed mutable history during render")
	case <-time.After(30 * time.Millisecond):
	}
	renderer.release <- struct{}{}
	if err := <-stepDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-deadlineDone:
	case <-time.After(time.Second):
		t.Fatal("NextDeadline remained blocked")
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if got := renderer.maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent renders = %d", got)
	}
}

func TestEngineRotationDeadlineIsEarliestCalculatedDueTime(t *testing.T) {
	now := time.Date(2026, 8, 21, 7, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	first := engineObservation("first", protocol.DispositionNotable, protocol.ImpactNormal, now)
	second := engineObservation("second", protocol.DispositionNotable, protocol.ImpactNormal, now)
	for _, value := range []protocol.Observation{first, second} {
		value.ValidUntil = now.Add(time.Minute)
		if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
			t.Fatal(err)
		}
	}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation, RotationIntervalMS: 10_000}, true
	}, &recordingRenderer{})
	engine.history.LastShown[recordID(first)] = now
	engine.history.LastShown[recordID(second)] = now.Add(3 * time.Second)
	if got, want := engine.NextDeadline(now.Add(time.Second)), now.Add(10*time.Second); !got.Equal(want) {
		t.Fatalf("deadline = %v, want %v", got, want)
	}
}

func TestEngineDoesNotClearCurrentRotationBeforeItsNextDueTime(t *testing.T) {
	now := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	value := engineObservation("rotation", protocol.DispositionNotable, protocol.ImpactNormal, now)
	if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	renderer := &recordingRenderer{}
	engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyRotation, RotationIntervalMS: 10_000}, true
	}, renderer)
	if err := engine.Step(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if renderer.cleared || len(renderer.values) != 1 {
		t.Fatalf("rotation caused writes before next due: cleared=%v renders=%d", renderer.cleared, len(renderer.values))
	}
	if err := engine.Step(context.Background(), now.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	if !renderer.cleared || len(renderer.values) != 2 || renderer.values[1] != nil {
		t.Fatalf("rotation did not yield after its readable hold: cleared=%v values=%#v", renderer.cleared, renderer.values)
	}
}

type recordingRenderer struct {
	values  []*presentation.Candidate
	cleared bool
	outcome attention.DeliveryOutcome
	err     error
}

type recordingForegroundCoordinator struct {
	acquire      bool
	acquired     []string
	releaseCalls int
}

func (c *recordingForegroundCoordinator) AcquireCritical(_ context.Context, candidate presentation.Candidate) bool {
	c.acquired = append(c.acquired, candidate.ID())
	return c.acquire
}

func (c *recordingForegroundCoordinator) ReleaseCritical() { c.releaseCalls++ }

func TestEngineCoordinatesCriticalOwnershipAroundPhysicalDelivery(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	newEngine := func(renderer *recordingRenderer, coordinator *recordingForegroundCoordinator) (*Engine, *observation.Store, protocol.Observation) {
		store := observation.NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
		critical := engineObservation("critical", protocol.DispositionActionable, protocol.ImpactCritical, now)
		if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, critical); err != nil {
			t.Fatal(err)
		}
		engine := newTestEngine(store, func(observation.Record) (attention.Rule, bool) {
			return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyAttention}, true
		}, renderer)
		setTestForegroundCoordinator(engine, coordinator)
		return engine, store, critical
	}

	t.Run("grants ownership before draw and releases when critical leaves", func(t *testing.T) {
		renderer := &recordingRenderer{}
		coordinator := &recordingForegroundCoordinator{acquire: true}
		engine, store, critical := newEngine(renderer, coordinator)
		if err := engine.Step(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		if len(coordinator.acquired) != 1 || coordinator.acquired[0] != recordID(critical) || len(renderer.values) != 1 {
			t.Fatalf("acquired=%v renders=%d", coordinator.acquired, len(renderer.values))
		}
		store.WithdrawGeneration("plugin", 1)
		if err := engine.Step(t.Context(), now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if coordinator.releaseCalls == 0 || !renderer.cleared {
			t.Fatalf("release calls=%d cleared=%t", coordinator.releaseCalls, renderer.cleared)
		}
	})

	t.Run("atomic execution winning the race prevents draw", func(t *testing.T) {
		renderer := &recordingRenderer{}
		coordinator := &recordingForegroundCoordinator{}
		engine, _, _ := newEngine(renderer, coordinator)
		if err := engine.Step(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		if len(coordinator.acquired) != 1 || len(renderer.values) != 0 {
			t.Fatalf("acquired=%v renders=%d", coordinator.acquired, len(renderer.values))
		}
	})

	t.Run("firmware rejection releases critical ownership", func(t *testing.T) {
		renderer := &recordingRenderer{outcome: attention.OutcomeFirmwareSuppressed}
		coordinator := &recordingForegroundCoordinator{acquire: true}
		engine, _, _ := newEngine(renderer, coordinator)
		if err := engine.Step(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		if coordinator.releaseCalls != 1 {
			t.Fatalf("release calls=%d, want 1", coordinator.releaseCalls)
		}
	})

	t.Run("failed clear retains ownership until physical clear succeeds", func(t *testing.T) {
		renderer := &recordingRenderer{}
		coordinator := &recordingForegroundCoordinator{acquire: true}
		engine, store, _ := newEngine(renderer, coordinator)
		if err := engine.Step(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		store.WithdrawGeneration("plugin", 1)
		renderer.err = errors.New("clear unavailable")
		if err := engine.Step(t.Context(), now.Add(time.Second)); err == nil {
			t.Fatal("failed clear returned success")
		}
		if coordinator.releaseCalls != 0 || !engine.criticalOwned {
			t.Fatalf("failed clear released ownership: calls=%d owned=%t", coordinator.releaseCalls, engine.criticalOwned)
		}
		renderer.err = nil
		if err := engine.Step(t.Context(), now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		if coordinator.releaseCalls != 1 || engine.criticalOwned {
			t.Fatalf("successful retry ownership: calls=%d owned=%t", coordinator.releaseCalls, engine.criticalOwned)
		}
	})

	t.Run("failed noncritical replacement retains ownership", func(t *testing.T) {
		renderer := &recordingRenderer{}
		coordinator := &recordingForegroundCoordinator{acquire: true}
		engine, store, critical := newEngine(renderer, coordinator)
		if err := engine.Step(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		replacement := critical
		replacement.Revision++
		replacement.Impact = protocol.ImpactNormal
		replacement.UpdatedAt = now.Add(time.Second)
		if err := store.Publish(observation.Source{PluginID: "plugin", Generation: 1}, replacement); err != nil {
			t.Fatal(err)
		}
		renderer.err = errors.New("draw unavailable")
		if err := engine.Step(t.Context(), now.Add(time.Second)); err == nil {
			t.Fatal("failed replacement returned success")
		}
		if coordinator.releaseCalls != 0 || !engine.criticalOwned {
			t.Fatalf("failed replacement released ownership: calls=%d owned=%t", coordinator.releaseCalls, engine.criticalOwned)
		}
		renderer.err = nil
		if err := engine.Step(t.Context(), now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		if coordinator.releaseCalls != 1 || engine.criticalOwned {
			t.Fatalf("replacement retry ownership: calls=%d owned=%t", coordinator.releaseCalls, engine.criticalOwned)
		}
	})
}

func TestValidateRenderResult(t *testing.T) {
	candidate := &presentation.Candidate{}
	failure := errors.New("render failed")
	tests := []struct {
		name      string
		candidate *presentation.Candidate
		outcome   attention.DeliveryOutcome
		err       error
		valid     bool
	}{
		{name: "candidate drawn", candidate: candidate, outcome: attention.OutcomeDrawn, valid: true},
		{name: "candidate unchanged", candidate: candidate, outcome: attention.OutcomeUnchanged, valid: true},
		{name: "candidate suppressed", candidate: candidate, outcome: attention.OutcomeFirmwareSuppressed, valid: true},
		{name: "clear cleared", outcome: attention.OutcomeCleared, valid: true},
		{name: "clear unchanged", outcome: attention.OutcomeUnchanged, valid: true},
		{name: "device failure", candidate: candidate, outcome: attention.OutcomeDeviceUnavailable, err: failure, valid: true},
		{name: "asset failure", candidate: candidate, outcome: attention.OutcomeAssetMissing, err: failure, valid: true},
		{name: "invalid presentation", candidate: candidate, outcome: attention.OutcomeInvalidPresentation, err: failure, valid: true},
		{name: "empty success", candidate: candidate},
		{name: "failure without outcome", candidate: candidate, err: failure},
		{name: "candidate cleared", candidate: candidate, outcome: attention.OutcomeCleared},
		{name: "clear drawn", outcome: attention.OutcomeDrawn},
		{name: "failure outcome without error", candidate: candidate, outcome: attention.OutcomeDeviceUnavailable},
		{name: "successful outcome with error", candidate: candidate, outcome: attention.OutcomeDrawn, err: failure},
		{name: "unknown outcome", candidate: candidate, outcome: attention.DeliveryOutcome("unknown")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateRenderResult(test.candidate, test.outcome, test.err)
			if test.valid && err != nil {
				t.Fatalf("validateRenderResult = %v, want valid", err)
			}
			if !test.valid && !errors.Is(err, errInvalidRendererResult) {
				t.Fatalf("validateRenderResult = %v, want invalid renderer result", err)
			}
		})
	}
}

type runRecordingRenderer struct {
	mu       sync.Mutex
	outcomes []attention.DeliveryOutcome
	last     attention.DeliveryOutcome
	calls    chan string
}

type boundaryRenderer struct {
	calls        chan uint64
	releaseFirst chan struct{}
	once         sync.Once
}

func (r *boundaryRenderer) Render(_ context.Context, candidate *presentation.Candidate) (attention.DeliveryOutcome, error) {
	if candidate == nil {
		return attention.OutcomeCleared, nil
	}
	r.calls <- candidate.Revision
	r.once.Do(func() { <-r.releaseFirst })
	return attention.OutcomeDrawn, nil
}

func (r *runRecordingRenderer) Render(_ context.Context, candidate *presentation.Candidate) (attention.DeliveryOutcome, error) {
	r.mu.Lock()
	r.last = attention.OutcomeDrawn
	if len(r.outcomes) != 0 {
		r.last = r.outcomes[0]
		r.outcomes = r.outcomes[1:]
	}
	outcome := r.last
	r.mu.Unlock()
	id := ""
	if candidate != nil {
		id = candidate.ID()
	}
	r.calls <- id
	return outcome, nil
}

func awaitEngineRender(t *testing.T, calls <-chan string) string {
	t.Helper()
	select {
	case value := <-calls:
		return value
	case <-time.After(time.Second):
		t.Fatal("engine did not render")
		return ""
	}
}

func awaitEngineRevision(t *testing.T, calls <-chan uint64) uint64 {
	t.Helper()
	select {
	case value := <-calls:
		return value
	case <-time.After(time.Second):
		t.Fatal("engine did not render revision")
		return 0
	}
}

type firmwareConflictDisplay struct {
	conflictAt int
	draws      int
	clears     int
	audio      int
}

func (d *firmwareConflictDisplay) Draw(context.Context, busylib.DisplayElements) error {
	d.draws++
	if d.draws == d.conflictAt {
		return &busylib.APIError{StatusCode: http.StatusConflict}
	}
	return nil
}

func (d *firmwareConflictDisplay) Clear(context.Context, string) error {
	d.clears++
	return nil
}

func (d *firmwareConflictDisplay) PlayAudio(context.Context, busylib.PlayAudio) error {
	d.audio++
	return nil
}

type flakyRenderer struct {
	attempts chan int
	count    int
}

type invalidPresentationRenderer struct{ attempted []string }

func (r *invalidPresentationRenderer) Render(_ context.Context, candidate *presentation.Candidate) (attention.DeliveryOutcome, error) {
	if candidate == nil {
		return attention.OutcomeCleared, nil
	}
	r.attempted = append(r.attempted, candidate.ID())
	if candidate.Key == "invalid" {
		return attention.OutcomeInvalidPresentation, presentation.ErrInvalidPresentation
	}
	return attention.OutcomeDrawn, nil
}

type gatedRenderer struct {
	entered chan struct{}
	release chan struct{}
	active  atomic.Int32
	maximum atomic.Int32
}

func (r *gatedRenderer) Render(context.Context, *presentation.Candidate) (attention.DeliveryOutcome, error) {
	active := r.active.Add(1)
	for maximum := r.maximum.Load(); active > maximum && !r.maximum.CompareAndSwap(maximum, active); maximum = r.maximum.Load() {
	}
	r.entered <- struct{}{}
	<-r.release
	r.active.Add(-1)
	return attention.OutcomeDrawn, nil
}

func (r *flakyRenderer) Render(context.Context, *presentation.Candidate) (attention.DeliveryOutcome, error) {
	r.count++
	r.attempts <- r.count
	if r.count == 1 {
		return attention.OutcomeDeviceUnavailable, errors.New("device unavailable")
	}
	return attention.OutcomeDrawn, nil
}

func (r *recordingRenderer) Render(_ context.Context, candidate *presentation.Candidate) (attention.DeliveryOutcome, error) {
	if candidate == nil {
		r.cleared = true
		r.values = append(r.values, nil)
		if r.err != nil {
			return attention.OutcomeDeviceUnavailable, r.err
		}
		return attention.OutcomeCleared, nil
	}
	copy := *candidate
	r.values = append(r.values, &copy)
	if r.err != nil {
		return attention.OutcomeDeviceUnavailable, r.err
	}
	if r.outcome != "" {
		return r.outcome, nil
	}
	return attention.OutcomeDrawn, nil
}

func (r *recordingRenderer) lastID() string {
	if len(r.values) == 0 || r.values[len(r.values)-1] == nil {
		return ""
	}
	return r.values[len(r.values)-1].ID()
}

func engineObservation(key string, disposition protocol.Disposition, impact protocol.Impact, now time.Time) protocol.Observation {
	return protocol.Observation{
		Instance: protocol.InstanceRef{ID: "app", Generation: 1}, Channel: "main", Key: key, Revision: 1,
		Disposition: disposition, Impact: impact, ReasonCode: "test_state",
		ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(time.Minute),
		Scene: &presentation.Scene{Elements: []presentation.Element{{ID: "text", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: key, Font: "normal"}}}},
	}
}

func recordID(value protocol.Observation) string {
	return observation.Record{PluginID: "plugin", Generation: 1, Observation: value}.ID()
}
