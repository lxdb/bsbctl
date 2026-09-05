package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/releaseartifact"
	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
)

const maxReleaseInputBytes = 2 << 20
const maxDependencyListBytes = 2 << 20

type buildRequest struct {
	Root      string
	Component releaseartifact.Component
	GOOS      string
	GOARCH    string
}

var (
	buildReleaseComponent            = buildGoComponent
	listReleaseComponentDependencies = goListComponentDependencies
	finalizeReleaseComponent         = finalizeDarwinReleaseComponent
)

type packagedArtifact struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	Tag       string `json:"tag"`
	Filename  string `json:"filename"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

type packageManifest struct {
	SchemaVersion   int                `json:"schema_version"`
	SourceDateEpoch int64              `json:"source_date_epoch"`
	GOOS            string             `json:"goos"`
	GOARCH          string             `json:"goarch"`
	Artifacts       []packagedArtifact `json:"artifacts"`
}

type archiveComponentContract struct {
	Binary           string
	TagPrefix        string
	MetadataName     string
	Definition       func(string) pluginsdk.Definition
	ConfigSchemaPath string
	AssetRoot        string
	Assets           []assets.Declaration
}

const (
	coderWebsocketLicensePath         = "LICENSES/coder-websocket-ISC.txt"
	busylibGoLicensePath              = "LICENSES/busylib-go-MIT.txt"
	jsonschemaLicensePath             = "LICENSES/jsonschema-Apache-2.0.txt"
	xSysLicensePath                   = "LICENSES/x-sys-BSD-3-Clause.txt"
	xTextLicensePath                  = "LICENSES/x-text-BSD-3-Clause.txt"
	protobufLicensePath               = "LICENSES/protobuf-BSD-3-Clause.txt"
	busyBarProtobufLicensePath        = "LICENSES/busybar-protobuf-MIT.txt"
	busyBarFirmwarePreviewLicensePath = "LICENSES/busybar-firmware-GPL-2.0-or-later.txt"
	lobeHubIconsLicensePath           = "LICENSES/lobehub-icons-MIT.txt"
)

var reviewedLegalArtifacts = map[string]struct {
	Module  string
	License string
	Size    int64
	SHA256  string
}{
	coderWebsocketLicensePath:         {Module: "github.com/coder/websocket", License: "ISC", Size: 720, SHA256: "cc0975a5f6305145bdd7b41ce9479632fdac3870e6ac4281f28017f18c767c4e"},
	busylibGoLicensePath:              {Module: "github.com/lxdb/busylib-go", License: "MIT", Size: 1061, SHA256: "b7ec5c1207ecb19fc7d4ed4436fe50caed09068f9fbd68bbe86f1065811c05be"},
	jsonschemaLicensePath:             {Module: "github.com/santhosh-tekuri/jsonschema/v6", License: "Apache-2.0", Size: 10142, SHA256: "09e8a9bcec8067104652c168685ab0931e7868f9c8284b66f5ae6edae5f1130b"},
	xSysLicensePath:                   {Module: "golang.org/x/sys", License: "BSD-3-Clause", Size: 1453, SHA256: "911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad"},
	xTextLicensePath:                  {Module: "golang.org/x/text", License: "BSD-3-Clause", Size: 1453, SHA256: "911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad"},
	protobufLicensePath:               {Module: "google.golang.org/protobuf", License: "BSD-3-Clause", Size: 1479, SHA256: "4835612df0098ca95f8e7d9e3bffcb02358d435dbb38057c844c99d7f725eb20"},
	busyBarProtobufLicensePath:        {Size: 1065, SHA256: "16ab8153ca12be65d7c707e696740e4f1e3c2531f8c8a04dbc4286478a8bb41f"},
	busyBarFirmwarePreviewLicensePath: {Size: 17337, SHA256: "aaf135472f81c5b4a0dca9367e5bb5e9750032b5bebe5442b36e4c0a47430df3"},
	lobeHubIconsLicensePath:           {Size: 1064, SHA256: "add9d7531d1b21646317a8958e38fc727506fa39d24bdecb44154d943c82753a"},
}

var archiveComponentContracts = firstPartyArchiveContracts()

func firstPartyArchiveContracts() map[string]archiveComponentContract {
	contracts := map[string]archiveComponentContract{
		"bsbctl": {Binary: "bsbctl", TagPrefix: "v", MetadataName: "release-metadata.json"},
	}
	for _, descriptor := range firstpartyplugins.All() {
		contracts[descriptor.ID] = archiveComponentContract{
			Binary: descriptor.Binary, TagPrefix: descriptor.TagPrefix, MetadataName: "manifest.json",
			Definition: descriptor.DefinitionForVersion, ConfigSchemaPath: descriptor.SchemaPath,
			AssetRoot: descriptor.AssetRoot, Assets: descriptor.Assets,
		}
	}
	return contracts
}

func runPackage(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("package")
	root := flags.String("root", ".", "repository root")
	output := flags.String("out", "", "output directory")
	goos := flags.String("goos", "", "target operating system")
	goarch := flags.String("goarch", "", "target architecture")
	epochRaw := flags.String("source-date-epoch", "", "canonical Unix timestamp")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *output == "" || *goos != "darwin" || (*goarch != "arm64" && *goarch != "amd64") {
		_, _ = fmt.Fprintln(stderr, "releasectl: invalid package arguments")
		return exitFailure
	}
	epochSeconds, err := strconv.ParseInt(*epochRaw, 10, 64)
	if err != nil || epochSeconds <= 0 {
		_, _ = fmt.Fprintln(stderr, "releasectl: invalid package arguments")
		return exitFailure
	}
	epoch := time.Unix(epochSeconds, 0).UTC()
	count, err := packageComponents(ctx, *root, *output, *goos, *goarch, epoch)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "releasectl: package failed: %v\n", err)
		return exitFailure
	}
	_, _ = fmt.Fprintf(stdout, "release artifacts: packaged %d component(s) for %s/%s\n", count, *goos, *goarch)
	return exitSuccess
}

func packageComponents(ctx context.Context, root, output, goos, goarch string, epoch time.Time) (int, error) {
	if err := prepareOutputDirectory(output); err != nil {
		return 0, err
	}
	planData, err := readBoundedFile(filepath.Join(root, "release", "versions.json"))
	if err != nil {
		return 0, err
	}
	plan, err := releaseartifact.DecodePlan(planData)
	if err != nil {
		return 0, err
	}
	dependencyData, err := readBoundedFile(filepath.Join(root, "release", "dependencies.json"))
	if err != nil {
		return 0, err
	}
	inventory, err := releaseartifact.DecodeDependencyInventory(dependencyData)
	if err != nil {
		return 0, err
	}
	legal := make(map[string][]byte, len(reviewedLegalArtifacts)+3)
	for _, name := range []string{"LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md"} {
		legal[name], err = readBoundedFile(filepath.Join(root, name))
		if err != nil {
			return 0, fmt.Errorf("read release legal input %s: %w", name, err)
		}
		if len(legal[name]) == 0 {
			return 0, fmt.Errorf("release legal input %s is empty", name)
		}
	}
	licenseEntries, err := os.ReadDir(filepath.Join(root, "LICENSES"))
	if err != nil {
		return 0, fmt.Errorf("read release license directory LICENSES: %w", err)
	}
	for _, entry := range licenseEntries {
		name := path.Join("LICENSES", entry.Name())
		info, infoErr := entry.Info()
		_, reviewed := reviewedLegalArtifacts[name]
		if infoErr != nil {
			return 0, fmt.Errorf("inspect release legal input %s: %w", name, infoErr)
		}
		if entry.Type()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !reviewed {
			return 0, fmt.Errorf("release legal input %s is not a reviewed regular file", name)
		}
	}
	for name, expected := range reviewedLegalArtifacts {
		legal[name], err = readBoundedFile(filepath.Join(root, name))
		if err != nil {
			return 0, fmt.Errorf("read release legal input %s: %w", name, err)
		}
		if int64(len(legal[name])) != expected.Size {
			return 0, fmt.Errorf("release legal input %s has size %d, want %d", name, len(legal[name]), expected.Size)
		}
		digest := sha256.Sum256(legal[name])
		if hex.EncodeToString(digest[:]) != expected.SHA256 {
			return 0, fmt.Errorf("release legal input %s has an unreviewed SHA-256 digest", name)
		}
	}
	if err := validateDependencyLicenseEvidence(inventory); err != nil {
		return 0, err
	}
	components := slices.Clone(plan.Components)
	slices.SortFunc(components, func(left, right releaseartifact.Component) int { return cmp.Compare(left.Binary, right.Binary) })
	artifacts := make([]packagedArtifact, 0, len(components))
	for _, component := range components {
		request := buildRequest{Root: root, Component: component, GOOS: goos, GOARCH: goarch}
		reachable, err := listReleaseComponentDependencies(ctx, request)
		if err != nil {
			return 0, fmt.Errorf("release component dependency closure failed: %w", err)
		}
		componentInventory, err := releaseartifact.SelectDependencies(inventory, reachable)
		if err != nil {
			return 0, err
		}
		binary, err := buildReleaseComponent(ctx, request)
		if err != nil || len(binary) == 0 {
			return 0, errors.New("release component build failed")
		}
		binaryDigest := sha256.Sum256(binary)
		sbom, err := releaseartifact.CycloneDX(component, componentInventory)
		if err != nil {
			return 0, err
		}
		metadata, metadataName, extraEntries, err := componentMetadata(root, component, binary, binaryDigest[:], goos, goarch, epoch)
		if err != nil {
			return 0, err
		}
		entries := []releaseartifact.ArchiveEntry{
			{Name: component.Binary, Mode: 0o755, Data: binary},
			{Name: metadataName, Mode: 0o644, Data: metadata},
			{Name: "LICENSE", Mode: 0o644, Data: legal["LICENSE"]},
			{Name: "NOTICE", Mode: 0o644, Data: legal["NOTICE"]},
			{Name: "THIRD_PARTY_NOTICES.md", Mode: 0o644, Data: legal["THIRD_PARTY_NOTICES.md"]},
			{Name: "sbom.cdx.json", Mode: 0o644, Data: sbom},
		}
		for name := range reviewedLegalArtifacts {
			entries = append(entries, releaseartifact.ArchiveEntry{Name: name, Mode: 0o644, Data: legal[name]})
		}
		entries = append(entries, extraEntries...)
		filename := fmt.Sprintf("%s_%s_%s_%s.tar.gz", component.Binary, component.Version, goos, goarch)
		archivePath := filepath.Join(output, filename)
		file, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return 0, err
		}
		writeErr := releaseartifact.WriteArchive(file, entries, epoch)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return 0, errors.Join(writeErr, closeErr)
		}
		if err := os.Chtimes(archivePath, epoch, epoch); err != nil {
			return 0, err
		}
		archive, err := os.ReadFile(archivePath)
		if err != nil {
			return 0, err
		}
		digest := sha256.Sum256(archive)
		artifacts = append(artifacts, packagedArtifact{
			Component: component.ID, Version: component.Version, Tag: component.Tag, Filename: filename,
			SHA256: hex.EncodeToString(digest[:]), Size: int64(len(archive)),
		})
	}
	manifest := packageManifest{SchemaVersion: 1, SourceDateEpoch: epoch.Unix(), GOOS: goos, GOARCH: goarch, Artifacts: artifacts}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return 0, err
	}
	manifestData = append(manifestData, '\n')
	if err := writeCanonicalFile(filepath.Join(output, "release-manifest.json"), manifestData, epoch); err != nil {
		return 0, err
	}
	manifestDigest := sha256.Sum256(manifestData)
	checksumEntries := slices.Clone(artifacts)
	checksumEntries = append(checksumEntries, packagedArtifact{Filename: "release-manifest.json", SHA256: hex.EncodeToString(manifestDigest[:])})
	slices.SortFunc(checksumEntries, func(left, right packagedArtifact) int { return cmp.Compare(left.Filename, right.Filename) })
	var checksums strings.Builder
	for _, artifact := range checksumEntries {
		_, _ = fmt.Fprintf(&checksums, "%s  %s\n", artifact.SHA256, artifact.Filename)
	}
	if err := writeCanonicalFile(filepath.Join(output, "SHA256SUMS"), []byte(checksums.String()), epoch); err != nil {
		return 0, err
	}
	return len(artifacts), nil
}

func validateDependencyLicenseEvidence(inventory releaseartifact.DependencyInventory) error {
	reviewedByModule := make(map[string]struct {
		Path    string
		License string
		SHA256  string
	})
	for artifactPath, artifact := range reviewedLegalArtifacts {
		if artifact.Module == "" {
			continue
		}
		reviewedByModule[artifact.Module] = struct {
			Path    string
			License string
			SHA256  string
		}{Path: artifactPath, License: artifact.License, SHA256: artifact.SHA256}
	}
	for _, dependency := range inventory.Modules {
		reviewed, exists := reviewedByModule[dependency.Module]
		if !exists || dependency.License != reviewed.License || dependency.LicenseSHA256 != reviewed.SHA256 {
			if !exists {
				return fmt.Errorf("dependency %s@%s has no reviewed license file", dependency.Module, dependency.Version)
			}
			return fmt.Errorf("dependency %s@%s license evidence does not match %s", dependency.Module, dependency.Version, reviewed.Path)
		}
	}
	return nil
}
