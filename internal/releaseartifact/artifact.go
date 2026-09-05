// Package releaseartifact defines deterministic public-release metadata and files.
package releaseartifact

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
)

var (
	stableVersionPattern  = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	binaryPattern         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	modulePattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~/-]{0,255}$`)
	lowerSHA256Pattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	releaseBlockerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

type Plan struct {
	SchemaVersion int         `json:"schema_version"`
	Components    []Component `json:"components"`
}

type Component struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Version string `json:"version"`
	Tag     string `json:"tag"`
	Package string `json:"package"`
	Binary  string `json:"binary"`
}

type ArchiveEntry struct {
	Name string
	Mode fs.FileMode
	Data []byte
}

type DependencyInventory struct {
	SchemaVersion int          `json:"schema_version"`
	Modules       []Dependency `json:"modules"`
}

type Dependency struct {
	Module           string `json:"module"`
	Version          string `json:"version"`
	License          string `json:"license"`
	LicenseSHA256    string `json:"license_sha256"`
	ReleaseBlocked   bool   `json:"release_blocked,omitempty"`
	ReleaseBlockerID string `json:"release_blocker_id,omitempty"`
}

// CatalogPredecessor binds publication to either the one explicit first
// release or the exact signed catalog bytes from the preceding core release.
type CatalogPredecessor struct {
	SchemaVersion   int    `json:"schema_version"`
	FirstRelease    bool   `json:"first_release,omitempty"`
	Tag             string `json:"tag,omitempty"`
	CatalogSHA256   string `json:"catalog_sha256,omitempty"`
	SignatureSHA256 string `json:"signature_sha256,omitempty"`
}

// DecodeCatalogPredecessor strictly decodes the tracked stable-catalog chain.
func DecodeCatalogPredecessor(data []byte) (CatalogPredecessor, error) {
	var predecessor CatalogPredecessor
	if err := strictJSON(data, &predecessor); err != nil || predecessor.SchemaVersion != 1 {
		return CatalogPredecessor{}, errors.New("catalog predecessor metadata is invalid")
	}
	if predecessor.FirstRelease {
		if predecessor.Tag != "" || predecessor.CatalogSHA256 != "" || predecessor.SignatureSHA256 != "" {
			return CatalogPredecessor{}, errors.New("catalog predecessor metadata is invalid")
		}
		return predecessor, nil
	}
	if !strings.HasPrefix(predecessor.Tag, "v") || !stableVersionPattern.MatchString(strings.TrimPrefix(predecessor.Tag, "v")) ||
		!lowerSHA256Pattern.MatchString(predecessor.CatalogSHA256) || !lowerSHA256Pattern.MatchString(predecessor.SignatureSHA256) {
		return CatalogPredecessor{}, errors.New("catalog predecessor metadata is invalid")
	}
	return predecessor, nil
}

// ModuleVersion is one external module/version pair reachable from an exact
// component build configuration.
type ModuleVersion struct {
	Module  string
	Version string
}

func DecodeDependencyInventory(data []byte) (DependencyInventory, error) {
	inventory := DependencyInventory{}
	if err := strictJSON(data, &inventory); err != nil {
		return DependencyInventory{}, errors.New("dependency license inventory is invalid")
	}
	if err := validateInventory(inventory); err != nil {
		return DependencyInventory{}, err
	}
	if len(inventory.Modules) == 0 {
		return DependencyInventory{}, errors.New("dependency license inventory is invalid")
	}
	return inventory, nil
}

// SelectDependencies requires an exact reviewed inventory entry for every
// external module/version reachable from a component and returns only that
// component's stable closure.
func SelectDependencies(inventory DependencyInventory, reachable []ModuleVersion) (DependencyInventory, error) {
	if err := validateInventory(inventory); err != nil || len(inventory.Modules) == 0 {
		return DependencyInventory{}, errors.New("component dependency closure is invalid")
	}
	reviewed := make(map[string]Dependency, len(inventory.Modules))
	for _, dependency := range inventory.Modules {
		reviewed[dependency.Module] = dependency
	}
	selected := make(map[string]Dependency, len(reachable))
	for _, module := range reachable {
		if !modulePattern.MatchString(module.Module) || strings.TrimSpace(module.Version) == "" || strings.TrimSpace(module.Version) != module.Version {
			return DependencyInventory{}, errors.New("component dependency closure is invalid")
		}
		dependency, exists := reviewed[module.Module]
		if !exists || dependency.Version != module.Version {
			return DependencyInventory{}, fmt.Errorf("component dependency %s@%s has no exact reviewed license entry", module.Module, module.Version)
		}
		if previous, duplicate := selected[module.Module]; duplicate && previous.Version != dependency.Version {
			return DependencyInventory{}, errors.New("component dependency closure contains multiple versions")
		}
		selected[module.Module] = dependency
	}
	modules := make([]Dependency, 0, len(selected))
	for _, dependency := range selected {
		modules = append(modules, dependency)
	}
	slices.SortFunc(modules, func(left, right Dependency) int { return cmp.Compare(left.Module, right.Module) })
	return DependencyInventory{SchemaVersion: 1, Modules: modules}, nil
}

type componentContract struct {
	kind      string
	packageID string
	binary    string
	tagPrefix string
}

var componentContracts = releaseComponentContracts()

func releaseComponentContracts() map[string]componentContract {
	contracts := map[string]componentContract{
		"bsbctl": {kind: "core", packageID: "./cmd/bsbctl", binary: "bsbctl", tagPrefix: "v"},
	}
	for _, descriptor := range firstpartyplugins.All() {
		contracts[descriptor.ID] = componentContract{
			kind: "plugin", packageID: descriptor.CommandPackage, binary: descriptor.Binary, tagPrefix: descriptor.TagPrefix,
		}
	}
	return contracts
}

func DecodePlan(data []byte) (Plan, error) {
	plan := Plan{}
	if err := strictJSON(data, &plan); err != nil {
		return Plan{}, errors.New("release version metadata is invalid")
	}
	if plan.SchemaVersion != 1 || len(plan.Components) != len(componentContracts) {
		return Plan{}, errors.New("release version metadata is invalid")
	}
	seen := make(map[string]struct{}, len(plan.Components))
	for _, component := range plan.Components {
		contract, exists := componentContracts[component.ID]
		if !exists || component.Kind != contract.kind || component.Package != contract.packageID || component.Binary != contract.binary || !binaryPattern.MatchString(component.Binary) || !stableVersionPattern.MatchString(component.Version) || component.Tag != contract.tagPrefix+component.Version {
			return Plan{}, fmt.Errorf("release component %q is invalid", component.ID)
		}
		if _, exists := seen[component.ID]; exists {
			return Plan{}, fmt.Errorf("release component %q is duplicated", component.ID)
		}
		seen[component.ID] = struct{}{}
	}
	return plan, nil
}

func WriteArchive(writer io.Writer, entries []ArchiveEntry, epoch time.Time) error {
	if writer == nil || epoch.IsZero() || len(entries) == 0 {
		return errors.New("release archive inputs are invalid")
	}
	canonical := make([]ArchiveEntry, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for index, entry := range entries {
		if !validArchiveName(entry.Name) || (entry.Mode != 0o644 && entry.Mode != 0o755) {
			return fmt.Errorf("release archive entry %q is invalid", entry.Name)
		}
		if _, exists := seen[entry.Name]; exists {
			return fmt.Errorf("release archive entry %q is duplicated", entry.Name)
		}
		seen[entry.Name] = struct{}{}
		canonical[index] = ArchiveEntry{Name: entry.Name, Mode: entry.Mode, Data: slices.Clone(entry.Data)}
	}
	slices.SortFunc(canonical, func(left, right ArchiveEntry) int { return cmp.Compare(left.Name, right.Name) })

	gzipWriter, err := gzip.NewWriterLevel(writer, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("create deterministic gzip writer: %w", err)
	}
	gzipWriter.Header.ModTime = epoch.UTC()
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range canonical {
		header := &tar.Header{
			Name: entry.Name, Mode: int64(entry.Mode), Size: int64(len(entry.Data)),
			ModTime: epoch.UTC(), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return fmt.Errorf("write release archive header: %w", err)
		}
		if _, err := tarWriter.Write(entry.Data); err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return fmt.Errorf("write release archive entry: %w", err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		_ = gzipWriter.Close()
		return fmt.Errorf("finish release tar stream: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return fmt.Errorf("finish release gzip stream: %w", err)
	}
	return nil
}

func validArchiveName(name string) bool {
	return name != "" && !strings.Contains(name, "\\") && !strings.HasSuffix(name, "/") && !strings.HasPrefix(name, "/") && path.Clean(name) == name && name != "." && !strings.HasPrefix(name, "../")
}

type cdxDocument struct {
	BOMFormat    string          `json:"bomFormat"`
	SpecVersion  string          `json:"specVersion"`
	Version      int             `json:"version"`
	Metadata     cdxMetadata     `json:"metadata"`
	Components   []cdxComponent  `json:"components"`
	Dependencies []cdxDependency `json:"dependencies"`
}

type cdxMetadata struct {
	Component cdxComponent `json:"component"`
}

type cdxComponent struct {
	Type       string        `json:"type"`
	BOMRef     string        `json:"bom-ref"`
	Name       string        `json:"name"`
	Version    string        `json:"version"`
	PURL       string        `json:"purl"`
	Licenses   []cdxLicense  `json:"licenses,omitempty"`
	Properties []cdxProperty `json:"properties,omitempty"`
}

type cdxLicense struct {
	License cdxLicenseID `json:"license"`
}

type cdxLicenseID struct {
	ID string `json:"id"`
}

type cdxProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type cdxDependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn"`
}

func CycloneDX(component Component, inventory DependencyInventory) ([]byte, error) {
	if _, exists := componentContracts[component.ID]; !exists || !stableVersionPattern.MatchString(component.Version) {
		return nil, errors.New("CycloneDX release component is invalid")
	}
	if err := validateInventory(inventory); err != nil {
		return nil, err
	}
	rootRef := "pkg:golang/github.com/lxdb/bsbctl@" + component.Version
	root := cdxComponent{Type: "application", BOMRef: rootRef, Name: component.Binary, Version: component.Version, PURL: rootRef}
	modules := slices.Clone(inventory.Modules)
	slices.SortFunc(modules, func(left, right Dependency) int { return cmp.Compare(left.Module, right.Module) })
	components := make([]cdxComponent, 0, len(modules))
	dependsOn := make([]string, 0, len(modules))
	dependencies := make([]cdxDependency, 0, len(modules)+1)
	for _, module := range modules {
		ref := "pkg:golang/" + module.Module + "@" + module.Version
		properties := []cdxProperty{{Name: "bsbctl:license-file-sha256", Value: module.LicenseSHA256}}
		if module.ReleaseBlocked {
			properties = append(properties, cdxProperty{Name: "bsbctl:release-blocked", Value: "true"})
		}
		components = append(components, cdxComponent{
			Type: "library", BOMRef: ref, Name: module.Module, Version: module.Version, PURL: ref,
			Licenses: []cdxLicense{{License: cdxLicenseID{ID: module.License}}}, Properties: properties,
		})
		dependsOn = append(dependsOn, ref)
		dependencies = append(dependencies, cdxDependency{Ref: ref, DependsOn: []string{}})
	}
	dependencies = append([]cdxDependency{{Ref: rootRef, DependsOn: dependsOn}}, dependencies...)
	document := cdxDocument{
		BOMFormat: "CycloneDX", SpecVersion: "1.6", Version: 1,
		Metadata: cdxMetadata{Component: root}, Components: components, Dependencies: dependencies,
	}
	data, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode CycloneDX document: %w", err)
	}
	return append(data, '\n'), nil
}

func validateInventory(inventory DependencyInventory) error {
	if inventory.SchemaVersion != 1 {
		return errors.New("dependency license inventory is invalid")
	}
	seen := make(map[string]struct{}, len(inventory.Modules))
	for _, module := range inventory.Modules {
		if !modulePattern.MatchString(module.Module) || strings.TrimSpace(module.Version) == "" || strings.TrimSpace(module.Version) != module.Version || strings.TrimSpace(module.License) == "" || strings.TrimSpace(module.License) != module.License || !lowerSHA256Pattern.MatchString(module.LicenseSHA256) {
			return errors.New("dependency license entry is invalid")
		}
		if module.ReleaseBlocked != (module.ReleaseBlockerID != "") || (module.ReleaseBlockerID != "" && !releaseBlockerPattern.MatchString(module.ReleaseBlockerID)) {
			return errors.New("dependency release blocker identity is invalid")
		}
		if _, exists := seen[module.Module]; exists {
			return fmt.Errorf("dependency module %q is duplicated", module.Module)
		}
		seen[module.Module] = struct{}{}
	}
	return nil
}

func strictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}
