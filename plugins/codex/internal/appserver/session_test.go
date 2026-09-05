package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

func TestCallDeadlineIncludesBlockedWrite(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	session := NewSession(client, Options{RPCTimeout: 20 * time.Millisecond})
	defer session.Close()
	session.Start(t.Context())
	done := make(chan error, 1)
	go func() { done <- session.Call(t.Context(), "blocked", nil, nil) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("deadline error = %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Error("configured RPC deadline did not interrupt the blocked write")
		session.Close()
		<-done
	}
}

func TestManagerDrainsNotificationsBeforeListResponse(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	manager := NewManager(nil, ManagerOptions{PollInterval: time.Hour, Session: Options{RPCTimeout: 100 * time.Millisecond, EventBuffer: 4}})
	events := make(chan ManagerEvent, 32)
	done := make(chan error, 1)
	go func() { done <- manager.runConnection(ctx, client, events) }()
	serverDone := make(chan error, 1)
	go func() {
		p := newManagerTestPeer(peer)
		init, err := p.read("initialize")
		if err != nil || !p.respond(init.ID, map[string]any{"userAgent": "audit"}) {
			serverDone <- errors.Join(err, p.err)
			return
		}
		if _, err := p.read("initialized"); err != nil {
			serverDone <- err
			return
		}
		list, err := p.read("thread/loaded/list")
		if err != nil {
			serverDone <- err
			return
		}
		for range 8 {
			if err := p.encoder.Encode(map[string]any{"method": "audit/notification", "params": map[string]any{}}); err != nil {
				serverDone <- err
				return
			}
		}
		p.respond(list.ID, map[string]any{"data": []string{}, "nextCursor": nil})
		serverDone <- p.err
	}()
	notifications, reconciled := 0, false
	for !reconciled || notifications != 8 {
		select {
		case event := <-events:
			if event.Kind == ManagerIncoming {
				if !reconciled {
					t.Error("live notification overtook the baseline reconciliation")
				}
				notifications++
			}
			if event.Kind == ManagerThreadsReconciled {
				reconciled = true
			}
		case err := <-done:
			t.Errorf("connection failed during valid notification burst: %v", err)
			peer.Close()
			<-serverDone
			return
		case <-time.After(time.Second):
			t.Error("connection did not finish notification burst and reconciliation")
			cancel()
			peer.Close()
			<-done
			<-serverDone
			return
		}
	}
	if err := <-serverDone; err != nil {
		t.Error(err)
	}
	cancel()
	<-done
}

func TestEventPumpBoundsStalledConsumerAndJoinsCancellation(t *testing.T) {
	for _, holdPhase := range []bool{false, true} {
		t.Run(map[bool]string{false: "stalled live consumer", true: "canceled snapshot"}[holdPhase], func(t *testing.T) {
			client, peer := net.Pipe()
			defer peer.Close()
			session := NewSession(client, Options{RPCTimeout: 25 * time.Millisecond, EventBuffer: 2})
			defer session.Close()
			session.Start(t.Context())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			pump := startEventPump(ctx, session, make(chan ManagerEvent))
			defer pump.stop()
			if holdPhase {
				entered := make(chan struct{})
				done := make(chan error, 1)
				go func() {
					done <- pump.phase(ctx, func(ctx context.Context, _ func(ManagerEvent) error) error {
						close(entered)
						<-ctx.Done()
						return ctx.Err()
					})
				}()
				<-entered
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("snapshot cancellation = %v", err)
				}
			} else {
				if err := pump.phase(ctx, func(context.Context, func(ManagerEvent) error) error { return nil }); err != nil {
					t.Fatal(err)
				}
				session.events <- Incoming{Kind: IncomingNotification, Method: "test/update"}
				select {
				case <-session.done:
					failure, ok := errors.AsType[*ProtocolError](session.Wait())
					if !ok || failure.Kind != ProtocolEventBackpressure {
						t.Fatalf("stalled consumer failure = %v", session.Wait())
					}
				case <-time.After(time.Second):
					t.Fatal("stalled consumer did not terminate the owned transport")
				}
			}
			select {
			case <-pump.done:
			case <-time.After(time.Second):
				t.Fatal("event pump did not join")
			}
		})
	}
}

func TestManagerActionsCannotRetargetReusedRequestIDs(t *testing.T) {
	oldClient, oldPeer := net.Pipe()
	defer oldPeer.Close()
	oldSession := NewSession(oldClient, Options{})
	oldSession.Start(t.Context())
	defer oldSession.Close()
	if err := json.NewEncoder(oldPeer).Encode(map[string]any{"id": "reused", "method": "request/approval", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	oldRequest := <-oldSession.Events()
	oldConnection := Connection{session: oldSession}
	_ = oldSession.Close()

	newClient, newPeer := net.Pipe()
	defer newPeer.Close()
	newSession := NewSession(newClient, Options{RPCTimeout: time.Second})
	newSession.Start(t.Context())
	defer newSession.Close()
	if err := json.NewEncoder(newPeer).Encode(map[string]any{"id": "reused", "method": "request/approval", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	newRequest := <-newSession.Events()
	if oldRequest.ID.Equal(newRequest.ID) {
		t.Fatal("request identity ignored the transport incarnation")
	}
	manager := NewManager(nil, ManagerOptions{})
	if err := manager.Respond(t.Context(), oldRequest.ID, map[string]string{"decision": "accept"}); err == nil {
		t.Fatal("retired approval succeeded")
	}
	if err := manager.Interrupt(t.Context(), oldConnection, "same-thread", "same-turn"); err == nil {
		t.Fatal("retired interrupt succeeded")
	}
	done := make(chan error, 1)
	go func() { done <- manager.Respond(t.Context(), newRequest.ID, map[string]string{"decision": "decline"}) }()
	var response struct {
		ID     string            `json:"id"`
		Result map[string]string `json:"result"`
	}
	if err := json.NewDecoder(newPeer).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.ID != "reused" || response.Result["decision"] != "decline" {
		t.Fatalf("new connection received a retired action: %#v", response)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
