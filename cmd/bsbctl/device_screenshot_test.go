package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	busylib "github.com/lxdb/busylib-go"
	framepkg "github.com/lxdb/busylib-go/frame"
)

type screenshotClientFunc func(context.Context, busylib.DisplayTarget) ([]byte, error)

func (function screenshotClientFunc) Screen(ctx context.Context, display busylib.DisplayTarget) ([]byte, error) {
	return function(ctx, display)
}

func TestDeviceScreenshotCapturesBothDisplaysAsReadablePNGs(t *testing.T) {
	t.Parallel()
	front := make([]byte, framepkg.FrontWidth*framepkg.FrontHeight*3)
	front[0], front[1], front[2] = 0x11, 0x22, 0x33
	back := make([]byte, framepkg.BackWidth*framepkg.BackHeight/2)
	back[0] = 0xa3
	var screens []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/version":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"api_semver":"25.0.0"}`))
		case "/api/screen":
			display := request.URL.Query().Get("display")
			screens = append(screens, display)
			payload := front
			if display == "1" {
				payload = back
			}
			_, _ = writer.Write([]byte(base64.StdEncoding.EncodeToString(payload)))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	if _, err := config.NewStore(configPath).ReplaceWithOutcome(0, config.Document{
		Version: config.CurrentVersion, Generation: 1, Device: config.Device{BaseURL: server.URL},
		Plugins: map[string]config.Plugin{}, Apps: map[string]config.App{},
	}); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "capture")
	var stdout, stderr bytes.Buffer
	code := execute(t.Context(), []string{"device", "screenshot", "--config", configPath, "--out", output}, strings.NewReader(""), &stdout, &stderr)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("screenshot = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	if !reflect.DeepEqual(screens, []string{"0", "1"}) {
		t.Fatalf("screen requests = %v, want [0 1]", screens)
	}

	manifest, err := os.ReadFile(filepath.Join(output, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifest, stdout.Bytes()) {
		t.Fatalf("manifest and stdout differ:\nmanifest=%s\nstdout=%s", manifest, stdout.Bytes())
	}
	var result struct {
		SchemaVersion int    `json:"schema_version"`
		Status        string `json:"status"`
		OutputDir     string `json:"output_dir"`
		Display       string `json:"display"`
		Count         int    `json:"count"`
		IntervalMS    int64  `json:"interval_ms"`
		Scale         int    `json:"scale"`
		Captures      []struct {
			Index        int    `json:"index"`
			Display      string `json:"display"`
			File         string `json:"file"`
			SHA256       string `json:"sha256"`
			NativeWidth  int    `json:"native_width"`
			NativeHeight int    `json:"native_height"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
		} `json:"captures"`
	}
	if err := json.Unmarshal(manifest, &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != 1 || result.Status != "complete" || result.OutputDir != output || result.Display != "both" || result.Count != 1 || result.IntervalMS != 500 || result.Scale != 4 || len(result.Captures) != 2 {
		t.Fatalf("manifest = %#v", result)
	}
	wantCaptures := []struct {
		file                      string
		display                   string
		nativeWidth, nativeHeight int
		width, height             int
	}{
		{file: "front-000.png", display: "front", nativeWidth: 72, nativeHeight: 16, width: 288, height: 64},
		{file: "back-000.png", display: "back", nativeWidth: 160, nativeHeight: 80, width: 640, height: 320},
	}
	for index, want := range wantCaptures {
		capture := result.Captures[index]
		if capture.Index != 0 || capture.Display != want.display || capture.File != want.file || capture.NativeWidth != want.nativeWidth || capture.NativeHeight != want.nativeHeight || capture.Width != want.width || capture.Height != want.height {
			t.Fatalf("capture %d = %#v", index, capture)
		}
		data, err := os.ReadFile(filepath.Join(output, capture.File))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); capture.SHA256 != got {
			t.Fatalf("%s hash = %q, want %q", capture.File, capture.SHA256, got)
		}
		info, err := os.Stat(filepath.Join(output, capture.File))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", capture.File, got)
		}
	}
	assertScreenshotPNG(t, filepath.Join(output, "front-000.png"), 288, 64, color.RGBA{R: 0x33, G: 0x22, B: 0x11, A: 0xff})
	assertScreenshotPNG(t, filepath.Join(output, "back-000.png"), 640, 320, color.Gray{Y: 0x33})
	assertScreenshotPixel(t, filepath.Join(output, "back-000.png"), 4, 0, color.Gray{Y: 0xaa})
}

func TestDeviceScreenshotLoadsDeviceConfigIndependentlyFromRuntimeRecords(t *testing.T) {
	t.Parallel()
	front := make([]byte, framepkg.FrontWidth*framepkg.FrontHeight*3)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/version":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"api_semver":"25.0.0"}`))
		case "/api/screen":
			_, _ = writer.Write([]byte(base64.StdEncoding.EncodeToString(front)))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	configWithInvalidPlugin := fmt.Sprintf(`{
  "version": 1,
  "generation": 24,
  "device": {"base_url": %q},
  "plugins": {
    "dev.bsbctl.legacy": {
      "id": "dev.bsbctl.legacy",
      "version": "0.1.0",
      "executable": "/missing/legacy-plugin"
    }
  },
  "apps": {}
}`, server.URL)
	if err := os.WriteFile(configPath, []byte(configWithInvalidPlugin), 0o600); err != nil {
		t.Fatal(err)
	}

	output := filepath.Join(root, "capture")
	var stdout, stderr bytes.Buffer
	code := execute(t.Context(), []string{"device", "screenshot", "--display", "front", "--config", configPath, "--out", output}, strings.NewReader(""), &stdout, &stderr)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("screenshot = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	assertScreenshotPNG(t, filepath.Join(output, "front-000.png"), 288, 64, color.RGBA{A: 0xff})
}

func assertScreenshotPNG(t *testing.T, path string, width, height int, want color.Color) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	image, err := png.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	if image.Bounds().Dx() != width || image.Bounds().Dy() != height {
		t.Fatalf("%s dimensions = %dx%d, want %dx%d", filepath.Base(path), image.Bounds().Dx(), image.Bounds().Dy(), width, height)
	}
	if got := color.RGBAModel.Convert(image.At(0, 0)); got != color.RGBAModel.Convert(want) {
		t.Fatalf("%s first pixel = %#v, want %#v", filepath.Base(path), got, color.RGBAModel.Convert(want))
	}
}

func assertScreenshotPixel(t *testing.T, path string, x, y int, want color.Color) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	image, err := png.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := color.RGBAModel.Convert(image.At(x, y)); got != color.RGBAModel.Convert(want) {
		t.Fatalf("%s pixel (%d,%d) = %#v, want %#v", filepath.Base(path), x, y, got, color.RGBAModel.Convert(want))
	}
}

func TestDeviceScreenshotPreservesPartialCapture(t *testing.T) {
	t.Parallel()
	front := make([]byte, framepkg.FrontWidth*framepkg.FrontHeight*3)
	front[0], front[1], front[2] = 0x11, 0x22, 0x33
	output := filepath.Join(t.TempDir(), "capture")
	client := screenshotClientFunc(func(_ context.Context, display busylib.DisplayTarget) ([]byte, error) {
		if display == busylib.DisplayFront {
			return front, nil
		}
		return nil, errors.New("device disconnected")
	})
	dependencies := productionScreenshotDependencies()
	dependencies.loadDevice = func(string) (config.Device, error) {
		return config.Device{}, nil
	}
	dependencies.newClient = func(string, string) (screenshotClient, error) { return client, nil }

	var stdout bytes.Buffer
	err := captureScreenshots(t.Context(), screenshotOptions{
		display: "both", count: 1, intervalMS: defaultScreenshotIntervalMS,
		output: output, configPath: "unused",
	}, &stdout, dependencies)
	commandErr, ok := errors.AsType[*commandError](err)
	if !ok || commandErr.code != exitPartial {
		t.Fatalf("capture error = %v, want exit %d", err, exitPartial)
	}
	manifest, readErr := os.ReadFile(filepath.Join(output, "manifest.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(manifest, stdout.Bytes()) {
		t.Fatalf("manifest and stdout differ:\nmanifest=%s\nstdout=%s", manifest, stdout.Bytes())
	}
	var result screenshotManifest
	if err := json.Unmarshal(manifest, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "partial" || len(result.Captures) != 1 || result.Captures[0].Display != "front" {
		t.Fatalf("partial manifest = %#v", result)
	}
	if result.Failure == nil || result.Failure.Index != 0 || result.Failure.Display != "back" || result.Failure.ErrorCode != "device_capture_failed" {
		t.Fatalf("partial failure = %#v", result.Failure)
	}
	assertScreenshotPNG(t, filepath.Join(output, "front-000.png"), 288, 64, color.RGBA{R: 0x33, G: 0x22, B: 0x11, A: 0xff})
}

func TestDeviceScreenshotRejectsNonemptyOutputBeforeResolvingSecret(t *testing.T) {
	t.Parallel()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(output, "keep.txt"), []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved := false
	dependencies := productionScreenshotDependencies()
	dependencies.loadDevice = func(string) (config.Device, error) {
		return config.Device{AccessTokenSecret: "keychain://bsbctl/device/access-token"}, nil
	}
	dependencies.resolveSecret = func(context.Context, string) (string, error) {
		resolved = true
		return "secret", nil
	}
	dependencies.newClient = func(string, string) (screenshotClient, error) {
		return screenshotClientFunc(func(context.Context, busylib.DisplayTarget) ([]byte, error) { return nil, nil }), nil
	}

	err := captureScreenshots(t.Context(), screenshotOptions{
		display: "front", count: 1, intervalMS: defaultScreenshotIntervalMS,
		output: output, configPath: "unused",
	}, &bytes.Buffer{}, dependencies)
	commandErr, ok := errors.AsType[*commandError](err)
	if !ok || commandErr.code != exitUsage {
		t.Fatalf("capture error = %v, want exit %d", err, exitUsage)
	}
	if resolved {
		t.Fatal("access token was resolved for an invalid output directory")
	}
}

func TestDeviceScreenshotRemovesCreatedDirectoryWhenFirstFrameIsInvalid(t *testing.T) {
	t.Parallel()
	output := filepath.Join(t.TempDir(), "capture")
	dependencies := productionScreenshotDependencies()
	dependencies.loadDevice = func(string) (config.Device, error) {
		return config.Device{}, nil
	}
	dependencies.newClient = func(string, string) (screenshotClient, error) {
		return screenshotClientFunc(func(context.Context, busylib.DisplayTarget) ([]byte, error) {
			return []byte("not a framebuffer"), nil
		}), nil
	}

	err := captureScreenshots(t.Context(), screenshotOptions{
		display: "front", count: 1, intervalMS: defaultScreenshotIntervalMS,
		output: output, configPath: "unused",
	}, &bytes.Buffer{}, dependencies)
	commandErr, ok := errors.AsType[*commandError](err)
	if !ok || commandErr.code != exitOperational {
		t.Fatalf("capture error = %v, want exit %d", err, exitOperational)
	}
	if _, statErr := os.Stat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("created output directory remains after empty failure: %v", statErr)
	}
}

func TestDeviceScreenshotReturnsCancellationBeforeFirstCapture(t *testing.T) {
	t.Parallel()
	output := filepath.Join(t.TempDir(), "capture")
	dependencies := productionScreenshotDependencies()
	dependencies.loadDevice = func(string) (config.Device, error) {
		return config.Device{}, nil
	}
	dependencies.newClient = func(string, string) (screenshotClient, error) {
		return screenshotClientFunc(func(context.Context, busylib.DisplayTarget) ([]byte, error) {
			return nil, context.Canceled
		}), nil
	}

	err := captureScreenshots(t.Context(), screenshotOptions{
		display: "front", count: 1, intervalMS: defaultScreenshotIntervalMS,
		output: output, configPath: "unused",
	}, &bytes.Buffer{}, dependencies)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("capture error = %v, want context cancellation", err)
	}
	if _, statErr := os.Stat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("created output directory remains after cancellation: %v", statErr)
	}
}

func TestDeviceScreenshotUsesDeadlineScheduling(t *testing.T) {
	t.Parallel()
	output := filepath.Join(t.TempDir(), "capture")
	current := time.Unix(100, 0)
	var waits []time.Duration
	front := make([]byte, framepkg.FrontWidth*framepkg.FrontHeight*3)
	dependencies := productionScreenshotDependencies()
	dependencies.loadDevice = func(string) (config.Device, error) {
		return config.Device{}, nil
	}
	dependencies.newClient = func(string, string) (screenshotClient, error) {
		return screenshotClientFunc(func(context.Context, busylib.DisplayTarget) ([]byte, error) {
			current = current.Add(100 * time.Millisecond)
			return front, nil
		}), nil
	}
	dependencies.now = func() time.Time { return current }
	dependencies.wait = func(_ context.Context, duration time.Duration) error {
		waits = append(waits, duration)
		current = current.Add(max(0, duration))
		return nil
	}

	var stdout bytes.Buffer
	err := captureScreenshots(t.Context(), screenshotOptions{
		display: "front", count: 3, intervalMS: 500,
		output: output, configPath: "unused",
	}, &stdout, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(waits, []time.Duration{400 * time.Millisecond, 400 * time.Millisecond}) {
		t.Fatalf("waits = %v, want deadline-adjusted waits", waits)
	}
	var manifest screenshotManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Captures) != 3 {
		t.Fatalf("captures = %d, want 3", len(manifest.Captures))
	}
	for index, capture := range manifest.Captures {
		if capture.Index != index || capture.ScheduledMS != int64(index*500) || capture.RequestStartedMS != int64(index*500) || capture.RequestDurationMS != 100 {
			t.Fatalf("capture %d timing = %#v", index, capture)
		}
	}
}

func TestDeviceScreenshotClientUsesConfiguredAccessToken(t *testing.T) {
	t.Parallel()
	front := make([]byte, framepkg.FrontWidth*framepkg.FrontHeight*3)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-API-Token") != "device-secret" {
			http.Error(writer, "missing access token", http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/api/version":
			_, _ = writer.Write([]byte(`{"api_semver":"25.0.0"}`))
		case "/api/screen":
			_, _ = writer.Write([]byte(base64.StdEncoding.EncodeToString(front)))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	client, err := productionScreenshotDependencies().newClient(server.URL, "device-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Screen(t.Context(), busylib.DisplayFront); err != nil {
		t.Fatal(err)
	}
}

func TestParseDeviceScreenshotOptionsRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "display", args: []string{"--display", "side"}},
		{name: "zero count", args: []string{"--count", "0"}},
		{name: "negative interval", args: []string{"--interval-ms", "-1"}},
		{name: "sequence interval below device limit", args: []string{"--count", "2", "--interval-ms", "499"}},
		{name: "positional", args: []string{"extra"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseScreenshotOptions(test.args)
			commandErr, ok := errors.AsType[*commandError](err)
			if !ok || commandErr.code != exitUsage {
				t.Fatalf("parse error = %v, want exit %d", err, exitUsage)
			}
		})
	}
}

func TestParseDeviceScreenshotOptionsEnforcesCaptureLimit(t *testing.T) {
	t.Parallel()

	options, err := parseScreenshotOptions([]string{"--count", "1000"})
	if err != nil || options.count != 1000 {
		t.Fatalf("parse maximum count = %#v, %v", options, err)
	}

	_, err = parseScreenshotOptions([]string{"--count", "1001"})
	commandErr, ok := errors.AsType[*commandError](err)
	if !ok || commandErr.code != exitUsage {
		t.Fatalf("parse excessive count error = %v, want exit %d", err, exitUsage)
	}
}
