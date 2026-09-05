package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"time"
)

func (p *Peer) write(ctx context.Context, msg message) error {
	if ctx == nil {
		return errors.New("json-rpc write context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := marshalMessage(msg)
	if err != nil {
		return err
	}
	request := outboundWrite{ctx: ctx, data: data, result: make(chan error, 1), state: new(atomic.Uint32)}
	select {
	case <-p.done:
		return errors.Join(ErrClosed, p.terminalError())
	default:
	}
	select {
	case p.writes <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.Join(ErrClosed, p.terminalError())
	}
	select {
	case err := <-request.result:
		if err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		if request.state.CompareAndSwap(outboundWriteQueued, outboundWriteCanceled) {
			p.finish(ctx.Err())
			return ctx.Err()
		}
		select {
		case err := <-request.result:
			return err
		case <-p.done:
			if err := ctx.Err(); err != nil {
				return writeOutcome(request, err)
			}
			return writeOutcome(request, errors.Join(ErrClosed, p.terminalError()))
		}
	case <-p.done:
		if err := ctx.Err(); err != nil {
			return writeOutcome(request, err)
		}
		return writeOutcome(request, errors.Join(ErrClosed, p.terminalError()))
	}
}

func writeOutcome(request outboundWrite, err error) error {
	if request.state.Load() == outboundWriteStarted {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	return err
}

func (p *Peer) writeHandlerResponse(ctx context.Context, msg message) {
	p.writeResponseWithin(ctx, msg, handlerWriteTimeout)
}

func (p *Peer) writeResponseWithin(ctx context.Context, msg message, timeout time.Duration) {
	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := p.write(writeCtx, msg)
	if errors.Is(err, errMessageTooLarge) {
		// Size rejection occurs before write admission. Return a bounded error
		// without implying that the handler's effect was not executed.
		err = p.write(writeCtx, message{JSONRPC: Version, ID: msg.ID, Error: &Error{Code: -32603, Message: "response exceeds transport limit"}})
	}
	if err != nil {
		p.finish(err)
	}
}

func (p *Peer) writer() {
	defer close(p.writerDone)
	for {
		select {
		case request := <-p.writes:
			if !request.state.CompareAndSwap(outboundWriteQueued, outboundWriteStarted) {
				continue
			}
			writeCtx := request.ctx
			cancel := func() {}
			if request.timeout > 0 {
				writeCtx, cancel = context.WithTimeout(writeCtx, request.timeout)
			}
			err := p.writeData(writeCtx, request.data)
			cancel()
			if request.result != nil {
				request.result <- err
			}
			if err != nil {
				p.finish(err)
				return
			}
		case <-p.done:
			return
		}
	}
}

func (p *Peer) writeData(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn := p.conn
	deadline := time.Time{}
	if value, ok := ctx.Deadline(); ok {
		deadline = value
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	writeFinished := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			_ = conn.SetWriteDeadline(time.Now())
		case <-writeFinished:
		}
	}()
	written, writeErr := conn.Write(data)
	close(writeFinished)
	<-watcherDone
	resetErr := conn.SetWriteDeadline(time.Time{})
	if written == len(data) && writeErr == nil {
		if resetErr != nil {
			return errors.Join(ErrOutcomeUnknown, resetErr)
		}
		return nil
	}
	if writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	err := errors.Join(writeErr, resetErr, ctx.Err())
	if written > 0 {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	return err
}

func marshalMessage(msg message) ([]byte, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxMessageBytes {
		return nil, errMessageTooLarge
	}
	return append(data, '\n'), nil
}
