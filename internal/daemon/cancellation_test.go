package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
)

func TestSetEnabledCancellationDuringSecretResolutionReturnsCommittedPartialAndKeepsRetry(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(false)
	app := document.Apps["ball8"]
	app.Secrets = map[string]string{"token": "keychain://bsbctl/account"}
	document.Apps["ball8"] = app
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	resolver := SecretResolverFunc(func(ctx context.Context, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	service := newTestReconcilerWithRetryDelay(t, store, resolver, &safePluginController{}, func(int) time.Duration { return time.Hour })
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	type response struct {
		operation EnableResult
		err       error
	}
	done := make(chan response, 1)
	go func() {
		operation, err := service.SetEnabled(ctx, "ball8", true)
		done <- response{operation: operation, err: err}
	}()
	awaitServiceSignal(t, started, "secret resolution after commit")
	cancel()
	result := <-done
	if result.err != nil || !errors.Is(result.operation.ReconciliationError, context.Canceled) {
		t.Fatalf("SetEnabled errors = %v / %v, want committed cancellation", result.err, result.operation.ReconciliationError)
	}
	if result.operation.Outcome != localstate.Committed || result.operation.Generation != 2 || !result.operation.Apps["ball8"].Enabled {
		t.Fatalf("committed operation = %#v", result.operation)
	}
	persisted, err := store.Load()
	if err != nil || persisted.Generation != 2 || !persisted.Apps["ball8"].Enabled {
		t.Fatalf("persisted document = %#v, %v", persisted, err)
	}
	service.live.mu.RLock()
	retryOwned := service.live.candidate != nil && !service.live.appReadiness["ball8"].RetryAt.IsZero()
	service.live.mu.RUnlock()
	if !retryOwned {
		t.Fatal("current committed generation has no daemon-owned retry")
	}
}

func TestReplaceAppConfigurationCancellationDuringPluginApplyReturnsCommittedPartialAndKeepsRetry(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := newCancelingApplyController()
	service := newTestReconcilerWithRetryDelay(t, store, nil, plugins, func(int) time.Duration { return time.Hour })
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	plugins.blockNext()

	ctx, cancel := context.WithCancel(context.Background())
	type response struct {
		document config.Document
		outcome  localstate.CommitOutcome
		err      error
	}
	done := make(chan response, 1)
	go func() {
		updated, outcome, err := service.ReplaceAppConfiguration(ctx, "ball8", AppConfiguration{
			Config: json.RawMessage(`{"question":"new"}`),
			Policies: map[string]presentation.PolicyConfig{
				"answer": {Policy: presentation.PolicyInteractive},
			},
		})
		done <- response{document: updated, outcome: outcome, err: err}
	}()
	awaitServiceSignal(t, plugins.started, "plugin Apply after config commit")
	cancel()
	result := <-done
	if result.outcome != localstate.Committed || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("ReplaceAppConfiguration outcome/error = %q, %v", result.outcome, result.err)
	}
	if result.document.Generation != 2 || string(result.document.Apps["ball8"].Config) != `{"question":"new"}` {
		t.Fatalf("committed document = %#v", result.document)
	}
	assertCurrentGenerationCorrectionOwnedOrComplete(t, service, 2)
}

func TestActivatePluginCancellationDuringAttentionReconcileReturnsCommittedPartialAndKeepsCorrection(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	attention := &cancelableAttention{started: make(chan struct{})}
	service := newTestReconcilerWithAttentionAndRetryDelay(t, store, nil, &safePluginController{}, attention, func(int) time.Duration { return time.Hour })
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	attention.arm()
	desired, err := service.DesiredPlugin(context.Background(), "plugin")
	if err != nil || desired == nil {
		t.Fatalf("DesiredPlugin = %#v, %v", desired, err)
	}
	desired.Version = "2"
	desired.Executable = "/verified/plugin-v2"

	ctx, cancel := context.WithCancel(context.Background())
	type response struct {
		outcome localstate.CommitOutcome
		err     error
	}
	done := make(chan response, 1)
	go func() {
		outcome, err := service.ActivatePlugin(ctx, *desired)
		done <- response{outcome: outcome, err: err}
	}()
	awaitServiceSignal(t, attention.started, "attention reconcile after plugin commit")
	cancel()
	result := <-done
	if result.outcome != localstate.Committed || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("ActivatePlugin outcome/error = %q, %v", result.outcome, result.err)
	}
	assertCurrentGenerationCorrectionOwnedOrComplete(t, service, 2)
}

func TestActivatePluginCancellationDuringAssetReconcileReturnsCommittedPartialAndKeepsCorrection(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	assetController := newCancelingAssetController()
	service := newTestReconcilerWithRetryDelay(t, store, nil, &safePluginController{}, func(int) time.Duration { return time.Hour }, assetController)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	assetController.arm()
	desired, err := service.DesiredPlugin(context.Background(), "plugin")
	if err != nil || desired == nil {
		t.Fatalf("DesiredPlugin = %#v, %v", desired, err)
	}
	desired.Version = "2"
	desired.Executable = "/verified/plugin-v2"

	ctx, cancel := context.WithCancel(context.Background())
	type response struct {
		outcome localstate.CommitOutcome
		err     error
	}
	done := make(chan response, 1)
	go func() {
		outcome, err := service.ActivatePlugin(ctx, *desired)
		done <- response{outcome: outcome, err: err}
	}()
	awaitServiceSignal(t, assetController.started, "asset reconcile after plugin commit")
	cancel()
	result := <-done
	if result.outcome != localstate.Committed || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("ActivatePlugin outcome/error = %q, %v", result.outcome, result.err)
	}
	awaitServiceSignal(t, assetController.retryStarted, "daemon-owned asset correction")
	assertCurrentGenerationCorrectionOwnedOrComplete(t, service, 2)
	close(assetController.releaseRetry)
}

func TestCommittedCancellationWithoutCandidateConstructsDaemonOwnedRetry(t *testing.T) {
	service := &Reconciler{
		live: &LiveState{
			epoch: 7,
			document: config.Document{Apps: map[string]config.App{
				"ball8": {ID: "ball8", PluginID: "plugin", Enabled: true},
			}},
			appReadiness: map[string]AppReadiness{
				"ball8": {AppID: "ball8", PluginID: "plugin", Phase: AppReconcilePending},
			},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.operationReconciliationError(ctx, 7, context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("operation error = %v", err)
	}
	service.live.mu.RLock()
	status := service.live.appReadiness["ball8"]
	service.live.mu.RUnlock()
	if status.RetryAt.IsZero() || status.LastErrorCode != reconcileFailedCode {
		t.Fatalf("retry status = %#v", status)
	}
}

type cancelingApplyController struct {
	*safePluginController
	mu      sync.Mutex
	block   bool
	started chan struct{}
}

type cancelingAssetController struct {
	mu           sync.Mutex
	armed        bool
	calls        int
	started      chan struct{}
	retryStarted chan struct{}
	releaseRetry chan struct{}
}

func newCancelingAssetController() *cancelingAssetController {
	return &cancelingAssetController{started: make(chan struct{}), retryStarted: make(chan struct{}), releaseRetry: make(chan struct{})}
}

func (c *cancelingAssetController) arm() {
	c.mu.Lock()
	c.armed = true
	c.mu.Unlock()
}

func (c *cancelingAssetController) Reconcile(ctx context.Context, _ []assets.Package) {
	c.mu.Lock()
	if !c.armed {
		c.mu.Unlock()
		return
	}
	c.calls++
	call := c.calls
	c.mu.Unlock()
	switch call {
	case 1:
		close(c.started)
		<-ctx.Done()
	case 3:
		close(c.retryStarted)
		select {
		case <-c.releaseRetry:
		case <-ctx.Done():
		}
	}
}

func (*cancelingAssetController) Ready(string) bool                                { return true }
func (*cancelingAssetController) ReadyFor(assets.Package) bool                     { return true }
func (*cancelingAssetController) Status() []assets.State                           { return nil }
func (*cancelingAssetController) CollectGarbage(context.Context, []assets.Package) {}

func newCancelingApplyController() *cancelingApplyController {
	return &cancelingApplyController{safePluginController: &safePluginController{}, started: make(chan struct{})}
}

func (c *cancelingApplyController) blockNext() {
	c.mu.Lock()
	c.block = true
	c.mu.Unlock()
}

func (c *cancelingApplyController) Apply(ctx context.Context, specs []pluginhost.Spec) error {
	c.mu.Lock()
	block := c.block
	c.block = false
	c.mu.Unlock()
	if block {
		close(c.started)
		<-ctx.Done()
		return ctx.Err()
	}
	return c.safePluginController.Apply(ctx, specs)
}

func assertCurrentGenerationCorrectionOwnedOrComplete(t *testing.T, service *Reconciler, generation uint64) {
	t.Helper()
	document, loaded := service.Document()
	if !loaded || document.Generation != generation {
		t.Fatalf("observable document = %#v, loaded=%v", document, loaded)
	}
	service.live.mu.RLock()
	retryOwnedOrComplete := service.live.candidate != nil && (service.live.activeAttempt != 0 || service.live.correctionRevision != 0 || !service.live.appReadiness["ball8"].RetryAt.IsZero() || service.live.finalizedRevision == service.live.candidate.revision)
	service.live.mu.RUnlock()
	if !retryOwnedOrComplete {
		t.Fatal("current committed generation has neither a daemon-owned retry nor a completed correction")
	}
}

var _ PluginController = (*cancelingApplyController)(nil)
