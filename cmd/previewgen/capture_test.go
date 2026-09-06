//go:build preview

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	busylib "github.com/lxdb/busylib-go"
)

func TestCaptureRejectsConfigurationOutsideTheCanonicalDeviceContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	want := []byte(`{"version":9,"device":{"base_url":"http://busybar.test"},"plugins":{},"apps":{}}`)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := captureDeviceFixtures(t.Context(), options{Config: path})
	if err == nil || !strings.Contains(err.Error(), "configuration version must be 1") {
		t.Fatalf("capture error = %v, want canonical configuration-version rejection", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(want) {
		t.Fatalf("capture config changed from %q to %q", want, after)
	}
}

func TestCleanupCaptureDeviceUsesLiveBoundedContextAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	display := &cleanupCaptureDisplay{}
	var assetContextErr error

	err := cleanupCaptureDevice(ctx, display, func(cleanupCtx context.Context) error {
		assetContextErr = cleanupCtx.Err()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if display.contextErr != nil || assetContextErr != nil {
		t.Fatalf("cleanup contexts = display:%v assets:%v, want both live", display.contextErr, assetContextErr)
	}
}

type cleanupCaptureDisplay struct {
	contextErr error
}

func (d *cleanupCaptureDisplay) Clear(ctx context.Context, _ string) error {
	d.contextErr = ctx.Err()
	return nil
}

func (*cleanupCaptureDisplay) Draw(context.Context, busylib.DisplayElements) error { return nil }

func (*cleanupCaptureDisplay) Screen(context.Context, busylib.DisplayTarget) ([]byte, error) {
	return nil, nil
}

func TestCaptureFixtureSetEncodesCompleteOpaqueDeviceRenderedGIFs(t *testing.T) {
	clock := &fakeCaptureClock{value: time.Unix(0, 0)}
	device := &fakeCaptureDisplay{clock: clock, animate: true}
	scenarios := []previewScenario{
		{Name: "Calendar", File: "calendar-front.gif", Capture: true, Steps: []previewStep{{Duration: 500 * time.Millisecond, Draw: busylib.DisplayElements{ApplicationName: "red"}}}, Duration: 500 * time.Millisecond},
		{Name: "Codex", File: "codex-front.gif", Capture: true, Steps: []previewStep{{Duration: 500 * time.Millisecond, Draw: busylib.DisplayElements{ApplicationName: "green"}}}, Duration: 500 * time.Millisecond},
		{Name: "Codex quota", File: "codex-quota-front.gif", Capture: true, Steps: []previewStep{{Duration: 500 * time.Millisecond, Draw: busylib.DisplayElements{ApplicationName: "blue"}}}, Duration: 500 * time.Millisecond},
		{Name: "GitHub notifications", File: "github-notifications-front.gif", Capture: true, Steps: []previewStep{{Duration: 500 * time.Millisecond, Draw: busylib.DisplayElements{ApplicationName: "yellow"}}}, Duration: 500 * time.Millisecond},
	}

	fixtures, err := captureFixtureSet(
		t.Context(),
		device,
		scenarios,
		captureTiming{now: clock.Now, wait: clock.Wait},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != len(capturedPreviewArtifactNames) {
		t.Fatalf("captured fixture count = %d, want %d", len(fixtures), len(capturedPreviewArtifactNames))
	}
	for index, fixture := range fixtures {
		if fixture.name != scenarios[index].File || fixture.format != "gif" {
			t.Fatalf("fixture %d = %#v, want %s GIF", index, fixture, scenarios[index].File)
		}
		digest := sha256.Sum256(fixture.data)
		if got := hex.EncodeToString(digest[:]); fixture.sha256 != got {
			t.Fatalf("fixture %d checksum = %q, want %q", index, fixture.sha256, got)
		}
		frames, err := decodeMockGIF(fixture.data)
		if err != nil {
			t.Fatalf("decode fixture %d: %v", index, err)
		}
		total := 0
		for _, frame := range frames {
			total += frame.Delay
			for y := range frontHeight {
				for x := range frontWidth {
					if alpha := frame.Image.RGBAAt(x, y).A; alpha != 0xff {
						t.Fatalf("fixture %d decoded alpha = %d, want opaque", index, alpha)
					}
				}
			}
		}
		if total != 50 {
			t.Fatalf("fixture %d duration = %d centiseconds, want 50", index, total)
		}
	}
}

func TestCaptureFixtureSetSelectsCapturedPreviewsIndependentOfScenarioOrder(t *testing.T) {
	clock := &fakeCaptureClock{value: time.Unix(0, 0)}
	device := &fakeCaptureDisplay{clock: clock}
	scenarios := []previewScenario{
		{Name: "Mac resources", File: "mac-resources-front.gif", Steps: []previewStep{{Duration: 10 * time.Millisecond, Draw: busylib.DisplayElements{ApplicationName: "blue"}}}, Duration: 10 * time.Millisecond},
		{Name: "Codex quota", File: "codex-quota-front.gif", Capture: true, Steps: []previewStep{{Duration: 10 * time.Millisecond, Draw: busylib.DisplayElements{ApplicationName: "blue"}}}, Duration: 10 * time.Millisecond},
		{Name: "Calendar", File: "calendar-front.gif", Capture: true, Steps: []previewStep{{Duration: 10 * time.Millisecond, Draw: busylib.DisplayElements{ApplicationName: "red"}}}, Duration: 10 * time.Millisecond},
		{Name: "Codex", File: "codex-front.gif", Capture: true, SampleInterval: 300 * time.Millisecond, Steps: []previewStep{{Duration: 10 * time.Millisecond, Draw: busylib.DisplayElements{ApplicationName: "green"}}}, Duration: 10 * time.Millisecond},
		{Name: "GitHub notifications", File: "github-notifications-front.gif", Capture: true, Steps: []previewStep{{Duration: 10 * time.Millisecond, Draw: busylib.DisplayElements{ApplicationName: "yellow"}}}, Duration: 10 * time.Millisecond},
	}

	fixtures, err := captureFixtureSet(t.Context(), device, scenarios, captureTiming{now: clock.Now, wait: clock.Wait})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(fixtures))
	for _, fixture := range fixtures {
		got[fixture.name] = true
	}
	for _, name := range capturedPreviewArtifactNames {
		if !got[name] {
			t.Errorf("captured fixtures %v do not include %s", got, name)
		}
	}
	if got["mac-resources-front.gif"] || len(got) != len(capturedPreviewArtifactNames) {
		t.Fatalf("captured fixtures = %v, want only the device-captured previews", got)
	}
}

func TestCaptureScenarioClearsAndSettlesEverySceneBeforeRecording(t *testing.T) {
	clock := &fakeCaptureClock{value: time.Unix(0, 0)}
	device := &fakeCaptureDisplay{clock: clock}
	scenario := previewScenario{
		Name: "test",
		Steps: []previewStep{
			{Duration: 500 * time.Millisecond, Draw: busylib.DisplayElements{ApplicationName: "red"}},
			{Duration: 500 * time.Millisecond, Draw: busylib.DisplayElements{ApplicationName: "green"}},
		},
		Duration: time.Second,
	}

	frames, err := captureScenario(
		t.Context(),
		device,
		scenario,
		captureTiming{now: clock.Now, wait: clock.Wait},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 4 {
		t.Fatalf("captured frames = %d, want two samples for each scene", len(frames))
	}
	want := []color.RGBA{
		{R: 255, A: 255}, {R: 255, A: 255},
		{G: 255, A: 255}, {G: 255, A: 255},
	}
	for index, frame := range frames {
		if frame.Delay != 25 {
			t.Fatalf("frame %d delay = %d, want 25 centiseconds", index, frame.Delay)
		}
		if got := frame.Image.RGBAAt(0, 0); got != want[index] {
			t.Fatalf("frame %d pixel = %#v, want settled %#v", index, got, want[index])
		}
	}
}

func TestCaptureScenarioKeepsCompleteCodexDurationAtPublishableCadence(t *testing.T) {
	clock := &fakeCaptureClock{value: time.Unix(0, 0)}
	device := &fakeCaptureDisplay{clock: clock, animate: true}
	scenario := previewScenario{
		Name: "Codex", SampleInterval: 300 * time.Millisecond, Steps: []previewStep{{Duration: 600 * time.Millisecond, Draw: busylib.DisplayElements{ApplicationName: "red"}}}, Duration: 600 * time.Millisecond,
	}
	frames, err := captureScenario(t.Context(), device, scenario, captureTiming{now: clock.Now, wait: clock.Wait})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || frames[0].Delay != 30 || frames[1].Delay != 30 {
		t.Fatalf("Codex frames/delays = %d/%v, want two 300ms samples", len(frames), []int{frames[0].Delay, frames[1].Delay})
	}
}

func TestCaptureScenarioCadenceDoesNotDependOnDisplayName(t *testing.T) {
	clock := &fakeCaptureClock{value: time.Unix(0, 0)}
	device := &fakeCaptureDisplay{clock: clock, animate: true}
	scenario := previewScenario{
		Name: "Renamed feature tour", File: "codex-front.gif", SampleInterval: 300 * time.Millisecond,
		Steps: []previewStep{{Duration: 600 * time.Millisecond, Draw: busylib.DisplayElements{ApplicationName: "red"}}}, Duration: 600 * time.Millisecond,
	}

	frames, err := captureScenario(t.Context(), device, scenario, captureTiming{now: clock.Now, wait: clock.Wait})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || frames[0].Delay != 30 || frames[1].Delay != 30 {
		t.Fatalf("renamed Codex frames = %d/%v, want two 300ms samples", len(frames), frameDelays(frames))
	}
}

func frameDelays(frames []timedFrame) []int {
	delays := make([]int, len(frames))
	for index, frame := range frames {
		delays[index] = frame.Delay
	}
	return delays
}

type fakeCaptureClock struct {
	value time.Time
}

func (c *fakeCaptureClock) Now() time.Time { return c.value }

func (c *fakeCaptureClock) Wait(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if duration > 0 {
		c.value = c.value.Add(duration)
	}
	return nil
}

type fakeCaptureDisplay struct {
	clock       *fakeCaptureClock
	application string
	drawnAt     time.Time
	cleared     bool
	animate     bool
	screens     int
}

func (d *fakeCaptureDisplay) Clear(context.Context, string) error {
	d.cleared = true
	d.application = ""
	return nil
}

func (d *fakeCaptureDisplay) Draw(_ context.Context, draw busylib.DisplayElements) error {
	if !d.cleared {
		d.application = "corrupt"
	} else {
		d.application = draw.ApplicationName
	}
	d.cleared = false
	d.drawnAt = d.clock.Now()
	return nil
}

func (d *fakeCaptureDisplay) Screen(context.Context, busylib.DisplayTarget) ([]byte, error) {
	value := color.RGBA{B: 255, A: 255}
	if d.clock.Now().Sub(d.drawnAt) >= 500*time.Millisecond {
		switch d.application {
		case "red":
			value = color.RGBA{R: 255, A: 255}
		case "green":
			value = color.RGBA{G: 255, A: 255}
		}
	}
	if d.animate && d.screens%2 == 1 {
		value.R /= 2
		value.G /= 2
		value.B /= 2
	}
	d.screens++
	result := make([]byte, frontWidth*frontHeight*3)
	for index := 0; index < len(result); index += 3 {
		result[index] = value.B
		result[index+1] = value.G
		result[index+2] = value.R
	}
	return result, nil
}
