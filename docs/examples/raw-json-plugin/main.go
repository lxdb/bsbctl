// raw-json-plugin is a transport example that intentionally uses only the Go
// standard library. It is not an independent conformance implementation.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

const (
	pluginID      = "dev.bsbctl.raw-example"
	pluginVersion = "1.0.0"
	protocolV1    = "1.0"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      string    `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "raw JSON plugin:", err)
		os.Exit(1)
	}
}

func run() error {
	fd := 3
	if value := os.Getenv("BSBCTL_RPC_FD"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 3 {
			return errors.New("invalid BSBCTL_RPC_FD")
		}
		fd = parsed
	}
	file := os.NewFile(uintptr(fd), "bsbctl-plugin-rpc")
	if file == nil {
		return errors.New("inherited RPC descriptor is unavailable")
	}
	connection, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		return err
	}
	defer connection.Close()
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	encoder := json.NewEncoder(connection)
	initialized := false
	for scanner.Scan() {
		var message request
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil || message.JSONRPC != "2.0" || message.Method == "" {
			return errors.New("invalid JSON-RPC request")
		}
		result, callErr, stop := dispatch(message, &initialized)
		if message.ID != "" {
			reply := response{JSONRPC: "2.0", ID: message.ID, Result: result}
			if callErr != nil {
				reply.Result = nil
				reply.Error = &rpcError{Code: -32602, Message: "invalid params"}
			}
			if err := encoder.Encode(reply); err != nil {
				return err
			}
		}
		if stop {
			return nil
		}
	}
	return scanner.Err()
}

func dispatch(message request, initialized *bool) (any, error, bool) {
	if message.Method == "plugin.initialize" {
		var params struct {
			PluginID        string `json:"plugin_id"`
			PluginVersion   string `json:"plugin_version"`
			ProtocolVersion string `json:"protocol_version"`
		}
		if *initialized || json.Unmarshal(message.Params, &params) != nil || params.PluginID != pluginID || params.PluginVersion != pluginVersion || params.ProtocolVersion != protocolV1 {
			return nil, errors.New("identity mismatch"), false
		}
		*initialized = true
		return map[string]any{
			"plugin_id": pluginID, "plugin_version": pluginVersion, "protocol_version": protocolV1,
			"execution_modes": []string{"resident", "interactive"},
			"channels":        []map[string]string{{"id": "main"}},
			"operations":      []map[string]string{{"id": "inspect", "kind": "query"}},
		}, nil, false
	}
	if !*initialized {
		return nil, errors.New("not initialized"), false
	}
	switch message.Method {
	case "plugin.instances.replace", "plugin.session.start", "plugin.session.end":
		return struct{}{}, nil, false
	case "plugin.session.input":
		return map[string]string{"disposition": "consumed"}, nil, false
	case "plugin.operation.invoke":
		return map[string]any{"payload": map[string]any{"implementation": "raw-json"}}, nil, false
	case "plugin.health":
		return map[string]any{"healthy": true, "observed_at": time.Now().UTC().Format(time.RFC3339Nano)}, nil, false
	case "plugin.shutdown":
		return struct{}{}, nil, true
	default:
		return nil, errors.New("unknown method"), false
	}
}
