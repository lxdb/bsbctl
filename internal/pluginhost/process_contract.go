package pluginhost

import (
	"encoding/json"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func equalExecutionModes(a, b []protocol.ExecutionMode) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[protocol.ExecutionMode]int, len(a))
	for _, value := range a {
		counts[value]++
	}
	for _, value := range b {
		counts[value]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func equalChannels(a, b []protocol.Channel) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func equalOperations(a, b []protocol.OperationDescriptor) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
