package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestProcessDiscardsUnstructuredChildOutput(t *testing.T) {
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		t.Fatal(err)
	}
	originalStdout, originalStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdoutWrite, stderrWrite
	restoreOutput := func() {
		os.Stdout, os.Stderr = originalStdout, originalStderr
		_ = stdoutWrite.Close()
		_ = stderrWrite.Close()
	}
	t.Cleanup(func() {
		restoreOutput()
		_ = stdoutRead.Close()
		_ = stderrRead.Close()
	})
	process, err := Start(context.Background(), "test", Spec{
		ID: "dev.bsbctl.test", Version: "test", Executable: os.Args[0],
		ProtocolVersion: protocol.Version,
		Args: []string{
			"-test.run=^TestPluginHelperProcess$", "-test.bsbctl-plugin-helper=true", "-test.bsbctl-plugin-raw-output=true",
		},
		ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
		Channels:       []protocol.Channel{{ID: "main"}},
		Instances: []Instance{{
			ID: "test", Generation: 1, Config: []byte(`{}`),
			Policies: map[string]presentation.PolicyConfig{"main": {Policy: presentation.PolicyInteractive}},
		}},
	}, Callbacks{})
	restoreOutput()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = process.Stop(ctx)
	}()
	stdout, err := io.ReadAll(stdoutRead)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := io.ReadAll(stderrRead)
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout) != 0 || len(stderr) != 0 {
		t.Fatalf("unstructured child output escaped discard boundary: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestProcessStopAllowsNaturalExitBeforeSignaling(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{
		cmd:           &exec.Cmd{Process: &os.Process{Pid: 4242}},
		peer:          rpc.NewPeer(leftConn),
		done:          make(chan error, 1),
		reaped:        make(chan struct{}),
		stopDone:      make(chan struct{}),
		shutdownGrace: 100 * time.Millisecond,
		termGrace:     100 * time.Millisecond,
	}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	signals := make(chan syscall.Signal, 2)
	process.signalGroup = func(_ int, signal syscall.Signal) error {
		signals <- signal
		return nil
	}
	if err := remote.Handle("plugin.shutdown", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		close(process.reaped)
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := process.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case signal := <-signals:
		t.Fatalf("signal = %v, want natural exit without signal", signal)
	default:
	}
}

func TestProcessStopOrdersTermThenKillAndConcurrentCallersShareResult(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	waitErr := errors.New("wait failed")
	done := make(chan error, 1)
	done <- waitErr
	close(done)
	process := &Process{
		cmd:           &exec.Cmd{Process: &os.Process{Pid: 4242}},
		peer:          rpc.NewPeer(leftConn),
		done:          done,
		reaped:        make(chan struct{}),
		stopDone:      make(chan struct{}),
		shutdownGrace: 5 * time.Millisecond,
		termGrace:     5 * time.Millisecond,
		waitErr:       waitErr,
	}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	var signalsMu sync.Mutex
	var signals []syscall.Signal
	process.signalGroup = func(_ int, signal syscall.Signal) error {
		signalsMu.Lock()
		signals = append(signals, signal)
		signalsMu.Unlock()
		if signal == syscall.SIGKILL {
			close(process.reaped)
		}
		return nil
	}
	shutdownCalls := make(chan struct{}, 2)
	if err := remote.Handle("plugin.shutdown", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		shutdownCalls <- struct{}{}
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()

	results := make(chan error, 2)
	for range 2 {
		go func() {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
			defer stopCancel()
			results <- process.Stop(stopCtx)
		}()
	}
	for range 2 {
		if err := <-results; !errors.Is(err, waitErr) {
			t.Fatalf("Stop error = %v, want shared wait error", err)
		}
	}
	select {
	case <-shutdownCalls:
	default:
		t.Fatal("plugin.shutdown was not called")
	}
	select {
	case <-shutdownCalls:
		t.Fatal("plugin.shutdown was called more than once")
	default:
	}
	signalsMu.Lock()
	gotSignals := append([]syscall.Signal(nil), signals...)
	signalsMu.Unlock()
	if len(gotSignals) != 2 || gotSignals[0] != syscall.SIGTERM || gotSignals[1] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want [terminated killed]", gotSignals)
	}
	if err := <-process.Done(); !errors.Is(err, waitErr) {
		t.Fatalf("Done error = %v, want unconsumed wait error", err)
	}
}

func TestCoreDisconnectTerminatesPluginProcessGroup(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "descendant.pid")
	process, err := Start(context.Background(), "test", Spec{
		ID: "dev.bsbctl.test", Version: "test", Executable: os.Args[0],
		ProtocolVersion: protocol.Version,
		Args: []string{
			"-test.run=^TestPluginHelperProcess$", "-test.bsbctl-plugin-helper=true", "-test.bsbctl-plugin-descendant-pid=" + pidPath,
		},
		ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
		Channels:       []protocol.Channel{{ID: "main"}},
		Instances: []Instance{{
			ID: "test", Generation: 1, Config: []byte(`{}`),
			Policies: map[string]presentation.PolicyConfig{"main": {Policy: presentation.PolicyInteractive}},
		}},
	}, Callbacks{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if child, findErr := os.FindProcess(pid); findErr == nil {
			_ = child.Kill()
		}
	})
	if err := process.peer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("plugin did not exit after core disconnect")
	}
	assertProcessExited(t, pid, 2*time.Second)
}

func assertProcessExited(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatalf("check descendant process %d: %v", pid, err)
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatalf("descendant process %d survived core disconnect", pid)
		}
	}
}
