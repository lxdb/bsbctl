package main

import (
	"bytes"
	"context"
	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/control"
	"strings"
	"testing"
	"time"
)

func TestAttentionReadCommandsRouteAndRenderPublicResults(t *testing.T) {
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	client := &fakeCLIClient{call: func(_ context.Context, method string, params, result any) error {
		switch method {
		case "attention.snapshot":
			if params != nil {
				t.Fatalf("snapshot params = %#v, want nil", params)
			}
			*(result.(*attention.Trace)) = attention.Trace{Sequence: 7, At: now, SelectedID: "obs-1", Outcome: attention.OutcomeDrawn, Evaluations: []attention.Evaluation{}}
		case "attention.explain":
			request := params.(control.AttentionExplainRequest)
			if request.ObservationID != "obs-1" {
				t.Fatalf("explain request = %#v", request)
			}
			*(result.(*attention.Evaluation)) = attention.Evaluation{ObservationID: "obs-1", PluginID: "plugin", InstanceID: "app", Channel: "main", Reason: attention.ReasonSelected, EvaluatedAt: now}
		case "attention.history":
			request := params.(control.AttentionHistoryRequest)
			if request.Limit != 2 || !request.Since.IsZero() {
				t.Fatalf("history request = %#v", request)
			}
			*(result.(*control.AttentionHistoryResult)) = control.AttentionHistoryResult{Traces: []attention.Trace{{Sequence: 6, At: now.Add(-time.Minute), Evaluations: []attention.Evaluation{}}}, Truncated: true}
		default:
			t.Fatalf("method = %q", method)
		}
		return nil
	}}
	restore := installCLIClient(t, client)
	t.Cleanup(restore)

	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"attention", "status", "--socket", "/ignored"}, want: `{"sequence":7,"at":"2026-09-02T12:00:00Z","selected_id":"obs-1","outcome":"drawn","evaluations":[]}` + "\n"},
		{args: []string{"attention", "explain", "obs-1", "--socket", "/ignored"}, want: `{"observation_id":"obs-1","plugin_id":"plugin","instance_id":"app","channel":"main","disposition":"","impact":"","reason_code":"","reason":"selected","evaluated_at":"2026-09-02T12:00:00Z","cooldown_until":"0001-01-01T00:00:00Z","next_due":"0001-01-01T00:00:00Z"}` + "\n"},
		{args: []string{"attention", "history", "--limit", "2", "--socket", "/ignored"}, want: `{"traces":[{"sequence":6,"at":"2026-09-02T11:59:00Z","evaluations":[]}],"truncated":true}` + "\n"},
	}
	for _, test := range tests {
		var stdout, stderr bytes.Buffer
		if code := execute(t.Context(), test.args, strings.NewReader(""), &stdout, &stderr); code != 0 || stdout.String() != test.want || stderr.Len() != 0 {
			t.Fatalf("execute(%v) = code %d stdout %q stderr %q", test.args, code, stdout.String(), stderr.String())
		}
	}
}

func TestAttentionReadCommandsRejectInvalidArgumentsBeforeDial(t *testing.T) {
	called := false
	restore := installCLIClient(t, &fakeCLIClient{call: func(context.Context, string, any, any) error {
		called = true
		return nil
	}})
	t.Cleanup(restore)
	for _, args := range [][]string{
		{"attention", "status", "extra", "--socket", "/ignored"},
		{"attention", "explain", "--socket", "/ignored"},
		{"attention", "history", "--limit", "0", "--socket", "/ignored"},
		{"attention", "history", "--since", "later", "--socket", "/ignored"},
	} {
		var stdout, stderr bytes.Buffer
		if code := execute(t.Context(), args, strings.NewReader(""), &stdout, &stderr); code != exitUsage || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("execute(%v) = code %d stdout %q stderr %q", args, code, stdout.String(), stderr.String())
		}
	}
	if called {
		t.Fatal("invalid attention command reached daemon")
	}
}
