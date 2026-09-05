package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/observation"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestReconcilerDoesNotStartEnabledPluginUntilAssetsAreReady(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(false)); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	assetsController := &fakeAssetController{}
	service := newTestReconciler(t, store, nil, plugins, assetsController)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetEnabled(context.Background(), "ball8", true); err != nil {
		t.Fatal(err)
	}
	if len(plugins.lastSpecs) != 0 {
		t.Fatalf("plugin started while assets pending: %#v", plugins.lastSpecs)
	}
	assetsController.ready = true
	if err := service.ReconcileAssets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(plugins.lastSpecs) != 1 {
		t.Fatalf("plugin not started after assets ready: %#v", plugins.lastSpecs)
	}
}

func TestReconcilerDisableOrdersPluginAttentionThenAssets(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	log := &orderedServiceLog{}
	plugins := &orderedPluginController{fakePluginController: &fakePluginController{}, log: log}
	assetController := &orderedAssetController{ready: true, log: log}
	service := newTestReconcilerWithAttention(t, store, nil, plugins, &orderedAttention{log: log}, assetController)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	log.reset()
	if _, err := service.SetEnabled(context.Background(), "ball8", false); err != nil {
		t.Fatal(err)
	}
	if got, want := log.snapshot(), []string{"plugin", "attention", "assets"}; !equalServiceOperations(got, want) {
		t.Fatalf("disable operations = %v, want %v", got, want)
	}
}

func TestReconcilerReconnectQuiescesPackagePluginBeforeAssetReadback(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	packageRoot := t.TempDir()
	content := []byte("asset")
	if err := os.WriteFile(filepath.Join(packageRoot, "icon.png"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	plugin := document.Plugins["plugin"]
	plugin.PackageRoot = packageRoot
	plugin.Assets = []assets.Declaration{{
		Source: "icon.png", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content)), MediaType: "image/png",
	}}
	document.Plugins["plugin"] = plugin
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	log := &orderedServiceLog{}
	plugins := &orderedPluginController{fakePluginController: &fakePluginController{}, log: log}
	assetController := &orderedAssetController{ready: true, log: log}
	service := newTestReconcilerWithAttention(t, store, nil, plugins, &orderedAttention{log: log}, assetController)
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	log.reset()
	assetController.ready = false
	if err := service.ReconcileAssets(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := log.snapshot()
	wantPrefix := []string{"plugin", "attention", "assets"}
	if len(got) < len(wantPrefix) || !equalServiceOperations(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("reconnect operations = %v, want prefix %v", got, wantPrefix)
	}
}

type upgradeAssetController struct {
	*orderedAssetController
	readyAfterReconcile bool
}

func (c *upgradeAssetController) Reconcile(ctx context.Context, packages []assets.Package) {
	c.orderedAssetController.Reconcile(ctx, packages)
	if c.readyAfterReconcile {
		c.ready = true
	}
}

func (c *upgradeAssetController) CollectGarbage(context.Context, []assets.Package) {
	c.log.add("asset-gc")
}

func TestReconcilerPackageUpgradeQuiescesPluginBeforeNewAssetReconciliation(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	packageRoot := t.TempDir()
	content := []byte("old asset")
	if err := os.WriteFile(filepath.Join(packageRoot, "icon.png"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	plugin := document.Plugins["plugin"]
	plugin.PackageRoot = packageRoot
	plugin.Assets = []assets.Declaration{{
		Source: "icon.png", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content)), MediaType: "image/png",
	}}
	document.Plugins["plugin"] = plugin
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}

	log := &orderedServiceLog{}
	plugins := &orderedPluginController{fakePluginController: &fakePluginController{}, log: log}
	assetController := &upgradeAssetController{orderedAssetController: &orderedAssetController{ready: true, log: log}}
	service := newTestReconcilerWithAttention(t, store, nil, plugins, &orderedAttention{log: log}, assetController)
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

	log.reset()
	assetController.ready = false
	assetController.readyAfterReconcile = true
	upgraded := plugin
	upgraded.Version = "2"
	if outcome, err := service.ActivatePlugin(t.Context(), upgraded); err != nil || !outcome.IsCommitted() {
		t.Fatalf("ActivatePlugin = %q, %v", outcome, err)
	}
	got := log.snapshot()
	wantPrefix := []string{"plugin", "attention", "assets", "plugin", "attention", "assets", "asset-gc"}
	if len(got) < len(wantPrefix) || !equalServiceOperations(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("package upgrade operations = %v, want prefix %v", got, wantPrefix)
	}
}

func TestReconcilerNewEnableCancelsStaleDisableBeforeAssetRemoval(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	assets := &enabledRecordingAssets{ready: true}
	attention := &cancelableAttention{started: make(chan struct{})}
	service := newTestReconcilerWithAttention(t, store, nil, plugins, attention, assets)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	attention.arm()
	assets.reset()
	disabled := make(chan error, 1)
	go func() { _, err := service.SetEnabled(context.Background(), "ball8", false); disabled <- err }()
	<-attention.started
	if _, err := service.SetEnabled(context.Background(), "ball8", true); err != nil {
		t.Fatal(err)
	}
	<-disabled
	for _, enabled := range assets.snapshot() {
		if !enabled {
			t.Fatalf("stale disable reconciled removed assets: %v", assets.snapshot())
		}
	}
}

type orderedAssetController struct {
	ready bool
	log   *orderedServiceLog
}

func (c *orderedAssetController) Reconcile(context.Context, []assets.Package) { c.log.add("assets") }

func (c *orderedAssetController) Ready(string) bool { return c.ready }

func (c *orderedAssetController) ReadyFor(assets.Package) bool { return c.ready }

func (*orderedAssetController) Status() []assets.State { return nil }

func (*orderedAssetController) CollectGarbage(context.Context, []assets.Package) {}

type orderedAttention struct{ log *orderedServiceLog }

func (*orderedAttention) SelectedObservation() (observation.Record, bool) {
	return observation.Record{}, false
}

func (a *orderedAttention) Reconcile(context.Context) error { a.log.add("attention"); return nil }

func (*orderedAttention) AttentionSnapshot() (attention.Trace, bool) { return attention.Trace{}, false }

func (*orderedAttention) AttentionExplain(string) (attention.Evaluation, bool) {
	return attention.Evaluation{}, false
}

func (*orderedAttention) AttentionHistory(int, time.Time) []attention.Trace { return nil }

func (*orderedAttention) AcknowledgeAttention(string) error { return nil }

func (*orderedAttention) Wake() {}

func (*orderedAttention) RecorderStatus() attention.RecorderStatus {
	return attention.RecorderStatus{Phase: attention.RecorderUnavailable}
}

func (*orderedAttention) ObservationDiagnostics() observation.StoreDiagnostics {
	return observation.StoreDiagnostics{}
}

func (*orderedAttention) AttentionStateStatus() AttentionStateDiagnostics {
	return AttentionStateDiagnostics{}
}

func (*orderedAttention) PresentationCooldownStatus() PresentationCooldownDiagnostics {
	return PresentationCooldownDiagnostics{}
}

type enabledRecordingAssets struct {
	mu      sync.Mutex
	ready   bool
	enabled []bool
}

func (a *enabledRecordingAssets) Reconcile(_ context.Context, packages []assets.Package) {
	a.mu.Lock()
	for _, value := range packages {
		a.enabled = append(a.enabled, value.Enabled)
	}
	a.mu.Unlock()
}

func (a *enabledRecordingAssets) Ready(string) bool { return a.ready }

func (a *enabledRecordingAssets) ReadyFor(assets.Package) bool { return a.ready }

func (*enabledRecordingAssets) Status() []assets.State { return nil }

func (*enabledRecordingAssets) CollectGarbage(context.Context, []assets.Package) {}

func (a *enabledRecordingAssets) reset() { a.mu.Lock(); a.enabled = nil; a.mu.Unlock() }

func (a *enabledRecordingAssets) snapshot() []bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]bool(nil), a.enabled...)
}

type cancelableAttention struct {
	mu      sync.Mutex
	armed   bool
	once    sync.Once
	started chan struct{}
}

func (*cancelableAttention) SelectedObservation() (observation.Record, bool) {
	return observation.Record{}, false
}

func (a *cancelableAttention) arm() {
	a.mu.Lock()
	a.armed = true
	a.mu.Unlock()
}

func (a *cancelableAttention) Reconcile(ctx context.Context) error {
	a.mu.Lock()
	armed := a.armed
	a.mu.Unlock()
	if !armed {
		return nil
	}
	first := false
	a.once.Do(func() { close(a.started); first = true })
	if first {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (*cancelableAttention) AttentionSnapshot() (attention.Trace, bool) {
	return attention.Trace{}, false
}

func (*cancelableAttention) AttentionExplain(string) (attention.Evaluation, bool) {
	return attention.Evaluation{}, false
}

func (*cancelableAttention) AttentionHistory(int, time.Time) []attention.Trace { return nil }

func (*cancelableAttention) AcknowledgeAttention(string) error { return nil }

func (*cancelableAttention) Wake() {}

func (*cancelableAttention) RecorderStatus() attention.RecorderStatus {
	return attention.RecorderStatus{Phase: attention.RecorderUnavailable}
}

func (*cancelableAttention) ObservationDiagnostics() observation.StoreDiagnostics {
	return observation.StoreDiagnostics{}
}

func (*cancelableAttention) AttentionStateStatus() AttentionStateDiagnostics {
	return AttentionStateDiagnostics{}
}

func (*cancelableAttention) PresentationCooldownStatus() PresentationCooldownDiagnostics {
	return PresentationCooldownDiagnostics{}
}

func TestAssetReconcileCannotApplyOlderSameEpochCandidate(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	app := document.Apps["ball8"]
	app.Secrets = map[string]string{"token": "keychain://bsbctl/pending/token"}
	document.Apps["ball8"] = app
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	resolveCalls := 0
	resolver := SecretResolverFunc(func(context.Context, string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		resolveCalls++
		if resolveCalls == 1 {
			return "", errors.New("pending")
		}
		return "value", nil
	})
	plugins := &safePluginController{}
	assets := newBlockingAssetController()
	service := newTestReconcilerWithRetryDelay(t, store, resolver, plugins, func(int) time.Duration { return time.Hour }, assets)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	assets.blockNext()
	assetsDone := make(chan error, 1)
	go func() { assetsDone <- service.ReconcileAssets(context.Background()) }()
	awaitServiceSignal(t, assets.started, "blocked asset reconciliation")
	service.live.mu.Lock()
	status := service.live.appReadiness["ball8"]
	status.RetryAt = time.Now()
	service.live.appReadiness["ball8"] = status
	service.live.mu.Unlock()
	service.wakeSecretRetry()
	awaitCondition(t, time.Second, func() bool { _, ok := service.Generation("plugin", "ball8"); return ok }, "newer secret candidate acceptance")
	close(assets.release)
	if err := <-assetsDone; err != nil {
		t.Fatalf("ReconcileAssets: %v", err)
	}
	awaitCondition(t, time.Second, func() bool { specs := plugins.specs(); return len(specs) == 1 && len(specs[0].Instances) == 1 }, "current candidate after asset reconcile")
	if generation, ok := service.Generation("plugin", "ball8"); !ok || generation != 1 {
		t.Fatalf("generation = %d,%v", generation, ok)
	}
	if got := readinessByID(service.AppReadiness())["ball8"]; got.Phase != AppReady {
		t.Fatalf("readiness regressed: %#v", got)
	}
}

type blockingAssetController struct {
	mu      sync.Mutex
	block   bool
	started chan struct{}
	release chan struct{}
}

func newBlockingAssetController() *blockingAssetController {
	return &blockingAssetController{started: make(chan struct{}, 1), release: make(chan struct{})}
}

func (f *blockingAssetController) blockNext() { f.mu.Lock(); f.block = true; f.mu.Unlock() }

func (f *blockingAssetController) Reconcile(context.Context, []assets.Package) {
	f.mu.Lock()
	block := f.block
	f.block = false
	f.mu.Unlock()
	if block {
		f.started <- struct{}{}
		<-f.release
	}
}

func (*blockingAssetController) Ready(string) bool { return true }

func (*blockingAssetController) ReadyFor(assets.Package) bool { return true }

func (*blockingAssetController) Status() []assets.State { return nil }

func (*blockingAssetController) CollectGarbage(context.Context, []assets.Package) {}
