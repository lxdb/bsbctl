package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lxdb/bsbctl/internal/checkpoint"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"path/filepath"
	"testing"
	"time"
)

func TestReconcilerRestoresAndReconcilesGenerationScopedCheckpointsAcrossRestart(t *testing.T) {
	configStore := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := configStore.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	checkpointStore := checkpoint.NewStore(filepath.Join(t.TempDir(), "checkpoints"))
	key := checkpoint.Key{PluginID: "plugin", InstanceID: "ball8", Generation: 1}
	if _, err := checkpointStore.Save(key, json.RawMessage(`{"cursor":"next"}`)); err != nil {
		t.Fatal(err)
	}

	firstPlugins := &fakePluginController{}
	first := newTestReconcilerWithCheckpoints(t, configStore, nil, firstPlugins, checkpointStore)
	if err := first.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertPluginCheckpointRestore(t, firstPlugins.lastSpecs, 1, `{"cursor":"next"}`)
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	secondPlugins := &fakePluginController{}
	second := newTestReconcilerWithCheckpoints(t, configStore, nil, secondPlugins, checkpointStore)
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	if err := second.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertPluginCheckpointRestore(t, secondPlugins.lastSpecs, 1, `{"cursor":"next"}`)
	if _, err := second.SetEnabled(context.Background(), "ball8", false); err != nil {
		t.Fatal(err)
	}
	if data, found, err := checkpointStore.Load(key); err != nil || found || data != nil {
		t.Fatalf("disabled checkpoint remains = %s, %v, %v", data, found, err)
	}
	if _, err := second.SetEnabled(context.Background(), "ball8", true); err != nil {
		t.Fatal(err)
	}
	if len(secondPlugins.lastSpecs) != 1 || secondPlugins.lastSpecs[0].Instances[0].Checkpoint != nil || secondPlugins.lastSpecs[0].Instances[0].Generation != 3 {
		t.Fatalf("re-enabled specs retained stale checkpoint: %#v", secondPlugins.lastSpecs)
	}
}

func TestFailedReplacementPreservesOldCheckpoint(t *testing.T) {
	configStore := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := configStore.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	checkpointStore := checkpoint.NewStore(filepath.Join(t.TempDir(), "checkpoints"))
	key := checkpoint.Key{PluginID: "plugin", InstanceID: "ball8", Generation: 1}
	if _, err := checkpointStore.Save(key, json.RawMessage(`{"cursor":"old"}`)); err != nil {
		t.Fatal(err)
	}
	plugins := newScriptedPluginController(nil, errors.New("replacement rejected"))
	service := newTestReconcilerWithCheckpointsAndRetryDelay(t, configStore, nil, plugins, checkpointStore, func(int) time.Duration { return time.Hour })
	t.Cleanup(func() { _ = service.Close(t.Context()) })
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, outcome, err := service.ReplaceAppConfiguration(t.Context(), "ball8", AppConfiguration{
		Config:       json.RawMessage(`{"revision":2}`),
		Policies:     map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}},
		LaunchAction: "ask",
	})
	if !outcome.IsCommitted() || err == nil {
		t.Fatalf("replacement outcome/error = %q, %v", outcome, err)
	}
	if data, found, loadErr := checkpointStore.Load(key); loadErr != nil || !found || string(data) != `{"cursor":"old"}` {
		t.Fatalf("old checkpoint after failed replacement = %s, %v, %v", data, found, loadErr)
	}
}

func TestSuccessfulReplacementRetiresOnlyAffectedCheckpoint(t *testing.T) {
	document := serviceDocument(true)
	sibling := document.Apps["ball8"]
	sibling.ID = "sibling"
	document.Apps[sibling.ID] = sibling
	configStore := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := configStore.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	checkpointStore := checkpoint.NewStore(filepath.Join(t.TempDir(), "checkpoints"))
	changedKey := checkpoint.Key{PluginID: "plugin", InstanceID: "ball8", Generation: 1}
	siblingKey := checkpoint.Key{PluginID: "plugin", InstanceID: "sibling", Generation: 1}
	for _, key := range []checkpoint.Key{changedKey, siblingKey} {
		if _, err := checkpointStore.Save(key, json.RawMessage(`{"cursor":"saved"}`)); err != nil {
			t.Fatal(err)
		}
	}
	service := newTestReconcilerWithCheckpoints(t, configStore, nil, &safePluginController{}, checkpointStore)
	t.Cleanup(func() { _ = service.Close(t.Context()) })
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.ReplaceAppConfiguration(t.Context(), "ball8", AppConfiguration{
		Config:       json.RawMessage(`{"revision":2}`),
		Policies:     map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}},
		LaunchAction: "ask",
	}); err != nil {
		t.Fatal(err)
	}
	if data, found, err := checkpointStore.Load(changedKey); err != nil || found || data != nil {
		t.Fatalf("affected checkpoint remains = %s, %v, %v", data, found, err)
	}
	if data, found, err := checkpointStore.Load(siblingKey); err != nil || !found || string(data) != `{"cursor":"saved"}` {
		t.Fatalf("sibling checkpoint = %s, %v, %v", data, found, err)
	}
}

func TestReconcilerSaveCheckpointAuthenticatesCurrentDesiredGeneration(t *testing.T) {
	configStore := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := configStore.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	checkpointStore := checkpoint.NewStore(filepath.Join(t.TempDir(), "checkpoints"))
	service := newTestReconcilerWithCheckpoints(t, configStore, nil, &safePluginController{}, checkpointStore)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	valid := protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: "ball8", Generation: 1}, Data: json.RawMessage(`{"cursor":1}`)}
	if err := service.checkpoints.SaveCheckpoint("plugin", valid); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	for _, test := range []struct {
		pluginID string
		request  protocol.CheckpointRequest
	}{
		{pluginID: "other", request: valid},
		{pluginID: "plugin", request: protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: "other", Generation: 1}, Data: json.RawMessage(`{}`)}},
		{pluginID: "plugin", request: protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: "ball8", Generation: 2}, Data: json.RawMessage(`{}`)}},
	} {
		err := service.checkpoints.SaveCheckpoint(test.pluginID, test.request)
		checkpointErr, ok := errors.AsType[*checkpoint.Error](err)
		if !ok || checkpointErr.Code != checkpoint.InvalidCode {
			t.Fatalf("unauthenticated SaveCheckpoint error = %#v", err)
		}
	}
	data, found, err := checkpointStore.Load(checkpoint.Key{PluginID: "plugin", InstanceID: "ball8", Generation: 1})
	if err != nil || !found || string(data) != `{"cursor":1}` {
		t.Fatalf("authenticated checkpoint = %s, %v, %v", data, found, err)
	}
}

func TestReconcilerRejectsInvalidCheckpointPayloadBeforeStore(t *testing.T) {
	configStore := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := configStore.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	checkpointStore := &recordingCheckpointStore{}
	service := newTestReconcilerWithCheckpoints(t, configStore, nil, &safePluginController{}, checkpointStore)
	t.Cleanup(func() { _ = service.Close(t.Context()) })
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, data := range []json.RawMessage{
		json.RawMessage(`"scalar"`),
		json.RawMessage(`[]`),
		json.RawMessage(`null`),
	} {
		err := service.checkpoints.SaveCheckpoint("plugin", protocol.CheckpointRequest{
			Instance: protocol.InstanceRef{ID: "ball8", Generation: 1},
			Data:     data,
		})
		checkpointErr, ok := errors.AsType[*checkpoint.Error](err)
		if !ok || checkpointErr.Code != checkpoint.InvalidCode || checkpointErr.Outcome != localstate.NotCommitted {
			t.Fatalf("invalid checkpoint %s error = %#v, want checkpoint invalid/not-committed", data, err)
		}
	}
	if checkpointStore.saves != 0 {
		t.Fatalf("checkpoint store calls = %d, want 0", checkpointStore.saves)
	}
}

func TestReconcilerCheckpointIODoesNotHoldMainStateLock(t *testing.T) {
	configStore := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := configStore.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	backend := &blockingCheckpointStore{started: make(chan struct{}), release: make(chan struct{})}
	service := newTestReconcilerWithCheckpoints(t, configStore, nil, &safePluginController{}, backend)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved := make(chan error, 1)
	go func() {
		saved <- service.checkpoints.SaveCheckpoint("plugin", protocol.CheckpointRequest{
			Instance: protocol.InstanceRef{ID: "ball8", Generation: 1}, Data: json.RawMessage(`{}`),
		})
	}()
	<-backend.started
	documented := make(chan struct{})
	go func() {
		_, _ = service.Document()
		close(documented)
	}()
	select {
	case <-documented:
	case <-time.After(time.Second):
		t.Fatal("checkpoint I/O held the service main state lock")
	}
	close(backend.release)
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
}

type blockingCheckpointStore struct {
	started chan struct{}
	release chan struct{}
}

func (s *blockingCheckpointStore) Save(checkpoint.Key, json.RawMessage) (localstate.CommitOutcome, error) {
	close(s.started)
	<-s.release
	return localstate.Committed, nil
}

func (*blockingCheckpointStore) Load(checkpoint.Key) (json.RawMessage, bool, error) {
	return nil, false, nil
}

func (*blockingCheckpointStore) Reconcile([]checkpoint.Key) error { return nil }

func (*blockingCheckpointStore) Status() checkpoint.Status { return checkpoint.Status{} }

type recordingCheckpointStore struct{ saves int }

func (s *recordingCheckpointStore) Save(checkpoint.Key, json.RawMessage) (localstate.CommitOutcome, error) {
	s.saves++
	return localstate.Committed, nil
}

func (*recordingCheckpointStore) Load(checkpoint.Key) (json.RawMessage, bool, error) {
	return nil, false, nil
}

func (*recordingCheckpointStore) Reconcile([]checkpoint.Key) error { return nil }

func (*recordingCheckpointStore) Status() checkpoint.Status { return checkpoint.Status{} }

func assertPluginCheckpointRestore(t *testing.T, specs []pluginhost.Spec, generation uint64, data string) {
	t.Helper()
	if len(specs) != 1 || len(specs[0].Instances) != 1 {
		t.Fatalf("specs = %#v", specs)
	}
	restore := specs[0].Instances[0].Checkpoint
	if restore == nil || restore.Generation != generation || string(restore.Data) != data {
		t.Fatalf("checkpoint restore = %#v", restore)
	}
}
