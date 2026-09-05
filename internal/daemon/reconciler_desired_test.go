package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/presentation"
	"path/filepath"
	"testing"
)

func TestReconcilerAcceptsDurabilityUncertainConfigWithoutRetryingGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	realStore := config.NewStore(path)
	if _, err := realStore.ReplaceWithOutcome(0, serviceDocument(false)); err != nil {
		t.Fatal(err)
	}
	store := &durabilityUncertainConfigStore{Store: realStore}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, nil, plugins)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	updated, err := service.SetEnabled(context.Background(), "ball8", true)
	if err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if updated.Generation != 2 || !updated.Apps["ball8"].Enabled || store.updates != 1 {
		t.Fatalf("updated/store = %#v, %d updates", updated, store.updates)
	}
	if got := service.desired.PersistenceStatus().LastErrorCode; got != ConfigDurabilityUncertainCode {
		t.Fatalf("configuration diagnostic = %q", got)
	}
	again, err := service.SetEnabled(context.Background(), "ball8", true)
	if err != nil || again.Generation != 2 || store.updates != 1 {
		t.Fatalf("idempotent retry = %#v, %v, %d updates", again, err, store.updates)
	}
	loaded, err := realStore.Load()
	if err != nil || loaded.Generation != 2 || !loaded.Apps["ball8"].Enabled {
		t.Fatalf("committed config = %#v, %v", loaded, err)
	}
}

type durabilityUncertainConfigStore struct {
	*config.Store
	updates int
}

func (s *durabilityUncertainConfigStore) Update(expected uint64, mutate func(*config.Document) error) (config.Document, localstate.CommitOutcome, error) {
	s.updates++
	document, outcome, err := s.Store.Update(expected, mutate)
	if err != nil || !outcome.IsCommitted() {
		return document, outcome, err
	}
	return document, localstate.CommittedDurabilityUncertain, &localstate.CommitError{
		Outcome: localstate.CommittedDurabilityUncertain,
		Op:      "sync state directory",
		Err:     errors.New("injected directory sync failure"),
	}
}

func TestReconcilerEnablePersistsGenerationAndReconcilesPlugins(t *testing.T) {
	t.Parallel()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(false)
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, nil, plugins)
	if err := service.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(plugins.lastSpecs) != 0 {
		t.Fatalf("disabled app specs = %#v", plugins.lastSpecs)
	}

	updated, err := service.SetEnabled(context.Background(), "ball8", true)
	if err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if updated.Generation != 2 || !updated.Apps["ball8"].Enabled {
		t.Fatalf("updated = %#v", updated)
	}
	if len(plugins.lastSpecs) != 1 || plugins.lastSpecs[0].Instances[0].Generation != 2 {
		t.Fatalf("applied specs = %#v", plugins.lastSpecs)
	}
	loaded, err := store.Load()
	if err != nil || loaded.Generation != 2 || !loaded.Apps["ball8"].Enabled {
		t.Fatalf("durable config = %#v, %v", loaded, err)
	}
	if generation, ok := service.Generation("plugin", "ball8"); !ok || generation != 2 {
		t.Fatalf("service generation = %d, %v", generation, ok)
	}
}

func TestAcceptDocumentDoesNotPromotePreparedGeneration(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	service := newTestReconciler(t, store, nil, &safePluginController{})
	t.Cleanup(func() { _ = service.Close(t.Context()) })
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

	prepared, _ := service.Document()
	prepared.Generation = 2
	app := prepared.Apps["ball8"]
	app.Generation = 2
	prepared.Apps[app.ID] = app
	if _, _, accepted := service.acceptDocument(prepared); !accepted {
		t.Fatal("prepared document was not accepted")
	}
	service.live.mu.Lock()
	service.live.candidate = &reconcileCandidate{epoch: service.live.epoch, revision: 1, plan: ReconciliationPlan{
		Generations: Generations{values: map[generationKey]uint64{{pluginID: "plugin", instanceID: "ball8"}: 2}},
	}}
	service.live.mu.Unlock()

	next := cloneDocument(prepared)
	next.Generation = 3
	next.Apps["unrelated"] = config.App{
		ID: "unrelated", PluginID: "plugin", Generation: 3, Enabled: false, Config: json.RawMessage(`{}`),
		Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}},
	}
	if _, _, accepted := service.acceptDocument(next); !accepted {
		t.Fatal("unrelated document was not accepted")
	}
	if generation, active := service.Generation("plugin", "ball8"); active {
		t.Fatalf("prepared generation became active: %d", generation)
	}
}
