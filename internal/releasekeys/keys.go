// Package releasekeys loads the tracked catalog verification trust set.
package releasekeys

import (
	"bytes"
	"crypto/ed25519"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/lxdb/bsbctl/internal/catalog"
)

var keyIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

//go:embed catalog_public_keys.json
var catalogPublicKeysJSON []byte

type keyDocument struct {
	SchemaVersion int         `json:"schema_version"`
	Keys          *[]keyEntry `json:"keys"`
}

type keyEntry struct {
	ID              string `json:"id"`
	Algorithm       string `json:"algorithm"`
	PublicKeyBase64 string `json:"public_key_base64"`
}

// CatalogKeyring returns the immutable public verification keys embedded in
// the binary at build time. It never reads a developer path or environment.
func CatalogKeyring() (catalog.Keyring, error) {
	return DecodeCatalogKeyring(catalogPublicKeysJSON)
}

// DecodeCatalogKeyring strictly decodes the tracked key document.
func DecodeCatalogKeyring(data []byte) (catalog.Keyring, error) {
	document := keyDocument{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("catalog public-key document is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("catalog public-key document is invalid")
	}
	if document.SchemaVersion != 1 || document.Keys == nil {
		return nil, errors.New("catalog public-key document is invalid")
	}
	keyring := make(catalog.Keyring, len(*document.Keys))
	for _, entry := range *document.Keys {
		if !keyIDPattern.MatchString(entry.ID) || entry.Algorithm != "ed25519" || strings.TrimSpace(entry.PublicKeyBase64) != entry.PublicKeyBase64 {
			return nil, errors.New("catalog public-key entry is invalid")
		}
		if _, exists := keyring[entry.ID]; exists {
			return nil, fmt.Errorf("catalog public-key id %q is duplicated", entry.ID)
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(entry.PublicKeyBase64)
		if err != nil || len(decoded) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(decoded) != entry.PublicKeyBase64 {
			return nil, fmt.Errorf("catalog public key %q is invalid", entry.ID)
		}
		keyring[entry.ID] = ed25519.PublicKey(slices.Clone(decoded))
	}
	return keyring, nil
}
