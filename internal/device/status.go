package device

import (
	"context"
	"errors"
	"time"

	"github.com/lxdb/busylib-go/proto/inputpb"
	publicstream "github.com/lxdb/busylib-go/stream"
)

type StatusStreamFactory func() (publicstream.Stream, error)

const (
	streamCreateErrorCode   = "status_stream_create_failed"
	streamStartErrorCode    = "status_stream_start_failed"
	streamStatusErrorCode   = "status_stream_status_error"
	streamTerminalErrorCode = "status_stream_terminal"
	streamClosedErrorCode   = "status_stream_closed"
)

// StatusSubscriber continuously drains the one-shot busylib status stream and
// recreates it after terminal failure. Draining is immediate so firmware input
// cannot be blocked by rendering or plugin work.
type StatusSubscriber struct {
	factory   StatusStreamFactory
	submit    func(*inputpb.InputEvent) bool
	backoff   time.Duration
	connected func()
	observer  StreamHealthObserver
}

type StatusSubscriberOptions struct {
	Factory     StatusStreamFactory
	Submit      func(*inputpb.InputEvent) bool
	Backoff     time.Duration
	OnConnected func()
	Observer    StreamHealthObserver
}

func NewStatusSubscriber(options StatusSubscriberOptions) (*StatusSubscriber, error) {
	if options.Factory == nil {
		return nil, errors.New("status stream factory is required")
	}
	if options.Submit == nil {
		return nil, errors.New("status input handler is required")
	}
	if options.OnConnected == nil {
		return nil, errors.New("status connection handler is required")
	}
	if options.Observer == nil {
		return nil, errors.New("status health observer is required")
	}
	if options.Backoff < 0 {
		options.Backoff = 0
	}
	return &StatusSubscriber{
		factory: options.Factory, submit: options.Submit, backoff: options.Backoff,
		connected: options.OnConnected, observer: options.Observer,
	}, nil
}

func (s *StatusSubscriber) Run(ctx context.Context) error {
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return nil
		}
		s.observe(StreamHealth{Phase: StreamCreating, Attempt: attempt})
		statusStream, err := s.factory()
		if err != nil {
			s.observe(StreamHealth{Phase: StreamTerminal, Attempt: attempt, ErrorCode: streamCreateErrorCode})
			if !s.waitBackoff(ctx, attempt, streamCreateErrorCode) {
				return nil
			}
			continue
		}
		s.observe(StreamHealth{Phase: StreamStarting, Attempt: attempt})
		if err := statusStream.Start(ctx); err != nil {
			_ = statusStream.Stop()
			if ctx.Err() != nil {
				return nil
			}
			s.observe(StreamHealth{Phase: StreamTerminal, Attempt: attempt, ErrorCode: streamStartErrorCode})
			if !s.waitBackoff(ctx, attempt, streamStartErrorCode) {
				return nil
			}
			continue
		}
		s.connected()
		s.observeStatus(statusStream.Status(), attempt)
		terminalCode := s.drain(ctx, statusStream, attempt)
		_ = statusStream.Stop()
		if ctx.Err() != nil {
			return nil
		}
		if terminalCode == "" {
			terminalCode = streamClosedErrorCode
			s.observe(StreamHealth{Phase: StreamTerminal, Attempt: attempt, ErrorCode: terminalCode})
		}
		if !s.waitBackoff(ctx, attempt, terminalCode) {
			return nil
		}
	}
}

func (s *StatusSubscriber) drain(ctx context.Context, statusStream publicstream.Stream, attempt int) string {
	messages := statusStream.Messages()
	statuses := statusStream.Statuses()
	for messages != nil || statuses != nil {
		select {
		case <-ctx.Done():
			return ""
		case message, ok := <-messages:
			if !ok {
				messages = nil
				continue
			}
			for _, update := range message.Updates {
				input, isInput := update.(publicstream.InputUpdate)
				if isInput && input.Value != nil {
					s.submit(input.Value)
				}
			}
		case status, ok := <-statuses:
			if !ok {
				statuses = nil
				continue
			}
			s.observeStatus(status, attempt)
		}
	}
	if err := statusStream.Wait(); err != nil {
		s.observe(StreamHealth{Phase: StreamTerminal, Attempt: attempt, ErrorCode: streamTerminalErrorCode})
		return streamTerminalErrorCode
	}
	return ""
}

func (s *StatusSubscriber) observeStatus(status publicstream.Status, fallbackAttempt int) {
	attempt := status.Attempt
	if attempt == 0 {
		attempt = fallbackAttempt
	}
	code := ""
	if status.LastError != nil {
		code = streamStatusErrorCode
	}
	s.observe(StreamHealth{
		Phase: StreamStatusTransition, Lifecycle: status.Lifecycle, Access: status.Access,
		Attempt: attempt, ConnectedAt: status.ConnectedAt, LastStateAt: status.LastStateAt, ErrorCode: code,
	})
}

func (s *StatusSubscriber) observe(update StreamHealth) {
	s.observer.ObserveStatusStream(update)
}

func (s *StatusSubscriber) waitBackoff(ctx context.Context, attempt int, code string) bool {
	retryAt := time.Now().UTC().Add(s.backoff)
	s.observe(StreamHealth{Phase: StreamBackoff, Attempt: attempt, RetryAt: retryAt, ErrorCode: code})
	return waitBackoff(ctx, s.backoff)
}

func waitBackoff(ctx context.Context, duration time.Duration) bool {
	if duration == 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
