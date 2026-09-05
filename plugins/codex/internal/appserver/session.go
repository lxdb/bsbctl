package appserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultMaxMessageBytes = 1 << 20
	defaultRPCTimeout      = 10 * time.Second
)

type IncomingKind uint8

const (
	IncomingNotification IncomingKind = iota + 1
	IncomingServerRequest
)

type Incoming struct {
	Kind   IncomingKind
	ID     RawID
	Method string
	Params json.RawMessage
}

// Connection binds an action to the exact app-server transport that supplied
// its target. Its zero value cannot authorize a write.
type Connection struct{ session *Session }

type RawID struct {
	value      json.RawMessage
	connection Connection
}

func (id RawID) Valid() bool { return len(id.value) != 0 }
func (id RawID) Key() string { return string(id.value) }

func (id RawID) Equal(other RawID) bool {
	return id.connection == other.connection && bytes.Equal(id.value, other.value)
}

func ParseRawID(raw json.RawMessage) (RawID, error) {
	if len(raw) == 0 || string(raw) == "null" || !validID(raw) {
		return RawID{}, errors.New("invalid JSON-RPC request id")
	}
	return RawID{value: append(json.RawMessage(nil), raw...)}, nil
}

type Options struct {
	MaxMessageBytes int
	RPCTimeout      time.Duration
	EventBuffer     int
}

type ProtocolErrorKind string

const (
	ProtocolMalformedJSON     ProtocolErrorKind = "malformed_json"
	ProtocolMessageTooLarge   ProtocolErrorKind = "message_too_large"
	ProtocolInvalidEnvelope   ProtocolErrorKind = "invalid_envelope"
	ProtocolEventBackpressure ProtocolErrorKind = "event_backpressure"
)

type ProtocolError struct{ Kind ProtocolErrorKind }

func (e *ProtocolError) Error() string { return "app-server protocol error: " + string(e.Kind) }

type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("app-server RPC error %d: %s", e.Code, e.Message)
}

type response struct {
	result json.RawMessage
	err    error
}

type Session struct {
	conn io.ReadWriteCloser
	opts Options

	nextID    atomic.Uint64
	writeGate chan struct{}

	pendingMu sync.Mutex
	pending   map[string]chan response

	events chan Incoming
	done   chan struct{}
	once   sync.Once
	errMu  sync.Mutex
	err    error
}

func NewSession(conn io.ReadWriteCloser, opts Options) *Session {
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = defaultMaxMessageBytes
	}
	if opts.RPCTimeout <= 0 {
		opts.RPCTimeout = defaultRPCTimeout
	}
	if opts.EventBuffer <= 0 {
		opts.EventBuffer = 64
	}
	return &Session{
		conn: conn, opts: opts, pending: make(map[string]chan response),
		writeGate: make(chan struct{}, 1),
		events:    make(chan Incoming, opts.EventBuffer), done: make(chan struct{}),
	}
}

func (s *Session) Start(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			s.finish(ctx.Err())
		case <-s.done:
		}
	}()
	go s.readLoop(ctx)
}

func (s *Session) Events() <-chan Incoming { return s.events }

func (s *Session) Wait() error {
	<-s.done
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *Session) Close() error {
	s.finish(nil)
	return nil
}

func (s *Session) Call(ctx context.Context, method string, params any, target any) error {
	ctx, cancel := context.WithTimeout(ctx, s.opts.RPCTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	id := s.nextID.Add(1)
	idBytes, _ := json.Marshal(id)
	key := string(idBytes)
	resultCh := make(chan response, 1)

	s.pendingMu.Lock()
	s.pending[key] = resultCh
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, key)
		s.pendingMu.Unlock()
	}()

	request := struct {
		ID     uint64 `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{ID: id, Method: method, Params: params}
	if err := s.writeJSON(ctx, request); err != nil {
		return fmt.Errorf("write %s request: %w", method, err)
	}

	select {
	case result := <-resultCh:
		return decodeCallResult(result, target)
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return decodeCallResult(<-resultCh, target)
	}
}

func decodeCallResult(result response, target any) error {
	if result.err != nil {
		return result.err
	}
	if target == nil || len(result.result) == 0 || string(result.result) == "null" {
		return nil
	}
	if err := json.Unmarshal(result.result, target); err != nil {
		return &ProtocolError{Kind: ProtocolMalformedJSON}
	}
	return nil
}

func (s *Session) Notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if params == nil {
		return s.writeJSON(ctx, struct {
			Method string `json:"method"`
		}{Method: method})
	}
	return s.writeJSON(ctx, struct {
		Method string `json:"method"`
		Params any    `json:"params"`
	}{Method: method, Params: params})
}

func (s *Session) Respond(ctx context.Context, id RawID, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !id.Valid() {
		return errors.New("server request id is missing")
	}
	if id.connection.session != s {
		return errors.New("server request belongs to a different connection")
	}
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal server response: %w", err)
	}
	if err := s.writeJSON(ctx, struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
	}{ID: id.value, Result: resultBytes}); err != nil {
		return fmt.Errorf("write server response: %w", err)
	}
	return nil
}

func (s *Session) writeJSON(ctx context.Context, value any) error {
	ctx, cancel := context.WithTimeout(ctx, s.opts.RPCTimeout)
	defer cancel()
	line, err := json.Marshal(value)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if len(line) > s.opts.MaxMessageBytes+1 {
		return &ProtocolError{Kind: ProtocolMessageTooLarge}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return io.ErrClosedPipe
	case s.writeGate <- struct{}{}:
	}
	defer func() { <-s.writeGate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.done:
		return io.ErrClosedPipe
	default:
	}
	// The session owns this stream. Closing it interrupts both a blocked pipe
	// write and the WebSocket adapter, whose io.Writer has no deadline API.
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		s.finish(ctx.Err())
		close(interrupted)
	})
	defer func() {
		if !stop() {
			<-interrupted
		}
	}()
	for len(line) != 0 {
		n, writeErr := s.conn.Write(line)
		if err := ctx.Err(); err != nil {
			return err
		}
		if writeErr != nil {
			s.finish(writeErr)
			return writeErr
		}
		if n == 0 {
			s.finish(io.ErrShortWrite)
			return io.ErrShortWrite
		}
		line = line[n:]
	}
	return nil
}

func (s *Session) readLoop(ctx context.Context) {
	bufferSize := 64 * 1024
	if s.opts.MaxMessageBytes < bufferSize {
		bufferSize = s.opts.MaxMessageBytes + 1
	}
	reader := bufio.NewReaderSize(s.conn, bufferSize)
	for {
		line, err := readBoundedLine(reader, s.opts.MaxMessageBytes)
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) == 0 {
				s.finish(io.EOF)
				return
			}
			s.finish(err)
			return
		}
		if err := s.handleLine(ctx, line); err != nil {
			s.finish(err)
			return
		}
	}
}

func readBoundedLine(reader *bufio.Reader, maximum int) ([]byte, error) {
	line := make([]byte, 0, min(maximum, 64*1024))
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > maximum+1 {
			return nil, &ProtocolError{Kind: ProtocolMessageTooLarge}
		}
		line = append(line, fragment...)
		if err == nil {
			return line[:len(line)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(line) != 0 {
			return nil, &ProtocolError{Kind: ProtocolMalformedJSON}
		}
		return line, err
	}
}

func (s *Session) handleLine(ctx context.Context, line []byte) error {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return &ProtocolError{Kind: ProtocolMalformedJSON}
	}
	if envelope.Method != "" {
		kind := IncomingNotification
		var id RawID
		if len(envelope.ID) != 0 && string(envelope.ID) != "null" {
			if !validID(envelope.ID) {
				return &ProtocolError{Kind: ProtocolInvalidEnvelope}
			}
			kind = IncomingServerRequest
			id = RawID{value: append(json.RawMessage(nil), envelope.ID...), connection: Connection{session: s}}
		}
		event := Incoming{Kind: kind, ID: id, Method: envelope.Method, Params: append(json.RawMessage(nil), envelope.Params...)}
		select {
		case s.events <- event:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return io.EOF
		}
	}
	if len(envelope.ID) == 0 || string(envelope.ID) == "null" {
		return &ProtocolError{Kind: ProtocolInvalidEnvelope}
	}
	key := string(envelope.ID)
	s.pendingMu.Lock()
	resultCh := s.pending[key]
	s.pendingMu.Unlock()
	if resultCh == nil {
		return nil
	}
	if envelope.Error != nil {
		resultCh <- response{err: &RPCError{Code: envelope.Error.Code, Message: envelope.Error.Message}}
		return nil
	}
	resultCh <- response{result: append(json.RawMessage(nil), envelope.Result...)}
	return nil
}

func validID(raw json.RawMessage) bool {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return true
	}
	var text string
	return json.Unmarshal(raw, &text) == nil
}

func (s *Session) finish(err error) {
	s.once.Do(func() {
		s.errMu.Lock()
		s.err = err
		s.errMu.Unlock()
		_ = s.conn.Close()
		s.pendingMu.Lock()
		pendingErr := err
		if pendingErr == nil {
			pendingErr = io.ErrClosedPipe
		}
		for _, resultCh := range s.pending {
			select {
			case resultCh <- response{err: pendingErr}:
			default:
			}
		}
		s.pendingMu.Unlock()
		close(s.done)
	})
}
