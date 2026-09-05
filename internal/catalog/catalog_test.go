package catalog

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

var catalogNow = time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC)

func TestVerifyAuthenticatesStableCatalogAndArtifact(t *testing.T) {
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("s", ed25519.SeedSize)))
	data := validCatalogJSON()
	envelope := signatureEnvelope(private, data, "stable-2026")

	verified, err := Verify(data, envelope, Keyring{"stable-2026": private.Public().(ed25519.PublicKey)}, 6, catalogNow)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	entry, err := verified.Resolve("dev.bsbctl.ball8", "1.0.0", "darwin", "arm64")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if entry.CompressedSize != 5 || entry.ArchiveFormat != "tar.gz" || entry.Manifest != "manifest.json" {
		t.Fatalf("entry = %#v", entry)
	}
}

func TestVerifyRejectsAuthenticationAndFreshnessFailures(t *testing.T) {
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("s", ed25519.SeedSize)))
	public := private.Public().(ed25519.PublicKey)
	data := validCatalogJSON()
	validEnvelope := signatureEnvelope(private, data, "stable-2026")

	tests := []struct {
		name     string
		data     []byte
		envelope []byte
		keys     Keyring
		sequence uint64
		now      time.Time
	}{
		{name: "tampered exact bytes", data: append(append([]byte(nil), data...), ' '), envelope: validEnvelope, keys: Keyring{"stable-2026": public}, sequence: 6, now: catalogNow},
		{name: "unknown key", data: data, envelope: validEnvelope, keys: Keyring{}, sequence: 6, now: catalogNow},
		{name: "stale sequence", data: data, envelope: validEnvelope, keys: Keyring{"stable-2026": public}, sequence: 7, now: catalogNow},
		{name: "wrong algorithm", data: data, envelope: []byte(`{"key_id":"stable-2026","algorithm":"rsa","signature":"` + base64.StdEncoding.EncodeToString(ed25519.Sign(private, data)) + `"}`), keys: Keyring{"stable-2026": public}, sequence: 6, now: catalogNow},
		{name: "unknown envelope field", data: data, envelope: append(validEnvelope[:len(validEnvelope)-1], []byte(`,"url":"https://secret.invalid"}`)...), keys: Keyring{"stable-2026": public}, sequence: 6, now: catalogNow},
		{name: "duplicate envelope field", data: data, envelope: []byte(`{"key_id":"stable-2026","key_id":"stable-2026","algorithm":"ed25519","signature":"` + base64.StdEncoding.EncodeToString(ed25519.Sign(private, data)) + `"}`), keys: Keyring{"stable-2026": public}, sequence: 6, now: catalogNow},
		{name: "noncanonical signature", data: data, envelope: []byte(`{"key_id":"stable-2026","algorithm":"ed25519","signature":"` + base64.RawStdEncoding.EncodeToString(ed25519.Sign(private, data)) + `"}`), keys: Keyring{"stable-2026": public}, sequence: 6, now: catalogNow},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Verify(test.data, test.envelope, test.keys, test.sequence, test.now); err == nil {
				t.Fatal("Verify accepted invalid catalog authentication/freshness")
			}
		})
	}
}

func TestVerifyRejectsInvalidStableCatalogShape(t *testing.T) {
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("s", ed25519.SeedSize)))
	valid := string(validCatalogJSON())
	tests := []struct{ name, data string }{
		{name: "unknown field", data: strings.Replace(valid, `"plugins":`, `"beta":true,"plugins":`, 1)},
		{name: "empty platform entries", data: strings.Replace(valid, validEntryJSON(), ``, 1)},
		{name: "duplicate field", data: strings.Replace(valid, `"channel":"stable"`, `"channel":"stable","channel":"stable"`, 1)},
		{name: "duplicate entry", data: strings.Replace(valid, `]}`, `,`+validEntryJSON()+`]}`, 1)},
		{name: "non stable", data: strings.Replace(valid, `"channel":"stable"`, `"channel":"beta"`, 1)},
		{name: "zero sequence", data: strings.Replace(valid, `"sequence":7`, `"sequence":0`, 1)},
		{name: "future generated", data: strings.Replace(valid, `2026-08-22T05:00:00Z`, `2026-08-22T06:01:00Z`, 1)},
		{name: "unsafe id", data: strings.Replace(valid, `dev.bsbctl.ball8`, `../ball8`, 1)},
		{name: "unsafe version", data: strings.Replace(valid, `1.0.0`, `../1`, 1)},
		{name: "wrong os", data: strings.Replace(valid, `"os":"darwin"`, `"os":"linux"`, 1)},
		{name: "wrong arch", data: strings.Replace(valid, `"arch":"arm64"`, `"arch":"386"`, 1)},
		{name: "credentialed url", data: strings.Replace(valid, `https://example.invalid`, `https://user:secret@example.invalid`, 1)},
		{name: "http url", data: strings.Replace(valid, `https://example.invalid`, `http://example.invalid`, 1)},
		{name: "wrong archive", data: strings.Replace(valid, `"archive_format":"tar.gz"`, `"archive_format":"zip"`, 1)},
		{name: "wrong manifest", data: strings.Replace(valid, `"manifest":"manifest.json"`, `"manifest":"plugin.json"`, 1)},
		{name: "zero size", data: strings.Replace(valid, `"compressed_size":5`, `"compressed_size":0`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := []byte(test.data)
			envelope := signatureEnvelope(private, data, "stable-2026")
			if _, err := Verify(data, envelope, Keyring{"stable-2026": private.Public().(ed25519.PublicKey)}, 6, catalogNow); err == nil {
				t.Fatal("Verify accepted invalid catalog")
			}
		})
	}
}

func TestVerifyRejectsCatalogOverOneMiB(t *testing.T) {
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("s", ed25519.SeedSize)))
	data := []byte(strings.Repeat(" ", (1<<20)+1))
	if _, err := Verify(data, signatureEnvelope(private, data, "stable-2026"), Keyring{"stable-2026": private.Public().(ed25519.PublicKey)}, 0, catalogNow); err == nil {
		t.Fatal("Verify accepted catalog over 1 MiB")
	}
}

func validCatalogJSON() []byte {
	return []byte(`{"version":1,"channel":"stable","sequence":7,"generated_at":"2026-08-22T05:00:00Z","plugins":[` + validEntryJSON() + `]}`)
}

func validEntryJSON() string {
	return `{"id":"dev.bsbctl.ball8","version":"1.0.0","os":"darwin","arch":"arm64","url":"https://example.invalid/ball8.tar.gz","sha256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824","compressed_size":5,"archive_format":"tar.gz","executable":"bsbctl-plugin-ball8","manifest":"manifest.json"}`
}

func signatureEnvelope(private ed25519.PrivateKey, data []byte, keyID string) []byte {
	return []byte(`{"key_id":"` + keyID + `","algorithm":"ed25519","signature":"` + base64.StdEncoding.EncodeToString(ed25519.Sign(private, data)) + `"}`)
}
