// Package control implements the local CLI-to-daemon JSON-RPC control plane.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/sdk/rpc"
)

var ErrAlreadyRunning = errors.New("bsbctl daemon is already listening")

const (
	defaultMaxControlPeers = 16
	defaultControlIdle     = 30 * time.Second
)

type Server struct {
	path        string
	version     string
	backends    Backends
	listener    net.Listener
	info        os.FileInfo
	maxPeers    int
	idleTimeout time.Duration
	now         func() time.Time
	newPeer     func(net.Conn) controlPeer
	peersMu     sync.Mutex
	peers       map[net.Conn]struct{}
	peerWG      sync.WaitGroup
	calls       callTracker
}

type controlOptions struct {
	maxPeers    int
	idleTimeout time.Duration
	now         func() time.Time
	newPeer     func(net.Conn) controlPeer
}

type controlPeer interface {
	Handle(string, rpc.Handler) error
	Serve(context.Context) error
	Close() error
}

func defaultControlOptions() controlOptions {
	return controlOptions{
		maxPeers: defaultMaxControlPeers, idleTimeout: defaultControlIdle, now: time.Now,
		newPeer: func(conn net.Conn) controlPeer { return rpc.NewPeer(conn) },
	}
}

func newServer(path, version string, backends Backends, listener net.Listener, info os.FileInfo, options controlOptions) *Server {
	if options.maxPeers <= 0 {
		options.maxPeers = defaultMaxControlPeers
	}
	if options.idleTimeout <= 0 {
		options.idleTimeout = defaultControlIdle
	}
	if options.now == nil {
		options.now = time.Now
	}
	if options.newPeer == nil {
		options.newPeer = func(conn net.Conn) controlPeer { return rpc.NewPeer(conn) }
	}
	server := &Server{
		path: path, version: version, backends: backends, listener: listener, info: info,
		maxPeers: options.maxPeers, idleTimeout: options.idleTimeout, now: options.now, newPeer: options.newPeer,
		peers: make(map[net.Conn]struct{}),
	}
	server.calls.cond = sync.NewCond(&server.calls.mu)
	return server
}

func Listen(path, version string, backends Backends) (*Server, error) {
	if err := backends.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create control socket directory: %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("protect control socket: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	return newServer(path, version, backends, listener, info, defaultControlOptions()), nil
}

func (s *Server) Serve(ctx context.Context) (result error) {
	watcherDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = s.listener.Close()
		case <-watcherDone:
		}
	}()
	defer func() {
		close(watcherDone)
		_ = s.listener.Close()
		s.closeActivePeers()
		s.peerWG.Wait()
		s.calls.stopAndWait()
		s.removeOwnSocket()
	}()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("listener_failed")
		}
		if !s.admitPeer(conn) {
			_ = conn.Close()
			continue
		}
		s.peerWG.Go(func() {
			defer s.releasePeer(conn)
			s.serveConn(ctx, newActivityConn(conn, s.idleTimeout, s.now))
		})
	}
}

func (s *Server) admitPeer(conn net.Conn) bool {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()
	if len(s.peers) >= s.maxPeers {
		return false
	}
	s.peers[conn] = struct{}{}
	return true
}

func (s *Server) releasePeer(conn net.Conn) {
	_ = conn.Close()
	s.peersMu.Lock()
	delete(s.peers, conn)
	s.peersMu.Unlock()
}

func (s *Server) closeActivePeers() {
	s.peersMu.Lock()
	peers := make([]net.Conn, 0, len(s.peers))
	for conn := range s.peers {
		peers = append(peers, conn)
	}
	s.peersMu.Unlock()
	for _, conn := range peers {
		_ = conn.Close()
	}
}

type activityConn struct {
	net.Conn
	idle time.Duration
	now  func() time.Time
}

type callTracker struct {
	mu      sync.Mutex
	cond    *sync.Cond
	active  int
	closing bool
}

func (t *callTracker) begin() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return false
	}
	t.active++
	return true
}

func (t *callTracker) done() {
	t.mu.Lock()
	t.active--
	if t.active == 0 {
		t.cond.Broadcast()
	}
	t.mu.Unlock()
}

func (t *callTracker) stopAndWait() {
	t.mu.Lock()
	t.closing = true
	for t.active != 0 {
		t.cond.Wait()
	}
	t.mu.Unlock()
}

func newActivityConn(conn net.Conn, idle time.Duration, now func() time.Time) net.Conn {
	return &activityConn{Conn: conn, idle: idle, now: now}
}

func (c *activityConn) Read(buffer []byte) (int, error) {
	if err := c.SetReadDeadline(c.now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Read(buffer)
}

func (s *Server) handle(peer controlPeer, method string, handler rpc.Handler) error {
	return peer.Handle(method, func(ctx context.Context, raw json.RawMessage) (any, *rpc.Error) {
		if !s.calls.begin() {
			return nil, &rpc.Error{Code: -32000, Message: "control server is shutting down"}
		}
		defer s.calls.done()
		return handler(ctx, raw)
	})
}

func (s *Server) removeOwnSocket() {
	if s.path == "" || s.info == nil {
		return
	}
	current, err := os.Lstat(s.path)
	if err == nil && os.SameFile(current, s.info) {
		_ = os.Remove(s.path)
	}
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("control path %q exists and is not a socket", path)
	}
	conn, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return ErrAlreadyRunning
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale control socket: %w", err)
	}
	return nil
}
