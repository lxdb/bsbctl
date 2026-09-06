package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/configschema"
	"github.com/lxdb/bsbctl/internal/releaseartifact"
	calendarplugin "github.com/lxdb/bsbctl/plugins/calendar"
	codexplugin "github.com/lxdb/bsbctl/plugins/codex"
	"github.com/lxdb/bsbctl/plugins/codexquota"
	"github.com/lxdb/bsbctl/plugins/githubnotifications"
	"github.com/lxdb/bsbctl/plugins/macresources"
	"github.com/lxdb/bsbctl/plugins/slack"
	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type packagedPluginManifest struct {
	ID              string                         `json:"id"`
	Version         string                         `json:"version"`
	ProtocolVersion string                         `json:"protocol_version"`
	ExecutionModes  []protocol.ExecutionMode       `json:"execution_modes"`
	Channels        []protocol.Channel             `json:"channels"`
	Operations      []protocol.OperationDescriptor `json:"operations"`
	ConfigSchema    *configschema.Declaration      `json:"config_schema"`
	Assets          []assets.Declaration           `json:"assets"`
}

func assertPackagedManifestMatchesDefinition(t *testing.T, data []byte, expected pluginsdk.Definition) packagedPluginManifest {
	t.Helper()
	var manifest packagedPluginManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ID != expected.ID || manifest.Version != expected.Version || manifest.ProtocolVersion != protocol.Version ||
		!reflect.DeepEqual(manifest.ExecutionModes, expected.Contract.ExecutionModes) || !reflect.DeepEqual(manifest.Channels, expected.Contract.Channels) ||
		!reflect.DeepEqual(manifest.Operations, expected.Contract.Operations) {
		t.Fatalf("packaged manifest contract = %#v, want definition %#v", manifest, expected)
	}
	return manifest
}

func TestArchiveComponentContractsReferenceCanonicalPluginDefinitions(t *testing.T) {
	want := map[string]func(string) pluginsdk.Definition{
		calendarplugin.PluginID:      calendarplugin.DefinitionForVersion,
		codexplugin.PluginID:         codexplugin.DefinitionForVersion,
		codexquota.PluginID:          codexquota.DefinitionForVersion,
		githubnotifications.PluginID: githubnotifications.DefinitionForVersion,
		macresources.PluginID:        macresources.DefinitionForVersion,
		slack.PluginID:               slack.DefinitionForVersion,
	}
	for id, definitionForVersion := range want {
		contract, exists := archiveComponentContracts[id]
		if !exists || contract.Definition == nil {
			t.Fatalf("component %q has no canonical definition", id)
		}
		got, expected := contract.Definition("9.8.7"), definitionForVersion("9.8.7")
		if got.ID != expected.ID || got.Version != expected.Version ||
			!reflect.DeepEqual(got.Contract, expected.Contract) {
			t.Fatalf("component %q definition = %#v, want %#v", id, got, expected)
		}
	}
}

func TestRepositoryReleasePlanIncludesExactSlackComponent(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "release", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := releaseartifact.DecodePlan(data)
	if err != nil {
		t.Fatal(err)
	}
	var component *releaseartifact.Component
	for index := range plan.Components {
		if plan.Components[index].ID == slack.PluginID {
			component = &plan.Components[index]
			break
		}
	}
	if component == nil || component.Kind != "plugin" || component.Tag != "plugin/slack/v"+component.Version || component.Package != "./cmd/bsbctl-plugin-slack" || component.Binary != "bsbctl-plugin-slack" {
		t.Fatalf("Slack release component = %#v", component)
	}
}

func TestRunPackageIsDeterministicAndInspectDetectsTampering(t *testing.T) {
	root := packageFixture(t)
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(_ context.Context, request buildRequest) ([]byte, error) {
		return []byte(fmt.Sprintf("mach-o:%s:%s:%s:%s\n", request.Component.ID, request.Component.Version, request.GOOS, request.GOARCH)), nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })

	for _, output := range []string{first, second} {
		var stdout, stderr bytes.Buffer
		code := run([]string{"package", "--root", root, "--out", output, "--goos", "darwin", "--goarch", "arm64", "--source-date-epoch", "1700000000"}, &stdout, &stderr)
		if code != exitSuccess || stderr.Len() != 0 || !strings.Contains(stdout.String(), "packaged 7 component(s)") {
			t.Fatalf("package exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if err := verifyArtifactDirectory(output); err != nil {
			t.Fatalf("verify generated artifact directory: %v", err)
		}
		stdout.Reset()
		code = run([]string{"inspect", "--dir", output}, &stdout, &stderr)
		if code != exitSuccess || stderr.Len() != 0 || stdout.String() != "release artifacts: verified\n" {
			t.Fatalf("inspect exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	}
	if got, want := snapshotDirectory(t, first), snapshotDirectory(t, second); !reflect.DeepEqual(got, want) {
		t.Fatalf("package outputs differ:\nfirst=%#v\nsecond=%#v", got, want)
	}
	archive := filepath.Join(first, "bsbctl_0.1.0_darwin_arm64.tar.gz")
	for licensePath := range reviewedLegalArtifacts {
		got := readArchiveMember(t, archive, licensePath)
		want, err := os.ReadFile(filepath.Join(root, licensePath))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("archive does not preserve reviewed license %s", licensePath)
		}
	}

	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(archive, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"inspect", "--dir", first}, &stdout, &stderr); code != exitFailure || stdout.Len() != 0 || stderr.String() != "releasectl: artifact verification failed\n" {
		t.Fatalf("tampered inspect exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestPackageComponentsDeclaresCalendarContractAndSchema(t *testing.T) {
	root := packageFixture(t)
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(_ context.Context, request buildRequest) ([]byte, error) {
		return []byte("binary:" + request.Component.ID), nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })

	output := filepath.Join(t.TempDir(), "artifacts")
	if _, err := packageComponents(context.Background(), root, output, "darwin", "arm64", time.Unix(1700000000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(output, "bsbctl-plugin-calendar_0.5.0_darwin_arm64.tar.gz")
	manifestData := readArchiveMember(t, archive, "manifest.json")
	schema := readArchiveMember(t, archive, configschema.FileName)
	wantSchema, err := os.ReadFile(filepath.Join(root, "plugins", "calendar", configschema.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(schema, wantSchema) {
		t.Fatal("Calendar archive schema does not match its reviewed source")
	}

	var manifest struct {
		ID              string                         `json:"id"`
		ProtocolVersion string                         `json:"protocol_version"`
		ExecutionModes  []protocol.ExecutionMode       `json:"execution_modes"`
		Channels        []protocol.Channel             `json:"channels"`
		Operations      []protocol.OperationDescriptor `json:"operations"`
		ConfigSchema    *configschema.Declaration      `json:"config_schema"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(wantSchema)
	wantDeclaration := &configschema.Declaration{Source: configschema.FileName, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(wantSchema))}
	if manifest.ID != "dev.bsbctl.calendar" || manifest.ProtocolVersion != protocol.Version ||
		!reflect.DeepEqual(manifest.ExecutionModes, []protocol.ExecutionMode{protocol.ExecutionModeResident, protocol.ExecutionModeInteractive}) ||
		!reflect.DeepEqual(manifest.Channels, []protocol.Channel{{ID: "upcoming"}, {ID: "active"}, {ID: "interaction"}}) ||
		!reflect.DeepEqual(manifest.Operations, []protocol.OperationDescriptor{{ID: "calendars", Kind: protocol.OperationQuery}}) || !reflect.DeepEqual(manifest.ConfigSchema, wantDeclaration) {
		t.Fatalf("Calendar manifest = %#v, want exact local Calendar contract", manifest)
	}
}

func TestCodexQuotaPackageDeclaresStaticMark(t *testing.T) {
	root := packageFixture(t)
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(_ context.Context, request buildRequest) ([]byte, error) {
		return []byte("binary:" + request.Component.ID), nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })

	output := filepath.Join(t.TempDir(), "artifacts")
	if _, err := packageComponents(context.Background(), root, output, "darwin", "arm64", time.Unix(1700000000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(output, "bsbctl-plugin-codex-quota_0.2.0_darwin_arm64.tar.gz")
	mark := readArchiveMember(t, archive, "assets/codex-mark.png")
	manifestData := readArchiveMember(t, archive, "manifest.json")
	manifest := assertPackagedManifestMatchesDefinition(t, manifestData, codexquota.DefinitionForVersion("0.2.0"))
	digest := sha256.Sum256(mark)
	want := assets.Declaration{
		Source: "assets/codex-mark.png",
		SHA256: hex.EncodeToString(digest[:]), Size: int64(len(mark)), MediaType: "image/png",
	}
	if !reflect.DeepEqual(manifest.Assets, []assets.Declaration{want}) {
		t.Fatalf("Codex quota asset contract = assets:%#v", manifest.Assets)
	}
}

func TestMacResourcesPackageUsesCanonicalDefinition(t *testing.T) {
	root := packageFixture(t)
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(_ context.Context, request buildRequest) ([]byte, error) {
		return []byte("binary:" + request.Component.ID), nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })

	output := filepath.Join(t.TempDir(), "artifacts")
	if _, err := packageComponents(context.Background(), root, output, "darwin", "arm64", time.Unix(1700000000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(output, "bsbctl-plugin-mac-resources_0.3.0_darwin_arm64.tar.gz")
	assertPackagedManifestMatchesDefinition(t, readArchiveMember(t, archive, "manifest.json"), macresources.DefinitionForVersion("0.3.0"))
}

func TestSlackPackageUsesCanonicalDefinitionAndConfigurationSchema(t *testing.T) {
	root := packageFixture(t)
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(_ context.Context, request buildRequest) ([]byte, error) {
		return []byte("binary:" + request.Component.ID), nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })

	output := filepath.Join(t.TempDir(), "artifacts")
	if _, err := packageComponents(context.Background(), root, output, "darwin", "arm64", time.Unix(1700000000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(output, "bsbctl-plugin-slack_0.1.0_darwin_arm64.tar.gz")
	manifest := assertPackagedManifestMatchesDefinition(t, readArchiveMember(t, archive, "manifest.json"), slack.DefinitionForVersion("0.1.0"))
	schema := readArchiveMember(t, archive, configschema.FileName)
	wantSchema, err := os.ReadFile(filepath.Join(root, "plugins", "slack", configschema.FileName))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(schema)
	if !bytes.Equal(schema, wantSchema) || manifest.ConfigSchema == nil || manifest.ConfigSchema.Source != configschema.FileName || manifest.ConfigSchema.SHA256 != hex.EncodeToString(digest[:]) || manifest.ConfigSchema.Size != int64(len(schema)) {
		t.Fatalf("Slack package schema contract = manifest:%#v schema:%q", manifest.ConfigSchema, schema)
	}
}

func TestRunPackageRejectsUnsupportedOrAmbiguousInputsBeforeBuild(t *testing.T) {
	root := packageFixture(t)
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(context.Context, buildRequest) ([]byte, error) {
		t.Fatal("builder called for invalid package request")
		return nil, nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })
	tests := [][]string{
		{"package", "--root", root, "--out", t.TempDir(), "--goos", "linux", "--goarch", "arm64", "--source-date-epoch", "1700000000"},
		{"package", "--root", root, "--out", t.TempDir(), "--goos", "darwin", "--goarch", "386", "--source-date-epoch", "1700000000"},
		{"package", "--root", root, "--out", t.TempDir(), "--goos", "darwin", "--goarch", "arm64", "--source-date-epoch", "0"},
		{"package", "--root", root, "--out", t.TempDir(), "--goos", "darwin", "--goarch", "arm64", "--source-date-epoch", "1700000000", "extra"},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != exitFailure || stdout.Len() != 0 || strings.TrimSpace(stderr.String()) == "" {
			t.Fatalf("args=%v exit=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestCodexPackageAuthenticatesItsInteractiveContractAndConfigurationSchema(t *testing.T) {
	root := packageFixture(t)
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(_ context.Context, request buildRequest) ([]byte, error) {
		return []byte("binary:" + request.Component.ID), nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })
	output := filepath.Join(t.TempDir(), "artifacts")
	if _, err := packageComponents(context.Background(), root, output, "darwin", "arm64", time.Unix(1700000000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(output, "bsbctl-plugin-codex_0.4.0_darwin_arm64.tar.gz")
	mark := readArchiveMember(t, archive, "assets/codex-mark.png")
	schema := readArchiveMember(t, archive, "config.schema.json")
	manifestData := readArchiveMember(t, archive, "manifest.json")
	manifest := assertPackagedManifestMatchesDefinition(t, manifestData, codexplugin.DefinitionForVersion("0.4.0"))
	digest := sha256.Sum256(schema)
	markDigest := sha256.Sum256(mark)
	wantMark := assets.Declaration{
		Source: "assets/codex-mark.png",
		SHA256: hex.EncodeToString(markDigest[:]), Size: int64(len(mark)), MediaType: "image/png",
	}
	if manifest.ConfigSchema == nil || manifest.ConfigSchema.Source != "config.schema.json" || manifest.ConfigSchema.SHA256 != hex.EncodeToString(digest[:]) || manifest.ConfigSchema.Size != int64(len(schema)) ||
		!reflect.DeepEqual(manifest.Assets, []assets.Declaration{wantMark}) {
		t.Fatalf("Codex package manifest = %#v", manifest)
	}
}

func TestBuildGoComponentUsesANewEmptyCacheForEveryBuildAndCleansIt(t *testing.T) {
	previousFinalizer := finalizeReleaseComponent
	finalizeReleaseComponent = func(context.Context, string, string) error { return nil }
	t.Cleanup(func() { finalizeReleaseComponent = previousFinalizer })
	temporary := t.TempDir()
	capture := filepath.Join(temporary, "capture")
	fakeGo := filepath.Join(temporary, "go")
	script := `#!/bin/sh
set -eu
entry_count=$(find "$GOCACHE" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')
printf '%s|%s\n' "$GOCACHE" "$entry_count" >> "$BSBCTL_BUILD_CAPTURE"
touch "$GOCACHE/used"
output=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then
    output=$2
    break
  fi
  shift
done
test -n "$output"
printf 'binary\n' > "$output"
`
	if err := os.WriteFile(fakeGo, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", temporary+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BSBCTL_BUILD_CAPTURE", capture)
	shared := t.TempDir()
	if err := os.WriteFile(filepath.Join(shared, "ambient"), []byte("warm"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOCACHE", shared)
	request := buildRequest{
		Root: t.TempDir(), GOOS: "darwin", GOARCH: "arm64",
		Component: releaseartifact.Component{ID: "bsbctl", Version: "0.1.0", Package: "./cmd/bsbctl", Binary: "bsbctl"},
	}
	for index := 0; index < 2; index++ {
		if _, err := buildGoComponent(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("cache captures = %q", data)
	}
	firstPath, firstEmpty, ok := strings.Cut(lines[0], "|")
	if !ok {
		t.Fatalf("first cache capture = %q", lines[0])
	}
	secondPath, secondEmpty, ok := strings.Cut(lines[1], "|")
	if !ok || firstPath == secondPath || firstEmpty != "0" || secondEmpty != "0" {
		t.Fatalf("cache captures = %q", data)
	}
	for _, path := range []string{firstPath, secondPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("build cache %q was not cleaned: %v", path, err)
		}
	}
}

func TestBuildEnvironmentRejectsAmbientWorkspaceAndUsesExactTarget(t *testing.T) {
	for name, value := range map[string]string{
		"GOWORK": "../go.work", "GOFLAGS": "-mod=mod", "GOOS": "linux", "GOARCH": "386",
		"GOCACHE": "/unexpected/cache", "CGO_ENABLED": "0", "GOPROXY": "https://unexpected.invalid",
		"GOAMD64": "v3", "GOARM64": "v9.0", "GOEXPERIMENT": "arenas", "GOFIPS140": "latest",
		"CGO_CPPFLAGS": "-DHOST_SPECIFIC", "CGO_CXXFLAGS": "-march=native", "CC": "custom-cc", "CXX": "custom-cxx",
		"SDKROOT": "/host/sdk", "MACOSX_DEPLOYMENT_TARGET": "99.0", "CPATH": "/host/include", "LIBRARY_PATH": "/host/lib",
	} {
		t.Setenv(name, value)
	}

	environment := buildEnvironment("darwin", "arm64", "/isolated/cache")
	want := map[string][]string{
		"GOWORK": {"off"}, "GOFLAGS": nil, "GOOS": {"darwin"}, "GOARCH": {"arm64"},
		"GOCACHE": {"/isolated/cache"}, "CGO_ENABLED": {"1"}, "GOPROXY": {"off"},
		"GOAMD64": nil, "GOARM64": {"v8.0"}, "GOEXPERIMENT": nil, "GOFIPS140": nil,
		"CGO_CPPFLAGS": nil, "CGO_CXXFLAGS": nil, "CC": nil, "CXX": nil,
		"SDKROOT": nil, "MACOSX_DEPLOYMENT_TARGET": nil, "CPATH": nil, "LIBRARY_PATH": nil,
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		want["CC"] = []string{"clang"}
		want["CXX"] = []string{"clang++"}
	}
	for name, wantValues := range want {
		if got := environmentValues(environment, name); !reflect.DeepEqual(got, wantValues) {
			t.Errorf("%s values = %q, want %q", name, got, wantValues)
		}
	}
}

func environmentValues(environment []string, name string) []string {
	var values []string
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == name {
			values = append(values, value)
		}
	}
	return values
}

func TestBuildGoComponentCreatesDeterministicBaseWithoutLinkerUUID(t *testing.T) {
	previousFinalizer := finalizeReleaseComponent
	finalizeReleaseComponent = func(context.Context, string, string) error { return nil }
	t.Cleanup(func() { finalizeReleaseComponent = previousFinalizer })
	temporary := t.TempDir()
	capture := filepath.Join(temporary, "capture")
	fakeGo := filepath.Join(temporary, "go")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" > "$BSBCTL_BUILD_CAPTURE"
output=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then
    output=$2
    break
  fi
  shift
done
test -n "$output"
printf 'binary\n' > "$output"
`
	if err := os.WriteFile(fakeGo, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", temporary+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BSBCTL_BUILD_CAPTURE", capture)
	request := buildRequest{
		Root: t.TempDir(), GOOS: "darwin", GOARCH: "arm64",
		Component: releaseartifact.Component{ID: "bsbctl", Version: "0.1.0", Package: "./cmd/bsbctl", Binary: "bsbctl"},
	}
	if _, err := buildGoComponent(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(arguments), "-extldflags=-Wl,-no_uuid") {
		t.Fatalf("Darwin base build does not suppress the nondeterministic linker UUID: %s", arguments)
	}
}

func TestBuildGoComponentProducesExecutableDarwinBinary(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin runtime compatibility requires dyld")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	request := buildRequest{
		Root: root, GOOS: "darwin", GOARCH: runtime.GOARCH,
		Component: releaseartifact.Component{ID: "bsbctl", Version: "0.1.0", Package: "./cmd/bsbctl", Binary: "bsbctl"},
	}
	binary, err := buildGoComponent(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildGoComponent(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(binary, second) {
		t.Fatal("Darwin release binaries are not reproducible")
	}
	path := filepath.Join(t.TempDir(), "bsbctl")
	if err := os.WriteFile(path, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(path, "help")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("execute release binary: %v: %s", err, output)
	}
}

func TestPackageComponentsGeneratesSBOMFromEachExactReachableModuleClosure(t *testing.T) {
	root := packageFixture(t)
	writeReleaseFile(t, filepath.Join(root, "release", "dependencies.json"), `{"schema_version":1,"modules":[{"module":"github.com/coder/websocket","version":"v1.8.15","license":"ISC","license_sha256":"cc0975a5f6305145bdd7b41ce9479632fdac3870e6ac4281f28017f18c767c4e"},{"module":"golang.org/x/text","version":"v0.41.0","license":"BSD-3-Clause","license_sha256":"911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad"}]}`)
	previousLister := listReleaseComponentDependencies
	listReleaseComponentDependencies = func(_ context.Context, request buildRequest) ([]releaseartifact.ModuleVersion, error) {
		switch request.Component.ID {
		case "bsbctl":
			return []releaseartifact.ModuleVersion{{Module: "github.com/coder/websocket", Version: "v1.8.15"}, {Module: "golang.org/x/text", Version: "v0.41.0"}}, nil
		case "dev.bsbctl.codex-quota":
			return nil, nil
		default:
			return []releaseartifact.ModuleVersion{{Module: "golang.org/x/text", Version: "v0.41.0"}}, nil
		}
	}
	t.Cleanup(func() { listReleaseComponentDependencies = previousLister })
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(_ context.Context, request buildRequest) ([]byte, error) {
		return []byte("binary:" + request.Component.ID), nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })
	output := filepath.Join(t.TempDir(), "artifacts")
	if _, err := packageComponents(context.Background(), root, output, "darwin", "arm64", time.Unix(1700000000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	core := readArchiveMember(t, filepath.Join(output, "bsbctl_0.1.0_darwin_arm64.tar.gz"), "sbom.cdx.json")
	calendar := readArchiveMember(t, filepath.Join(output, "bsbctl-plugin-calendar_0.5.0_darwin_arm64.tar.gz"), "sbom.cdx.json")
	codexApp := readArchiveMember(t, filepath.Join(output, "bsbctl-plugin-codex_0.4.0_darwin_arm64.tar.gz"), "sbom.cdx.json")
	codexQuota := readArchiveMember(t, filepath.Join(output, "bsbctl-plugin-codex-quota_0.2.0_darwin_arm64.tar.gz"), "sbom.cdx.json")
	mac := readArchiveMember(t, filepath.Join(output, "bsbctl-plugin-mac-resources_0.3.0_darwin_arm64.tar.gz"), "sbom.cdx.json")
	if !bytes.Contains(core, []byte("github.com/coder/websocket")) || !bytes.Contains(core, []byte("golang.org/x/text")) {
		t.Fatalf("core SBOM lacks exact closure: %s", core)
	}
	if bytes.Contains(codexQuota, []byte("github.com/coder/websocket")) || bytes.Contains(codexQuota, []byte("golang.org/x/text")) {
		t.Fatalf("Codex quota SBOM is not component-specific: %s", codexQuota)
	}
	if bytes.Contains(calendar, []byte("github.com/coder/websocket")) || !bytes.Contains(calendar, []byte("golang.org/x/text")) {
		t.Fatalf("Calendar SBOM is not component-specific: %s", calendar)
	}
	if bytes.Contains(codexApp, []byte("github.com/coder/websocket")) || !bytes.Contains(codexApp, []byte("golang.org/x/text")) {
		t.Fatalf("Codex app-server SBOM is not component-specific: %s", codexApp)
	}
	if bytes.Contains(mac, []byte("github.com/coder/websocket")) || !bytes.Contains(mac, []byte("golang.org/x/text")) {
		t.Fatalf("Mac resources SBOM is not component-specific: %s", mac)
	}
}

func TestPackageComponentsFailsClosedWhenReachableModuleReviewIsMissingOrMismatched(t *testing.T) {
	for _, module := range []releaseartifact.ModuleVersion{
		{Module: "example.com/missing", Version: "v1.0.0"},
		{Module: "github.com/coder/websocket", Version: "v1.8.14"},
	} {
		t.Run(module.Module+"@"+module.Version, func(t *testing.T) {
			root := packageFixture(t)
			previousLister := listReleaseComponentDependencies
			listReleaseComponentDependencies = func(context.Context, buildRequest) ([]releaseartifact.ModuleVersion, error) {
				return []releaseartifact.ModuleVersion{module}, nil
			}
			t.Cleanup(func() { listReleaseComponentDependencies = previousLister })
			previousBuilder := buildReleaseComponent
			buildReleaseComponent = func(context.Context, buildRequest) ([]byte, error) {
				t.Fatal("component built before its reachable module licenses matched")
				return nil, nil
			}
			t.Cleanup(func() { buildReleaseComponent = previousBuilder })
			if _, err := packageComponents(context.Background(), root, filepath.Join(t.TempDir(), "artifacts"), "darwin", "arm64", time.Unix(1700000000, 0).UTC()); err == nil {
				t.Fatal("packageComponents accepted an unreviewed reachable module/version")
			}
		})
	}
}

func TestPackageComponentsFailsClosedWhenLicenseDirectoryIsNotReviewed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "modified license",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, coderWebsocketLicensePath)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data[0] ^= 0xff
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unexpected license",
			mutate: func(t *testing.T, root string) {
				writeReleaseFile(t, filepath.Join(root, "LICENSES", "unreviewed.txt"), "unreviewed\n")
			},
		},
		{
			name: "symlinked license",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, coderWebsocketLicensePath)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, busyBarProtobufLicensePath), path); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := packageFixture(t)
			test.mutate(t, root)
			previousBuilder := buildReleaseComponent
			buildReleaseComponent = func(context.Context, buildRequest) ([]byte, error) {
				t.Fatal("component built before legal inputs were authenticated")
				return nil, nil
			}
			t.Cleanup(func() { buildReleaseComponent = previousBuilder })

			if _, err := packageComponents(context.Background(), root, filepath.Join(t.TempDir(), "artifacts"), "darwin", "arm64", time.Unix(1700000000, 0).UTC()); err == nil {
				t.Fatal("packageComponents accepted an unreviewed license directory")
			}
		})
	}
}

func TestPackageComponentsRejectsDependencyEvidenceThatDoesNotMatchReviewedLicense(t *testing.T) {
	tests := []struct {
		name        string
		oldEvidence string
		newEvidence string
	}{
		{name: "digest", oldEvidence: "cc0975a5f6305145bdd7b41ce9479632fdac3870e6ac4281f28017f18c767c4e", newEvidence: strings.Repeat("a", 64)},
		{name: "license identifier", oldEvidence: `"license":"ISC"`, newEvidence: `"license":"MIT"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := packageFixture(t)
			dependencyPath := filepath.Join(root, "release", "dependencies.json")
			data, err := os.ReadFile(dependencyPath)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.Replace(data, []byte(test.oldEvidence), []byte(test.newEvidence), 1)
			if err := os.WriteFile(dependencyPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			previousBuilder := buildReleaseComponent
			buildReleaseComponent = func(context.Context, buildRequest) ([]byte, error) {
				t.Fatal("component built before dependency license evidence was authenticated")
				return nil, nil
			}
			t.Cleanup(func() { buildReleaseComponent = previousBuilder })

			if _, err := packageComponents(t.Context(), root, filepath.Join(t.TempDir(), "artifacts"), "darwin", "arm64", time.Unix(1700000000, 0).UTC()); err == nil {
				t.Fatal("packageComponents accepted dependency evidence that identifies no reviewed license file")
			}
		})
	}
}

func TestRunPackageIdentifiesInvalidLegalInput(t *testing.T) {
	root := packageFixture(t)
	if err := os.Remove(filepath.Join(root, coderWebsocketLicensePath)); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"package", "--root", root, "--out", filepath.Join(t.TempDir(), "artifacts"), "--goos", "darwin", "--goarch", "arm64", "--source-date-epoch", "1700000000"}, &stdout, &stderr)
	if code != exitFailure || stdout.Len() != 0 || !strings.Contains(stderr.String(), coderWebsocketLicensePath) {
		t.Fatalf("package exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunInspectRejectsRechecksummedArchiveWithUnexpectedPayload(t *testing.T) {
	root := packageFixture(t)
	output := filepath.Join(t.TempDir(), "artifacts")
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(_ context.Context, request buildRequest) ([]byte, error) {
		return []byte(fmt.Sprintf("mach-o:%s:%s:%s:%s\n", request.Component.ID, request.Component.Version, request.GOOS, request.GOARCH)), nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })
	if _, err := packageComponents(context.Background(), root, output, "darwin", "arm64", time.Unix(1700000000, 0).UTC()); err != nil {
		t.Fatal(err)
	}

	archiveName := "bsbctl-plugin-codex-quota_0.2.0_darwin_arm64.tar.gz"
	rewriteArchiveMember(t, filepath.Join(output, archiveName), "manifest.json", "unexpected.json", nil)
	rebindArtifactChecksums(t, output, archiveName)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"inspect", "--dir", output}, &stdout, &stderr); code != exitFailure || stdout.Len() != 0 || stderr.String() != "releasectl: artifact verification failed\n" {
		t.Fatalf("rechecksummed unexpected payload inspect exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunInspectRejectsRechecksummedModifiedLegalArtifact(t *testing.T) {
	root := packageFixture(t)
	output := filepath.Join(t.TempDir(), "artifacts")
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(_ context.Context, request buildRequest) ([]byte, error) {
		return []byte("binary:" + request.Component.ID), nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })
	if _, err := packageComponents(context.Background(), root, output, "darwin", "arm64", time.Unix(1700000000, 0).UTC()); err != nil {
		t.Fatal(err)
	}

	archiveName := "bsbctl_0.1.0_darwin_arm64.tar.gz"
	rewriteArchiveMember(t, filepath.Join(output, archiveName), busyBarProtobufLicensePath, busyBarProtobufLicensePath, []byte("substituted license\n"))
	rebindArtifactChecksums(t, output, archiveName)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"inspect", "--dir", output}, &stdout, &stderr); code != exitFailure || stdout.Len() != 0 || stderr.String() != "releasectl: artifact verification failed\n" {
		t.Fatalf("rechecksummed legal artifact inspect exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func rewriteArchiveMember(t *testing.T, path, oldName, newName string, replacement []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	epoch := gzipReader.ModTime
	tarReader := tar.NewReader(gzipReader)
	entries := make([]releaseartifact.ArchiveEntry, 0, 6)
	found := false
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		name := header.Name
		if name == oldName {
			name = newName
			if replacement != nil {
				body = replacement
			}
			found = true
		}
		entries = append(entries, releaseartifact.ArchiveEntry{Name: name, Mode: os.FileMode(header.Mode), Data: body})
	}
	if err := gzipReader.Close(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("archive member %q was not found", oldName)
	}
	var rewritten bytes.Buffer
	if err := releaseartifact.WriteArchive(&rewritten, entries, epoch); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rewritten.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rebindArtifactChecksums(t *testing.T, directory, archiveName string) {
	t.Helper()
	manifestPath := filepath.Join(directory, "release-manifest.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest packageManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	archiveData, err := os.ReadFile(filepath.Join(directory, archiveName))
	if err != nil {
		t.Fatal(err)
	}
	archiveDigest := sha256.Sum256(archiveData)
	found := false
	for index := range manifest.Artifacts {
		if manifest.Artifacts[index].Filename == archiveName {
			manifest.Artifacts[index].SHA256 = hex.EncodeToString(archiveDigest[:])
			manifest.Artifacts[index].Size = int64(len(archiveData))
			found = true
		}
	}
	if !found {
		t.Fatalf("release manifest artifact %q was not found", archiveName)
	}
	manifestData, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestData = append(manifestData, '\n')
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifestData)

	checksumPath := filepath.Join(directory, "SHA256SUMS")
	checksumData, err := os.ReadFile(checksumPath)
	if err != nil {
		t.Fatal(err)
	}
	checksums, err := parseChecksums(checksumData)
	if err != nil {
		t.Fatal(err)
	}
	checksums[archiveName] = hex.EncodeToString(archiveDigest[:])
	checksums["release-manifest.json"] = hex.EncodeToString(manifestDigest[:])
	names := make([]string, 0, len(checksums))
	for name := range checksums {
		names = append(names, name)
	}
	sort.Strings(names)
	var output strings.Builder
	for _, name := range names {
		_, _ = fmt.Fprintf(&output, "%s  %s\n", checksums[name], name)
	}
	if err := os.WriteFile(checksumPath, []byte(output.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func packageFixture(t *testing.T) string {
	t.Helper()
	previousLister := listReleaseComponentDependencies
	listReleaseComponentDependencies = func(context.Context, buildRequest) ([]releaseartifact.ModuleVersion, error) {
		return []releaseartifact.ModuleVersion{{Module: "github.com/coder/websocket", Version: "v1.8.15"}}, nil
	}
	t.Cleanup(func() { listReleaseComponentDependencies = previousLister })
	root := t.TempDir()
	writeReleaseFile(t, filepath.Join(root, "release", "versions.json"), `{"schema_version":1,"components":[{"id":"bsbctl","kind":"core","version":"0.1.0","tag":"v0.1.0","package":"./cmd/bsbctl","binary":"bsbctl"},{"id":"dev.bsbctl.calendar","kind":"plugin","version":"0.5.0","tag":"plugin/calendar/v0.5.0","package":"./cmd/bsbctl-plugin-calendar","binary":"bsbctl-plugin-calendar"},{"id":"dev.bsbctl.codex","kind":"plugin","version":"0.4.0","tag":"plugin/codex/v0.4.0","package":"./cmd/bsbctl-plugin-codex","binary":"bsbctl-plugin-codex"},{"id":"dev.bsbctl.codex-quota","kind":"plugin","version":"0.2.0","tag":"plugin/codex-quota/v0.2.0","package":"./cmd/bsbctl-plugin-codex-quota","binary":"bsbctl-plugin-codex-quota"},{"id":"dev.bsbctl.github-notifications","kind":"plugin","version":"0.1.0","tag":"plugin/github-notifications/v0.1.0","package":"./cmd/bsbctl-plugin-github-notifications","binary":"bsbctl-plugin-github-notifications"},{"id":"dev.bsbctl.mac-resources","kind":"plugin","version":"0.3.0","tag":"plugin/mac-resources/v0.3.0","package":"./cmd/bsbctl-plugin-mac-resources","binary":"bsbctl-plugin-mac-resources"},{"id":"dev.bsbctl.slack","kind":"plugin","version":"0.1.0","tag":"plugin/slack/v0.1.0","package":"./cmd/bsbctl-plugin-slack","binary":"bsbctl-plugin-slack"}]}`)
	writeReleaseFile(t, filepath.Join(root, "release", "dependencies.json"), `{"schema_version":1,"modules":[{"module":"github.com/coder/websocket","version":"v1.8.15","license":"ISC","license_sha256":"cc0975a5f6305145bdd7b41ce9479632fdac3870e6ac4281f28017f18c767c4e"}]}`)
	writeReleaseFile(t, filepath.Join(root, "plugins", "codex", "config.schema.json"), `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false}`)
	writeReleaseFile(t, filepath.Join(root, "plugins", "calendar", "config.schema.json"), `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"reminder_lead_minutes":{"type":"integer","minimum":1,"maximum":60}},"additionalProperties":false}`)
	writeReleaseFile(t, filepath.Join(root, "plugins", "codexquota", "config.schema.json"), `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false}`)
	writeReleaseFile(t, filepath.Join(root, "plugins", "githubnotifications", "config.schema.json"), `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false}`)
	writeReleaseFile(t, filepath.Join(root, "plugins", "macresources", "config.schema.json"), `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false}`)
	writeReleaseFile(t, filepath.Join(root, "plugins", "slack", "config.schema.json"), `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false}`)
	mark, err := os.ReadFile(filepath.Join("..", "..", "plugins", "codexquota", "assets", "codex-mark.png"))
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseFile(t, filepath.Join(root, "plugins", "codexquota", "assets", "codex-mark.png"), string(mark))
	writeReleaseFile(t, filepath.Join(root, "plugins", "codex", "assets", "codex-mark.png"), string(mark))
	githubMark, err := os.ReadFile(filepath.Join("..", "..", "plugins", "githubnotifications", "assets", "github-mark.png"))
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseFile(t, filepath.Join(root, "plugins", "githubnotifications", "assets", "github-mark.png"), string(githubMark))
	slackMark, err := os.ReadFile(filepath.Join("..", "..", "plugins", "slack", "assets", "slack-mark.png"))
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseFile(t, filepath.Join(root, "plugins", "slack", "assets", "slack-mark.png"), string(slackMark))
	writeReleaseFile(t, filepath.Join(root, "LICENSE"), "license\n")
	writeReleaseFile(t, filepath.Join(root, "NOTICE"), "notice\n")
	writeReleaseFile(t, filepath.Join(root, "THIRD_PARTY_NOTICES.md"), "third party\n")
	for licensePath := range reviewedLegalArtifacts {
		license, err := os.ReadFile(filepath.Join("..", "..", licensePath))
		if err != nil {
			t.Fatal(err)
		}
		writeReleaseFile(t, filepath.Join(root, licensePath), string(license))
	}
	writeReleaseFile(t, filepath.Join(root, "install.sh"), "#!/bin/sh\nexit 0\n")
	return root
}

func readArchiveMember(t *testing.T, path, name string) []byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			t.Fatalf("archive member %q was not found", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == name {
			data, err := io.ReadAll(tarReader)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
	}
}

func snapshotDirectory(t *testing.T, root string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("unexpected directory %q", entry.Name())
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	result := make(map[string][]byte, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		result[name] = data
	}
	return result
}
