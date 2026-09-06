package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
)

func setTestDesiredStateValidator(state *DesiredState, validate DesiredStateValidator) {
	state.mu.Lock()
	state.validator = validate
	state.mu.Unlock()
}

func TestReplaceAppConfigurationCommitsOneCompleteReplacementAndReconciles(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, SecretResolverFunc(func(context.Context, string) (string, error) { return "resolved", nil }), plugins)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	replacement := AppConfiguration{
		Config:       json.RawMessage(`{"question":"new"}`),
		Secrets:      map[string]string{"token": "keychain://bsbctl/ball8-token"},
		Policies:     map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive, DevicePriority: 42}},
		LaunchAction: "shake",
	}
	updated, outcome, err := service.ReplaceAppConfiguration(context.Background(), "ball8", replacement)
	if err != nil {
		t.Fatalf("ReplaceAppConfiguration: %v", err)
	}
	if outcome != localstate.Committed || updated.Generation != 2 {
		t.Fatalf("outcome/generation = %q/%d", outcome, updated.Generation)
	}
	app := updated.Apps["ball8"]
	if !app.Enabled || app.PluginID != "plugin" || string(app.Config) != string(replacement.Config) || app.LaunchAction != "shake" || !reflect.DeepEqual(app.Secrets, replacement.Secrets) || !reflect.DeepEqual(app.Policies, replacement.Policies) {
		t.Fatalf("replacement app = %#v", app)
	}
	if got := updated.Plugins["plugin"]; got.Version != "1" || got.Executable != "/plugin" {
		t.Fatalf("plugin package was changed: %#v", got)
	}
	if len(plugins.lastSpecs) != 1 || len(plugins.lastSpecs[0].Instances) != 1 || plugins.lastSpecs[0].Instances[0].Generation != 2 || string(plugins.lastSpecs[0].Instances[0].Config) != string(replacement.Config) {
		t.Fatalf("reconciled specs = %#v", plugins.lastSpecs)
	}
}

func TestReplaceAppConfigurationRejectsStaleExpectedGenerationBeforeReplacingNewerFields(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := newBlockingApplyController()
	service := newTestReconciler(t, store, SecretResolverFunc(func(context.Context, string) (string, error) { return "resolved", nil }), plugins)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(func() { close(plugins.oldRelease) })
	t.Cleanup(release)

	newer := AppConfiguration{
		ExpectedGeneration: 1,
		Config:             json.RawMessage(`{"question":"newer"}`),
		Policies:           map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyRotation, RotationIntervalMS: 30_000}},
		LaunchAction:       "newer-action",
	}
	newerDone := make(chan error, 1)
	go func() {
		_, _, err := service.ReplaceAppConfiguration(t.Context(), "ball8", newer)
		newerDone <- err
	}()
	awaitServiceSignal(t, plugins.oldStarted, "newer configuration reconciliation")

	stale := AppConfiguration{
		ExpectedGeneration: 1,
		Config:             json.RawMessage(`{"question":"stale"}`),
		Policies:           map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}},
		LaunchAction:       "stale-action",
	}
	_, outcome, err := service.ReplaceAppConfiguration(t.Context(), "ball8", stale)
	if !errors.Is(err, config.ErrConflict) || outcome != localstate.NotCommitted {
		t.Fatalf("stale replacement outcome/error = %q, %v", outcome, err)
	}
	persisted, loadErr := store.Load()
	app := persisted.Apps["ball8"]
	if loadErr != nil || persisted.Generation != 2 || string(app.Config) != string(newer.Config) || app.LaunchAction != newer.LaunchAction || !reflect.DeepEqual(app.Policies, newer.Policies) {
		t.Fatalf("newer configuration was overwritten: document=%#v error=%v", persisted, loadErr)
	}

	release()
	if err := <-newerDone; err != nil {
		t.Fatalf("newer replacement: %v", err)
	}
}

func TestReconcilerActivatorChangesOnlyVerifiedPluginPackage(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	original := serviceDocument(true)
	original.Apps["ball8"] = config.App{
		ID: "ball8", PluginID: "plugin", Enabled: true, LaunchAction: "ask",
		Config: json.RawMessage(`{"private":"preserved"}`), Secrets: map[string]string{"token": "keychain://bsbctl/token"},
		Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}},
	}
	if _, err := store.ReplaceWithOutcome(0, original); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, SecretResolverFunc(func(context.Context, string) (string, error) { return "resolved", nil }), plugins)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	desired, err := service.DesiredPlugin(context.Background(), "plugin")
	if err != nil || desired == nil || desired.Version != "1" {
		t.Fatalf("DesiredPlugin = %#v, %v", desired, err)
	}
	nextPlugin := *desired
	nextPlugin.Version = "2"
	nextPlugin.Executable = "/verified/plugin-v2"
	outcome, err := service.ActivatePlugin(context.Background(), nextPlugin)
	if err != nil || outcome != localstate.Committed {
		t.Fatalf("ActivatePlugin = %q, %v", outcome, err)
	}
	updated, loaded := service.Document()
	if !loaded || updated.Generation != 2 || updated.Plugins["plugin"].Version != "2" {
		t.Fatalf("updated document = %#v, loaded=%v", updated, loaded)
	}
	wantApp := original.Apps["ball8"]
	wantApp.Generation = updated.Generation
	gotApp := updated.Apps["ball8"]
	var gotConfig, wantConfig map[string]any
	if err := json.Unmarshal(gotApp.Config, &gotConfig); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantApp.Config, &wantConfig); err != nil {
		t.Fatal(err)
	}
	gotApp.Config, wantApp.Config = nil, nil
	if !reflect.DeepEqual(gotApp, wantApp) || !reflect.DeepEqual(gotConfig, wantConfig) {
		t.Fatalf("app configuration changed: got %#v want %#v", updated.Apps["ball8"], original.Apps["ball8"])
	}
	if len(plugins.lastSpecs) != 1 || plugins.lastSpecs[0].Version != "2" || plugins.lastSpecs[0].Executable != "/verified/plugin-v2" {
		t.Fatalf("runtime package not reconciled: %#v", plugins.lastSpecs)
	}
}

func TestConfigStoreActivatorSupportsRecoveryBeforeServiceConstruction(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	original := serviceDocument(false)
	if _, err := store.ReplaceWithOutcome(0, original); err != nil {
		t.Fatal(err)
	}
	activator := NewConfigStoreActivator(store)
	desired, err := activator.DesiredPlugin(context.Background(), "plugin")
	if err != nil || desired == nil {
		t.Fatalf("DesiredPlugin = %#v, %v", desired, err)
	}
	next := *desired
	next.Version = "2"
	next.Executable = "/verified/plugin-v2"
	outcome, err := activator.ActivatePlugin(context.Background(), next)
	if err != nil || outcome != localstate.Committed {
		t.Fatalf("ActivatePlugin = %q, %v", outcome, err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Generation != 2 || loaded.Plugins["plugin"].Version != "2" || loaded.Apps["ball8"].PluginID != original.Apps["ball8"].PluginID || loaded.Apps["ball8"].Enabled != original.Apps["ball8"].Enabled {
		t.Fatalf("recovered document = %#v", loaded)
	}
}

func TestReplaceAppConfigurationRejectsInvalidCompleteReplacementBeforePersistence(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	service := newTestReconciler(t, store, nil, &fakePluginController{})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, outcome, err := service.ReplaceAppConfiguration(context.Background(), "ball8", AppConfiguration{
		Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{"undeclared": {Policy: presentation.PolicyInteractive}},
	})
	if !errors.Is(err, ErrInvalidAppConfiguration) || outcome != localstate.NotCommitted {
		t.Fatalf("invalid replacement outcome/error = %q, %v", outcome, err)
	}
	document, loadErr := store.Load()
	if loadErr != nil || document.Generation != 1 || document.Apps["ball8"].Policies["answer"].Policy != presentation.PolicyInteractive {
		t.Fatalf("persisted document changed = %#v, %v", document, loadErr)
	}
}

func TestCreateAppInstanceCommitsValidatedDefinitionAndReconciles(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, nil, plugins)
	setTestDesiredStateValidator(service.desired, func(document config.Document) error {
		if len(document.Apps) == 1 {
			return nil
		}
		if len(document.Apps) != 2 {
			return errors.New("expected complete candidate document")
		}
		return nil
	})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	operation, err := service.CreateAppInstance(context.Background(), config.App{
		ID: "codex-secondary", PluginID: "plugin", Generation: 99, Enabled: true,
		Config:   json.RawMessage(`{"question":"secondary"}`),
		Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}},
	})
	if err != nil {
		t.Fatalf("CreateAppInstance: %v", err)
	}
	if !operation.Outcome.IsCommitted() || operation.Generation != 2 || operation.ReconciliationError != nil {
		t.Fatalf("operation = %#v", operation)
	}
	if got := operation.Apps["codex-secondary"]; got.ID != "codex-secondary" || got.PluginID != "plugin" || got.Generation != 2 || !got.Enabled {
		t.Fatalf("created app = %#v", got)
	}
	if len(plugins.lastSpecs) != 1 || len(plugins.lastSpecs[0].Instances) != 2 {
		t.Fatalf("reconciled specs = %#v", plugins.lastSpecs)
	}
}

func TestCreateAppInstanceRejectsDuplicateAndValidatorFailureBeforePersistence(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	service := newTestReconciler(t, store, nil, &fakePluginController{})
	setTestDesiredStateValidator(service.desired, func(document config.Document) error {
		if len(document.Apps) == 1 {
			return nil
		}
		return errors.New("plugin schema rejected")
	})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	definition := config.App{ID: "second", PluginID: "plugin", Enabled: true, Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}}}
	if _, err := service.CreateAppInstance(context.Background(), definition); !errors.Is(err, ErrInvalidAppConfiguration) {
		t.Fatalf("validator error = %v", err)
	}
	setTestDesiredStateValidator(service.desired, nil)
	definition.ID = "ball8"
	if _, err := service.CreateAppInstance(context.Background(), definition); !errors.Is(err, ErrAppAlreadyExists) {
		t.Fatalf("duplicate error = %v", err)
	}
	document, err := store.Load()
	if err != nil || document.Generation != 1 || len(document.Apps) != 1 {
		t.Fatalf("persisted document = %#v, %v", document, err)
	}
}

func TestExistingAppMutationsUseCompleteDesiredStateValidatorBeforePersistence(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(false)
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	service := newTestReconciler(t, store, nil, &fakePluginController{})
	setTestDesiredStateValidator(service.desired, func(document config.Document) error {
		if document.Generation == 1 {
			return nil
		}
		return errors.New("cross-instance conflict")
	})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetEnabled(context.Background(), "ball8", true); !errors.Is(err, ErrInvalidAppConfiguration) {
		t.Fatalf("enable error = %v", err)
	}
	if _, outcome, err := service.ReplaceAppConfiguration(context.Background(), "ball8", AppConfiguration{
		Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}},
	}); !errors.Is(err, ErrInvalidAppConfiguration) || outcome.IsCommitted() {
		t.Fatalf("replace outcome/error = %q, %v", outcome, err)
	}
	if _, err := service.DeleteAppInstance(context.Background(), "ball8"); !errors.Is(err, ErrInvalidAppConfiguration) {
		t.Fatalf("delete error = %v", err)
	}
	persisted, err := store.Load()
	if err != nil || persisted.Generation != 1 || persisted.Apps["ball8"].Enabled {
		t.Fatalf("persisted document = %#v, %v", persisted, err)
	}
}

func TestLoadRejectsDesiredStateThatFailsPluginValidation(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, nil, plugins)
	setTestDesiredStateValidator(service.desired, func(config.Document) error { return errors.New("plugin schema rejected") })
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err == nil {
		t.Fatal("Load accepted plugin-invalid desired state")
	}
	if _, loaded := service.Document(); loaded || len(plugins.lastSpecs) != 0 {
		t.Fatalf("invalid document became live: loaded=%v specs=%#v", loaded, plugins.lastSpecs)
	}
}

func TestDeleteAppInstanceCommitsBeforePluginRetirement(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	document.Apps["second"] = config.App{ID: "second", PluginID: "plugin", Enabled: true, Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}}}
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, nil, plugins)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	operation, err := service.DeleteAppInstance(context.Background(), "second")
	if err != nil {
		t.Fatalf("DeleteAppInstance: %v", err)
	}
	if !operation.Outcome.IsCommitted() || operation.Generation != 2 {
		t.Fatalf("operation = %#v", operation)
	}
	if _, exists := operation.Apps["second"]; exists {
		t.Fatalf("deleted app remains in %#v", operation.Apps)
	}
	if len(plugins.lastSpecs) != 1 || len(plugins.lastSpecs[0].Instances) != 1 {
		t.Fatalf("reconciled specs = %#v", plugins.lastSpecs)
	}
}

func TestDeleteReplacementSerializesSameIDRecreation(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	definition := config.App{ID: "second", PluginID: "plugin", Enabled: true, Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}}}
	document.Apps[definition.ID] = definition
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	plugins := newBlockingApplyController()
	service := newTestReconciler(t, store, nil, plugins)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	deleteDone := make(chan error, 1)
	go func() {
		_, err := service.DeleteAppInstance(context.Background(), definition.ID)
		deleteDone <- err
	}()
	awaitServiceSignal(t, plugins.oldStarted, "delete replacement")
	createDone := make(chan error, 1)
	go func() {
		_, err := service.CreateAppInstance(context.Background(), definition)
		createDone <- err
	}()
	select {
	case err := <-createDone:
		t.Fatalf("same-ID recreation passed cleanup boundary: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(plugins.oldRelease)
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteAppInstance: %v", err)
	}
	if err := <-createDone; err != nil {
		t.Fatalf("CreateAppInstance: %v", err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Apps[definition.ID].Generation; got != 3 {
		t.Fatalf("recreated generation = %d, want 3", got)
	}
}

func TestDeleteRetirementDoesNotBlockUnrelatedAppCreation(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	deleted := config.App{ID: "second", PluginID: "plugin", Enabled: true, Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}}}
	document.Apps[deleted.ID] = deleted
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	plugins := newBlockingApplyController()
	service := newTestReconciler(t, store, nil, plugins)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(func() { close(plugins.oldRelease) })
	t.Cleanup(release)

	deleteDone := make(chan error, 1)
	go func() {
		_, err := service.DeleteAppInstance(context.Background(), deleted.ID)
		deleteDone <- err
	}()
	awaitServiceSignal(t, plugins.oldStarted, "deleted-app retirement")

	unrelated := config.App{ID: "third", PluginID: "plugin", Enabled: true, Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}}}
	createDone := make(chan error, 1)
	go func() {
		_, err := service.CreateAppInstance(context.Background(), unrelated)
		createDone <- err
	}()
	select {
	case err := <-createDone:
		if err != nil {
			t.Fatalf("CreateAppInstance for unrelated app: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated app creation waited for deleted-app retirement")
	}

	release()
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteAppInstance: %v", err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := loaded.Apps[deleted.ID]; exists {
		t.Fatalf("deleted app remains in %#v", loaded.Apps)
	}
	if _, exists := loaded.Apps[unrelated.ID]; !exists {
		t.Fatalf("unrelated app was not committed in %#v", loaded.Apps)
	}
}

func TestCanceledSameIDRecreationStopsWaitingForRetirement(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	definition := config.App{ID: "second", PluginID: "plugin", Enabled: true, Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}}}
	document.Apps[definition.ID] = definition
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	plugins := newBlockingApplyController()
	service := newTestReconciler(t, store, nil, plugins)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(func() { close(plugins.oldRelease) })
	t.Cleanup(release)

	deleteDone := make(chan error, 1)
	go func() {
		_, err := service.DeleteAppInstance(context.Background(), definition.ID)
		deleteDone <- err
	}()
	awaitServiceSignal(t, plugins.oldStarted, "deleted-app retirement")

	ctx, cancel := context.WithCancel(context.Background())
	createDone := make(chan error, 1)
	go func() {
		_, err := service.CreateAppInstance(ctx, definition)
		createDone <- err
	}()
	cancel()
	select {
	case err := <-createDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled recreation error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled same-ID recreation remained blocked by retirement")
	}

	release()
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteAppInstance: %v", err)
	}
}

func TestCanceledDeleteReconciliationFinishesRetirementFence(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	definition := config.App{ID: "second", PluginID: "plugin", Enabled: true, Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}}}
	document.Apps[definition.ID] = definition
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	plugins := newCancelBlockedApplyController()
	service := newTestReconciler(t, store, nil, plugins)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	type deleteResult struct {
		result AppInstanceResult
		err    error
	}
	deleteDone := make(chan deleteResult, 1)
	go func() {
		result, err := service.DeleteAppInstance(ctx, definition.ID)
		deleteDone <- deleteResult{result: result, err: err}
	}()
	awaitServiceSignal(t, plugins.started, "cancelable deleted-app retirement")
	cancel()
	deleted := <-deleteDone
	if deleted.err != nil {
		t.Fatalf("DeleteAppInstance: %v", deleted.err)
	}
	if !errors.Is(deleted.result.ReconciliationError, context.Canceled) {
		t.Fatalf("delete reconciliation error = %v, want context cancellation", deleted.result.ReconciliationError)
	}
	if _, err := service.CreateAppInstance(context.Background(), definition); err != nil {
		t.Fatalf("same-ID recreation after canceled cleanup: %v", err)
	}
}

func TestShutdownReleasesSameIDRetirementWaiter(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	definition := config.App{ID: "second", PluginID: "plugin", Enabled: true, Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}}}
	document.Apps[definition.ID] = definition
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	plugins := newBlockingApplyController()
	service := newTestReconciler(t, store, nil, plugins)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(func() { close(plugins.oldRelease) })
	t.Cleanup(release)

	deleteDone := make(chan error, 1)
	go func() {
		_, err := service.DeleteAppInstance(context.Background(), definition.ID)
		deleteDone <- err
	}()
	awaitServiceSignal(t, plugins.oldStarted, "deleted-app retirement")

	createDone := make(chan error, 1)
	go func() {
		_, err := service.CreateAppInstance(context.Background(), definition)
		createDone <- err
	}()
	closeDone := make(chan error, 1)
	go func() { closeDone <- service.Close(context.Background()) }()
	select {
	case err := <-createDone:
		if err == nil || err.Error() != "daemon is closing" {
			t.Fatalf("recreation during shutdown error = %v, want daemon-closing error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown left same-ID recreation blocked by retirement")
	}

	release()
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteAppInstance: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type cancelBlockedApplyController struct {
	*safePluginController
	mu      sync.Mutex
	calls   int
	started chan struct{}
}

func newCancelBlockedApplyController() *cancelBlockedApplyController {
	return &cancelBlockedApplyController{safePluginController: &safePluginController{}, started: make(chan struct{}, 1)}
}

func (f *cancelBlockedApplyController) Apply(ctx context.Context, specs []pluginhost.Spec) error {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()
	if call == 2 {
		f.started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	return f.safePluginController.Apply(ctx, specs)
}
