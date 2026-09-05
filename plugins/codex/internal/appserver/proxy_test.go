package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestProxyArgsConnectsOnlyToDefaultDaemon(t *testing.T) {
	if got := proxyArgs(); !reflect.DeepEqual(got, []string{"app-server", "proxy"}) {
		t.Fatalf("proxy args = %#v", got)
	}
}

func TestProxyProcessOutlivesConnectContext(t *testing.T) {
	if os.Getenv("BSBCTL_PROXY_PROCESS_HELPER") == "1" {
		_, _ = os.Stdout.WriteString("ready\n")
		stopped := make(chan os.Signal, 1)
		signal.Notify(stopped, syscall.SIGTERM)
		<-stopped
		return
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command := newProxyProcess(ctx, os.Args[0], "-test.run=TestProxyProcessOutlivesConnectContext")
	command.Env = append(os.Environ(), "BSBCTL_PROXY_PROCESS_HELPER=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = command.Wait()
		close(waitDone)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			t.Error("proxy helper was not reaped during cleanup")
		}
	})
	awaitProxyReadiness(t, stdout)
	cancel()
	select {
	case <-waitDone:
		t.Fatalf("connect context stopped the owned proxy process: %v", waitErr)
	case <-time.After(50 * time.Millisecond):
	}

	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waitDone:
		if waitErr == nil {
			t.Fatal("killed proxy helper exited successfully")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("killed proxy helper was not reaped")
	}
	if command.ProcessState == nil {
		t.Fatal("proxy helper was not reaped")
	}
}

func TestProxyConnectionCloseKillsAndReapsChild(t *testing.T) {
	command := newProxyProcess(t.Context(), os.Args[0], "-test.run=TestProxyProcessOutlivesConnectContext")
	command.Env = append(os.Environ(), "BSBCTL_PROXY_PROCESS_HELPER=1")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	awaitProxyReadiness(t, stdout)

	connection := &proxyConnection{stdin: stdin, stdout: stdout, command: command}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if command.ProcessState == nil {
		t.Fatal("proxy connection did not reap its child")
	}
}

func awaitProxyReadiness(t *testing.T, stdout io.ReadCloser) {
	t.Helper()
	done := make(chan struct{})
	var line string
	var readErr error
	go func() {
		line, readErr = bufio.NewReader(stdout).ReadString('\n')
		close(done)
	}()
	t.Cleanup(func() {
		_ = stdout.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("proxy readiness reader did not stop")
		}
	})
	select {
	case <-done:
		if readErr != nil || line != "ready\n" {
			t.Fatalf("proxy helper readiness = %q, %v", line, readErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy helper readiness timed out")
	}
}

func TestProxyWebSocketConvertsJSONLLinesToTextMessages(t *testing.T) {
	client, server := net.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- http.Serve(&singleConnectionListener{connection: server}, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			connection, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			messageType, payload, err := connection.Read(request.Context())
			if err != nil || messageType != websocket.MessageText {
				return
			}
			var requestEnvelope struct {
				ID int `json:"id"`
			}
			if json.Unmarshal(payload, &requestEnvelope) != nil || requestEnvelope.ID != 1 {
				return
			}
			_ = connection.Write(request.Context(), websocket.MessageText, []byte(`{"id":1,"result":{}}`))
		}))
	}()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	connection, err := dialWebSocketOver(ctx, client)
	if err != nil {
		t.Fatalf("dialWebSocketOver: %v", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte(`{"id":1,"method":"ping","params":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 128)
	n, err := connection.Read(buffer)
	if err != nil || string(buffer[:n]) != `{"id":1,"result":{}}`+"\n" {
		t.Fatalf("proxy response = %q, %v", buffer[:n], err)
	}
	_ = connection.Close()
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("proxy server did not stop")
	}
}

func TestProxyWebSocketAcceptsProtocolSizedThreadSnapshot(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	payload := `{"id":1,"result":{"text":"` + strings.Repeat("x", 64<<10) + `"}}`
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_ = http.Serve(&singleConnectionListener{connection: server}, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			connection, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			_ = connection.Write(request.Context(), websocket.MessageText, []byte(payload))
		}))
	}()
	connection, err := dialWebSocketOver(t.Context(), client)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	data := make([]byte, len(payload)+1)
	if _, err := io.ReadFull(connection, data); err != nil {
		t.Fatalf("valid 64 KiB snapshot rejected below the 1 MiB protocol limit: %v", err)
	}
	if string(data) != payload+"\n" {
		t.Fatal("WebSocket conversion changed the snapshot")
	}
	_ = connection.Close()
	<-serverDone
}

type singleConnectionListener struct {
	connection net.Conn
	accepted   bool
}

func (l *singleConnectionListener) Accept() (net.Conn, error) {
	if l.accepted {
		return nil, net.ErrClosed
	}
	l.accepted = true
	return l.connection, nil
}

func (l *singleConnectionListener) Close() error   { return nil }
func (l *singleConnectionListener) Addr() net.Addr { return streamAddr("test") }
