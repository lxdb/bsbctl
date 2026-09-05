package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/releaseartifact"
)

type stringListFlag []string

func (values *stringListFlag) String() string { return strings.Join(*values, ",") }

func (values *stringListFlag) Set(value string) error {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return errors.New("path is required")
	}
	*values = append(*values, value)
	return nil
}

func runCatalog(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("catalog")
	root := flags.String("root", ".", "repository root")
	baseURL := flags.String("base-url", "", "HTTPS release download base URL")
	sequenceRaw := flags.String("sequence", "", "monotonic catalog sequence")
	generatedRaw := flags.String("generated-at", "", "RFC3339 generation time")
	output := flags.String("out", "", "catalog output")
	var artifactDirectories stringListFlag
	flags.Var(&artifactDirectories, "artifacts", "verified architecture artifact directory (repeat twice)")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *output == "" {
		_, _ = fmt.Fprintln(stderr, "releasectl: catalog generation failed")
		return exitFailure
	}
	sequence, err := strconv.ParseUint(*sequenceRaw, 10, 64)
	if err != nil || sequence == 0 {
		_, _ = fmt.Fprintln(stderr, "releasectl: catalog generation failed")
		return exitFailure
	}
	generatedAt, generatedErr := time.Parse(time.RFC3339, *generatedRaw)
	if generatedErr != nil || generatedAt.IsZero() {
		_, _ = fmt.Fprintln(stderr, "releasectl: catalog generation failed")
		return exitFailure
	}
	entries, err := catalogEntries(*root, artifactDirectories, *baseURL)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "releasectl: catalog generation failed")
		return exitFailure
	}
	document := catalog.Catalog{
		Version: 1, Channel: "stable", Sequence: sequence,
		GeneratedAt: generatedAt.UTC(), Plugins: entries,
	}
	data, err := json.Marshal(document)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "releasectl: catalog generation failed")
		return exitFailure
	}
	data = append(data, '\n')
	if err := writeExclusiveFile(*output, data); err != nil {
		_, _ = fmt.Fprintln(stderr, "releasectl: catalog generation failed")
		return exitFailure
	}
	_, _ = fmt.Fprintf(stdout, "stable catalog: written with %d platform entries\n", len(entries))
	return exitSuccess
}

func catalogEntries(root string, artifactDirectories []string, rawBaseURL string) ([]catalog.Entry, error) {
	if len(artifactDirectories) != 2 {
		return nil, errors.New("exactly two artifact directories are required")
	}
	baseURL, err := validateReleaseBaseURL(rawBaseURL)
	if err != nil {
		return nil, err
	}
	planData, err := readBoundedFile(filepath.Join(root, "release", "versions.json"))
	if err != nil {
		return nil, err
	}
	plan, err := releaseartifact.DecodePlan(planData)
	if err != nil {
		return nil, err
	}
	components := make(map[string]releaseartifact.Component, len(plan.Components))
	pluginCount := 0
	for _, component := range plan.Components {
		components[component.ID] = component
		if component.Kind == "plugin" {
			pluginCount++
		}
	}
	seenArchitecture := make(map[string]struct{}, 2)
	entries := make([]catalog.Entry, 0, pluginCount*2)
	for _, directory := range artifactDirectories {
		if err := verifyArtifactDirectory(directory); err != nil {
			return nil, err
		}
		manifestData, err := readBoundedFile(filepath.Join(directory, "release-manifest.json"))
		if err != nil {
			return nil, err
		}
		manifest, err := decodePackageManifest(manifestData)
		if err != nil || manifest.GOOS != "darwin" || (manifest.GOARCH != "arm64" && manifest.GOARCH != "amd64") {
			return nil, errors.New("release manifest platform is invalid")
		}
		if _, exists := seenArchitecture[manifest.GOARCH]; exists {
			return nil, errors.New("release manifest architecture is duplicated")
		}
		seenArchitecture[manifest.GOARCH] = struct{}{}
		foundPlugins := 0
		for _, artifact := range manifest.Artifacts {
			component, exists := components[artifact.Component]
			if !exists || artifact.Version != component.Version || artifact.Tag != component.Tag || artifact.Size < 1 || len(artifact.SHA256) != 64 {
				return nil, errors.New("release manifest component is invalid")
			}
			wantFilename := fmt.Sprintf("%s_%s_darwin_%s.tar.gz", component.Binary, component.Version, manifest.GOARCH)
			if artifact.Filename != wantFilename {
				return nil, errors.New("release manifest filename is invalid")
			}
			if component.Kind != "plugin" {
				continue
			}
			foundPlugins++
			entries = append(entries, catalog.Entry{
				ID: component.ID, Version: component.Version, OS: "darwin", Arch: manifest.GOARCH,
				URL:    baseURL + "/" + url.PathEscape(component.Tag) + "/" + url.PathEscape(artifact.Filename),
				SHA256: artifact.SHA256, CompressedSize: artifact.Size, ArchiveFormat: "tar.gz",
				Executable: component.Binary, Manifest: "manifest.json",
			})
		}
		if foundPlugins != pluginCount {
			return nil, errors.New("release manifest plugin set is incomplete")
		}
	}
	if len(seenArchitecture) != 2 || len(entries) != pluginCount*2 {
		return nil, errors.New("release platform set is incomplete")
	}
	slices.SortFunc(entries, func(left, right catalog.Entry) int {
		return cmp.Or(cmp.Compare(left.ID, right.ID), cmp.Compare(left.Arch, right.Arch))
	})
	return entries, nil
}

func validateReleaseBaseURL(raw string) (string, error) {
	if strings.TrimSpace(raw) != raw || strings.HasSuffix(raw, "/") {
		return "", errors.New("release base URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", errors.New("release base URL is invalid")
	}
	return parsed.String(), nil
}

func decodePackageManifest(data []byte) (packageManifest, error) {
	document := packageManifest{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return packageManifest{}, errors.New("release manifest is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return packageManifest{}, errors.New("release manifest is invalid")
	}
	if document.SchemaVersion != 1 || document.SourceDateEpoch <= 0 || len(document.Artifacts) != len(archiveComponentContracts) {
		return packageManifest{}, errors.New("release manifest is invalid")
	}
	return document, nil
}
