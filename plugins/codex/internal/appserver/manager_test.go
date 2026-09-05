package appserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"
)

type connectorFunc func(context.Context) (ReadWriteCloser, error)

func (f connectorFunc) Connect(ctx context.Context) (ReadWriteCloser, error) { return f(ctx) }

func TestManagerInitializesAndRecoversLoadedThread(t *testing.T) {
	client, server := net.Pipe()
	used := false
	connector := connectorFunc(func(context.Context) (ReadWriteCloser, error) {
		if used {
			return nil, errors.New("no more connections")
		}
		used = true
		return client, nil
	})
	manager := NewManager(connector, ManagerOptions{
		PollInterval: time.Hour, Backoff: []time.Duration{time.Millisecond},
		Session: Options{RPCTimeout: time.Second},
	})
	ctx, cancel := context.WithCancel(t.Context())
	events := make(chan ManagerEvent, 16)
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx, events) }()

	serverErr := make(chan error, 1)
	go func() {
		defer server.Close()
		reader := bufio.NewReader(server)
		read := func(method string) map[string]any {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				serverErr <- err
				return nil
			}
			var request map[string]any
			if err := json.Unmarshal(line, &request); err != nil || request["method"] != method {
				serverErr <- errors.New("unexpected app-server request")
				return nil
			}
			return request
		}
		respond := func(id int, result string) bool {
			_, err := server.Write([]byte(`{"id":` + string(rune('0'+id)) + `,"result":` + result + `}` + "\n"))
			if err != nil {
				serverErr <- err
				return false
			}
			return true
		}
		initialize := read("initialize")
		if initialize == nil {
			return
		}
		capabilities := initialize["params"].(map[string]any)["capabilities"].(map[string]any)
		if capabilities["experimentalApi"] != true {
			serverErr <- errors.New("experimental API was not enabled")
			return
		}
		for _, value := range capabilities["optOutNotificationMethods"].([]any) {
			if value == "turn/plan/updated" {
				serverErr <- errors.New("plan updates were incorrectly suppressed")
				return
			}
			if value == "account/rateLimits/updated" {
				capabilities["rateLimitsOptedOut"] = true
			}
		}
		if capabilities["rateLimitsOptedOut"] != true {
			serverErr <- errors.New("disabled quota did not suppress rate-limit updates")
			return
		}
		if !respond(1, `{"codexHome":"/hidden","platformFamily":"unix","platformOs":"macos","userAgent":"bsbctl_plugin_codex/0.149.1 (Mac OS 26.5.2; arm64) dumb (bsbctl_plugin_codex; 0.1.0)"}`) || read("initialized") == nil || read("thread/loaded/list") == nil {
			return
		}
		if !respond(2, `{"data":["thread-1"],"nextCursor":null}`) {
			return
		}
		resume := read("thread/resume")
		if resume == nil {
			return
		}
		want := map[string]any{
			"threadId": "thread-1", "excludeTurns": true,
			"initialTurnsPage": map[string]any{"limit": float64(1), "sortDirection": "desc", "itemsView": "summary"},
		}
		if !reflect.DeepEqual(resume["params"], want) {
			serverErr <- errors.New("manager did not use bounded thread recovery")
			return
		}
		if !respond(3, `{"thread":{"id":"thread-1","name":"Safe","cwd":"/hidden/project","status":{"type":"idle"}},"initialTurnsPage":{"data":[],"nextCursor":null,"backwardsCursor":null}}`) {
			return
		}
		serverErr <- nil
		<-ctx.Done()
	}()

	var connected, attached, reconciled bool
	deadline := time.After(2 * time.Second)
	for !connected || !attached || !reconciled {
		select {
		case event := <-events:
			switch event.Kind {
			case ManagerConnected:
				connected = event.Initialize.UserAgent == "bsbctl_plugin_codex/0.149.1 (Mac OS 26.5.2; arm64) dumb (bsbctl_plugin_codex; 0.1.0)"
			case ManagerThreadAttached:
				attached = event.Thread != nil && event.Thread.ID == "thread-1"
			case ManagerThreadsReconciled:
				reconciled = reflect.DeepEqual(event.ThreadIDs, []string{"thread-1"})
			}
		case <-deadline:
			t.Fatalf("events connected=%v attached=%v reconciled=%v", connected, attached, reconciled)
		}
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run error = %v", err)
	}
}

func TestManagerReadsRateLimitsWhenEnabledWithoutMakingFailureFatal(t *testing.T) {
	client, server := net.Pipe()
	used := false
	manager := NewManager(connectorFunc(func(context.Context) (ReadWriteCloser, error) {
		if used {
			return nil, errors.New("no more connections")
		}
		used = true
		return client, nil
	}), ManagerOptions{
		PollInterval: time.Hour, RateLimitsEnabled: true, RateLimitsPollInterval: time.Hour,
		Backoff: []time.Duration{time.Millisecond}, Session: Options{RPCTimeout: time.Second},
	})
	ctx, cancel := context.WithCancel(t.Context())
	events := make(chan ManagerEvent, 16)
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx, events) }()

	serverErr := make(chan error, 1)
	go func() {
		defer server.Close()
		reader := bufio.NewReader(server)
		read := func(method string) map[string]any {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				serverErr <- err
				return nil
			}
			var request map[string]any
			if json.Unmarshal(line, &request) != nil || request["method"] != method {
				serverErr <- errors.New("unexpected app-server request")
				return nil
			}
			return request
		}
		initialize := read("initialize")
		if initialize == nil {
			return
		}
		optOut := initialize["params"].(map[string]any)["capabilities"].(map[string]any)["optOutNotificationMethods"].([]any)
		for _, method := range optOut {
			if method == "account/rateLimits/updated" {
				serverErr <- errors.New("enabled quota suppressed rate-limit updates")
				return
			}
		}
		if _, err := server.Write([]byte(`{"id":1,"result":{"platformFamily":"unix","platformOs":"macos","userAgent":"codex"}}` + "\n")); err != nil || read("initialized") == nil || read("account/rateLimits/read") == nil {
			serverErr <- err
			return
		}
		if _, err := server.Write([]byte(`{"id":2,"result":{"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":13,"windowDurationMins":300,"resetsAt":1787600000}}},"rateLimitResetCredits":null}}` + "\n")); err != nil || read("thread/loaded/list") == nil {
			serverErr <- err
			return
		}
		if _, err := server.Write([]byte(`{"id":3,"result":{"data":[],"nextCursor":null}}` + "\n")); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
		<-ctx.Done()
	}()

	var limits, reconciled bool
	deadline := time.After(2 * time.Second)
	for !limits || !reconciled {
		select {
		case event := <-events:
			if event.Kind == ManagerRateLimitsSnapshot {
				limits = event.RateLimits != nil && event.RateLimits.Primary != nil && event.RateLimits.Primary.UsedPercent == 13
			}
			if event.Kind == ManagerThreadsReconciled {
				reconciled = true
			}
		case <-deadline:
			t.Fatalf("events limits=%v reconciled=%v", limits, reconciled)
		}
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run error = %v", err)
	}
}

func TestManagerRecoversThreadsAfterOptionalRPCTimeout(t *testing.T) {
	for _, method := range []string{"account/rateLimits/read", "thread/resume"} {
		t.Run(method, func(t *testing.T) {
			client, peer := net.Pipe()
			ctx, cancel := context.WithCancel(t.Context())
			manager := NewManager(nil, ManagerOptions{
				PollInterval: time.Hour, RateLimitsEnabled: method == "account/rateLimits/read",
				RateLimitsPollInterval: time.Hour, Session: Options{RPCTimeout: 250 * time.Millisecond},
			})
			events := make(chan ManagerEvent, 8)
			done := make(chan error, 1)
			serverDone := make(chan error, 1)
			go func() { done <- manager.runConnection(ctx, client, events) }()
			t.Cleanup(func() {
				cancel()
				_ = peer.Close()
				<-done
				if err := <-serverDone; err != nil {
					t.Error(err)
				}
			})
			go func() {
				p := newManagerTestPeer(peer)
				serve := func() error {
					init, err := p.read("initialize")
					if err != nil || !p.respond(init.ID, map[string]any{"userAgent": "test"}) {
						return errors.Join(err, p.err)
					}
					if _, err := p.read("initialized"); err != nil {
						return err
					}
					if method == "account/rateLimits/read" {
						if _, err := p.read(method); err != nil {
							return err
						}
						// Leave only the optional request unanswered. Required RPCs remain healthy.
					}
					list, err := p.read("thread/loaded/list")
					if err != nil || !p.respond(list.ID, map[string]any{"data": []string{"first", "second"}}) {
						return errors.Join(err, p.err)
					}
					for _, id := range []string{"first", "second"} {
						resume, err := p.read("thread/resume")
						if err != nil {
							return err
						}
						if resume.Params.ThreadID != id {
							return errors.New("unexpected recovered thread")
						}
						if method == "thread/resume" && id == "first" {
							continue
						}
						if !p.respond(resume.ID, managerTestThread(id)) {
							return p.err
						}
					}
					return nil
				}
				serverDone <- serve()
			}()
			failed, attached := false, false
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for {
				select {
				case event := <-events:
					switch event.Kind {
					case ManagerRateLimitsReadFailed, ManagerThreadAttachFailed:
						failed = event.FailureCode == "timeout"
					case ManagerThreadAttached:
						attached = attached || event.Thread != nil && event.Thread.ID == "second"
					case ManagerThreadsReconciled:
						if !failed || !attached || !reflect.DeepEqual(event.ThreadIDs, []string{"first", "second"}) {
							t.Fatalf("recovery after timeout: failed=%v attached=%v event=%#v", failed, attached, event)
						}
						return
					}
				case err := <-done:
					done <- err
					t.Fatalf("optional RPC timeout closed required monitoring: %v", err)
				case <-deadline.C:
					t.Fatal("required thread recovery did not finish")
				}
			}
		})
	}
}

func TestManagerAttemptsEveryAttachedThreadUnsubscribeBeforeClosing(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	manager := newSingleConnectionTestManager(client, time.Second, Options{RPCTimeout: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	events := make(chan ManagerEvent, 16)
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx, events) }()

	serverDone := make(chan error, 1)
	go func() {
		peer := newManagerTestPeer(server)
		if err := peer.initializeAndResume([]string{"thread-1", "thread-2"}); err != nil {
			serverDone <- err
			return
		}
		unsubscribed := make(map[string]bool, 2)
		for index := range 2 {
			unsubscribe, err := peer.read("thread/unsubscribe")
			if err != nil {
				serverDone <- err
				return
			}
			unsubscribed[unsubscribe.Params.ThreadID] = true
			if index == 0 && !peer.respondError(unsubscribe.ID, -32000, "rejected") {
				serverDone <- peer.err
				return
			}
			if index != 0 && !peer.respond(unsubscribe.ID, nil) {
				serverDone <- peer.err
				return
			}
		}
		if !unsubscribed["thread-1"] || !unsubscribed["thread-2"] {
			serverDone <- errors.New("manager did not unsubscribe every attached thread")
			return
		}
		serverDone <- nil
	}()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind == ManagerThreadsReconciled {
				cancel()
				goto stopped
			}
		case <-deadline:
			t.Fatal("manager did not reconcile loaded threads")
		}
	}

stopped:
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run error = %v", err)
	}
}

func TestManagerUnsubscribesWhenCancellationInterruptsReconciliation(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	manager := newSingleConnectionTestManager(client, time.Second, Options{RPCTimeout: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	events := make(chan ManagerEvent, 2)
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx, events) }()

	secondResume := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		peer := newManagerTestPeer(server)
		if err := peer.initialize([]string{"thread-1", "thread-2"}); err != nil {
			serverDone <- err
			return
		}
		resume, err := peer.read("thread/resume")
		if err != nil || !peer.respond(resume.ID, managerTestThread("thread-1")) {
			serverDone <- errors.Join(err, peer.err)
			return
		}
		if _, err := peer.read("thread/resume"); err != nil {
			serverDone <- err
			return
		}
		close(secondResume)
		unsubscribe, err := peer.read("thread/unsubscribe")
		if err != nil || unsubscribe.Params.ThreadID != "thread-1" || !peer.respond(unsubscribe.ID, nil) {
			serverDone <- errors.Join(err, peer.err, errors.New("attached thread was not unsubscribed"))
			return
		}
		serverDone <- nil
	}()

	select {
	case <-secondResume:
		connected := <-events
		if connected.Kind != ManagerConnected || connected.Connection.session == nil {
			t.Fatalf("first event = %#v, want connection ownership", connected)
		}
		// The peer read does not prove the local Write and its cancellation
		// callback have finished. Cancel only once this writer releases ownership.
		connected.Connection.session.writeGate <- struct{}{}
		<-connected.Connection.session.writeGate
		cancel()
	case <-time.After(time.Second):
		t.Fatal("manager did not begin the second recovery")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run error = %v", err)
	}
}

func TestManagerUnsubscribesWhenCancellationInterruptsEventDelivery(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	manager := newSingleConnectionTestManager(client, time.Second, Options{RPCTimeout: time.Second, EventBuffer: 1})
	ctx, cancel := context.WithCancel(t.Context())
	events := make(chan ManagerEvent, 3)
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx, events) }()

	deliveryBlocked := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		peer := newManagerTestPeer(server)
		if err := peer.initializeAndResume([]string{"thread-1"}); err != nil {
			serverDone <- err
			return
		}
		for range 3 {
			if err := peer.encoder.Encode(map[string]any{"method": "thread/status/changed", "params": map[string]any{}}); err != nil {
				serverDone <- err
				return
			}
		}
		close(deliveryBlocked)
		unsubscribe, err := peer.read("thread/unsubscribe")
		if err != nil || unsubscribe.Params.ThreadID != "thread-1" || !peer.respond(unsubscribe.ID, nil) {
			serverDone <- errors.Join(err, peer.err, errors.New("blocked delivery bypassed unsubscription"))
			return
		}
		serverDone <- nil
	}()

	select {
	case <-deliveryBlocked:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("manager did not block on event delivery")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run error = %v", err)
	}
}

func TestManagerShutdownClosesConnectionWhenUnsubscribeWriteStalls(t *testing.T) {
	client, server := net.Pipe()
	connection := &blockingUnsubscribeConnection{
		ReadWriteCloser: client,
		writeStarted:    make(chan struct{}),
		closed:          make(chan struct{}),
	}
	t.Cleanup(func() { _ = connection.Close(); _ = server.Close() })
	manager := newSingleConnectionTestManager(connection, 20*time.Millisecond, Options{RPCTimeout: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	events := make(chan ManagerEvent, 16)
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx, events) }()

	serverDone := make(chan error, 1)
	go func() {
		peer := newManagerTestPeer(server)
		if err := peer.initializeAndResume([]string{"thread-1"}); err != nil {
			serverDone <- err
			return
		}
		<-connection.closed
		serverDone <- nil
	}()

	for {
		select {
		case event := <-events:
			if event.Kind == ManagerThreadsReconciled {
				cancel()
				goto canceled
			}
		case <-time.After(time.Second):
			t.Fatal("manager did not reconcile the attached thread")
		}
	}

canceled:
	select {
	case <-connection.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("manager did not begin clean unsubscription")
	}
	select {
	case <-connection.closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown deadline did not close the stalled connection")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run error = %v", err)
	}
}

func TestManagerAttemptsEveryAttachedThreadWhenOneUnsubscribeResponseStalls(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	manager := newSingleConnectionTestManager(client, 50*time.Millisecond, Options{RPCTimeout: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	events := make(chan ManagerEvent, 16)
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx, events) }()

	requests := make(chan []string, 1)
	serverDone := make(chan error, 1)
	go func() {
		peer := newManagerTestPeer(server)
		if err := peer.initializeAndResume([]string{"thread-1", "thread-2"}); err != nil {
			serverDone <- err
			return
		}
		seen := make([]string, 0, 2)
		for range 2 {
			unsubscribe, err := peer.read("thread/unsubscribe")
			if err != nil {
				serverDone <- err
				return
			}
			seen = append(seen, unsubscribe.Params.ThreadID)
			if len(seen) == 2 && !peer.respond(unsubscribe.ID, nil) {
				serverDone <- peer.err
				return
			}
		}
		requests <- seen
		<-ctx.Done()
		serverDone <- nil
	}()

	for {
		select {
		case event := <-events:
			if event.Kind == ManagerThreadsReconciled {
				cancel()
				goto canceled
			}
		case <-time.After(time.Second):
			t.Fatal("manager did not reconcile attached threads")
		}
	}

canceled:
	select {
	case seen := <-requests:
		if len(seen) != 2 || seen[0] == seen[1] {
			t.Fatalf("unsubscribe requests = %v", seen)
		}
	case <-time.After(time.Second):
		t.Fatal("manager did not attempt every unsubscribe before the deadline")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run error = %v", err)
	}
}

func newSingleConnectionTestManager(connection ReadWriteCloser, shutdownTimeout time.Duration, session Options) *Manager {
	used := false
	return NewManager(connectorFunc(func(context.Context) (ReadWriteCloser, error) {
		if used {
			return nil, errors.New("no more connections")
		}
		used = true
		return connection, nil
	}), ManagerOptions{
		PollInterval: time.Hour, Backoff: []time.Duration{time.Millisecond},
		ShutdownTimeout: shutdownTimeout, Session: session,
	})
}

type managerTestRequest struct {
	ID     any    `json:"id"`
	Method string `json:"method"`
	Params struct {
		ThreadID string `json:"threadId"`
	} `json:"params"`
}

type managerTestPeer struct {
	decoder *json.Decoder
	encoder *json.Encoder
	err     error
}

func newManagerTestPeer(connection net.Conn) *managerTestPeer {
	return &managerTestPeer{decoder: json.NewDecoder(connection), encoder: json.NewEncoder(connection)}
}

func (p *managerTestPeer) read(want string) (managerTestRequest, error) {
	var request managerTestRequest
	if err := p.decoder.Decode(&request); err != nil {
		return managerTestRequest{}, err
	}
	if request.Method != want {
		return managerTestRequest{}, errors.New("unexpected app-server request")
	}
	return request, nil
}

func (p *managerTestPeer) respond(id, result any) bool {
	p.err = p.encoder.Encode(map[string]any{"id": id, "result": result})
	return p.err == nil
}

func (p *managerTestPeer) respondError(id any, code int, message string) bool {
	p.err = p.encoder.Encode(map[string]any{
		"id": id, "error": map[string]any{"code": code, "message": message},
	})
	return p.err == nil
}

func (p *managerTestPeer) initialize(threadIDs []string) error {
	initialize, err := p.read("initialize")
	if err != nil || !p.respond(initialize.ID, map[string]any{"platformFamily": "unix", "platformOs": "macos", "userAgent": "codex"}) {
		return errors.Join(err, p.err)
	}
	if _, err := p.read("initialized"); err != nil {
		return err
	}
	list, err := p.read("thread/loaded/list")
	if err != nil || !p.respond(list.ID, map[string]any{"data": threadIDs, "nextCursor": nil}) {
		return errors.Join(err, p.err)
	}
	return nil
}

func (p *managerTestPeer) initializeAndResume(threadIDs []string) error {
	if err := p.initialize(threadIDs); err != nil {
		return err
	}
	for _, threadID := range threadIDs {
		resume, err := p.read("thread/resume")
		if err != nil || !p.respond(resume.ID, managerTestThread(threadID)) {
			return errors.Join(err, p.err)
		}
	}
	return nil
}

func managerTestThread(threadID string) map[string]any {
	return map[string]any{
		"thread":           map[string]any{"id": threadID, "status": map[string]any{"type": "idle"}},
		"initialTurnsPage": map[string]any{"data": []any{}},
	}
}

type blockingUnsubscribeConnection struct {
	ReadWriteCloser
	writeStarted chan struct{}
	closed       chan struct{}
	writeOnce    sync.Once
	closeOnce    sync.Once
}

func (c *blockingUnsubscribeConnection) Write(payload []byte) (int, error) {
	if bytes.Contains(payload, []byte(`"method":"thread/unsubscribe"`)) {
		c.writeOnce.Do(func() { close(c.writeStarted) })
	}
	return c.ReadWriteCloser.Write(payload)
}

func (c *blockingUnsubscribeConnection) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		err = c.ReadWriteCloser.Close()
	})
	return err
}
