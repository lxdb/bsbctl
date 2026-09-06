package device

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	busylib "github.com/lxdb/busylib-go"
)

func newTestGateway(display Display) *Gateway {
	return newGateway(display, nil)
}

func setTestAssetResolver(gateway *Gateway, resolver AssetResolver) {
	gateway.mu.Lock()
	gateway.assets = resolver
	gateway.mu.Unlock()
}

func TestGatewayConstructorRejectsMissingRequiredDependencies(t *testing.T) {
	resolver := assetResolverFunc(func(_ string, scene presentation.Scene) (presentation.ResolvedScene, error) {
		return presentation.ResolveScene(scene), nil
	})
	if _, err := NewGateway(nil, resolver); err == nil {
		t.Fatal("NewGateway accepted a nil display")
	}
	if _, err := NewGateway(&fakeDisplay{}, nil); err == nil {
		t.Fatal("NewGateway accepted a nil asset resolver")
	}
}

func TestGatewaySuppressesDuplicatesAndClearsBeforePriorityDrop(t *testing.T) {
	t.Parallel()
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	high := candidate(100, "urgent")
	if _, err := gateway.Render(context.Background(), &high); err != nil {
		t.Fatalf("Render high: %v", err)
	}
	if _, err := gateway.Render(context.Background(), &high); err != nil {
		t.Fatalf("Render duplicate: %v", err)
	}
	low := candidate(20, "routine")
	if _, err := gateway.Render(context.Background(), &low); err != nil {
		t.Fatalf("Render low: %v", err)
	}

	if got, want := display.operations, []string{"draw:100:urgent", "clear:bsbctl", "draw:20:routine"}; !equalOperations(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
	if display.draws[0].ApplicationName != ApplicationName || display.draws[1].ApplicationName != ApplicationName {
		t.Fatalf("application names = %q/%q", display.draws[0].ApplicationName, display.draws[1].ApplicationName)
	}
}

func TestGatewayUpdatesChangedSceneInPlaceWhenTopologyMatches(t *testing.T) {
	t.Parallel()
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	first := candidate(20, "first")
	second := candidate(20, "second")
	second.Revision = 2
	if _, err := gateway.Render(context.Background(), &first); err != nil {
		t.Fatalf("Render first: %v", err)
	}
	if _, err := gateway.Render(context.Background(), &second); err != nil {
		t.Fatalf("Render second: %v", err)
	}
	if got, want := display.operations, []string{"draw:20:first", "draw:20:second"}; !equalOperations(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
}

func TestGatewayUpdatesZeroAndNonzeroBarsWithoutClearing(t *testing.T) {
	t.Parallel()
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	zero := candidate(20, "unused")
	zero.Scene.Elements = []presentation.Element{{ID: "bar", Display: protocol.DisplayFront, X: 1, Y: 1, Rectangle: &protocol.RectangleElement{Width: 1, Height: 3, Color: "#111A20FF"}}}
	full := zero
	full.Revision = 2
	full.Scene.Elements = []presentation.Element{{ID: "bar", Display: protocol.DisplayFront, X: 1, Y: 1, Rectangle: &protocol.RectangleElement{Width: 53, Height: 3, Color: "#35D07FFF"}}}
	if _, err := gateway.Render(context.Background(), &zero); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Render(context.Background(), &full); err != nil {
		t.Fatal(err)
	}
	if display.clearCount != 0 || len(display.draws) != 2 {
		t.Fatalf("zero/full operations = %v", display.operations)
	}
}

func TestGatewayClearsBeforeDrawingChangedTopologyAtSamePriority(t *testing.T) {
	t.Parallel()
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	first := candidate(20, "first")
	second := candidate(20, "second")
	second.Revision = 2
	second.Scene.Elements[0].ID = "replacement"
	if _, err := gateway.Render(context.Background(), &first); err != nil {
		t.Fatalf("Render first: %v", err)
	}
	if _, err := gateway.Render(context.Background(), &second); err != nil {
		t.Fatalf("Render second: %v", err)
	}
	if got, want := display.operations, []string{"draw:20:first", "clear:bsbctl", "draw:20:second"}; !equalOperations(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
}

func TestGatewayClearAndConflictAreIdempotent(t *testing.T) {
	t.Parallel()
	display := &fakeDisplay{drawErr: &busylib.APIError{StatusCode: http.StatusConflict}}
	gateway := newTestGateway(display)
	value := candidate(60, "build")
	outcome, err := gateway.Render(context.Background(), &value)
	if err != nil {
		t.Fatalf("conflicting Render = %v, want suppressed", err)
	}
	if outcome != attention.OutcomeFirmwareSuppressed {
		t.Fatalf("conflicting Render outcome = %q, want firmware suppression", outcome)
	}
	display.drawErr = nil
	outcome, err = gateway.Render(context.Background(), &value)
	if err != nil {
		t.Fatalf("retry Render: %v", err)
	}
	if outcome != attention.OutcomeDrawn {
		t.Fatalf("successful Render outcome = %q, want drawn", outcome)
	}
	outcome, err = gateway.Render(context.Background(), nil)
	if err != nil {
		t.Fatalf("clear Render: %v", err)
	}
	if outcome != attention.OutcomeCleared {
		t.Fatalf("clear Render outcome = %q, want cleared", outcome)
	}
	outcome, err = gateway.Render(context.Background(), nil)
	if err != nil {
		t.Fatalf("duplicate clear Render: %v", err)
	}
	if outcome != attention.OutcomeUnchanged {
		t.Fatalf("duplicate clear outcome = %q, want unchanged", outcome)
	}
	if got := display.clearCount; got != 1 {
		t.Fatalf("clear count = %d, want 1", got)
	}
}

func TestGatewayConflictThroughOutputDoesNotReportDeviceFailure(t *testing.T) {
	backend := &recordingOutputBackend{drawErr: &busylib.APIError{StatusCode: http.StatusConflict}}
	output := NewOutput(backend, OutputOptions{CallTimeout: time.Second})
	defer func() { _ = output.Close(context.Background()) }()
	gateway := newTestGateway(output)
	value := candidate(60, "native-owned")

	outcome, err := gateway.Render(t.Context(), &value)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != attention.OutcomeFirmwareSuppressed {
		t.Fatalf("outcome = %q, want firmware suppression", outcome)
	}
	if got := output.Status().LastErrorCode; got != "" {
		t.Fatalf("output status error = %q, want no device failure for native arbitration", got)
	}
}

func TestGatewayClearsAfterCanceledDrawMayReachDevice(t *testing.T) {
	backend := &cancelAfterDrawBackend{started: make(chan struct{}), release: make(chan struct{}), completed: make(chan struct{})}
	output := NewOutput(backend, OutputOptions{CallTimeout: time.Second})
	defer func() { _ = output.Close(context.Background()) }()
	gateway := newTestGateway(output)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := gateway.Render(ctx, new(candidate(60, "ambiguous")))
		done <- err
	}()
	<-backend.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Render = %v, want context cancellation", err)
	}
	close(backend.release)
	<-backend.completed
	if outcome, err := gateway.Render(t.Context(), nil); err != nil || outcome != attention.OutcomeCleared {
		t.Fatalf("clear after canceled draw = %q, %v", outcome, err)
	}
	if got, want := backend.operations, []string{"draw", "clear"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
}

func TestGatewayConflictInvalidatesClaimedPhysicalSceneWithoutClearing(t *testing.T) {
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	first := candidate(60, "first")
	if _, err := gateway.Render(t.Context(), &first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Revision++
	second.Scene.Elements[0].Text.Value = "second"
	display.drawErr = &busylib.APIError{StatusCode: http.StatusConflict}
	outcome, err := gateway.Render(t.Context(), &second)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != attention.OutcomeFirmwareSuppressed {
		t.Fatalf("conflict outcome = %q, want firmware suppression", outcome)
	}
	if gateway.hasScene {
		t.Fatal("firmware-suppressed scene remained cached as physically current")
	}
	if _, err := gateway.Render(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if display.clearCount != 0 {
		t.Fatalf("suppression triggered a follow-up clear: %d", display.clearCount)
	}
}

func TestGatewayCanvasInvalidationForcesIdenticalSceneRedraw(t *testing.T) {
	t.Parallel()
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	value := candidate(100, "launcher")

	if _, err := gateway.Render(t.Context(), &value); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Render(t.Context(), &value); err != nil {
		t.Fatal(err)
	}
	if len(display.draws) != 1 {
		t.Fatalf("draws before invalidation = %d, want 1", len(display.draws))
	}

	gateway.InvalidateCanvas()
	if _, err := gateway.Render(t.Context(), &value); err != nil {
		t.Fatal(err)
	}
	if len(display.draws) != 2 {
		t.Fatalf("draws after invalidation = %d, want 2", len(display.draws))
	}
}

func TestGatewayReconnectRetainsSurvivingSceneOwnership(t *testing.T) {
	unchanged := candidate(100, "surviving")
	lowerPriority := candidate(20, "routine")
	newTopology := candidate(100, "replacement")
	newTopology.Scene.Elements[0].ID = "replacement"
	for _, test := range []struct {
		name string
		next *presentation.Candidate
		want []string
	}{
		{name: "withdrawal", want: []string{"draw:100:surviving", "clear:bsbctl"}},
		{name: "identical redraw", next: &unchanged, want: []string{"draw:100:surviving", "draw:100:surviving"}},
		{name: "priority drop", next: &lowerPriority, want: []string{"draw:100:surviving", "clear:bsbctl", "draw:20:routine"}},
		{name: "topology replacement", next: &newTopology, want: []string{"draw:100:surviving", "clear:bsbctl", "draw:100:replacement"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			display := &fakeDisplay{}
			gateway := newTestGateway(display)
			first := candidate(100, "surviving")
			if _, err := gateway.Render(t.Context(), &first); err != nil {
				t.Fatal(err)
			}
			gateway.InvalidateConnection()
			outcome, err := gateway.Render(t.Context(), test.next)
			wantOutcome := attention.OutcomeDrawn
			if test.next == nil {
				wantOutcome = attention.OutcomeCleared
			}
			if err != nil || outcome != wantOutcome || !equalOperations(display.operations, test.want) {
				t.Fatalf("render after reconnect = %v, %v; operations = %v, want %v", outcome, err, display.operations, test.want)
			}
		})
	}
}

func TestGatewayRejectsUnknownElementBeforeDeviceIO(t *testing.T) {
	t.Parallel()
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	value := candidate(20, "bad")
	value.Scene.Elements[0].Text = nil
	outcome, err := gateway.Render(context.Background(), &value)
	if !errors.Is(err, presentation.ErrInvalidPresentation) {
		t.Fatalf("Render error = %v, want ErrInvalidPresentation", err)
	}
	if outcome != attention.OutcomeInvalidPresentation {
		t.Fatalf("invalid Render outcome = %q, want invalid presentation", outcome)
	}
	if len(display.operations) != 0 {
		t.Fatalf("device operations = %v, want none", display.operations)
	}
}

func TestGatewayTranslatesNativeCountdownElement(t *testing.T) {
	t.Parallel()
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	value := candidate(100, "meeting")
	value.Scene.Elements = []presentation.Element{{
		ID: "countdown", Display: protocol.DisplayFront, X: 70,
		Countdown: &protocol.CountdownElement{EndsAtUnixSeconds: 1787504700, Color: "#F2B84BFF", ShowHours: protocol.CountdownShowHoursWhenNonZero},
	}}
	if _, err := gateway.Render(context.Background(), &value); err != nil {
		t.Fatal(err)
	}
	if len(display.draws) != 1 || len(display.draws[0].Elements) != 1 {
		t.Fatalf("draws = %#v", display.draws)
	}
	countdown, ok := display.draws[0].Elements[0].(busylib.CountdownElement)
	if !ok {
		t.Fatalf("element type = %T", display.draws[0].Elements[0])
	}
	if countdown.Timestamp != "1787504700" || countdown.Direction != busylib.CountdownTimeLeft ||
		countdown.ShowHours != busylib.CountdownShowHoursWhenNonZero || countdown.Color != "#F2B84BFF" {
		t.Fatalf("countdown = %#v", countdown)
	}
}

func TestGatewayTranslatesNativeMarqueeFields(t *testing.T) {
	t.Parallel()
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	value := candidate(100, "Team synchronization with a long title")
	value.Scene.Elements[0] = presentation.Element{
		ID: "title", Display: protocol.DisplayFront, X: 18,
		Text: &protocol.TextElement{Value: "Team synchronization with a long title", Font: "normal", Width: 54,
			Marquee: &protocol.Marquee{PixelsPerMinute: 1000, StartDelayMilliseconds: 1000, RepeatDelayMilliseconds: 2500}},
	}
	if _, err := gateway.Render(context.Background(), &value); err != nil {
		t.Fatal(err)
	}
	text := display.draws[0].Elements[0].(busylib.TextElement)
	if text.Width != 54 || text.ScrollRate != 1000 || text.ScrollStartDelay != 1000 || text.ScrollRepeatDelay != 2500 {
		t.Fatalf("marquee text = %#v", text)
	}
}

func TestGatewayPlaysStableUnexpiredAudioCueOnlyOnceAcrossVisualRevisions(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	setTestAssetResolver(gateway, builtinAudioResolver{})
	gateway.now = func() time.Time { return now }
	value := candidate(100, "Team sync")
	value.AudioCue = &protocol.AudioCue{
		ID: "calendar-reminder:event-opaque", Asset: protocol.AssetRef{StockName: "calendar_reminder_ends.snd"},
		ExpiresAt: now.Add(15 * time.Second),
	}
	if _, err := gateway.Render(context.Background(), &value); err != nil {
		t.Fatal(err)
	}
	value.Revision++
	value.Scene.Elements[0].Text.Value = "Team sync updated"
	if _, err := gateway.Render(context.Background(), &value); err != nil {
		t.Fatal(err)
	}
	if got, want := display.audio, []busylib.PlayAudio{{ApplicationName: ApplicationName, StockPath: "shared/sounds/calendar_reminder_ends.snd"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("audio requests = %#v, want %#v", got, want)
	}
}

func TestGatewayResolvesAudioBeforeVisualIO(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	setTestAssetResolver(gateway, failingAudioResolver{err: errors.New("missing cue")})
	gateway.now = func() time.Time { return now }
	value := candidate(100, "Team sync")
	value.AudioCue = &protocol.AudioCue{
		ID: "calendar-reminder:event-opaque", Asset: protocol.AssetRef{StockName: "calendar_reminder_ends.snd"},
		ExpiresAt: now.Add(15 * time.Second),
	}
	outcome, err := gateway.Render(t.Context(), &value)
	if err == nil {
		t.Fatal("Render succeeded with an unresolved audio cue")
	}
	if outcome != attention.OutcomeAssetMissing {
		t.Fatalf("unresolved audio outcome = %q, want asset missing", outcome)
	}
	if len(display.operations) != 0 {
		t.Fatalf("device operations = %#v, want none", display.operations)
	}
}

func TestGatewayConfirmsVisualOutcomeWhenAudioDeviceFails(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	display := &fakeDisplay{audioErr: errors.New("speaker unavailable")}
	gateway := newTestGateway(display)
	setTestAssetResolver(gateway, builtinAudioResolver{})
	gateway.now = func() time.Time { return now }
	value := candidate(100, "Team sync")
	value.AudioCue = &protocol.AudioCue{
		ID: "calendar-reminder:event-opaque", Asset: protocol.AssetRef{StockName: "calendar_reminder_ends.snd"},
		ExpiresAt: now.Add(15 * time.Second),
	}
	outcome, err := gateway.Render(t.Context(), &value)
	if err != nil {
		t.Fatalf("Render returned audio failure after a successful draw: %v", err)
	}
	if outcome != attention.OutcomeDrawn {
		t.Fatalf("visual outcome = %q, want %q", outcome, attention.OutcomeDrawn)
	}
	if got := gateway.Status(); got.LastErrorCode != "audio_play_failed" || got.Attempts != 1 {
		t.Fatalf("audio status = %#v, want one failed attempt", got)
	}
	value.Revision++
	value.Scene.Elements[0].Text.Value = "Team sync updated"
	if _, err := gateway.Render(t.Context(), &value); err != nil {
		t.Fatalf("second Render returned audio failure: %v", err)
	}
	if got := len(display.audio); got != 1 {
		t.Fatalf("audio attempts = %d, want one attempt for the cue identity", got)
	}
	if gateway.Status().LastErrorCode != "audio_play_failed" {
		t.Fatalf("deduplicated cue erased the most recent audio attempt diagnostic")
	}
}

func TestGatewayScopesAudioCueDeduplicationToGeneration(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	setTestAssetResolver(gateway, builtinAudioResolver{})
	gateway.now = func() time.Time { return now }
	first := candidate(100, "First")
	first.AudioCue = &protocol.AudioCue{ID: "shared-cue", Asset: protocol.AssetRef{StockName: "calendar_event_starts.snd"}, ExpiresAt: now.Add(time.Minute)}
	if _, err := gateway.Render(t.Context(), &first); err != nil {
		t.Fatal(err)
	}
	second := candidate(100, "Second")
	second.Generation++
	second.AudioCue = &protocol.AudioCue{ID: "shared-cue", Asset: protocol.AssetRef{StockName: "calendar_event_starts.snd"}, ExpiresAt: now.Add(time.Minute)}
	if _, err := gateway.Render(t.Context(), &second); err != nil {
		t.Fatal(err)
	}
	if len(display.audio) != 2 {
		t.Fatalf("audio requests = %d, want one per generation", len(display.audio))
	}
}

func TestGatewayScopesAudioCueDeduplicationToPluginInstance(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		mutate func(*presentation.Candidate)
	}{
		{name: "plugin", mutate: func(value *presentation.Candidate) { value.PluginID = "other-plugin" }},
		{name: "instance", mutate: func(value *presentation.Candidate) { value.InstanceID = "other-instance" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			display := &fakeDisplay{}
			gateway := newTestGateway(display)
			setTestAssetResolver(gateway, builtinAudioResolver{})
			gateway.now = func() time.Time { return now }
			first := candidate(100, "First")
			first.AudioCue = &protocol.AudioCue{ID: "shared-cue", Asset: protocol.AssetRef{StockName: "calendar_event_starts.snd"}, ExpiresAt: now.Add(time.Minute)}
			if _, err := gateway.Render(t.Context(), &first); err != nil {
				t.Fatal(err)
			}
			second := candidate(100, "Second")
			second.AudioCue = &protocol.AudioCue{ID: "shared-cue", Asset: protocol.AssetRef{StockName: "calendar_event_starts.snd"}, ExpiresAt: now.Add(time.Minute)}
			test.mutate(&second)
			if _, err := gateway.Render(t.Context(), &second); err != nil {
				t.Fatal(err)
			}
			if len(display.audio) != 2 {
				t.Fatalf("audio requests = %d, want independent playback", len(display.audio))
			}
		})
	}
}

func TestGatewayDoesNotPlayExpiredAudioCue(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 20, 0, time.UTC)
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	setTestAssetResolver(gateway, builtinAudioResolver{})
	gateway.now = func() time.Time { return now }
	value := candidate(100, "Team sync")
	value.AudioCue = &protocol.AudioCue{
		ID: "calendar-reminder:event-opaque", Asset: protocol.AssetRef{StockName: "calendar_reminder_ends.snd"},
		ExpiresAt: now.Add(-5 * time.Second),
	}
	if _, err := gateway.Render(context.Background(), &value); err != nil {
		t.Fatal(err)
	}
	if len(display.audio) != 0 {
		t.Fatalf("expired audio requests = %#v", display.audio)
	}
}

func TestGatewayBoundsConsumedAudioCueHistory(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	setTestAssetResolver(gateway, builtinAudioResolver{})
	gateway.now = func() time.Time { return now }
	for index := range maxPlayedAudioCues + 1 {
		value := candidate(100, "Team sync")
		value.Key = "event-" + strconv.Itoa(index)
		value.AudioCue = &protocol.AudioCue{
			ID: "calendar-active:event-" + strconv.Itoa(index), Asset: protocol.AssetRef{StockName: "calendar_event_starts.snd"},
			ExpiresAt: now.Add(time.Hour + time.Duration(index)*time.Second),
		}
		if _, err := gateway.Render(context.Background(), &value); err != nil {
			t.Fatal(err)
		}
	}
	if len(gateway.playedAudio) != maxPlayedAudioCues {
		t.Fatalf("played audio history = %d, want %d", len(gateway.playedAudio), maxPlayedAudioCues)
	}
}

func TestGatewaySolidRectangleHasNoImplicitFirmwareBorder(t *testing.T) {
	t.Parallel()
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	value := candidate(20, "unused")
	value.Scene.Elements = []presentation.Element{{
		ID: "progress", Display: protocol.DisplayFront, X: 35, Y: 13,
		Rectangle: &protocol.RectangleElement{Width: 10, Height: 2, Color: "#2AC7B5FF"},
	}}

	if _, err := gateway.Render(context.Background(), &value); err != nil {
		t.Fatal(err)
	}
	rectangle := display.draws[0].Elements[0].(busylib.RectangleElement)
	if rectangle.BorderWidth == nil || *rectangle.BorderWidth != 0 {
		t.Fatalf("border width = %v, want explicit zero", rectangle.BorderWidth)
	}
	if rectangle.Fill != busylib.RectangleFillSolid || !reflect.DeepEqual(rectangle.FillColors, []string{"#2AC7B5FF"}) {
		t.Fatalf("fill = %q %v, want solid palette color", rectangle.Fill, rectangle.FillColors)
	}
}

func TestGatewayStartsNativeBusyTimerWithMeetingThemeAndRemainingTime(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 0, 0, 0, time.UTC)
	display := &fakeDisplay{
		snapshot: busylib.BusySnapshot{
			Snapshot:            busylib.BusySnapshotData{Type: busylib.BusySnapshotNotStarted, BusyBarSettings: busylib.BusyBarSettings{Theme: "on_air", ShowWorkPhaseOnly: true, TriggerSmartHome: true}},
			SnapshotTimestampMS: now.Add(-time.Second).UnixMilli(),
		},
		deviceTime: busylib.TimestampInfo{Timestamp: now.Format(time.RFC3339)},
	}
	gateway := newTestGateway(display)
	gateway.now = func() time.Time { return now }
	value := timerCandidate(now.Add(30 * time.Minute))

	if _, err := gateway.Render(context.Background(), &value); err != nil {
		t.Fatal(err)
	}
	if len(display.setSnapshots) != 1 {
		t.Fatalf("snapshot writes = %d, want 1", len(display.setSnapshots))
	}
	written := display.setSnapshots[0]
	if written.Snapshot.Type != busylib.BusySnapshotSimple || written.Snapshot.CardID == "" || written.Snapshot.IsPaused == nil || *written.Snapshot.IsPaused {
		t.Fatalf("snapshot = %#v", written)
	}
	if written.Snapshot.TimeLeftMS == nil || *written.Snapshot.TimeLeftMS != int64(30*time.Minute/time.Millisecond) {
		t.Fatalf("time left = %v", written.Snapshot.TimeLeftMS)
	}
	settings := written.Snapshot.BusyBarSettings
	if settings.Theme != "meeting" || !settings.ShowWorkPhaseOnly || !settings.TriggerSmartHome {
		t.Fatalf("settings = %#v", settings)
	}
	if got, want := display.operations[:2], []string{"clear:bsbctl", "busy:snapshot"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("initial operations = %v, want %v", got, want)
	}
}

func TestGatewayDoesNotRestartIdenticalNativeBusyTimer(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 0, 0, 0, time.UTC)
	display := newTimerDisplay(now)
	gateway := newTestGateway(display)
	gateway.now = func() time.Time { return now }
	value := timerCandidate(now.Add(20 * time.Minute))
	if _, err := gateway.Render(context.Background(), &value); err != nil {
		t.Fatal(err)
	}
	value.Revision++
	value.UpdatedAt = now.Add(time.Minute)
	if _, err := gateway.Render(context.Background(), &value); err != nil {
		t.Fatal(err)
	}
	if len(display.setSnapshots) != 1 {
		t.Fatalf("same occurrence revision restarted timer: %d snapshot writes", len(display.setSnapshots))
	}
}

func TestGatewayRestoresSameNativeTimerAfterReconnectAndRetainsCheckedOwnership(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	display := newTimerDisplay(now)
	gateway := newTestGateway(display)
	gateway.now = func() time.Time { return now }
	value := timerCandidate(now.Add(20 * time.Minute))
	if _, err := gateway.Render(t.Context(), &value); err != nil {
		t.Fatal(err)
	}
	display.snapshot.Snapshot = busylib.BusySnapshotData{Type: busylib.BusySnapshotNotStarted}
	gateway.InvalidateConnection()
	if outcome, err := gateway.Render(t.Context(), &value); err != nil || outcome != attention.OutcomeDrawn {
		t.Fatalf("restore identical timer after firmware reset: %s, %v", outcome, err)
	}
	if len(display.setSnapshots) != 2 || display.snapshot.Snapshot.CardID == "" {
		t.Fatalf("physical timer was not restored: %#v", display.setSnapshots)
	}
	gateway.InvalidateConnection()
	if outcome, err := gateway.Render(t.Context(), nil); err != nil || outcome != attention.OutcomeCleared {
		t.Fatalf("clear surviving owned timer after reconnect: %s, %v", outcome, err)
	}
}

func TestGatewayClearsAppliedNativeTimerAfterReadbackFailure(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	display := &failedTimerReadback{fakeDisplay: newTimerDisplay(now)}
	gateway := newTestGateway(display)
	gateway.now = func() time.Time { return now }
	value := timerCandidate(now.Add(20 * time.Minute))
	if _, err := gateway.Render(t.Context(), &value); err == nil {
		t.Fatal("failed readback was reported as delivered")
	}
	display.failReadback = false
	if outcome, err := gateway.Render(t.Context(), nil); err != nil || outcome != attention.OutcomeCleared {
		t.Fatalf("clear unverified but applied timer: %s, %v", outcome, err)
	}
	if display.snapshot.Snapshot.Type != busylib.BusySnapshotNotStarted {
		t.Fatal("applied timer survived selection withdrawal")
	}
}

type failedTimerReadback struct {
	*fakeDisplay
	failReadback bool
}

func (d *failedTimerReadback) SetBusySnapshot(ctx context.Context, value busylib.BusySnapshot) error {
	err := d.fakeDisplay.SetBusySnapshot(ctx, value)
	d.failReadback = true
	return err
}

func (d *failedTimerReadback) BusySnapshot(ctx context.Context) (busylib.BusySnapshot, error) {
	if d.failReadback {
		return busylib.BusySnapshot{}, errors.New("readback unavailable")
	}
	return d.fakeDisplay.BusySnapshot(ctx)
}

func TestGatewayStopsOnlyTheNativeTimerItOwns(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 0, 0, 0, time.UTC)
	display := newTimerDisplay(now)
	gateway := newTestGateway(display)
	gateway.now = func() time.Time { return now }
	value := timerCandidate(now.Add(20 * time.Minute))
	if _, err := gateway.Render(context.Background(), &value); err != nil {
		t.Fatal(err)
	}
	ownedCard := display.snapshot.Snapshot.CardID
	outcome, err := gateway.Render(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != attention.OutcomeCleared {
		t.Fatalf("owned timer clear outcome = %q, want cleared", outcome)
	}
	if got := display.setSnapshots[len(display.setSnapshots)-1].Snapshot.Type; got != busylib.BusySnapshotNotStarted {
		t.Fatalf("owned timer stop type = %q", got)
	}

	display = newTimerDisplay(now)
	gateway = newTestGateway(display)
	gateway.now = func() time.Time { return now }
	if _, err := gateway.Render(context.Background(), &value); err != nil {
		t.Fatal(err)
	}
	display.snapshot.Snapshot.CardID = "00000000-0000-0000-0000-000000000999"
	outcome, err = gateway.Render(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != attention.OutcomeUnchanged {
		t.Fatalf("foreign timer clear outcome = %q, want unchanged", outcome)
	}
	if len(display.setSnapshots) != 1 {
		t.Fatalf("replacement timer was overwritten after owning %q: %d writes", ownedCard, len(display.setSnapshots))
	}
}

func TestGatewayStopsOwnedTimerBeforeRenderingAnotherScene(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 0, 0, 0, time.UTC)
	display := newTimerDisplay(now)
	gateway := newTestGateway(display)
	gateway.now = func() time.Time { return now }
	timer := timerCandidate(now.Add(20 * time.Minute))
	if _, err := gateway.Render(context.Background(), &timer); err != nil {
		t.Fatal(err)
	}
	scene := candidate(60, "available")
	if _, err := gateway.Render(context.Background(), &scene); err != nil {
		t.Fatal(err)
	}
	if len(display.setSnapshots) != 2 || display.setSnapshots[1].Snapshot.Type != busylib.BusySnapshotNotStarted || len(display.draws) != 1 {
		t.Fatalf("timer-to-scene transition: snapshots=%#v draws=%d", display.setSnapshots, len(display.draws))
	}
}

func TestGatewayRejectsFirmwareSuccessWithoutMatchingTimerReadback(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 0, 0, 0, time.UTC)
	display := newTimerDisplay(now)
	display.ignoreBusySet = true
	gateway := newTestGateway(display)
	gateway.now = func() time.Time { return now }
	gateway.verifyDelay = func(context.Context, time.Duration) error { return nil }
	timer := timerCandidate(now.Add(20 * time.Minute))
	if _, err := gateway.Render(context.Background(), &timer); err == nil {
		t.Fatal("firmware-ignored timer write was accepted")
	}
}

func TestGatewayResolvesLogicalAssetsBeforeDeviceIO(t *testing.T) {
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	setTestAssetResolver(gateway, assetResolverFunc(func(pluginID string, scene presentation.Scene) (presentation.ResolvedScene, error) {
		if pluginID != "p" {
			t.Fatalf("plugin id = %q", pluginID)
		}
		resolved := presentation.ResolveScene(scene)
		resolved.Elements[0].Path = "plugins/ball8/animations/shake.anim"
		return resolved, nil
	}))
	value := candidate(100, "unused")
	value.Scene.Elements = []presentation.Element{{ID: "animation", Display: protocol.DisplayFront, Animation: &protocol.AnimationElement{Asset: protocol.AssetRef{PackagePath: "animations/shake.anim"}, Loop: true}}}
	if _, err := gateway.Render(context.Background(), &value); err != nil {
		t.Fatal(err)
	}
	got := display.draws[0].Elements[0].(busylib.AnimationElement)
	if got.Path != "plugins/ball8/animations/shake.anim" {
		t.Fatalf("path = %q", got.Path)
	}
}

func TestGatewayDrawsNewGenerationWhenLogicalAssetMappingChanges(t *testing.T) {
	display := &fakeDisplay{}
	gateway := newTestGateway(display)
	resolvedPath := "p0123456789_ABCDEFGHIJKLMN.png"
	setTestAssetResolver(gateway, assetResolverFunc(func(_ string, scene presentation.Scene) (presentation.ResolvedScene, error) {
		resolved := presentation.ResolveScene(scene)
		resolved.Elements[0].Path = resolvedPath
		return resolved, nil
	}))
	value := candidate(60, "unused")
	value.Scene.Elements = []presentation.Element{{
		ID: "image", Display: protocol.DisplayFront,
		Image: &protocol.ImageElement{Asset: protocol.AssetRef{PackagePath: "assets/mark.png"}},
	}}
	if _, err := gateway.Render(t.Context(), &value); err != nil {
		t.Fatal(err)
	}
	firstPath := resolvedPath
	resolvedPath = "p0123456789_ZYXWVUTSRQPONM.png"
	value.Generation++
	outcome, err := gateway.Render(t.Context(), &value)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == attention.OutcomeUnchanged {
		t.Fatal("new generation with remapped asset bytes was suppressed as unchanged")
	}
	if len(display.draws) != 2 || display.clearCount != 0 {
		t.Fatalf("device operations = %v, want two in-place draws", display.operations)
	}
	first := display.draws[0].Elements[0].(busylib.ImageElement)
	second := display.draws[1].Elements[0].(busylib.ImageElement)
	if first.Path != firstPath || second.Path != resolvedPath {
		t.Fatalf("resolved paths = %q, %q; want %q, %q", first.Path, second.Path, firstPath, resolvedPath)
	}
}

type assetResolverFunc func(string, presentation.Scene) (presentation.ResolvedScene, error)

func (f assetResolverFunc) ResolveScene(pluginID string, scene presentation.Scene) (presentation.ResolvedScene, error) {
	return f(pluginID, scene)
}

type builtinAudioResolver struct{}

func (builtinAudioResolver) ResolveScene(_ string, scene presentation.Scene) (presentation.ResolvedScene, error) {
	return presentation.ResolveScene(scene), nil
}

type failingAudioResolver struct{ err error }

func (f failingAudioResolver) ResolveScene(_ string, scene presentation.Scene) (presentation.ResolvedScene, error) {
	return presentation.ResolveScene(scene), nil
}

func (f failingAudioResolver) ResolveAudioCue(string, presentation.AudioCue) (presentation.ResolvedAudioCue, error) {
	return presentation.ResolvedAudioCue{}, f.err
}

func (builtinAudioResolver) ResolveAudioCue(_ string, cue presentation.AudioCue) (presentation.ResolvedAudioCue, error) {
	resolved := presentation.ResolveAudioCue(cue)
	switch cue.Asset.StockName {
	case "calendar_reminder_ends.snd":
		resolved.Path = "shared/sounds/calendar_reminder_ends.snd"
	case "calendar_event_starts.snd":
		resolved.Path = "shared/sounds/calendar_event_starts.snd"
	}
	return resolved, nil
}

type fakeDisplay struct {
	operations    []string
	draws         []busylib.DisplayElements
	drawErr       error
	clearErr      error
	clearCount    int
	snapshot      busylib.BusySnapshot
	deviceTime    busylib.TimestampInfo
	setSnapshots  []busylib.BusySnapshot
	ignoreBusySet bool
	audio         []busylib.PlayAudio
	audioErr      error
}

type cancelAfterDrawBackend struct {
	started, release, completed chan struct{}
	operations                  []string
}

func (b *cancelAfterDrawBackend) Draw(context.Context, busylib.DisplayElements) error {
	b.operations = append(b.operations, "draw")
	close(b.started)
	<-b.release
	close(b.completed)
	return nil
}
func (b *cancelAfterDrawBackend) Clear(context.Context, string) error {
	b.operations = append(b.operations, "clear")
	return nil
}
func (*cancelAfterDrawBackend) UploadFile(context.Context, string, string, string) error { return nil }
func (*cancelAfterDrawBackend) ReadTo(context.Context, string, io.Writer) (int64, error) {
	return 0, nil
}
func (*cancelAfterDrawBackend) Remove(context.Context, string) error { return nil }

func (d *fakeDisplay) PlayAudio(_ context.Context, value busylib.PlayAudio) error {
	d.audio = append(d.audio, value)
	d.operations = append(d.operations, "audio:play")
	return d.audioErr
}

func (d *fakeDisplay) Draw(_ context.Context, request busylib.DisplayElements) error {
	d.draws = append(d.draws, request)
	if text, ok := request.Elements[0].(busylib.TextElement); ok {
		d.operations = append(d.operations, "draw:"+itoa(request.Priority)+":"+text.Text)
	} else {
		d.operations = append(d.operations, "draw:"+itoa(request.Priority)+":asset")
	}
	return d.drawErr
}

func (d *fakeDisplay) Clear(_ context.Context, applicationName string) error {
	d.clearCount++
	d.operations = append(d.operations, "clear:"+applicationName)
	return d.clearErr
}

func (d *fakeDisplay) BusySnapshot(context.Context) (busylib.BusySnapshot, error) {
	d.operations = append(d.operations, "busy:snapshot")
	return d.snapshot, nil
}

func (d *fakeDisplay) SetBusySnapshot(_ context.Context, value busylib.BusySnapshot) error {
	d.operations = append(d.operations, "busy:set")
	d.setSnapshots = append(d.setSnapshots, value)
	if !d.ignoreBusySet {
		d.snapshot = value
	}
	return nil
}

func (d *fakeDisplay) DeviceTime(context.Context) (busylib.TimestampInfo, error) {
	d.operations = append(d.operations, "time:now")
	return d.deviceTime, nil
}

func candidate(priority int, text string) presentation.Candidate {
	return presentation.Candidate{
		PluginID: "p", InstanceID: "i", Channel: "c", Key: "k", Revision: 1, Generation: 1, AdmissionSequence: 1,
		Policy: presentation.PolicyWhenRelevant, Band: presentation.BandRelevant, Impact: protocol.ImpactNormal, DevicePriority: priority,
		Scene: presentation.Scene{Elements: []presentation.Element{{ID: "text", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: text, Font: "normal"}}}},
	}
}

func timerCandidate(expiresAt time.Time) presentation.Candidate {
	return presentation.Candidate{
		PluginID: "calendar", InstanceID: "work", Channel: "active", Key: "opaque-occurrence", Revision: 7, Generation: 2, AdmissionSequence: 1,
		Policy: presentation.PolicyWhenRelevant, Band: presentation.BandRelevant, Impact: protocol.ImpactNormal, ExpiresAt: expiresAt,
		BusyTimer: &protocol.BusyTimerPresentation{Theme: "meeting"},
	}
}

func newTimerDisplay(now time.Time) *fakeDisplay {
	return &fakeDisplay{
		snapshot: busylib.BusySnapshot{Snapshot: busylib.BusySnapshotData{
			Type: busylib.BusySnapshotNotStarted, BusyBarSettings: busylib.BusyBarSettings{Theme: "on_air"},
		}, SnapshotTimestampMS: now.Add(-time.Second).UnixMilli()},
		deviceTime: busylib.TimestampInfo{Timestamp: now.Format(time.RFC3339)},
	}
}

func itoa(value int) string {
	if value == 100 {
		return "100"
	}
	if value == 60 {
		return "60"
	}
	return "20"
}

func equalOperations(a, b []string) bool {
	return reflect.DeepEqual(a, b)
}
