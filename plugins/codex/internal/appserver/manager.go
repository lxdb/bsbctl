package appserver

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type ReadWriteCloser interface {
	io.Reader
	io.Writer
	io.Closer
}

type Connector interface {
	Connect(context.Context) (ReadWriteCloser, error)
}

type ManagerEventKind uint8

const (
	ManagerConnected ManagerEventKind = iota + 1
	ManagerDisconnected
	ManagerIncoming
	ManagerThreadsReconciled
	ManagerThreadAttached
	ManagerThreadAttachFailed
	ManagerRateLimitsSnapshot
	ManagerRateLimitsReadFailed
)

type ManagerEvent struct {
	Kind         ManagerEventKind
	Connection   Connection
	Incoming     Incoming
	Initialize   InitializeResponse
	ThreadIDs    []string
	ThreadID     string
	Thread       *ThreadSnapshot
	FailureStage string
	FailureCode  string
	RateLimits   *RateLimitSnapshot
}

type ManagerOptions struct {
	PollInterval           time.Duration
	Backoff                []time.Duration
	Session                Options
	ShutdownTimeout        time.Duration
	RateLimitsEnabled      bool
	RateLimitsPollInterval time.Duration
}

type Manager struct {
	connector Connector
	opts      ManagerOptions
}

func NewManager(connector Connector, opts ManagerOptions) *Manager {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 15 * time.Second
	}
	if len(opts.Backoff) == 0 {
		opts.Backoff = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = time.Second
	}
	if opts.RateLimitsPollInterval <= 0 {
		opts.RateLimitsPollInterval = 2 * time.Minute
	}
	return &Manager{connector: connector, opts: opts}
}

func (m *Manager) Run(ctx context.Context, events chan<- ManagerEvent) error {
	backoffIndex := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		connection, err := m.connector.Connect(ctx)
		if err != nil {
			if !sendManagerEvent(ctx, events, disconnectedEvent("connect", err)) {
				return ctx.Err()
			}
			if err := waitBackoff(ctx, m.opts.Backoff, &backoffIndex); err != nil {
				return err
			}
			continue
		}

		backoffIndex = 0
		err = m.runConnection(ctx, connection, events)
		_ = connection.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !sendManagerEvent(ctx, events, disconnectedEvent("session", err)) {
			return ctx.Err()
		}
		if err := waitBackoff(ctx, m.opts.Backoff, &backoffIndex); err != nil {
			return err
		}
	}
}

func (m *Manager) runConnection(parent context.Context, connection ReadWriteCloser, events chan<- ManagerEvent) error {
	// The manager closes the session after it attempts clean unsubscription.
	sessionCtx, cancelSession := context.WithCancel(context.WithoutCancel(parent))
	defer cancelSession()
	session := NewSession(connection, m.opts.Session)
	defer session.Close()
	session.Start(sessionCtx)

	initialized, err := session.Initialize(parent, m.opts.RateLimitsEnabled)
	if err != nil {
		return &connectionFailure{stage: "initialize", err: err}
	}
	if !sendManagerEvent(parent, events, ManagerEvent{Kind: ManagerConnected, Connection: Connection{session: session}, Initialize: initialized}) {
		return parent.Err()
	}
	pump := startEventPump(parent, session, events)
	attached := make(map[string]struct{})
	defer func() {
		pump.stop()
		if parent.Err() != nil {
			m.unsubscribeBestEffort(session, attached)
		}
	}()
	readRateLimits := func(ctx context.Context, emit func(ManagerEvent) error) error {
		response, err := session.ReadRateLimits(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return emit(ManagerEvent{
				Kind: ManagerRateLimitsReadFailed, FailureStage: "rate_limits", FailureCode: safeFailureCode(err),
			})
		}
		snapshot, ok := response.CodexSnapshot()
		if !ok {
			return emit(ManagerEvent{
				Kind: ManagerRateLimitsReadFailed, FailureStage: "rate_limits", FailureCode: "windows_unavailable",
			})
		}
		return emit(ManagerEvent{Kind: ManagerRateLimitsSnapshot, RateLimits: &snapshot})
	}

	reconcile := func(ctx context.Context, emit func(ManagerEvent) error) error {
		loaded, err := session.ListLoadedThreads(ctx)
		if err != nil {
			return &connectionFailure{stage: "thread_list", err: err}
		}
		current := make(map[string]struct{}, len(loaded))
		for _, threadID := range loaded {
			current[threadID] = struct{}{}
			if _, exists := attached[threadID]; exists {
				continue
			}
			snapshot, err := session.ResumeThreadSnapshot(ctx, threadID)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err := emit(ManagerEvent{
					Kind: ManagerThreadAttachFailed, ThreadID: threadID,
					FailureStage: "thread_resume", FailureCode: safeFailureCode(err),
				}); err != nil {
					return err
				}
				continue
			}
			if snapshot.ID == "" {
				snapshot.ID = threadID
			}
			attached[threadID] = struct{}{}
			copy := snapshot
			if err := emit(ManagerEvent{Kind: ManagerThreadAttached, Thread: &copy}); err != nil {
				return err
			}
		}
		for threadID := range attached {
			if _, exists := current[threadID]; !exists {
				delete(attached, threadID)
			}
		}
		return emit(ManagerEvent{Kind: ManagerThreadsReconciled, ThreadIDs: slices.Clone(loaded)})
	}
	if err := pump.phase(parent, func(ctx context.Context, emit func(ManagerEvent) error) error {
		if m.opts.RateLimitsEnabled {
			if err := readRateLimits(ctx, emit); err != nil {
				return err
			}
		}
		return reconcile(ctx, emit)
	}); err != nil {
		return err
	}

	ticker := time.NewTicker(m.opts.PollInterval)
	defer ticker.Stop()
	var rateLimitsTicker *time.Ticker
	var rateLimitsTick <-chan time.Time
	if m.opts.RateLimitsEnabled {
		rateLimitsTicker = time.NewTicker(m.opts.RateLimitsPollInterval)
		rateLimitsTick = rateLimitsTicker.C
		defer rateLimitsTicker.Stop()
	}
	for {
		select {
		case <-parent.Done():
			return parent.Err()
		case <-session.done:
			return &connectionFailure{stage: "read", err: session.Wait()}
		case <-ticker.C:
			if err := pump.phase(parent, reconcile); err != nil {
				return err
			}
		case <-rateLimitsTick:
			if err := pump.phase(parent, readRateLimits); err != nil {
				return err
			}
		}
	}
}

type connectionFailure struct {
	stage string
	err   error
}

func (e *connectionFailure) Error() string { return e.stage + " failed" }
func (e *connectionFailure) Unwrap() error { return e.err }

func disconnectedEvent(defaultStage string, err error) ManagerEvent {
	stage := defaultStage
	if failure, ok := errors.AsType[*connectionFailure](err); ok {
		stage = failure.stage
	}
	return ManagerEvent{Kind: ManagerDisconnected, FailureStage: stage, FailureCode: safeFailureCode(err)}
}

func safeFailureCode(err error) string {
	if err == nil {
		return "closed"
	}
	if protocolErr, ok := errors.AsType[*ProtocolError](err); ok {
		return "protocol_" + string(protocolErr.Kind)
	}
	if rpcErr, ok := errors.AsType[*RPCError](err); ok {
		return "rpc_" + strconv.Itoa(rpcErr.Code)
	}
	if status := websocket.CloseStatus(err); status != -1 {
		return "websocket_" + strconv.Itoa(int(status))
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out"):
		return "timeout"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, io.ErrClosedPipe):
		return "closed_pipe"
	default:
		return "transport"
	}
}

func (m *Manager) Respond(ctx context.Context, id RawID, result any) error {
	session := id.connection.session
	if session == nil {
		return errors.New("app-server is disconnected")
	}
	return session.Respond(ctx, id, result)
}

func (m *Manager) Interrupt(ctx context.Context, connection Connection, threadID, turnID string) error {
	session := connection.session
	if session == nil {
		return errors.New("app-server is disconnected")
	}
	return session.InterruptTurn(ctx, threadID, turnID)
}

func (m *Manager) unsubscribeBestEffort(session *Session, attached map[string]struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), m.opts.ShutdownTimeout)
	defer cancel()
	stopForcedClose := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stopForcedClose()
	drainCtx, stopDrain := context.WithCancel(ctx)
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case <-drainCtx.Done():
				return
			case <-session.done:
				return
			case <-session.Events():
			}
		}
	}()
	defer func() {
		stopDrain()
		<-drainDone
	}()
	var unsubscribes sync.WaitGroup
	for threadID := range attached {
		unsubscribes.Go(func() {
			_ = session.UnsubscribeThread(ctx, threadID)
		})
	}
	unsubscribes.Wait()
}

func sendManagerEvent(ctx context.Context, target chan<- ManagerEvent, event ManagerEvent) bool {
	select {
	case target <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func waitBackoff(ctx context.Context, delays []time.Duration, index *int) error {
	delay := delays[min(*index, len(delays)-1)]
	if *index < len(delays)-1 {
		(*index)++
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
