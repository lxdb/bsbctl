package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/configschema"
	"github.com/lxdb/bsbctl/internal/releaseartifact"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func goListComponentDependencies(ctx context.Context, request buildRequest) ([]releaseartifact.ModuleVersion, error) {
	goCache, err := os.MkdirTemp("", "bsbctl-release-list-gocache-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(goCache)
	const format = `{{with .Module}}{{if not .Main}}{{.Path}}{{"\t"}}{{.Version}}{{"\n"}}{{end}}{{end}}`
	command := exec.CommandContext(ctx, "go", "list", "-mod=readonly", "-deps", "-f", format, request.Component.Package)
	command.Dir = request.Root
	command.Env = buildEnvironment(request.GOOS, request.GOARCH, goCache)
	output := &boundedOutput{remaining: maxDependencyListBytes}
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil || output.truncated {
		return nil, errors.New("bounded offline component dependency listing failed")
	}
	seen := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(output.String()))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 2 || strings.TrimSpace(fields[0]) != fields[0] || strings.TrimSpace(fields[1]) != fields[1] || fields[0] == "" || fields[1] == "" {
			return nil, errors.New("component dependency listing is invalid")
		}
		if version, exists := seen[fields[0]]; exists && version != fields[1] {
			return nil, errors.New("component dependency listing contains multiple versions")
		}
		seen[fields[0]] = fields[1]
		if len(seen) > 4096 {
			return nil, errors.New("component dependency listing exceeds module limit")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("component dependency listing scan failed: %w", err)
	}
	if len(seen) == 0 {
		return []releaseartifact.ModuleVersion{}, nil
	}
	modules := make([]releaseartifact.ModuleVersion, 0, len(seen))
	for module, version := range seen {
		modules = append(modules, releaseartifact.ModuleVersion{Module: module, Version: version})
	}
	slices.SortFunc(modules, func(left, right releaseartifact.ModuleVersion) int { return cmp.Compare(left.Module, right.Module) })
	return modules, nil
}

func componentMetadata(root string, component releaseartifact.Component, binary []byte, digest []byte, goos, goarch string, epoch time.Time) ([]byte, string, []releaseartifact.ArchiveEntry, error) {
	if component.Kind == "core" {
		value := struct {
			SchemaVersion   int    `json:"schema_version"`
			ID              string `json:"id"`
			Version         string `json:"version"`
			Tag             string `json:"tag"`
			GOOS            string `json:"goos"`
			GOARCH          string `json:"goarch"`
			Executable      string `json:"executable"`
			ExecutableSHA   string `json:"executable_sha256"`
			ExecutableSize  int64  `json:"executable_size"`
			SourceDateEpoch int64  `json:"source_date_epoch"`
		}{1, component.ID, component.Version, component.Tag, goos, goarch, component.Binary, hex.EncodeToString(digest), int64(len(binary)), epoch.Unix()}
		data, err := json.Marshal(value)
		return append(data, '\n'), "release-metadata.json", nil, err
	}
	contract, exists := archiveComponentContracts[component.ID]
	if !exists || contract.MetadataName != "manifest.json" || contract.Definition == nil {
		return nil, "", nil, errors.New("release plugin contract is unavailable")
	}
	definition := contract.Definition(component.Version)
	if definition.ID != component.ID || definition.Version != component.Version {
		return nil, "", nil, errors.New("release plugin definition identity does not match component")
	}
	var declaration *configschema.Declaration
	var extraEntries []releaseartifact.ArchiveEntry
	if contract.ConfigSchemaPath != "" {
		schema, err := readBoundedFile(filepath.Join(root, filepath.FromSlash(contract.ConfigSchemaPath)))
		if err != nil {
			return nil, "", nil, err
		}
		schemaDigest := sha256.Sum256(schema)
		declaration = &configschema.Declaration{Source: configschema.FileName, SHA256: hex.EncodeToString(schemaDigest[:]), Size: int64(len(schema))}
		extraEntries = []releaseartifact.ArchiveEntry{{Name: configschema.FileName, Mode: 0o644, Data: schema}}
	}
	for _, declaration := range contract.Assets {
		content, err := readBoundedFile(filepath.Join(root, filepath.FromSlash(contract.AssetRoot), filepath.FromSlash(declaration.Source)))
		if err != nil {
			return nil, "", nil, err
		}
		digest := sha256.Sum256(content)
		if declaration.SHA256 != hex.EncodeToString(digest[:]) || declaration.Size != int64(len(content)) {
			return nil, "", nil, errors.New("release plugin asset does not match its declaration")
		}
		extraEntries = append(extraEntries, releaseartifact.ArchiveEntry{Name: declaration.Source, Mode: 0o644, Data: content})
	}
	value := struct {
		ID              string                         `json:"id"`
		Version         string                         `json:"version"`
		ProtocolVersion string                         `json:"protocol_version"`
		Executable      string                         `json:"executable"`
		ExecutableSHA   string                         `json:"executable_sha256"`
		ExecutableSize  int64                          `json:"executable_size"`
		ExecutionModes  []protocol.ExecutionMode       `json:"execution_modes"`
		Channels        []protocol.Channel             `json:"channels"`
		Operations      []protocol.OperationDescriptor `json:"operations,omitempty"`
		ConfigSchema    *configschema.Declaration      `json:"config_schema,omitempty"`
		Assets          []assets.Declaration           `json:"assets"`
	}{
		component.ID, component.Version, protocol.Version,
		component.Binary, hex.EncodeToString(digest), int64(len(binary)), slices.Clone(definition.Contract.ExecutionModes), slices.Clone(definition.Contract.Channels),
		slices.Clone(definition.Contract.Operations), declaration, append([]assets.Declaration{}, contract.Assets...),
	}
	data, err := json.Marshal(value)
	return append(data, '\n'), "manifest.json", extraEntries, err
}

func buildGoComponent(ctx context.Context, request buildRequest) ([]byte, error) {
	directory, err := os.MkdirTemp("", "bsbctl-release-build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	goCache, err := os.MkdirTemp("", "bsbctl-release-gocache-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(goCache)
	output := filepath.Join(directory, request.Component.Binary)
	linkerFlags := "-buildid= -X main.version=" + request.Component.Version + " -extldflags=-Wl,-no_uuid"
	command := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags="+linkerFlags, "-o", output, request.Component.Package)
	command.Dir = request.Root
	command.Env = buildEnvironment(request.GOOS, request.GOARCH, goCache)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return nil, err
	}
	if err := finalizeReleaseComponent(ctx, output, request.GOOS); err != nil {
		return nil, err
	}
	return os.ReadFile(output)
}

func buildEnvironment(goos, goarch, goCache string) []string {
	blocked := slices.Clone(ambientBuildVariables)
	blocked = append(blocked, "CGO_ENABLED", "GOARCH", "GOCACHE", "GOFLAGS", "GOOS")
	result := isolatedGoEnvironment(blocked...)
	result = append(result,
		"CGO_ENABLED=1", "GOARCH="+goarch, "GOCACHE="+goCache, "GOOS="+goos,
	)
	switch goarch {
	case "amd64":
		result = append(result, "GOAMD64=v1")
	case "arm64":
		result = append(result, "GOARM64=v8.0")
	}
	if runtime.GOOS == "darwin" && goos == "darwin" && runtime.GOARCH != goarch {
		arch := goarch
		if goarch == "amd64" {
			arch = "x86_64"
		}
		result = append(result, "CC=clang", "CXX=clang++", "CGO_CFLAGS=-arch "+arch, "CGO_LDFLAGS=-arch "+arch)
	}
	return result
}

var ambientBuildVariables = []string{
	"AR", "CC", "CFLAGS", "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS", "CPATH", "CPPFLAGS", "CXX", "CXXFLAGS", "FC", "LDFLAGS", "LIBRARY_PATH", "PKG_CONFIG",
	"C_INCLUDE_PATH", "CPLUS_INCLUDE_PATH", "DYLD_LIBRARY_PATH", "MACOSX_DEPLOYMENT_TARGET", "SDKROOT",
	"GO386", "GOAMD64", "GOARM", "GOARM64", "GOEXPERIMENT", "GOFIPS140", "GOMIPS", "GOMIPS64", "GOPPC64", "GORISCV64", "GOWASM",
}

func isolatedGoEnvironment(additionalBlocked ...string) []string {
	fixed := []string{
		"GOAUTH=off", "GOENV=off", "GOINSECURE=", "GONOPROXY=none", "GONOSUMDB=none",
		"GOPRIVATE=", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOVCS=*:off", "GOWORK=off",
	}
	blocked := make(map[string]struct{}, len(fixed)+len(additionalBlocked))
	for _, entry := range fixed {
		name, _, _ := strings.Cut(entry, "=")
		blocked[name] = struct{}{}
	}
	for _, name := range additionalBlocked {
		blocked[name] = struct{}{}
	}
	result := make([]string, 0, len(os.Environ())+len(fixed))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, exists := blocked[name]; !exists {
			result = append(result, entry)
		}
	}
	return append(result, fixed...)
}
