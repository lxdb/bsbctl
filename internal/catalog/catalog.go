// Package catalog verifies the signed metadata and content digests used for
// independently released plugin executables.
package catalog

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/configschema"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	maxCatalogBytes  = 1 << 20
	maxEnvelopeBytes = 4 << 10
	MaxArtifactBytes = 128 << 20
)

var pluginIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,127}$`)
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
var executablePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Keyring map[string]ed25519.PublicKey

type Catalog struct {
	Version     int       `json:"version"`
	Channel     string    `json:"channel"`
	Sequence    uint64    `json:"sequence"`
	GeneratedAt time.Time `json:"generated_at"`
	Plugins     []Entry   `json:"plugins"`
}

type Entry struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	URL            string `json:"url"`
	SHA256         string `json:"sha256"`
	CompressedSize int64  `json:"compressed_size"`
	ArchiveFormat  string `json:"archive_format"`
	Executable     string `json:"executable"`
	Manifest       string `json:"manifest"`
}

type signatureDocument struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Signature string `json:"signature"`
}

// PackageManifest is transitively authenticated by the signed catalog's
// artifact digest and supplies runtime plugin and asset declarations.
type PackageManifest struct {
	ID               string                         `json:"id"`
	Version          string                         `json:"version"`
	ProtocolVersion  string                         `json:"protocol_version"`
	Executable       string                         `json:"executable"`
	ExecutableSHA256 string                         `json:"executable_sha256"`
	ExecutableSize   int64                          `json:"executable_size"`
	ExecutionModes   []protocol.ExecutionMode       `json:"execution_modes"`
	Channels         []protocol.Channel             `json:"channels"`
	Operations       []protocol.OperationDescriptor `json:"operations,omitempty"`
	ConfigSchema     *configschema.Declaration      `json:"config_schema,omitempty"`
	Assets           []assets.Declaration           `json:"assets"`
}

type ConfigSchemaDeclaration = configschema.Declaration

//go:embed package_manifest.schema.json
var packageManifestSchemaJSON []byte

var packageManifestSchemaOnce sync.Once
var packageManifestSchema *jsonschema.Schema
var packageManifestSchemaErr error

// VerifyPackageManifest strictly binds extracted declarations to the catalog entry.
func VerifyPackageManifest(data []byte, entry Entry, root string) (PackageManifest, error) {
	if len(data) == 0 || len(data) > maxCatalogBytes {
		return PackageManifest{}, errors.New("plugin manifest must be between 1 byte and 1 MiB")
	}
	if err := validatePackageManifestSchema(data); err != nil {
		return PackageManifest{}, errors.New("plugin manifest does not match the package schema")
	}
	var manifest PackageManifest
	if err := strictJSON(data, &manifest); err != nil {
		return PackageManifest{}, errors.New("plugin manifest JSON is invalid")
	}
	if manifest.ID != entry.ID || manifest.Version != entry.Version || manifest.Executable != entry.Executable {
		return PackageManifest{}, errors.New("plugin manifest identity does not match catalog entry")
	}
	if manifest.ProtocolVersion != protocol.Version {
		return PackageManifest{}, fmt.Errorf("plugin manifest requires protocol_version %q", protocol.Version)
	}
	if len(manifest.ExecutableSHA256) != sha256.Size*2 || !lowerHex(manifest.ExecutableSHA256) {
		return PackageManifest{}, errors.New("plugin manifest executable SHA-256 is invalid")
	}
	if manifest.ExecutableSize < 1 || manifest.ExecutableSize > MaxArtifactBytes {
		return PackageManifest{}, errors.New("plugin manifest executable size is invalid")
	}
	if len(manifest.ExecutionModes) == 0 || len(manifest.Channels) == 0 {
		return PackageManifest{}, errors.New("plugin manifest requires execution_modes and channels")
	}
	if manifest.ConfigSchema != nil {
		if err := manifest.ConfigSchema.Validate(); err != nil {
			return PackageManifest{}, err
		}
	}
	channels := make(map[string]struct{}, len(manifest.Channels))
	for _, channel := range manifest.Channels {
		if strings.TrimSpace(channel.ID) == "" {
			return PackageManifest{}, errors.New("plugin manifest channel id is required")
		}
		if _, exists := channels[channel.ID]; exists {
			return PackageManifest{}, fmt.Errorf("plugin manifest channel %q is duplicated", channel.ID)
		}
		channels[channel.ID] = struct{}{}
	}
	operations := make(map[string]struct{}, len(manifest.Operations))
	for _, descriptor := range manifest.Operations {
		if err := descriptor.Validate(); err != nil {
			return PackageManifest{}, err
		}
		if _, exists := operations[descriptor.ID]; exists {
			return PackageManifest{}, fmt.Errorf("plugin manifest operation %q is duplicated", descriptor.ID)
		}
		operations[descriptor.ID] = struct{}{}
	}
	if err := assets.ValidatePackage(assets.Package{PluginID: manifest.ID, Version: manifest.Version, Root: root, Enabled: true, Assets: manifest.Assets}); err != nil {
		return PackageManifest{}, fmt.Errorf("plugin manifest assets: %w", err)
	}
	return manifest, nil
}

func validatePackageManifestSchema(data []byte) error {
	packageManifestSchemaOnce.Do(func() {
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(packageManifestSchemaJSON))
		if err != nil {
			packageManifestSchemaErr = err
			return
		}
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		if err := compiler.AddResource("package-manifest.json", document); err != nil {
			packageManifestSchemaErr = err
			return
		}
		packageManifestSchema, packageManifestSchemaErr = compiler.Compile("package-manifest.json")
	})
	if packageManifestSchemaErr != nil {
		return packageManifestSchemaErr
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}
	return packageManifestSchema.Validate(instance)
}

func Verify(data, envelopeData []byte, keyring Keyring, lastSequence uint64, now time.Time) (Catalog, error) {
	if len(data) == 0 || len(data) > maxCatalogBytes {
		return Catalog{}, errors.New("plugin catalog exceeds 1 MiB")
	}
	if len(envelopeData) == 0 || len(envelopeData) > maxEnvelopeBytes {
		return Catalog{}, errors.New("plugin catalog signature envelope is invalid")
	}
	var envelope signatureDocument
	if err := strictJSON(envelopeData, &envelope); err != nil {
		return Catalog{}, errors.New("plugin catalog signature envelope is invalid")
	}
	if envelope.Algorithm != "ed25519" || envelope.KeyID == "" {
		return Catalog{}, errors.New("plugin catalog signature envelope is invalid")
	}
	publicKey, ok := keyring[envelope.KeyID]
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return Catalog{}, errors.New("plugin catalog signing key is unknown")
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(signature) != envelope.Signature {
		return Catalog{}, errors.New("plugin catalog signature envelope is invalid")
	}
	if !ed25519.Verify(publicKey, data, signature) {
		return Catalog{}, errors.New("plugin catalog signature is invalid")
	}
	var catalog Catalog
	if err := strictJSON(data, &catalog); err != nil {
		return Catalog{}, errors.New("plugin catalog metadata is invalid")
	}
	if catalog.Version != 1 || catalog.Channel != "stable" || catalog.Sequence == 0 {
		return Catalog{}, errors.New("plugin catalog metadata is invalid")
	}
	if catalog.Sequence <= lastSequence {
		return Catalog{}, errors.New("plugin catalog sequence is stale")
	}
	if now.IsZero() || catalog.GeneratedAt.IsZero() || catalog.GeneratedAt.After(now) {
		return Catalog{}, errors.New("plugin catalog generation time is invalid")
	}
	if len(catalog.Plugins) == 0 {
		return Catalog{}, errors.New("plugin catalog has no platform entries")
	}
	seen := make(map[string]struct{}, len(catalog.Plugins))
	pluginIDs := make([]string, 0, len(catalog.Plugins))
	seenPluginIDs := make(map[string]struct{}, len(catalog.Plugins))
	for _, entry := range catalog.Plugins {
		key := strings.Join([]string{entry.ID, entry.Version, entry.OS, entry.Arch}, "\x00")
		if _, exists := seen[key]; exists {
			return Catalog{}, errors.New("plugin catalog contains a duplicate entry")
		}
		seen[key] = struct{}{}
		if !pluginIDPattern.MatchString(entry.ID) || entry.ID == "." || entry.ID == ".." || !versionPattern.MatchString(entry.Version) || entry.Version == "." || entry.Version == ".." {
			return Catalog{}, errors.New("plugin catalog entry identity is invalid")
		}
		if entry.OS != "darwin" || (entry.Arch != "arm64" && entry.Arch != "amd64") {
			return Catalog{}, errors.New("plugin catalog entry platform is unsupported")
		}
		parsed, err := url.Parse(entry.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" || parsed.Fragment != "" {
			return Catalog{}, errors.New("plugin catalog entry URL is invalid")
		}
		if len(entry.SHA256) != sha256.Size*2 || !lowerHex(entry.SHA256) {
			return Catalog{}, errors.New("plugin catalog entry digest is invalid")
		}
		if entry.CompressedSize < 1 || entry.CompressedSize > MaxArtifactBytes || entry.ArchiveFormat != "tar.gz" || entry.Manifest != "manifest.json" {
			return Catalog{}, errors.New("plugin catalog entry archive metadata is invalid")
		}
		if !executablePattern.MatchString(entry.Executable) || filepath.Base(entry.Executable) != entry.Executable || entry.Executable == "." || entry.Executable == ".." {
			return Catalog{}, errors.New("plugin catalog entry executable is invalid")
		}
		if _, exists := seenPluginIDs[entry.ID]; !exists {
			seenPluginIDs[entry.ID] = struct{}{}
			pluginIDs = append(pluginIDs, entry.ID)
		}
	}
	if err := assets.ValidatePluginHashCollisions(pluginIDs); err != nil {
		return Catalog{}, fmt.Errorf("plugin catalog asset path prefixes: %w", err)
	}
	return catalog, nil
}

func (c Catalog) Resolve(id, version, goos, goarch string) (Entry, error) {
	for _, entry := range c.Plugins {
		if entry.ID == id && entry.Version == version && entry.OS == goos && entry.Arch == goarch {
			return entry, nil
		}
	}
	return Entry{}, errors.New("plugin release is not present in the verified catalog")
}

func strictJSON(data []byte, destination any) error {
	return protocol.DecodeStrict(data, destination)
}

func lowerHex(value string) bool {
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}
