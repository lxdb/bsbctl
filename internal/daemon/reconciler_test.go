package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/checkpoint"
	"github.com/lxdb/bsbctl/internal/config"
)

func newTestReconciler(t testing.TB, store ConfigurationStore, resolver SecretResolver, plugins PluginController, assetControllers ...AssetController) *Reconciler {
	t.Helper()
	return newTestReconcilerWithCheckpoints(t, store, resolver, plugins, checkpoint.NewStore(filepath.Join(t.TempDir(), "checkpoints")), assetControllers...)
}

func newTestReconcilerWithCheckpoints(t testing.TB, store ConfigurationStore, resolver SecretResolver, plugins PluginController, checkpointStore CheckpointStore, assetControllers ...AssetController) *Reconciler {
	t.Helper()
	return newTestReconcilerWithRuntime(t, store, resolver, plugins, checkpointStore, &recordingSessionInputController{}, func(context.Context) error { return nil }, &observationDiagnostics{}, nil, assetControllers...)
}

func newTestReconcilerWithSessionInputs(t testing.TB, store ConfigurationStore, resolver SecretResolver, plugins PluginController, inputs SessionInputController, assetControllers ...AssetController) *Reconciler {
	t.Helper()
	return newTestReconcilerWithRuntime(t, store, resolver, plugins, checkpoint.NewStore(filepath.Join(t.TempDir(), "checkpoints")), inputs, func(context.Context) error { return nil }, &observationDiagnostics{}, nil, assetControllers...)
}

func newTestReconcilerWithInvalidator(t testing.TB, store ConfigurationStore, resolver SecretResolver, plugins PluginController, invalidate sessionContextInvalidator, assetControllers ...AssetController) *Reconciler {
	t.Helper()
	return newTestReconcilerWithRuntime(t, store, resolver, plugins, checkpoint.NewStore(filepath.Join(t.TempDir(), "checkpoints")), &recordingSessionInputController{}, invalidate, &observationDiagnostics{}, nil, assetControllers...)
}

func newTestReconcilerWithAttention(t testing.TB, store ConfigurationStore, resolver SecretResolver, plugins PluginController, attentionController AttentionController, assetControllers ...AssetController) *Reconciler {
	t.Helper()
	return newTestReconcilerWithRuntime(t, store, resolver, plugins, checkpoint.NewStore(filepath.Join(t.TempDir(), "checkpoints")), &recordingSessionInputController{}, func(context.Context) error { return nil }, attentionController, nil, assetControllers...)
}

func newTestReconcilerWithRetryDelay(t testing.TB, store ConfigurationStore, resolver SecretResolver, plugins PluginController, retryDelay func(int) time.Duration, assetControllers ...AssetController) *Reconciler {
	t.Helper()
	return newTestReconcilerWithRuntime(t, store, resolver, plugins, checkpoint.NewStore(filepath.Join(t.TempDir(), "checkpoints")), &recordingSessionInputController{}, func(context.Context) error { return nil }, &observationDiagnostics{}, retryDelay, assetControllers...)
}

func newTestReconcilerWithAttentionAndRetryDelay(t testing.TB, store ConfigurationStore, resolver SecretResolver, plugins PluginController, attentionController AttentionController, retryDelay func(int) time.Duration, assetControllers ...AssetController) *Reconciler {
	t.Helper()
	return newTestReconcilerWithRuntime(t, store, resolver, plugins, checkpoint.NewStore(filepath.Join(t.TempDir(), "checkpoints")), &recordingSessionInputController{}, func(context.Context) error { return nil }, attentionController, retryDelay, assetControllers...)
}

func newTestReconcilerWithCheckpointsAndRetryDelay(t testing.TB, store ConfigurationStore, resolver SecretResolver, plugins PluginController, checkpointStore CheckpointStore, retryDelay func(int) time.Duration, assetControllers ...AssetController) *Reconciler {
	t.Helper()
	return newTestReconcilerWithRuntime(t, store, resolver, plugins, checkpointStore, &recordingSessionInputController{}, func(context.Context) error { return nil }, &observationDiagnostics{}, retryDelay, assetControllers...)
}

func newTestReconcilerWithRuntime(t testing.TB, store ConfigurationStore, resolver SecretResolver, plugins PluginController, checkpointStore CheckpointStore, inputs SessionInputController, invalidate sessionContextInvalidator, attentionController AttentionController, retryDelay func(int) time.Duration, assetControllers ...AssetController) *Reconciler {
	t.Helper()
	desired, err := NewDesiredState(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLiveState()
	checkpoints, err := NewCheckpoints(checkpointStore, live)
	if err != nil {
		t.Fatal(err)
	}
	sessions := NewSessionCoordinator(func(change SessionChange) { change.Apply(inputs) })
	sessionRuntime, err := NewSessionRuntime(sessions, plugins, inputs, invalidate)
	if err != nil {
		t.Fatal(err)
	}
	assetController := AssetController(&fakeAssetController{ready: true})
	if len(assetControllers) > 0 {
		assetController = assetControllers[0]
	}
	policy, err := NewPolicyResolver(live, sessions, assetController)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := NewReconciler(ReconcilerOptions{
		Desired: desired, Live: live, Sessions: sessionRuntime, Policy: policy, Checkpoints: checkpoints,
		Resolver: resolver, Plugins: plugins, Attention: attentionController, Assets: assetController, RetryDelay: retryDelay,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := reconciler.Close(ctx); err != nil {
			t.Errorf("close test reconciler: %v", err)
		}
	})
	return reconciler
}

func TestOwnerConstructorsRejectMissingRequiredDependencies(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	desired, err := NewDesiredState(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLiveState()
	plugins := &safePluginController{}
	sessionState := NewSessionCoordinator(nil)
	inputs := &recordingSessionInputController{}
	sessions, err := NewSessionRuntime(sessionState, plugins, inputs, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := NewCheckpoints(checkpoint.NewStore(filepath.Join(t.TempDir(), "checkpoints")), live)
	if err != nil {
		t.Fatal(err)
	}
	assetController := &fakeAssetController{ready: true}
	attentionController := &observationDiagnostics{}
	policy, err := NewPolicyResolver(live, sessionState, assetController)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := NewDesiredState(nil, nil); err == nil {
		t.Fatal("NewDesiredState accepted a nil store")
	}
	tests := []struct {
		name        string
		desired     *DesiredState
		live        *LiveState
		sessions    *SessionRuntime
		checkpoints *Checkpoints
		plugins     PluginController
		attention   AttentionController
		assets      AssetController
	}{
		{name: "policy resolver", desired: desired, live: live, sessions: sessions, checkpoints: checkpoints, plugins: plugins, attention: attentionController, assets: assetController},
		{name: "desired state", live: live, sessions: sessions, checkpoints: checkpoints, plugins: plugins, attention: attentionController, assets: assetController},
		{name: "live state", desired: desired, sessions: sessions, checkpoints: checkpoints, plugins: plugins, attention: attentionController, assets: assetController},
		{name: "session runtime", desired: desired, live: live, checkpoints: checkpoints, plugins: plugins, attention: attentionController, assets: assetController},
		{name: "checkpoints", desired: desired, live: live, sessions: sessions, plugins: plugins, attention: attentionController, assets: assetController},
		{name: "plugin controller", desired: desired, live: live, sessions: sessions, checkpoints: checkpoints, attention: attentionController, assets: assetController},
		{name: "attention controller", desired: desired, live: live, sessions: sessions, checkpoints: checkpoints, plugins: plugins, assets: assetController},
		{name: "asset controller", desired: desired, live: live, sessions: sessions, checkpoints: checkpoints, plugins: plugins, attention: attentionController},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			injectedPolicy := policy
			if test.name == "policy resolver" {
				injectedPolicy = nil
			}
			if _, err := NewReconciler(ReconcilerOptions{
				Desired: test.desired, Live: test.live, Sessions: test.sessions, Policy: injectedPolicy, Checkpoints: test.checkpoints,
				Plugins: test.plugins, Attention: test.attention, Assets: test.assets,
			}); err == nil {
				t.Fatal("NewReconciler accepted a missing required dependency")
			}
		})
	}
}
