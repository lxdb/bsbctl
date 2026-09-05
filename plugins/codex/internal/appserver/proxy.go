package appserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type ProxyConnector struct {
	CodexBin string
	mu       sync.Mutex
}

func (c *ProxyConnector) Connect(ctx context.Context) (ReadWriteCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	command := newProxyProcess(ctx, c.CodexBin, proxyArgs()...)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	raw := &proxyConnection{stdin: stdin, stdout: stdout, command: command}
	handshakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	connection, err := dialWebSocketOver(handshakeCtx, raw)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	return connection, nil
}

func newProxyProcess(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(context.WithoutCancel(ctx), name, args...)
}

func proxyArgs() []string { return []string{"app-server", "proxy"} }

type proxyConnection struct {
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	command *exec.Cmd
	once    sync.Once
}

func (c *proxyConnection) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *proxyConnection) Write(p []byte) (int, error) { return c.stdin.Write(p) }

func (c *proxyConnection) Close() error {
	var closeErr error
	c.once.Do(func() {
		if err := c.stdin.Close(); err != nil {
			closeErr = err
		}
		_ = c.stdout.Close()
		if c.command.Process != nil {
			_ = c.command.Process.Kill()
		}
		_ = c.command.Wait()
	})
	return closeErr
}

func dialWebSocketOver(ctx context.Context, raw ReadWriteCloser) (ReadWriteCloser, error) {
	if raw == nil {
		return nil, errors.New("proxy connection is nil")
	}
	var dialMu sync.Mutex
	dialed := false
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialMu.Lock()
			defer dialMu.Unlock()
			if dialed {
				return nil, errors.New("proxy stream already used")
			}
			dialed = true
			if connection, ok := raw.(net.Conn); ok {
				return connection, nil
			}
			return &streamNetConn{ReadWriteCloser: raw}, nil
		},
	}
	client := &http.Client{Transport: transport}
	connection, _, err := websocket.Dial(ctx, "ws://localhost/rpc", &websocket.DialOptions{
		HTTPClient: client, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	connection.SetReadLimit(defaultMaxMessageBytes)
	return &webSocketJSONLConnection{connection: connection, raw: raw}, nil
}

type webSocketJSONLConnection struct {
	connection  *websocket.Conn
	raw         ReadWriteCloser
	readMu      sync.Mutex
	readBuffer  []byte
	writeMu     sync.Mutex
	writeBuffer []byte
	once        sync.Once
}

func (c *webSocketJSONLConnection) Read(target []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.readBuffer) == 0 {
		messageType, payload, err := c.connection.Read(context.Background())
		if err != nil {
			return 0, err
		}
		if messageType != websocket.MessageText {
			return 0, errors.New("app-server WebSocket returned a non-text message")
		}
		payload = bytes.TrimSuffix(payload, []byte{'\n'})
		c.readBuffer = append(c.readBuffer, payload...)
		c.readBuffer = append(c.readBuffer, '\n')
	}
	n := copy(target, c.readBuffer)
	c.readBuffer = c.readBuffer[n:]
	return n, nil
}

func (c *webSocketJSONLConnection) Write(source []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.writeBuffer = append(c.writeBuffer, source...)
	for {
		newline := bytes.IndexByte(c.writeBuffer, '\n')
		if newline < 0 {
			return len(source), nil
		}
		message := bytes.TrimSuffix(c.writeBuffer[:newline], []byte{'\r'})
		if err := c.connection.Write(context.Background(), websocket.MessageText, slices.Clone(message)); err != nil {
			return 0, err
		}
		c.writeBuffer = c.writeBuffer[newline+1:]
	}
}

func (c *webSocketJSONLConnection) Close() error {
	var closeErr error
	c.once.Do(func() {
		if err := c.connection.CloseNow(); err != nil {
			closeErr = err
		}
		if err := c.raw.Close(); closeErr == nil && err != nil {
			closeErr = err
		}
	})
	return closeErr
}

type streamNetConn struct{ ReadWriteCloser }

func (c *streamNetConn) LocalAddr() net.Addr              { return streamAddr("proxy-local") }
func (c *streamNetConn) RemoteAddr() net.Addr             { return streamAddr("proxy-remote") }
func (c *streamNetConn) SetDeadline(time.Time) error      { return nil }
func (c *streamNetConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamNetConn) SetWriteDeadline(time.Time) error { return nil }

type streamAddr string

func (streamAddr) Network() string        { return "stdio-proxy" }
func (address streamAddr) String() string { return string(address) }
