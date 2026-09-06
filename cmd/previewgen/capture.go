//go:build preview

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"os"
	"path/filepath"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/deviceownership"
	"github.com/lxdb/bsbctl/internal/secrets"
	"github.com/lxdb/bsbctl/sdk/protocol"
	busylib "github.com/lxdb/busylib-go"
	framepkg "github.com/lxdb/busylib-go/frame"
)

const (
	previewCaptureInterval = 250 * time.Millisecond
	previewCaptureSettle   = 500 * time.Millisecond
	previewCleanupTimeout  = 5 * time.Second
)

type previewCaptureDisplay interface {
	Clear(context.Context, string) error
	Draw(context.Context, busylib.DisplayElements) error
	Screen(context.Context, busylib.DisplayTarget) ([]byte, error)
}

type captureTiming struct {
	now  func() time.Time
	wait func(context.Context, time.Duration) error
}

type previewCaptureAsset struct {
	label      string
	devicePath string
	sourcePath string
}

var previewCaptureAssets = []previewCaptureAsset{
	{label: "Codex mark", devicePath: previewAssetPath, sourcePath: filepath.Join("plugins", "codex", previewAssetSource)},
	{label: "GitHub mark", devicePath: githubPreviewAssetPath, sourcePath: filepath.Join("plugins", "githubnotifications", githubPreviewAssetSource)},
	{label: "Slack mark", devicePath: slackPreviewAssetPath, sourcePath: filepath.Join("plugins", "slack", slackPreviewAssetSource)},
}

func captureDeviceFixtures(ctx context.Context, options options) (result []mockFixture, resultErr error) {
	configPath := options.Config
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, errors.New("resolve preview capture configuration")
		}
		configPath = filepath.Join(home, ".bsbctl", "config.json")
	}
	deviceConfig, err := config.NewStore(configPath).LoadDevice()
	if err != nil {
		return nil, fmt.Errorf("load device configuration: %w", err)
	}
	baseURL := deviceConfig.BaseURL
	if baseURL == "" {
		baseURL = busylib.DefaultLocalBaseURL
	}
	lease, err := deviceownership.Acquire(baseURL, previewApplication)
	if err != nil {
		return nil, fmt.Errorf("acquire preview device ownership: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, lease.Close()) }()

	accessToken := ""
	if deviceConfig.AccessTokenSecret != "" {
		accessToken, err = secrets.NewKeychain(nil).Resolve(ctx, deviceConfig.AccessTokenSecret)
		if err != nil {
			return nil, fmt.Errorf("resolve preview device access token: %w", err)
		}
	}
	clientOptions := []busylib.Option{busylib.WithBaseURL(baseURL)}
	if accessToken != "" {
		clientOptions = append(clientOptions, busylib.WithLocalAccessToken(accessToken))
	}
	client, err := busylib.NewClient(clientOptions...)
	accessToken = ""
	if err != nil {
		return nil, fmt.Errorf("create preview device client: %w", err)
	}
	device := busylibCaptureDisplay{client: client}
	defer func() {
		resultErr = errors.Join(resultErr, cleanupCaptureDevice(ctx, device, func(cleanupCtx context.Context) error {
			return client.Assets().DeleteApplicationAssets(cleanupCtx, previewApplication)
		}))
	}()
	if err := device.Clear(ctx, previewApplication); err != nil {
		return nil, fmt.Errorf("clear prior preview display: %w", err)
	}
	if err := client.Assets().DeleteApplicationAssets(ctx, previewApplication); err != nil {
		return nil, fmt.Errorf("clear prior preview assets: %w", err)
	}
	for _, asset := range previewCaptureAssets {
		if err := client.Assets().UploadFile(ctx, previewApplication, asset.devicePath, asset.sourcePath); err != nil {
			return nil, fmt.Errorf("upload preview %s: %w", asset.label, err)
		}
	}
	deviceNow, err := client.Time().Now(ctx)
	if err != nil {
		return nil, fmt.Errorf("read BUSY Bar time: %w", err)
	}
	now, err := time.Parse(time.RFC3339, deviceNow.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("parse BUSY Bar time: %w", err)
	}
	scenarios, err := previewScenarios(now)
	if err != nil {
		return nil, err
	}
	timing := captureTiming{now: time.Now, wait: waitForCapture}
	if err := establishCapturePrivacyGuard(ctx, device, timing); err != nil {
		return nil, err
	}
	return captureFixtureSet(ctx, device, scenarios, timing)
}

func cleanupCaptureDevice(ctx context.Context, display previewCaptureDisplay, deleteAssets func(context.Context) error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), previewCleanupTimeout)
	defer cancel()
	return errors.Join(
		display.Clear(cleanupCtx, previewApplication),
		deleteAssets(cleanupCtx),
	)
}

type busylibCaptureDisplay struct {
	client *busylib.Client
}

func (d busylibCaptureDisplay) Clear(ctx context.Context, application string) error {
	return d.client.Display().Clear(ctx, application)
}

func (d busylibCaptureDisplay) Draw(ctx context.Context, draw busylib.DisplayElements) error {
	return d.client.Display().Draw(ctx, draw)
}

func (d busylibCaptureDisplay) Screen(ctx context.Context, display busylib.DisplayTarget) ([]byte, error) {
	return d.client.Display().Screen(ctx, display)
}

func waitForCapture(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func establishCapturePrivacyGuard(ctx context.Context, display previewCaptureDisplay, timing captureTiming) error {
	guards := []color.RGBA{{R: 0x27, G: 0x91, B: 0xc4, A: 0xff}, {R: 0xc4, G: 0x5a, B: 0x27, A: 0xff}}
	for _, guard := range guards {
		scene := protocol.Scene{Elements: []protocol.Element{{
			ID: "privacy-guard", Display: protocol.DisplayFront,
			Rectangle: &protocol.RectangleElement{Width: frontWidth, Height: frontHeight, Color: rgbaHex(guard)},
		}}}
		draw, err := compilePreviewScene(scene, rgbaHex(guard))
		if err != nil {
			return err
		}
		if err := display.Clear(ctx, previewApplication); err != nil {
			return err
		}
		if err := display.Draw(ctx, draw); err != nil {
			return err
		}
		guardContext, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = awaitCaptureColor(guardContext, display, guard, timing)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func awaitCaptureColor(ctx context.Context, display previewCaptureDisplay, want color.RGBA, timing captureTiming) error {
	for {
		frame, err := readCaptureFrame(ctx, display)
		if err == nil && uniformCaptureColor(frame, want) {
			return nil
		}
		if err := timing.wait(ctx, previewCaptureInterval); err != nil {
			return errors.New("preview privacy guard was not observed")
		}
	}
}

func uniformCaptureColor(frame *image.RGBA, want color.RGBA) bool {
	if frame == nil || frame.Bounds() != image.Rect(0, 0, frontWidth, frontHeight) {
		return false
	}
	for y := range frontHeight {
		for x := range frontWidth {
			if frame.RGBAAt(x, y) != want {
				return false
			}
		}
	}
	return true
}

func rgbaHex(value color.RGBA) string {
	return fmt.Sprintf("#%02X%02X%02XFF", value.R, value.G, value.B)
}

func captureScenario(ctx context.Context, display previewCaptureDisplay, scenario previewScenario, timing captureTiming) ([]timedFrame, error) {
	if display == nil || timing.now == nil || timing.wait == nil || len(scenario.Steps) == 0 || scenario.Duration <= 0 {
		return nil, errors.New("preview capture scenario is invalid")
	}
	interval := previewCaptureInterval
	if scenario.SampleInterval > 0 {
		interval = scenario.SampleInterval
	}
	result := make([]timedFrame, 0, int(scenario.Duration/interval))
	total := time.Duration(0)
	for index, step := range scenario.Steps {
		duration := step.Duration
		if duration <= 0 || duration%(10*time.Millisecond) != 0 {
			return nil, errors.New("preview capture scene timing is invalid")
		}
		total += duration
		if err := display.Clear(ctx, previewApplication); err != nil {
			return nil, fmt.Errorf("clear %s scene %d: %w", scenario.Name, index, err)
		}
		if err := display.Draw(ctx, step.Draw); err != nil {
			return nil, fmt.Errorf("draw %s scene %d: %w", scenario.Name, index, err)
		}
		if err := timing.wait(ctx, previewCaptureSettle); err != nil {
			return nil, err
		}
		started := timing.now()
		for sampleAt := time.Duration(0); sampleAt < duration; sampleAt += interval {
			if err := timing.wait(ctx, started.Add(sampleAt).Sub(timing.now())); err != nil {
				return nil, err
			}
			frame, err := readCaptureFrame(ctx, display)
			if err != nil {
				return nil, fmt.Errorf("read %s scene %d: %w", scenario.Name, index, err)
			}
			delay := min(interval, duration-sampleAt)
			result = append(result, timedFrame{Image: frame, Delay: int(delay / (10 * time.Millisecond))})
		}
		if err := timing.wait(ctx, started.Add(duration).Sub(timing.now())); err != nil {
			return nil, err
		}
	}
	if total != scenario.Duration {
		return nil, errors.New("preview capture scenario duration is inconsistent")
	}
	return result, nil
}

func captureFixtureSet(ctx context.Context, display previewCaptureDisplay, scenarios []previewScenario, timing captureTiming) ([]mockFixture, error) {
	selected := make(map[string]previewScenario, len(capturedPreviewArtifactNames))
	for _, scenario := range scenarios {
		if !scenario.Capture {
			continue
		}
		if !isCapturedPreviewArtifact(scenario.File) {
			return nil, fmt.Errorf("preview capture scenario %q is unexpected", scenario.File)
		}
		if _, exists := selected[scenario.File]; exists {
			return nil, fmt.Errorf("preview capture scenario %q is duplicated", scenario.File)
		}
		selected[scenario.File] = scenario
	}
	if len(selected) != len(capturedPreviewArtifactNames) {
		return nil, errors.New("preview capture requires every device-rendered animation scenario")
	}
	result := make([]mockFixture, 0, len(capturedPreviewArtifactNames))
	for _, name := range capturedPreviewArtifactNames {
		scenario := selected[name]
		frames, err := captureScenario(ctx, display, scenario, timing)
		if err != nil {
			return nil, err
		}
		data, err := encodeRawGIF(frames)
		if err != nil {
			return nil, fmt.Errorf("encode %s fixture: %w", scenario.Name, err)
		}
		digest := sha256.Sum256(data)
		result = append(result, mockFixture{
			name: scenario.File, format: "gif", data: data, sha256: hex.EncodeToString(digest[:]),
		})
	}
	return result, nil
}

func readCaptureFrame(ctx context.Context, display previewCaptureDisplay) (*image.RGBA, error) {
	raw, err := display.Screen(ctx, busylib.DisplayFront)
	if err != nil {
		return nil, err
	}
	frame, err := framepkg.FromHTTP(busylib.DisplayFront, raw)
	if err != nil {
		return nil, err
	}
	return frame.RGBA()
}

func encodeRawGIF(source []timedFrame) ([]byte, error) {
	frames, err := coalesceFrames(source)
	if err != nil {
		return nil, err
	}
	animation := &gif.GIF{
		LoopCount: 0,
		Config:    image.Config{ColorModel: color.Palette(palette.Plan9), Width: frontWidth, Height: frontHeight},
	}
	var previous *image.RGBA
	for _, current := range frames {
		bounds := current.Image.Bounds()
		if previous != nil {
			bounds = changedBounds(previous, current.Image)
			if bounds.Empty() {
				animation.Delay[len(animation.Delay)-1] += current.Delay
				continue
			}
		}
		frame := image.NewPaletted(bounds, palette.Plan9)
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				frame.Set(x, y, current.Image.At(x, y))
			}
		}
		animation.Image = append(animation.Image, frame)
		animation.Delay = append(animation.Delay, current.Delay)
		animation.Disposal = append(animation.Disposal, gif.DisposalNone)
		previous = current.Image
	}
	var output bytes.Buffer
	if err := gif.EncodeAll(&output, animation); err != nil {
		return nil, fmt.Errorf("encode raw preview GIF: %w", err)
	}
	return output.Bytes(), nil
}
