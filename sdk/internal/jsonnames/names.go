// Package jsonnames checks JSON object names before typed or raw-value decoding.
package jsonnames

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// RejectDuplicates consumes one JSON value, rejecting duplicate names in every
// object, including objects nested in arrays. Callers configure the decoder's
// number handling and validate any trailing input.
func RejectDuplicates(decoder *json.Decoder) error {
	return rejectNames(decoder, nil)
}

// RejectUnknownFields also rejects case aliases of typed JSON fields. Map
// keys and custom JSON representations remain owned by their value decoder.
func RejectUnknownFields(decoder *json.Decoder, target any) error {
	return rejectNames(decoder, reflect.TypeOf(target))
}

func rejectNames(decoder *json.Decoder, target reflect.Type) error {
	target = structuralType(target)
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delimiter {
	case '{':
		var fields map[string]reflect.Type
		var element reflect.Type
		if target != nil {
			switch target.Kind() {
			case reflect.Struct:
				fields = jsonFields(target)
			case reflect.Map:
				element = target.Elem()
			}
		}
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return errors.New("JSON object name is invalid")
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("duplicate JSON object name %q", name)
			}
			seen[name] = struct{}{}
			valueType := element
			if fields != nil {
				var exists bool
				valueType, exists = fields[name]
				if !exists {
					return fmt.Errorf("json: unknown field %q", name)
				}
			}
			if err := rejectNames(decoder, valueType); err != nil {
				return err
			}
		}
		return requireDelimiter(decoder, '}')
	case '[':
		var element reflect.Type
		if target != nil && (target.Kind() == reflect.Slice || target.Kind() == reflect.Array) {
			element = target.Elem()
		}
		for decoder.More() {
			if err := rejectNames(decoder, element); err != nil {
				return err
			}
		}
		return requireDelimiter(decoder, ']')
	default:
		return errors.New("invalid JSON delimiter")
	}
}

func requireDelimiter(decoder *json.Decoder, expected json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != expected {
		return errors.New("invalid JSON delimiter")
	}
	return nil
}
