package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestResumeThreadUsesBoundedRecoveryAndReturnsLatestTurn(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	session := NewSession(client, Options{RPCTimeout: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	session.Start(ctx)

	serverErr := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(server).ReadBytes('\n')
		if err != nil {
			serverErr <- err
			return
		}
		var request struct {
			ID     uint64         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(line, &request); err != nil {
			serverErr <- err
			return
		}
		want := map[string]any{
			"threadId": "thread-1", "excludeTurns": true,
			"initialTurnsPage": map[string]any{"limit": float64(1), "sortDirection": "desc", "itemsView": "summary"},
		}
		if request.Method != "thread/resume" || !reflect.DeepEqual(request.Params, want) {
			serverErr <- errors.New("resume request was not the bounded recovery shape")
			return
		}
		response := `{"id":1,"result":{"thread":{"id":"thread-1","name":"Safe title","cwd":"/private/project","status":{"type":"active","activeFlags":["waitingOnApproval"]}},"initialTurnsPage":{"data":[{"id":"turn-9","status":"inProgress","items":[],"startedAt":1720000000}],"nextCursor":null,"backwardsCursor":null}}}` + "\n"
		_, err = server.Write([]byte(response))
		serverErr <- err
	}()

	snapshot, err := session.ResumeThreadSnapshot(ctx, "thread-1")
	if err != nil {
		t.Fatalf("ResumeThreadSnapshot: %v", err)
	}
	if snapshot.ID != "thread-1" || snapshot.Name != "Safe title" || snapshot.Status.Type != "active" || !reflect.DeepEqual(snapshot.Status.ActiveFlags, []string{"waitingOnApproval"}) {
		t.Fatalf("thread snapshot = %#v", snapshot)
	}
	if snapshot.LatestTurn == nil || snapshot.LatestTurn.ID != "turn-9" || snapshot.LatestTurn.Status != "inProgress" {
		t.Fatalf("latest turn = %#v", snapshot.LatestTurn)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestReadRateLimitsUsesThePinnedAccountMethodAndPreservesWindowFields(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	session := NewSession(client, Options{RPCTimeout: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	session.Start(ctx)

	serverErr := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(server).ReadBytes('\n')
		if err != nil {
			serverErr <- err
			return
		}
		var request map[string]any
		if err := json.Unmarshal(line, &request); err != nil {
			serverErr <- err
			return
		}
		if request["method"] != "account/rateLimits/read" {
			serverErr <- errors.New("wrong rate-limit method")
			return
		}
		if _, exists := request["params"]; exists {
			serverErr <- errors.New("parameterless rate-limit request included params")
			return
		}
		response := `{"id":1,"result":{"rateLimits":{"limitId":"legacy","primary":{"usedPercent":7,"windowDurationMins":300,"resetsAt":1787600000}},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":13,"windowDurationMins":300,"resetsAt":1787600000},"secondary":{"usedPercent":41,"windowDurationMins":10080,"resetsAt":1788200000}}},"rateLimitResetCredits":null}}` + "\n"
		_, err = server.Write([]byte(response))
		serverErr <- err
	}()

	response, err := session.ReadRateLimits(ctx)
	if err != nil {
		t.Fatalf("ReadRateLimits: %v", err)
	}
	selected, ok := response.CodexSnapshot()
	if !ok || selected.LimitID != "codex" || selected.Primary == nil || selected.Secondary == nil {
		t.Fatalf("selected Codex rate limits = %#v/%v", selected, ok)
	}
	if selected.Primary.UsedPercent != 13 || selected.Primary.WindowDurationMinutes != 300 || selected.Primary.ResetsAt != 1787600000 ||
		selected.Secondary.UsedPercent != 41 || selected.Secondary.WindowDurationMinutes != 10080 || selected.Secondary.ResetsAt != 1788200000 {
		t.Fatalf("selected Codex windows = %#v", selected)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestMergeRateLimitsTreatsRollingNotificationAsSparse(t *testing.T) {
	current := RateLimitSnapshot{
		LimitID:   "codex",
		Primary:   &RateLimitWindow{UsedPercent: 10, WindowDurationMinutes: 300, ResetsAt: 1787600000},
		Secondary: &RateLimitWindow{UsedPercent: 40, WindowDurationMinutes: 10080, ResetsAt: 1788200000},
	}
	update := RateLimitSnapshot{Primary: &RateLimitWindow{UsedPercent: 12}}
	merged := MergeRateLimits(current, update)
	if merged.LimitID != "codex" || merged.Primary == nil || merged.Secondary == nil ||
		merged.Primary.UsedPercent != 12 || merged.Primary.WindowDurationMinutes != 300 || merged.Primary.ResetsAt != 1787600000 ||
		merged.Secondary.UsedPercent != 40 {
		t.Fatalf("sparse merge = %#v", merged)
	}
}

func TestRespondPreservesServerRequestIDAndEncodesResult(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	session := NewSession(client, Options{})
	if err := session.handleLine(t.Context(), []byte(`{"id":"request-7","method":"approval","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	id := (<-session.Events()).ID
	written := make(chan []byte, 1)
	go func() {
		line, _ := bufio.NewReader(server).ReadBytes('\n')
		written <- line
	}()
	if err := session.Respond(t.Context(), id, map[string]bool{"accepted": true}); err != nil {
		t.Fatal(err)
	}
	if got, want := string(<-written), `{"id":"request-7","result":{"accepted":true}}`+"\n"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
	if err := session.Respond(t.Context(), RawID{}, nil); err == nil {
		t.Fatal("Respond accepted a missing request id")
	}
	parsed, err := ParseRawID(json.RawMessage(`"request-7"`))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Respond(t.Context(), parsed, nil); err == nil {
		t.Fatal("syntactic request ID without issuing connection authorized a response")
	}
}

func TestInterruptTurnSendsPinnedMethodAndPropagatesRPCFailure(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	session := NewSession(client, Options{RPCTimeout: time.Second})
	session.Start(t.Context())
	serverErr := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(server).ReadBytes('\n')
		if err != nil {
			serverErr <- err
			return
		}
		var request struct {
			ID     uint64         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(line, &request); err != nil {
			serverErr <- err
			return
		}
		if request.Method != "turn/interrupt" || request.Params["threadId"] != "thread-1" || request.Params["turnId"] != "turn-2" {
			serverErr <- errors.New("interrupt request did not match pinned contract")
			return
		}
		_, err = server.Write([]byte(`{"id":1,"error":{"code":-32001,"message":"turn already completed"}}` + "\n"))
		serverErr <- err
	}()
	err := session.InterruptTurn(t.Context(), "thread-1", "turn-2")
	rpcErr, matched := errors.AsType[*RPCError](err)
	if !matched || rpcErr.Code != -32001 || rpcErr.Message != "turn already completed" {
		t.Fatalf("InterruptTurn error = %#v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if err := session.InterruptTurn(t.Context(), "", "turn-2"); err == nil {
		t.Fatal("InterruptTurn accepted an empty thread id")
	}
}

func TestSessionDisconnectFailsPendingCallAndUnblocksWait(t *testing.T) {
	client, server := net.Pipe()
	session := NewSession(client, Options{RPCTimeout: time.Hour})
	session.Start(t.Context())
	callDone := make(chan error, 1)
	go func() { callDone <- session.Call(t.Context(), "thread/loaded/list", nil, nil) }()
	if _, err := bufio.NewReader(server).ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-callDone:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("pending call error = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending call remained blocked after disconnect")
	}
	if err := session.Wait(); !errors.Is(err, io.EOF) {
		t.Fatalf("Wait error = %v, want EOF", err)
	}
}

func TestSessionCloseIsIdempotentAndUnblocksWait(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	session := NewSession(client, Options{})
	session.Start(t.Context())
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Wait(); err != nil {
		t.Fatalf("Wait after Close = %v", err)
	}
}

func FuzzSessionEnvelope(f *testing.F) {
	for _, seed := range []string{
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{}}}`,
		`{"id":"request-1","method":"item/tool/requestUserInput","params":{}}`,
		`{"id":1,"result":{"ok":true}}`,
		`{"id":null,"result":null}`,
		`{`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > defaultMaxMessageBytes {
			t.Skip()
		}
		client, server := net.Pipe()
		t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
		session := NewSession(client, Options{EventBuffer: 1})
		err := session.handleLine(t.Context(), raw)
		if err != nil {
			return
		}
		select {
		case event := <-session.Events():
			if event.Method == "" || event.Kind != IncomingNotification && event.Kind != IncomingServerRequest {
				t.Fatalf("accepted invalid event: %#v", event)
			}
		default:
		}
	})
}
