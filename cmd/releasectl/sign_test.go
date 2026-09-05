package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/catalog"
)

func TestRunSignCatalogConsumesCanonicalPrivateKeyOnlyFromStdin(t *testing.T) {
	directory := t.TempDir()
	catalogPath := filepath.Join(directory, "catalog.json")
	signaturePath := filepath.Join(directory, "catalog.sig")
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	catalogData := []byte(`{"version":1,"channel":"stable","sequence":1,"generated_at":"2026-08-22T11:00:00Z","plugins":[{"id":"dev.bsbctl.codex-quota","version":"0.1.0","os":"darwin","arch":"arm64","url":"https://example.invalid/plugin.tar.gz","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","compressed_size":1,"archive_format":"tar.gz","executable":"bsbctl-plugin-codex-quota","manifest":"manifest.json"}]}`)
	if err := os.WriteFile(catalogPath, catalogData, 0o600); err != nil {
		t.Fatal(err)
	}
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
	encoded := base64.StdEncoding.EncodeToString(private)
	installReleaseKeyring(t, catalog.Keyring{"stable-2026": private.Public().(ed25519.PublicKey)})

	var stdout, stderr bytes.Buffer
	code := runWithInput(context.Background(), []string{"sign-catalog", "--catalog", catalogPath, "--key-id", "stable-2026", "--out", signaturePath}, strings.NewReader(encoded), &stdout, &stderr)
	if code != exitSuccess || stdout.String() != "catalog signature: written\n" || stderr.Len() != 0 {
		t.Fatalf("sign exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	envelope, err := os.ReadFile(signaturePath)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := catalog.Verify(catalogData, envelope, catalog.Keyring{"stable-2026": private.Public().(ed25519.PublicKey)}, 0, now)
	if err != nil {
		t.Fatalf("catalog.Verify signed output: %v", err)
	}
	if verified.Sequence != 1 {
		t.Fatalf("verified sequence = %d, want 1", verified.Sequence)
	}
}

func TestRunSignCatalogRejectsPrivateKeyThatDoesNotMatchTrackedKeyID(t *testing.T) {
	directory := t.TempDir()
	catalogPath := filepath.Join(directory, "catalog.json")
	signaturePath := filepath.Join(directory, "catalog.sig")
	if err := os.WriteFile(catalogPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x24}, ed25519.SeedSize))
	installReleaseKeyring(t, catalog.Keyring{"stable-2026": other.Public().(ed25519.PublicKey)})

	var stdout, stderr bytes.Buffer
	code := runWithInput(context.Background(), []string{"sign-catalog", "--catalog", catalogPath, "--key-id", "stable-2026", "--out", signaturePath}, strings.NewReader(base64.StdEncoding.EncodeToString(private)), &stdout, &stderr)
	if code != exitFailure || stdout.Len() != 0 || stderr.String() != "catalog signing failed: signing key is not authorized\n" {
		t.Fatalf("sign exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(signaturePath); !os.IsNotExist(err) {
		t.Fatalf("unauthorized signature output exists: %v", err)
	}
}

func TestRunSignCatalogRejectsMissingNoncanonicalOrArgumentPrivateKeys(t *testing.T) {
	directory := t.TempDir()
	catalogPath := filepath.Join(directory, "catalog.json")
	if err := os.WriteFile(catalogPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x24}, ed25519.SeedSize))
	encoded := base64.StdEncoding.EncodeToString(private)
	installReleaseKeyring(t, catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)})
	tests := []struct {
		name  string
		args  []string
		stdin string
	}{
		{name: "missing", args: []string{"sign-catalog", "--catalog", catalogPath, "--key-id", "stable", "--out", filepath.Join(directory, "missing.sig")}},
		{name: "trailing whitespace", args: []string{"sign-catalog", "--catalog", catalogPath, "--key-id", "stable", "--out", filepath.Join(directory, "whitespace.sig")}, stdin: encoded + "\n"},
		{name: "argument", args: []string{"sign-catalog", "--catalog", catalogPath, "--key-id", "stable", "--out", filepath.Join(directory, "argument.sig"), "--private-key", encoded}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runWithInput(context.Background(), test.args, strings.NewReader(test.stdin), &stdout, &stderr)
			if code != exitFailure || stdout.Len() != 0 || !strings.Contains(stderr.String(), "catalog signing failed") {
				t.Fatalf("sign exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestReadSigningKeyNeverMaterializesImmutableSecretStrings(t *testing.T) {
	t.Parallel()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test source path is unavailable")
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(testFile), "sign.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var target *ast.FuncDecl
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "readSigningKey" {
			target = function
			break
		}
	}
	if target == nil {
		t.Fatal("readSigningKey is missing")
	}
	var unsafe []string
	ast.Inspect(target.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if identifier, ok := call.Fun.(*ast.Ident); ok && identifier.Name == "string" {
			unsafe = append(unsafe, "string conversion")
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "EncodeToString" {
			unsafe = append(unsafe, "base64 string encoding")
		}
		return true
	})
	if len(unsafe) != 0 {
		t.Fatalf("readSigningKey retains private material in immutable representations: %v", unsafe)
	}
}

func TestRunVerifyCatalogUsesTrackedKeyringAndProductionVerifier(t *testing.T) {
	directory := t.TempDir()
	catalogPath := filepath.Join(directory, "catalog.json")
	signaturePath := filepath.Join(directory, "catalog.sig")
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	catalogData := []byte(`{"version":1,"channel":"stable","sequence":7,"generated_at":"2026-08-22T11:00:00Z","plugins":[{"id":"dev.bsbctl.codex-quota","version":"0.1.0","os":"darwin","arch":"arm64","url":"https://example.invalid/plugin.tar.gz","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","compressed_size":1,"archive_format":"tar.gz","executable":"bsbctl-plugin-codex-quota","manifest":"manifest.json"}]}`)
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
	envelope := []byte(`{"key_id":"stable-2026","algorithm":"ed25519","signature":"` + base64.StdEncoding.EncodeToString(ed25519.Sign(private, catalogData)) + `"}`)
	if err := os.WriteFile(catalogPath, catalogData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signaturePath, envelope, 0o600); err != nil {
		t.Fatal(err)
	}
	installReleaseKeyring(t, catalog.Keyring{"stable-2026": private.Public().(ed25519.PublicKey)})
	previousClock := catalogVerificationClock
	catalogVerificationClock = func() time.Time { return now }
	t.Cleanup(func() { catalogVerificationClock = previousClock })

	var stdout, stderr bytes.Buffer
	code := run([]string{"verify-catalog", "--catalog", catalogPath, "--signature", signaturePath}, &stdout, &stderr)
	if code != exitSuccess || stdout.String() != "stable catalog: verified sequence 7\n" || stderr.Len() != 0 {
		t.Fatalf("verify exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	if err := os.WriteFile(catalogPath, append(catalogData, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	code = run([]string{"verify-catalog", "--catalog", catalogPath, "--signature", signaturePath}, &stdout, &stderr)
	if code != exitFailure || stdout.Len() != 0 || stderr.String() != "catalog verification failed\n" {
		t.Fatalf("tampered verify exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func installReleaseKeyring(t *testing.T, keyring catalog.Keyring) {
	t.Helper()
	previous := catalogKeyringLoader
	catalogKeyringLoader = func() (catalog.Keyring, error) { return keyring, nil }
	t.Cleanup(func() { catalogKeyringLoader = previous })
}
