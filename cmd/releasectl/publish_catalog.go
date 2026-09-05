package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/releaseartifact"
)

func verifyCatalogPublicationSequence(ctx context.Context, root, catalogPath, signaturePath string, specs []releaseSpec, remote releaseRemote, now time.Time) error {
	if remote == nil || root == "" || catalogPath == "" || signaturePath == "" || now.IsZero() {
		return errors.New("catalog publication sequence inputs are invalid")
	}
	predecessorData, err := readBoundedFile(filepath.Join(root, "release", "catalog-predecessor.json"))
	if err != nil {
		return err
	}
	predecessor, err := releaseartifact.DecodeCatalogPredecessor(predecessorData)
	if err != nil {
		return err
	}
	currentCatalogData, err := readBoundedFile(catalogPath)
	if err != nil {
		return err
	}
	currentSignatureData, err := readBoundedFile(signaturePath)
	if err != nil {
		return err
	}
	currentTag, err := bindPublicationCatalogAssets(specs, catalogPath, signaturePath, currentCatalogData, currentSignatureData)
	if err != nil {
		return err
	}
	keyring, err := catalogKeyringLoader()
	if err != nil {
		return err
	}
	verificationTime := now.UTC()
	if predecessor.FirstRelease {
		verified, err := catalog.Verify(currentCatalogData, currentSignatureData, keyring, 0, verificationTime)
		if err != nil || verified.Sequence != 1 {
			return errors.New("first stable catalog must have sequence one")
		}
		return nil
	}
	if predecessor.Tag == currentTag {
		return errors.New("catalog predecessor tag matches the current release")
	}
	priorRelease, exists, err := remote.Get(ctx, predecessor.Tag)
	if err != nil || !exists || priorRelease.Draft || priorRelease.Tag != predecessor.Tag {
		return errors.New("catalog predecessor release is unavailable")
	}
	if err := validatePredecessorAssetMetadata(priorRelease.Assets, predecessor); err != nil {
		return err
	}
	priorCatalogData, err := remote.DownloadAsset(ctx, predecessor.Tag, "catalog.json")
	if err != nil {
		return err
	}
	priorSignatureData, err := remote.DownloadAsset(ctx, predecessor.Tag, "catalog.sig")
	if err != nil {
		return err
	}
	if sha256Hex(priorCatalogData) != predecessor.CatalogSHA256 || sha256Hex(priorSignatureData) != predecessor.SignatureSHA256 {
		return errors.New("catalog predecessor bytes do not match tracked digests")
	}
	priorCatalog, err := catalog.Verify(priorCatalogData, priorSignatureData, keyring, 0, verificationTime)
	if err != nil {
		return errors.New("catalog predecessor signature is invalid")
	}
	if _, err := catalog.Verify(currentCatalogData, currentSignatureData, keyring, priorCatalog.Sequence, verificationTime); err != nil {
		return errors.New("current catalog does not advance the authenticated predecessor")
	}
	return nil
}

func bindPublicationCatalogAssets(specs []releaseSpec, catalogPath, signaturePath string, catalogData, signatureData []byte) (string, error) {
	expected := map[string]releaseAsset{
		"catalog.json": {Name: "catalog.json", Path: catalogPath, Size: int64(len(catalogData)), SHA256: sha256Hex(catalogData)},
		"catalog.sig":  {Name: "catalog.sig", Path: signaturePath, Size: int64(len(signatureData)), SHA256: sha256Hex(signatureData)},
	}
	var tag string
	for _, spec := range specs {
		matched := 0
		for _, asset := range spec.Assets {
			want, exists := expected[asset.Name]
			if !exists {
				continue
			}
			if asset != want {
				return "", errors.New("publication catalog bytes do not match inspected release assets")
			}
			matched++
		}
		if matched == len(expected) {
			if tag != "" || !strings.HasPrefix(spec.Tag, "v") {
				return "", errors.New("publication catalog release is ambiguous")
			}
			tag = spec.Tag
		} else if matched != 0 {
			return "", errors.New("publication catalog asset set is incomplete")
		}
	}
	if tag == "" {
		return "", errors.New("publication catalog release is unavailable")
	}
	return tag, nil
}

func validatePredecessorAssetMetadata(assets []remoteReleaseAsset, predecessor releaseartifact.CatalogPredecessor) error {
	expected := map[string]string{"catalog.json": predecessor.CatalogSHA256, "catalog.sig": predecessor.SignatureSHA256}
	seen := make(map[string]struct{}, len(expected))
	for _, asset := range assets {
		digest, relevant := expected[asset.Name]
		if !relevant {
			continue
		}
		if _, duplicate := seen[asset.Name]; duplicate || asset.Size < 1 || asset.Digest != "sha256:"+digest {
			return errors.New("catalog predecessor asset metadata is invalid")
		}
		seen[asset.Name] = struct{}{}
	}
	if len(seen) != len(expected) {
		return errors.New("catalog predecessor asset set is incomplete")
	}
	return nil
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func decodeRemoteRelease(data []byte) (remoteRelease, error) {
	var release remoteRelease
	if err := json.Unmarshal(data, &release); err != nil {
		return remoteRelease{}, errors.New("GitHub release response is invalid")
	}
	return release, nil
}

func buildReleaseSpecs(root string, artifactDirectories []string, catalogPath, signaturePath string, now time.Time) ([]releaseSpec, error) {
	if len(artifactDirectories) != 2 || filepath.Base(catalogPath) != "catalog.json" || filepath.Base(signaturePath) != "catalog.sig" || now.IsZero() {
		return nil, errors.New("release publication inputs are invalid")
	}
	planData, err := readBoundedFile(filepath.Join(root, "release", "versions.json"))
	if err != nil {
		return nil, err
	}
	plan, err := releaseartifact.DecodePlan(planData)
	if err != nil {
		return nil, err
	}
	catalogData, err := readBoundedFile(catalogPath)
	if err != nil {
		return nil, err
	}
	signatureData, err := readBoundedFile(signaturePath)
	if err != nil {
		return nil, err
	}
	keyring, err := catalogKeyringLoader()
	if err != nil {
		return nil, err
	}
	verifiedCatalog, err := catalog.Verify(catalogData, signatureData, keyring, 0, now.UTC())
	if err != nil {
		return nil, err
	}

	components := make(map[string]releaseartifact.Component, len(plan.Components))
	assetsByComponent := make(map[string][]releaseAsset, len(plan.Components))
	for _, component := range plan.Components {
		components[component.ID] = component
	}
	seenArchitectures := make(map[string]struct{}, len(artifactDirectories))
	checksumAssets := make([]releaseAsset, 0, len(artifactDirectories))
	pluginCatalogEntries := 0
	for _, directory := range artifactDirectories {
		if err := verifyArtifactDirectory(directory); err != nil {
			return nil, err
		}
		manifestData, err := readBoundedFile(filepath.Join(directory, "release-manifest.json"))
		if err != nil {
			return nil, err
		}
		manifest, err := decodePackageManifest(manifestData)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seenArchitectures[manifest.GOARCH]; duplicate {
			return nil, errors.New("release publication repeats an architecture")
		}
		seenArchitectures[manifest.GOARCH] = struct{}{}
		checksumAsset, err := releaseAssetFromFile(filepath.Join(directory, "SHA256SUMS"), "SHA256SUMS-darwin-"+manifest.GOARCH)
		if err != nil {
			return nil, err
		}
		checksumAssets = append(checksumAssets, checksumAsset)
		for _, artifact := range manifest.Artifacts {
			component, exists := components[artifact.Component]
			if !exists || artifact.Version != component.Version || artifact.Tag != component.Tag {
				return nil, errors.New("release publication manifest does not match the release plan")
			}
			path := filepath.Join(directory, artifact.Filename)
			asset, err := releaseAssetFromFile(path, artifact.Filename)
			if err != nil || asset.Size != artifact.Size || asset.SHA256 != artifact.SHA256 {
				return nil, errors.New("release publication asset does not match the inspected manifest")
			}
			assetsByComponent[component.ID] = append(assetsByComponent[component.ID], asset)
			if component.Kind == "plugin" {
				entry, err := verifiedCatalog.Resolve(component.ID, component.Version, manifest.GOOS, manifest.GOARCH)
				if err != nil || entry.SHA256 != artifact.SHA256 || entry.CompressedSize != artifact.Size || entry.Executable != component.Binary {
					return nil, errors.New("signed catalog does not match the inspected plugin artifact")
				}
				pluginCatalogEntries++
			}
		}
	}
	if len(seenArchitectures) != 2 || pluginCatalogEntries != len(verifiedCatalog.Plugins) {
		return nil, errors.New("release publication platform set is incomplete")
	}
	catalogAsset, err := releaseAssetFromFile(catalogPath, "catalog.json")
	if err != nil {
		return nil, err
	}
	signatureAsset, err := releaseAssetFromFile(signaturePath, "catalog.sig")
	if err != nil {
		return nil, err
	}
	installAsset, err := releaseAssetFromFile(filepath.Join(root, "install.sh"), "install.sh")
	if err != nil {
		return nil, err
	}

	specs := make([]releaseSpec, 0, len(plan.Components))
	for _, component := range plan.Components {
		assets := append([]releaseAsset(nil), assetsByComponent[component.ID]...)
		if len(assets) != 2 {
			return nil, errors.New("release publication component platform set is incomplete")
		}
		if component.Kind == "core" {
			assets = append(assets, catalogAsset, signatureAsset, installAsset)
			assets = append(assets, checksumAssets...)
		}
		slices.SortFunc(assets, func(left, right releaseAsset) int { return cmp.Compare(left.Name, right.Name) })
		title, err := releaseTitle(component)
		if err != nil {
			return nil, err
		}
		specs = append(specs, releaseSpec{Tag: component.Tag, Title: title, Assets: assets})
	}
	slices.SortFunc(specs, func(left, right releaseSpec) int {
		leftCore := left.Tag[0] == 'v'
		rightCore := right.Tag[0] == 'v'
		if leftCore != rightCore {
			if !leftCore {
				return -1
			}
			return 1
		}
		return cmp.Compare(left.Tag, right.Tag)
	})
	return specs, nil
}

func releaseAssetFromFile(path, name string) (releaseAsset, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 {
		return releaseAsset{}, errors.New("release publication asset is not a nonempty regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return releaseAsset{}, err
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return releaseAsset{}, errors.Join(copyErr, closeErr)
	}
	return releaseAsset{Name: name, Path: path, Size: info.Size(), SHA256: hex.EncodeToString(digest.Sum(nil))}, nil
}

func releaseTitle(component releaseartifact.Component) (string, error) {
	if component.ID == "bsbctl" {
		return "bsbctl v" + component.Version, nil
	}
	descriptor, ok := firstpartyplugins.LookupID(component.ID)
	if !ok {
		return "", errors.New("release publication component is unsupported")
	}
	return descriptor.ReleaseTitle + " v" + component.Version, nil
}
