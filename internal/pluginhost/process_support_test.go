package pluginhost

import (
	"encoding/json"
	"github.com/lxdb/bsbctl/internal/presentation"
	"strings"
	"testing"
)

const hostileRPCSecret = "authorization=Bearer hostile-rpc-secret-canary"

func testProcessInstance(generation uint64) Instance {
	return Instance{
		ID: "app", Generation: generation, Config: json.RawMessage(`{}`),
		Policies: map[string]presentation.PolicyConfig{"main": {Policy: presentation.PolicyWhenRelevant}},
	}
}

func pluginObjectOfSize(t *testing.T, size int) json.RawMessage {
	t.Helper()
	const shell = `{"x":""}`
	if size < len(shell) {
		t.Fatalf("JSON object size %d is smaller than minimum %d", size, len(shell))
	}
	value := json.RawMessage(`{"x":"` + strings.Repeat("x", size-len(shell)) + `"}`)
	if len(value) != size || !json.Valid(value) {
		t.Fatalf("invalid JSON object fixture: %d bytes", len(value))
	}
	return value
}
