package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

func (p *Peer) dispatch(ctx context.Context, msg message) {
	if msg.Method == "rpc.cancel" {
		if msg.hasID {
			p.writeHandlerResponse(ctx, message{JSONRPC: Version, ID: msg.ID, Error: &Error{Code: -32600, Message: "invalid request"}})
			return
		}
		var request cancelParams
		if err := decodeStrict(msg.Params, &request); err != nil {
			p.finish(errors.Join(ErrProtocol, errors.New("invalid rpc.cancel notification")))
			return
		}
		idJSON, _ := json.Marshal(request.ID)
		if _, err := requestID(idJSON); err != nil {
			p.finish(errors.Join(ErrProtocol, errors.New("invalid rpc.cancel notification")))
			return
		}
		p.mu.Lock()
		cancel := p.inflight[request.ID]
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}

	handlerCtx := ctx
	cancel := func() {}
	if msg.Metadata != nil && msg.Metadata.DeadlineUnixMilliseconds > 0 {
		remoteDeadline := time.UnixMilli(msg.Metadata.DeadlineUnixMilliseconds)
		if localDeadline, ok := ctx.Deadline(); !ok || remoteDeadline.Before(localDeadline) {
			handlerCtx, cancel = context.WithDeadline(ctx, remoteDeadline)
		} else {
			handlerCtx, cancel = context.WithCancel(ctx)
		}
	} else {
		handlerCtx, cancel = context.WithCancel(ctx)
	}
	if msg.hasID {
		id, _ := requestID(msg.ID)
		p.mu.Lock()
		_, duplicate := p.inflight[id]
		if !duplicate {
			p.inflight[id] = cancel
		}
		p.mu.Unlock()
		if duplicate {
			cancel()
			p.finish(errors.Join(ErrProtocol, fmt.Errorf("duplicate in-flight request id %q", id)))
			return
		}
	}
	method := inboundMethod{ctx: handlerCtx, writeCtx: ctx, msg: msg, cancel: cancel}
	select {
	case p.methods <- method:
	case <-handlerCtx.Done():
		p.finishInbound(method)
		return
	case <-p.done:
		p.finishInbound(method)
		return
	default:
		p.finishInbound(method)
		if len(msg.ID) == 0 {
			if p.isLossyMethod(msg.Method) {
				p.droppedInbound.Add(1)
				return
			}
			p.finish(ErrInboundSaturated)
			return
		}
		p.writeHandlerResponse(ctx, message{
			JSONRPC: Version, ID: msg.ID,
			Error: &Error{Code: -32603, Message: "internal error"},
		})
		return
	}
}

func (p *Peer) isLossyMethod(method string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, lossy := p.lossyMethods[method]
	return lossy
}

func (p *Peer) methodDispatcher() {
	defer close(p.methodDone)
	for {
		select {
		case method := <-p.methods:
			select {
			case p.handlerSlots <- struct{}{}:
				go p.handleMethod(method)
			case <-method.ctx.Done():
				p.finishInbound(method)
			case <-p.done:
				p.finishInbound(method)
				return
			}
		case <-p.done:
			return
		}
	}
}

func (p *Peer) handleMethod(method inboundMethod) {
	defer func() { <-p.handlerSlots }()
	retireInbound := sync.OnceFunc(func() { p.retireInbound(method) })
	defer method.cancel()
	defer retireInbound()
	ctx, msg := method.ctx, method.msg
	p.mu.Lock()
	handler := p.handlers[msg.Method]
	p.mu.Unlock()
	if handler == nil {
		if len(msg.ID) != 0 {
			retireInbound()
			p.writeHandlerResponse(method.writeCtx, message{JSONRPC: Version, ID: msg.ID, Error: &Error{Code: -32601, Message: "method not found"}})
		}
		return
	}
	result, rpcErr := handler(ctx, msg.Params)
	if len(msg.ID) == 0 {
		return
	}
	if ctx.Err() != nil {
		return
	}
	response := message{JSONRPC: Version, ID: msg.ID, Error: rpcErr}
	if rpcErr == nil {
		encoded, err := json.Marshal(result)
		if err != nil {
			response.Error = &Error{Code: -32603, Message: "internal error"}
		} else {
			response.Result = encoded
		}
	}
	retireInbound()
	p.writeHandlerResponse(method.writeCtx, response)
}

func (p *Peer) finishInbound(method inboundMethod) {
	method.cancel()
	p.retireInbound(method)
}

func (p *Peer) retireInbound(method inboundMethod) {
	if !method.msg.hasID {
		return
	}
	var id string
	_ = json.Unmarshal(method.msg.ID, &id)
	p.mu.Lock()
	delete(p.inflight, id)
	p.mu.Unlock()
}

func (p *Peer) deliver(msg message) error {
	var id string
	_ = json.Unmarshal(msg.ID, &id)
	p.mu.Lock()
	response := p.pending[id]
	p.mu.Unlock()
	if response != nil {
		select {
		case response <- msg:
		default:
		}
		return nil
	}
	written, err := strconv.ParseUint(id, 10, 64)
	if err == nil && written > 0 && strconv.FormatUint(written, 10) == id && written <= p.highestWrittenID.Load() {
		return nil
	}
	return errors.Join(ErrProtocol, fmt.Errorf("unsolicited response id %q", id))
}

func (p *Peer) removePending(id string) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}
