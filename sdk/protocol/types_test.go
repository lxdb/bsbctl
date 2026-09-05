package protocol

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestV1UsesOneExactProtocolVersion(t *testing.T) {
	if Version != "1.0" {
		t.Fatalf("version = %q, want 1.0", Version)
	}
	wire, err := json.Marshal(InitializeRequest{CoreVersion: "1.2.3", PluginID: "dev.bsbctl.calendar", PluginVersion: "2.0.0", ProtocolVersion: Version})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(wire), `{"core_version":"1.2.3","plugin_id":"dev.bsbctl.calendar","plugin_version":"2.0.0","protocol_version":"1.0"}`; got != want {
		t.Fatalf("initialize request = %s, want %s", got, want)
	}
}

func TestObservationValidateRejectsMissingFreshness(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	valid := Observation{
		Instance: InstanceRef{ID: "app", Generation: 1}, Channel: "main", Key: "state", Revision: 1,
		Disposition: DispositionActionable, Impact: ImpactNotable,
		ReasonCode: "build_failed", ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(time.Minute),
		Scene: &Scene{Elements: []Element{{ID: "icon", Display: DisplayFront, Image: &ImageElement{Asset: AssetRef{PackagePath: "assets/failed-icon.png"}}}}},
	}
	if err := valid.Validate(now); err != nil {
		t.Fatalf("valid observation: %v", err)
	}

	missingExpiry := valid
	missingExpiry.ValidUntil = time.Time{}
	if err := missingExpiry.Validate(now); err == nil {
		t.Fatal("observation without valid_until unexpectedly validated")
	}
}

func TestResolvedObservationRequiresNoPresentation(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	resolved := Observation{
		Instance: InstanceRef{ID: "app", Generation: 1}, Channel: "main", Key: "state", Revision: 2,
		Disposition: DispositionResolved, Impact: ImpactNormal,
		ReasonCode: "build_recovered", ObservedAt: now, UpdatedAt: now,
	}
	if err := resolved.Validate(now); err != nil {
		t.Fatalf("resolved observation: %v", err)
	}
	resolved.Scene = &Scene{Elements: []Element{{ID: "text", Display: DisplayFront, Text: &TextElement{Value: "done"}}}}
	if err := resolved.Validate(now); err == nil {
		t.Fatal("resolved observation with a presentation unexpectedly validated")
	}
}

func FuzzDecodeStrictOperationRequest(f *testing.F) {
	f.Add([]byte(`{"instance":{"id":"main","generation":7},"operation":"inspect","payload":{"detail":true}}`))
	f.Add([]byte(`{"instance":{"id":"main","generation":0},"operation":"inspect","payload":[]}`))
	f.Add([]byte(`{"instance":{"id":"main","generation":7},"operation":"inspect","unknown":true}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxMessageBytes {
			t.Skip()
		}
		var request OperationRequest
		if err := DecodeStrict(data, &request); err != nil || request.Validate() != nil {
			return
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatalf("marshal accepted request: %v", err)
		}
		var roundTrip OperationRequest
		if err := DecodeStrict(encoded, &roundTrip); err != nil || roundTrip.Validate() != nil {
			t.Fatalf("accepted request did not round trip: %s", encoded)
		}
	})
}

func TestInstanceContainsOnlyPluginOwnedState(t *testing.T) {
	instance := Instance{ID: "calendar", Generation: 7, Config: json.RawMessage(`{"calendar":"Work"}`), Secrets: map[string]string{"token": "resolved"}, Checkpoint: json.RawMessage(`{"cursor":"opaque"}`)}
	if err := instance.Validate(); err != nil {
		t.Fatalf("valid instance: %v", err)
	}
	wire, err := json.Marshal(instance)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(wire), `{"id":"calendar","generation":7,"config":{"calendar":"Work"},"secrets":{"token":"resolved"},"checkpoint":{"cursor":"opaque"}}`; got != want {
		t.Fatalf("instance = %s, want %s", got, want)
	}
	for _, invalid := range []Instance{
		{ID: "calendar", Generation: 0, Config: json.RawMessage(`{}`)},
		{ID: "calendar", Generation: 1, Config: json.RawMessage(`[]`)},
		{ID: "calendar", Generation: 1, Config: json.RawMessage(`{}`), Checkpoint: json.RawMessage(`null`)},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid instance was accepted: %#v", invalid)
		}
	}
}

func TestInstanceConfigUsesExact64KiBObjectLimit(t *testing.T) {
	objectOfSize := func(size int) json.RawMessage {
		const shell = `{"x":""}`
		return json.RawMessage(`{"x":"` + strings.Repeat("x", size-len(shell)) + `"}`)
	}
	for _, test := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "at limit", size: MaxConfigObjectBytes},
		{name: "over limit", size: MaxConfigObjectBytes + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := objectOfSize(test.size)
			if len(config) != test.size {
				t.Fatalf("config size = %d, want %d", len(config), test.size)
			}
			err := (Instance{ID: "calendar", Generation: 1, Config: config}).Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate error = %v, wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func TestSharedJSONObjectValidatorsUseOneExactBoundary(t *testing.T) {
	objectOfSize := func(size int) json.RawMessage {
		const shell = `{"x":""}`
		return json.RawMessage(`{"x":"` + strings.Repeat("x", size-len(shell)) + `"}`)
	}
	for _, test := range []struct {
		name    string
		value   json.RawMessage
		wantErr bool
	}{
		{name: "object at limit", value: objectOfSize(MaxJSONObjectBytes)},
		{name: "object over limit", value: objectOfSize(MaxJSONObjectBytes + 1), wantErr: true},
		{name: "scalar", value: json.RawMessage(`"value"`), wantErr: true},
		{name: "array", value: json.RawMessage(`[]`), wantErr: true},
		{name: "null", value: json.RawMessage(`null`), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateJSONObject("value", test.value, false)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateJSONObject(%d bytes) error = %v, wantErr=%t", len(test.value), err, test.wantErr)
			}
		})
	}
	if err := ValidateJSONObject("optional value", nil, true); err != nil {
		t.Fatalf("optional omitted object: %v", err)
	}
	if err := ValidateJSONObject("required value", nil, false); err == nil {
		t.Fatal("required omitted object was accepted")
	}
}

func TestValidateEmptyParamsAcceptsOnlyOmittedOrEmptyObject(t *testing.T) {
	for _, params := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(" \n {} \t")} {
		if err := ValidateEmptyParams(params); err != nil {
			t.Fatalf("valid params %s: %v", params, err)
		}
	}
	for _, params := range []json.RawMessage{
		json.RawMessage(`{"unknown":true}`),
		json.RawMessage(`[]`),
		json.RawMessage(`null`),
		json.RawMessage(`"scalar"`),
	} {
		if err := ValidateEmptyParams(params); err == nil {
			t.Fatalf("invalid params %s were accepted", params)
		}
	}
}

func TestInstanceBoundMethodShapesUseExactReference(t *testing.T) {
	ref := InstanceRef{ID: "calendar", Generation: 7}
	values := []struct {
		name  string
		value any
		want  string
	}{
		{"session start", SessionStartRequest{Instance: ref, Action: "open", Payload: json.RawMessage(`{"screen":"detail"}`), SessionToken: "session-1"}, `{"instance":{"id":"calendar","generation":7},"action":"open","payload":{"screen":"detail"},"session_token":"session-1"}`},
		{"session end", SessionEndRequest{Instance: ref, SessionToken: "session-1"}, `{"instance":{"id":"calendar","generation":7},"session_token":"session-1"}`},
		{"operation", OperationRequest{Instance: ref, Operation: "refresh", Payload: json.RawMessage(`{"force":true}`)}, `{"instance":{"id":"calendar","generation":7},"operation":"refresh","payload":{"force":true}}`},
		{"withdraw", WithdrawRequest{Instance: ref, Channel: "active", Key: "meeting"}, `{"instance":{"id":"calendar","generation":7},"channel":"active","key":"meeting"}`},
		{"checkpoint", CheckpointRequest{Instance: ref, Data: json.RawMessage(`{"cursor":9}`)}, `{"instance":{"id":"calendar","generation":7},"data":{"cursor":9}}`},
		{"begin execution", SessionExecutionRequest{Instance: ref, SessionToken: "session-1"}, `{"instance":{"id":"calendar","generation":7},"session_token":"session-1"}`},
		{"complete session", CompleteSessionRequest{Instance: ref, SessionToken: "session-1"}, `{"instance":{"id":"calendar","generation":7},"session_token":"session-1"}`},
	}
	for _, test := range values {
		t.Run(test.name, func(t *testing.T) {
			wire, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(wire); got != test.want {
				t.Fatalf("wire = %s, want %s", got, test.want)
			}
		})
	}
}

func TestObservationUsesExactPresentationAndElementUnions(t *testing.T) {
	now := time.Date(2026, time.August, 28, 18, 0, 0, 0, time.UTC)
	observation := Observation{
		Instance: InstanceRef{ID: "calendar", Generation: 7}, Channel: "upcoming", Key: "meeting", Revision: 1,
		Disposition: DispositionActionable, Impact: ImpactNotable, ReasonCode: "meeting_upcoming",
		ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(5 * time.Minute),
		Scene: &Scene{Elements: []Element{
			{ID: "title", Display: DisplayFront, X: 3, Y: 2, Text: &TextElement{Value: "Planning", Font: "normal", Color: "#FFFFFFFF", Marquee: &Marquee{PixelsPerMinute: 600, StartDelayMilliseconds: 1000, RepeatDelayMilliseconds: 2500}}},
			{ID: "icon", Display: DisplayFront, Image: &ImageElement{Asset: AssetRef{StockName: "clock_5x5.image"}}},
			{ID: "animation", Display: DisplayBack, Animation: &AnimationElement{Asset: AssetRef{PackagePath: "assets/calendar-logo.anim"}, Loop: true}},
			{ID: "bar", Display: DisplayBack, Rectangle: &RectangleElement{Width: 10, Height: 2, Color: "#00FF00FF"}},
			{ID: "countdown", Display: DisplayFront, Countdown: &CountdownElement{EndsAtUnixSeconds: now.Add(time.Minute).Unix(), ShowHours: CountdownShowHoursWhenNonZero, Color: "#FFFFFFFF"}},
		}},
		Audio: &AudioCue{ID: "reminder", Asset: AssetRef{StockName: "calendar_reminder_ends.snd"}, ExpiresAt: now.Add(15 * time.Second)},
	}
	if err := observation.Validate(now); err != nil {
		t.Fatalf("valid observation: %v", err)
	}
	wire, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), `"path"`) || strings.Contains(string(wire), "time_since") || strings.Contains(string(wire), `"type"`) {
		t.Fatalf("v3 presentation escaped onto wire: %s", wire)
	}
	var decoded Observation
	if err := DecodeStrict(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(now); err != nil {
		t.Fatalf("decoded observation: %v", err)
	}

	invalidElement := observation
	invalidElement.Scene = &Scene{Elements: []Element{{ID: "both", Display: DisplayFront, Text: &TextElement{Value: "x"}, Image: &ImageElement{Asset: AssetRef{StockName: "clock_5x5.image"}}}}}
	if err := invalidElement.Validate(now); err == nil {
		t.Fatal("element with two payloads was accepted")
	}
	missingVisual := observation
	missingVisual.Scene = nil
	if err := missingVisual.Validate(now); err == nil {
		t.Fatal("unresolved observation without a visual was accepted")
	}
	bothVisuals := observation
	bothVisuals.BusyTimer = &BusyTimerPresentation{Theme: "meeting"}
	if err := bothVisuals.Validate(now); err == nil {
		t.Fatal("unresolved observation with two visuals was accepted")
	}
	resolved := observation
	resolved.Disposition = DispositionResolved
	resolved.ValidUntil = time.Time{}
	resolved.Scene = nil
	resolved.Audio = nil
	if err := resolved.Validate(now); err != nil {
		t.Fatalf("resolved observation without presentation: %v", err)
	}
}

func TestAudioCueRejectsPackageAssetsForFirstRelease(t *testing.T) {
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	cue := AudioCue{
		ID:        "reminder",
		Asset:     AssetRef{PackagePath: "assets/reminder-sound.snd"},
		ExpiresAt: now.Add(time.Minute),
	}
	if err := cue.Validate(now, now.Add(2*time.Minute)); err == nil {
		t.Fatal("package-backed audio was accepted")
	}
}

func TestAssetReferencesUseExactPackageOrTypedStockBasename(t *testing.T) {
	for _, asset := range []AssetRef{{PackagePath: "assets/codex-mark.png"}, {StockName: "clock_5x5.image"}, {StockName: "calendar_event_16x16.anim"}, {StockName: "calendar_event_starts.snd"}} {
		if err := asset.Validate(); err != nil {
			t.Fatalf("valid asset %#v: %v", asset, err)
		}
	}
	for _, asset := range []AssetRef{
		{},
		{PackagePath: "assets/codex-mark.png", StockName: "clock_5x5.image"},
		{PackagePath: "assets/./codex-mark.png"},
		{PackagePath: "../codex-mark.png"},
		{StockName: "shared/images/clock_5x5.image"},
		{StockName: `shared\\clock_5x5.image`},
		{StockName: ".."},
		{StockName: "clock\n.image"},
		{StockName: "clock.png"},
	} {
		if err := asset.Validate(); err == nil {
			t.Fatalf("invalid asset %#v was accepted", asset)
		}
	}
	for _, element := range []Element{
		{ID: "image", Display: DisplayFront, Image: &ImageElement{Asset: AssetRef{StockName: "motion.anim"}}},
		{ID: "animation", Display: DisplayFront, Animation: &AnimationElement{Asset: AssetRef{StockName: "icon.image"}}},
	} {
		if err := element.Validate(); err == nil {
			t.Fatalf("stock media kind mismatch %#v was accepted", element)
		}
	}
}

func TestObservationPreservesPresentationLimits(t *testing.T) {
	now := time.Date(2026, time.August, 28, 18, 0, 0, 0, time.UTC)
	base := Observation{
		Instance: InstanceRef{ID: "calendar", Generation: 1}, Channel: "active", Key: "meeting", Revision: 1,
		Disposition: DispositionNotable, Impact: ImpactNormal, ReasonCode: "meeting_active", ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(time.Minute),
	}
	base.Scene = &Scene{Elements: []Element{{ID: "title", Display: DisplayFront, Text: &TextElement{Value: strings.Repeat("x", MaxTextBytes+1)}}}}
	if err := base.Validate(now); err == nil {
		t.Fatal("oversized text was accepted")
	}
	base.Scene = nil
	base.BusyTimer = &BusyTimerPresentation{Theme: "meeting"}
	base.ValidUntil = now.Add(MaxBusyTimerDuration + time.Second)
	if err := base.Validate(now); err == nil {
		t.Fatal("oversized busy timer was accepted")
	}
}

func TestBusyTimerThemeUsesSafeFirmwareDirectoryName(t *testing.T) {
	now := time.Date(2026, time.August, 28, 18, 0, 0, 0, time.UTC)
	base := Observation{
		Instance: InstanceRef{ID: "calendar", Generation: 1}, Channel: "active", Key: "meeting", Revision: 1,
		Disposition: DispositionNotable, Impact: ImpactNormal, ReasonCode: "meeting_active",
		ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(time.Minute),
	}
	for _, theme := range []string{"busy", "meeting", "on_air"} {
		value := base
		value.BusyTimer = &BusyTimerPresentation{Theme: theme}
		if err := value.Validate(now); err != nil {
			t.Fatalf("theme %q: %v", theme, err)
		}
	}
	for _, theme := range []string{"busy.theme", "themes/busy", `themes\\busy`, "..", "../busy"} {
		value := base
		value.BusyTimer = &BusyTimerPresentation{Theme: theme}
		if err := value.Validate(now); err == nil {
			t.Fatalf("unsafe theme %q was accepted", theme)
		}
	}
}

func TestSessionInputRequestUsesExactSessionAndInputVariant(t *testing.T) {
	now := time.Date(2026, time.August, 28, 18, 0, 0, 0, time.UTC)
	tests := []SessionInputRequest{
		{Sequence: 5, OccurredAt: now, Instance: InstanceRef{ID: "calendar", Generation: 7}, SessionToken: "session-1", Input: SessionInput{Button: &ButtonInput{Button: ButtonOK, Action: ButtonPress}}},
		{Sequence: 6, OccurredAt: now, Instance: InstanceRef{ID: "calendar", Generation: 7}, SessionToken: "session-1", Input: SessionInput{Encoder: &EncoderInput{Delta: -1}}},
	}
	for _, request := range tests {
		if err := request.Validate(); err != nil {
			t.Fatalf("valid session input: %v", err)
		}
		wire, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var decoded SessionInputRequest
		if err := DecodeStrict(wire, &decoded); err != nil {
			t.Fatal(err)
		}
		if err := decoded.Validate(); err != nil {
			t.Fatalf("decoded session input: %v", err)
		}
	}

	missing := tests[0]
	missing.Input = SessionInput{}
	if err := missing.Validate(); err == nil {
		t.Fatal("session input without a variant was accepted")
	}
	twoInputs := tests[0]
	twoInputs.Input.Encoder = &EncoderInput{Delta: 1}
	if err := twoInputs.Validate(); err == nil {
		t.Fatal("session input with two variants was accepted")
	}
	stale := tests[0]
	stale.Instance.Generation = 0
	if err := stale.Validate(); err == nil {
		t.Fatal("session input without an exact generation was accepted")
	}
}

func TestSessionInputResultUsesExactDisposition(t *testing.T) {
	for _, disposition := range []SessionInputDisposition{SessionInputConsumed, SessionInputNotConsumed} {
		if err := (SessionInputResult{Disposition: disposition}).Validate(); err != nil {
			t.Fatalf("valid disposition %q: %v", disposition, err)
		}
	}
	for _, disposition := range []SessionInputDisposition{"", "handled"} {
		if err := (SessionInputResult{Disposition: disposition}).Validate(); err == nil {
			t.Fatalf("invalid disposition %q was accepted", disposition)
		}
	}
}

func TestPluginOwnedObjectsAreBoundedJSONObjects(t *testing.T) {
	ref := InstanceRef{ID: "calendar", Generation: 1}
	valid := []interface{ Validate() error }{
		SessionStartRequest{Instance: ref, Action: "open", Payload: json.RawMessage(`{}`), SessionToken: "session"},
		OperationRequest{Instance: ref, Operation: "refresh", Payload: json.RawMessage(`{}`)},
		OperationResult{Payload: json.RawMessage(`{"ok":true}`)},
		CheckpointRequest{Instance: ref, Data: json.RawMessage(`{"cursor":1}`)},
	}
	for _, value := range valid {
		if err := value.Validate(); err != nil {
			t.Fatalf("valid plugin object: %v", err)
		}
	}
	for _, payload := range []json.RawMessage{json.RawMessage(`[]`), json.RawMessage(`null`), json.RawMessage(`"scalar"`)} {
		if err := (OperationRequest{Instance: ref, Operation: "refresh", Payload: payload}).Validate(); err == nil {
			t.Fatalf("operation request payload %s was accepted", payload)
		}
		if err := (OperationResult{Payload: payload}).Validate(); err == nil {
			t.Fatalf("operation result payload %s was accepted", payload)
		}
		if err := (CheckpointRequest{Instance: ref, Data: payload}).Validate(); err == nil {
			t.Fatalf("checkpoint payload %s was accepted", payload)
		}
	}
	if err := (CheckpointRequest{Instance: ref, Data: json.RawMessage(`{"value":"` + strings.Repeat("x", MaxCheckpointObjectBytes) + `"}`)}).Validate(); err == nil {
		t.Fatal("oversized checkpoint object was accepted")
	}
}

func TestDomainErrorsExposeOnlyStableKinds(t *testing.T) {
	cause := errors.New("token=secret private failure")
	err := NewDomainError(ErrorNotReady, cause)
	if !errors.Is(err, cause) {
		t.Fatal("domain error did not preserve its private cause locally")
	}
	if err.Error() != "not_ready" {
		t.Fatalf("error = %q, want stable kind", err.Error())
	}
	if data := err.Data(); data != (ErrorData{Kind: ErrorNotReady}) {
		t.Fatalf("error data = %#v", data)
	}
}

func TestSessionExecutionRequestRequiresExactSession(t *testing.T) {
	valid := SessionExecutionRequest{Instance: InstanceRef{ID: "calendar", Generation: 7}, SessionToken: "session-1"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	for _, request := range []SessionExecutionRequest{
		{Instance: InstanceRef{ID: "calendar", Generation: 7}},
		{Instance: InstanceRef{ID: "calendar"}, SessionToken: "session-1"},
		{Instance: InstanceRef{ID: "calendar", Generation: 7}, SessionToken: "bad\nvalue"},
	} {
		if err := request.Validate(); err == nil {
			t.Fatalf("invalid execution request was accepted: %#v", request)
		}
	}
}

func TestDecodeRemoteErrorAcceptsOnlyStandardOrValidDomainErrors(t *testing.T) {
	kind, domain, err := DecodeRemoteError(DomainErrorCode, json.RawMessage(`{"kind":"not_ready"}`))
	if err != nil || !domain || kind != ErrorNotReady {
		t.Fatalf("valid domain error = %q, %v, %v", kind, domain, err)
	}
	for name, test := range map[string]struct {
		code int
		data json.RawMessage
	}{
		"missing domain data": {code: DomainErrorCode},
		"unknown domain kind": {code: DomainErrorCode, data: json.RawMessage(`{"kind":"unsupported"}`)},
		"private server code": {code: -32021},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := DecodeRemoteError(test.code, test.data); err == nil {
				t.Fatal("malformed remote error was accepted")
			}
		})
	}
	if _, domain, err := DecodeRemoteError(-32603, nil); err != nil || domain {
		t.Fatalf("standard internal error = domain:%v err:%v", domain, err)
	}
}

func TestExecutionDomainErrorKindsAreStable(t *testing.T) {
	for _, want := range []ErrorKind{ErrorSessionNotActive, ErrorSessionCanceled, ErrorSessionGenerationMismatch} {
		wire, err := json.Marshal(ErrorData{Kind: want})
		if err != nil {
			t.Fatal(err)
		}
		got, domain, err := DecodeRemoteError(DomainErrorCode, wire)
		if err != nil || !domain || got != want {
			t.Fatalf("kind %q decoded as %q/%t/%v", want, got, domain, err)
		}
	}
}

func TestHealthAndMetricValidation(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	if err := (HealthResult{Healthy: true, ObservedAt: now}).Validate(); err != nil {
		t.Fatalf("valid health: %v", err)
	}
	for _, health := range []HealthResult{{Healthy: true}, {Healthy: true, ObservedAt: now.In(time.FixedZone("offset", 3600))}} {
		if err := health.Validate(); err == nil {
			t.Fatalf("invalid health accepted: %#v", health)
		}
	}
	if err := (MetricNotification{Instance: InstanceRef{ID: "app", Generation: 7}, Name: "queue.depth", Value: 2, Unit: "items"}).Validate(); err != nil {
		t.Fatalf("valid metric: %v", err)
	}
	for _, metric := range []MetricNotification{
		{Name: "", Value: 1},
		{Name: "queue.depth", Value: math.NaN()},
		{Name: "queue.depth", Value: math.Inf(1)},
		{Instance: InstanceRef{ID: "app"}, Name: "queue.depth", Value: 1},
		{Name: "queue.depth", Value: 1, Unit: "bad\nunit"},
	} {
		if err := metric.Validate(); err == nil {
			t.Fatalf("invalid metric accepted: %#v", metric)
		}
	}
}
