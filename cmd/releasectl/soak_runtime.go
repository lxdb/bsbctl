package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/soak"
)

func executeSoak(ctx context.Context, options soakOptions) (result soakEvidence, resultErr error) {
	temporary, err := os.MkdirTemp("", "bsbctl-release-soak-")
	if err != nil {
		return soakEvidence{}, errors.New("create soak workspace")
	}
	defer os.RemoveAll(temporary)

	paths := map[string]string{soak.DaemonName: filepath.Join(temporary, soak.DaemonName)}
	packages := map[string]string{soak.DaemonName: "./cmd/bsbctl"}
	names := []string{soak.DaemonName}
	for _, descriptor := range releaseSoakPluginDescriptors() {
		paths[descriptor.Binary] = filepath.Join(temporary, descriptor.Binary, descriptor.Binary)
		packages[descriptor.Binary] = descriptor.CommandPackage
		names = append(names, descriptor.Binary)
	}
	for _, name := range names {
		if err := buildSoakBinary(ctx, options.Root, paths[name], packages[name]); err != nil {
			return soakEvidence{}, fmt.Errorf("build non-race %s: %w", name, err)
		}
	}
	for _, descriptor := range releaseSoakPluginDescriptors() {
		if err := stageLocalPluginMetadata(options.Root, filepath.Dir(paths[descriptor.Binary]), descriptor); err != nil {
			return soakEvidence{}, fmt.Errorf("stage local %s package metadata: %w", descriptor.ID, err)
		}
	}

	macOSVersion, err := commandText(ctx, options.Root, os.Environ(), "sw_vers", "-productVersion")
	if err != nil || strings.TrimSpace(macOSVersion) == "" {
		return soakEvidence{}, errors.New("macOS version telemetry is unavailable")
	}
	binaries := make([]soakBinary, 0, len(names))
	for _, name := range names {
		digest, err := fileSHA256(paths[name])
		if err != nil {
			return soakEvidence{}, fmt.Errorf("hash %s", name)
		}
		binaries = append(binaries, soakBinary{Name: name, SHA256: digest})
	}

	fake := newFakeDependencies()
	defer fake.Close()
	codexHome := filepath.Join(temporary, "synthetic-codex-home")
	home := filepath.Join(temporary, "synthetic-user-home")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return soakEvidence{}, errors.New("create synthetic Codex home")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return soakEvidence{}, errors.New("create synthetic user home")
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), syntheticCodexAuthFixture(), 0o600); err != nil {
		return soakEvidence{}, errors.New("write synthetic Codex credentials")
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("chatgpt_base_url = "+strconv.Quote(fake.URL())+"\n"), 0o600); err != nil {
		return soakEvidence{}, errors.New("write synthetic Codex configuration")
	}
	environment := soakRuntimeEnvironment(home, codexHome)
	configPath := filepath.Join(temporary, "config.json")
	socketPath := filepath.Join(temporary, "bsbctl.sock")
	initArguments := []string{"init", "--config", configPath}
	for _, descriptor := range releaseSoakPluginDescriptors() {
		initArguments = append(initArguments, "--plugin", paths[descriptor.Binary])
	}
	initArguments = append(initArguments, "--device-url", fake.URL())
	if _, err := commandText(ctx, options.Root, environment, paths[soak.DaemonName], initArguments...); err != nil {
		return soakEvidence{}, fmt.Errorf("initialize production-shaped config: %w", err)
	}

	startedAt := time.Now()
	daemonProcess, err := startSoakDaemon(ctx, options.Root, environment, paths[soak.DaemonName], configPath, socketPath)
	if err != nil {
		return soakEvidence{}, err
	}
	var identities []soak.ProcessIdentity
	defer func() {
		cleanupErr := cleanupSoakDaemon(daemonProcess, identities, socketPath)
		if cleanupErr != nil {
			resultErr = errors.Join(resultErr, cleanupErr)
		}
	}()
	controlCtx, cancelControl := context.WithTimeout(ctx, options.StartupTimeout)
	if err := awaitSoakControl(controlCtx, options.Root, environment, paths[soak.DaemonName], socketPath, daemonProcess); err != nil {
		cancelControl()
		return soakEvidence{}, err
	}
	cancelControl()
	macAppPath := filepath.Join(temporary, "mac-resources-app.json")
	quotaAppPath := filepath.Join(temporary, "codex-quota-app.json")
	githubAppPath := filepath.Join(temporary, "github-notifications-app.json")
	if err := os.WriteFile(macAppPath, []byte(`{"config":{},"policies":{"summary":{"policy":"rotation","rotation_interval_ms":60000},"pressure":{"policy":"when_relevant"}}}`), 0o600); err != nil {
		return soakEvidence{}, errors.New("write synthetic Mac resources app configuration")
	}
	if err := os.WriteFile(quotaAppPath, []byte(`{"config":{"credentials_home":"`+codexHome+`","configuration_home":"`+codexHome+`","label":"MAIN","badge":"M"},"policies":{"summary":{"policy":"rotation","rotation_interval_ms":300000},"pressure":{"policy":"when_relevant"}}}`), 0o600); err != nil {
		return soakEvidence{}, errors.New("write synthetic Codex quota app configuration")
	}
	if err := os.WriteFile(githubAppPath, []byte(`{"config":{},"launch_action":"open","policies":{"summary":{"policy":"rotation","activation_action":"open","rotation_interval_ms":60000,"rotation_jitter_percent":10},"attention":{"policy":"attention","activation_action":"open","activation_input":"start","requires_ack":true},"connection":{"policy":"when_relevant","activation_action":"open"},"live":{"policy":"interactive"}}}`), 0o600); err != nil {
		return soakEvidence{}, errors.New("write synthetic unconfigured GitHub Notifications app configuration")
	}
	for _, app := range []struct {
		id, pluginID, path string
	}{
		{"mac-resources", "dev.bsbctl.mac-resources", macAppPath},
		{"codex-quota", "dev.bsbctl.codex-quota", quotaAppPath},
		{"github-notifications", "dev.bsbctl.github-notifications", githubAppPath},
	} {
		if _, err := commandText(ctx, options.Root, environment, paths[soak.DaemonName],
			"app", "create", app.id, "--plugin", app.pluginID, "--file", app.path, "--socket", socketPath,
		); err != nil {
			return soakEvidence{}, fmt.Errorf("create synthetic %s app: %w", app.id, err)
		}
	}
	if _, err := commandText(ctx, options.Root, environment, paths[soak.DaemonName],
		"app", "create", "slack", "--socket", socketPath,
	); err != nil {
		return soakEvidence{}, fmt.Errorf("create unconfigured Slack app: %w", err)
	}
	readyCtx, cancelReady := context.WithTimeout(ctx, options.StartupTimeout)
	status, readyIdentities, err := awaitSoakReadiness(readyCtx, options.Root, environment, paths[soak.DaemonName], socketPath, daemonProcess, fake)
	cancelReady()
	if err != nil {
		return soakEvidence{}, err
	}
	identities = readyIdentities
	readyAt := time.Now()
	readinessCounts := fake.Counts()

	evidence := soakEvidence{
		Metadata: soakMetadata{
			SchemaVersion: 1,
			Environment: soakEnvironment{
				GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version(),
				MacOSVersion: strings.TrimSpace(macOSVersion), LogicalCPUCount: runtime.NumCPU(),
			},
			Workload: "idle bsbctl daemon with native Mac resource sampling, one bounded synthetic Codex quota fetch, unconfigured GitHub Notifications and Slack process lifecycles, and a loopback fake BUSY Bar status/display endpoint",
			SyntheticInputs: []string{
				"temporary synthetic Codex auth.json and config.toml",
				"loopback bounded Codex usage JSON",
				"exact-empty GitHub Notifications configuration without credentials or provider requests",
				"exact-empty Slack configuration without credentials or provider requests",
				"loopback fake BUSY Bar version, protobuf status stream, and display success responses",
			},
			BuiltWithRace: false, BuildFlags: []string{"-trimpath"},
			WarmMilliseconds: options.Warm.Milliseconds(), SampleCount: options.Samples,
			IntervalMilliseconds: options.Interval.Milliseconds(),
			Limits:               soak.Limits{CPUPercent: soak.DefaultCPUPercent, RSSBytes: soak.DefaultRSSBytes},
			Binaries:             binaries,
			ExcludedHelpers: []soakExcludedHelper{
				{PID: os.Getpid(), Role: "soak coordinator plus loopback fake dependencies", Reason: "test infrastructure, not a shipped long-lived process"},
				{Role: "build, init, and readiness CLI subprocesses", Reason: "transient helpers completed before warm steady-state sampling"},
			},
		},
		Readiness: readyEvidence(status, identities, readinessCounts, readyAt.Sub(startedAt)),
	}
	if err := waitBounded(ctx, options.Warm); err != nil {
		return evidence, errors.New("warm steady-state interval interrupted")
	}

	evidence.Samples, err = collectSoakSamples(ctx, options.Samples, options.Interval, identities, status, readinessCounts, soakSamplingHooks{
		Now:      time.Now,
		Wait:     waitBounded,
		Snapshot: processSnapshot,
		Status: func(sampleCtx context.Context) (control.Status, error) {
			return readDaemonStatus(sampleCtx, options.Root, environment, paths[soak.DaemonName], socketPath)
		},
		Tree: func(sampleCtx context.Context) ([]soak.ProcessIdentity, error) {
			return discoverDaemonTree(sampleCtx, daemonProcess.command.Process.Pid)
		},
		Counts: fake.Counts,
	})
	if err != nil {
		return evidence, err
	}
	metricSamples := make([]soak.Sample, 0, len(evidence.Samples))
	for _, sample := range evidence.Samples {
		metricSamples = append(metricSamples, sample.Sample)
	}
	summary, err := soak.Evaluate(metricSamples, evidence.Metadata.Limits)
	if err != nil {
		return evidence, err
	}
	evidence.Summary = &summary
	return evidence, nil
}

func syntheticCodexAuthFixture() []byte {
	return []byte("{\"tokens\":{\"access_token\":\"synthetic-soak-token\",\"account_id\":\"synthetic-soak-account\"}}\n")
}
