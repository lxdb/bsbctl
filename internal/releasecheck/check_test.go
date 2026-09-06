package releasecheck

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lxdb/bsbctl/internal/releaseartifact"
)

func TestCheckReportsEveryCurrentReleaseBlockerInStableOrder(t *testing.T) {
	t.Parallel()
	root := releaseFixture(t,
		`module github.com/lxdb/bsbctl

go 1.26

require github.com/lxdb/busylib-go v0.0.0
replace github.com/lxdb/busylib-go => ../busylib/busylib-go
`,
		`{"schema_version":1,"keys":[]}`,
		`{"schema_version":1,"blockers":[{"id":"busylib-protobuf-license","message":"busylib-go copied protobuf inputs lack public redistribution permission","operator_action":"obtain compatible permission and update the dependency evidence"}]}`,
		`{"schema_version":1,"modules":[{"module":"github.com/lxdb/busylib-go","version":"v0.0.0","license":"MIT","license_sha256":"b7ec5c1207ecb19fc7d4ed4436fe50caed09068f9fbd68bbe86f1065811c05be","release_blocked":true,"release_blocker_id":"busylib-protobuf-license"}]}`,
	)

	report, err := checkWithReachable(root, []releaseartifact.ModuleVersion{{Module: busylibModule, Version: "v0.0.0"}})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := []Finding{
		{ID: "busylib-protobuf-license", Message: "busylib-go copied protobuf inputs lack public redistribution permission", OperatorAction: "obtain compatible permission and update the dependency evidence"},
		{ID: "busylib-local-replace", Message: "go.mod replaces github.com/lxdb/busylib-go with a local filesystem path", OperatorAction: "publish an immutable reachable busylib-go version and remove the local replacement"},
		{ID: "catalog-public-key", Message: "no authorized Ed25519 catalog public key is provisioned", OperatorAction: "add the authorized public key to internal/releasekeys/catalog_public_keys.json and review its provenance"},
	}
	if !reflect.DeepEqual(report.Findings, want) {
		t.Fatalf("findings = %#v, want %#v", report.Findings, want)
	}
}

func TestCheckAcceptsResolvedPublicationInputs(t *testing.T) {
	t.Parallel()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x44}, ed25519.PublicKeySize))
	root := releaseFixture(t,
		`module github.com/lxdb/bsbctl

go 1.26

require github.com/lxdb/busylib-go v0.2.0
`,
		`{"schema_version":1,"keys":[{"id":"stable-2026","algorithm":"ed25519","public_key_base64":"`+key+`"}]}`,
		`{"schema_version":1,"blockers":[]}`,
		`{"schema_version":1,"modules":[{"module":"github.com/lxdb/busylib-go","version":"v0.2.0","license":"MIT","license_sha256":"b7ec5c1207ecb19fc7d4ed4436fe50caed09068f9fbd68bbe86f1065811c05be"}]}`,
	)
	report, err := checkWithReachable(root, []releaseartifact.ModuleVersion{{Module: busylibModule, Version: "v0.2.0"}})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("findings = %#v, want none", report.Findings)
	}
}

func TestCheckRejectsInvalidMachineInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		goMod    string
		keys     string
		blockers string
		deps     string
	}{
		{name: "invalid keys", goMod: "module github.com/lxdb/bsbctl\n", keys: `{"schema_version":1,"keys":[{}]}`, blockers: `{"schema_version":1,"blockers":[]}`, deps: validDependencies},
		{name: "invalid blockers", goMod: "module github.com/lxdb/bsbctl\n", keys: `{"schema_version":1,"keys":[]}`, blockers: `{"schema_version":1,"blockers":[{"id":"bad id","message":"x","operator_action":"y"}]}`, deps: validDependencies},
		{name: "duplicate blockers", goMod: "module github.com/lxdb/bsbctl\n", keys: `{"schema_version":1,"keys":[]}`, blockers: `{"schema_version":1,"blockers":[{"id":"same","message":"x","operator_action":"y"},{"id":"same","message":"z","operator_action":"w"}]}`, deps: validDependencies},
		{name: "invalid dependencies", goMod: "module github.com/lxdb/bsbctl\n", keys: `{"schema_version":1,"keys":[]}`, blockers: `{"schema_version":1,"blockers":[]}`, deps: `{"schema_version":1,"modules":[]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := releaseFixture(t, test.goMod, test.keys, test.blockers, test.deps)
			if _, err := checkWithReachable(root, []releaseartifact.ModuleVersion{{Module: busylibModule, Version: "v0.2.0"}}); err == nil {
				t.Fatal("Check accepted invalid release input")
			}
		})
	}
}

func TestCheckRejectsBlockedDependencyWithoutMatchingBlocker(t *testing.T) {
	t.Parallel()
	root := releaseFixture(t,
		"module github.com/lxdb/bsbctl\n",
		`{"schema_version":1,"keys":[]}`,
		`{"schema_version":1,"blockers":[]}`,
		`{"schema_version":1,"modules":[{"module":"github.com/lxdb/busylib-go","version":"v0.0.0","license":"MIT","license_sha256":"b7ec5c1207ecb19fc7d4ed4436fe50caed09068f9fbd68bbe86f1065811c05be","release_blocked":true,"release_blocker_id":"busylib-protobuf-license"}]}`,
	)
	if _, err := checkWithReachable(root, []releaseartifact.ModuleVersion{{Module: busylibModule, Version: "v0.0.0"}}); err == nil {
		t.Fatal("Check accepted a release-blocked dependency without its matching blocker")
	}
}

func TestCheckRequiresExactDualDarwinComponentDependencyUnion(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x55}, ed25519.PublicKeySize))
	const alpha = `{"module":"example.com/alpha","version":"v1.0.0","license":"MIT","license_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	const beta = `{"module":"example.com/beta","version":"v2.0.0","license":"Apache-2.0","license_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`
	const stale = `{"module":"example.com/stale","version":"v3.0.0","license":"BSD-3-Clause","license_sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}`
	root := releaseFixture(t, "module github.com/lxdb/bsbctl\n", `{"schema_version":1,"keys":[{"id":"stable","algorithm":"ed25519","public_key_base64":"`+key+`"}]}`, `{"schema_version":1,"blockers":[]}`, `{"schema_version":1,"modules":[`+alpha+`,`+beta+`]}`)

	calls := make(map[string]int)
	lister := func(_ string, component releaseartifact.Component, goos, goarch string) ([]releaseartifact.ModuleVersion, error) {
		calls[component.ID+"/"+goos+"/"+goarch]++
		if goos != "darwin" {
			return nil, fmt.Errorf("unexpected target %s/%s", goos, goarch)
		}
		if goarch == "arm64" {
			return []releaseartifact.ModuleVersion{{Module: "example.com/alpha", Version: "v1.0.0"}}, nil
		}
		if goarch == "amd64" {
			return []releaseartifact.ModuleVersion{{Module: "example.com/beta", Version: "v2.0.0"}}, nil
		}
		return nil, fmt.Errorf("unexpected architecture %q", goarch)
	}
	if report, err := check(root, lister); err != nil || len(report.Findings) != 0 {
		t.Fatalf("exact inventory check = %#v, %v", report, err)
	}
	if len(calls) != 12 {
		t.Fatalf("component/architecture closure calls = %v, want all twelve", calls)
	}
	for call, count := range calls {
		if count != 1 {
			t.Fatalf("closure call %q count = %d, want 1", call, count)
		}
	}

	for name, inventory := range map[string]string{
		"missing":       `{"schema_version":1,"modules":[` + alpha + `]}`,
		"wrong version": `{"schema_version":1,"modules":[` + alpha + `,{"module":"example.com/beta","version":"v2.0.1","license":"Apache-2.0","license_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`,
		"stale":         `{"schema_version":1,"modules":[` + alpha + `,` + beta + `,` + stale + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			writeFixtureFile(t, filepath.Join(root, "release", "dependencies.json"), inventory)
			if _, err := check(root, lister); err == nil {
				t.Fatalf("preflight accepted %s dependency inventory", name)
			}
		})
	}
}

func TestCheckRunsEveryClosureWithANewOfflineCache(t *testing.T) {
	temporary := t.TempDir()
	capture := filepath.Join(temporary, "capture")
	fakeGo := filepath.Join(temporary, "go")
	script := `#!/bin/sh
set -eu
test "$GOPROXY" = off
test "$GOSUMDB" = off
test "$GOTOOLCHAIN" = local
test "$GOVCS" = '*:off'
test "$GONOPROXY" = none
test "$GOPRIVATE" = ''
test "${GOFLAGS-}" = ''
test "$GOWORK" = off
test "$GOOS" = darwin
test ! -e "$GOCACHE/used"
: > "$GOCACHE/used"
last=
for argument in "$@"; do last=$argument; done
printf '%s|%s|%s\n' "$GOCACHE" "$GOARCH" "$last" >> "$BSBCTL_PREFLIGHT_CAPTURE"
if test "$GOARCH" = arm64; then
  printf 'example.com/alpha\tv1.0.0\n'
else
  printf 'example.com/beta\tv2.0.0\n'
fi
`
	if err := os.WriteFile(fakeGo, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", temporary+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BSBCTL_PREFLIGHT_CAPTURE", capture)
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GONOPROXY", "example.com")
	t.Setenv("GOPRIVATE", "example.com")
	t.Setenv("GOVCS", "*:all")
	t.Setenv("GOWORK", filepath.Join(temporary, "ambient.work"))
	ambient := t.TempDir()
	if err := os.WriteFile(filepath.Join(ambient, "ambient"), []byte("warm"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOCACHE", ambient)
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x66}, ed25519.PublicKeySize))
	root := releaseFixture(t, "module github.com/lxdb/bsbctl\n", `{"schema_version":1,"keys":[{"id":"stable","algorithm":"ed25519","public_key_base64":"`+key+`"}]}`, `{"schema_version":1,"blockers":[]}`, `{"schema_version":1,"modules":[{"module":"example.com/alpha","version":"v1.0.0","license":"MIT","license_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"module":"example.com/beta","version":"v2.0.0","license":"Apache-2.0","license_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`)
	if report, err := Check(root); err != nil || len(report.Findings) != 0 {
		t.Fatalf("Check with offline lister = %#v, %v", report, err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 12 {
		t.Fatalf("closure captures = %q, want twelve", data)
	}
	seenCaches := make(map[string]struct{}, len(lines))
	seenTargets := make(map[string]int)
	for _, line := range lines {
		cache, target, ok := strings.Cut(line, "|")
		if !ok || cache == ambient {
			t.Fatalf("closure capture = %q", line)
		}
		architecture, component, ok := strings.Cut(target, "|")
		if !ok {
			t.Fatalf("closure capture = %q", line)
		}
		seenTargets[architecture+"/"+component]++
		if _, duplicate := seenCaches[cache]; duplicate {
			t.Fatalf("closure reused cache %q", cache)
		}
		seenCaches[cache] = struct{}{}
		if _, err := os.Lstat(cache); !os.IsNotExist(err) {
			t.Fatalf("closure cache %q was not cleaned: %v", cache, err)
		}
	}
	if len(seenTargets) != 12 {
		t.Fatalf("closure targets = %v, want both arches for every component", seenTargets)
	}
}

const validDependencies = `{"schema_version":1,"modules":[{"module":"github.com/lxdb/busylib-go","version":"v0.2.0","license":"MIT","license_sha256":"b7ec5c1207ecb19fc7d4ed4436fe50caed09068f9fbd68bbe86f1065811c05be"}]}`

func releaseFixture(t *testing.T, goMod, keys, blockers, dependencies string) string {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "go.mod"), goMod)
	writeFixtureFile(t, filepath.Join(root, "internal", "releasekeys", "catalog_public_keys.json"), keys)
	writeFixtureFile(t, filepath.Join(root, "release", "blockers.json"), blockers)
	writeFixtureFile(t, filepath.Join(root, "release", "dependencies.json"), dependencies)
	writeFixtureFile(t, filepath.Join(root, "release", "versions.json"), validReleasePlan)
	return root
}

func checkWithReachable(root string, reachable []releaseartifact.ModuleVersion) (Report, error) {
	return check(root, func(string, releaseartifact.Component, string, string) ([]releaseartifact.ModuleVersion, error) {
		return append([]releaseartifact.ModuleVersion(nil), reachable...), nil
	})
}

const validReleasePlan = `{"schema_version":1,"components":[{"id":"bsbctl","kind":"core","version":"0.1.0","tag":"v0.1.0","package":"./cmd/bsbctl","binary":"bsbctl"},{"id":"dev.bsbctl.calendar","kind":"plugin","version":"0.1.0","tag":"plugin/calendar/v0.1.0","package":"./cmd/bsbctl-plugin-calendar","binary":"bsbctl-plugin-calendar"},{"id":"dev.bsbctl.codex","kind":"plugin","version":"0.1.0","tag":"plugin/codex/v0.1.0","package":"./cmd/bsbctl-plugin-codex","binary":"bsbctl-plugin-codex"},{"id":"dev.bsbctl.codex-quota","kind":"plugin","version":"0.1.0","tag":"plugin/codex-quota/v0.1.0","package":"./cmd/bsbctl-plugin-codex-quota","binary":"bsbctl-plugin-codex-quota"},{"id":"dev.bsbctl.github-notifications","kind":"plugin","version":"0.1.0","tag":"plugin/github-notifications/v0.1.0","package":"./cmd/bsbctl-plugin-github-notifications","binary":"bsbctl-plugin-github-notifications"},{"id":"dev.bsbctl.mac-resources","kind":"plugin","version":"0.1.0","tag":"plugin/mac-resources/v0.1.0","package":"./cmd/bsbctl-plugin-mac-resources","binary":"bsbctl-plugin-mac-resources"}]}`

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
