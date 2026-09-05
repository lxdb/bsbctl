package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/installer"
	"github.com/lxdb/bsbctl/internal/launchagent"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("BSBCTL_CLI_HELPER") != "1" {
		return
	}
	separator := -1
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		os.Exit(97)
	}
	processCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var cancel context.CancelFunc
	var serverDone <-chan error
	if mode := os.Getenv("BSBCTL_CLI_BACKEND"); mode != "" {
		ctx, stop := context.WithCancel(context.Background())
		cancel = stop
		backend := &processControlBackend{mode: mode}
		server, err := control.Listen(os.Getenv("BSBCTL_CLI_SOCKET"), version, control.Backends{
			Apps: backend, Catalog: backend, Operations: backend, Attention: backend, Status: backend,
		})
		if err != nil {
			os.Exit(96)
		}
		done := make(chan error, 1)
		serverDone = done
		go func() { done <- server.Serve(ctx) }()
	}
	if os.Getenv("BSBCTL_CLI_STDIN_READY") == "1" {
		processCtx = &stdinWaitContext{Context: processCtx}
	}
	code := executeProcess(processCtx, os.Args[separator+1:], os.Stdin, os.Stdout, os.Stderr)
	stopSignals()
	if cancel != nil {
		cancel()
		<-serverDone
	}
	os.Exit(code)
}

// This input-only command first waits on Done after its stdin read would block.
// The marker observes that wait without adding a hook to the production reader.
type stdinWaitContext struct {
	context.Context
	once sync.Once
}

func (c *stdinWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { _, _ = fmt.Fprintln(os.Stderr, "stdin ready") })
	return c.Context.Done()
}

func TestCLIProcessExitAndStreamContract(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr bool
	}{
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "usage: bsbctl <command> [options]\n"},
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: version + "\n"},
		{name: "missing", wantCode: 2, wantStderr: true},
		{name: "unknown", args: []string{"unknown"}, wantCode: 2, wantStderr: true},
		{name: "unknown flag", args: []string{"plugin", "list", "--wat"}, wantCode: 2, wantStderr: true},
		{name: "extra launch arg", args: []string{"app", "launch", "ball8", "ask", "extra"}, wantCode: 2, wantStderr: true},
		{name: "daemon unavailable", args: []string{"status", "--socket", filepath.Join(t.TempDir(), "missing.sock")}, wantCode: 4, wantStderr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := runCLIProcess(t, test.args...)
			if code != test.wantCode {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, test.wantCode, stdout, stderr)
			}
			if test.wantStdout != "" && !strings.HasPrefix(stdout, test.wantStdout) {
				t.Fatalf("stdout = %q, want prefix %q", stdout, test.wantStdout)
			}
			if test.wantStdout == "" && stdout != "" {
				t.Fatalf("failure stdout = %q", stdout)
			}
			if test.wantStderr != (stderr != "") {
				t.Fatalf("stderr = %q", stderr)
			}
		})
	}
}

func TestCLIHelpGroupsCommandsAndDocumentsExitCodes(t *testing.T) {
	code, stdout, stderr := runCLIProcess(t, "help")
	if code != exitSuccess || stderr != "" {
		t.Fatalf("help = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	for _, text := range []string{
		"Core commands:\n",
		"  bsbctl setup [--apps APP-ID,...|none] [--device-url URL]",
		"Apps and plugins:\n",
		"Attention:\n",
		"Service:\n",
		"Exit codes:\n",
		"  0  Success",
		"  6  Partial result or recovery required",
	} {
		if !strings.Contains(stdout, text) {
			t.Errorf("help does not contain %q\n%s", text, stdout)
		}
	}
}

func TestCLIProcessUsesPrivateHomeControlSocketByDefault(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "bctl-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	socketPath := filepath.Join(home, ".bsbctl", "ctl.sock")
	code, stdout, stderr := runCLIProcessEnv(t, map[string]string{
		"HOME":               home,
		"BSBCTL_CLI_BACKEND": "healthy",
		"BSBCTL_CLI_SOCKET":  socketPath,
	}, "status")
	if code != exitSuccess || !json.Valid([]byte(stdout)) || stderr != "" {
		t.Fatalf("default status = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestCLIWithoutHomeFailsBeforeUsingFallbackState(t *testing.T) {
	t.Setenv("HOME", "")

	t.Run("status does not dial a fallback socket", func(t *testing.T) {
		called := false
		previous := dialControl
		dialControl = func(context.Context, string) (controlClient, error) {
			called = true
			return nil, errors.New("unexpected dial")
		}
		defer func() { dialControl = previous }()
		var stdout, stderr bytes.Buffer
		if code := execute(context.Background(), []string{"status"}, strings.NewReader(""), &stdout, &stderr); code != exitOperational || called {
			t.Fatalf("status without home = code %d dialed %v stdout %q stderr %q", code, called, stdout.String(), stderr.String())
		}
	})

	t.Run("init does not write a fallback config", func(t *testing.T) {
		directory := t.TempDir()
		t.Chdir(directory)
		var stdout, stderr bytes.Buffer
		code := execute(context.Background(), []string{
			"init",
		}, strings.NewReader(""), &stdout, &stderr)
		if code != exitOperational || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("init without home = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
		}
		if _, err := os.Lstat(filepath.Join(directory, "bsbctl.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fallback config was created: %v", err)
		}
	})
}

func TestCLIProcessRejectsMalformedOversizedAndSymlinkInputsBeforeDaemon(t *testing.T) {
	directory := t.TempDir()
	malformedConfig := filepath.Join(directory, "malformed-config.json")
	oversizedConfig := filepath.Join(directory, "oversized-config.json")
	malformedCatalog := filepath.Join(directory, "malformed-catalog.json")
	oversizedCatalog := filepath.Join(directory, "oversized-catalog.json")
	signature := filepath.Join(directory, "catalog.sig")
	oversizedSignature := filepath.Join(directory, "oversized-catalog.sig")
	symlinkCatalog := filepath.Join(directory, "catalog-link.json")
	if err := os.WriteFile(malformedConfig, []byte(`{"config":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oversizedConfig, bytes.Repeat([]byte("x"), maxPluginConfigInput+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(malformedCatalog, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oversizedCatalog, bytes.Repeat([]byte("x"), (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signature, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oversizedSignature, bytes.Repeat([]byte("x"), (16<<10)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(malformedCatalog, symlinkCatalog); err != nil {
		t.Fatal(err)
	}
	tests := [][]string{
		{"app", "config", "ball8", "--file", malformedConfig},
		{"app", "config", "ball8", "--file", oversizedConfig},
		{"plugin", "install", "plugin", "--catalog", malformedCatalog, "--signature", signature, "--version", "1"},
		{"plugin", "install", "plugin", "--catalog", oversizedCatalog, "--signature", signature, "--version", "1"},
		{"plugin", "install", "plugin", "--catalog", signature, "--signature", oversizedSignature, "--version", "1"},
		{"plugin", "install", "plugin", "--catalog", symlinkCatalog, "--signature", signature, "--version", "1"},
	}
	for index, args := range tests {
		code, stdout, stderr := runCLIProcess(t, args...)
		if code != exitUsage || stdout != "" || stderr == "" {
			t.Fatalf("input %d = code %d stdout %q stderr %q", index, code, stdout, stderr)
		}
	}
}

func TestCLIProcessMapsRPCErrorsAndRedactsSensitiveBackendDetails(t *testing.T) {
	for _, test := range []struct {
		mode     string
		wantCode int
	}{
		{mode: "not_found", wantCode: exitRejected},
		{mode: "raw_failure", wantCode: exitOperational},
	} {
		t.Run(test.mode, func(t *testing.T) {
			directory, err := os.MkdirTemp("/tmp", "bctl-cli-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			socketPath := filepath.Join(directory, "control.sock")
			code, stdout, stderr := runCLIProcessEnv(t, map[string]string{"BSBCTL_CLI_BACKEND": test.mode, "BSBCTL_CLI_SOCKET": socketPath}, "app", "enable", "ball8", "--socket", socketPath)
			if code != test.wantCode || stdout != "" || stderr == "" || strings.Contains(stderr, "token=secret") || strings.Contains(stderr, "/private/path") || strings.Contains(stderr, "provider.invalid") {
				t.Fatalf("RPC process = code %d stdout %q stderr %q", code, stdout, stderr)
			}
		})
	}
}

func TestCLIProcessMapsAttentionRejectionSeparatelyFromBackendFailure(t *testing.T) {
	for _, test := range []struct {
		mode     string
		wantCode int
	}{
		{mode: "attention_rejected", wantCode: exitRejected},
		{mode: "attention_failure", wantCode: exitOperational},
	} {
		t.Run(test.mode, func(t *testing.T) {
			directory, err := os.MkdirTemp("/tmp", "bctl-cli-attention-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			socketPath := filepath.Join(directory, "control.sock")
			code, stdout, stderr := runCLIProcessEnv(t, map[string]string{"BSBCTL_CLI_BACKEND": test.mode, "BSBCTL_CLI_SOCKET": socketPath}, "attention", "acknowledge", "observation", "--socket", socketPath)
			if code != test.wantCode || stdout != "" || stderr == "" {
				t.Fatalf("attention RPC = code %d stdout %q stderr %q", code, stdout, stderr)
			}
		})
	}
}

func TestCommittedMutationResultSurvivesControlConnectionCloseFailure(t *testing.T) {
	client := &fakeCLIClient{
		call: func(_ context.Context, method string, _ any, result any) error {
			if method != "app.set_enabled" {
				t.Fatalf("method = %q", method)
			}
			*result.(*control.AppMutationResult) = control.AppMutationResult{
				Status: control.MutationUpdated, AppID: "codex-secondary", Enabled: true, Generation: 8,
			}
			return nil
		},
		closeErr: errors.New("private socket /Users/person/control.sock failed"),
	}
	restore := installCLIClient(t, client)
	defer restore()
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{"app", "enable", "codex-secondary", "--socket", "/unused"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitSuccess, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, `"status":"updated"`) || !strings.Contains(got, `"generation":8`) {
		t.Fatalf("committed result was lost: %q", got)
	}
	if stderr.String() == "" || strings.Contains(stderr.String(), "/Users/") || strings.Contains(stderr.String(), "private socket") {
		t.Fatalf("unsafe or missing close warning: %q", stderr.String())
	}
}

func TestSuccessfulStatusResultSurvivesControlConnectionCloseFailure(t *testing.T) {
	client := &fakeCLIClient{
		call: func(_ context.Context, method string, _ any, result any) error {
			if method != "daemon.status" {
				t.Fatalf("method = %q", method)
			}
			*result.(*control.Status) = control.Status{Version: "1.2.3", Generation: 9}
			return nil
		},
		closeErr: errors.New("private socket /Users/person/control.sock failed"),
	}
	restore := installCLIClient(t, client)
	defer restore()
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{"status", "--socket", "/unused"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitSuccess || !strings.Contains(stdout.String(), `"generation":9`) {
		t.Fatalf("status = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestCLIProcessInterruptMapsToCancellationAndJoinsRPC(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "bctl-cancel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketPath := filepath.Join(directory, "control.sock")
	commandArgs := []string{"-test.run=^TestCLIHelperProcess$", "--", "app", "enable", "ball8", "--socket", socketPath}
	command := exec.Command(os.Args[0], commandArgs...)
	command.Env = append(os.Environ(), "BSBCTL_CLI_HELPER=1", "BSBCTL_CLI_BACKEND=blocking", "BSBCTL_CLI_SOCKET="+socketPath)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Lstat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal("helper control socket did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		if !ok || exitErr.ExitCode() != exitCanceled || stdout.Len() != 0 || !strings.Contains(stderr.String(), "operation canceled") {
			t.Fatalf("interrupt = %v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
		}
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("interrupted helper did not exit")
	}
}

func TestCLIProcessTermCancelsUnfinishedStdin(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIHelperProcess$", "--", "app", "create", "example", "--plugin", "dev.test", "--file", "-", "--socket", "/unused")
	command.Env = append(os.Environ(), "BSBCTL_CLI_HELPER=1", "BSBCTL_CLI_STDIN_READY=1")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	reader := bufio.NewReader(stderr)
	if line, err := reader.ReadString('\n'); err != nil || line != "stdin ready\n" {
		t.Fatalf("unfinished stdin readiness = %q, %v", line, err)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != exitCanceled {
			t.Fatalf("unfinished stdin exit = %v, want %d", err, exitCanceled)
		}
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
		<-done
		t.Fatal("SIGTERM did not release the unfinished stdin read")
	}
}

func TestCatalogAndServiceCommandsMapStableOutcomes(t *testing.T) {
	directory := t.TempDir()
	catalogPath := filepath.Join(directory, "catalog.json")
	signaturePath := filepath.Join(directory, "catalog.sig")
	// Catalogs have their own 1 MiB limit, not the 64 KiB app-config limit.
	catalogData := []byte(`{"padding":"` + strings.Repeat("x", 65<<10) + `"}`)
	if err := os.WriteFile(catalogPath, catalogData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signaturePath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &fakeCLIClient{call: func(_ context.Context, method string, params, result any) error {
		if method != "plugin.install" {
			return errors.New("unexpected method")
		}
		request := params.(control.CatalogInstallRequest)
		catalogDigest := fmt.Sprintf("%x", sha256.Sum256(catalogData))
		signatureDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(`{}`)))
		if request.CatalogPath != catalogPath || request.SignaturePath != signaturePath || request.CatalogSHA256 != catalogDigest || request.SignatureSHA256 != signatureDigest || request.OS != "darwin" || (request.Arch != "arm64" && request.Arch != "amd64") {
			t.Fatalf("catalog request = %#v", request)
		}
		*(result.(*control.CatalogOperationResponse)) = control.CatalogOperationResponse{Result: installer.Result{Status: installer.StatusInstalled, Release: installer.ReleaseRef{ID: "plugin", Version: "1", OS: "darwin", Arch: request.Arch}}}
		return nil
	}}
	restoreClient := installCLIClient(t, client)
	defer restoreClient()
	t.Chdir(directory)
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{"plugin", "install", "plugin", "--catalog", filepath.Base(catalogPath), "--signature", filepath.Base(signaturePath), "--version", "1"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), `"status":"installed"`) || stderr.Len() != 0 {
		t.Fatalf("catalog install = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}

	manager := &fakeServiceManager{installResult: launchagent.Result{Status: launchagent.StateLoaded, PlistMatches: true, Changed: true}}
	restoreManager := installServiceManager(t, manager)
	defer restoreManager()
	stdout.Reset()
	stderr.Reset()
	code = execute(context.Background(), []string{"service", "install", "--config", "/tmp/config.json", "--socket", "/tmp/control.sock", "--plist", "/tmp/dev.bsbctl.plist"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stdout.String() != `{"status":"loaded","plist_matches":true,"changed":true}`+"\n" || stderr.Len() != 0 {
		t.Fatalf("service install = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	if manager.installConfig.LogPath != "" {
		t.Fatalf("default service install claimed application-owned log path %q", manager.installConfig.LogPath)
	}
	stdout.Reset()
	stderr.Reset()
	code = execute(context.Background(), []string{"service", "install", "--config", "/tmp/config.json", "--socket", "/tmp/control.sock", "--plist", "/tmp/dev.bsbctl.plist", "--log", "/tmp/diagnostic.jsonl"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || manager.installConfig.LogPath != "/tmp/diagnostic.jsonl" {
		t.Fatalf("explicit service log = code %d config %#v stderr %q", code, manager.installConfig, stderr.String())
	}
}

func TestCLIInputFailuresUseUsageOrOperationalExit(t *testing.T) {
	directory := t.TempDir()
	validConfig := filepath.Join(directory, "config.json")
	validCatalog := filepath.Join(directory, "catalog.json")
	validSignature := filepath.Join(directory, "catalog.sig")
	for path, data := range map[string][]byte{
		validConfig: []byte(`{"config":{}}`), validCatalog: []byte(`{}`), validSignature: []byte(`{}`),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unreadableConfig := filepath.Join(directory, "unreadable-config.json")
	unreadableCatalog := filepath.Join(directory, "unreadable-catalog.json")
	unreadableSignature := filepath.Join(directory, "unreadable-catalog.sig")
	for path, data := range map[string][]byte{
		unreadableConfig: []byte(`{"config":{}}`), unreadableCatalog: []byte(`{}`), unreadableSignature: []byte(`{}`),
	} {
		if err := os.WriteFile(path, data, 0); err != nil {
			t.Fatal(err)
		}
	}
	symlinkConfig := filepath.Join(directory, "config-link.json")
	if err := os.Symlink(validConfig, symlinkConfig); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		args     []string
		wantExit int
	}{
		{name: "config malformed", args: []string{"app", "config", "ball8", "--file", validCatalog}, wantExit: exitUsage},
		{name: "config symlink", args: []string{"app", "config", "ball8", "--file", symlinkConfig}, wantExit: exitUsage},
		{name: "config missing", args: []string{"app", "config", "ball8", "--file", filepath.Join(directory, "missing-config.json")}, wantExit: exitOperational},
		{name: "config unreadable", args: []string{"app", "config", "ball8", "--file", unreadableConfig}, wantExit: exitOperational},
		{name: "catalog missing", args: []string{"plugin", "install", "plugin", "--catalog", filepath.Join(directory, "missing-catalog.json"), "--signature", validSignature, "--version", "1"}, wantExit: exitOperational},
		{name: "catalog unreadable", args: []string{"plugin", "install", "plugin", "--catalog", unreadableCatalog, "--signature", validSignature, "--version", "1"}, wantExit: exitOperational},
		{name: "signature missing", args: []string{"plugin", "install", "plugin", "--catalog", validCatalog, "--signature", filepath.Join(directory, "missing-signature.sig"), "--version", "1"}, wantExit: exitOperational},
		{name: "signature unreadable", args: []string{"plugin", "install", "plugin", "--catalog", validCatalog, "--signature", unreadableSignature, "--version", "1"}, wantExit: exitOperational},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := execute(context.Background(), test.args, strings.NewReader(""), &stdout, &stderr)
			if code != test.wantExit || stdout.Len() != 0 || stderr.Len() == 0 || strings.Contains(stderr.String(), directory) {
				t.Fatalf("exit=%d stdout=%q stderr=%q, want exit %d and path-safe failure", code, stdout.String(), stderr.String(), test.wantExit)
			}
		})
	}
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{"app", "config", "ball8", "--file", "-"}, &failingInputReader{}, &stdout, &stderr)
	if code != exitOperational || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("mid-read failure = exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestCLIRejectsFIFOAndUnixSocketInputsWithoutOpeningThem(t *testing.T) {
	directory := t.TempDir()
	fifoPath := filepath.Join(directory, "config.fifo")
	if err := unix.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatal(err)
	}

	type result struct {
		code   int
		stdout string
		stderr string
	}
	resultChannel := make(chan result, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := execute(context.Background(), []string{"app", "config", "ball8", "--file", fifoPath}, strings.NewReader(""), &stdout, &stderr)
		resultChannel <- result{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}()

	stopProbe := make(chan struct{})
	probeConnected := make(chan struct{})
	probeDone := make(chan struct{})
	probeError := make(chan error, 1)
	go probeFIFOWriter(fifoPath, stopProbe, probeConnected, probeDone, probeError)

	var fifoResult result
	select {
	case fifoResult = <-resultChannel:
		close(stopProbe)
		<-probeDone
	case <-probeConnected:
		fifoResult = <-resultChannel
		<-probeDone
		t.Fatalf("CLI opened FIFO before classifying it: code=%d stdout=%q stderr=%q", fifoResult.code, fifoResult.stdout, fifoResult.stderr)
	case err := <-probeError:
		close(stopProbe)
		<-probeDone
		t.Fatal(err)
	}
	if fifoResult.code != exitUsage || fifoResult.stdout != "" || fifoResult.stderr == "" {
		t.Fatalf("FIFO = code %d stdout %q stderr %q", fifoResult.code, fifoResult.stdout, fifoResult.stderr)
	}

	socketDirectory, err := os.MkdirTemp("/tmp", "bctl-input-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "config.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{"app", "config", "ball8", "--file", socketPath}, strings.NewReader(""), &stdout, &stderr)
	if code != exitUsage || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("Unix socket = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func probeFIFOWriter(path string, stop <-chan struct{}, connected chan<- struct{}, done chan<- struct{}, failure chan<- error) {
	defer close(done)
	for {
		select {
		case <-stop:
			return
		default:
		}
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			connected <- struct{}{}
			if closeErr := unix.Close(fd); closeErr != nil {
				failure <- closeErr
			}
			return
		}
		if !errors.Is(err, unix.ENXIO) {
			failure <- err
			return
		}
		runtime.Gosched()
	}
}

type failingInputReader struct{ read bool }

func (reader *failingInputReader) Read(destination []byte) (int, error) {
	if reader.read {
		return 0, errors.New("injected read failure")
	}
	reader.read = true
	return copy(destination, `{"config":`), nil
}

func TestExecuteMapsCancellationPartialAndBrokenStdout(t *testing.T) {
	client := &fakeCLIClient{call: func(ctx context.Context, _ string, _, _ any) error { return ctx.Err() }}
	restore := installCLIClient(t, client)
	defer restore()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if code := execute(canceled, []string{"status", "--socket", "/ignored"}, strings.NewReader(""), io.Discard, io.Discard); code != 5 {
		t.Fatalf("cancellation exit = %d", code)
	}
	client.call = func(_ context.Context, method string, _, result any) error {
		if method == "app.set_enabled" {
			*(result.(*control.AppMutationResult)) = control.AppMutationResult{Status: control.MutationDurabilityUncertain, AppID: "ball8", Enabled: true, Generation: 2}
		}
		return nil
	}
	if code := execute(context.Background(), []string{"app", "enable", "ball8"}, strings.NewReader(""), io.Discard, io.Discard); code != 6 {
		t.Fatalf("partial exit = %d", code)
	}
	client.call = func(_ context.Context, method string, _, result any) error {
		if method == "daemon.status" {
			*(result.(*control.Status)) = control.Status{}
		}
		return nil
	}
	if code := execute(context.Background(), []string{"status"}, strings.NewReader(""), failingWriter{}, io.Discard); code != 4 {
		t.Fatalf("broken stdout exit = %d", code)
	}
}

func TestExecuteCancellationPreservesPartialMutationExit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	manager := &fakeServiceManager{
		installResult: launchagent.Result{Status: launchagent.StateLoaded, Changed: true},
		installErr:    errors.Join(launchagent.ErrPartial, context.Canceled),
	}
	t.Cleanup(installServiceManager(t, manager))
	cancel()
	var stdout, stderr bytes.Buffer
	code := execute(ctx, []string{
		"service", "install", "--config", "/tmp/test-config.json",
		"--socket", "/tmp/test-control.sock", "--plist", "/tmp/test-agent.plist",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != exitPartial || !strings.Contains(stdout.String(), `"changed":true`) || !strings.Contains(stderr.String(), "service operation partially completed") {
		t.Fatalf("partial mutation = exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestInitAndAttentionMutationCommandsNeverSucceedSilently(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{"init", "--config", configPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stdout.String() != `{"status":"initialized","generation":1}`+"\n" || stderr.Len() != 0 {
		t.Fatalf("init = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}

	client := &fakeCLIClient{call: func(_ context.Context, method string, params, result any) error {
		if method != "attention.acknowledge" || params.(control.AttentionAcknowledgeRequest).ObservationID != "observation" {
			return errors.New("unexpected attention call")
		}
		return nil
	}}
	restore := installCLIClient(t, client)
	defer restore()
	stdout.Reset()
	stderr.Reset()
	code = execute(context.Background(), []string{"attention", "acknowledge", "observation", "--socket", "/ignored"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stdout.String() != `{"status":"acknowledged","observation_id":"observation"}`+"\n" || stderr.Len() != 0 {
		t.Fatalf("acknowledge = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestCLIInitAndDaemonRejectNonStrictLongOptions(t *testing.T) {
	pluginPath := "/bin/true"
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "init duplicate",
			args: []string{"init", "--config", filepath.Join(t.TempDir(), "one.json"), "--config", filepath.Join(t.TempDir(), "two.json"), "--plugin", pluginPath},
		},
		{
			name: "init single dash",
			args: []string{"init", "-config", filepath.Join(t.TempDir(), "config.json"), "--plugin", pluginPath},
		},
		{
			name: "init extra argument",
			args: []string{"init", "--config", filepath.Join(t.TempDir(), "config.json"), "--plugin", pluginPath, "extra"},
		},
		{
			name: "daemon duplicate",
			args: []string{"daemon", "--config", filepath.Join(t.TempDir(), "one.json"), "--config", filepath.Join(t.TempDir(), "two.json")},
		},
		{
			name: "daemon single dash",
			args: []string{"daemon", "-config", filepath.Join(t.TempDir(), "config.json")},
		},
		{
			name: "daemon extra argument",
			args: []string{"daemon", "--config", filepath.Join(t.TempDir(), "config.json"), "extra"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := execute(context.Background(), test.args, strings.NewReader(""), &stdout, &stderr); code != exitUsage {
				t.Fatalf("execute(%v) = code %d, stdout %q, stderr %q; want usage", test.args, code, stdout.String(), stderr.String())
			}
		})
	}
}

func runCLIProcess(t *testing.T, args ...string) (int, string, string) {
	return runCLIProcessEnv(t, nil, args...)
}

func runCLIProcessEnv(t *testing.T, environment map[string]string, args ...string) (int, string, string) {
	t.Helper()
	commandArgs := []string{"-test.run=^TestCLIHelperProcess$", "--"}
	commandArgs = append(commandArgs, args...)
	command := exec.Command(os.Args[0], commandArgs...)
	command.Env = append(os.Environ(), "BSBCTL_CLI_HELPER=1")
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err == nil {
		return 0, stdout.String(), stderr.String()
	}
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		t.Fatal(err)
	}
	return exitErr.ExitCode(), stdout.String(), stderr.String()
}

type processControlBackend struct{ mode string }

func (b *processControlBackend) SetEnabled(ctx context.Context, _ string, _ bool) (daemon.EnableResult, error) {
	if b.mode == "blocking" {
		<-ctx.Done()
		return daemon.EnableResult{}, ctx.Err()
	}
	if b.mode == "not_found" {
		return daemon.EnableResult{}, daemon.ErrAppNotFound
	}
	return daemon.EnableResult{}, errors.New("token=secret /private/path provider.invalid")
}

func (*processControlBackend) CreateAppInstance(context.Context, config.App) (daemon.AppInstanceResult, error) {
	return daemon.AppInstanceResult{}, errors.New("app creation unavailable")
}

func (*processControlBackend) DeleteAppInstance(context.Context, string) (daemon.AppInstanceResult, error) {
	return daemon.AppInstanceResult{}, errors.New("app deletion unavailable")
}

func (*processControlBackend) ReplaceAppConfiguration(context.Context, string, daemon.AppConfiguration) (config.Document, localstate.CommitOutcome, error) {
	return config.Document{}, localstate.NotCommitted, errors.New("app configuration unavailable")
}

func (*processControlBackend) CatalogInstall(context.Context, installer.InstallRequest, bool) (installer.Result, error) {
	return installer.Result{}, errors.New("plugin installation unavailable")
}

func (*processControlBackend) CatalogRollback(context.Context, installer.RollbackRequest) (installer.Result, error) {
	return installer.Result{}, errors.New("plugin rollback unavailable")
}

func (*processControlBackend) CatalogStatus(context.Context, string) (installer.Snapshot, error) {
	return installer.Snapshot{}, errors.New("plugin status unavailable")
}

func (*processControlBackend) Launch(context.Context, string, string, json.RawMessage) error {
	return nil
}

func (*processControlBackend) PluginOperation(context.Context, string, protocol.OperationKind, string, json.RawMessage) (protocol.OperationResult, error) {
	return protocol.OperationResult{}, errors.New("plugin operation unavailable")
}

func (*processControlBackend) Document() (config.Document, bool) {
	return config.Document{Version: config.CurrentVersion, Generation: 1, Apps: map[string]config.App{}, Plugins: map[string]config.Plugin{}}, true
}

func (*processControlBackend) Status() []pluginhost.PluginStatus { return nil }

func (*processControlBackend) RuntimeDiagnostics() daemon.RuntimeDiagnostics {
	return daemon.RuntimeDiagnostics{}
}

func (*processControlBackend) AttentionSnapshot() (attention.Trace, bool) {
	return attention.Trace{}, false
}

func (*processControlBackend) AttentionExplain(string) (attention.Evaluation, bool) {
	return attention.Evaluation{}, false
}

func (*processControlBackend) AttentionHistory(int, time.Time) []attention.Trace { return nil }

func (b *processControlBackend) AcknowledgeAttention(string) error {
	switch b.mode {
	case "attention_rejected":
		return daemon.ErrObservationNotFound
	case "attention_failure":
		return errors.New("backend unavailable")
	default:
		return nil
	}
}

func (*processControlBackend) Wake() {}

func (*processControlBackend) Reconcile(context.Context) error { return nil }

func (*processControlBackend) RecorderStatus() attention.RecorderStatus {
	return attention.RecorderStatus{}
}

func (*processControlBackend) ObservationDiagnostics() observation.StoreDiagnostics {
	return observation.StoreDiagnostics{}
}

func (*processControlBackend) AttentionStateStatus() daemon.AttentionStateDiagnostics {
	return daemon.AttentionStateDiagnostics{}
}

func (*processControlBackend) PresentationCooldownStatus() daemon.PresentationCooldownDiagnostics {
	return daemon.PresentationCooldownDiagnostics{}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
