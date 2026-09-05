package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/secrets"
	busylib "github.com/lxdb/busylib-go"
	framepkg "github.com/lxdb/busylib-go/frame"
)

const (
	minimumScreenshotIntervalMS = int64(500)
	defaultScreenshotIntervalMS = minimumScreenshotIntervalMS
	maximumScreenshotCount      = 1000
	screenshotScale             = 4
)

var errInvalidScreenshotOutput = errors.New("invalid screenshot output directory")

type screenshotClient interface {
	Screen(context.Context, busylib.DisplayTarget) ([]byte, error)
}

type busylibScreenshotClient struct{ client *busylib.Client }

func (c busylibScreenshotClient) Screen(ctx context.Context, display busylib.DisplayTarget) ([]byte, error) {
	return c.client.Display().Screen(ctx, display)
}

type screenshotDependencies struct {
	loadDevice    func(string) (config.Device, error)
	resolveSecret func(context.Context, string) (string, error)
	newClient     func(string, string) (screenshotClient, error)
	now           func() time.Time
	wait          func(context.Context, time.Duration) error
}

type screenshotOptions struct {
	display    string
	count      int
	intervalMS int64
	output     string
	configPath string
}

type screenshotCapture struct {
	Index             int    `json:"index"`
	Display           string `json:"display"`
	File              string `json:"file"`
	SHA256            string `json:"sha256"`
	NativeWidth       int    `json:"native_width"`
	NativeHeight      int    `json:"native_height"`
	Width             int    `json:"width"`
	Height            int    `json:"height"`
	ScheduledMS       int64  `json:"scheduled_ms"`
	RequestStartedMS  int64  `json:"request_started_ms"`
	CapturedMS        int64  `json:"captured_ms"`
	RequestDurationMS int64  `json:"request_duration_ms"`
}

type screenshotFailure struct {
	Index     int    `json:"index"`
	Display   string `json:"display"`
	ErrorCode string `json:"error_code"`
}

type screenshotManifest struct {
	SchemaVersion int                 `json:"schema_version"`
	Status        string              `json:"status"`
	OutputDir     string              `json:"output_dir"`
	Display       string              `json:"display"`
	Count         int                 `json:"count"`
	IntervalMS    int64               `json:"interval_ms"`
	Scale         int                 `json:"scale"`
	Captures      []screenshotCapture `json:"captures"`
	Failure       *screenshotFailure  `json:"failure,omitempty"`
}

func runDevice(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "screenshot" {
		return commandFailure(exitUsage, "device command requires screenshot")
	}
	options, err := parseScreenshotOptions(args[1:])
	if err != nil {
		return err
	}
	return captureScreenshots(ctx, options, stdout, productionScreenshotDependencies())
}

func parseScreenshotOptions(args []string) (screenshotOptions, error) {
	values, positionals, err := parseOptions(args, "display", "count", "interval-ms", "out", "config")
	if err != nil || len(positionals) != 0 {
		return screenshotOptions{}, commandFailure(exitUsage, "invalid device screenshot flags")
	}
	result := screenshotOptions{
		display:    optionDefault(values, "display", "both"),
		count:      1,
		intervalMS: defaultScreenshotIntervalMS,
		output:     values["out"],
	}
	if result.display != "front" && result.display != "back" && result.display != "both" {
		return screenshotOptions{}, commandFailure(exitUsage, "screenshot display must be front, back, or both")
	}
	if value := values["count"]; value != "" {
		result.count, err = strconv.Atoi(value)
		if err != nil || result.count < 1 {
			return screenshotOptions{}, commandFailure(exitUsage, "screenshot count must be positive")
		}
	}
	if result.count > maximumScreenshotCount {
		return screenshotOptions{}, commandFailure(exitUsage, "screenshot count must not exceed 1000")
	}
	if value := values["interval-ms"]; value != "" {
		result.intervalMS, err = strconv.ParseInt(value, 10, 64)
		if err != nil || result.intervalMS < 0 {
			return screenshotOptions{}, commandFailure(exitUsage, "screenshot interval must be nonnegative milliseconds")
		}
	}
	if result.count > 1 && result.intervalMS < minimumScreenshotIntervalMS {
		return screenshotOptions{}, commandFailure(exitUsage, "screenshot sequences require an interval of at least 500 milliseconds")
	}
	maximumMilliseconds := int64(math.MaxInt64 / int64(time.Millisecond))
	if result.count > 1 && result.intervalMS > maximumMilliseconds/int64(result.count-1) {
		return screenshotOptions{}, commandFailure(exitUsage, "screenshot schedule is too long")
	}
	result.configPath, err = resolveStatePath(values, "config", "config.json")
	if err != nil {
		return screenshotOptions{}, err
	}
	return result, nil
}

func productionScreenshotDependencies() screenshotDependencies {
	return screenshotDependencies{
		loadDevice: func(path string) (config.Device, error) { return config.NewStore(path).LoadDevice() },
		resolveSecret: func(ctx context.Context, reference string) (string, error) {
			return secrets.NewKeychain(nil).Resolve(ctx, reference)
		},
		newClient: func(baseURL, accessToken string) (screenshotClient, error) {
			options := []busylib.Option{busylib.WithBaseURL(baseURL)}
			if accessToken != "" {
				options = append(options, busylib.WithLocalAccessToken(accessToken))
			}
			client, err := busylib.NewClient(options...)
			if err != nil {
				return nil, err
			}
			return busylibScreenshotClient{client: client}, nil
		},
		now: time.Now,
		wait: func(ctx context.Context, delay time.Duration) error {
			if delay <= 0 {
				return ctx.Err()
			}
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

func captureScreenshots(ctx context.Context, options screenshotOptions, stdout io.Writer, dependencies screenshotDependencies) error {
	deviceConfig, err := dependencies.loadDevice(options.configPath)
	if err != nil {
		if _, ok := errors.AsType[*os.PathError](err); ok {
			return commandFailure(exitOperational, "load screenshot configuration failed")
		}
		return commandFailure(exitUsage, "screenshot configuration is invalid")
	}
	if _, err := inspectScreenshotDirectory(options.output); err != nil {
		if errors.Is(err, errInvalidScreenshotOutput) {
			return commandFailure(exitUsage, "screenshot output directory is invalid")
		}
		return commandFailure(exitOperational, "inspect screenshot output directory failed")
	}
	baseURL := deviceConfig.BaseURL
	if baseURL == "" {
		baseURL = busylib.DefaultLocalBaseURL
	}
	accessToken := ""
	if deviceConfig.AccessTokenSecret != "" {
		accessToken, err = dependencies.resolveSecret(ctx, deviceConfig.AccessTokenSecret)
		if err != nil {
			if isCancellation(err) {
				return err
			}
			return commandFailure(exitOperational, "resolve device access token failed")
		}
	}
	client, err := dependencies.newClient(baseURL, accessToken)
	accessToken = ""
	if err != nil {
		return commandFailure(exitOperational, "create screenshot client failed")
	}
	outputDir, created, err := prepareScreenshotDirectory(options.output)
	if err != nil {
		if errors.Is(err, errInvalidScreenshotOutput) {
			return commandFailure(exitUsage, "screenshot output directory is invalid")
		}
		return commandFailure(exitOperational, "create screenshot output directory failed")
	}

	manifest := screenshotManifest{
		SchemaVersion: 1, Status: "complete", OutputDir: outputDir,
		Display: options.display, Count: options.count, IntervalMS: options.intervalMS, Scale: screenshotScale,
		Captures: make([]screenshotCapture, 0),
	}
	displays := screenshotDisplays(options.display)
	started := dependencies.now()
	for index := range options.count {
		scheduled := time.Duration(int64(index)*options.intervalMS) * time.Millisecond
		if index > 0 {
			if err := dependencies.wait(ctx, started.Add(scheduled).Sub(dependencies.now())); err != nil {
				return finishScreenshotFailure(stdout, &manifest, created, index, displays[0], "capture_canceled", err)
			}
		}
		for _, display := range displays {
			requestStarted := dependencies.now()
			raw, err := client.Screen(ctx, display)
			captured := dependencies.now()
			if err != nil {
				cause := error(commandFailure(exitOperational, "device screenshot failed"))
				errorCode := "device_capture_failed"
				if isCancellation(err) {
					cause = err
					errorCode = "capture_canceled"
				}
				return finishScreenshotFailure(stdout, &manifest, created, index, display, errorCode, cause)
			}
			capture, data, err := renderScreenshot(index, display, raw, scheduled, started, requestStarted, captured)
			if err != nil {
				return finishScreenshotFailure(stdout, &manifest, created, index, display, "invalid_device_frame", commandFailure(exitOperational, "device screenshot frame is invalid"))
			}
			if err := writeExclusiveFile(filepath.Join(outputDir, capture.File), data); err != nil {
				return finishScreenshotFailure(stdout, &manifest, created, index, display, "screenshot_write_failed", commandFailure(exitOperational, "write screenshot failed"))
			}
			manifest.Captures = append(manifest.Captures, capture)
		}
	}
	if err := writeScreenshotManifestFile(manifest); err != nil {
		return commandFailure(exitPartial, "write screenshot manifest failed after capture")
	}
	if err := writeScreenshotManifestOutput(stdout, manifest); err != nil {
		return commandFailure(exitOperational, "write output failed")
	}
	return nil
}

func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

type screenshotDirectoryInspection struct {
	path   string
	exists bool
}

func inspectScreenshotDirectory(requested string) (screenshotDirectoryInspection, error) {
	if requested == "" {
		return screenshotDirectoryInspection{}, nil
	}
	path, err := filepath.Abs(requested)
	if err != nil {
		return screenshotDirectoryInspection{}, errInvalidScreenshotOutput
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return screenshotDirectoryInspection{path: path}, nil
	}
	if err != nil {
		return screenshotDirectoryInspection{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return screenshotDirectoryInspection{}, errInvalidScreenshotOutput
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return screenshotDirectoryInspection{}, err
	}
	if len(entries) != 0 {
		return screenshotDirectoryInspection{}, errInvalidScreenshotOutput
	}
	return screenshotDirectoryInspection{path: path, exists: true}, nil
}

func prepareScreenshotDirectory(requested string) (string, bool, error) {
	if requested == "" {
		path, err := os.MkdirTemp("/tmp", "bsbctl-screenshot-")
		return path, err == nil, err
	}
	directory, err := inspectScreenshotDirectory(requested)
	if err != nil {
		return "", false, err
	}
	if !directory.exists {
		if err := os.Mkdir(directory.path, 0o700); err != nil {
			return "", false, err
		}
		return directory.path, true, nil
	}
	return directory.path, false, nil
}

func finishScreenshotFailure(stdout io.Writer, manifest *screenshotManifest, created bool, index int, display busylib.DisplayTarget, errorCode string, cause error) error {
	if len(manifest.Captures) == 0 {
		removeEmptyScreenshotDirectory(manifest.OutputDir, created)
		return cause
	}
	manifest.Status = "partial"
	manifest.Failure = &screenshotFailure{Index: index, Display: string(display), ErrorCode: errorCode}
	if err := writeScreenshotManifestFile(*manifest); err != nil {
		return commandFailure(exitPartial, "screenshot capture partially completed; write manifest failed")
	}
	if err := writeScreenshotManifestOutput(stdout, *manifest); err != nil {
		return commandFailure(exitPartial, "screenshot capture partially completed; write output failed")
	}
	return commandFailure(exitPartial, "screenshot capture partially completed")
}

func removeEmptyScreenshotDirectory(path string, created bool) {
	if !created {
		return
	}
	entries, err := os.ReadDir(path)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(path)
	}
}

func screenshotDisplays(value string) []busylib.DisplayTarget {
	switch value {
	case "front":
		return []busylib.DisplayTarget{busylib.DisplayFront}
	case "back":
		return []busylib.DisplayTarget{busylib.DisplayBack}
	default:
		return []busylib.DisplayTarget{busylib.DisplayFront, busylib.DisplayBack}
	}
}

func renderScreenshot(index int, display busylib.DisplayTarget, raw []byte, scheduled time.Duration, started, requestStarted, captured time.Time) (screenshotCapture, []byte, error) {
	frame, err := framepkg.FromHTTP(display, raw)
	if err != nil {
		return screenshotCapture{}, nil, err
	}
	decoded, err := frame.RGBA()
	if err != nil {
		return screenshotCapture{}, nil, err
	}
	scaled := nearestNeighborScale(decoded, screenshotScale)
	var output bytes.Buffer
	if err := png.Encode(&output, scaled); err != nil {
		return screenshotCapture{}, nil, err
	}
	data := bytes.Clone(output.Bytes())
	digest := sha256.Sum256(data)
	name := fmt.Sprintf("%s-%03d.png", display, index)
	return screenshotCapture{
		Index: index, Display: string(display), File: name, SHA256: fmt.Sprintf("%x", digest),
		NativeWidth: decoded.Bounds().Dx(), NativeHeight: decoded.Bounds().Dy(),
		Width: scaled.Bounds().Dx(), Height: scaled.Bounds().Dy(), ScheduledMS: scheduled.Milliseconds(),
		RequestStartedMS:  max(0, requestStarted.Sub(started).Milliseconds()),
		CapturedMS:        max(0, captured.Sub(started).Milliseconds()),
		RequestDurationMS: max(0, captured.Sub(requestStarted).Milliseconds()),
	}, data, nil
}

func nearestNeighborScale(source image.Image, scale int) *image.RGBA {
	bounds := source.Bounds()
	width, height := bounds.Dx()*scale, bounds.Dy()*scale
	result := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			result.Set(x, y, source.At(bounds.Min.X+x/scale, bounds.Min.Y+y/scale))
		}
	}
	return result
}

func screenshotManifestJSON(manifest screenshotManifest) ([]byte, error) {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func writeScreenshotManifestFile(manifest screenshotManifest) error {
	data, err := screenshotManifestJSON(manifest)
	if err != nil {
		return err
	}
	return writeExclusiveFile(filepath.Join(manifest.OutputDir, "manifest.json"), data)
}

func writeScreenshotManifestOutput(stdout io.Writer, manifest screenshotManifest) error {
	data, err := screenshotManifestJSON(manifest)
	if err != nil {
		return err
	}
	_, err = stdout.Write(data)
	return err
}

func writeExclusiveFile(path string, data []byte) (result error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); result == nil {
			result = err
		}
		if result != nil {
			_ = os.Remove(path)
		}
	}()
	_, result = file.Write(data)
	return result
}
