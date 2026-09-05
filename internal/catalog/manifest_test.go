package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestVerifyPackageManifestUsesStrictSchemaAndBindsCatalogEntry(t *testing.T) {
	executableDigest := sha256.Sum256([]byte("executable"))
	entry := Entry{ID: "dev.bsbctl.ball8", Version: "1.0.0", Executable: "bsbctl-plugin-ball8"}
	data := []byte(`{"id":"dev.bsbctl.ball8","version":"1.0.0","protocol_version":"1.0","executable":"bsbctl-plugin-ball8","executable_sha256":"` + hex.EncodeToString(executableDigest[:]) + `","executable_size":10,"execution_modes":["interactive"],"channels":[{"id":"answer"}],"assets":[]}`)

	manifest, err := VerifyPackageManifest(data, entry, t.TempDir())
	if err != nil {
		t.Fatalf("VerifyPackageManifest: %v", err)
	}
	if manifest.ProtocolVersion != "1.0" || manifest.ExecutableSize != 10 {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestVerifyPackageManifestAcceptsDeclaredConfigurationSchema(t *testing.T) {
	executableDigest := sha256.Sum256([]byte("executable"))
	schemaDigest := sha256.Sum256([]byte(`{"type":"object"}`))
	entry := Entry{ID: "dev.bsbctl.ball8", Version: "1.0.0", Executable: "bsbctl-plugin-ball8"}
	data := []byte(`{"id":"dev.bsbctl.ball8","version":"1.0.0","protocol_version":"1.0","executable":"bsbctl-plugin-ball8","executable_sha256":"` + hex.EncodeToString(executableDigest[:]) + `","executable_size":10,"execution_modes":["interactive"],"channels":[{"id":"answer"}],"config_schema":{"source":"config.schema.json","sha256":"` + hex.EncodeToString(schemaDigest[:]) + `","size":17},"assets":[]}`)

	_, err := VerifyPackageManifest(data, entry, t.TempDir())
	if err != nil {
		t.Fatalf("VerifyPackageManifest: %v", err)
	}
}

func TestVerifyPackageManifestRejectsSchemaAndBindingFailures(t *testing.T) {
	digest := strings.Repeat("a", 64)
	valid := `{"id":"dev.bsbctl.ball8","version":"1.0.0","protocol_version":"1.0","executable":"bsbctl-plugin-ball8","executable_sha256":"` + digest + `","executable_size":10,"execution_modes":["interactive"],"channels":[{"id":"answer"}],"assets":[]}`
	entry := Entry{ID: "dev.bsbctl.ball8", Version: "1.0.0", Executable: "bsbctl-plugin-ball8"}
	tests := []struct{ name, data string }{
		{name: "unknown field", data: strings.Replace(valid, `"assets":[]`, `"assets":[],"policy":"attention"`, 1)},
		{name: "duplicate field", data: strings.Replace(valid, `"version":"1.0.0"`, `"version":"1.0.0","version":"1.0.0"`, 1)},
		{name: "wrong identity", data: strings.Replace(valid, `dev.bsbctl.ball8`, `dev.bsbctl.other`, 1)},
		{name: "wrong version", data: strings.Replace(valid, `1.0.0`, `2.0.0`, 1)},
		{name: "wrong executable", data: strings.Replace(valid, `bsbctl-plugin-ball8`, `bsbctl-plugin-other`, 1)},
		{name: "missing protocol version", data: strings.Replace(valid, `"protocol_version":"1.0",`, ``, 1)},
		{name: "wrong protocol version", data: strings.Replace(valid, `"protocol_version":"1.0"`, `"protocol_version":"3.0"`, 1)},
		{name: "missing execution modes", data: strings.Replace(valid, `"execution_modes":["interactive"],`, ``, 1)},
		{name: "scheduled execution mode", data: strings.Replace(valid, `"interactive"`, `"scheduled"`, 1)},
		{name: "unsupported execution mode", data: strings.Replace(valid, `"interactive"`, `"network"`, 1)},
		{name: "zero executable size", data: strings.Replace(valid, `"executable_size":10`, `"executable_size":0`, 1)},
		{name: "uppercase digest", data: strings.Replace(valid, digest, strings.ToUpper(digest), 1)},
		{name: "duplicate execution mode", data: strings.Replace(valid, `"interactive"`, `"interactive","interactive"`, 1)},
		{name: "duplicate channel", data: strings.Replace(valid, `{"id":"answer"}`, `{"id":"answer"},{"id":"answer"}`, 1)},
		{name: "unknown channel field", data: strings.Replace(valid, `{"id":"answer"}`, `{"id":"answer","secret":true}`, 1)},
		{name: "subscriptions removed", data: strings.Replace(valid, `"assets":[]`, `"subscriptions":["session.input"],"assets":[]`, 1)},
		{name: "asset namespace removed", data: strings.Replace(valid, `"assets":[]`, `"asset_namespace":"ball8","assets":[]`, 1)},
		{name: "removed device path", data: strings.Replace(valid, `"assets":[]`, `"assets":[{"source":"shake.anim","device_path":"shake.anim","sha256":"`+digest+`","size":1,"media_type":"application/x-busy-animation"}]`, 1)},
		{name: "missing asset media type", data: strings.Replace(valid, `"assets":[]`, `"assets":[{"source":"shake.anim","sha256":"`+digest+`","size":1}]`, 1)},
		{name: "package audio not supported", data: strings.Replace(valid, `"assets":[]`, `"assets":[{"source":"tone.snd","sha256":"`+digest+`","size":1,"media_type":"audio/x-busy-sound"}]`, 1)},
		{name: "noncanonical repeated separator", data: strings.Replace(valid, `"assets":[]`, `"assets":[{"source":"assets//shake.anim","sha256":"`+digest+`","size":1,"media_type":"application/x-busy-animation"}]`, 1)},
		{name: "noncanonical current segment", data: strings.Replace(valid, `"assets":[]`, `"assets":[{"source":"assets/./shake.anim","sha256":"`+digest+`","size":1,"media_type":"application/x-busy-animation"}]`, 1)},
		{name: "noncanonical parent segment", data: strings.Replace(valid, `"assets":[]`, `"assets":[{"source":"assets/old/../shake.anim","sha256":"`+digest+`","size":1,"media_type":"application/x-busy-animation"}]`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := VerifyPackageManifest([]byte(test.data), entry, t.TempDir()); err == nil {
				t.Fatal("VerifyPackageManifest accepted invalid manifest")
			}
		})
	}
}

func TestVerifyPackageManifestRejectsManifestOverOneMiB(t *testing.T) {
	entry := Entry{ID: "dev.bsbctl.ball8", Version: "1.0.0", Executable: "bsbctl-plugin-ball8"}
	if _, err := VerifyPackageManifest([]byte(strings.Repeat(" ", (1<<20)+1)), entry, t.TempDir()); err == nil {
		t.Fatal("VerifyPackageManifest accepted manifest over 1 MiB")
	}
}
