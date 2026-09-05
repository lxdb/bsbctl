package rpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/lxdb/bsbctl/sdk/internal/jsonnames"
)

func marshalOptional(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if !isJSONObject(raw) {
		return nil, errors.New("json-rpc params must be an object")
	}
	return raw, nil
}

func decodeMessage(raw []byte) (message, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return message{}, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return message{}, errors.New("json-rpc envelope must be one object")
	}
	var msg message
	seen := make(map[string]struct{}, 7)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return message{}, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return message{}, errors.New("json-rpc envelope key must be a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return message{}, fmt.Errorf("duplicate json-rpc envelope key %q", key)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return message{}, err
		}
		switch key {
		case "jsonrpc":
			if err := json.Unmarshal(value, &msg.JSONRPC); err != nil {
				return message{}, errors.New("jsonrpc must be a string")
			}
		case "id":
			msg.ID, msg.hasID = value, true
		case "method":
			if err := json.Unmarshal(value, &msg.Method); err != nil {
				return message{}, errors.New("method must be a string")
			}
		case "params":
			msg.Params, msg.hasParams = value, true
		case "result":
			msg.Result, msg.hasResult = value, true
		case "error":
			msg.hasError = true
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || decodeStrict(value, &msg.Error) != nil {
				return message{}, errors.New("error must be a canonical error object")
			}
		case "bsbctl":
			msg.hasMetadata = true
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || decodeStrict(value, &msg.Metadata) != nil {
				return message{}, errors.New("bsbctl metadata must be an object")
			}
		default:
			return message{}, fmt.Errorf("unknown json-rpc envelope key %q", key)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return message{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return message{}, errors.New("multiple JSON values are not allowed")
	}
	if err := validateMessage(msg); err != nil {
		return message{}, err
	}
	return msg, nil
}

func validateMessage(msg message) error {
	if msg.JSONRPC != Version {
		return errors.New("jsonrpc must equal 2.0")
	}
	if msg.Method != "" {
		if msg.hasResult || msg.hasError {
			return errors.New("request must not contain result or error")
		}
		if msg.hasID {
			if _, err := requestID(msg.ID); err != nil {
				return err
			}
		}
		if msg.hasParams && !isJSONObject(msg.Params) {
			return errors.New("params must be an object")
		}
		if msg.Metadata != nil && msg.Metadata.DeadlineUnixMilliseconds <= 0 {
			return errors.New("deadline_unix_milliseconds must be positive")
		}
		return nil
	}
	if !msg.hasID {
		return errors.New("response id is required")
	}
	if _, err := requestID(msg.ID); err != nil {
		return err
	}
	if msg.hasParams {
		return errors.New("response must not contain params")
	}
	if msg.hasMetadata {
		return errors.New("response must not contain bsbctl request metadata")
	}
	if msg.hasResult == msg.hasError {
		return errors.New("response must contain exactly one of result or error")
	}
	return nil
}

func requestID(raw json.RawMessage) (string, error) {
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", errors.New("request id must be a decimal string")
	}
	value, err := strconv.ParseUint(id, 10, 64)
	if err != nil || value == 0 || strconv.FormatUint(value, 10) != id {
		return "", errors.New("request id must be a canonical positive decimal uint64")
	}
	return id, nil
}

func decodeStrict(raw []byte, target any) error {
	names := json.NewDecoder(bytes.NewReader(raw))
	names.UseNumber()
	if err := jsonnames.RejectUnknownFields(names, target); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("multiple JSON values are not allowed")
	}
	return nil
}

func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}

func looksLikeResponse(raw []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	_, hasMethod := fields["method"]
	_, hasID := fields["id"]
	return hasID && !hasMethod
}
