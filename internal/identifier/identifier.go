// Package identifier validates and encodes identifiers used at trust boundaries.
package identifier

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxBytes = 128

func Validate(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if len(value) > MaxBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, MaxBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", name)
	}
	for _, value := range value {
		if unicode.IsControl(value) {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}

func Encode(parts ...string) (string, error) {
	if len(parts) == 0 {
		return "", errors.New("identifier requires at least one part")
	}
	encoded := make([]string, len(parts))
	for index, part := range parts {
		encoded[index] = url.PathEscape(part)
	}
	return strings.Join(encoded, "/"), nil
}
