package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
)

// Call sends a request and waits for its correlated response.
func (p *Peer) Call(ctx context.Context, method string, params any, result any) error {
	if ctx == nil {
		return errors.New("json-rpc call context is required")
	}
	if method == "" {
		return errors.New("json-rpc method is required")
	}
	paramsJSON, err := marshalOptional(params)
	if err != nil {
		return fmt.Errorf("marshal %s params: %w", method, err)
	}
	select {
	case p.callGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.Join(ErrClosed, p.terminalError())
	}
	if err := ctx.Err(); err != nil {
		<-p.callGate
		return err
	}
	idNumber := p.nextID.Add(1)
	id := strconv.FormatUint(idNumber, 10)
	idJSON, _ := json.Marshal(id)
	response := make(chan message, 1)
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		<-p.callGate
		return errors.Join(ErrClosed, p.err)
	}
	p.pending[id] = response
	p.mu.Unlock()
	request := message{JSONRPC: Version, ID: idJSON, Method: method, Params: paramsJSON}
	if deadline, ok := ctx.Deadline(); ok {
		request.Metadata = &metadata{DeadlineUnixMilliseconds: deadline.UnixMilli()}
	}
	if err := p.write(ctx, request); err != nil {
		p.removePending(id)
		p.nextID.Store(idNumber - 1)
		<-p.callGate
		return err
	}
	p.highestWrittenID.Store(idNumber)
	<-p.callGate
	select {
	case msg := <-response:
		p.removePending(id)
		if msg.Error != nil {
			return msg.Error
		}
		if result == nil {
			return nil
		}
		if len(msg.Result) == 0 {
			return fmt.Errorf("decode %s result: result is required", method)
		}
		if err := json.Unmarshal(msg.Result, result); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		p.removePending(id)
		cancelCtx, cancel := context.WithTimeout(context.Background(), handlerWriteTimeout)
		err := p.Notify(cancelCtx, "rpc.cancel", cancelParams{ID: id})
		cancel()
		if err != nil {
			p.finish(errors.Join(ErrProtocol, fmt.Errorf("write rpc.cancel for request %s: %w", id, err)))
		}
		return errors.Join(ErrOutcomeUnknown, ctx.Err())
	case <-p.done:
		p.removePending(id)
		return errors.Join(ErrOutcomeUnknown, ErrClosed, p.terminalError())
	}
}

// CallEmpty sends a request whose successful response is exactly an empty
// JSON object. It rejects null, scalars, arrays, and objects with fields.
func (p *Peer) CallEmpty(ctx context.Context, method string, params any) error {
	var result emptyResult
	return p.Call(ctx, method, params, &result)
}

type emptyResult struct{}

func (*emptyResult) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil || len(fields) != 0 {
		return errors.New("empty response must be an object without fields")
	}
	return nil
}

// Notify sends a critical notification and waits for its write to complete.
func (p *Peer) Notify(ctx context.Context, method string, params any) error {
	if method == "" {
		return errors.New("json-rpc method is required")
	}
	paramsJSON, err := marshalOptional(params)
	if err != nil {
		return fmt.Errorf("marshal %s params: %w", method, err)
	}
	return p.write(ctx, message{JSONRPC: Version, Method: method, Params: paramsJSON})
}

// TryNotifyLossy attempts to enqueue a noncritical notification without blocking.
// A false result with a nil error means the bounded outbound queue was full.
func (p *Peer) TryNotifyLossy(method string, params any) (bool, error) {
	if method == "" {
		return false, errors.New("json-rpc method is required")
	}
	paramsJSON, err := marshalOptional(params)
	if err != nil {
		return false, fmt.Errorf("marshal %s params: %w", method, err)
	}
	data, err := marshalMessage(message{JSONRPC: Version, Method: method, Params: paramsJSON})
	if err != nil {
		return false, err
	}
	request := outboundWrite{
		ctx: context.Background(), data: data, state: new(atomic.Uint32), timeout: handlerWriteTimeout,
	}
	select {
	case <-p.done:
		return false, errors.Join(ErrClosed, p.terminalError())
	default:
	}
	select {
	case p.writes <- request:
		return true, nil
	default:
		return false, nil
	}
}
