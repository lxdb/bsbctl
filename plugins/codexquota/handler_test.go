package codexquota

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestDefinitionDeclaresResidentAndInteractiveChannels(t *testing.T) {
	t.Parallel()
	definition := DefinitionForVersion(PluginVersion)
	if definition.ID != PluginID || definition.Version != PluginVersion {
		t.Fatalf("identity = %q/%q", definition.ID, definition.Version)
	}
	if !reflect.DeepEqual(definition.Contract.ExecutionModes, []protocol.ExecutionMode{protocol.ExecutionModeResident, protocol.ExecutionModeInteractive}) {
		t.Fatalf("execution modes = %v", definition.Contract.ExecutionModes)
	}
	if !reflect.DeepEqual(definition.Contract.Channels, []protocol.Channel{{ID: ChannelSummary}, {ID: ChannelPressure}, {ID: ChannelLive}}) {
		t.Fatalf("channels = %v", definition.Contract.Channels)
	}
}

func TestQuotaLiveSessionPublishesWaitingDataUnavailableRecoveryAndCompletesOnBack(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	host := &recordingQuotaHost{observations: make(chan protocol.Observation, 32)}
	worker := &accountWorker{
		instanceID: "main", generation: 7, config: defaultConfig("/tmp/codex"), host: host, now: func() time.Time { return now },
	}
	handler := newHandler(host, nil, func() (string, error) { return "/tmp/codex", nil }, func() time.Time { return now })
	handler.workers[worker.instanceID] = worker
	start := protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: "main", Generation: 7}, Action: "open", SessionToken: "live-session",
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher},
	}
	if err := handler.StartSession(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	waiting := host.next(t)
	if waiting.Channel != ChannelLive || waiting.Key != "panel" || waiting.Revision != 1 || sceneText(waiting, "front-status") != "WAITING" {
		t.Fatalf("waiting live observation = %#v", waiting)
	}

	snapshot := Snapshot{UpdatedAt: now, Windows: []Window{
		{Duration: 5 * time.Hour, RemainingPercent: 80, ResetsAt: now.Add(time.Hour)},
		{Duration: 7 * 24 * time.Hour, RemainingPercent: 10, ResetsAt: now.Add(24 * time.Hour)},
	}}
	if err := worker.publish(t.Context(), snapshot, now); err != nil {
		t.Fatal(err)
	}
	live := nextQuotaObservation(t, host, ChannelLive)
	if live.Revision != 2 || sceneText(live, "front-window-label") != "1W" || sceneText(live, "back-window-0-label") != "5 HOURS" || sceneText(live, "back-window-1-label") != "WEEKLY" {
		t.Fatalf("live quota data = %#v", live)
	}

	worker.recordFailure(t.Context(), errors.New("temporary"))
	worker.recordFailure(t.Context(), errors.New("temporary"))
	select {
	case value := <-host.observations:
		if value.Channel == ChannelLive {
			t.Fatalf("live data was replaced before failure threshold: %#v", value)
		}
	default:
	}
	worker.recordFailure(t.Context(), errors.New("temporary"))
	unavailable := nextQuotaObservation(t, host, ChannelLive)
	if unavailable.Revision != 3 || sceneText(unavailable, "front-status") != "UNAVAILABLE" {
		t.Fatalf("unavailable live observation = %#v", unavailable)
	}

	now = now.Add(time.Minute)
	if err := worker.publish(t.Context(), snapshot, now); err != nil {
		t.Fatal(err)
	}
	recovered := nextQuotaObservation(t, host, ChannelLive)
	if recovered.Revision != 4 || sceneText(recovered, "front-window-label") != "1W" {
		t.Fatalf("recovered live observation = %#v", recovered)
	}
	if err := handler.EndSession(t.Context(), protocol.SessionEndRequest{Instance: start.Instance, SessionToken: "stale-session"}); err != nil {
		t.Fatal(err)
	}
	for sequence, input := range []protocol.SessionInput{
		{Button: &protocol.ButtonInput{Button: protocol.ButtonOK, Action: protocol.ButtonPress}},
		{Button: &protocol.ButtonInput{Button: protocol.ButtonStart, Action: protocol.ButtonPress}},
		{Encoder: &protocol.EncoderInput{Delta: 1}},
	} {
		result, err := handler.HandleSessionInput(t.Context(), protocol.SessionInputRequest{
			Sequence: uint64(sequence + 1), OccurredAt: now, Instance: start.Instance, SessionToken: start.SessionToken, Input: input,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Disposition != protocol.SessionInputNotConsumed {
			t.Fatalf("read-only input result = %#v", result)
		}
	}
	select {
	case value := <-host.observations:
		t.Fatalf("read-only input changed live quota: %#v", value)
	default:
	}
	result, err := handler.HandleSessionInput(t.Context(), protocol.SessionInputRequest{
		Sequence: 4, OccurredAt: now, Instance: start.Instance, SessionToken: start.SessionToken,
		Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonBack, Action: protocol.ButtonPress}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != protocol.SessionInputConsumed {
		t.Fatalf("Back result = %#v", result)
	}
	resolved := nextQuotaObservation(t, host, ChannelLive)
	if resolved.Revision != 5 || resolved.Disposition != protocol.DispositionResolved || resolved.Scene != nil {
		t.Fatalf("resolved live observation = %#v", resolved)
	}
	host.mu.Lock()
	completions := append([]protocol.CompleteSessionRequest(nil), host.completions...)
	host.mu.Unlock()
	if !reflect.DeepEqual(completions, []protocol.CompleteSessionRequest{{Instance: start.Instance, SessionToken: start.SessionToken}}) {
		t.Fatalf("session completions = %#v", completions)
	}
}

func TestQuotaLiveSessionRequiresExactLauncherIdentity(t *testing.T) {
	host := &recordingQuotaHost{observations: make(chan protocol.Observation, 4)}
	worker := &accountWorker{instanceID: "main", generation: 7, config: defaultConfig("/tmp/codex"), host: host}
	handler := newHandler(host, nil, func() (string, error) { return "/tmp/codex", nil }, time.Now)
	handler.workers[worker.instanceID] = worker
	valid := protocol.SessionStartRequest{Instance: protocol.InstanceRef{ID: "main", Generation: 7}, Action: "open", SessionToken: "live-session", Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher}}
	for _, mutate := range []func(*protocol.SessionStartRequest){
		func(value *protocol.SessionStartRequest) { value.Instance.Generation++ },
		func(value *protocol.SessionStartRequest) { value.Action = "inspect" },
		func(value *protocol.SessionStartRequest) { value.Trigger.Kind = protocol.SessionTriggerObservation },
	} {
		request := valid
		trigger := *valid.Trigger
		request.Trigger = &trigger
		mutate(&request)
		if err := handler.StartSession(t.Context(), request); err == nil {
			t.Fatalf("invalid session was accepted: %#v", request)
		}
	}
}

func TestQuotaLivePublishesSuccessfulFetchWhenResidentPublicationFails(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	host := &recordingQuotaHost{observations: make(chan protocol.Observation, 8)}
	worker := &accountWorker{instanceID: "main", generation: 7, config: defaultConfig("/tmp/codex"), host: host, now: func() time.Time { return now }}
	if err := worker.startLive(t.Context(), "live-session"); err != nil {
		t.Fatal(err)
	}
	_ = host.next(t)
	host.mu.Lock()
	host.failChannel = ChannelSummary
	host.mu.Unlock()
	snapshot := Snapshot{UpdatedAt: now, Windows: []Window{{Duration: 5 * time.Hour, RemainingPercent: 80, ResetsAt: now.Add(time.Hour)}}}
	if err := worker.publish(t.Context(), snapshot, now); err == nil {
		t.Fatal("resident publication failure was hidden")
	}
	live := nextQuotaObservation(t, host, ChannelLive)
	if live.Revision != 2 || sceneText(live, "front-window-label") != "5H" {
		t.Fatalf("live quota after resident failure = %#v", live)
	}
}

func TestResidentPublicationFailureDoesNotReportQuotaSourceUnavailable(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	host := &recordingQuotaHost{observations: make(chan protocol.Observation, 32), failChannel: ChannelSummary}
	calls := make(chan struct{}, 8)
	workerCtx, cancel := context.WithCancel(context.Background())
	worker := &accountWorker{
		instanceID: "main", generation: 7, config: defaultConfig("/tmp/codex"), host: host,
		now: func() time.Time { return now }, slots: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{}),
		source: quotaSourceFunc(func(context.Context) (Snapshot, error) {
			calls <- struct{}{}
			return Snapshot{UpdatedAt: now, Windows: []Window{{Duration: 5 * time.Hour, RemainingPercent: 80, ResetsAt: now.Add(time.Hour)}}}, nil
		}),
	}
	worker.config.PollInterval = time.Millisecond
	if err := worker.startLive(t.Context(), "live-session"); err != nil {
		t.Fatal(err)
	}
	_ = host.next(t)
	go worker.run(workerCtx)
	defer func() {
		cancel()
		<-worker.done
	}()
	for range 4 {
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatal("quota source was not polled")
		}
	}
	worker.healthMu.RLock()
	unhealthy := worker.unhealthy
	worker.healthMu.RUnlock()
	if unhealthy {
		t.Fatal("resident publication failure marked the quota source unhealthy")
	}
	for {
		select {
		case value := <-host.observations:
			if value.Channel == ChannelLive && value.ReasonCode == "codex_quota_unavailable" {
				t.Fatalf("resident publication failure replaced live source data: %#v", value)
			}
		default:
			return
		}
	}
}

func TestQuotaPublicationFailuresDegradePluginHealthWithoutReplacingLiveData(t *testing.T) {
	host := &recordingQuotaHost{observations: make(chan protocol.Observation, 4)}
	handler := newHandler(host, nil, func() (string, error) { return "/tmp/codex", nil }, time.Now)
	worker := &accountWorker{instanceID: "main", generation: 7, host: host, now: time.Now}
	handler.workers[worker.instanceID] = worker
	if err := worker.startLive(t.Context(), "live-session"); err != nil {
		t.Fatal(err)
	}
	_ = host.next(t)
	for range unhealthyFailureThreshold {
		worker.recordPublicationFailure(t.Context())
	}
	if health := handler.Health(t.Context()); health.Healthy {
		t.Fatalf("plugin health after sustained publication failures = %#v", health)
	}
	select {
	case value := <-host.observations:
		t.Fatalf("publication failure replaced valid live data: %#v", value)
	default:
	}
}

func TestReplaceInstancesPreservesLiveSessionForUnchangedAccount(t *testing.T) {
	host := &recordingQuotaHost{observations: make(chan protocol.Observation, 8)}
	factory := func(Config) quotaSource {
		return quotaSourceFunc(func(ctx context.Context) (Snapshot, error) {
			<-ctx.Done()
			return Snapshot{}, ctx.Err()
		})
	}
	handler := newHandler(host, factory, func() (string, error) { return "/tmp/codex", nil }, time.Now)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := handler.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	instances := []protocol.Instance{
		{ID: "main", Generation: 1, Config: json.RawMessage(`{"badge":"M"}`)},
		{ID: "work", Generation: 1, Config: json.RawMessage(`{"credentials_home":"/tmp/work","badge":"W"}`)},
	}
	if err := handler.ReplaceInstances(t.Context(), instances); err != nil {
		t.Fatal(err)
	}
	start := protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: "work", Generation: 1}, Action: "open", SessionToken: "work-live",
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher},
	}
	if err := handler.StartSession(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	if waiting := host.next(t); waiting.Channel != ChannelLive || waiting.Disposition != protocol.DispositionSnapshot {
		t.Fatalf("initial live observation = %#v", waiting)
	}

	instances[0].Generation = 2
	if err := handler.ReplaceInstances(t.Context(), instances); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-host.observations:
		t.Fatalf("unchanged account session was disturbed during replacement: %#v", value)
	default:
	}

	result, err := handler.HandleSessionInput(t.Context(), protocol.SessionInputRequest{
		Sequence: 1, OccurredAt: time.Now(), Instance: start.Instance, SessionToken: start.SessionToken,
		Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonBack, Action: protocol.ButtonPress}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != protocol.SessionInputConsumed {
		t.Fatalf("Back result = %#v", result)
	}
	resolved := host.next(t)
	if resolved.Channel != ChannelLive || resolved.Disposition != protocol.DispositionResolved {
		t.Fatalf("resolved live observation = %#v", resolved)
	}
	host.mu.Lock()
	completions := append([]protocol.CompleteSessionRequest(nil), host.completions...)
	host.mu.Unlock()
	if !reflect.DeepEqual(completions, []protocol.CompleteSessionRequest{{Instance: start.Instance, SessionToken: start.SessionToken}}) {
		t.Fatalf("session completions = %#v", completions)
	}
}

func TestAccountSetChangesUpdateBadgesWithoutResettingRetainedRevisions(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	host := &recordingQuotaHost{observations: make(chan protocol.Observation, 32)}
	factory := func(Config) quotaSource {
		return quotaSourceFunc(func(ctx context.Context) (Snapshot, error) { <-ctx.Done(); return Snapshot{}, ctx.Err() })
	}
	handler := newHandler(host, factory, func() (string, error) { return "/tmp/codex", nil }, func() time.Time { return now })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := handler.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	main := protocol.Instance{ID: "main", Generation: 1, Config: json.RawMessage(`{"badge":"M"}`)}
	work := protocol.Instance{ID: "work", Generation: 1, Config: json.RawMessage(`{"credentials_home":"/tmp/work","badge":"W"}`)}
	snapshot := Snapshot{UpdatedAt: now, Windows: []Window{{Duration: 5 * time.Hour, RemainingPercent: 80, ResetsAt: now.Add(time.Hour)}}}
	for index, test := range []struct {
		instances []protocol.Instance
		label     string
	}{
		{[]protocol.Instance{main}, "5H"},
		{[]protocol.Instance{main, work}, "M 5H"},
		{[]protocol.Instance{main}, "5H"},
	} {
		if err := handler.ReplaceInstances(t.Context(), test.instances); err != nil {
			t.Fatal(err)
		}
		handler.mu.RLock()
		worker := handler.workers["main"]
		handler.mu.RUnlock()
		// The provider is blocked until shutdown, so this is the only publisher.
		if err := worker.publish(t.Context(), snapshot, now); err != nil {
			t.Fatal(err)
		}
		value := nextQuotaObservation(t, host, ChannelSummary)
		if got := sceneText(value, "front-window-label"); got != test.label || value.Revision != uint64(index+1) {
			t.Fatalf("account set %d: label=%q revision=%d, want %q/%d", index, got, value.Revision, test.label, index+1)
		}
	}
}

func TestDefinitionForVersionUsesReleaseBuildMetadata(t *testing.T) {
	t.Parallel()
	definition := DefinitionForVersion("9.8.7")
	if definition.ID != PluginID || definition.Version != "9.8.7" {
		t.Fatalf("release definition identity = %q/%q", definition.ID, definition.Version)
	}
	if got := DefinitionForVersion(PluginVersion).Version; got != PluginVersion {
		t.Fatalf("default definition version = %q, want %q", got, PluginVersion)
	}
	if protocol.Version != "1.0" {
		t.Fatalf("protocol version = %q", protocol.Version)
	}
}

func TestHandlerPublishesIndependentMultiAccountSummaryAndPressure(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	host := &recordingQuotaHost{observations: make(chan protocol.Observation, 8)}
	sources := map[string]*gatedQuotaSource{}
	var sourcesMu sync.Mutex
	factory := func(config Config) quotaSource {
		source := newGatedQuotaSource()
		sourcesMu.Lock()
		sources[config.CredentialsHome] = source
		sourcesMu.Unlock()
		return source
	}
	mainHome := filepath.Join(string(filepath.Separator), "Users", "tester", ".codex")
	handler := newHandler(host, factory, func() (string, error) { return mainHome, nil }, func() time.Time { return now })
	instances := []protocol.Instance{
		{ID: "main", Generation: 1, Config: json.RawMessage(`{"label":"MAIN","badge":"M"}`)},
		{ID: "work", Generation: 1, Config: json.RawMessage(`{"credentials_home":"~/work","label":"WORK","badge":"W"}`)},
	}
	if err := handler.ReplaceInstances(context.Background(), instances); err != nil {
		t.Fatal(err)
	}
	sourcesMu.Lock()
	mainSource := sources[mainHome]
	workSource := sources[filepath.Join(filepath.Dir(mainHome), "work")]
	sourcesMu.Unlock()
	if mainSource == nil || workSource == nil {
		t.Fatalf("sources = %#v", sources)
	}
	mainSource.reply(t, Snapshot{UpdatedAt: now, Windows: []Window{{Duration: 5 * time.Hour, RemainingPercent: 80, ResetsAt: now.Add(time.Hour)}}}, nil)
	workSource.reply(t, Snapshot{UpdatedAt: now, Windows: []Window{{Duration: 7 * 24 * time.Hour, RemainingPercent: 20, ResetsAt: now.Add(time.Hour)}}}, nil)

	values := []protocol.Observation{host.next(t), host.next(t), host.next(t)}
	sort.Slice(values, func(i, j int) bool {
		if values[i].Instance.ID == values[j].Instance.ID {
			return values[i].Channel < values[j].Channel
		}
		return values[i].Instance.ID < values[j].Instance.ID
	})
	if values[0].Instance.ID != "main" || values[0].Channel != ChannelSummary || values[0].Disposition != protocol.DispositionNotable {
		t.Fatalf("main summary = %#v", values[0])
	}
	if values[1].Instance.ID != "work" || values[1].Channel != ChannelPressure || values[1].Disposition != protocol.DispositionNotable || values[1].Impact != protocol.ImpactNotable {
		t.Fatalf("work pressure = %#v", values[1])
	}
	if values[2].Instance.ID != "work" || values[2].Channel != ChannelSummary {
		t.Fatalf("work summary = %#v", values[2])
	}
	if err := handler.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerCriticalRecoveryResolvesPressure(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	host := &recordingQuotaHost{observations: make(chan protocol.Observation, 8)}
	worker := &accountWorker{
		instanceID: "main", config: defaultConfig("/tmp/codex"), host: host, now: func() time.Time { return now },
	}
	critical := Snapshot{UpdatedAt: now, Windows: []Window{{Duration: 5 * time.Hour, RemainingPercent: 5, ResetsAt: now.Add(time.Hour)}}}
	if err := worker.publish(context.Background(), critical, now); err != nil {
		t.Fatal(err)
	}
	_ = host.next(t)
	pressure := host.next(t)
	if pressure.Disposition != protocol.DispositionActionable || pressure.Impact != protocol.ImpactCritical || pressure.ReasonCode != "codex_quota_critical" {
		t.Fatalf("critical pressure = %#v", pressure)
	}
	now = now.Add(time.Minute)
	recovered := Snapshot{UpdatedAt: now, Windows: []Window{{Duration: 5 * time.Hour, RemainingPercent: 100, ResetsAt: now.Add(5 * time.Hour)}}}
	if err := worker.publish(context.Background(), recovered, now); err != nil {
		t.Fatal(err)
	}
	_ = host.next(t)
	resolved := host.next(t)
	if resolved.Disposition != protocol.DispositionResolved || resolved.ReasonCode != "codex_quota_recovered" || resolved.Scene != nil {
		t.Fatalf("resolved = %#v", resolved)
	}
}

func TestWorkerPublishesOneRotatableSummaryPerWindowAndResolvesRemovedWindow(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	host := &recordingQuotaHost{observations: make(chan protocol.Observation, 12)}
	worker := &accountWorker{
		instanceID: "main", config: defaultConfig("/tmp/codex"), host: host, now: func() time.Time { return now },
	}
	initial := Snapshot{UpdatedAt: now, Windows: []Window{
		{Duration: 5 * time.Hour, RemainingPercent: 80, ResetsAt: now.Add(time.Hour)},
		{Duration: 7 * 24 * time.Hour, RemainingPercent: 10, ResetsAt: now.Add(24 * time.Hour)},
	}}
	if err := worker.publish(context.Background(), initial, now); err != nil {
		t.Fatal(err)
	}
	values := []protocol.Observation{host.next(t), host.next(t), host.next(t)}
	byIdentity := make(map[string]protocol.Observation, len(values))
	for _, value := range values {
		byIdentity[value.Channel+"/"+value.Key] = value
	}
	fiveHour := byIdentity[ChannelSummary+"/quota-5h"]
	weekly := byIdentity[ChannelSummary+"/quota-1w"]
	pressure := byIdentity[ChannelPressure+"/"+observationKey]
	if got := sceneText(fiveHour, "front-window-label"); got != "5H" {
		t.Fatalf("5-hour front label = %q", got)
	}
	if got := sceneText(fiveHour, "front-window-state"); got != "LEFT" {
		t.Fatalf("5-hour front state = %q", got)
	}
	if got := sceneText(weekly, "front-window-label"); got != "1W" {
		t.Fatalf("weekly front label = %q", got)
	}
	if got := sceneText(pressure, "front-window-label"); got != "1W" {
		t.Fatalf("pressure front window = %q", got)
	}
	if got := sceneText(pressure, "front-window-state"); got != "LOW" {
		t.Fatalf("pressure front state = %q", got)
	}

	now = now.Add(time.Minute)
	recovered := Snapshot{UpdatedAt: now, Windows: []Window{{
		Duration: 5 * time.Hour, RemainingPercent: 90, ResetsAt: now.Add(4 * time.Hour),
	}}}
	if err := worker.publish(context.Background(), recovered, now); err != nil {
		t.Fatal(err)
	}
	values = []protocol.Observation{host.next(t), host.next(t), host.next(t)}
	byIdentity = make(map[string]protocol.Observation, len(values))
	for _, value := range values {
		byIdentity[value.Channel+"/"+value.Key] = value
	}
	if got := byIdentity[ChannelSummary+"/quota-1w"].Disposition; got != protocol.DispositionResolved {
		t.Fatalf("removed weekly disposition = %q, want resolved", got)
	}
	if got := byIdentity[ChannelPressure+"/"+observationKey].Disposition; got != protocol.DispositionResolved {
		t.Fatalf("recovered pressure disposition = %q, want resolved", got)
	}
}

func TestHandlerBoundsProviderConcurrencyAcrossAccounts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := &recordingQuotaHost{observations: make(chan protocol.Observation, 8)}
		started := make(chan string, 3)
		release := make(chan struct{}, 1)
		factory := func(config Config) quotaSource {
			return quotaSourceFunc(func(ctx context.Context) (Snapshot, error) {
				started <- config.Badge
				select {
				case <-release:
					return Snapshot{UpdatedAt: time.Now(), Windows: []Window{{Duration: 5 * time.Hour, RemainingPercent: 80, ResetsAt: time.Now().Add(time.Hour)}}}, nil
				case <-ctx.Done():
					return Snapshot{}, ctx.Err()
				}
			})
		}
		handler := newHandler(host, factory, func() (string, error) { return "/tmp/codex", nil }, time.Now)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := handler.Shutdown(ctx); err != nil {
				t.Error(err)
			}
		})
		instances := []protocol.Instance{
			{ID: "one", Generation: 1, Config: json.RawMessage(`{"credentials_home":"/tmp/one","badge":"A"}`)},
			{ID: "two", Generation: 1, Config: json.RawMessage(`{"credentials_home":"/tmp/two","badge":"B"}`)},
			{ID: "three", Generation: 1, Config: json.RawMessage(`{"credentials_home":"/tmp/three","badge":"C"}`)},
		}
		if err := handler.ReplaceInstances(t.Context(), instances); err != nil {
			t.Fatal(err)
		}
		// All workers reach a blocking point before admission is counted.
		synctest.Wait()
		if got := len(started); got != 2 {
			t.Fatalf("provider calls before releasing a slot = %d, want 2", got)
		}
		<-started
		<-started
		release <- struct{}{}
		synctest.Wait()
		if got := len(started); got != 1 {
			t.Fatalf("new provider calls after releasing a slot = %d, want 1", got)
		}
	})
}

func TestHandlerHealthTracksSustainedPerAccountFailuresAndRecovery(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	handler := newHandler(nil, nil, func() (string, error) { return "/tmp/codex", nil }, func() time.Time { return now })
	worker := &accountWorker{instanceID: "private-account-name", now: func() time.Time { return now }}
	handler.workers[worker.instanceID] = worker

	for attempt := 1; attempt < unhealthyFailureThreshold; attempt++ {
		worker.recordFailure(context.Background(), errors.New("token=secret /Users/private/account"))
		if health := handler.Health(context.Background()); !health.Healthy {
			t.Fatalf("health became unhealthy after %d failures: %#v", attempt, health)
		}
	}
	worker.recordFailure(context.Background(), errors.New("token=secret /Users/private/account"))
	if health := handler.Health(context.Background()); health.Healthy || !health.ObservedAt.Equal(now) {
		t.Fatalf("health after threshold failures = %#v", health)
	}

	worker.recordSuccess(context.Background())
	if health := handler.Health(context.Background()); !health.Healthy || !health.ObservedAt.Equal(now) {
		t.Fatalf("health after recovery = %#v", health)
	}
}

func TestHandlerHealthAggregatesAllAccounts(t *testing.T) {
	t.Parallel()
	handler := newHandler(nil, nil, func() (string, error) { return "/tmp/codex", nil }, time.Now)
	healthy := &accountWorker{instanceID: "healthy"}
	unhealthy := &accountWorker{instanceID: "unhealthy"}
	handler.workers[healthy.instanceID] = healthy
	handler.workers[unhealthy.instanceID] = unhealthy
	for range unhealthyFailureThreshold {
		unhealthy.recordFailure(context.Background(), errors.New("unavailable"))
	}
	if got := handler.Health(context.Background()); got.Healthy {
		t.Fatalf("aggregate health = %#v, want unhealthy", got)
	}
}

func TestAccountHealthTransitionsEmitRedactedPerInstanceDiagnostics(t *testing.T) {
	t.Parallel()
	host := &recordingQuotaHost{observations: make(chan protocol.Observation, 1)}
	worker := &accountWorker{instanceID: "codex-secondary", host: host}
	for range unhealthyFailureThreshold {
		worker.recordFailure(context.Background(), errors.New("token=secret /Users/private provider.invalid"))
	}
	worker.recordSuccess(context.Background())
	host.mu.Lock()
	logs := append([]protocol.LogNotification(nil), host.logs...)
	host.mu.Unlock()
	if len(logs) != 3 || logs[0].Instance.ID != "codex-secondary" || logs[1].Event != "codex_quota_unhealthy" || logs[2].Event != "codex_quota_recovered" {
		t.Fatalf("health transition logs = %#v", logs)
	}
	for _, value := range logs {
		encoded, _ := json.Marshal(value)
		if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "/Users/") || strings.Contains(string(encoded), "provider.invalid") {
			t.Fatalf("unsafe health diagnostic = %s", encoded)
		}
	}
}

type quotaSourceFunc func(context.Context) (Snapshot, error)

func (function quotaSourceFunc) Fetch(ctx context.Context) (Snapshot, error) {
	return function(ctx)
}

type gatedQuotaSource struct {
	calls chan chan quotaResult
}

type quotaResult struct {
	snapshot Snapshot
	err      error
}

func newGatedQuotaSource() *gatedQuotaSource {
	return &gatedQuotaSource{calls: make(chan chan quotaResult, 1)}
}

func (s *gatedQuotaSource) Fetch(ctx context.Context) (Snapshot, error) {
	reply := make(chan quotaResult, 1)
	select {
	case s.calls <- reply:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
	select {
	case result := <-reply:
		return result.snapshot, result.err
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
}

func (s *gatedQuotaSource) reply(t *testing.T, snapshot Snapshot, err error) {
	t.Helper()
	select {
	case reply := <-s.calls:
		reply <- quotaResult{snapshot: snapshot, err: err}
	case <-time.After(time.Second):
		t.Fatal("source was not called")
	}
}

type recordingQuotaHost struct {
	observations chan protocol.Observation
	mu           sync.Mutex
	logs         []protocol.LogNotification
	completions  []protocol.CompleteSessionRequest
	failChannel  string
}

func (h *recordingQuotaHost) PublishObservation(ctx context.Context, value protocol.Observation) error {
	select {
	case h.observations <- value:
		h.mu.Lock()
		fail := value.Channel == h.failChannel
		h.mu.Unlock()
		if fail {
			return errors.New("publication failed")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *recordingQuotaHost) Log(_ context.Context, value protocol.LogNotification) error {
	h.mu.Lock()
	h.logs = append(h.logs, value)
	h.mu.Unlock()
	return nil
}

func (h *recordingQuotaHost) CompleteSession(_ context.Context, value protocol.CompleteSessionRequest) error {
	h.mu.Lock()
	h.completions = append(h.completions, value)
	h.mu.Unlock()
	return nil
}

func (h *recordingQuotaHost) next(t *testing.T) protocol.Observation {
	t.Helper()
	select {
	case value := <-h.observations:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for observation")
		return protocol.Observation{}
	}
}

func nextQuotaObservation(t *testing.T, host *recordingQuotaHost, channel string) protocol.Observation {
	t.Helper()
	for {
		value := host.next(t)
		if value.Channel == channel {
			return value
		}
	}
}

func sceneText(value protocol.Observation, id string) string {
	if value.Scene == nil {
		return ""
	}
	for _, element := range value.Scene.Elements {
		if element.ID == id {
			if element.Text != nil {
				return element.Text.Value
			}
		}
	}
	return ""
}
