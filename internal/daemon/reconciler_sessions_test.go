package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/eventbus"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReconcilerLaunchRequiresEnabledApp(t *testing.T) {
	t.Parallel()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(false)); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, nil, plugins)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "ask", nil); err == nil {
		t.Fatal("Launch accepted disabled app")
	}
}

func TestReconcilerLaunchUsesConfiguredDefaultAction(t *testing.T) {
	t.Parallel()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, nil, plugins)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	if plugins.invoked.Action != "ask" {
		t.Fatalf("action = %q, want ask", plugins.invoked.Action)
	}
	if plugins.invoked.SessionToken == "" || plugins.invoked.SessionToken != string(plugins.invokedToken) || plugins.invoked.Trigger == nil || plugins.invoked.Trigger.Kind != protocol.SessionTriggerLauncher {
		t.Fatalf("launcher invocation identity = %#v / %q", plugins.invoked, plugins.invokedToken)
	}
}

func TestReconcilerLaunchableAppsRequireExplicitLauncherAction(t *testing.T) {
	t.Parallel()
	service := &Reconciler{
		live: &LiveState{
			generations: Generations{values: map[generationKey]uint64{
				{pluginID: "interactive", instanceID: "launchable"}:             7,
				{pluginID: "interactive", instanceID: "non-interactive-policy"}: 7,
				{pluginID: "interactive", instanceID: "no-action"}:              7,
				{pluginID: "resident", instanceID: "resident"}:                  7,
			}},
			document: config.Document{
				Plugins: map[string]config.Plugin{
					"interactive": {ID: "interactive", ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive}},
					"resident":    {ID: "resident", ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident}},
				},
				Apps: map[string]config.App{
					"launchable":             {ID: "launchable", PluginID: "interactive", Generation: 7, Enabled: true, LaunchAction: "open", Policies: map[string]presentation.PolicyConfig{"live": {Policy: presentation.PolicyInteractive}}},
					"non-interactive-policy": {ID: "non-interactive-policy", PluginID: "interactive", Generation: 7, Enabled: true, LaunchAction: "open", Policies: map[string]presentation.PolicyConfig{"live": {Policy: presentation.PolicyRotation}}},
					"not-ready":              {ID: "not-ready", PluginID: "interactive", Generation: 7, Enabled: true, LaunchAction: "open", Policies: map[string]presentation.PolicyConfig{"live": {Policy: presentation.PolicyInteractive}}},
					"stale":                  {ID: "stale", PluginID: "interactive", Generation: 6, Enabled: true, LaunchAction: "open", Policies: map[string]presentation.PolicyConfig{"live": {Policy: presentation.PolicyInteractive}}},
					"no-action":              {ID: "no-action", PluginID: "interactive", Generation: 7, Enabled: true, Policies: map[string]presentation.PolicyConfig{"live": {Policy: presentation.PolicyInteractive}}},
					"disabled":               {ID: "disabled", PluginID: "interactive", Generation: 7, LaunchAction: "open", Policies: map[string]presentation.PolicyConfig{"live": {Policy: presentation.PolicyInteractive}}},
					"resident":               {ID: "resident", PluginID: "resident", Generation: 7, Enabled: true, LaunchAction: "open", Policies: map[string]presentation.PolicyConfig{"live": {Policy: presentation.PolicyInteractive}}},
				},
			},
		},
	}

	if got := service.LaunchableApps(); !reflect.DeepEqual(got, []LaunchableApp{{ID: "launchable", PluginID: "interactive", Action: "open"}}) {
		t.Fatalf("launchable apps = %#v", got)
	}
}

func TestReconcilerLaunchesFirstPartyCalendarDefaultFromLauncher(t *testing.T) {
	descriptor, ok := firstpartyplugins.LookupAppID("calendar")
	if !ok {
		t.Fatal("Calendar descriptor not found")
	}
	definition := descriptor.DefinitionForVersion(descriptor.DevelopmentVersion)
	app := descriptor.DefaultApp
	app.Generation = 1
	document := config.Document{
		Version: config.CurrentVersion, Generation: 1,
		Plugins: map[string]config.Plugin{descriptor.ID: {
			ID: descriptor.ID, Version: definition.Version, Executable: "/bin/true", ProtocolVersion: protocol.Version,
			ExecutionModes: definition.Contract.ExecutionModes, Channels: definition.Contract.Channels, Operations: definition.Contract.Operations,
		}},
		Apps: map[string]config.App{app.ID: app},
	}
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, nil, plugins)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := service.LaunchableApps(); !reflect.DeepEqual(got, []LaunchableApp{{ID: app.ID, PluginID: descriptor.ID, Action: app.LaunchAction}}) {
		t.Fatalf("Calendar launcher catalog = %#v", got)
	}
	if err := service.Launch(t.Context(), app.ID, app.LaunchAction, nil); err != nil {
		t.Fatal(err)
	}
	if plugins.invoked.InstanceID != app.ID || plugins.invoked.Generation != app.Generation || plugins.invoked.Action != app.LaunchAction ||
		plugins.invoked.Trigger == nil || plugins.invoked.Trigger.Kind != protocol.SessionTriggerLauncher || plugins.invoked.Trigger.Observation != nil {
		t.Fatalf("Calendar launcher invocation = %#v", plugins.invoked)
	}
}

func TestReconcilerLaunchValidatesPayloadBeforeSessionAdmission(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload json.RawMessage
		wantErr bool
	}{
		{name: "object at limit", payload: daemonObjectOfSize(t, protocol.MaxJSONObjectBytes)},
		{name: "object over limit", payload: daemonObjectOfSize(t, protocol.MaxJSONObjectBytes+1), wantErr: true},
		{name: "scalar", payload: json.RawMessage(`"value"`), wantErr: true},
		{name: "array", payload: json.RawMessage(`[]`), wantErr: true},
		{name: "null", payload: json.RawMessage(`null`), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
			if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
				t.Fatal(err)
			}
			plugins := &fakePluginController{}
			service := newTestReconciler(t, store, nil, plugins)
			t.Cleanup(func() { _ = service.Close(t.Context()) })
			if err := service.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			err := service.Launch(t.Context(), "ball8", "ask", test.payload)
			if (err != nil) != test.wantErr {
				t.Fatalf("Launch error = %v, wantErr=%t", err, test.wantErr)
			}
			wantInvocations := 1
			if test.wantErr {
				wantInvocations = 0
				domain, ok := errors.AsType[*protocol.DomainError](err)
				if !ok || domain.Kind() != protocol.ErrorInvalidArgument {
					t.Fatalf("Launch error = %#v, want invalid_argument", err)
				}
				if service.Foreground() != "" {
					t.Fatalf("invalid launch admitted foreground %q", service.Foreground())
				}
			}
			if plugins.invocations != wantInvocations {
				t.Fatalf("plugin invocations = %d, want %d", plugins.invocations, wantInvocations)
			}
		})
	}
}

func TestReconcilerActivatesExactSelectedObservationAndSuppressesOnlyItsRevision(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	app := document.Apps["ball8"]
	app.Policies["answer"] = presentation.PolicyConfig{
		Policy: presentation.PolicyAttention, ActivationAction: "open",
	}
	document.Apps["ball8"] = app
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	selected := observation.Record{PluginID: "plugin", Generation: 1, Observation: protocol.Observation{
		Instance: protocol.InstanceRef{ID: "ball8", Generation: 1}, Channel: "answer", Key: "request.abc", Revision: 7,
	}}
	attention := &observationDiagnostics{selected: selected}
	service := newTestReconcilerWithAttention(t, store, nil, plugins, attention)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	activated, err := service.ActivateSelected(context.Background())
	if err != nil || !activated {
		t.Fatalf("ActivateSelected = %v, %v", activated, err)
	}
	request := plugins.invoked
	if request.Action != "open" || request.SessionToken != string(plugins.invokedToken) || request.Trigger == nil || request.Trigger.Kind != protocol.SessionTriggerObservation || request.Trigger.Observation == nil {
		t.Fatalf("observation invocation = %#v / token %q", request, plugins.invokedToken)
	}
	if trigger := request.Trigger.Observation; trigger.Channel != "answer" || trigger.Key != "request.abc" || trigger.Revision != 7 {
		t.Fatalf("observation trigger = %#v", trigger)
	}
	if rule, ok := service.AttentionRule(selected); ok && rule.Enabled {
		t.Fatalf("activated source revision remained eligible: %#v", rule)
	}
	newer := selected
	newer.Observation.Revision = 8
	if rule, ok := service.AttentionRule(newer); !ok || !rule.Enabled {
		t.Fatalf("newer source revision was suppressed: %#v/%v", rule, ok)
	}
	service.sessions.state.PluginSessionCompleted(service.live, "plugin", protocol.CompleteSessionRequest{
		Instance: protocol.InstanceRef{ID: "ball8", Generation: 1}, SessionToken: "stale",
	})
	if got := service.Foreground(); got != "ball8" {
		t.Fatalf("stale completion cleared foreground = %q", got)
	}
	service.sessions.state.PluginSessionCompleted(service.live, "plugin", protocol.CompleteSessionRequest{
		Instance: protocol.InstanceRef{ID: "ball8", Generation: 1}, SessionToken: request.SessionToken,
	})
	if rule, ok := service.AttentionRule(selected); !ok || !rule.Enabled {
		t.Fatalf("source did not return after exact session clear: %#v/%v", rule, ok)
	}
	if plugins.endedInstance != "" {
		t.Fatalf("plugin-completed session received redundant cleanup for %q", plugins.endedInstance)
	}
}

func TestReconcilerDoesNotActivateSelectedObservationWithoutDeclaredAction(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	selected := observation.Record{PluginID: "plugin", Generation: 1, Observation: protocol.Observation{
		Instance: protocol.InstanceRef{ID: "ball8", Generation: 1}, Channel: "answer", Key: "request.abc", Revision: 7,
	}}
	service := newTestReconcilerWithAttention(t, store, nil, plugins, &observationDiagnostics{selected: selected})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	activated, err := service.ActivateSelected(context.Background())
	if err != nil || activated || plugins.invoked.InstanceID != "" {
		t.Fatalf("ActivateSelected/invocation = %v, %v, %#v", activated, err, plugins.invoked)
	}
}

func TestPluginSessionCompletionCancelsTheExactPendingLaunch(t *testing.T) {
	t.Parallel()
	sessions, err := NewSessionRuntime(NewSessionCoordinator(nil), &safePluginController{}, &recordingSessionInputController{}, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	service := &Reconciler{
		sessions: sessions,
		live: &LiveState{
			loaded: true,
			document: config.Document{Apps: map[string]config.App{
				"calendar": {ID: "calendar", PluginID: "dev.bsbctl.calendar", Enabled: true},
			}},
			generations: Generations{values: map[generationKey]uint64{
				{pluginID: "dev.bsbctl.calendar", instanceID: "calendar"}: 1,
			}},
		},
	}
	service.sessions.state.pendingForeground = "calendar"
	service.sessions.state.pendingPlugin = "dev.bsbctl.calendar"
	service.sessions.state.pendingGeneration = 1
	service.sessions.state.pendingSession = "interactive-9"

	service.sessions.state.PluginSessionCompleted(service.live, "other", protocol.CompleteSessionRequest{
		Instance: protocol.InstanceRef{ID: "calendar", Generation: 1}, SessionToken: "interactive-9",
	})
	if service.sessions.state.pendingForeground == "" || service.sessions.state.pendingSession == "" {
		t.Fatal("another plugin cleared the pending Calendar session")
	}
	service.sessions.state.PluginSessionCompleted(service.live, "dev.bsbctl.calendar", protocol.CompleteSessionRequest{
		Instance: protocol.InstanceRef{ID: "calendar", Generation: 1}, SessionToken: "stale",
	})
	if service.sessions.state.pendingForeground == "" || service.sessions.state.pendingSession == "" {
		t.Fatal("a stale token cleared the pending Calendar session")
	}
	service.sessions.state.PluginSessionCompleted(service.live, "dev.bsbctl.calendar", protocol.CompleteSessionRequest{
		Instance: protocol.InstanceRef{ID: "calendar", Generation: 1}, SessionToken: "interactive-9",
	})
	if service.sessions.state.pendingForeground != "" || service.sessions.state.pendingSession != "" {
		t.Fatalf("pending session = %q/%q", service.sessions.state.pendingForeground, service.sessions.state.pendingSession)
	}
}

func TestSessionInvalidationCancelsExactPendingAdmission(t *testing.T) {
	owner := NewSessionCoordinator(nil)
	admission, admitted := owner.begin("dev.bsbctl.calendar", "calendar", 7)
	if !admitted {
		t.Fatal("initial admission was rejected")
	}
	if !owner.invalidate(pluginhost.SessionInvalidation{
		PluginID: "dev.bsbctl.calendar", InstanceID: "calendar", Generation: 7, Token: admission.token,
		Reason: pluginhost.SessionInvalidatedExit,
	}) {
		t.Fatal("exact pending invalidation was ignored")
	}
	if _, accepted := owner.promote(admission, "dev.bsbctl.calendar", nil); accepted {
		t.Fatal("invalidated pending admission was promoted")
	}

	unrelated, admitted := owner.begin("dev.bsbctl.calendar", "calendar", 8)
	if !admitted {
		t.Fatal("unrelated admission was rejected")
	}
	if owner.invalidate(pluginhost.SessionInvalidation{
		PluginID: "other", InstanceID: "calendar", Generation: 8, Token: unrelated.token,
		Reason: pluginhost.SessionInvalidatedExit,
	}) {
		t.Fatal("unrelated plugin invalidated pending admission")
	}
	if _, accepted := owner.promote(unrelated, "dev.bsbctl.calendar", nil); !accepted {
		t.Fatal("unrelated invalidation blocked pending admission")
	}
}

func TestUnrelatedForegroundCompletionDoesNotRejectPendingAdmission(t *testing.T) {
	owner := NewSessionCoordinator(nil)
	active, admitted := owner.begin("dev.bsbctl.calendar", "calendar", 1)
	if !admitted {
		t.Fatal("active admission was rejected")
	}
	if _, accepted := owner.promote(active, active.pluginID, nil); !accepted {
		t.Fatal("initial foreground was not promoted")
	}
	pending, admitted := owner.begin("dev.bsbctl.codex", "codex", 2)
	if !admitted {
		t.Fatal("pending admission was rejected")
	}
	foregroundChanged, changed := owner.complete(active.pluginID, active.instanceID, active.generation, active.token, false)
	if !foregroundChanged || !changed {
		t.Fatal("exact foreground completion was ignored")
	}
	if _, accepted := owner.promote(pending, pending.pluginID, nil); !accepted {
		t.Fatal("unrelated foreground completion rejected pending admission")
	}
}

func TestSessionOwnerCriticalPreemptionRespectsAtomicExecutionBoundary(t *testing.T) {
	owner := NewSessionCoordinator(nil)
	pending, admitted := owner.begin("plugin", "app", 7)
	if !admitted || owner.state() != foregroundPending {
		t.Fatalf("pending admission = %#v/%t state=%q", pending, admitted, owner.state())
	}
	if termination, acquired := owner.acquireCritical("critical-a"); !acquired || termination.pluginID != "" || owner.state() != foregroundCriticalOwned {
		t.Fatalf("pending preemption = %#v/%t state=%q", termination, acquired, owner.state())
	}
	if _, accepted := owner.promote(pending, pending.pluginID, nil); accepted {
		t.Fatal("critically canceled pending admission was promoted")
	}
	owner.releaseCritical()

	active, admitted := owner.begin("plugin", "app", 7)
	if !admitted {
		t.Fatal("interactive admission was rejected")
	}
	if _, promoted := owner.promote(active, active.pluginID, nil); !promoted {
		t.Fatal("interactive admission was not promoted")
	}
	request := protocol.SessionExecutionRequest{
		Instance:     protocol.InstanceRef{ID: active.instanceID, Generation: active.generation},
		SessionToken: string(active.token),
	}
	if err := owner.beginExecution(t.Context(), active.pluginID, request); err != nil || owner.state() != foregroundExecuting {
		t.Fatalf("begin execution = %v state=%q", err, owner.state())
	}
	if termination, acquired := owner.acquireCritical("critical-b"); acquired || termination.pluginID != "" || owner.state() != foregroundExecuting {
		t.Fatalf("atomic execution was preempted: %#v/%t state=%q", termination, acquired, owner.state())
	}
	if foregroundChanged, changed := owner.complete(active.pluginID, active.instanceID, active.generation, active.token, false); !foregroundChanged || !changed || owner.state() != foregroundIdle {
		t.Fatalf("execution completion = %t/%t state=%q", foregroundChanged, changed, owner.state())
	}
}

func TestSessionOwnerCriticalInvalidatesLauncherAdmission(t *testing.T) {
	owner := NewSessionCoordinator(nil)
	sequence, admitted := owner.beginLauncherAdmission()
	if !admitted {
		t.Fatal("idle launcher admission was rejected")
	}
	if _, acquired := owner.acquireCritical("critical"); !acquired {
		t.Fatal("critical ownership was rejected")
	}
	if owner.launcherAdmissionCurrent(sequence) {
		t.Fatal("critical acquisition left prior launcher admission current")
	}
	if _, admitted := owner.beginLauncherAdmission(); admitted {
		t.Fatal("launcher admission was accepted during critical ownership")
	}
	owner.releaseCritical()
	if owner.launcherAdmissionCurrent(sequence) {
		t.Fatal("released critical ownership revived prior launcher admission")
	}
	if _, admitted := owner.beginLauncherAdmission(); !admitted {
		t.Fatal("fresh launcher admission was rejected after critical release")
	}
}

func TestSessionOwnerLinearizesCriticalAgainstExecutionGrant(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		owner := NewSessionCoordinator(nil)
		admission, admitted := owner.begin("plugin", "app", 7)
		if !admitted {
			t.Fatal("session admission was rejected")
		}
		if _, promoted := owner.promote(admission, admission.pluginID, nil); !promoted {
			t.Fatal("session promotion failed")
		}
		request := protocol.SessionExecutionRequest{
			Instance:     protocol.InstanceRef{ID: admission.instanceID, Generation: admission.generation},
			SessionToken: string(admission.token),
		}
		start := make(chan struct{})
		grantResult := make(chan error, 1)
		criticalResult := make(chan bool, 1)
		go func() {
			<-start
			grantResult <- owner.beginExecution(t.Context(), admission.pluginID, request)
		}()
		go func() {
			<-start
			_, acquired := owner.acquireCritical("critical")
			criticalResult <- acquired
		}()
		close(start)
		grantErr, criticalAcquired := <-grantResult, <-criticalResult
		grantAcquired := grantErr == nil
		if grantAcquired == criticalAcquired {
			t.Fatalf("iteration %d grant=%t critical=%t err=%v state=%q", iteration, grantAcquired, criticalAcquired, grantErr, owner.state())
		}
		if grantAcquired && owner.state() != foregroundExecuting {
			t.Fatalf("iteration %d state=%q after grant", iteration, owner.state())
		}
		if criticalAcquired && owner.state() != foregroundCriticalOwned {
			t.Fatalf("iteration %d state=%q after critical", iteration, owner.state())
		}
	}
}

func TestSessionOwnerExecutionGrantCancelsUnrelatedPendingAdmission(t *testing.T) {
	owner := NewSessionCoordinator(nil)
	active, admitted := owner.begin("plugin-a", "app-a", 1)
	if !admitted {
		t.Fatal("active session was not admitted")
	}
	if _, promoted := owner.promote(active, active.pluginID, nil); !promoted {
		t.Fatal("active session was not promoted")
	}
	pending, admitted := owner.begin("plugin-b", "app-b", 2)
	if !admitted || owner.state() != foregroundPending {
		t.Fatalf("pending admission=%t state=%q", admitted, owner.state())
	}
	request := protocol.SessionExecutionRequest{
		Instance: protocol.InstanceRef{ID: active.instanceID, Generation: active.generation}, SessionToken: string(active.token),
	}
	if err := owner.beginExecution(t.Context(), active.pluginID, request); err != nil {
		t.Fatal(err)
	}
	if owner.state() != foregroundExecuting {
		t.Fatalf("state=%q, want executing", owner.state())
	}
	if _, promoted := owner.promote(pending, pending.pluginID, nil); promoted {
		t.Fatal("pending session replaced an atomically executing foreground")
	}
}

func TestSessionOwnerRejectsCanceledExecutionGrantWithoutMutation(t *testing.T) {
	owner := NewSessionCoordinator(nil)
	active, admitted := owner.begin("plugin", "app", 1)
	if !admitted {
		t.Fatal("session was not admitted")
	}
	if _, promoted := owner.promote(active, active.pluginID, nil); !promoted {
		t.Fatal("session was not promoted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := owner.beginExecution(ctx, active.pluginID, protocol.SessionExecutionRequest{
		Instance: protocol.InstanceRef{ID: active.instanceID, Generation: active.generation}, SessionToken: string(active.token),
	})
	assertDomainErrorKind(t, err, protocol.ErrorSessionCanceled)
	if owner.state() != foregroundInteractive {
		t.Fatalf("canceled grant changed state to %q", owner.state())
	}
}

func TestReconcilerExecutionGrantIsExactAndCriticalCancellationIsTerminal(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &lifecyclePluginController{}
	controllers := &foregroundControllerRecorder{}
	service := newTestReconcilerWithInvalidator(t, store, nil, plugins, func(context.Context) error {
		controllers.Close()
		controllers.InvalidateContext()
		return nil
	})
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(t.Context(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	ref, token := service.ForegroundSessionRef()
	wrongToken := protocol.SessionExecutionRequest{Instance: ref, SessionToken: token + "-stale"}
	assertDomainErrorKind(t, service.sessions.state.BeginExecution(t.Context(), service.live, "plugin", wrongToken), protocol.ErrorSessionNotActive)
	wrongGeneration := protocol.SessionExecutionRequest{Instance: protocol.InstanceRef{ID: ref.ID, Generation: ref.Generation + 1}, SessionToken: token}
	assertDomainErrorKind(t, service.sessions.state.BeginExecution(t.Context(), service.live, "plugin", wrongGeneration), protocol.ErrorSessionGenerationMismatch)

	request := protocol.SessionExecutionRequest{Instance: ref, SessionToken: token}
	if err := service.sessions.state.BeginExecution(t.Context(), service.live, "plugin", request); err != nil {
		t.Fatal(err)
	}
	if service.sessions.state.state() != foregroundExecuting {
		t.Fatalf("state=%q, want executing", service.sessions.state.state())
	}
	if gotRef, gotToken := service.ForegroundSessionRef(); gotRef != (protocol.InstanceRef{}) || gotToken != "" {
		t.Fatalf("executing session still accepted physical input: %#v/%q", gotRef, gotToken)
	}
	if service.AcquireCritical(t.Context(), presentation.Candidate{PluginID: "alerts", InstanceID: "critical", Channel: "main", Key: "first"}) {
		t.Fatal("critical preempted an executing session")
	}
	if controllers.closed != 0 || controllers.invalidated != 0 {
		t.Fatalf("denied critical mutated controllers: %#v", controllers)
	}
	service.sessions.state.PluginSessionCompleted(service.live, "plugin", protocol.CompleteSessionRequest{Instance: ref, SessionToken: token})
	if service.sessions.state.state() != foregroundIdle {
		t.Fatalf("state after completion=%q", service.sessions.state.state())
	}

	if err := service.Launch(t.Context(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	activeRef, activeToken := service.ForegroundSessionRef()
	critical := presentation.Candidate{PluginID: "alerts", InstanceID: "critical", Channel: "main", Key: "second"}
	if !service.AcquireCritical(t.Context(), critical) || service.sessions.state.state() != foregroundCriticalOwned {
		t.Fatalf("critical ownership=%t state=%q", service.CriticalPresentationOwned(), service.sessions.state.state())
	}
	if controllers.closed != 1 || controllers.invalidated != 1 {
		t.Fatalf("critical controller transitions=%#v", controllers)
	}
	_, ended, _ := plugins.snapshot()
	if len(ended) != 1 || ended[0] != pluginhost.SessionToken(activeToken) {
		t.Fatalf("ended=%v, want exact active token %q once", ended, activeToken)
	}
	if err := service.sessions.state.BeginExecution(t.Context(), service.live, "plugin", protocol.SessionExecutionRequest{Instance: activeRef, SessionToken: activeToken}); err == nil {
		t.Fatal("canceled session obtained an execution grant")
	} else {
		assertDomainErrorKind(t, err, protocol.ErrorSessionCanceled)
	}
	if err := service.Launch(t.Context(), "ball8", "", nil); !errors.Is(err, ErrForegroundUnavailable) {
		t.Fatalf("launch during critical = %v, want foreground unavailable", err)
	}
	service.ReleaseCritical()
	if service.CriticalPresentationOwned() || service.Foreground() != "" {
		t.Fatalf("critical release resumed canceled session: critical=%t foreground=%q", service.CriticalPresentationOwned(), service.Foreground())
	}
}

func TestAttentionRuleReportsCriticalBlockedByAtomicExecution(t *testing.T) {
	document := serviceDocument(true)
	app := document.Apps["ball8"]
	app.Policies["answer"] = presentation.PolicyConfig{Policy: presentation.PolicyAttention}
	document.Apps["ball8"] = app
	live := &LiveState{
		loaded: true, document: document,
		generations: Generations{values: map[generationKey]uint64{{pluginID: "plugin", instanceID: "ball8"}: 1}},
	}
	sessions := NewSessionCoordinator(nil)
	resolver, err := NewPolicyResolver(live, sessions, &fakeAssetController{ready: true})
	if err != nil {
		t.Fatal(err)
	}
	admission, admitted := sessions.begin("plugin", "ball8", 1)
	if !admitted {
		t.Fatal("session admission was rejected")
	}
	if _, promoted := sessions.promote(admission, admission.pluginID, nil); !promoted {
		t.Fatal("session promotion failed")
	}
	if err := sessions.beginExecution(t.Context(), "plugin", protocol.SessionExecutionRequest{
		Instance: protocol.InstanceRef{ID: "ball8", Generation: 1}, SessionToken: string(admission.token),
	}); err != nil {
		t.Fatal(err)
	}
	record := observation.Record{PluginID: "plugin", Generation: 1, Observation: protocol.Observation{
		Instance: protocol.InstanceRef{ID: "ball8", Generation: 1}, Channel: "answer", Key: "critical", Revision: 1,
		Disposition: protocol.DispositionActionable, Impact: protocol.ImpactCritical,
	}}
	rule, ok := resolver.Resolve(record)
	if !ok || !rule.BlockedByAtomicExecution {
		t.Fatalf("rule=%#v/%t", rule, ok)
	}
	sessions.PluginSessionCompleted(live, "plugin", protocol.CompleteSessionRequest{
		Instance: protocol.InstanceRef{ID: "ball8", Generation: 1}, SessionToken: string(admission.token),
	})
	rule, ok = resolver.Resolve(record)
	if !ok || rule.BlockedByAtomicExecution {
		t.Fatalf("completed execution remained blocked: %#v/%t", rule, ok)
	}
}

func TestAttentionRuleRejectsStaleRuntimeGenerationAndUsesExactAssetPackage(t *testing.T) {
	document := serviceDocument(true)
	app := document.Apps["ball8"]
	app.Generation = 2
	document.Apps[app.ID] = app
	plugin := document.Plugins[app.PluginID]
	plugin.Version = "2"
	plugin.PackageRoot = "/verified/plugin"
	plugin.Assets = []assets.Declaration{{Source: "assets/mark.png", SHA256: strings.Repeat("a", 64), Size: 10, MediaType: "image/png"}}
	document.Plugins[plugin.ID] = plugin
	assetsController := &recordingReadyForAssets{ready: true}
	live := &LiveState{
		loaded: true, document: document,
		generations: Generations{values: map[generationKey]uint64{{pluginID: plugin.ID, instanceID: app.ID}: app.Generation}},
	}
	resolver, err := NewPolicyResolver(live, NewSessionCoordinator(nil), assetsController)
	if err != nil {
		t.Fatal(err)
	}
	stale := observation.Record{PluginID: plugin.ID, Generation: 1, Observation: protocol.Observation{
		Instance: protocol.InstanceRef{ID: app.ID, Generation: 1}, Channel: "answer", Key: "state", Revision: 1,
	}}
	if rule, ok := resolver.Resolve(stale); !ok || rule.Enabled {
		t.Fatalf("stale rule = %#v, %v", rule, ok)
	}
	current := stale
	current.Generation = app.Generation
	current.Observation.Instance.Generation = app.Generation
	if rule, ok := resolver.Resolve(current); !ok || !rule.Enabled || !rule.AssetsReady {
		t.Fatalf("current rule = %#v, %v", rule, ok)
	}
	want := assets.Package{PluginID: plugin.ID, Version: plugin.Version, Root: plugin.PackageRoot, Enabled: true, Assets: plugin.Assets}
	if !reflect.DeepEqual(assetsController.packageValue, want) {
		t.Fatalf("asset package = %#v, want %#v", assetsController.packageValue, want)
	}
}

type recordingReadyForAssets struct {
	ready        bool
	packageValue assets.Package
}

func (*recordingReadyForAssets) Reconcile(context.Context, []assets.Package) {}

func (*recordingReadyForAssets) Ready(string) bool { return false }

func (a *recordingReadyForAssets) ReadyFor(value assets.Package) bool {
	a.packageValue = value
	return a.ready
}

func (*recordingReadyForAssets) Status() []assets.State { return nil }

func TestReconcilerClearForegroundEndsInteractivePluginSession(t *testing.T) {
	t.Parallel()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, nil, plugins)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	service.ClearForeground("ball8")
	if plugins.endedPlugin != "plugin" || plugins.endedInstance != "ball8" {
		t.Fatalf("ended session = %q/%q, want plugin/ball8", plugins.endedPlugin, plugins.endedInstance)
	}
}

func TestReconcilerTerminalSessionTransitionsCancelExactInputQueue(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	inputs := &recordingSessionInputController{}
	service := newTestReconcilerWithSessionInputs(t, store, nil, plugins, inputs)
	t.Cleanup(func() { _ = service.Close(t.Context()) })
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := service.Launch(t.Context(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	firstRef, firstToken := service.ForegroundSessionRef()
	service.ClearForegroundSessionContext(t.Context(), firstRef.ID, firstToken)

	if err := service.Launch(t.Context(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	secondRef, secondToken := service.ForegroundSessionRef()
	service.sessions.state.PluginSessionCompleted(service.live, "plugin", protocol.CompleteSessionRequest{Instance: secondRef, SessionToken: secondToken})

	if err := service.Launch(t.Context(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	thirdRef, thirdToken := service.ForegroundSessionRef()
	service.sessions.state.PluginSessionInvalidated(pluginhost.SessionInvalidation{
		PluginID: "plugin", InstanceID: thirdRef.ID, Generation: thirdRef.Generation, Token: pluginhost.SessionToken(thirdToken),
		Reason: pluginhost.SessionInvalidatedExit,
	})

	got := inputs.snapshot()
	want := []canceledSessionInput{
		{pluginID: "plugin", target: firstRef, token: firstToken},
		{pluginID: "plugin", target: thirdRef, token: thirdToken},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canceled session inputs = %#v, want %#v", got, want)
	}
	if completed := inputs.completedSnapshot(); !reflect.DeepEqual(completed, []canceledSessionInput{{pluginID: "plugin", target: secondRef, token: secondToken}}) {
		t.Fatalf("completed session inputs = %#v", completed)
	}
}

func TestDisableAndDeleteEndActiveInteractiveSession(t *testing.T) {
	for _, operation := range []string{"disable", "delete"} {
		t.Run(operation, func(t *testing.T) {
			store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
			if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
				t.Fatal(err)
			}
			plugins := &fakePluginController{}
			service := newTestReconciler(t, store, nil, plugins)
			t.Cleanup(func() { _ = service.Close(context.Background()) })
			if err := service.Load(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
				t.Fatal(err)
			}
			var err error
			if operation == "disable" {
				_, err = service.SetEnabled(context.Background(), "ball8", false)
			} else {
				_, err = service.DeleteAppInstance(context.Background(), "ball8")
			}
			if err != nil {
				t.Fatal(err)
			}
			if service.Foreground() != "" || plugins.endedPlugin != "plugin" || plugins.endedInstance != "ball8" {
				t.Fatalf("foreground/end after %s = %q / %q/%q", operation, service.Foreground(), plugins.endedPlugin, plugins.endedInstance)
			}
		})
	}
}

func TestDisableAndDeleteInvalidatePendingInteractiveAdmission(t *testing.T) {
	for _, operation := range []string{"disable", "delete"} {
		t.Run(operation, func(t *testing.T) {
			store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
			if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
				t.Fatal(err)
			}
			plugins := newAdmissionRacePluginController()
			service := newTestReconciler(t, store, nil, plugins)
			t.Cleanup(func() { _ = service.Close(context.Background()) })
			if err := service.Load(context.Background()); err != nil {
				t.Fatal(err)
			}
			launchDone := make(chan error, 1)
			go func() { launchDone <- service.Launch(context.Background(), "ball8", "", nil) }()
			awaitServiceSignal(t, plugins.invokeStarted, "pending invoke")
			if operation == "disable" {
				_, err := service.SetEnabled(context.Background(), "ball8", false)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				_, err := service.DeleteAppInstance(context.Background(), "ball8")
				if err != nil {
					t.Fatal(err)
				}
			}
			close(plugins.invokeRelease)
			if err := <-launchDone; err != nil {
				t.Fatal(err)
			}
			active, invoked, ended := plugins.snapshot()
			if active != "" || invoked == "" || len(ended) != 1 || ended[0] != invoked || service.Foreground() != "" {
				t.Fatalf("pending %s left active=%q invoked=%q ended=%v foreground=%q", operation, active, invoked, ended, service.Foreground())
			}
		})
	}
}

func TestAcceptedGenerationInvalidatesPendingAdmissionBeforeConfigReplacementSupervisorCleanup(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := newAdmissionRacePluginController()
	service := newTestReconciler(t, store, nil, plugins)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	launchDone := make(chan error, 1)
	go func() { launchDone <- service.Launch(context.Background(), "ball8", "", nil) }()
	awaitServiceSignal(t, plugins.invokeStarted, "pending invoke")
	token := plugins.pendingToken()

	_, outcome, err := service.ReplaceAppConfiguration(context.Background(), "ball8", AppConfiguration{
		Config: json.RawMessage(`{"revision":2}`), Policies: map[string]presentation.PolicyConfig{
			"answer": {Policy: presentation.PolicyInteractive},
		}, LaunchAction: "ask",
	})
	if err != nil || !outcome.IsCommitted() {
		t.Fatalf("ReplaceAppConfiguration = %q, %v", outcome, err)
	}
	service.sessions.state.PluginSessionInvalidated(pluginhost.SessionInvalidation{
		PluginID: "plugin", InstanceID: "ball8", Generation: 1, Token: token,
		Reason: pluginhost.SessionInvalidatedGeneration,
	})
	close(plugins.invokeRelease)
	if err := <-launchDone; err != nil {
		t.Fatal(err)
	}
	assertPendingAdmissionCompensatedOnce(t, service, plugins)
}

func TestAcceptedUnrelatedAppChangePreservesPendingAdmission(t *testing.T) {
	for _, operation := range []string{"create", "enable"} {
		t.Run(operation, func(t *testing.T) {
			document := serviceDocument(true)
			unrelated := config.App{
				ID: "unrelated", PluginID: "plugin", Enabled: operation != "enable", Config: json.RawMessage(`{}`),
				Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}},
			}
			if operation == "enable" {
				document.Apps[unrelated.ID] = unrelated
			}
			store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
			if _, err := store.ReplaceWithOutcome(0, document); err != nil {
				t.Fatal(err)
			}
			plugins := newAdmissionRacePluginController()
			service := newTestReconciler(t, store, nil, plugins)
			t.Cleanup(func() { _ = service.Close(context.Background()) })
			if err := service.Load(context.Background()); err != nil {
				t.Fatal(err)
			}
			launchDone := make(chan error, 1)
			go func() { launchDone <- service.Launch(context.Background(), "ball8", "", nil) }()
			awaitServiceSignal(t, plugins.invokeStarted, "pending invoke")
			if operation == "create" {
				_, err := service.CreateAppInstance(context.Background(), unrelated)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				_, err := service.SetEnabled(context.Background(), unrelated.ID, true)
				if err != nil {
					t.Fatal(err)
				}
			}
			close(plugins.invokeRelease)
			if err := <-launchDone; err != nil {
				t.Fatal(err)
			}
			active, invoked, ended := plugins.snapshot()
			if active != invoked || invoked == "" || len(ended) != 0 || service.Foreground() != "ball8" {
				t.Fatalf("unrelated %s changed admission: active=%q invoked=%q ended=%v foreground=%q", operation, active, invoked, ended, service.Foreground())
			}
		})
	}
}

func TestAcceptedUnrelatedGenerationPreservesPromotedForeground(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, nil, plugins)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	_, err := service.CreateAppInstance(context.Background(), config.App{
		ID: "unrelated", PluginID: "plugin", Enabled: true, Config: json.RawMessage(`{}`),
		Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := service.Foreground(); got != "ball8" || plugins.endedInstance != "" {
		t.Fatalf("unrelated generation changed foreground/end = %q/%q", got, plugins.endedInstance)
	}
}

func assertPendingAdmissionCompensatedOnce(t *testing.T, service *Reconciler, plugins *admissionRacePluginController) {
	t.Helper()
	active, invoked, ended := plugins.snapshot()
	if active != "" || invoked == "" || len(ended) != 1 || ended[0] != invoked || service.Foreground() != "" {
		t.Fatalf("pending admission left active=%q invoked=%q ended=%v foreground=%q", active, invoked, ended, service.Foreground())
	}
}

func TestReconcilerPluginSessionInvalidationClearsOnlyExactForegroundToken(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, nil, plugins)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	ref, token := service.ForegroundSessionRef()
	if ref != (protocol.InstanceRef{ID: "ball8", Generation: 1}) || token != string(plugins.invokedToken) {
		t.Fatalf("promoted foreground identity = %#v/%q", ref, token)
	}
	for _, invalidation := range []pluginhost.SessionInvalidation{
		{PluginID: "other", InstanceID: ref.ID, Generation: ref.Generation, Token: plugins.invokedToken, Reason: pluginhost.SessionInvalidatedExit},
		{PluginID: "plugin", InstanceID: "other", Generation: ref.Generation, Token: plugins.invokedToken, Reason: pluginhost.SessionInvalidatedExit},
		{PluginID: "plugin", InstanceID: ref.ID, Generation: ref.Generation + 1, Token: plugins.invokedToken, Reason: pluginhost.SessionInvalidatedExit},
		{PluginID: "plugin", InstanceID: ref.ID, Generation: ref.Generation, Token: "stale", Reason: pluginhost.SessionInvalidatedExit},
	} {
		service.sessions.state.PluginSessionInvalidated(invalidation)
		if gotRef, gotToken := service.ForegroundSessionRef(); gotRef != ref || gotToken != token {
			t.Fatalf("inexact invalidation %#v changed foreground to %#v/%q", invalidation, gotRef, gotToken)
		}
	}
	service.sessions.state.PluginSessionInvalidated(pluginhost.SessionInvalidation{PluginID: "plugin", InstanceID: "ball8", Generation: 1, Token: plugins.invokedToken, Reason: pluginhost.SessionInvalidatedExit})
	if got := service.Foreground(); got != "" {
		t.Fatalf("exact invalidation left foreground: %q", got)
	}
}

func TestReconcilerPluginSessionInvalidationDoesNotRequireDesiredAppToRemain(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &fakePluginController{}
	service := newTestReconciler(t, store, nil, plugins)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	service.live.mu.Lock()
	delete(service.live.document.Apps, "ball8")
	service.live.mu.Unlock()

	service.sessions.state.PluginSessionInvalidated(pluginhost.SessionInvalidation{
		PluginID: "plugin", InstanceID: "ball8", Generation: 1, Token: plugins.invokedToken,
		Reason: pluginhost.SessionInvalidatedExit,
	})
	if got := service.Foreground(); got != "" {
		t.Fatalf("exact invalidation after desired-state removal left foreground: %q", got)
	}
}

func TestReconcilerStaleClearCannotEndNewSameAppSession(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := newSessionRacePluginController()
	service := newTestReconciler(t, store, nil, plugins)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	clearDone := make(chan struct{})
	go func() {
		service.ClearForeground("ball8")
		close(clearDone)
	}()
	awaitServiceSignal(t, plugins.endStarted, "old session end")
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	close(plugins.endRelease)
	awaitServiceSignal(t, clearDone, "old foreground clear")
	if !plugins.activeSession() {
		t.Fatal("stale foreground clear ended the replacement same-app session")
	}
}

func TestReconcilerFailedSameAppRelaunchPreservesPriorForeground(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &failSecondInvokePluginController{}
	diagnostics := &wakeAttention{}
	service := newTestReconcilerWithAttention(t, store, nil, plugins, diagnostics)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	if diagnostics.count() != 1 {
		t.Fatalf("successful launch wakes = %d, want 1", diagnostics.count())
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err == nil {
		t.Fatal("second launch unexpectedly succeeded")
	}
	if got := service.Foreground(); got != "ball8" {
		t.Fatalf("foreground after failed same-app relaunch = %q, want prior session", got)
	}
	if diagnostics.count() != 1 {
		t.Fatalf("failed relaunch wakes = %d, want no transition wake", diagnostics.count())
	}
}

func TestReconcilerClearDuringBlockedInvokeCompensatesLateSession(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := newAdmissionRacePluginController()
	service := newTestReconciler(t, store, nil, plugins)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	launchDone := make(chan error, 1)
	go func() { launchDone <- service.Launch(context.Background(), "ball8", "", nil) }()
	awaitServiceSignal(t, plugins.invokeStarted, "blocked invoke")
	service.ClearForeground("ball8")
	if got := service.Foreground(); got != "" {
		t.Fatalf("foreground after clear = %q, want empty", got)
	}
	close(plugins.invokeRelease)
	if err := <-launchDone; err != nil {
		t.Fatalf("Launch: %v", err)
	}
	active, invoked, ended := plugins.snapshot()
	if active != "" {
		t.Fatalf("late invoke left active session %q", active)
	}
	if invoked == "" {
		t.Fatal("invoke used an empty session token")
	}
	if len(ended) != 1 || ended[0] != invoked {
		t.Fatalf("ended tokens = %#v, want exact late token %q once", ended, invoked)
	}
}

func TestReconcilerSerializesConcurrentLaunches(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := newSerializedLaunchPluginController()
	service := newTestReconciler(t, store, nil, plugins)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- service.Launch(context.Background(), "ball8", "", nil) }()
	awaitServiceSignal(t, plugins.firstStarted, "first invoke")
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondEntered)
		secondDone <- service.Launch(context.Background(), "ball8", "", nil)
	}()
	awaitServiceSignal(t, secondEntered, "second launch entry")
	if service.sessions.state.launchMu.TryLock() {
		service.sessions.state.launchMu.Unlock()
		t.Fatal("first blocked invoke did not retain launch transaction ownership")
	}
	close(plugins.firstRelease)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	awaitServiceSignal(t, plugins.secondStarted, "second invoke")
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if got := plugins.maxOverlap(); got != 1 {
		t.Fatalf("maximum concurrent plugin invokes = %d, want 1", got)
	}
}

func TestReconcilerSuccessfulReplacementEndsExactPriorSessionOnce(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(true)); err != nil {
		t.Fatal(err)
	}
	plugins := &lifecyclePluginController{}
	diagnostics := &wakeAttention{}
	service := newTestReconcilerWithAttention(t, store, nil, plugins, diagnostics)
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := service.Launch(context.Background(), "ball8", "", nil); err != nil {
		t.Fatal(err)
	}
	invoked, ended, _ := plugins.snapshot()
	if len(invoked) != 2 || len(ended) != 1 || ended[0] != invoked[0] {
		t.Fatalf("invoked=%#v ended=%#v, want exact prior token once", invoked, ended)
	}
	if diagnostics.count() != 2 {
		t.Fatalf("attention wakes = %d, want one per visible launch", diagnostics.count())
	}
}

func TestReconcilerReconcilesCommittedSessionInputTargetsBeforePlugins(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	document := serviceDocument(true)
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}

	log := &orderedServiceLog{}
	plugins := &orderedPluginController{fakePluginController: &fakePluginController{}, log: log}
	inputs := &orderedSessionInputController{log: log}
	service := newTestReconcilerWithSessionInputs(t, store, nil, plugins, inputs)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	desired, err := service.DesiredPlugin(context.Background(), "plugin")
	if err != nil || desired == nil {
		t.Fatalf("DesiredPlugin = %#v, %v", desired, err)
	}
	desired.Version = "2"
	if outcome, err := service.ActivatePlugin(context.Background(), *desired); err != nil || !outcome.IsCommitted() {
		t.Fatalf("ActivatePlugin = %q, %v", outcome, err)
	}

	if got, want := log.snapshot(), []string{"session-input", "plugin", "session-input", "plugin"}; !equalServiceOperations(got, want) {
		t.Fatalf("reconciliation order = %v, want %v", got, want)
	}
	plans := inputs.snapshot()
	if len(plans) != 2 {
		t.Fatalf("session input plans = %#v, want two committed plans", plans)
	}
	assertSessionInputTargetPlan(t, plans[0])
	assertSessionInputTargetPlan(t, plans[1])
}

type orderedSessionInputController struct {
	mu    sync.Mutex
	log   *orderedServiceLog
	plans [][]eventbus.TargetSet
}

func (c *orderedSessionInputController) Apply(values []eventbus.TargetSet) {
	cloned := make([]eventbus.TargetSet, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].InstanceIDs = append([]string(nil), value.InstanceIDs...)
	}
	c.mu.Lock()
	c.plans = append(c.plans, cloned)
	c.mu.Unlock()
	c.log.add("session-input")
}

func (*orderedSessionInputController) Status() []eventbus.Status { return nil }

func (*orderedSessionInputController) Cancel(string, protocol.InstanceRef, string) {
}

func (*orderedSessionInputController) Complete(string, protocol.InstanceRef, string) {}

func (c *orderedSessionInputController) snapshot() [][]eventbus.TargetSet {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]eventbus.TargetSet(nil), c.plans...)
}

func assertSessionInputTargetPlan(t *testing.T, values []eventbus.TargetSet) {
	t.Helper()
	if len(values) != 1 || values[0].PluginID != "plugin" || len(values[0].InstanceIDs) != 1 || values[0].InstanceIDs[0] != "ball8" {
		t.Fatalf("session input plan = %#v, want plugin/ball8", values)
	}
}

type wakeAttention struct {
	mu    sync.Mutex
	wakes int
}

func (*wakeAttention) SelectedObservation() (observation.Record, bool) {
	return observation.Record{}, false
}

func (a *wakeAttention) Wake() { a.mu.Lock(); a.wakes++; a.mu.Unlock() }

func (a *wakeAttention) count() int { a.mu.Lock(); defer a.mu.Unlock(); return a.wakes }

func (*wakeAttention) AttentionSnapshot() (attention.Trace, bool) { return attention.Trace{}, false }

func (*wakeAttention) AttentionExplain(string) (attention.Evaluation, bool) {
	return attention.Evaluation{}, false
}

func (*wakeAttention) AttentionHistory(int, time.Time) []attention.Trace { return nil }

func (*wakeAttention) AcknowledgeAttention(string) error { return nil }

func (*wakeAttention) Reconcile(context.Context) error { return nil }

func (*wakeAttention) RecorderStatus() attention.RecorderStatus {
	return attention.RecorderStatus{Phase: attention.RecorderUnavailable}
}

func (*wakeAttention) ObservationDiagnostics() observation.StoreDiagnostics {
	return observation.StoreDiagnostics{}
}

func (*wakeAttention) AttentionStateStatus() AttentionStateDiagnostics {
	return AttentionStateDiagnostics{}
}

func (*wakeAttention) PresentationCooldownStatus() PresentationCooldownDiagnostics {
	return PresentationCooldownDiagnostics{}
}

func TestReconcilerLaunchRejectsAcceptedGenerationUntilPluginApplyCommits(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(false)); err != nil {
		t.Fatal(err)
	}
	plugins := newBlockingApplyController()
	service := newTestReconciler(t, store, nil, plugins)
	releaseApply := sync.OnceFunc(func() { close(plugins.oldRelease) })
	t.Cleanup(func() {
		releaseApply()
		_ = service.Close(t.Context())
	})
	if err := service.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	type enableResponse struct {
		result EnableResult
		err    error
	}
	enabled := make(chan enableResponse, 1)
	go func() {
		result, err := service.SetEnabled(t.Context(), "ball8", true)
		enabled <- enableResponse{result: result, err: err}
	}()
	awaitServiceSignal(t, plugins.oldStarted, "accepted generation apply")
	document, loaded := service.Document()
	if !loaded || document.Generation != 2 || !document.Apps["ball8"].Enabled {
		t.Fatalf("accepted document while apply blocked = %#v, loaded=%t", document, loaded)
	}
	if generation, ok := service.Generation("plugin", "ball8"); ok {
		t.Fatalf("unapplied accepted generation became effective: %d", generation)
	}
	if err := service.Launch(t.Context(), "ball8", "ask", nil); !errors.Is(err, ErrAppNotReady) {
		t.Fatalf("Launch while generation unapplied = %v, want ErrAppNotReady", err)
	}
	if got := plugins.invocationRequests(); len(got) != 0 {
		t.Fatalf("unapplied generation reached plugin: %#v", got)
	}
	releaseApply()
	response := <-enabled
	if response.err != nil || response.result.Generation != 2 || response.result.ReconciliationError != nil {
		t.Fatalf("SetEnabled = %#v, %v", response.result, response.err)
	}
	if err := service.Launch(t.Context(), "ball8", "ask", nil); err != nil {
		t.Fatalf("Launch after apply: %v", err)
	}
	invocations := plugins.invocationRequests()
	if len(invocations) != 1 || invocations[0].Generation != 2 {
		t.Fatalf("applied launch requests = %#v, want generation 2", invocations)
	}
}

func TestStaleBlockingApplyCannotPreventLatestAdmissionOrRegressManager(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := store.ReplaceWithOutcome(0, serviceDocument(false)); err != nil {
		t.Fatal(err)
	}
	plugins := newBlockingApplyController()
	service := newTestReconcilerWithRetryDelay(t, store, nil, plugins, func(int) time.Duration { return time.Millisecond })
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if err := service.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	type enableResponse struct {
		result EnableResult
		err    error
	}
	enableDone := make(chan enableResponse, 1)
	go func() {
		result, err := service.SetEnabled(context.Background(), "ball8", true)
		enableDone <- enableResponse{result: result, err: err}
	}()
	awaitServiceSignal(t, plugins.oldStarted, "old blocking apply")
	disableDone := make(chan enableResponse, 1)
	go func() {
		result, err := service.SetEnabled(context.Background(), "ball8", false)
		disableDone <- enableResponse{result: result, err: err}
	}()
	select {
	case response := <-disableDone:
		if response.err != nil || response.result.Generation != 3 || response.result.Outcome != localstate.Committed {
			t.Fatalf("disable: %#v, %v", response.result, response.err)
		}
	case <-time.After(100 * time.Millisecond):
		close(plugins.oldRelease)
		t.Fatal("latest Apply admission blocked behind stale Apply")
	}
	close(plugins.oldRelease)
	select {
	case response := <-enableDone:
		if response.err != nil || response.result.Generation != 2 || response.result.Outcome != localstate.Committed || response.result.ReconciliationError != nil {
			t.Fatalf("superseded enable: %#v, %v", response.result, response.err)
		}
	case <-time.After(time.Second):
		t.Fatal("stale enable did not unwind")
	}
	awaitCondition(t, time.Second, func() bool { return plugins.calls() >= 4 }, "corrective latest apply")
	if specs := plugins.specs(); len(specs) != 0 {
		t.Fatalf("manager regressed to stale specs: %#v", specs)
	}
	if _, ok := service.Generation("plugin", "ball8"); ok {
		t.Fatal("stale generation admitted")
	}
	if got := readinessByID(service.AppReadiness())["ball8"]; got.Phase != AppDisabled {
		t.Fatalf("readiness = %#v", got)
	}
}

type foregroundControllerRecorder struct {
	closed      int
	invalidated int
}

func (r *foregroundControllerRecorder) Close() { r.closed++ }

func (r *foregroundControllerRecorder) InvalidateContext() { r.invalidated++ }

type failSecondInvokePluginController struct {
	safePluginController
	invocations int
}

func (f *failSecondInvokePluginController) Invoke(context.Context, string, pluginhost.InvokeRequest, pluginhost.InvocationKind, pluginhost.SessionToken) error {
	f.invocations++
	if f.invocations == 2 {
		return errors.New("invoke failed")
	}
	return nil
}

func daemonObjectOfSize(t *testing.T, size int) json.RawMessage {
	t.Helper()
	const shell = `{"x":""}`
	if size < len(shell) {
		t.Fatalf("JSON object size %d is smaller than minimum %d", size, len(shell))
	}
	value := json.RawMessage(`{"x":"` + strings.Repeat("x", size-len(shell)) + `"}`)
	if len(value) != size || !json.Valid(value) {
		t.Fatalf("invalid JSON object fixture: %d bytes", len(value))
	}
	return value
}

type sessionRacePluginController struct {
	mu         sync.Mutex
	active     pluginhost.SessionToken
	endStarted chan struct{}
	endRelease chan struct{}
}

type admissionRacePluginController struct {
	mu            sync.Mutex
	active        pluginhost.SessionToken
	invoked       pluginhost.SessionToken
	pending       pluginhost.SessionToken
	ended         []pluginhost.SessionToken
	invokeStarted chan struct{}
	invokeRelease chan struct{}
}

type serializedLaunchPluginController struct {
	mu            sync.Mutex
	invocations   int
	active        int
	maxActive     int
	firstStarted  chan struct{}
	firstRelease  chan struct{}
	secondStarted chan struct{}
}

func newSerializedLaunchPluginController() *serializedLaunchPluginController {
	return &serializedLaunchPluginController{
		firstStarted: make(chan struct{}, 1), firstRelease: make(chan struct{}), secondStarted: make(chan struct{}, 1),
	}
}

func (*serializedLaunchPluginController) Apply(context.Context, []pluginhost.Spec) error {
	return nil
}

func (f *serializedLaunchPluginController) Invoke(context.Context, string, pluginhost.InvokeRequest, pluginhost.InvocationKind, pluginhost.SessionToken) error {
	f.mu.Lock()
	f.invocations++
	invocation := f.invocations
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	if invocation == 1 {
		f.firstStarted <- struct{}{}
		<-f.firstRelease
	} else {
		f.secondStarted <- struct{}{}
	}
	return nil
}

func (f *serializedLaunchPluginController) maxOverlap() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func (*serializedLaunchPluginController) EndSession(context.Context, string, protocol.InstanceRef, pluginhost.SessionToken) error {
	return nil
}

func (*serializedLaunchPluginController) Status() []pluginhost.PluginStatus { return nil }

func (*serializedLaunchPluginController) Operation(context.Context, string, protocol.OperationRequest) (protocol.OperationResult, error) {
	return protocol.OperationResult{}, errors.New("unexpected plugin operation")
}

func (*serializedLaunchPluginController) Close(context.Context) error { return nil }

func newAdmissionRacePluginController() *admissionRacePluginController {
	return &admissionRacePluginController{invokeStarted: make(chan struct{}, 1), invokeRelease: make(chan struct{})}
}

func (*admissionRacePluginController) Apply(context.Context, []pluginhost.Spec) error { return nil }

func (f *admissionRacePluginController) Invoke(_ context.Context, _ string, _ pluginhost.InvokeRequest, _ pluginhost.InvocationKind, token pluginhost.SessionToken) error {
	f.mu.Lock()
	f.pending = token
	f.mu.Unlock()
	f.invokeStarted <- struct{}{}
	<-f.invokeRelease
	f.mu.Lock()
	f.invoked = token
	f.active = token
	f.mu.Unlock()
	return nil
}

func (f *admissionRacePluginController) pendingToken() pluginhost.SessionToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending
}

func (f *admissionRacePluginController) EndSession(_ context.Context, _ string, _ protocol.InstanceRef, token pluginhost.SessionToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, token)
	if f.active == token {
		f.active = ""
	}
	return nil
}

func (*admissionRacePluginController) Status() []pluginhost.PluginStatus { return nil }

func (*admissionRacePluginController) Operation(context.Context, string, protocol.OperationRequest) (protocol.OperationResult, error) {
	return protocol.OperationResult{}, errors.New("unexpected plugin operation")
}

func (*admissionRacePluginController) Close(context.Context) error { return nil }

func (f *admissionRacePluginController) snapshot() (pluginhost.SessionToken, pluginhost.SessionToken, []pluginhost.SessionToken) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active, f.invoked, append([]pluginhost.SessionToken(nil), f.ended...)
}

func newSessionRacePluginController() *sessionRacePluginController {
	return &sessionRacePluginController{endStarted: make(chan struct{}, 1), endRelease: make(chan struct{})}
}

func (*sessionRacePluginController) Apply(context.Context, []pluginhost.Spec) error { return nil }

func (f *sessionRacePluginController) Invoke(_ context.Context, _ string, _ pluginhost.InvokeRequest, _ pluginhost.InvocationKind, token pluginhost.SessionToken) error {
	f.mu.Lock()
	f.active = token
	f.mu.Unlock()
	return nil
}

func (f *sessionRacePluginController) EndSession(_ context.Context, _ string, _ protocol.InstanceRef, token pluginhost.SessionToken) error {
	f.endStarted <- struct{}{}
	<-f.endRelease
	f.mu.Lock()
	if f.active == token {
		f.active = ""
	}
	f.mu.Unlock()
	return nil
}

func (*sessionRacePluginController) Status() []pluginhost.PluginStatus { return nil }

func (*sessionRacePluginController) Operation(context.Context, string, protocol.OperationRequest) (protocol.OperationResult, error) {
	return protocol.OperationResult{}, errors.New("unexpected plugin operation")
}

func (*sessionRacePluginController) Close(context.Context) error { return nil }

func (f *sessionRacePluginController) activeSession() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active != ""
}

func assertDomainErrorKind(t *testing.T, err error, want protocol.ErrorKind) {
	t.Helper()
	domain, ok := errors.AsType[*protocol.DomainError](err)
	if !ok || domain.Kind() != want {
		t.Fatalf("error=%#v, want domain kind %q", err, want)
	}
}
