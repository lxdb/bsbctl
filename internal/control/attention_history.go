package control

import (
	"encoding/json"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

// Reserve the complete JSON-RPC envelope, including its largest uint64 ID.
const attentionHistoryResultBytes = rpc.MaxMessageBytes - 128

func boundAttentionHistory(traces []attention.Trace) (AttentionHistoryResult, *rpc.Error) {
	used := len(`{"traces":[],"truncated":false}`)
	start := len(traces)
	for start > 0 {
		data, err := json.Marshal(traces[start-1])
		if err != nil {
			return AttentionHistoryResult{}, &rpc.Error{Code: -32603, Message: "attention history encoding failed"}
		}
		separator := 0
		if start < len(traces) {
			separator = 1
		}
		if len(data) > attentionHistoryResultBytes-used-separator {
			break
		}
		used += len(data) + separator
		start--
	}
	if len(traces) != 0 && start == len(traces) {
		return AttentionHistoryResult{}, &rpc.Error{Code: -32055, Message: "attention history trace exceeds response limit"}
	}
	result := AttentionHistoryResult{Traces: traces[start:], Truncated: start != 0}
	if result.Traces == nil {
		result.Traces = []attention.Trace{}
	}
	return result, nil
}
