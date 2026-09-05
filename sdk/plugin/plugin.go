// Package plugin provides the public author API and runtime for executable
// bsbctl plugins. The normative wire and lifecycle contract is documented in
// docs/protocol/v1/spec.md and docs/plugin-authoring.md in the bsbctl module.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

// ErrPermanentConfiguration marks a replacement failure that cannot succeed
// until the executable or desired configuration changes.
var ErrPermanentConfiguration = errors.New("plugin configuration is permanently unavailable")

// PermanentConfiguration wraps err as a permanent replacement failure.
func PermanentConfiguration(err error) error {
	if err == nil {
		return ErrPermanentConfiguration
	}
	return errors.Join(ErrPermanentConfiguration, err)
}

// IsPermanentConfiguration reports whether err requires a configuration or
// executable change rather than a retry.
func IsPermanentConfiguration(err error) bool { return errors.Is(err, ErrPermanentConfiguration) }

// RejectSecrets enforces the contract for plugins that declare no secret
// inputs. It deliberately omits secret names and values from the error.
func RejectSecrets(instanceID string, secrets map[string]string) error {
	if len(secrets) == 0 {
		return nil
	}
	return PermanentConfiguration(fmt.Errorf("instance %q does not accept secrets", instanceID))
}

// Contract is the immutable capability set returned during initialization.
type Contract struct {
	ExecutionModes []protocol.ExecutionMode
	Channels       []protocol.Channel
	Operations     []protocol.OperationDescriptor
}

// Definition binds one immutable plugin identity and contract to its factory.
type Definition struct {
	ID       string
	Version  string
	Contract Contract
	New      func(*Host) Plugin
}

// Plugin is the required lifecycle surface. ReplaceInstances receives the
// complete enabled desired set, is serialized with every other callback, and
// must replace prior state atomically or leave it unchanged on error.
type Plugin interface {
	ReplaceInstances(context.Context, []protocol.Instance) error
}

// SessionHandler owns declared interactive sessions. EndSession must be
// idempotent for the exact instance generation and session token.
type SessionHandler interface {
	StartSession(context.Context, protocol.SessionStartRequest) error
	HandleSessionInput(context.Context, protocol.SessionInputRequest) (protocol.SessionInputResult, error)
	EndSession(context.Context, protocol.SessionEndRequest) error
}

// OperationHandler implements the immutable declared operation set.
type OperationHandler interface {
	InvokeOperation(context.Context, protocol.OperationRequest) (protocol.OperationResult, error)
}

// HealthReporter returns a current, UTC-stamped health result.
type HealthReporter interface {
	Health(context.Context) protocol.HealthResult
}

// Shutdowner releases plugin-owned workers and resources before its context
// expires. The runtime invokes it at most once.
type Shutdowner interface{ Shutdown(context.Context) error }

// Host exposes daemon-owned effects. State-changing methods require a
// committed instance set and may return ErrorNotReady during replacement.
// BeginSessionExecution instead returns ErrorSessionGenerationMismatch.
// Log and RecordMetric remain available during initialization and replacement.
type Host struct {
	peer  *rpc.Peer
	ready atomic.Bool
}

// PublishObservation replaces one exact-generation observation after local and
// daemon validation.
func (h *Host) PublishObservation(ctx context.Context, value protocol.Observation) error {
	if err := h.requireReady(); err != nil {
		return err
	}
	if err := value.Validate(time.Now().UTC()); err != nil {
		return protocol.NewDomainError(protocol.ErrorInvalidArgument, err)
	}
	return translateRemoteError(h.peer, h.peer.CallEmpty(ctx, "host.observation.publish", protocol.PublishRequest{Observation: value}))
}

// WithdrawObservation removes one exact-generation observation identity.
func (h *Host) WithdrawObservation(ctx context.Context, request protocol.WithdrawRequest) error {
	if err := h.requireReady(); err != nil {
		return err
	}
	if err := request.Validate(); err != nil {
		return protocol.NewDomainError(protocol.ErrorInvalidArgument, err)
	}
	return translateRemoteError(h.peer, h.peer.CallEmpty(ctx, "host.observation.withdraw", request))
}

// SaveCheckpoint durably replaces the bounded, non-secret checkpoint for one
// exact instance generation.
func (h *Host) SaveCheckpoint(ctx context.Context, request protocol.CheckpointRequest) error {
	if err := h.requireReady(); err != nil {
		return err
	}
	if err := request.Validate(); err != nil {
		return protocol.NewDomainError(protocol.ErrorInvalidArgument, err)
	}
	return translateRemoteError(h.peer, h.peer.CallEmpty(ctx, "host.checkpoint.save", request))
}

// CompleteSession reports that one exact interactive session has finished.
func (h *Host) CompleteSession(ctx context.Context, request protocol.CompleteSessionRequest) error {
	if err := h.requireReady(); err != nil {
		return err
	}
	if err := request.Validate(); err != nil {
		return protocol.NewDomainError(protocol.ErrorInvalidArgument, err)
	}
	return translateRemoteError(h.peer, h.peer.CallEmpty(ctx, "host.session.complete", request))
}

// BeginSessionExecution obtains the daemon's final grant immediately before
// one irreversible effect owned by an exact foreground session.
func (h *Host) BeginSessionExecution(ctx context.Context, request protocol.SessionExecutionRequest) error {
	if !h.ready.Load() {
		return protocol.NewDomainError(protocol.ErrorSessionGenerationMismatch, errors.New("instance replacement is not committed"))
	}
	if err := request.Validate(); err != nil {
		return protocol.NewDomainError(protocol.ErrorInvalidArgument, err)
	}
	return translateRemoteError(h.peer, h.peer.CallEmpty(ctx, "host.session.execution.begin", request))
}

// Log sends one validated structured diagnostic notification.
func (h *Host) Log(ctx context.Context, notification protocol.LogNotification) error {
	if err := notification.Validate(); err != nil {
		return protocol.NewDomainError(protocol.ErrorInvalidArgument, err)
	}
	return h.peer.Notify(ctx, "host.log", notification)
}

// RecordMetric attempts one validated lossy metric notification. accepted is
// false when the bounded transport queue drops it; callers should not spin.
func (h *Host) RecordMetric(notification protocol.MetricNotification) (bool, error) {
	if err := notification.Validate(); err != nil {
		return false, protocol.NewDomainError(protocol.ErrorInvalidArgument, err)
	}
	return h.peer.TryNotifyLossy("host.metric", notification)
}
func (h *Host) requireReady() error {
	if !h.ready.Load() {
		return protocol.NewDomainError(protocol.ErrorNotReady, errors.New("instance replacement is not committed"))
	}
	return nil
}

// Run validates definition, serves the inherited private RPC socket, serializes
// callbacks, propagates cancellation and deadlines, and performs bounded
// shutdown when ctx ends or the daemon disconnects.
func Run(ctx context.Context, definition Definition) error {
	if err := validateDefinition(definition); err != nil {
		return err
	}
	fd := 3
	if raw := os.Getenv("BSBCTL_RPC_FD"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 3 {
			return errors.New("BSBCTL_RPC_FD must be an inherited descriptor number")
		}
		fd = parsed
	}
	syscall.CloseOnExec(fd)
	file := os.NewFile(uintptr(fd), "bsbctl-plugin-rpc")
	if file == nil {
		return errors.New("open inherited bsbctl RPC descriptor")
	}
	conn, err := net.FileConn(file)
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("open bsbctl RPC socket: %w", err)
	}
	if closeErr != nil {
		_ = conn.Close()
		return fmt.Errorf("close inherited RPC descriptor: %w", closeErr)
	}
	peer := rpc.NewPeer(conn)
	host := &Host{peer: peer}
	runtime := &runtime{definition: definition, host: host}
	if err := runtime.register(peer); err != nil {
		_ = peer.Close()
		return err
	}
	serveErr, shutdownErr := serveAndShutdown(ctx, peer, runtime)
	if shouldTerminateOwnedGroup(ctx, serveErr) {
		_ = syscall.Kill(-os.Getpid(), syscall.SIGTERM)
	}
	if errors.Is(serveErr, context.Canceled) || errors.Is(serveErr, io.EOF) || errors.Is(serveErr, rpc.ErrClosed) || errors.Is(serveErr, syscall.ECONNRESET) || errors.Is(serveErr, os.ErrClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, shutdownErr)
}

func serveAndShutdown(ctx context.Context, peer *rpc.Peer, runtime *runtime) (error, error) {
	serveErr := peer.Serve(ctx)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return serveErr, runtime.shutdownRuntime(shutdownCtx)
}
func shouldTerminateOwnedGroup(ctx context.Context, serveErr error) bool {
	if ctx.Err() != nil || os.Getenv("BSBCTL_OWN_PROCESS_GROUP") != "1" {
		return false
	}
	if !(errors.Is(serveErr, io.EOF) || errors.Is(serveErr, rpc.ErrClosed) || errors.Is(serveErr, syscall.ECONNRESET) || errors.Is(serveErr, os.ErrClosed)) {
		return false
	}
	group, err := syscall.Getpgid(os.Getpid())
	return err == nil && group == os.Getpid()
}

type runtime struct {
	definition   Definition
	host         *Host
	stateMu      sync.Mutex
	initialized  bool
	handler      Plugin
	callbackMu   sync.Mutex
	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
}

func (r *runtime) register(peer *rpc.Peer) error {
	registrations := map[string]rpc.Handler{
		"plugin.initialize":        r.initialize,
		"plugin.instances.replace": r.replaceInstances,
		"plugin.session.start":     r.startSession,
		"plugin.session.input":     r.handleSessionInput,
		"plugin.session.end":       r.endSession,
		"plugin.operation.invoke":  r.invokeOperation,
		"plugin.health":            r.health,
		"plugin.shutdown":          r.shutdown,
	}
	for method, handler := range registrations {
		if err := peer.Handle(method, handler); err != nil {
			return err
		}
	}
	return nil
}

func (r *runtime) initialize(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
	var request protocol.InitializeRequest
	if err := decodeAndValidate(raw, &request); err != nil {
		return nil, invalidParams()
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.initialized {
		return nil, domainRPCError(protocol.ErrorGenerationConflict)
	}
	if request.PluginID != r.definition.ID || request.PluginVersion != r.definition.Version || request.ProtocolVersion != protocol.Version {
		return nil, domainRPCError(protocol.ErrorInvalidArgument)
	}
	handler := r.definition.New(r.host)
	if handler == nil {
		return nil, internalRPCError()
	}
	if err := validateHandlerContract(r.definition.Contract, handler); err != nil {
		return nil, domainRPCError(protocol.ErrorInvalidArgument)
	}
	r.handler, r.initialized = handler, true
	return protocol.InitializeResult{
		PluginID: r.definition.ID, PluginVersion: r.definition.Version, ProtocolVersion: protocol.Version,
		ExecutionModes: slices.Clone(r.definition.Contract.ExecutionModes),
		Channels:       slices.Clone(r.definition.Contract.Channels),
		Operations:     slices.Clone(r.definition.Contract.Operations),
	}, nil
}

func (r *runtime) replaceInstances(ctx context.Context, raw json.RawMessage) (any, *rpc.Error) {
	var request protocol.ReplaceInstancesRequest
	if err := r.decodeInitialized(raw, &request); err != nil {
		return nil, err
	}
	r.callbackMu.Lock()
	defer r.callbackMu.Unlock()
	wasReady := r.host.ready.Swap(false)
	if err := r.handler.ReplaceInstances(ctx, request.Instances); err != nil {
		r.host.ready.Store(wasReady)
		return nil, callbackError(err)
	}
	r.host.ready.Store(true)
	return struct{}{}, nil
}
func (r *runtime) startSession(ctx context.Context, raw json.RawMessage) (any, *rpc.Error) {
	var request protocol.SessionStartRequest
	if err := r.decodeInitialized(raw, &request); err != nil {
		return nil, err
	}
	handler, ok := r.handler.(SessionHandler)
	if !ok {
		return nil, domainRPCError(protocol.ErrorNotReady)
	}
	r.callbackMu.Lock()
	defer r.callbackMu.Unlock()
	if err := handler.StartSession(ctx, request); err != nil {
		return nil, callbackError(err)
	}
	return struct{}{}, nil
}
func (r *runtime) endSession(ctx context.Context, raw json.RawMessage) (any, *rpc.Error) {
	var request protocol.SessionEndRequest
	if err := r.decodeInitialized(raw, &request); err != nil {
		return nil, err
	}
	handler, ok := r.handler.(SessionHandler)
	if !ok {
		return nil, domainRPCError(protocol.ErrorNotReady)
	}
	r.callbackMu.Lock()
	defer r.callbackMu.Unlock()
	if err := handler.EndSession(ctx, request); err != nil {
		return nil, callbackError(err)
	}
	return struct{}{}, nil
}
func (r *runtime) handleSessionInput(ctx context.Context, raw json.RawMessage) (any, *rpc.Error) {
	var request protocol.SessionInputRequest
	if err := r.decodeInitialized(raw, &request); err != nil {
		return nil, err
	}
	handler, ok := r.handler.(SessionHandler)
	if !ok {
		return nil, domainRPCError(protocol.ErrorNotReady)
	}
	r.callbackMu.Lock()
	defer r.callbackMu.Unlock()
	result, err := handler.HandleSessionInput(ctx, request)
	if err != nil {
		return nil, callbackError(err)
	}
	if err := result.Validate(); err != nil {
		return nil, callbackError(err)
	}
	return result, nil
}
func (r *runtime) invokeOperation(ctx context.Context, raw json.RawMessage) (any, *rpc.Error) {
	var request protocol.OperationRequest
	if err := r.decodeInitialized(raw, &request); err != nil || !operationDeclared(r.definition.Contract.Operations, request.Operation) {
		return nil, invalidParams()
	}
	handler, ok := r.handler.(OperationHandler)
	if !ok {
		return nil, domainRPCError(protocol.ErrorNotReady)
	}
	r.callbackMu.Lock()
	defer r.callbackMu.Unlock()
	result, callErr := handler.InvokeOperation(ctx, request)
	if callErr != nil {
		return nil, callbackError(callErr)
	}
	if err := result.Validate(); err != nil {
		return nil, internalRPCError()
	}
	return result, nil
}
func (r *runtime) health(ctx context.Context, raw json.RawMessage) (any, *rpc.Error) {
	if err := protocol.ValidateEmptyParams(raw); err != nil {
		return nil, invalidParams()
	}
	if rpcErr := r.requireInitialized(); rpcErr != nil {
		return nil, rpcErr
	}
	r.callbackMu.Lock()
	defer r.callbackMu.Unlock()
	if handler, ok := r.handler.(HealthReporter); ok {
		result := handler.Health(ctx)
		if err := result.Validate(); err != nil {
			return nil, internalRPCError()
		}
		return result, nil
	}
	return protocol.HealthResult{Healthy: true, ObservedAt: time.Now().UTC()}, nil
}
func (r *runtime) shutdown(ctx context.Context, raw json.RawMessage) (any, *rpc.Error) {
	if err := protocol.ValidateEmptyParams(raw); err != nil {
		return nil, invalidParams()
	}
	if err := r.shutdownRuntime(ctx); err != nil {
		return nil, callbackError(err)
	}
	return struct{}{}, nil
}
func (r *runtime) shutdownRuntime(ctx context.Context) error {
	r.shutdownOnce.Do(func() {
		r.shutdownDone = make(chan struct{})
		go func() {
			r.callbackMu.Lock()
			defer r.callbackMu.Unlock()
			r.stateMu.Lock()
			handler := r.handler
			r.stateMu.Unlock()
			if shutdowner, ok := handler.(Shutdowner); ok {
				r.shutdownErr = shutdowner.Shutdown(ctx)
			}
			close(r.shutdownDone)
		}()
	})
	select {
	case <-r.shutdownDone:
		return r.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *runtime) decodeInitialized(raw json.RawMessage, target interface{ Validate() error }) *rpc.Error {
	if rpcErr := r.requireInitialized(); rpcErr != nil {
		return rpcErr
	}
	if err := decodeAndValidate(raw, target); err != nil {
		return invalidParams()
	}
	return nil
}
func (r *runtime) requireInitialized() *rpc.Error {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if !r.initialized {
		return domainRPCError(protocol.ErrorNotReady)
	}
	return nil
}

func validateDefinition(definition Definition) error {
	if definition.ID == "" || definition.Version == "" || definition.New == nil {
		return errors.New("plugin definition requires id, version, and handler factory")
	}
	result := protocol.InitializeResult{
		PluginID: definition.ID, PluginVersion: definition.Version, ProtocolVersion: protocol.Version,
		ExecutionModes: definition.Contract.ExecutionModes, Channels: definition.Contract.Channels,
		Operations: definition.Contract.Operations,
	}
	if err := result.Validate(); err != nil {
		return fmt.Errorf("plugin contract: %w", err)
	}
	if len(definition.Contract.ExecutionModes) == 0 {
		return errors.New("plugin contract requires at least one execution mode")
	}
	return nil
}
func validateHandlerContract(contract Contract, handler Plugin) error {
	for _, mode := range contract.ExecutionModes {
		switch mode {
		case protocol.ExecutionModeInteractive:
			if _, ok := handler.(SessionHandler); !ok {
				return errors.New("interactive contract requires SessionHandler")
			}
		case protocol.ExecutionModeResident:
			if _, ok := handler.(Shutdowner); !ok {
				return errors.New("resident contract requires Shutdowner")
			}
		}
	}
	if len(contract.Operations) > 0 {
		if _, ok := handler.(OperationHandler); !ok {
			return errors.New("operations require OperationHandler")
		}
	}
	return nil
}
func operationDeclared(operations []protocol.OperationDescriptor, id string) bool {
	for _, operation := range operations {
		if operation.ID == id {
			return true
		}
	}
	return false
}
func decodeAndValidate(raw json.RawMessage, target interface{ Validate() error }) error {
	if err := protocol.DecodeStrict(raw, target); err != nil {
		return err
	}
	return target.Validate()
}
func invalidParams() *rpc.Error    { return &rpc.Error{Code: -32602, Message: "invalid params"} }
func internalRPCError() *rpc.Error { return &rpc.Error{Code: -32603, Message: "internal error"} }
func callbackError(err error) *rpc.Error {
	if IsPermanentConfiguration(err) {
		return domainRPCError(protocol.ErrorInvalidArgument)
	}
	if domain, ok := errors.AsType[*protocol.DomainError](err); ok {
		return domainRPCError(domain.Kind())
	}
	return internalRPCError()
}
func domainRPCError(kind protocol.ErrorKind) *rpc.Error {
	if (protocol.ErrorData{Kind: kind}).Validate() != nil {
		return internalRPCError()
	}
	data, _ := json.Marshal(protocol.ErrorData{Kind: kind})
	return &rpc.Error{Code: protocol.DomainErrorCode, Message: "bsbctl request failed", Data: data}
}
func translateRemoteError(peer *rpc.Peer, err error) error {
	if err == nil {
		return nil
	}
	remote, ok := errors.AsType[*rpc.Error](err)
	if !ok {
		return err
	}
	kind, domain, decodeErr := protocol.DecodeRemoteError(remote.Code, remote.Data)
	if decodeErr != nil {
		peer.TerminateProtocol(nil)
		return rpc.ErrProtocol
	}
	if domain {
		return protocol.NewDomainError(kind, remote)
	}
	return err
}
