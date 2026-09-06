package releaseartifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDecodePlanRequiresIndependentStableComponentMetadata(t *testing.T) {
	t.Parallel()
	plan, err := DecodePlan([]byte(`{"schema_version":1,"components":[` +
		`{"id":"bsbctl","kind":"core","version":"0.1.0","tag":"v0.1.0","package":"./cmd/bsbctl","binary":"bsbctl"},` +
		`{"id":"dev.bsbctl.calendar","kind":"plugin","version":"0.1.0","tag":"plugin/calendar/v0.1.0","package":"./cmd/bsbctl-plugin-calendar","binary":"bsbctl-plugin-calendar"},` +
		`{"id":"dev.bsbctl.codex","kind":"plugin","version":"0.1.0","tag":"plugin/codex/v0.1.0","package":"./cmd/bsbctl-plugin-codex","binary":"bsbctl-plugin-codex"},` +
		`{"id":"dev.bsbctl.codex-quota","kind":"plugin","version":"0.2.0","tag":"plugin/codex-quota/v0.2.0","package":"./cmd/bsbctl-plugin-codex-quota","binary":"bsbctl-plugin-codex-quota"},` +
		`{"id":"dev.bsbctl.github-notifications","kind":"plugin","version":"0.1.0","tag":"plugin/github-notifications/v0.1.0","package":"./cmd/bsbctl-plugin-github-notifications","binary":"bsbctl-plugin-github-notifications"},` +
		`{"id":"dev.bsbctl.mac-resources","kind":"plugin","version":"1.0.0","tag":"plugin/mac-resources/v1.0.0","package":"./cmd/bsbctl-plugin-mac-resources","binary":"bsbctl-plugin-mac-resources"}]}`))

	if err != nil {
		t.Fatalf("DecodePlan: %v", err)
	}
	if len(plan.Components) != 6 || plan.Components[0].Tag != "v0.1.0" || plan.Components[1].Tag != "plugin/calendar/v0.1.0" || plan.Components[2].Tag != "plugin/codex/v0.1.0" || plan.Components[3].Version != "0.2.0" || plan.Components[4].Tag != "plugin/github-notifications/v0.1.0" || plan.Components[5].Version != "1.0.0" {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestDecodePlanRejectsAmbiguousOrUnsafeMetadata(t *testing.T) {
	t.Parallel()
	valid := `{"schema_version":1,"components":[{"id":"bsbctl","kind":"core","version":"0.1.0","tag":"v0.1.0","package":"./cmd/bsbctl","binary":"bsbctl"}]}`
	tests := []struct {
		name string
		data string
	}{
		{name: "unknown field", data: `{"schema_version":1,"components":[],"latest":true}`},
		{name: "empty components", data: `{"schema_version":1,"components":[]}`},
		{name: "prerelease", data: replaceJSON(valid, `"version":"0.1.0"`, `"version":"0.1.0-dev"`)},
		{name: "wrong core tag", data: replaceJSON(valid, `"tag":"v0.1.0"`, `"tag":"core/v0.1.0"`)},
		{name: "unsafe binary", data: replaceJSON(valid, `"binary":"bsbctl"`, `"binary":"../bsbctl"`)},
		{name: "unsafe package", data: replaceJSON(valid, `"package":"./cmd/bsbctl"`, `"package":"../bsbctl"`)},
		{name: "duplicate", data: replaceJSON(valid, `]}`, `,{"id":"bsbctl","kind":"core","version":"0.1.0","tag":"v0.1.0","package":"./cmd/bsbctl","binary":"bsbctl"}]}`)},
		{name: "trailing JSON", data: valid + `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodePlan([]byte(test.data)); err == nil {
				t.Fatal("DecodePlan accepted invalid metadata")
			}
		})
	}
}

func TestWriteArchiveIsByteDeterministicAndCanonical(t *testing.T) {
	t.Parallel()
	epoch := time.Unix(1_700_000_000, 0).UTC()
	entries := []ArchiveEntry{
		{Name: "NOTICE", Mode: 0o644, Data: []byte("notice\n")},
		{Name: "bsbctl", Mode: 0o755, Data: []byte("binary")},
		{Name: "manifest.json", Mode: 0o644, Data: []byte(`{"version":"0.1.0"}`)},
	}
	var first, second bytes.Buffer
	if err := WriteArchive(&first, entries, epoch); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	reversed := []ArchiveEntry{entries[2], entries[0], entries[1]}
	if err := WriteArchive(&second, reversed, epoch); err != nil {
		t.Fatalf("WriteArchive reversed: %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("archive bytes depend on caller entry order")
	}

	gzipReader, err := gzip.NewReader(bytes.NewReader(first.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !gzipReader.ModTime.Equal(epoch) || gzipReader.Name != "" || gzipReader.Comment != "" {
		t.Fatalf("gzip header time=%v name=%q comment=%q", gzipReader.ModTime, gzipReader.Name, gzipReader.Comment)
	}
	tarReader := tar.NewReader(gzipReader)
	var got []ArchiveEntry
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" || !header.ModTime.Equal(epoch) || header.Typeflag != tar.TypeReg {
			t.Fatalf("noncanonical header = %#v", header)
		}
		got = append(got, ArchiveEntry{Name: header.Name, Mode: header.FileInfo().Mode().Perm(), Data: data})
	}
	want := []ArchiveEntry{entries[0], entries[1], entries[2]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %#v, want %#v", got, want)
	}
}

func TestWriteArchiveRejectsUnsafeOrDuplicateEntriesAndInvalidEpoch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries []ArchiveEntry
		epoch   time.Time
	}{
		{name: "unsafe path", entries: []ArchiveEntry{{Name: "../binary", Mode: 0o755, Data: []byte("x")}}, epoch: time.Unix(1, 0)},
		{name: "directory", entries: []ArchiveEntry{{Name: "dir/", Mode: 0o755}}, epoch: time.Unix(1, 0)},
		{name: "duplicate", entries: []ArchiveEntry{{Name: "a", Mode: 0o644}, {Name: "a", Mode: 0o644}}, epoch: time.Unix(1, 0)},
		{name: "bad mode", entries: []ArchiveEntry{{Name: "a", Mode: 0o666}}, epoch: time.Unix(1, 0)},
		{name: "zero epoch", entries: []ArchiveEntry{{Name: "a", Mode: 0o644}}, epoch: time.Time{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := WriteArchive(io.Discard, test.entries, test.epoch); err == nil {
				t.Fatal("WriteArchive accepted invalid input")
			}
		})
	}
}

func TestCycloneDXIsDeterministicAndContainsLicenseEvidence(t *testing.T) {
	t.Parallel()
	component := Component{ID: "bsbctl", Kind: "core", Version: "0.1.0", Tag: "v0.1.0", Package: "./cmd/bsbctl", Binary: "bsbctl"}
	inventory, err := DecodeDependencyInventory([]byte(`{"schema_version":1,"modules":[` +
		`{"module":"github.com/coder/websocket","version":"v1.8.15","license":"ISC","license_sha256":"cc0975a5f6305145bdd7b41ce9479632fdac3870e6ac4281f28017f18c767c4e"},` +
		`{"module":"github.com/lxdb/busylib-go","version":"v0.0.0","license":"MIT","license_sha256":"b7ec5c1207ecb19fc7d4ed4436fe50caed09068f9fbd68bbe86f1065811c05be","release_blocked":true,"release_blocker_id":"busylib-protobuf-license"}]}`))
	if err != nil {
		t.Fatalf("DecodeDependencyInventory: %v", err)
	}
	first, err := CycloneDX(component, inventory)
	if err != nil {
		t.Fatalf("CycloneDX: %v", err)
	}
	second, err := CycloneDX(component, inventory)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("nondeterministic CycloneDX: err=%v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(first, &document); err != nil {
		t.Fatal(err)
	}
	if document["bomFormat"] != "CycloneDX" || document["specVersion"] != "1.6" || document["serialNumber"] != nil || document["timestamp"] != nil {
		t.Fatalf("CycloneDX envelope = %#v", document)
	}
	components, ok := document["components"].([]any)
	if !ok || len(components) != 2 {
		t.Fatalf("CycloneDX components = %#v", document["components"])
	}
	if !bytes.Contains(first, []byte(`"bsbctl:release-blocked","value":"true"`)) || !bytes.Contains(first, []byte(`"bsbctl:license-file-sha256","value":"cc0975a5f6305145bdd7b41ce9479632fdac3870e6ac4281f28017f18c767c4e"`)) {
		t.Fatalf("CycloneDX lacks blocker/license evidence: %s", first)
	}
}

func TestDecodeDependencyInventoryIsStrict(t *testing.T) {
	t.Parallel()
	valid := `{"schema_version":1,"modules":[{"module":"github.com/coder/websocket","version":"v1.8.15","license":"ISC","license_sha256":"cc0975a5f6305145bdd7b41ce9479632fdac3870e6ac4281f28017f18c767c4e"}]}`
	inventory, err := DecodeDependencyInventory([]byte(valid))
	if err != nil || len(inventory.Modules) != 1 || inventory.Modules[0].License != "ISC" {
		t.Fatalf("DecodeDependencyInventory = %#v, %v", inventory, err)
	}
	for _, invalid := range []string{
		replaceJSON(valid, `"schema_version":1`, `"schema_version":2`),
		`{"schema_version":1,"modules":[]}`,
		replaceJSON(valid, `}]}`, `}],"generated":"network"}`),
		replaceJSON(valid, `"license_sha256":"cc0975a5f6305145bdd7b41ce9479632fdac3870e6ac4281f28017f18c767c4e"`, `"license_sha256":"unknown"`),
		valid + `{}`,
	} {
		if _, err := DecodeDependencyInventory([]byte(invalid)); err == nil {
			t.Fatalf("DecodeDependencyInventory accepted %q", invalid)
		}
	}
}

func TestDecodeDependencyInventoryRequiresExactReleaseBlockerIdentity(t *testing.T) {
	t.Parallel()
	valid := `{"schema_version":1,"modules":[{"module":"github.com/lxdb/busylib-go","version":"v0.0.0","license":"MIT","license_sha256":"b7ec5c1207ecb19fc7d4ed4436fe50caed09068f9fbd68bbe86f1065811c05be","release_blocked":true,"release_blocker_id":"busylib-protobuf-license"}]}`
	inventory, err := DecodeDependencyInventory([]byte(valid))
	if err != nil || len(inventory.Modules) != 1 || !inventory.Modules[0].ReleaseBlocked {
		t.Fatalf("DecodeDependencyInventory = %#v, %v", inventory, err)
	}
	for _, invalid := range []string{
		strings.Replace(valid, `,"release_blocker_id":"busylib-protobuf-license"`, "", 1),
		strings.Replace(valid, `"release_blocked":true,`, "", 1),
		strings.Replace(valid, `"busylib-protobuf-license"`, `"bad id"`, 1),
	} {
		if _, err := DecodeDependencyInventory([]byte(invalid)); err == nil {
			t.Fatalf("DecodeDependencyInventory accepted mismatched blocker identity %q", invalid)
		}
	}
}

func TestDecodeCatalogPredecessorRequiresExplicitFirstReleaseOrExactPriorBytes(t *testing.T) {
	t.Parallel()
	first, err := DecodeCatalogPredecessor([]byte(`{"schema_version":1,"first_release":true}`))
	if err != nil || !first.FirstRelease {
		t.Fatalf("first release = %#v, %v", first, err)
	}
	prior, err := DecodeCatalogPredecessor([]byte(`{"schema_version":1,"tag":"v0.1.0","catalog_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","signature_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`))
	if err != nil || prior.FirstRelease || prior.Tag != "v0.1.0" {
		t.Fatalf("prior release = %#v, %v", prior, err)
	}
	for name, document := range map[string]string{
		"implicit first release": `{"schema_version":1}`,
		"mixed modes":            `{"schema_version":1,"first_release":true,"tag":"v0.1.0","catalog_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","signature_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`,
		"unbound tag":            `{"schema_version":1,"tag":"v0.1.0"}`,
		"plugin tag":             `{"schema_version":1,"tag":"plugin/test/v0.1.0","catalog_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","signature_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`,
		"duplicate field":        `{"schema_version":1,"first_release":true,"first_release":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCatalogPredecessor([]byte(document)); err == nil {
				t.Fatal("DecodeCatalogPredecessor accepted invalid metadata")
			}
		})
	}
}

func TestSelectDependenciesProducesComponentSpecificSBOMClosures(t *testing.T) {
	t.Parallel()
	inventory := DependencyInventory{SchemaVersion: 1, Modules: []Dependency{
		{Module: "example.com/alpha", Version: "v1.0.0", License: "MIT", LicenseSHA256: strings.Repeat("a", 64)},
		{Module: "example.com/beta", Version: "v2.0.0", License: "Apache-2.0", LicenseSHA256: strings.Repeat("b", 64)},
	}}
	core, err := SelectDependencies(inventory, []ModuleVersion{{Module: "example.com/alpha", Version: "v1.0.0"}, {Module: "example.com/beta", Version: "v2.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := SelectDependencies(inventory, []ModuleVersion{{Module: "example.com/alpha", Version: "v1.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	coreSBOM, err := CycloneDX(Component{ID: "bsbctl", Version: "0.1.0", Binary: "bsbctl"}, core)
	if err != nil {
		t.Fatal(err)
	}
	pluginSBOM, err := CycloneDX(Component{ID: "dev.bsbctl.codex-quota", Version: "0.1.0", Binary: "bsbctl-plugin-codex-quota"}, plugin)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(pluginSBOM, []byte("example.com/beta")) || !bytes.Contains(coreSBOM, []byte("example.com/beta")) || bytes.Equal(coreSBOM, pluginSBOM) {
		t.Fatalf("component SBOMs do not reflect distinct closures:\ncore=%s\nplugin=%s", coreSBOM, pluginSBOM)
	}
}

func TestSelectDependenciesProducesRootOnlySBOMForEmptyExternalClosure(t *testing.T) {
	t.Parallel()
	inventory := DependencyInventory{SchemaVersion: 1, Modules: []Dependency{
		{Module: "example.com/reviewed", Version: "v1.0.0", License: "MIT", LicenseSHA256: strings.Repeat("a", 64)},
	}}
	selected, err := SelectDependencies(inventory, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Modules) != 0 {
		t.Fatalf("selected modules = %#v, want empty external closure", selected.Modules)
	}
	sbom, err := CycloneDX(Component{ID: "dev.bsbctl.codex-quota", Version: "0.1.0", Binary: "bsbctl-plugin-codex-quota"}, selected)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sbom, []byte("example.com/reviewed")) || !bytes.Contains(sbom, []byte(`"name":"bsbctl-plugin-codex-quota"`)) {
		t.Fatalf("root-only SBOM is invalid: %s", sbom)
	}
}

func TestSelectDependenciesFailsClosedOnMissingOrMismatchedReview(t *testing.T) {
	t.Parallel()
	inventory := DependencyInventory{SchemaVersion: 1, Modules: []Dependency{
		{Module: "example.com/alpha", Version: "v1.0.0", License: "MIT", LicenseSHA256: strings.Repeat("a", 64)},
	}}
	for _, closure := range [][]ModuleVersion{
		{{Module: "example.com/missing", Version: "v1.0.0"}},
		{{Module: "example.com/alpha", Version: "v1.0.1"}},
	} {
		if _, err := SelectDependencies(inventory, closure); err == nil {
			t.Fatalf("SelectDependencies accepted unreviewed closure %#v", closure)
		}
	}
}

func replaceJSON(value, old, replacement string) string {
	return strings.Replace(value, old, replacement, 1)
}
