package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

type separateDeadlineConn struct {
	net.Conn
	readDeadline  time.Time
	writeDeadline time.Time
}

type retainedHistoryBackend struct {
	*fakeBackend
	traces []attention.Trace
}

func (b *retainedHistoryBackend) AttentionHistory(int, time.Time) []attention.Trace { return b.traces }

func TestAttentionHistoryFitsControlTransport(t *testing.T) {
	now := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	store := observation.NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	for i := range 64 {
		value := protocol.Observation{
			Instance: protocol.InstanceRef{ID: "codex", Generation: 1}, Channel: "activity", Key: fmt.Sprintf("thread-%03d", i), Revision: 1,
			Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNormal, ReasonCode: "codex_thread_active",
			ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(10 * time.Minute),
			Scene: &protocol.Scene{Elements: []protocol.Element{{ID: "status", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: "WORKING", Font: "normal"}}}},
		}
		if err := store.Publish(observation.Source{PluginID: "dev.bsbctl.codex", Generation: 1}, value); err != nil {
			t.Fatal(err)
		}
	}
	records := store.Snapshot()
	traces := make([]attention.Trace, 50)
	for i := range traces {
		at := now.Add(time.Duration(i) * time.Second)
		decision := attention.Select(records, func(observation.Record) (attention.Rule, bool) {
			return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyWhenRelevant}, true
		}, presentation.History{}, at)
		traces[i] = attention.Trace{Sequence: uint64(i + 1), At: at, SelectedID: decision.Candidate.ID(), Outcome: attention.OutcomeDrawn, Evaluations: decision.Evaluations}
	}
	data, err := json.Marshal(traces)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("default history workload: 50 traces x 64 observations = %d bytes (RPC limit %d)", len(data), rpc.MaxMessageBytes)
	backend := &retainedHistoryBackend{fakeBackend: &fakeBackend{document: emptyControlDocument()}, traces: traces}
	serverConn, clientConn := net.Pipe()
	server := newServer("", "audit", testBackends(backend), nil, nil, defaultControlOptions())
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	serverDone := make(chan struct{})
	go func() { defer close(serverDone); server.serveConn(ctx, serverConn) }()
	client := rpc.NewPeer(clientConn)
	clientDone := make(chan struct{})
	go func() { defer close(clientDone); _ = client.Serve(ctx) }()
	t.Cleanup(func() { _ = client.Close(); _ = serverConn.Close(); cancel(); <-clientDone; <-serverDone })
	var result AttentionHistoryResult
	if err := client.Call(ctx, "attention.history", AttentionHistoryRequest{Limit: 50}, &result); err != nil {
		t.Fatalf("valid default history request closed control connection: %v", err)
	}
	if !result.Truncated || len(result.Traces) == 0 || len(result.Traces) >= len(traces) || result.Traces[len(result.Traces)-1].Sequence != 50 {
		t.Fatalf("history did not retain a bounded newest suffix: %+v", result)
	}
	for index, trace := range result.Traces {
		if trace.Sequence != uint64(51-len(result.Traces)+index) || len(trace.Evaluations) != 64 {
			t.Fatalf("trace details or order changed: %+v", trace)
		}
	}
}

func (c *separateDeadlineConn) SetDeadline(value time.Time) error {
	c.readDeadline, c.writeDeadline = value, value
	return nil
}
func (c *separateDeadlineConn) SetReadDeadline(value time.Time) error {
	c.readDeadline = value
	return nil
}
func (c *separateDeadlineConn) SetWriteDeadline(value time.Time) error {
	c.writeDeadline = value
	return nil
}
func (*separateDeadlineConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (*separateDeadlineConn) Write(p []byte) (int, error) { return len(p), nil }

func TestActivityDoesNotOverwriteRPCWriteDeadline(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, operation := range []string{"read", "write"} {
		t.Run(operation, func(t *testing.T) {
			conn := &separateDeadlineConn{}
			wrapped := newActivityConn(conn, 30*time.Second, func() time.Time { return now })
			want := now.Add(2 * time.Second)
			if err := wrapped.SetWriteDeadline(want); err != nil {
				t.Fatal(err)
			}
			if operation == "read" {
				_, _ = wrapped.Read(make([]byte, 1))
			} else {
				_, _ = wrapped.Write([]byte("x"))
			}
			if !conn.writeDeadline.Equal(want) {
				t.Fatalf("%s changed RPC write budget from %v to %v", operation, want, conn.writeDeadline)
			}
		})
	}
}

func TestHistoryBudgetKeepsOnlyAContiguousSuffixAndFitsLargestRPCID(t *testing.T) {
	empty, rpcErr := boundAttentionHistory(nil)
	data, err := json.Marshal(empty)
	if err != nil || rpcErr != nil || string(data) != `{"traces":[],"truncated":false}` {
		t.Fatalf("empty history = %s, %v, %v", data, err, rpcErr)
	}
	trace := attention.Trace{Sequence: 1, SelectedID: "x", Evaluations: []attention.Evaluation{}}
	base, err := json.Marshal(AttentionHistoryResult{Traces: []attention.Trace{trace}})
	if err != nil {
		t.Fatal(err)
	}
	trace.SelectedID = strings.Repeat("x", 1+attentionHistoryResultBytes-len(base))
	exact, rpcErr := boundAttentionHistory([]attention.Trace{trace})
	data, err = json.Marshal(exact)
	if err != nil || rpcErr != nil || exact.Truncated || len(exact.Traces) != 1 || len(data) != attentionHistoryResultBytes {
		t.Fatalf("exact fit: bytes=%d, truncated=%v, count=%d, errors=%v/%v", len(data), exact.Truncated, len(exact.Traces), err, rpcErr)
	}
	envelope, err := json.Marshal(struct {
		Version string                 `json:"jsonrpc"`
		ID      uint64                 `json:"id"`
		Result  AttentionHistoryResult `json:"result"`
	}{"2.0", math.MaxUint64, exact})
	if err != nil || len(envelope)+1 > rpc.MaxMessageBytes {
		t.Fatalf("largest-ID envelope: bytes=%d, error=%v", len(envelope)+1, err)
	}
	trace.SelectedID += "x"
	if _, rpcErr := boundAttentionHistory([]attention.Trace{trace}); rpcErr == nil || rpcErr.Code != -32055 {
		t.Fatalf("oversized newest trace = %v", rpcErr)
	}
	trace.Sequence = 2
	suffix, rpcErr := boundAttentionHistory([]attention.Trace{{Sequence: 1}, trace, {Sequence: 3}})
	if rpcErr != nil || !suffix.Truncated || len(suffix.Traces) != 1 || suffix.Traces[0].Sequence != 3 {
		t.Fatalf("oversized middle trace created a gap: %+v, %v", suffix, rpcErr)
	}
}

func TestOversizedHistoryErrorKeepsTheControlPeerUsable(t *testing.T) {
	backend := &retainedHistoryBackend{fakeBackend: &fakeBackend{document: emptyControlDocument()}, traces: []attention.Trace{{Sequence: 1, SelectedID: strings.Repeat("x", rpc.MaxMessageBytes)}}}
	serverConn, clientConn := net.Pipe()
	server := newServer("", "history-test", testBackends(backend), nil, nil, defaultControlOptions())
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	serverDone := make(chan struct{})
	go func() { defer close(serverDone); server.serveConn(ctx, serverConn) }()
	client := rpc.NewPeer(clientConn)
	clientDone := make(chan struct{})
	go func() { defer close(clientDone); _ = client.Serve(ctx) }()
	t.Cleanup(func() { _ = client.Close(); _ = serverConn.Close(); cancel(); <-clientDone; <-serverDone })
	var result AttentionHistoryResult
	callErr := client.Call(ctx, "attention.history", AttentionHistoryRequest{Limit: 1}, &result)
	if rpcErr, ok := errors.AsType[*rpc.Error](callErr); !ok || rpcErr.Code != -32055 {
		t.Fatalf("oversized history error = %v", callErr)
	}
	var status Status
	if err := client.Call(ctx, "daemon.status", nil, &status); err != nil || status.Version != "history-test" {
		t.Fatalf("same peer after bounded error = %+v, %v", status, err)
	}
}
