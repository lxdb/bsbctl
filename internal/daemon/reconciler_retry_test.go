package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUnrelatedMutationPreservesSecretPendingRetryState(t *testing.T) {
	document := serviceDocument(true)
	ball8 := document.Apps["ball8"]
	ball8.Secrets = map[string]string{"token": "keychain://bsbctl/ball8/token"}
	document.Apps[ball8.ID] = ball8
	document.Apps["unrelated"] = config.App{
		ID: "unrelated", PluginID: "plugin", Enabled: false, Config: json.RawMessage(`{}`),
		Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}},
	}
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	service := newTestReconcilerWithRetryDelay(t, store, SecretResolverFunc(func(context.Context, string) (string, error) {
		return "", errors.New("secret unavailable")
	}), &safePluginController{}, func(int) time.Duration { return time.Hour })
	t.Cleanup(func() { _ = service.Close(t.Context()) })
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	service.live.mu.RLock()
	before := service.live.appReadiness["ball8"]
	service.live.mu.RUnlock()
	if before.Phase != AppSecretPending || before.Attempt == 0 || before.RetryAt.IsZero() {
		t.Fatalf("initial readiness = %#v", before)
	}
	if _, err := service.SetEnabled(t.Context(), "unrelated", true); err != nil {
		t.Fatal(err)
	}
	service.live.mu.RLock()
	after := service.live.appReadiness["ball8"]
	service.live.mu.RUnlock()
	if after != before {
		t.Fatalf("unrelated mutation changed retry state: before=%#v after=%#v", before, after)
	}
}

func TestActiveReconciliationDefersAllRetryKindsUntilCompletion(t *testing.T) {
	due := time.Now().Add(-time.Second)
	for _, phase := range []AppReadinessPhase{AppSecretPending, AppReconcilePending} {
		t.Run(string(phase), func(t *testing.T) {
			live := NewLiveState()
			live.loaded, live.activeAttempt = true, 7
			live.appReadiness = map[string]AppReadiness{
				"pending": {AppID: "pending", Phase: phase, RetryAt: due, LastErrorCode: reconcileFailedCode},
			}
			service := &Reconciler{live: live}
			if at, ready := service.nextRetry(); ready {
				t.Fatalf("active reconciliation scheduled an immediate retry at %v", at)
			}
			// A timer that fired before admission must also respect the current owner.
			service.runDueRetry()
			live.activeAttempt = 0
			if at, ready := service.nextRetry(); !ready || !at.Equal(due) {
				t.Fatalf("completion lost pending retry: %v, %v", at, ready)
			}
		})
	}
}

func TestRetryAdmissionCannotCancelAnAttemptStartedAfterScheduling(t *testing.T) {
	service := newTestReconciler(t, config.NewStore(filepath.Join(t.TempDir(), "config.json")), nil, &safePluginController{})
	ctx, id, finish, admitted := service.beginAttempt(t.Context(), 0, true)
	if !admitted {
		t.Fatal("initial reconciliation was not admitted")
	}
	defer finish()
	err := service.resolveAndApply(t.Context(), 0, config.Document{}, nil, nil, nil, false)
	if !errors.Is(err, context.Canceled) || ctx.Err() != nil {
		t.Fatalf("retry displaced active attempt %d: retry=%v active=%v", id, err, ctx.Err())
	}
}

func TestReconcilerLoadKeepsReadyAppRunningWhenAnotherSecretIsPending(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := twoSecretAppsDocument()
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	resolver := SecretResolverFunc(func(_ context.Context, reference string) (string, error) {
		if strings.Contains(reference, "pending") {
			return "", errors.New("account customer@example.test token-secret at secret.invalid")
		}
		return "ready-value", nil
	})
	plugins := &safePluginController{}
	service := newTestReconcilerWithRetryDelay(t, store, resolver, plugins, func(int) time.Duration { return time.Hour })
	t.Cleanup(func() { _ = service.Close(context.Background()) })

	if err := service.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	specs := plugins.specs()
	if len(specs) != 1 || len(specs[0].Instances) != 1 || specs[0].Instances[0].ID != "ready" {
		t.Fatalf("specs = %#v, want only ready instance", specs)
	}
	status := readinessByID(service.AppReadiness())
	if status["ready"].Phase != AppReady || status["pending"].Phase != AppSecretPending || status["pending"].Attempt != 1 {
		t.Fatalf("readiness = %#v", status)
	}
	encoded, err := json.Marshal(status["pending"])
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{"customer@example.test", "token-secret", "secret.invalid", "keychain://"} {
		if strings.Contains(string(encoded), sensitive) {
			t.Fatalf("status leaked %q: %s", sensitive, encoded)
		}
	}
}

func TestReconcilerEnablePersistsSecretPendingAndRetryRecoveryReconciles(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(false)
	app := document.Apps["ball8"]
	app.Secrets = map[string]string{"token": "keychain://bsbctl/pending/token"}
	document.Apps["ball8"] = app
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	attempts := 0
	resolver := SecretResolverFunc(func(context.Context, string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 1 {
			return "", errors.New("raw resolver failure with token-secret")
		}
		return "resolved-value", nil
	})
	plugins := &safePluginController{applyWake: make(chan struct{}, 8)}
	service := newTestReconcilerWithRetryDelay(t, store, resolver, plugins, func(int) time.Duration { return 5 * time.Millisecond })
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	updated, err := service.SetEnabled(context.Background(), "ball8", true)
	if err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if !updated.Apps["ball8"].Enabled || updated.Generation != 2 {
		t.Fatalf("updated = %#v", updated)
	}
	persisted, err := store.Load()
	if err != nil || !persisted.Apps["ball8"].Enabled || persisted.Generation != 2 {
		t.Fatalf("persisted = %#v, %v", persisted, err)
	}
	awaitCondition(t, time.Second, func() bool {
		values := plugins.specs()
		return len(values) == 1 && len(values[0].Instances) == 1
	}, "secret retry recovery")
	status := readinessByID(service.AppReadiness())["ball8"]
	if status.Phase != AppReady || status.Attempt != 0 || !status.RetryAt.IsZero() || status.LastErrorCode != "" {
		t.Fatalf("recovered readiness = %#v", status)
	}
}

func TestReconcilerDisableDuringBackoffCancelsRetry(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	app := document.Apps["ball8"]
	app.Secrets = map[string]string{"token": "keychain://bsbctl/pending/token"}
	document.Apps["ball8"] = app
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	attempts := 0
	resolver := SecretResolverFunc(func(context.Context, string) (string, error) {
		mu.Lock()
		attempts++
		mu.Unlock()
		return "", errors.New("unavailable")
	})
	service := newTestReconcilerWithRetryDelay(t, store, resolver, &safePluginController{}, func(int) time.Duration { return time.Hour })
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetEnabled(context.Background(), "ball8", false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotAttempts := attempts
	mu.Unlock()
	if gotAttempts != 1 {
		t.Fatalf("resolver attempts = %d, want 1", gotAttempts)
	}
	if got := readinessByID(service.AppReadiness())["ball8"]; got.Phase != AppDisabled {
		t.Fatalf("readiness = %#v", got)
	}
}

func TestFailedCorrectiveApplyRetriesExactAcceptedCandidate(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(false)
	app := document.Apps["ball8"]
	app.Secrets = map[string]string{"token": "keychain://bsbctl/ball8/token"}
	document.Apps["ball8"] = app
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	resolveCalls := 0
	resolver := SecretResolverFunc(func(context.Context, string) (string, error) {
		mu.Lock()
		resolveCalls++
		mu.Unlock()
		return "value", nil
	})
	plugins := newFailingCorrectionController()
	service := newTestReconcilerWithRetryDelay(t, store, resolver, plugins, func(int) time.Duration { return time.Hour })
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	enableDone := make(chan error, 1)
	go func() { _, err := service.SetEnabled(context.Background(), "ball8", true); enableDone <- err }()
	awaitServiceSignal(t, plugins.oldStarted, "old blocking apply")
	disableDone := make(chan error, 1)
	go func() { _, err := service.SetEnabled(context.Background(), "ball8", false); disableDone <- err }()
	select {
	case err := <-disableDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("disable blocked")
	}
	close(plugins.oldRelease)
	select {
	case <-enableDone:
	case <-time.After(time.Second):
		t.Fatal("stale enable did not unwind")
	}
	awaitCondition(t, time.Second, func() bool { return plugins.calls() >= 4 }, "failed first correction")
	if got := plugins.calls(); got != 4 {
		t.Fatalf("correction busy-looped: calls=%d", got)
	}
	service.live.mu.Lock()
	if service.live.correctionAttempt != 1 || service.live.correctionRevision == 0 || !service.live.correctionRetryAt.After(time.Now()) {
		state := []any{service.live.correctionRevision, service.live.correctionAttempt, service.live.correctionRetryAt}
		service.live.mu.Unlock()
		t.Fatalf("correction state = %#v", state)
	}
	service.live.correctionRetryAt = time.Now()
	service.live.mu.Unlock()
	service.wakeSecretRetry()
	awaitCondition(t, time.Second, func() bool { return plugins.calls() >= 5 }, "second correction success")
	if specs := plugins.specs(); len(specs) != 0 {
		t.Fatalf("final manager specs = %#v, want latest disabled plan", specs)
	}
	mu.Lock()
	gotResolveCalls := resolveCalls
	mu.Unlock()
	if gotResolveCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1", gotResolveCalls)
	}
	if _, ok := service.Generation("plugin", "ball8"); ok {
		t.Fatal("disabled generation admitted")
	}
	if got := readinessByID(service.AppReadiness())["ball8"]; got.Phase != AppDisabled {
		t.Fatalf("readiness changed during correction: %#v", got)
	}
	service.live.mu.RLock()
	defer service.live.mu.RUnlock()
	if service.live.correctionRevision != 0 || service.live.correctionAttempt != 0 || !service.live.correctionRetryAt.IsZero() {
		t.Fatalf("correction state was not cleared")
	}
}

func TestSecretRetryDelayUsesExponentialLadderAndCap(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for index, expected := range want {
		if got := secretRetryDelay(index+1, 1); got != expected {
			t.Fatalf("attempt %d delay = %s, want %s", index+1, got, expected)
		}
	}
	if got := secretRetryDelay(6, .8); got != 24*time.Second {
		t.Fatalf("low jitter delay = %s", got)
	}
	if got := secretRetryDelay(6, 1.2); got != 30*time.Second {
		t.Fatalf("high jitter delay = %s, want hard cap 30s", got)
	}
}

func TestSecretRetryDelayHandlesExtremeAttemptsAndJitter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		attempt int
		jitter  float64
		want    time.Duration
	}{
		{name: "maximum attempt", attempt: math.MaxInt, jitter: 1, want: 30 * time.Second},
		{name: "minimum attempt", attempt: math.MinInt, jitter: 1, want: time.Second},
		{name: "negative infinity", attempt: 5, jitter: math.Inf(-1), want: 12_800 * time.Millisecond},
		{name: "positive infinity", attempt: 5, jitter: math.Inf(1), want: 19_200 * time.Millisecond},
		{name: "nan", attempt: 5, jitter: math.NaN(), want: 16 * time.Second},
		{name: "absolute cap", attempt: math.MaxInt, jitter: math.Inf(1), want: 30 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := secretRetryDelay(test.attempt, test.jitter); got != test.want {
				t.Fatalf("secretRetryDelay(%d, %v) = %v, want %v", test.attempt, test.jitter, got, test.want)
			}
		})
	}
}

func TestBlockedSecretRetryCannotBlockDisableOrInstallStaleResult(t *testing.T) {
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
	var mu sync.Mutex
	calls := 0
	resolver := SecretResolverFunc(func(ctx context.Context, _ string) (string, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			return "", errors.New("pending")
		}
		started <- struct{}{}
		<-release // deliberately ignore cancellation and return a stale success
		return "stale-value", nil
	})
	plugins := &safePluginController{}
	service := newTestReconcilerWithRetryDelay(t, store, resolver, plugins, func(int) time.Duration { return time.Millisecond }, &fakeAssetController{ready: true})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitServiceSignal(t, started, "blocked secret retry")
	assetsDone := make(chan error, 1)
	go func() { assetsDone <- service.ReconcileAssets(context.Background()) }()
	select {
	case err := <-assetsDone:
		if err != nil {
			t.Fatalf("ReconcileAssets: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		close(release)
		t.Fatal("asset reconciliation blocked behind secret resolver")
	}
	disabled := make(chan error, 1)
	go func() { _, err := service.SetEnabled(context.Background(), "ball8", false); disabled <- err }()
	select {
	case err := <-disabled:
		if err != nil {
			t.Fatalf("disable: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		close(release)
		t.Fatal("disable blocked behind secret resolver")
	}
	close(release)
	awaitCondition(t, time.Second, func() bool { return readinessByID(service.AppReadiness())["ball8"].Phase == AppDisabled }, "disabled readiness")
	if _, ok := service.Generation("plugin", "ball8"); ok {
		t.Fatal("stale generation admitted")
	}
	if specs := plugins.specs(); len(specs) != 0 {
		t.Fatalf("stale specs applied: %#v", specs)
	}
}

func TestSetEnabledApplyFailureRetriesExactResolvedPlanWithoutResolvingAgain(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(false)
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
		resolveCalls++
		mu.Unlock()
		return "value", nil
	})
	plugins := newScriptedPluginController(nil, errors.New("apply failed"), nil)
	service := newTestReconcilerWithRetryDelay(t, store, resolver, plugins, func(int) time.Duration { return time.Hour })
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	operation, err := service.SetEnabled(context.Background(), "ball8", true)
	if err != nil || operation.ReconciliationError == nil {
		t.Fatalf("SetEnabled errors = %v / %v", err, operation.ReconciliationError)
	}
	status := readinessByID(service.AppReadiness())["ball8"]
	if status.Phase != AppReconcilePending || status.LastErrorCode != reconcileFailedCode {
		t.Fatalf("status = %#v", status)
	}
	if _, ok := service.Generation("plugin", "ball8"); ok {
		t.Fatal("failed plan generation admitted")
	}
	service.live.mu.Lock()
	status = service.live.appReadiness["ball8"]
	status.RetryAt = time.Now().Add(-time.Nanosecond)
	service.live.appReadiness["ball8"] = status
	service.live.mu.Unlock()
	service.wakeSecretRetry()
	awaitCondition(t, time.Second, func() bool { generation, ok := service.Generation("plugin", "ball8"); return ok && generation == 2 }, "apply retry acceptance")
	mu.Lock()
	gotCalls := resolveCalls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1", gotCalls)
	}
	if got := readinessByID(service.AppReadiness())["ball8"]; got.Phase != AppReady {
		t.Fatalf("recovered status = %#v", got)
	}
}

func TestBackgroundResolvedPlanApplyFailureRetriesWithoutResolvingAgain(t *testing.T) {
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
	plugins := newScriptedPluginController(nil, errors.New("background apply failed"), nil)
	service := newTestReconcilerWithRetryDelay(t, store, resolver, plugins, func(int) time.Duration { return time.Millisecond })
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitCondition(t, time.Second, func() bool { _, ok := service.Generation("plugin", "ball8"); return ok }, "background apply recovery")
	mu.Lock()
	gotCalls := resolveCalls
	mu.Unlock()
	if gotCalls != 2 {
		t.Fatalf("resolver calls = %d, want initial failure plus one recovery", gotCalls)
	}
}

func TestLoadApplyFailureLeavesRetryableDaemonState(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := newScriptedPluginController(errors.New("apply failed"), nil)
	service := newTestReconcilerWithRetryDelay(t, store, nil, plugins, func(int) time.Duration { return time.Millisecond })
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatalf("Load should remain available: %v", err)
	}
	if got := readinessByID(service.AppReadiness())["ball8"]; got.Phase != AppReconcilePending {
		t.Fatalf("status = %#v", got)
	}
	awaitCondition(t, time.Second, func() bool { _, ok := service.Generation("plugin", "ball8"); return ok }, "load apply retry")
}

func TestReconcilerMissingResolverMarksOnlySecretAppPending(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	app := document.Apps["ball8"]
	app.Secrets = map[string]string{"token": "keychain://bsbctl/pending/token"}
	document.Apps["ball8"] = app
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	service := newTestReconcilerWithRetryDelay(t, store, nil, &safePluginController{}, func(int) time.Duration { return time.Hour })
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatalf("Load without resolver: %v", err)
	}
	if got := readinessByID(service.AppReadiness())["ball8"]; got.Phase != AppSecretPending || got.LastErrorCode != secretUnavailableCode {
		t.Fatalf("readiness = %#v", got)
	}
}

func TestSecretRetryResolvesOnlyDuePendingApp(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, twoSecretAppsDocument()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	counts := map[string]int{}
	pendingRetried := make(chan struct{}, 1)
	resolver := SecretResolverFunc(func(_ context.Context, reference string) (string, error) {
		mu.Lock()
		counts[reference]++
		count := counts[reference]
		mu.Unlock()
		if strings.Contains(reference, "/pending/") && count == 2 {
			pendingRetried <- struct{}{}
		}
		return "", errors.New("unavailable")
	})
	delayCall := 0
	service := newTestReconcilerWithRetryDelay(t, store, resolver, &safePluginController{}, func(int) time.Duration {
		delayCall++
		if delayCall == 1 {
			return 5 * time.Millisecond
		}
		return time.Hour
	})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pendingRetried:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first pending app retry")
	}
	mu.Lock()
	pendingCalls := counts["keychain://bsbctl/pending/token"]
	readyCalls := counts["keychain://bsbctl/ready/token"]
	mu.Unlock()
	if pendingCalls != 2 || readyCalls != 1 {
		t.Fatalf("resolver calls pending=%d ready=%d, want 2 and 1", pendingCalls, readyCalls)
	}
}

type failingCorrectionController struct{ *blockingApplyController }

func newFailingCorrectionController() *failingCorrectionController {
	return &failingCorrectionController{blockingApplyController: newBlockingApplyController()}
}

func (f *failingCorrectionController) Apply(ctx context.Context, specs []pluginhost.Spec) error {
	f.muCalls.Lock()
	f.callCount++
	call := f.callCount
	f.muCalls.Unlock()
	if call == 2 {
		f.oldStarted <- struct{}{}
		<-f.oldRelease
	}
	if call == 4 {
		return errors.New("transient corrective apply failure")
	}
	return f.safePluginController.Apply(ctx, specs)
}

func twoSecretAppsDocument() config.Document {
	document := serviceDocument(true)
	plugin := document.Plugins["plugin"]
	plugin.ExecutionModes = []protocol.ExecutionMode{protocol.ExecutionModeResident}
	plugin.Channels = []protocol.Channel{{ID: "answer"}}
	document.Plugins["plugin"] = plugin
	ready := document.Apps["ball8"]
	delete(document.Apps, "ball8")
	ready.ID = "ready"
	ready.Secrets = map[string]string{"token": "keychain://bsbctl/ready/token"}
	pending := ready
	pending.ID = "pending"
	pending.Secrets = map[string]string{"token": "keychain://bsbctl/pending/token"}
	document.Apps["ready"] = ready
	document.Apps["pending"] = pending
	return document
}
