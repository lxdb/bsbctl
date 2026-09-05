package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/lxdb/bsbctl/sdk/internal/jsonnames"
)

func decodeStrict(raw []byte, target any) error {
	if len(raw) == 0 {
		return errors.New("JSON value is required")
	}
	names := json.NewDecoder(bytes.NewReader(raw))
	names.UseNumber()
	if err := jsonnames.RejectUnknownFields(names, target); err != nil {
		return err
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := requireEnd(decoder); err != nil {
		return err
	}
	return nil
}

func rejectDuplicateNames(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := jsonnames.RejectDuplicates(decoder); err != nil {
		return err
	}
	return requireEnd(decoder)
}

func requireEnd(decoder *json.Decoder) error {
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}
