package daemon

import (
	"context"
	"errors"
	"github.com/lxdb/bsbctl/internal/config"
	"path/filepath"
	"testing"
	"time"
)

func TestReconcilerStartsSecretRetryOwnershipOnlyAfterSuccessfulLoad(t *testing.T) {
	plugins := &safePluginController{}
	service := newTestReconciler(t, config.NewStore(filepath.Join(t.TempDir(), "missing.json")), nil, plugins)
	service.live.mu.RLock()
	startedBeforeLoad := service.retryStarted
	service.live.mu.RUnlock()
	if startedBeforeLoad {
		t.Fatal("NewReconciler started secret retry ownership")
	}
	if err := service.Load(context.Background()); err == nil {
		t.Fatal("Load accepted missing configuration")
	}
	service.live.mu.RLock()
	startedAfterFailure := service.retryStarted
	service.live.mu.RUnlock()
	if startedAfterFailure {
		t.Fatal("failed Load started secret retry ownership")
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatalf("Close before successful Load: %v", err)
	}
}

func TestCloseUsesInternalLiveShutdownContext(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(false)); err != nil {
		t.Fatal(err)
	}
	plugins := newContextCheckingCloseController()
	service := newTestReconciler(t, store, nil, plugins)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Close(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Close = %v", err)
	}
	select {
	case live := <-plugins.liveContext:
		if !live {
			t.Fatal("plugins.Close received the canceled waiter context")
		}
	case <-time.After(time.Second):
		t.Fatal("plugins.Close was not started")
	}
	close(plugins.closeRelease)
	if err := service.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestCloseCallsPluginsWhenResolverIgnoresCancellation(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	app := document.Apps["ball8"]
	app.Secrets = map[string]string{"token": "keychain://bsbctl/pending/token"}
	document.Apps["ball8"] = app
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	resolver := SecretResolverFunc(func(context.Context, string) (string, error) { started <- struct{}{}; <-release; return "value", nil })
	plugins := &safePluginController{}
	service := newTestReconciler(t, store, resolver, plugins)
	loadDone := make(chan error, 1)
	go func() { loadDone <- service.Load(context.Background()) }()
	awaitServiceSignal(t, started, "blocked load resolver")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := service.Close(ctx)
	cancel()
	if err == nil {
		t.Fatal("Close unexpectedly joined blocked resolver")
	}
	if !plugins.closed() {
		t.Fatal("plugins.Close was not invoked before Close returned")
	}
	close(release)
	select {
	case <-loadDone:
	case <-time.After(time.Second):
		t.Fatal("Load did not unwind")
	}
	if err := service.Close(context.Background()); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repeat Close: %v", err)
	}
}

func TestReconcilerCloseCancelsPendingSecretRetry(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	app := document.Apps["ball8"]
	app.Secrets = map[string]string{"token": "keychain://bsbctl/pending/token"}
	document.Apps["ball8"] = app
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	resolver := SecretResolverFunc(func(context.Context, string) (string, error) { return "", errors.New("unavailable") })
	plugins := &safePluginController{}
	service := newTestReconcilerWithRetryDelay(t, store, resolver, plugins, func(int) time.Duration { return time.Hour })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !plugins.closed() {
		t.Fatal("plugin controller was not closed")
	}
}

type contextCheckingCloseController struct {
	*safePluginController
	liveContext  chan bool
	closeRelease chan struct{}
}

func newContextCheckingCloseController() *contextCheckingCloseController {
	return &contextCheckingCloseController{safePluginController: &safePluginController{}, liveContext: make(chan bool, 1), closeRelease: make(chan struct{})}
}

func (f *contextCheckingCloseController) Close(ctx context.Context) error {
	f.liveContext <- ctx.Err() == nil
	select {
	case <-f.closeRelease:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
