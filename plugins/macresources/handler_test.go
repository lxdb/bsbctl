package macresources

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestDefinitionDeclaresResidentAndInteractiveResourceChannels(t *testing.T) {
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

func TestMacLiveSessionPublishesEveryReadingAndSurvivesTransientFailures(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	config, err := decodeConfig(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	host := &recordingHost{observations: make(chan protocol.Observation, 16)}
	handler := newHandler(host, nil, nil)
	worker := &worker{instanceID: AppID, generation: 7, config: config, host: host, owner: handler, pressure: newPressureMachine(config)}
	handler.worker = worker
	start := protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: AppID, Generation: 7}, Action: "open", SessionToken: "live-session",
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher},
	}
	if err := handler.StartSession(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	waiting := host.nextObservation(t)
	if waiting.Channel != ChannelLive || waiting.Key != "panel" || waiting.Revision != 1 || macSceneText(waiting, "front-status") != "WAITING" {
		t.Fatalf("waiting live observation = %#v", waiting)
	}

	first := reading{CPUPercent: 20, MemoryPercent: 30, RXBytesPerSecond: 1024, TXBytesPerSecond: 2048}
	if err := worker.publish(t.Context(), now, first); err != nil {
		t.Fatal(err)
	}
	live := nextMacObservation(t, host, ChannelLive)
	if live.Revision != 2 || macSceneText(live, "front-cpu-value") != "20%" {
		t.Fatalf("first live reading = %#v", live)
	}
	second := reading{CPUPercent: 21, MemoryPercent: 31, RXBytesPerSecond: 2048, TXBytesPerSecond: 4096}
	if err := worker.publish(t.Context(), now.Add(time.Second), second); err != nil {
		t.Fatal(err)
	}
	live = nextMacObservation(t, host, ChannelLive)
	if live.Revision != 3 || macSceneText(live, "front-cpu-value") != "21%" {
		t.Fatalf("unsuppressed live reading = %#v", live)
	}
	host.assertNoObservation(t)

	handler.recordFailure(t.Context(), AppID, 7, "collection_failed")
	handler.recordFailure(t.Context(), AppID, 7, "collection_failed")
	host.assertNoObservation(t)
	handler.recordFailure(t.Context(), AppID, 7, "collection_failed")
	unavailable := nextMacObservation(t, host, ChannelLive)
	if unavailable.Revision != 4 || macSceneText(unavailable, "front-status") != "UNAVAILABLE" {
		t.Fatalf("unavailable live observation = %#v", unavailable)
	}

	if err := worker.publish(t.Context(), now.Add(2*time.Second), second); err != nil {
		t.Fatal(err)
	}
	recovered := nextMacObservation(t, host, ChannelLive)
	if recovered.Revision != 5 || macSceneText(recovered, "front-cpu-value") != "21%" {
		t.Fatalf("recovered live reading = %#v", recovered)
	}
	handler.recordSuccess(t.Context(), AppID, 7)
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
	host.assertNoObservation(t)
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
	resolved := nextMacObservation(t, host, ChannelLive)
	if resolved.Revision != 6 || resolved.Disposition != protocol.DispositionResolved || resolved.Scene != nil {
		t.Fatalf("resolved live observation = %#v", resolved)
	}
	host.mu.Lock()
	completions := append([]protocol.CompleteSessionRequest(nil), host.completions...)
	host.mu.Unlock()
	if !reflect.DeepEqual(completions, []protocol.CompleteSessionRequest{{Instance: start.Instance, SessionToken: start.SessionToken}}) {
		t.Fatalf("session completions = %#v", completions)
	}
}

func TestMacLiveSessionRejectsStaleGeneration(t *testing.T) {
	host := &recordingHost{observations: make(chan protocol.Observation, 1)}
	handler := newHandler(host, nil, nil)
	handler.worker = &worker{instanceID: AppID, generation: 7, host: host}
	err := handler.StartSession(t.Context(), protocol.SessionStartRequest{
		Instance: protocol.InstanceRef{ID: AppID, Generation: 6}, Action: "open", SessionToken: "stale-session",
		Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher},
	})
	if err == nil {
		t.Fatal("stale generation started a live session")
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
}

func TestWorkerPublishesSummaryOnlyInitiallyPeriodicallyOrAfterMaterialChange(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	config, err := decodeConfig(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	host := &recordingHost{observations: make(chan protocol.Observation, 8)}
	worker := &worker{instanceID: AppID, generation: 1, config: config, host: host, pressure: newPressureMachine(config)}

	if err := worker.publish(context.Background(), now, reading{CPUPercent: 20, MemoryPercent: 30}); err != nil {
		t.Fatal(err)
	}
	initial := host.nextObservation(t)
	if initial.Channel != ChannelSummary || initial.Revision != 1 || !initial.ValidUntil.Equal(now.Add(10*time.Second)) {
		t.Fatalf("initial summary = %#v", initial)
	}
	assertText(t, initial.Scene, "front-cpu-value", "20%")
	assertText(t, initial.Scene, "front-mem-value", "30%")

	for _, test := range []struct {
		at    time.Duration
		value reading
	}{
		{at: 2 * time.Second, value: reading{CPUPercent: 30, MemoryPercent: 30}},
		{at: 89 * time.Second, value: reading{CPUPercent: 60, MemoryPercent: 30}},
	} {
		if err := worker.publish(context.Background(), now.Add(test.at), test.value); err != nil {
			t.Fatal(err)
		}
		host.assertNoObservation(t)
	}

	materialAt := now.Add(90 * time.Second)
	if err := worker.publish(context.Background(), materialAt, reading{CPUPercent: 35, MemoryPercent: 30}); err != nil {
		t.Fatal(err)
	}
	material := host.nextObservation(t)
	if material.Channel != ChannelSummary || material.Revision != 2 || !material.ObservedAt.Equal(materialAt) {
		t.Fatalf("material summary = %#v", material)
	}

	if err := worker.publish(context.Background(), materialAt.Add(179*time.Second), reading{CPUPercent: 35, MemoryPercent: 30}); err != nil {
		t.Fatal(err)
	}
	host.assertNoObservation(t)

	periodicAt := materialAt.Add(3 * time.Minute)
	if err := worker.publish(context.Background(), periodicAt, reading{CPUPercent: 35, MemoryPercent: 30}); err != nil {
		t.Fatal(err)
	}
	periodic := host.nextObservation(t)
	if periodic.Channel != ChannelSummary || periodic.Revision != 3 || !periodic.ObservedAt.Equal(periodicAt) {
		t.Fatalf("periodic summary = %#v", periodic)
	}
}

func TestMaterialResourceChangeUsesCPUThenMemoryThenNetworkUtilization(t *testing.T) {
	config, err := decodeConfig(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	baseline := reading{CPUPercent: 20, MemoryPercent: 30, RXBytesPerSecond: 1024, TXBytesPerSecond: 1024}
	tests := []struct {
		name  string
		value reading
		want  bool
	}{
		{name: "below threshold", value: reading{CPUPercent: 34.9, MemoryPercent: 44.9, RXBytesPerSecond: 1024, TXBytesPerSecond: 1024}},
		{name: "cpu", value: reading{CPUPercent: 35, MemoryPercent: 30, RXBytesPerSecond: 1024, TXBytesPerSecond: 1024}, want: true},
		{name: "memory", value: reading{CPUPercent: 20, MemoryPercent: 45, RXBytesPerSecond: 1024, TXBytesPerSecond: 1024}, want: true},
		{name: "network", value: reading{CPUPercent: 20, MemoryPercent: 30, RXBytesPerSecond: 819200, TXBytesPerSecond: 819200}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := materiallyChanged(baseline, test.value, config); got != test.want {
				t.Fatalf("materiallyChanged = %v, want %v", got, test.want)
			}
		})
	}
}

func TestWorkerPublishesBriefPressureTransitionsAndPeriodicReminders(t *testing.T) {
	now := time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)
	config, err := decodeConfig(json.RawMessage(`{"sustain_samples":1}`))
	if err != nil {
		t.Fatal(err)
	}
	host := &recordingHost{observations: make(chan protocol.Observation, 12)}
	worker := &worker{instanceID: AppID, generation: 1, config: config, host: host, pressure: newPressureMachine(config)}

	if err := worker.publish(context.Background(), now, reading{CPUPercent: 20}); err != nil {
		t.Fatal(err)
	}
	_ = host.nextObservation(t)

	warningAt := now.Add(time.Second)
	if err := worker.publish(context.Background(), warningAt, reading{CPUPercent: 80}); err != nil {
		t.Fatal(err)
	}
	warning := host.nextObservation(t)
	if warning.Channel != ChannelPressure || warning.Disposition != protocol.DispositionNotable || warning.Revision != 1 || !warning.ValidUntil.Equal(warningAt.Add(10*time.Second)) {
		t.Fatalf("warning = %#v", warning)
	}
	assertText(t, warning.Scene, "back-status", "MAC WARN")
	assertText(t, warning.Scene, "back-reason", "CPU")
	assertText(t, warning.Scene, "front-cpu-marker", "!")

	if err := worker.publish(context.Background(), warningAt.Add(time.Second), reading{CPUPercent: 80}); err != nil {
		t.Fatal(err)
	}
	host.assertNoObservation(t)

	criticalAt := warningAt.Add(2 * time.Second)
	if err := worker.publish(context.Background(), criticalAt, reading{CPUPercent: 95}); err != nil {
		t.Fatal(err)
	}
	critical := host.nextObservation(t)
	if critical.Channel != ChannelPressure || critical.Disposition != protocol.DispositionActionable || critical.Revision != 2 {
		t.Fatalf("critical = %#v", critical)
	}
	assertText(t, critical.Scene, "back-status", "MAC CRIT")
	assertText(t, critical.Scene, "back-reason", "CPU")
	assertText(t, critical.Scene, "front-cpu-marker", "!!")

	reminderAt := criticalAt.Add(config.SummaryInterval)
	if err := worker.publish(context.Background(), reminderAt, reading{CPUPercent: 95}); err != nil {
		t.Fatal(err)
	}
	reminder := host.nextObservation(t)
	if reminder.Channel != ChannelPressure || reminder.Disposition != protocol.DispositionActionable || reminder.Revision != 3 || !reminder.ObservedAt.Equal(reminderAt) {
		t.Fatalf("pressure reminder = %#v", reminder)
	}

	resolvedAt := reminderAt.Add(time.Second)
	if err := worker.publish(context.Background(), resolvedAt, reading{CPUPercent: 20}); err != nil {
		t.Fatal(err)
	}
	resolved := host.nextObservation(t)
	if resolved.Channel != ChannelPressure || resolved.Disposition != protocol.DispositionResolved || resolved.Revision != 4 || resolved.Scene != nil || !resolved.ValidUntil.IsZero() {
		t.Fatalf("resolved pressure = %#v", resolved)
	}
	host.assertNoObservation(t)

	if err := worker.publish(context.Background(), resolvedAt.Add(89*time.Second), reading{CPUPercent: 20, MemoryPercent: 40}); err != nil {
		t.Fatal(err)
	}
	host.assertNoObservation(t)
	if err := worker.publish(context.Background(), resolvedAt.Add(90*time.Second), reading{CPUPercent: 20, MemoryPercent: 15}); err != nil {
		t.Fatal(err)
	}
	afterRecovery := host.nextObservation(t)
	if afterRecovery.Channel != ChannelSummary || afterRecovery.Revision != 2 {
		t.Fatalf("summary after recovery baseline = %#v", afterRecovery)
	}
}

func TestHandlerPublishesSummaryAndSustainedPressureFromOneSampleStream(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	collector := newGatedCollector()
	ticker := &manualTicker{ticks: make(chan time.Time, 4)}
	host := &recordingHost{observations: make(chan protocol.Observation, 8)}
	handler := newHandler(host, collector, func(time.Duration) Ticker { return ticker })

	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{
		ID: AppID, Generation: 7, Config: json.RawMessage(`{"sustain_samples":3}`),
	}}); err != nil {
		t.Fatalf("ReplaceInstances: %v", err)
	}
	collector.reply(t, RawSample{CPUTotal: 100, CPUIdle: 50, MemoryPercent: 60, RXBytes: 1000, TXBytes: 500, CollectedAt: now})

	for index := 1; index <= 3; index++ {
		tickAt := now.Add(time.Duration(index) * 2 * time.Second)
		ticker.ticks <- tickAt
		collector.reply(t, RawSample{
			CPUTotal: uint64(100 + 100*index), CPUIdle: uint64(50 + 5*index), MemoryPercent: 60,
			RXBytes: uint64(1000 + 1024*index), TXBytes: uint64(500 + 1024*index), CollectedAt: tickAt,
		})
		switch index {
		case 1:
			summary := host.nextObservation(t)
			if summary.Channel != ChannelSummary || summary.ReasonCode != "resource_snapshot" || summary.Disposition != protocol.DispositionNotable {
				t.Fatalf("initial summary = %#v", summary)
			}
			if summary.ValidUntil.Sub(tickAt) != 10*time.Second {
				t.Fatalf("summary expiry = %v", summary.ValidUntil)
			}
		case 2:
			host.assertNoObservation(t)
		case 3:
			pressure := host.nextObservation(t)
			if pressure.Channel != ChannelPressure || pressure.ReasonCode != "cpu_pressure" || pressure.Disposition != protocol.DispositionActionable || pressure.Impact != protocol.ImpactCritical {
				t.Fatalf("pressure = %#v", pressure)
			}
		}
	}
	var pressure protocol.Observation
	for index := 1; index <= 3; index++ {
		tickAt := now.Add(time.Duration(3+index) * 2 * time.Second)
		ticker.ticks <- tickAt
		collector.reply(t, RawSample{
			CPUTotal: uint64(400 + 100*index), CPUIdle: uint64(65 + 95*index), MemoryPercent: 60,
			RXBytes: uint64(4000 + 1024*index), TXBytes: uint64(3500 + 1024*index), CollectedAt: tickAt,
		})
		if index < 3 {
			host.assertNoObservation(t)
		} else {
			pressure = host.nextObservation(t)
		}
	}
	if pressure.Disposition != protocol.DispositionResolved || pressure.ReasonCode != "pressure_resolved" || pressure.Scene != nil || !pressure.ValidUntil.IsZero() {
		t.Fatalf("resolved pressure = %#v", pressure)
	}

	if err := handler.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !ticker.stopped() {
		t.Fatal("ticker was not stopped")
	}
}

func TestHandlerRejectsMultipleEnabledInstancesWithoutReplacingCurrentWorker(t *testing.T) {
	collector := newGatedCollector()
	ticker := &manualTicker{ticks: make(chan time.Time, 1)}
	handler := newHandler(&recordingHost{observations: make(chan protocol.Observation, 1)}, collector, func(time.Duration) Ticker { return ticker })
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: AppID, Generation: 1, Config: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	collector.reply(t, RawSample{CPUTotal: 1, CollectedAt: time.Now()})
	err := handler.ReplaceInstances(context.Background(), []protocol.Instance{
		{ID: "one", Generation: 2, Config: json.RawMessage(`{}`)},
		{ID: "two", Generation: 2, Config: json.RawMessage(`{}`)},
	})
	if err == nil {
		t.Fatal("ReplaceInstances accepted multiple instances")
	}
	if ticker.stopped() {
		t.Fatal("invalid replacement stopped the current worker")
	}
	if err := handler.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerPreservesRevisionsOnlyForSameInstanceGeneration(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	collector := newGatedCollector()
	tickers := make(chan Ticker, 3)
	first := &manualTicker{ticks: make(chan time.Time, 1)}
	second := &manualTicker{ticks: make(chan time.Time, 1)}
	third := &manualTicker{ticks: make(chan time.Time, 1)}
	tickers <- first
	tickers <- second
	tickers <- third
	host := &recordingHost{observations: make(chan protocol.Observation, 4)}
	handler := newHandler(host, collector, func(time.Duration) Ticker { return <-tickers })
	configure := func(generation uint64) {
		t.Helper()
		if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: AppID, Generation: generation, Config: json.RawMessage(`{}`)}}); err != nil {
			t.Fatalf("ReplaceInstances generation %d: %v", generation, err)
		}
		collector.reply(t, RawSample{CPUTotal: generation * 100, CPUIdle: generation * 50, CollectedAt: now})
	}
	publish := func(ticker *manualTicker, total uint64) uint64 {
		t.Helper()
		ticker.ticks <- now.Add(2 * time.Second)
		collector.reply(t, RawSample{CPUTotal: total, CPUIdle: total / 2, CollectedAt: now.Add(2 * time.Second)})
		return host.nextObservation(t).Revision
	}

	configure(1)
	if revision := publish(first, 200); revision != 1 {
		t.Fatalf("first revision = %d", revision)
	}
	configure(1)
	if revision := publish(second, 200); revision != 2 {
		t.Fatalf("same-generation revision = %d, want 2", revision)
	}
	configure(2)
	if revision := publish(third, 300); revision != 1 {
		t.Fatalf("new-generation revision = %d, want 1", revision)
	}
	if err := handler.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerShutdownCancelsAndJoinsBlockedCollection(t *testing.T) {
	collector := &cancelCollector{started: make(chan struct{}), done: make(chan struct{})}
	handler := newHandler(&recordingHost{observations: make(chan protocol.Observation, 1)}, collector, nil)
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: AppID, Generation: 1, Config: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	<-collector.started
	if err := handler.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-collector.done:
	default:
		t.Fatal("Shutdown returned before collector exited")
	}
}

func TestCanceledReplacementRetainsWorkerOwnershipUntilItCanJoin(t *testing.T) {
	collector := &stubbornCollector{started: make(chan struct{}), release: make(chan struct{})}
	ticker := &manualTicker{ticks: make(chan time.Time)}
	handler := newHandler(&recordingHost{observations: make(chan protocol.Observation, 1)}, collector, func(time.Duration) Ticker { return ticker })
	instance := []protocol.Instance{{ID: AppID, Generation: 1, Config: json.RawMessage(`{}`)}}
	if err := handler.ReplaceInstances(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	<-collector.started
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := handler.ReplaceInstances(canceled, instance); !errors.Is(err, context.Canceled) {
		t.Fatalf("replacement error = %v, want context.Canceled", err)
	}
	handler.mu.RLock()
	retained := handler.worker != nil
	handler.mu.RUnlock()
	if !retained {
		t.Fatal("canceled replacement orphaned the unjoined worker")
	}
	close(collector.release)
	if err := handler.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerHealthBecomesUnhealthyAfterThreeCollectionFailuresAndRecovers(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	collector := newGatedCollector()
	ticker := &manualTicker{ticks: make(chan time.Time, 4)}
	host := &recordingHost{observations: make(chan protocol.Observation, 2)}
	handler := newHandler(host, collector, func(time.Duration) Ticker { return ticker })
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: AppID, Generation: 1, Config: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	collector.replyError(t, errors.New("sample failed"))
	for index := 1; index < 3; index++ {
		ticker.ticks <- now.Add(time.Duration(index) * time.Second)
		collector.replyError(t, errors.New("sample failed"))
	}
	ticker.ticks <- now.Add(3 * time.Second)
	var recoveryReply chan collectorResult
	select {
	case recoveryReply = <-collector.requests:
	case <-time.After(time.Second):
		t.Fatal("collector did not begin the recovery sample")
	}
	if got := handler.Health(context.Background()); got.Healthy {
		t.Fatalf("health after three failures = %#v", got)
	}
	recoveryReply <- collectorResult{sample: RawSample{CPUTotal: 100, CPUIdle: 50, CollectedAt: now.Add(3 * time.Second)}}
	ticker.ticks <- now.Add(4 * time.Second)
	select {
	case <-collector.requests:
	case <-time.After(time.Second):
		t.Fatal("collector recovery was not processed")
	}
	if got := handler.Health(context.Background()); !got.Healthy {
		t.Fatalf("health after recovery = %#v", got)
	}
	if got := host.logEvents(); len(got) != 2 || got[0] != "collection_failed" || got[1] != "collection_recovered" {
		t.Fatalf("log events = %v", got)
	}
	if err := handler.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerPublicationFailuresReportPluginUnhealthyWithoutMislabelingCollection(t *testing.T) {
	now := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	collector := newGatedCollector()
	ticker := &manualTicker{ticks: make(chan time.Time, 8)}
	host := &publicationHealthHost{fail: true}
	handler := newHandler(host, collector, func(time.Duration) Ticker { return ticker })
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: AppID, Generation: 1, Config: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	collector.reply(t, RawSample{CPUTotal: 100, CPUIdle: 50, CollectedAt: now})
	for index := 1; index <= 3; index++ {
		ticker.ticks <- now.Add(time.Duration(index) * time.Second)
		collector.reply(t, RawSample{CPUTotal: uint64(100 + 100*index), CPUIdle: uint64(50 + 50*index), CollectedAt: now.Add(time.Duration(index) * time.Second)})
	}

	ticker.ticks <- now.Add(4 * time.Second)
	recoveryReply := awaitCollectorRequest(t, collector)
	if got := handler.Health(context.Background()); got.Healthy {
		t.Fatalf("plugin health after three publication failures = %#v", got)
	}
	host.setFail(false)
	recoveryReply <- collectorResult{sample: RawSample{CPUTotal: 500, CPUIdle: 250, CollectedAt: now.Add(4 * time.Second)}}

	ticker.ticks <- now.Add(5 * time.Second)
	_ = awaitCollectorRequest(t, collector)
	if got := handler.Health(context.Background()); !got.Healthy {
		t.Fatalf("health after complete recovery cycle = %#v", got)
	}
	if got := host.logEvents(); len(got) != 2 || got[0] != "publication_failed" || got[1] != "publication_recovered" {
		t.Fatalf("transition log events = %v", got)
	}
	if err := handler.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerRetriesExactFailedPressureResolutionBeforeHealthRecovery(t *testing.T) {
	now := time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC)
	collector := newGatedCollector()
	ticker := &manualTicker{ticks: make(chan time.Time, 8)}
	host := &pressureRetryHost{attempts: make(chan protocol.Observation, 16), resolutionFailures: 3}
	handler := newHandler(host, collector, func(time.Duration) Ticker { return ticker })
	if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{
		ID: AppID, Generation: 1, Config: json.RawMessage(`{"sustain_samples":1}`),
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
	collector.reply(t, RawSample{CPUTotal: 100, CPUIdle: 50, CollectedAt: now})

	// Establish a published critical state before exercising its resolution.
	ticker.ticks <- now.Add(time.Second)
	collector.reply(t, RawSample{CPUTotal: 200, CPUIdle: 50, CollectedAt: now.Add(time.Second)})
	critical := host.nextAttempt(t)
	if critical.Channel != ChannelPressure || critical.Disposition != protocol.DispositionActionable {
		t.Fatalf("critical observation = %#v", critical)
	}

	// The first resolution delivery is uncertain.
	ticker.ticks <- now.Add(2 * time.Second)
	collector.reply(t, RawSample{CPUTotal: 300, CPUIdle: 150, CollectedAt: now.Add(2 * time.Second)})
	failedResolution := host.nextAttempt(t)
	if failedResolution.Channel != ChannelPressure || failedResolution.Disposition != protocol.DispositionResolved || failedResolution.ReasonCode != "pressure_resolved" {
		t.Fatalf("failed resolution = %#v", failedResolution)
	}

	// Each complete cycle must retry the exact same resolution and keep overall
	// plugin health degraded until delivery recovers.
	for retry := 1; retry <= 2; retry++ {
		ticker.ticks <- now.Add(time.Duration(2+retry) * time.Second)
		collector.reply(t, RawSample{
			CPUTotal: uint64(300 + retry*100), CPUIdle: uint64(250 + retry*100),
			CollectedAt: now.Add(time.Duration(2+retry) * time.Second),
		})
		got := host.nextAttempt(t)
		if !reflect.DeepEqual(got, failedResolution) {
			t.Fatalf("resolution retry %d = %#v, want exact %#v", retry, got, failedResolution)
		}
	}

	ticker.ticks <- now.Add(5 * time.Second)
	recoveryReply := awaitCollectorRequest(t, collector)
	if got := handler.Health(context.Background()); got.Healthy {
		t.Fatalf("plugin health recovered before pending delivery succeeded: %#v", got)
	}
	recoveryReply <- collectorResult{sample: RawSample{CPUTotal: 600, CPUIdle: 550, CollectedAt: now.Add(5 * time.Second)}}
	succeededResolution := host.nextAttempt(t)
	if !reflect.DeepEqual(succeededResolution, failedResolution) {
		t.Fatalf("successful resolution retry = %#v, want exact %#v", succeededResolution, failedResolution)
	}

	ticker.ticks <- now.Add(6 * time.Second)
	_ = awaitCollectorRequest(t, collector)
	if got := handler.Health(context.Background()); !got.Healthy {
		t.Fatalf("health did not recover after pending resolution succeeded: %#v", got)
	}
}

func TestHandlerRefreshesExpiredPendingActivePressureBeforeHealthRecovery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		cpuIdle     uint64
		disposition protocol.Disposition
		status      string
	}{
		{name: "warning", cpuIdle: 70, disposition: protocol.DispositionNotable, status: "MAC WARN"},
		{name: "critical", cpuIdle: 55, disposition: protocol.DispositionActionable, status: "MAC CRIT"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC)
			collector := newGatedCollector()
			ticker := &manualTicker{ticks: make(chan time.Time, 8)}
			host := &pressureRetryHost{attempts: make(chan protocol.Observation, 20)}
			host.failDisposition(test.disposition, 4)
			handler := newHandler(host, collector, func(time.Duration) Ticker { return ticker })
			if err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{
				ID: AppID, Generation: 1, Config: json.RawMessage(`{"sustain_samples":1}`),
			}}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
			collector.reply(t, RawSample{CPUTotal: 100, CPUIdle: 50, CollectedAt: now})

			// Commit the active pressure state, but leave its first publication pending.
			firstAt := now.Add(time.Second)
			ticker.ticks <- firstAt
			collector.reply(t, RawSample{CPUTotal: 200, CPUIdle: test.cpuIdle, CollectedAt: firstAt})
			original := host.nextAttempt(t)
			if original.Disposition != test.disposition || original.Revision != 1 || !original.ObservedAt.Equal(firstAt) || !original.ValidUntil.Equal(firstAt.Add(10*time.Second)) {
				t.Fatalf("original pressure = %#v", original)
			}
			assertText(t, original.Scene, "back-status", test.status)
			assertText(t, original.Scene, "back-reason", "CPU")

			// Two still-valid retries fail exactly and degrade overall plugin health.
			lastTotal, lastIdle := uint64(200), test.cpuIdle
			for retry := 1; retry <= 2; retry++ {
				at := now.Add(time.Duration(1+retry) * time.Second)
				lastTotal += 100
				lastIdle += 100
				ticker.ticks <- at
				collector.reply(t, RawSample{CPUTotal: lastTotal, CPUIdle: lastIdle, CollectedAt: at})
				if got := host.nextAttempt(t); !reflect.DeepEqual(got, original) {
					t.Fatalf("valid retry %d = %#v, want exact %#v", retry, got, original)
				}
			}

			// Once its fixed display lifetime passes, the old active pressure cannot
			// be retried. It must be refreshed from the current scene and retained.
			refreshAt := now.Add(12 * time.Second)
			lastTotal += 100
			lastIdle += 100
			ticker.ticks <- refreshAt
			collector.reply(t, RawSample{CPUTotal: lastTotal, CPUIdle: lastIdle, MemoryPercent: 33, CollectedAt: refreshAt})
			refreshed := host.nextAttempt(t)
			if refreshed.Disposition != test.disposition || refreshed.Revision != 2 {
				t.Fatalf("refreshed pressure identity/state = %#v", refreshed)
			}
			if !refreshed.ObservedAt.Equal(refreshAt) || !refreshed.UpdatedAt.Equal(refreshAt) || !refreshed.ValidUntil.Equal(refreshAt.Add(10*time.Second)) {
				t.Fatalf("refreshed pressure times = observed %v updated %v valid %v", refreshed.ObservedAt, refreshed.UpdatedAt, refreshed.ValidUntil)
			}
			if reflect.DeepEqual(refreshed.Scene, original.Scene) {
				t.Fatal("expired pressure retained its stale scene")
			}
			assertText(t, refreshed.Scene, "back-status", test.status)
			assertText(t, refreshed.Scene, "back-reason", "CPU")
			assertText(t, refreshed.Scene, "back-cpu-value", "0%")
			assertText(t, refreshed.Scene, "back-mem-value", "33%")

			retryAt := now.Add(13 * time.Second)
			ticker.ticks <- retryAt
			recoveryReply := awaitCollectorRequest(t, collector)
			if got := handler.Health(context.Background()); got.Healthy {
				t.Fatalf("plugin health recovered before refreshed delivery succeeded: %#v", got)
			}
			recoveryReply <- collectorResult{sample: RawSample{CPUTotal: lastTotal + 100, CPUIdle: lastIdle + 100, MemoryPercent: 44, CollectedAt: retryAt}}
			if got := host.nextAttempt(t); !reflect.DeepEqual(got, refreshed) {
				t.Fatalf("refreshed retry = %#v, want exact %#v", got, refreshed)
			}

			ticker.ticks <- now.Add(14 * time.Second)
			_ = awaitCollectorRequest(t, collector)
			if got := handler.Health(context.Background()); !got.Healthy {
				t.Fatalf("health did not recover after refreshed pressure succeeded: %#v", got)
			}
		})
	}
}

func TestWorkerRetriesExactFailedPressureObservationForEveryDisposition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		disposition protocol.Disposition
		prime       []reading
		failed      reading
	}{
		{name: "warning", disposition: protocol.DispositionNotable, failed: reading{CPUPercent: 80}},
		{name: "critical", disposition: protocol.DispositionActionable, failed: reading{CPUPercent: 95}},
		{name: "resolved", disposition: protocol.DispositionResolved, prime: []reading{{CPUPercent: 95}}, failed: reading{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
			host := &pressureRetryHost{attempts: make(chan protocol.Observation, 8)}
			config := Config{SampleInterval: 2 * time.Second, SummaryInterval: 3 * time.Minute, WarningPercent: 70, CriticalPercent: 90, SustainSamples: 1, RecoveryMarginPercent: 5, NetworkCapacityBytesPerSecond: 1024}
			worker := &worker{instanceID: AppID, generation: 1, config: config, host: host, pressure: newPressureMachine(config)}
			for index, value := range test.prime {
				if err := worker.publish(context.Background(), now.Add(time.Duration(index)*time.Second), value); err != nil {
					t.Fatal(err)
				}
				_ = host.nextAttempt(t) // pressure
			}
			host.failDisposition(test.disposition, 1)
			if err := worker.publish(context.Background(), now.Add(2*time.Second), test.failed); err == nil {
				t.Fatal("pressure publication unexpectedly succeeded")
			}
			failed := host.nextAttempt(t)
			if failed.Disposition != test.disposition {
				t.Fatalf("failed disposition = %q, want %q", failed.Disposition, test.disposition)
			}
			if err := worker.publish(context.Background(), now.Add(3*time.Second), reading{}); err != nil {
				t.Fatalf("retry: %v", err)
			}
			retried := host.nextAttempt(t)
			if !reflect.DeepEqual(retried, failed) {
				t.Fatalf("retry = %#v, want exact %#v", retried, failed)
			}
		})
	}
}

func TestHandlerRejectsUnavailableCollectorBeforeLaunchingWorker(t *testing.T) {
	tickerCreated := false
	handler := newHandler(&recordingHost{observations: make(chan protocol.Observation, 1)}, unavailableCollector{}, func(time.Duration) Ticker {
		tickerCreated = true
		return &manualTicker{ticks: make(chan time.Time)}
	})
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
	err := handler.ReplaceInstances(context.Background(), []protocol.Instance{{ID: AppID, Generation: 1, Config: json.RawMessage(`{}`)}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ReplaceInstances error = %v, want ErrUnsupported", err)
	}
	if !pluginsdk.IsPermanentConfiguration(err) {
		t.Fatalf("ReplaceInstances error = %v, want permanent configuration classification", err)
	}
	if tickerCreated || handler.worker != nil {
		t.Fatalf("unavailable collector launched worker: ticker=%v worker=%#v", tickerCreated, handler.worker)
	}
}

type collectorResult struct {
	sample RawSample
	err    error
}

func awaitCollectorRequest(t *testing.T, collector *gatedCollector) chan collectorResult {
	t.Helper()
	select {
	case reply := <-collector.requests:
		return reply
	case <-time.After(time.Second):
		t.Fatal("collector was not sampled")
		return nil
	}
}

type unavailableCollector struct{}

func (unavailableCollector) Availability() error { return ErrUnsupported }
func (unavailableCollector) Sample(ctx context.Context) (RawSample, error) {
	<-ctx.Done()
	return RawSample{}, ctx.Err()
}

type publicationHealthHost struct {
	mu   sync.Mutex
	fail bool
	logs []string
}

type pressureRetryHost struct {
	mu                 sync.Mutex
	attempts           chan protocol.Observation
	fail               protocol.Disposition
	failures           int
	resolutionFailures int
}

func (h *pressureRetryHost) PublishObservation(_ context.Context, value protocol.Observation) error {
	h.attempts <- value
	h.mu.Lock()
	defer h.mu.Unlock()
	if value.Channel != ChannelPressure {
		return nil
	}
	if value.Disposition == protocol.DispositionResolved && h.resolutionFailures > 0 {
		h.resolutionFailures--
		return errors.New("resolution delivery uncertain")
	}
	if value.Disposition == h.fail && h.failures > 0 {
		h.failures--
		return errors.New("pressure delivery uncertain")
	}
	return nil
}

func (*pressureRetryHost) CompleteSession(context.Context, protocol.CompleteSessionRequest) error {
	return nil
}

func (h *pressureRetryHost) Log(context.Context, protocol.LogNotification) error { return nil }

func (h *pressureRetryHost) failDisposition(disposition protocol.Disposition, failures int) {
	h.mu.Lock()
	h.fail = disposition
	h.failures = failures
	h.mu.Unlock()
}

func (h *pressureRetryHost) nextAttempt(t *testing.T) protocol.Observation {
	t.Helper()
	select {
	case value := <-h.attempts:
		return value
	case <-time.After(time.Second):
		t.Fatal("host did not receive observation")
		return protocol.Observation{}
	}
}

func (h *publicationHealthHost) PublishObservation(context.Context, protocol.Observation) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail {
		return errors.New("publish failed")
	}
	return nil
}

func (*publicationHealthHost) CompleteSession(context.Context, protocol.CompleteSessionRequest) error {
	return nil
}

func (h *publicationHealthHost) Log(_ context.Context, value protocol.LogNotification) error {
	h.mu.Lock()
	h.logs = append(h.logs, value.Event)
	h.mu.Unlock()
	return nil
}

func (h *publicationHealthHost) setFail(fail bool) {
	h.mu.Lock()
	h.fail = fail
	h.mu.Unlock()
}

func (h *publicationHealthHost) logEvents() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.logs...)
}

type gatedCollector struct {
	requests chan chan collectorResult
}

func newGatedCollector() *gatedCollector {
	return &gatedCollector{requests: make(chan chan collectorResult)}
}

func (c *gatedCollector) Sample(ctx context.Context) (RawSample, error) {
	reply := make(chan collectorResult, 1)
	select {
	case c.requests <- reply:
	case <-ctx.Done():
		return RawSample{}, ctx.Err()
	}
	select {
	case result := <-reply:
		return result.sample, result.err
	case <-ctx.Done():
		return RawSample{}, ctx.Err()
	}
}

func (c *gatedCollector) reply(t *testing.T, sample RawSample) {
	t.Helper()
	select {
	case reply := <-c.requests:
		reply <- collectorResult{sample: sample}
	case <-time.After(time.Second):
		t.Fatal("collector was not sampled")
	}
}

func (c *gatedCollector) replyError(t *testing.T, err error) {
	t.Helper()
	select {
	case reply := <-c.requests:
		reply <- collectorResult{err: err}
	case <-time.After(time.Second):
		t.Fatal("collector was not sampled")
	}
}

type manualTicker struct {
	ticks chan time.Time
	mu    sync.Mutex
	stop  bool
}

func (t *manualTicker) C() <-chan time.Time { return t.ticks }
func (t *manualTicker) Stop() {
	t.mu.Lock()
	t.stop = true
	t.mu.Unlock()
}
func (t *manualTicker) stopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stop
}

type recordingHost struct {
	observations chan protocol.Observation
	mu           sync.Mutex
	logs         []string
	completions  []protocol.CompleteSessionRequest
}

func (h *recordingHost) PublishObservation(_ context.Context, value protocol.Observation) error {
	h.observations <- value
	return nil
}

func (*recordingHost) Withdraw(context.Context, protocol.WithdrawRequest) error { return nil }

func (h *recordingHost) Log(_ context.Context, value protocol.LogNotification) error {
	h.mu.Lock()
	h.logs = append(h.logs, value.Event)
	h.mu.Unlock()
	return nil
}

func (h *recordingHost) CompleteSession(_ context.Context, value protocol.CompleteSessionRequest) error {
	h.mu.Lock()
	h.completions = append(h.completions, value)
	h.mu.Unlock()
	return nil
}

func (h *recordingHost) nextObservation(t *testing.T) protocol.Observation {
	t.Helper()
	select {
	case value := <-h.observations:
		return value
	case <-time.After(time.Second):
		t.Fatal("observation was not published")
		return protocol.Observation{}
	}
}

func nextMacObservation(t *testing.T, host *recordingHost, channel string) protocol.Observation {
	t.Helper()
	for {
		value := host.nextObservation(t)
		if value.Channel == channel {
			return value
		}
	}
}

func macSceneText(value protocol.Observation, id string) string {
	if value.Scene == nil {
		return ""
	}
	for _, element := range value.Scene.Elements {
		if element.ID == id && element.Text != nil {
			return element.Text.Value
		}
	}
	return ""
}

func (h *recordingHost) assertNoObservation(t *testing.T) {
	t.Helper()
	select {
	case value := <-h.observations:
		t.Fatalf("unexpected observation %#v", value)
	default:
	}
}

func (h *recordingHost) logEvents() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.logs...)
}

type cancelCollector struct {
	started chan struct{}
	done    chan struct{}
	once    sync.Once
}

func (c *cancelCollector) Sample(ctx context.Context) (RawSample, error) {
	c.once.Do(func() { close(c.started) })
	<-ctx.Done()
	close(c.done)
	return RawSample{}, ctx.Err()
}

type stubbornCollector struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *stubbornCollector) Sample(context.Context) (RawSample, error) {
	c.once.Do(func() { close(c.started) })
	<-c.release
	return RawSample{CollectedAt: time.Now()}, nil
}
