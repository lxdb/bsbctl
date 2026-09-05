// Package pluginhost supervises direct-child executable plugins.
package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

const (
	initializeTimeout   = 5 * time.Second
	shutdownTimeout     = 3 * time.Second
	shutdownGracePeriod = 500 * time.Millisecond
	termTimeout         = time.Second
)

// Spec is one verified plugin executable and its current enabled instances.
type Spec struct {
	ID              string
	Version         string
	Executable      string
	Args            []string
	SHA256          string
	ProtocolVersion string
	ExecutionModes  []protocol.ExecutionMode
	Channels        []protocol.Channel
	Operations      []protocol.OperationDescriptor
	Instances       []Instance
}

// Callbacks receive validated plugin effects.
type Callbacks struct {
	Observe            func(observation.Source, protocol.Observation) error
	Withdraw           func(string, protocol.WithdrawRequest) error
	WithdrawGeneration func(string, uint64)
	WithdrawInstance   func(string, string, uint64)
	Checkpoint         func(string, protocol.CheckpointRequest) error
	CompleteSession    func(context.Context, string, protocol.CompleteSessionRequest) error
	BeginExecution     func(context.Context, string, protocol.SessionExecutionRequest) error
	SessionCompleted   func(string, protocol.CompleteSessionRequest)
	Metric             func(protocol.MetricNotification)
	Log                func(string, protocol.LogNotification)
	// SessionInvalidated is a short, non-blocking core hook for sessions that
	// can no longer receive input from this package.
	SessionInvalidated func(SessionInvalidation)
	// SessionCleanup is a short, non-blocking core hook for stable cleanup
	// lifecycle ownership outside the supervisor.
	SessionCleanup func(SessionCleanup)
	// Started is a non-blocking core hook called after a child handshake.
	Started func(pluginID string, runID uint64)
}

// Process owns one direct child, its process group, RPC connection, and Wait call.
type Process struct {
	spec       Spec
	executable executableIdentity
	cmd        *exec.Cmd
	peer       *rpc.Peer
	cancel     context.CancelFunc
	done       chan error
	reaped     chan struct{}
	waitErr    error

	stopOnce       sync.Once
	stopDone       chan struct{}
	stopErr        error
	signalGroup    func(int, syscall.Signal) error
	shutdownGrace  time.Duration
	termGrace      time.Duration
	callbacks      Callbacks
	replaceMu      sync.Mutex
	effectMu       sync.Mutex
	effectInFlight map[protocol.InstanceRef]int
	effectChanged  chan struct{}

	policyMu  sync.RWMutex
	instances map[string]Instance
	pending   map[string]Instance
}

var beforeExecutableStart = func(string) error { return nil }

// Start verifies and starts a direct child, then completes the identity handshake.
func Start(ctx context.Context, coreVersion string, spec Spec, callbacks Callbacks) (*Process, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	executableFile, executable, err := openVerifiedExecutable(spec.Executable)
	if err != nil {
		return nil, err
	}
	closeExecutable := func() { _ = executableFile.Close() }
	if spec.SHA256 != "" && spec.SHA256 != executable.SHA256 {
		closeExecutable()
		return nil, errors.New("plugin executable digest does not match verified specification")
	}

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		closeExecutable()
		return nil, fmt.Errorf("create plugin socketpair: %w", err)
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	parentFile := os.NewFile(uintptr(fds[0]), "bsbctl-plugin-parent")
	childFile := os.NewFile(uintptr(fds[1]), "bsbctl-plugin-child")
	launch, err := prepareExecutableLaunch(executableFile, executable)
	if err != nil {
		_ = parentFile.Close()
		_ = childFile.Close()
		closeExecutable()
		return nil, err
	}
	if err := beforeExecutableStart(spec.Executable); err != nil {
		_ = parentFile.Close()
		_ = childFile.Close()
		closeExecutable()
		launch.cleanup()
		return nil, err
	}
	cleanupFiles := func() {
		_ = parentFile.Close()
		_ = childFile.Close()
		closeExecutable()
		launch.cleanup()
	}

	cmd := exec.Command(launch.path, spec.Args...)
	cmd.ExtraFiles = []*os.File{childFile}
	if launch.extra != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, launch.extra)
	}
	cmd.Env = childEnvironment(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: 0}
	if err := cmd.Start(); err != nil {
		cleanupFiles()
		return nil, fmt.Errorf("start plugin %s: %w", spec.ID, err)
	}
	_ = childFile.Close()
	closeExecutable()
	if launch.releaseParentAfterStart != nil {
		launch.releaseParentAfterStart()
	}
	conn, err := net.FileConn(parentFile)
	_ = parentFile.Close()
	if err != nil {
		_ = killGroup(cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		launch.cleanup()
		return nil, fmt.Errorf("open plugin %s socket: %w", spec.ID, err)
	}

	serveCtx, cancel := context.WithCancel(context.Background())
	process := &Process{
		spec: spec, executable: executable, cmd: cmd, peer: rpc.NewPeer(conn), cancel: cancel,
		done: make(chan error, 1), reaped: make(chan struct{}), stopDone: make(chan struct{}),
		signalGroup: killGroup, shutdownGrace: shutdownGracePeriod, termGrace: termTimeout, callbacks: callbacks,
		instances: make(map[string]Instance),
	}
	if err := process.register(callbacks); err != nil {
		_ = process.peer.Close()
		_ = killGroup(cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		launch.cleanup()
		return nil, err
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- process.peer.Serve(serveCtx) }()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	go func() {
		waitErr := <-waitDone
		launch.cleanup()
		cancel()
		_ = process.peer.Close()
		<-serveDone
		process.waitErr = waitErr
		process.done <- waitErr
		close(process.done)
		close(process.reaped)
	}()

	initCtx, initCancel := context.WithTimeout(ctx, initializeTimeout)
	defer initCancel()
	var rawResult json.RawMessage
	initializeErr := process.peer.Call(initCtx, "plugin.initialize", protocol.InitializeRequest{
		CoreVersion: coreVersion, PluginID: spec.ID, PluginVersion: spec.Version,
		ProtocolVersion: protocol.Version,
	}, &rawResult)
	err = mapInitializeRPCError(process.peer, initializeErr)
	var result protocol.InitializeResult
	if err == nil {
		if decodeErr := protocol.DecodeStrict(rawResult, &result); decodeErr != nil || result.Validate() != nil {
			err = newPluginRPCError("plugin_initialize_failed", "plugin initialization failed", nil)
		}
	}
	if err != nil || !matchesInitializeResult(spec, result) {
		_ = process.stopWithoutRPC()
		if err == nil {
			err = newPluginRPCError("plugin_initialize_failed", "plugin initialization failed", nil)
		}
		return nil, err
	}
	if !equalExecutionModes(result.ExecutionModes, spec.ExecutionModes) || !equalChannels(result.Channels, spec.Channels) || !equalOperations(result.Operations, spec.Operations) {
		_ = process.stopWithoutRPC()
		return nil, newPluginRPCError("plugin_initialize_failed", "plugin initialization failed", nil)
	}
	if err := process.ReplaceInstances(initCtx, spec.Instances); err != nil {
		_ = process.stopWithoutRPC()
		if domain, ok := errors.AsType[*protocol.DomainError](err); ok && domain.Kind() == protocol.ErrorInvalidArgument {
			return nil, PermanentStart(err)
		}
		return nil, err
	}
	return process, nil
}

func childEnvironment(parent []string) []string {
	values := make(map[string]string, len(parent))
	for _, entry := range parent {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	environment := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"BSBCTL_RPC_FD=3",
		"BSBCTL_OWN_PROCESS_GROUP=1",
	}
	for _, key := range []string{"HOME", "TMPDIR", "LANG", "LC_ALL", "TZ", "CODEX_HOME"} {
		if value := values[key]; value != "" {
			environment = append(environment, key+"="+value)
		}
	}
	return environment
}

func matchesInitializeResult(spec Spec, result protocol.InitializeResult) bool {
	return result.PluginID == spec.ID && result.PluginVersion == spec.Version && result.ProtocolVersion == protocol.Version
}

// Invoke starts one interactive plugin session.
