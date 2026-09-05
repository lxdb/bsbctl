package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/lxdb/busylib-go/proto/statepb"
	"google.golang.org/protobuf/proto"
)

type fakeStorage struct {
	mu    sync.RWMutex
	files map[string][]byte
}

func newFakeDependencies() *fakeDependencies {
	fake := &fakeDependencies{storage: fakeStorage{files: make(map[string][]byte)}}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.ServeHTTP))
	return fake
}

func (fake *fakeDependencies) URL() string { return fake.server.URL }

func (fake *fakeDependencies) Close() { fake.server.Close() }

func (fake *fakeDependencies) Ready() bool {
	counts := fake.Counts()
	return counts.Version > 0 && counts.StatusStreams > 0 && counts.StateMessages > 0 && counts.CodexUsage > 0 && counts.DisplayDraw > 0
}

func (fake *fakeDependencies) Counts() fakeRequestCounts {
	return fakeRequestCounts{
		Version: fake.version.Load(), StatusStreams: fake.statusStreams.Load(), StateMessages: fake.stateMessages.Load(),
		CodexUsage: fake.codexUsage.Load(), DisplayDraw: fake.displayDraw.Load(), DisplayClear: fake.displayClear.Load(),
	}
}

func (fake *fakeDependencies) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/api/version":
		fake.version.Add(1)
		writeFakeJSON(writer, `{"api_semver":"25.0.0"}`)
	case "/api/codex/usage":
		fake.codexUsage.Add(1)
		writeFakeJSON(writer, `{"rate_limit":{"primary_window":{"used_percent":20,"reset_at":2000000000,"limit_window_seconds":18000},"secondary_window":{"used_percent":30,"reset_at":2000500000,"limit_window_seconds":604800}}}`)
	case "/api/display/draw":
		_, _ = io.Copy(io.Discard, io.LimitReader(request.Body, 1<<20))
		if request.Method == http.MethodDelete {
			fake.displayClear.Add(1)
		} else if request.Method == http.MethodPost {
			fake.displayDraw.Add(1)
		} else {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeFakeJSON(writer, `{"result":"OK"}`)
	case "/api/assets/upload":
		fake.uploadAsset(writer, request)
	case "/api/storage/read":
		fake.readStorage(writer, request)
	case "/api/storage/remove":
		fake.removeStorage(writer, request)
	case "/api/status/ws":
		fake.serveStatusStream(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

func (fake *fakeDependencies) uploadAsset(writer http.ResponseWriter, request *http.Request) {
	application := request.URL.Query().Get("application_name")
	file := request.URL.Query().Get("file")
	if request.Method != http.MethodPost || application == "" || file == "" {
		http.Error(writer, "invalid asset upload", http.StatusBadRequest)
		return
	}
	content, err := io.ReadAll(io.LimitReader(request.Body, 1<<20+1))
	if err != nil || len(content) > 1<<20 {
		http.Error(writer, "invalid asset upload", http.StatusBadRequest)
		return
	}
	path := "/ext/user_assets/" + application + "/" + file
	fake.storage.mu.Lock()
	fake.storage.files[path] = append([]byte(nil), content...)
	fake.storage.mu.Unlock()
	writeFakeJSON(writer, `{"result":"OK"}`)
}

func (fake *fakeDependencies) readStorage(writer http.ResponseWriter, request *http.Request) {
	path := request.URL.Query().Get("path")
	if request.Method != http.MethodGet || path == "" {
		http.Error(writer, "invalid storage read", http.StatusBadRequest)
		return
	}
	fake.storage.mu.RLock()
	content, exists := fake.storage.files[path]
	content = append([]byte(nil), content...)
	fake.storage.mu.RUnlock()
	if !exists {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(writer, `{"error":"file not found"}`)
		return
	}
	writer.Header().Set("Content-Type", "application/octet-stream")
	_, _ = writer.Write(content)
}

func (fake *fakeDependencies) removeStorage(writer http.ResponseWriter, request *http.Request) {
	path := request.URL.Query().Get("path")
	if request.Method != http.MethodDelete || path == "" {
		http.Error(writer, "invalid storage removal", http.StatusBadRequest)
		return
	}
	fake.storage.mu.Lock()
	delete(fake.storage.files, path)
	fake.storage.mu.Unlock()
	writeFakeJSON(writer, `{"result":"OK"}`)
}

func (fake *fakeDependencies) serveStatusStream(writer http.ResponseWriter, request *http.Request) {
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	fake.statusStreams.Add(1)
	defer connection.CloseNow()
	handshakeCtx, cancelHandshake := context.WithTimeout(context.Background(), 5*time.Second)
	messageType, message, err := connection.Read(handshakeCtx)
	cancelHandshake()
	if err != nil || messageType != websocket.MessageText || string(message) != `{"enable":true,"send":"all"}` {
		return
	}
	payload, err := proto.Marshal(&statepb.State{Timestamp: uint64(time.Now().Unix())})
	if err != nil {
		return
	}
	writeState := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := connection.Write(ctx, websocket.MessageBinary, payload); err != nil {
			return err
		}
		fake.stateMessages.Add(1)
		return nil
	}
	if err := writeState(); err != nil {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := writeState(); err != nil {
			return
		}
	}
}

func writeFakeJSON(writer http.ResponseWriter, value string) {
	writer.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(writer, value)
}
