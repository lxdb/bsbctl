package pluginhost

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

type command interface {
	response() chan error
	commandContext() context.Context
}

type applyCommand struct {
	ctx   context.Context
	spec  Spec
	reply chan error
}

func (c applyCommand) response() chan error            { return c.reply }
func (c applyCommand) commandContext() context.Context { return c.ctx }

type invokeCommand struct {
	ctx     context.Context
	request InvokeRequest
	kind    InvocationKind
	token   SessionToken
	reply   chan error
}

func (c invokeCommand) response() chan error            { return c.reply }
func (c invokeCommand) commandContext() context.Context { return c.ctx }

type endSessionCommand struct {
	ctx    context.Context
	target protocol.InstanceRef
	token  SessionToken
	reply  chan error
}

func (c endSessionCommand) response() chan error            { return c.reply }
func (c endSessionCommand) commandContext() context.Context { return c.ctx }

type completeSessionCommand struct {
	ctx    context.Context
	target protocol.InstanceRef
	token  SessionToken
	reply  chan error
}

func (c completeSessionCommand) response() chan error            { return c.reply }
func (c completeSessionCommand) commandContext() context.Context { return c.ctx }

type restartCommand struct {
	ctx   context.Context
	reply chan error
}

func (c restartCommand) response() chan error            { return c.reply }
func (c restartCommand) commandContext() context.Context { return c.ctx }

type sessionInputCommand struct {
	ctx     context.Context
	request protocol.SessionInputRequest
	result  *protocol.SessionInputResult
	reply   chan error
}

func (c sessionInputCommand) response() chan error            { return c.reply }
func (c sessionInputCommand) commandContext() context.Context { return c.ctx }

type operationCommand struct {
	ctx     context.Context
	request protocol.OperationRequest
	result  *protocol.OperationResult
	reply   chan error
}

func (c operationCommand) response() chan error            { return c.reply }
func (c operationCommand) commandContext() context.Context { return c.ctx }

type exitCommand struct {
	runID uint64
	err   error
	reply chan error
}

type postStopAction uint8

const (
	postStopNone postStopAction = iota
	postStopRestart
	postStopRetry
)

func (c exitCommand) response() chan error            { return c.reply }
func (c exitCommand) commandContext() context.Context { return nil }

type supervisor struct {
	manager           *Manager
	id                string
	commands          chan command
	completions       chan completeSessionCommand
	shutdown          chan struct{}
	done              chan struct{}
	joined            chan struct{}
	shutdownOnce      sync.Once
	cleanupOnce       sync.Once
	shutdownRequested atomic.Bool
	errorReported     atomic.Bool
	observers         sync.WaitGroup
	statusMu          sync.Mutex
	status            PluginStatus
	exits             []time.Time
	desired           bool
	spec              Spec
	child             Child
	childSpec         Spec
	childReaped       <-chan struct{}
	stopping          bool
	postStop          postStopAction
	runID             uint64
	sessions          map[string]interactiveSession
	pendingCleanups   []pendingSessionCleanup
	phase             Phase
	healthy           bool
	healthMisses      int
	healthTimer       Timer
	retryTimer        Timer
	retryAt           time.Time
	retryAttempt      int
	lastErrorCode     string
	lastErrorAt       time.Time
	sessionErrorCode  string
	sessionErrorAt    time.Time
	shutdownErrMu     sync.Mutex
	shutdownErr       error
}

type interactiveSession struct {
	generation uint64
	token      SessionToken
}

type pendingSessionCleanup struct {
	instanceID string
	generation uint64
	token      SessionToken
}

func newSupervisor(manager *Manager, id string) *supervisor {
	current := &supervisor{manager: manager, id: id, commands: make(chan command, supervisorQueueCapacity), completions: make(chan completeSessionCommand, supervisorQueueCapacity), shutdown: make(chan struct{}), done: make(chan struct{}), joined: make(chan struct{}), sessions: make(map[string]interactiveSession), phase: PhaseStopped}
	current.status = PluginStatus{ID: id, Phase: PhaseStopped}
	go current.run()
	return current
}

func (s *supervisor) call(ctx context.Context, value command) error {
	reply := make(chan error, 1)
	switch current := value.(type) {
	case applyCommand:
		current.reply = reply
		value = current
	case invokeCommand:
		current.reply = reply
		value = current
	case endSessionCommand:
		current.reply = reply
		value = current
	case completeSessionCommand:
		current.reply = reply
		value = current
	case restartCommand:
		current.reply = reply
		value = current
	case sessionInputCommand:
		current.reply = reply
		value = current
	case operationCommand:
		current.reply = reply
		value = current
	}
	select {
	case s.commands <- value:
	case <-s.done:
		return ErrPluginNotConfigured
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-s.done:
		return ErrPluginNotConfigured
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *supervisor) admitCompletion(ctx context.Context, value completeSessionCommand) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.completions <- value:
		return nil
	case <-s.done:
		return ErrPluginNotConfigured
	default:
		return ErrPluginUnavailable
	}
}

func (s *supervisor) join(ctx context.Context) error {
	select {
	case <-s.joined:
		return s.shutdownError()
	case <-ctx.Done():
		return errors.Join(ctx.Err(), s.shutdownError())
	}
}

func (s *supervisor) requestShutdown() {
	s.shutdownOnce.Do(func() {
		s.shutdownRequested.Store(true)
		close(s.shutdown)
		go func() {
			<-s.done
			s.observers.Wait()
			close(s.joined)
		}()
	})
}

func (s *supervisor) shutdownError() error {
	s.shutdownErrMu.Lock()
	defer s.shutdownErrMu.Unlock()
	return s.shutdownErr
}

func (s *supervisor) run() {
	defer close(s.done)
	for {
		select {
		case <-s.shutdown:
			s.shutdownNow()
			return
		default:
		}
		select {
		case completion := <-s.completions:
			s.handleCompletion(completion)
			continue
		default:
		}
		var health, retry <-chan time.Time
		if s.healthTimer != nil {
			health = s.healthTimer.C()
		}
		if s.retryTimer != nil {
			retry = s.retryTimer.C()
		}
		select {
		case <-s.shutdown:
			s.shutdownNow()
			return
		case <-health:
			s.healthTimer = nil
			s.handleHealth()
		case <-retry:
			s.retryTimer = nil
			s.handleRetry()
		case completion := <-s.completions:
			s.handleCompletion(completion)
		case value := <-s.commands:
			if commandCtx := value.commandContext(); commandCtx != nil {
				if err := commandCtx.Err(); err != nil {
					if _, isEndSession := value.(endSessionCommand); !isEndSession {
						if reply := value.response(); reply != nil {
							reply <- err
						}
						continue
					}
				}
			}
			err, stop := s.handle(value)
			if reply := value.response(); reply != nil {
				reply <- err
			}
			if stop {
				return
			}
		}
	}
}

func (s *supervisor) handle(value command) (error, bool) {
	switch current := value.(type) {
	case applyCommand:
		return s.apply(current.ctx, current.spec), false
	case invokeCommand:
		return s.invoke(current.ctx, current.request, current.kind, current.token), false
	case endSessionCommand:
		return s.endSession(current.ctx, current.target, current.token), false
	case completeSessionCommand:
		return s.handleCompletion(current), false
	case restartCommand:
		return s.restart(current.ctx), false
	case sessionInputCommand:
		result, err := s.sessionInput(current.ctx, current.request)
		if current.result != nil {
			*current.result = result
		}
		return err, false
	case operationCommand:
		result, err := s.operation(current.ctx, current.request)
		if current.result != nil {
			*current.result = result
		}
		return err, false
	case exitCommand:
		s.exited(current)
		return nil, false
	default:
		return errors.New("unsupported supervisor command"), false
	}
}

func (s *supervisor) shutdownNow() {
	s.desired = false
	s.clearPostStop()
	s.invalidateSessions(SessionInvalidatedDisabled)
	s.cancelTimers()
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	err := s.stopCurrent(ctx)
	cancel()
	s.shutdownErrMu.Lock()
	s.shutdownErr = err
	s.shutdownErrMu.Unlock()
	if s.stopping && s.childReaped != nil {
		<-s.childReaped
		s.finishStop()
	}
	s.phase = PhaseStopped
	s.retryAt = time.Time{}
	s.publish()
}
