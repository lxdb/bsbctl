package configschema

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAuthenticatesRegularSchemaBeforeCompiling(t *testing.T) {
	root := t.TempDir()
	data := []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"enabled":{"type":"boolean"}},"additionalProperties":false}`)
	path := filepath.Join(root, FileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	declaration := Declaration{Source: FileName, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}
	schema, err := Load(root, declaration)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate([]byte(`{"enabled":true}`)); err != nil {
		t.Fatalf("valid configuration: %v", err)
	}
	if err := schema.Validate([]byte(`{"unknown":true}`)); err == nil {
		t.Fatal("schema accepted an unknown property")
	}

	for _, test := range []struct {
		name   string
		root   string
		mutate func(Declaration) Declaration
	}{
		{name: "relative root", root: ".", mutate: func(value Declaration) Declaration { return value }},
		{name: "wrong digest", root: root, mutate: func(value Declaration) Declaration { value.SHA256 = strings.Repeat("0", 64); return value }},
		{name: "wrong size", root: root, mutate: func(value Declaration) Declaration { value.Size++; return value }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Load(test.root, test.mutate(declaration)); err == nil {
				t.Fatal("Load accepted unauthenticated schema")
			}
		})
	}
}

func TestLoadRejectsSchemaSymlink(t *testing.T) {
	root := t.TempDir()
	data := []byte(`{"type":"object"}`)
	target := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, FileName)); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if _, err := Load(root, Declaration{Source: FileName, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}); err == nil {
		t.Fatal("Load followed a schema symlink")
	}
}

func TestCompileRejectsExternalReferencesAndOversizedDocuments(t *testing.T) {
	if _, err := Compile([]byte(`{"$ref":"https://example.invalid/schema.json"}`)); err == nil {
		t.Fatal("Compile accepted an external schema reference")
	}
	if _, err := Compile(make([]byte, MaxBytes+1)); err == nil {
		t.Fatal("Compile accepted an oversized schema")
	}
}
