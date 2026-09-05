package releasekeys

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

func TestDecodeCatalogKeyringAcceptsCanonicalEd25519Keys(t *testing.T) {
	t.Parallel()
	first := bytes.Repeat([]byte{0x11}, ed25519.PublicKeySize)
	second := bytes.Repeat([]byte{0x22}, ed25519.PublicKeySize)
	document := []byte(`{"schema_version":1,"keys":[` +
		`{"id":"stable-2026","algorithm":"ed25519","public_key_base64":"` + base64.StdEncoding.EncodeToString(first) + `"},` +
		`{"id":"stable-2027","algorithm":"ed25519","public_key_base64":"` + base64.StdEncoding.EncodeToString(second) + `"}]}`)

	keyring, err := DecodeCatalogKeyring(document)
	if err != nil {
		t.Fatalf("DecodeCatalogKeyring: %v", err)
	}
	if len(keyring) != 2 || !bytes.Equal(keyring["stable-2026"], first) || !bytes.Equal(keyring["stable-2027"], second) {
		t.Fatalf("keyring = %#v", keyring)
	}
	first[0] = 0xff
	if keyring["stable-2026"][0] != 0x11 {
		t.Fatal("decoded key aliases caller-owned input")
	}
}

func TestDecodeCatalogKeyringAllowsExplicitEmptyTrustSet(t *testing.T) {
	t.Parallel()
	keyring, err := DecodeCatalogKeyring([]byte(`{"schema_version":1,"keys":[]}`))
	if err != nil {
		t.Fatalf("DecodeCatalogKeyring: %v", err)
	}
	if len(keyring) != 0 {
		t.Fatalf("keyring = %#v, want empty", keyring)
	}
}

func TestDecodeCatalogKeyringRejectsAmbiguousOrInvalidDocuments(t *testing.T) {
	t.Parallel()
	validKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x33}, ed25519.PublicKeySize))
	tests := []struct {
		name string
		data string
	}{
		{name: "empty", data: ``},
		{name: "unknown field", data: `{"schema_version":1,"keys":[],"fallback":true}`},
		{name: "wrong schema", data: `{"schema_version":2,"keys":[]}`},
		{name: "missing keys", data: `{"schema_version":1}`},
		{name: "duplicate id", data: `{"schema_version":1,"keys":[{"id":"stable","algorithm":"ed25519","public_key_base64":"` + validKey + `"},{"id":"stable","algorithm":"ed25519","public_key_base64":"` + validKey + `"}]}`},
		{name: "unsafe id", data: `{"schema_version":1,"keys":[{"id":"../stable","algorithm":"ed25519","public_key_base64":"` + validKey + `"}]}`},
		{name: "wrong algorithm", data: `{"schema_version":1,"keys":[{"id":"stable","algorithm":"rsa","public_key_base64":"` + validKey + `"}]}`},
		{name: "wrong length", data: `{"schema_version":1,"keys":[{"id":"stable","algorithm":"ed25519","public_key_base64":"YQ=="}]}`},
		{name: "noncanonical base64", data: `{"schema_version":1,"keys":[{"id":"stable","algorithm":"ed25519","public_key_base64":"` + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{0x33}, ed25519.PublicKeySize)) + `"}]}`},
		{name: "trailing JSON", data: `{"schema_version":1,"keys":[]} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeCatalogKeyring([]byte(test.data)); err == nil {
				t.Fatal("DecodeCatalogKeyring accepted invalid document")
			}
		})
	}
}
