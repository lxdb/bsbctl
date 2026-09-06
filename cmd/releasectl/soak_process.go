package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/soak"
)

func buildSoakBinary(ctx context.Context, root, output, packagePath string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return err
	}
	environment := replaceEnvironment(offlineEnvironment(), map[string]string{
		"CGO_ENABLED": "1", "GOOS": "darwin", "GOARCH": runtime.GOARCH,
	})
	_, err := commandText(ctx, root, environment, "go", "build", "-trimpath", "-o", output, packagePath)
	return err
}

func releaseSoakPluginDescriptors() []firstpartyplugins.Descriptor {
	result := make([]firstpartyplugins.Descriptor, 0)
	for _, descriptor := range firstpartyplugins.All() {
		if descriptor.SoakProfile == releaseSoakProfile {
			result = append(result, descriptor)
		}
	}
	return result
}

func stageLocalPluginMetadata(root, destination string, descriptor firstpartyplugins.Descriptor) error {
	if descriptor.SchemaPath == "" {
		return errors.New("configuration schema path is unavailable")
	}
	if err := copySoakFile(filepath.Join(root, filepath.FromSlash(descriptor.SchemaPath)), filepath.Join(destination, "config.schema.json")); err != nil {
		return err
	}
	for _, declaration := range descriptor.Assets {
		source := filepath.Join(root, filepath.FromSlash(descriptor.AssetRoot), filepath.FromSlash(declaration.Source))
		target := filepath.Join(destination, filepath.FromSlash(declaration.Source))
		if err := copySoakFile(source, target); err != nil {
			return err
		}
	}
	return nil
}

func copySoakFile(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o600)
}

func soakRuntimeEnvironment(home, codexHome string) []string {
	return replaceEnvironment(os.Environ(), map[string]string{
		"HOME": home, "CODEX_HOME": codexHome, "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local",
	})
}

func replaceEnvironment(source []string, replacements map[string]string) []string {
	result := make([]string, 0, len(source)+len(replacements))
	for _, entry := range source {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, replaced := replacements[name]; !replaced {
			result = append(result, entry)
		}
	}
	names := make([]string, 0, len(replacements))
	for name := range replacements {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		result = append(result, name+"="+replacements[name])
	}
	return result
}

func commandText(ctx context.Context, root string, environment []string, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = root
	command.Env = environment
	output := &boundedOutput{remaining: soakCommandOutputLimit}
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		return "", commandError(err, output.String())
	}
	return output.String(), nil
}

func startSoakDaemon(ctx context.Context, root string, environment []string, binary, configPath, socketPath string) (*soakDaemon, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The soak owns graceful shutdown and descendant cleanup after cancellation.
	// CommandContext would kill the parent before that cleanup can run.
	command := exec.Command(binary, "daemon", "--config", configPath, "--socket", socketPath)
	command.Dir = root
	command.Env = environment
	output := &boundedOutput{remaining: soakCommandOutputLimit}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start bsbctl daemon: %w", err)
	}
	process := &soakDaemon{command: command, done: make(chan struct{}), output: output}
	go func() {
		process.waitErr = command.Wait()
		close(process.done)
	}()
	return process, nil
}

func (process *soakDaemon) Stop() error {
	if process == nil || process.command == nil || process.command.Process == nil {
		return errors.New("bsbctl daemon process is unavailable for cleanup")
	}
	select {
	case <-process.done:
		if process.waitErr != nil {
			return fmt.Errorf("bsbctl daemon exited before cleanup: %w", process.waitErr)
		}
		return errors.New("bsbctl daemon exited before cleanup")
	default:
	}
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal bsbctl daemon for cleanup: %w", err)
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-process.done:
		return nil
	case <-timer.C:
		if err := process.command.Process.Kill(); err != nil {
			return fmt.Errorf("force bsbctl daemon cleanup: %w", err)
		}
		select {
		case <-process.done:
			return errors.New("bsbctl daemon required forced cleanup")
		case <-time.After(2 * time.Second):
			return errors.New("bsbctl daemon did not exit after forced cleanup")
		}
	}
}

func awaitSoakReadiness(ctx context.Context, root string, environment []string, binary, socketPath string, process *soakDaemon, fake *fakeDependencies) (control.Status, []soak.ProcessIdentity, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastStatus control.Status
	var lastStatusErr error
	var lastTreeErr error
	for {
		select {
		case <-ctx.Done():
			return control.Status{}, nil, fmt.Errorf(
				"deterministic daemon readiness timed out (status_error=%v status_ready=%t device=%s plugins=%d apps=%d fake=%+v process_tree_error=%v)",
				lastStatusErr, statusReady(lastStatus), lastStatus.Device.Phase, len(lastStatus.Plugins), len(lastStatus.Readiness), fake.Counts(), lastTreeErr,
			)
		case <-process.done:
			return control.Status{}, nil, fmt.Errorf("bsbctl daemon exited before readiness: %v: %s", process.waitErr, strings.TrimSpace(process.output.String()))
		case <-ticker.C:
			status, err := readDaemonStatus(ctx, root, environment, binary, socketPath)
			lastStatus = status
			lastStatusErr = err
			if err != nil || !statusReady(status) || !fake.Ready() {
				continue
			}
			identities, err := discoverDaemonTree(ctx, process.command.Process.Pid)
			if err != nil {
				lastTreeErr = err
				continue
			}
			return status, identities, nil
		}
	}
}

func awaitSoakControl(ctx context.Context, root string, environment []string, binary, socketPath string, process *soakDaemon) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("daemon control readiness timed out: %v", lastErr)
		case <-process.done:
			return fmt.Errorf("bsbctl daemon exited before control readiness: %v: %s", process.waitErr, strings.TrimSpace(process.output.String()))
		case <-ticker.C:
			if _, err := readDaemonStatus(ctx, root, environment, binary, socketPath); err == nil {
				return nil
			} else {
				lastErr = err
			}
		}
	}
}

func readDaemonStatus(ctx context.Context, root string, environment []string, binary, socketPath string) (control.Status, error) {
	output, err := commandText(ctx, root, environment, binary, "status", "--socket", socketPath)
	if err != nil {
		return control.Status{}, err
	}
	var status control.Status
	decoder := json.NewDecoder(strings.NewReader(output))
	if err := decoder.Decode(&status); err != nil {
		return control.Status{}, err
	}
	return status, nil
}

func statusReady(status control.Status) bool {
	descriptors := releaseSoakPluginDescriptors()
	if status.Device.Phase != device.PhaseReady || status.Device.LastStateAt.IsZero() || len(status.Plugins) != len(descriptors) || len(status.Readiness) != len(descriptors) {
		return false
	}
	plugins := make(map[string]pluginhost.PluginStatus, len(descriptors))
	for _, plugin := range status.Plugins {
		plugins[plugin.ID] = plugin
	}
	for _, descriptor := range descriptors {
		plugin, exists := plugins[descriptor.ID]
		if !exists || plugin.Phase != pluginhost.PhaseRunning || !plugin.Running || !plugin.Healthy {
			return false
		}
	}
	apps := make(map[string]daemon.AppReadinessPhase, len(descriptors))
	for _, app := range status.Readiness {
		apps[app.AppID] = app.Phase
	}
	for _, descriptor := range descriptors {
		if apps[descriptor.DefaultApp.ID] != daemon.AppReady {
			return false
		}
	}
	return true
}
