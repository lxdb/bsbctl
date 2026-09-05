package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
)

// NewPeer takes ownership of an established connection and closes it on shutdown.
// The connection must honor net.Conn's deadline and Close semantics so blocked
// I/O can be interrupted. Callers must not read, write, or set deadlines on conn
// after handing it to the peer.
func NewPeer(conn net.Conn) *Peer {
	p := &Peer{
		conn: conn, handlers: make(map[string]Handler), lossyMethods: make(map[string]struct{}),
		pending: make(map[string]chan message), inflight: make(map[string]context.CancelFunc),
		callGate: make(chan struct{}, 1),
		done:     make(chan struct{}), writes: make(chan outboundWrite, outboundQueueCapacity),
		writerDone: make(chan struct{}), methods: make(chan inboundMethod, inboundQueueCapacity),
		methodDone: make(chan struct{}), handlerSlots: make(chan struct{}, maxHandlers),
	}
	go p.writer()
	go p.methodDispatcher()
	return p
}

// Handle registers a method before Serve begins.
func (p *Peer) Handle(method string, handler Handler) error {
	return p.handle(method, handler, false)
}

// HandleLossyNotification registers a method whose notifications may be
// dropped, and counted, when the inbound method queue is saturated.
func (p *Peer) HandleLossyNotification(method string, handler Handler) error {
	return p.handle(method, handler, true)
}

func (p *Peer) handle(method string, handler Handler, lossy bool) error {
	if method == "" || handler == nil {
		return errors.New("method and handler are required")
	}
	if method == "rpc.cancel" {
		return errors.New("rpc.cancel is reserved by the transport")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.handlers[method]; exists {
		return fmt.Errorf("method %q is already registered", method)
	}
	p.handlers[method] = handler
	if lossy {
		p.lossyMethods[method] = struct{}{}
	}
	return nil
}

// Serve reads and dispatches messages until the connection, context, or protocol fails.
func (p *Peer) Serve(ctx context.Context) error {
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Close()
		case <-p.done:
		}
	}()

	scanner := bufio.NewScanner(p.conn)
	scanner.Buffer(make([]byte, 4096), MaxMessageBytes+1)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if len(line) > MaxMessageBytes {
			p.finish(errors.New("json-rpc message exceeds 1 MiB"))
			return p.terminalError()
		}
		msg, err := decodeMessage(line)
		if err != nil {
			if looksLikeResponse(line) {
				p.finish(errors.Join(ErrProtocol, err))
				return p.terminalError()
			}
			code, text := -32600, "invalid request"
			if !json.Valid(line) {
				code, text = -32700, "parse error"
			}
			p.writeHandlerResponse(ctx, message{JSONRPC: Version, ID: json.RawMessage("null"), Error: &Error{Code: code, Message: text}})
			continue
		}
		if msg.Method != "" {
			p.dispatch(ctx, msg)
			continue
		}
		if err := p.deliver(msg); err != nil {
			p.finish(err)
			return p.terminalError()
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	p.finish(err)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return p.terminalError()
}

// Close terminates the connection and unblocks all calls. It is safe to repeat.
// It joins the transport workers without waiting for active handlers to return.
func (p *Peer) Close() error {
	p.finish(ErrClosed)
	<-p.writerDone
	<-p.methodDone
	return nil
}

// TerminateProtocol marks the peer unusable without waiting for active
// handlers to join. It is safe to call from a reentrant handler.
func (p *Peer) TerminateProtocol(cause error) {
	p.finish(errors.Join(ErrProtocol, cause))
}

// Done closes when the peer terminates.
func (p *Peer) Done() <-chan struct{} { return p.done }

func (p *Peer) finish(err error) {
	p.once.Do(func() {
		p.mu.Lock()
		p.err = err
		for _, cancel := range p.inflight {
			cancel()
		}
		clear(p.inflight)
		p.mu.Unlock()
		close(p.done)
		_ = p.conn.Close()
	})
}

func (p *Peer) terminalError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}
