// Package pluginverify exercises the built-in package contract without
// certifying an independent protocol implementation.
package pluginverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/configschema"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	fixtureVersion  = 1
	maxFixtureBytes = 1 << 20
	stepTimeout     = 5 * time.Second
)

type SessionCase struct {
	Start  protocol.SessionStartRequest   `json:"start"`
	Inputs []protocol.SessionInputRequest `json:"inputs,omitempty"`
	End    protocol.SessionEndRequest     `json:"end"`
}

type Fixture struct {
	Version    int                         `json:"version"`
	Instances  []protocol.Instance         `json:"instances"`
	Sessions   []SessionCase               `json:"sessions,omitempty"`
	Operations []protocol.OperationRequest `json:"operations,omitempty"`
}

type Options struct {
	ManifestPath   string
	FixturePath    string
	ExecutablePath string
	CoreVersion    string
}

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

type HostCalls struct {
	Observations       int `json:"observations"`
	Withdrawals        int `json:"withdrawals"`
	Checkpoints        int `json:"checkpoints"`
	SessionCompletions int `json:"session_completions"`
	Logs               int `json:"logs"`
	Metrics            int `json:"metrics"`
}

type Report struct {
	PluginID       string    `json:"plugin_id,omitempty"`
	PluginVersion  string    `json:"plugin_version,omitempty"`
	Protocol       string    `json:"protocol_version,omitempty"`
	Passed         bool      `json:"passed"`
	Claim          string    `json:"claim"`
	SignatureTrust string    `json:"signature_trust"`
	Checks         []Check   `json:"checks"`
	HostCalls      HostCalls `json:"host_calls"`
	Error          string    `json:"error,omitempty"`
}

func Verify(ctx context.Context, options Options) (report Report, resultErr error) {
	report.Claim = "built-in package contract exercised"
	report.SignatureTrust = "not_evaluated"
	addCheck := func(name string) { report.Checks = append(report.Checks, Check{Name: name, Passed: true}) }
	fail := func(err error) (Report, error) {
		report.Passed = false
		report.Error = err.Error()
		return report, err
	}

	manifestPath, err := absoluteRegularPath(options.ManifestPath)
	if err != nil {
		return fail(fmt.Errorf("manifest: %w", err))
	}
	root := filepath.Dir(manifestPath)
	manifestData, err := readBounded(manifestPath, 1<<20)
	if err != nil {
		return fail(fmt.Errorf("manifest: %w", err))
	}
	var identity struct {
		ID         string `json:"id"`
		Version    string `json:"version"`
		Executable string `json:"executable"`
	}
	if err := json.Unmarshal(manifestData, &identity); err != nil {
		return fail(errors.New("manifest JSON is invalid"))
	}
	manifest, err := catalog.VerifyPackageManifest(manifestData, catalog.Entry{ID: identity.ID, Version: identity.Version, Executable: identity.Executable}, root)
	if err != nil {
		return fail(err)
	}
	report.PluginID, report.PluginVersion, report.Protocol = manifest.ID, manifest.Version, manifest.ProtocolVersion
	addCheck("manifest")

	executablePath := options.ExecutablePath
	if executablePath == "" {
		executablePath = filepath.Join(root, filepath.FromSlash(manifest.Executable))
	}
	executablePath, err = absoluteRegularPath(executablePath)
	if err != nil {
		return fail(fmt.Errorf("executable: %w", err))
	}
	if err := verifyFile(executablePath, manifest.ExecutableSize, manifest.ExecutableSHA256); err != nil {
		return fail(fmt.Errorf("executable: %w", err))
	}
	addCheck("executable_digest")
	for _, declaration := range manifest.Assets {
		if err := verifyFile(filepath.Join(root, filepath.FromSlash(declaration.Source)), declaration.Size, declaration.SHA256); err != nil {
			return fail(fmt.Errorf("asset %q: %w", declaration.Source, err))
		}
	}
	addCheck("assets")

	fixturePath, err := absoluteRegularPath(options.FixturePath)
	if err != nil {
		return fail(fmt.Errorf("fixture: %w", err))
	}
	fixtureData, err := readBounded(fixturePath, maxFixtureBytes)
	if err != nil {
		return fail(fmt.Errorf("fixture: %w", err))
	}
	var fixture Fixture
	if err := protocol.DecodeStrict(fixtureData, &fixture); err != nil {
		return fail(errors.New("fixture JSON is invalid"))
	}
	if err := validateFixture(manifest, fixture, time.Now().UTC()); err != nil {
		return fail(err)
	}
	if manifest.ConfigSchema != nil {
		schema, err := configschema.Load(root, *manifest.ConfigSchema)
		if err != nil {
			return fail(err)
		}
		for _, instance := range fixture.Instances {
			if err := schema.Validate(instance.Config); err != nil {
				return fail(fmt.Errorf("instance %q configuration does not match schema", instance.ID))
			}
		}
	}
	addCheck("fixture_coverage")

	instances := make([]pluginhost.Instance, len(fixture.Instances))
	for index, instance := range fixture.Instances {
		policies := make(map[string]presentation.PolicyConfig, len(manifest.Channels))
		for _, channel := range manifest.Channels {
			policies[channel.ID] = presentation.PolicyConfig{Policy: "rotation"}
		}
		instances[index] = pluginhost.Instance{ID: instance.ID, Generation: instance.Generation, Config: instance.Config, Secrets: instance.Secrets, Policies: policies}
		if len(instance.Checkpoint) != 0 {
			instances[index].Checkpoint = &pluginhost.CheckpointRestore{Generation: instance.Generation, Data: instance.Checkpoint}
		}
	}

	var callsMu sync.Mutex
	var calls HostCalls
	defer func() {
		callsMu.Lock()
		report.HostCalls = calls
		callsMu.Unlock()
	}()
	callbacks := pluginhost.Callbacks{
		Observe: func(_ observation.Source, value protocol.Observation) error {
			if err := value.Validate(time.Now().UTC()); err != nil {
				return err
			}
			callsMu.Lock()
			calls.Observations++
			callsMu.Unlock()
			return nil
		},
		Withdraw: func(_ string, request protocol.WithdrawRequest) error {
			if err := request.Instance.Validate(); err != nil {
				return err
			}
			callsMu.Lock()
			calls.Withdrawals++
			callsMu.Unlock()
			return nil
		},
		Checkpoint: func(_ string, request protocol.CheckpointRequest) error {
			if err := request.Validate(); err != nil {
				return err
			}
			callsMu.Lock()
			calls.Checkpoints++
			callsMu.Unlock()
			return nil
		},
		CompleteSession: func(_ context.Context, _ string, request protocol.CompleteSessionRequest) error {
			if err := request.Validate(); err != nil {
				return err
			}
			callsMu.Lock()
			calls.SessionCompletions++
			callsMu.Unlock()
			return nil
		},
		Log: func(_ string, notification protocol.LogNotification) {
			callsMu.Lock()
			calls.Logs++
			callsMu.Unlock()
		},
		Metric: func(protocol.MetricNotification) {
			callsMu.Lock()
			calls.Metrics++
			callsMu.Unlock()
		},
	}
	spec := pluginhost.Spec{
		ID: manifest.ID, Version: manifest.Version, Executable: executablePath, SHA256: manifest.ExecutableSHA256,
		ProtocolVersion: manifest.ProtocolVersion, ExecutionModes: slices.Clone(manifest.ExecutionModes),
		Channels:   slices.Clone(manifest.Channels),
		Operations: slices.Clone(manifest.Operations), Instances: instances,
	}
	coreVersion := options.CoreVersion
	if coreVersion == "" {
		coreVersion = "plugin-verifier"
	}
	startCtx, cancel := context.WithTimeout(ctx, stepTimeout)
	process, err := pluginhost.Start(startCtx, coreVersion, spec, callbacks)
	cancel()
	if err != nil {
		return fail(err)
	}
	stopped := false
	defer func() {
		if stopped {
			return
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), stepTimeout)
		defer stopCancel()
		_ = process.Stop(stopCtx)
	}()
	addCheck("initialize_and_replace")

	call := func(name string, operation func(context.Context) error) error {
		stepCtx, stepCancel := context.WithTimeout(ctx, stepTimeout)
		defer stepCancel()
		if err := operation(stepCtx); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		addCheck(name)
		return nil
	}
	if err := call("idempotent_replace", func(stepCtx context.Context) error { return process.ReplaceInstances(stepCtx, instances) }); err != nil {
		return fail(err)
	}
	for index, session := range fixture.Sessions {
		session := session
		if err := call(fmt.Sprintf("session_start_%d", index), func(stepCtx context.Context) error {
			return process.Invoke(stepCtx, pluginhost.InvokeRequest{InstanceID: session.Start.Instance.ID, Generation: session.Start.Instance.Generation, Action: session.Start.Action, Payload: session.Start.Payload, SessionToken: session.Start.SessionToken, Trigger: session.Start.Trigger})
		}); err != nil {
			return fail(err)
		}
		for inputIndex, input := range session.Inputs {
			input := input
			if err := call(fmt.Sprintf("session_%d_input_%d", index, inputIndex), func(stepCtx context.Context) error {
				_, err := process.SessionInput(stepCtx, input)
				return err
			}); err != nil {
				return fail(err)
			}
		}
		if err := call(fmt.Sprintf("session_end_%d", index), func(stepCtx context.Context) error {
			return process.EndSession(stepCtx, pluginhost.EndSessionRequest{InstanceID: session.End.Instance.ID, Generation: session.End.Instance.Generation, SessionToken: session.End.SessionToken})
		}); err != nil {
			return fail(err)
		}
	}
	for index, request := range fixture.Operations {
		request := request
		if err := call(fmt.Sprintf("operation_%d", index), func(stepCtx context.Context) error {
			result, err := process.Operation(stepCtx, request)
			if err == nil {
				err = result.Validate()
			}
			return err
		}); err != nil {
			return fail(err)
		}
	}
	if err := call("health", func(stepCtx context.Context) error {
		health, err := process.Ping(stepCtx)
		if err == nil && !health.Healthy {
			err = errors.New("plugin reported unhealthy")
		}
		return err
	}); err != nil {
		return fail(err)
	}
	canceledCtx, cancelCall := context.WithCancel(ctx)
	cancelCall()
	if _, err := process.Ping(canceledCtx); !errors.Is(err, context.Canceled) {
		return fail(errors.New("pre-canceled call was not canceled"))
	}
	addCheck("pre_canceled_call")
	if err := call("empty_replace", func(stepCtx context.Context) error { return process.ReplaceInstances(stepCtx, nil) }); err != nil {
		return fail(err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), stepTimeout)
	err = process.Stop(stopCtx)
	stopCancel()
	stopped = true
	if err != nil {
		return fail(fmt.Errorf("shutdown: %w", err))
	}
	addCheck("shutdown")
	report.Passed = true
	return report, nil
}

func validateFixture(manifest catalog.PackageManifest, fixture Fixture, now time.Time) error {
	if fixture.Version != fixtureVersion || len(fixture.Instances) == 0 {
		return errors.New("fixture requires version 1 and at least one instance")
	}
	instances := make(map[protocol.InstanceRef]struct{}, len(fixture.Instances))
	for index, instance := range fixture.Instances {
		if err := instance.Validate(); err != nil {
			return fmt.Errorf("fixture instance %d: %w", index, err)
		}
		if _, exists := instances[instance.Ref()]; exists {
			return fmt.Errorf("fixture instance %q generation %d is duplicated", instance.ID, instance.Generation)
		}
		instances[instance.Ref()] = struct{}{}
	}
	hasMode := func(want protocol.ExecutionMode) bool {
		return slices.Contains(manifest.ExecutionModes, want)
	}
	if hasMode(protocol.ExecutionModeInteractive) && len(fixture.Sessions) == 0 {
		return errors.New("fixture requires an interactive session case")
	}
	checkRef := func(ref protocol.InstanceRef) error {
		if _, exists := instances[ref]; !exists {
			return fmt.Errorf("fixture references unknown instance %q generation %d", ref.ID, ref.Generation)
		}
		return nil
	}
	for _, session := range fixture.Sessions {
		if err := session.Start.Validate(); err != nil {
			return err
		}
		if err := session.End.Validate(); err != nil {
			return err
		}
		if session.Start.Instance != session.End.Instance || session.Start.SessionToken != session.End.SessionToken {
			return errors.New("session start and end identities must match")
		}
		if err := checkRef(session.Start.Instance); err != nil {
			return err
		}
		for _, input := range session.Inputs {
			if err := input.Validate(); err != nil {
				return err
			}
			if input.Instance != session.Start.Instance || input.SessionToken != session.Start.SessionToken {
				return errors.New("session input target must match its session case")
			}
		}
	}
	operations := make(map[string]bool)
	for _, request := range fixture.Operations {
		if err := request.Validate(); err != nil {
			return err
		}
		if err := checkRef(request.Instance); err != nil {
			return err
		}
		operations[request.Operation] = true
	}
	for _, descriptor := range manifest.Operations {
		if !operations[descriptor.ID] {
			return fmt.Errorf("fixture requires a case for operation %q", descriptor.ID)
		}
	}
	return nil
}

func absoluteRegularPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("path is invalid")
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("path must name a regular non-symlink file")
	}
	return absolute, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || int64(len(data)) > limit {
		return nil, errors.New("file exceeds its size limit")
	}
	return data, nil
}

func verifyFile(path string, size int64, digest string) error {
	path, err := absoluteRegularPath(path)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != size {
		return errors.New("size does not match manifest")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, size+1))
	if err != nil || written != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("SHA-256 does not match manifest")
	}
	return nil
}
