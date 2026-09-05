package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/soak"
)

const (
	defaultSoakSamples        = 12
	defaultSoakInterval       = 5 * time.Second
	defaultSoakWarm           = 10 * time.Second
	defaultSoakStartupTimeout = 30 * time.Second
	soakCommandOutputLimit    = 1 << 20
	releaseSoakProfile        = "synthetic-resident-data-sources"
)

type soakOptions struct {
	Root           string
	Output         string
	Samples        int
	Interval       time.Duration
	Warm           time.Duration
	StartupTimeout time.Duration
}

type soakEvidence struct {
	Metadata  soakMetadata
	Readiness soakReadiness
	Samples   []soakSampleRecord
	Summary   *soak.Summary
}

type soakSampleRecord struct {
	soak.Sample
	Health soakRuntimeHealth `json:"health"`
}

type soakRuntimeHealth struct {
	DevicePhase         device.Phase           `json:"device_phase"`
	DeviceStateObserved bool                   `json:"device_state_observed"`
	Plugins             []soakReadyPlugin      `json:"plugins"`
	Apps                []soakReadyApp         `json:"apps"`
	Processes           []soak.ProcessIdentity `json:"processes"`
	FakeRequests        fakeRequestCounts      `json:"fake_requests"`
}

type soakMetadata struct {
	SchemaVersion        int                  `json:"schema_version"`
	Environment          soakEnvironment      `json:"environment"`
	Workload             string               `json:"workload"`
	SyntheticInputs      []string             `json:"synthetic_inputs"`
	BuiltWithRace        bool                 `json:"built_with_race"`
	BuildFlags           []string             `json:"build_flags"`
	WarmMilliseconds     int64                `json:"warm_milliseconds"`
	SampleCount          int                  `json:"sample_count"`
	IntervalMilliseconds int64                `json:"interval_milliseconds"`
	Limits               soak.Limits          `json:"limits"`
	Binaries             []soakBinary         `json:"binaries"`
	ExcludedHelpers      []soakExcludedHelper `json:"excluded_helpers"`
}

type soakEnvironment struct {
	GOOS            string `json:"goos"`
	GOARCH          string `json:"goarch"`
	GoVersion       string `json:"go_version"`
	MacOSVersion    string `json:"macos_version"`
	LogicalCPUCount int    `json:"logical_cpu_count"`
}

type soakBinary struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type soakExcludedHelper struct {
	PID    int    `json:"pid,omitempty"`
	Role   string `json:"role"`
	Reason string `json:"reason"`
}

type soakReadiness struct {
	StartupMilliseconds int64                  `json:"startup_milliseconds"`
	DevicePhase         device.Phase           `json:"device_phase"`
	DeviceStateObserved bool                   `json:"device_state_observed"`
	Plugins             []soakReadyPlugin      `json:"plugins"`
	Apps                []soakReadyApp         `json:"apps"`
	Processes           []soak.ProcessIdentity `json:"processes"`
	FakeRequests        fakeRequestCounts      `json:"fake_requests"`
}

type soakReadyPlugin struct {
	ID      string           `json:"id"`
	Phase   pluginhost.Phase `json:"phase"`
	Running bool             `json:"running"`
	Healthy bool             `json:"healthy"`
}

type soakReadyApp struct {
	AppID string                   `json:"app_id"`
	Phase daemon.AppReadinessPhase `json:"phase"`
}

type fakeRequestCounts struct {
	Version       int64 `json:"version"`
	StatusStreams int64 `json:"status_streams"`
	StateMessages int64 `json:"state_messages"`
	CodexUsage    int64 `json:"codex_usage"`
	DisplayDraw   int64 `json:"display_draw"`
	DisplayClear  int64 `json:"display_clear"`
}

type fakeDependencies struct {
	server        *httptest.Server
	storage       fakeStorage
	version       atomic.Int64
	statusStreams atomic.Int64
	stateMessages atomic.Int64
	codexUsage    atomic.Int64
	displayDraw   atomic.Int64
	displayClear  atomic.Int64
}

type soakDaemon struct {
	command *exec.Cmd
	done    chan struct{}
	waitErr error
	output  *boundedOutput
}

type soakSamplingHooks struct {
	Now      func() time.Time
	Wait     func(context.Context, time.Duration) error
	Snapshot func(context.Context, []soak.ProcessIdentity) ([]soak.ProcessSnapshot, error)
	Status   func(context.Context) (control.Status, error)
	Tree     func(context.Context) ([]soak.ProcessIdentity, error)
	Counts   func() fakeRequestCounts
}

type soakCleanupHooks struct {
	Descendants  func(context.Context, int) ([]int, error)
	Stop         func() error
	ProcessAlive func(int) (bool, error)
	SocketExists func(string) (bool, error)
	Wait         func(context.Context, time.Duration) error
}

func runSoak(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("soak")
	root := flags.String("root", ".", "repository root")
	output := flags.String("output", "", "absolute raw JSONL evidence path")
	samples := flags.Int("samples", defaultSoakSamples, "warm steady-state sample count")
	interval := flags.Duration("interval", defaultSoakInterval, "CPU measurement interval")
	warm := flags.Duration("warm", defaultSoakWarm, "warm duration after deterministic readiness")
	startupTimeout := flags.Duration("startup-timeout", defaultSoakStartupTimeout, "readiness deadline")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "releasectl: invalid soak arguments")
		return exitFailure
	}
	options := soakOptions{
		Root: *root, Output: *output, Samples: *samples, Interval: *interval,
		Warm: *warm, StartupTimeout: *startupTimeout,
	}
	if err := validateSoakOptions(&options); err != nil {
		_, _ = fmt.Fprintln(stderr, "releasectl: invalid soak arguments")
		return exitFailure
	}
	if runtime.GOOS != "darwin" {
		_, _ = fmt.Fprintln(stderr, "release soak failed: macOS process telemetry requires Darwin")
		return exitFailure
	}
	evidence, soakErr := executeSoak(ctx, options)
	if evidence.Metadata.SchemaVersion != 0 {
		if err := writeSoakEvidence(options.Output, evidence, soakErr); err != nil {
			_, _ = fmt.Fprintln(stderr, "release soak failed: write raw evidence")
			return exitFailure
		}
	}
	if soakErr != nil {
		_, _ = fmt.Fprintf(stderr, "release soak failed: %v\n", soakErr)
		return exitFailure
	}
	result := struct {
		Status  string       `json:"status"`
		Output  string       `json:"output"`
		Summary soak.Summary `json:"summary"`
	}{Status: "passed", Output: options.Output, Summary: *evidence.Summary}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		_, _ = fmt.Fprintln(stderr, "release soak failed: write result")
		return exitFailure
	}
	return exitSuccess
}

func validateSoakOptions(options *soakOptions) error {
	if options == nil || strings.TrimSpace(options.Root) == "" || !filepath.IsAbs(options.Output) || options.Samples < 2 || options.Samples > 120 {
		return errors.New("invalid soak options")
	}
	if options.Interval < time.Second || options.Interval > time.Minute || options.Warm < 2*time.Second || options.Warm > 10*time.Minute {
		return errors.New("invalid soak timing")
	}
	if options.StartupTimeout < 5*time.Second || options.StartupTimeout > 2*time.Minute {
		return errors.New("invalid soak startup timeout")
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return err
	}
	info, err := os.Stat(filepath.Join(root, "go.mod"))
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("repository root is invalid")
	}
	options.Root = root
	options.Output = filepath.Clean(options.Output)
	if _, err := os.Lstat(options.Output); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("raw evidence path already exists or is unavailable")
	}
	return nil
}
