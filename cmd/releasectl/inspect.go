package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/internal/configschema"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
)

func runInspect(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("inspect")
	directory := flags.String("dir", "", "artifact directory")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *directory == "" {
		_, _ = fmt.Fprintln(stderr, "releasectl: invalid inspect arguments")
		return exitFailure
	}
	if err := verifyArtifactDirectory(*directory); err != nil {
		_, _ = fmt.Fprintln(stderr, "releasectl: artifact verification failed")
		return exitFailure
	}
	_, _ = fmt.Fprintln(stdout, "release artifacts: verified")
	return exitSuccess
}

func verifyArtifactDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("artifact directory is invalid")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != len(archiveComponentContracts)+2 {
		return errors.New("artifact directory contains unexpected files")
	}
	for _, entry := range entries {
		entryInfo, err := entry.Info()
		if err != nil || entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() || !entryInfo.Mode().IsRegular() {
			return errors.New("artifact directory contains a non-regular file")
		}
	}
	checksumData, err := readBoundedFile(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		return err
	}
	checksums, err := parseChecksums(checksumData)
	if err != nil {
		return err
	}
	manifestData, err := readBoundedFile(filepath.Join(directory, "release-manifest.json"))
	if err != nil {
		return err
	}
	manifest, err := decodePackageManifest(manifestData)
	if err != nil {
		return err
	}
	artifacts, err := validateInspectionManifest(manifest, checksums)
	if err != nil {
		return err
	}
	epoch := time.Unix(manifest.SourceDateEpoch, 0).UTC()
	for _, entry := range entries {
		if entry.Name() == "SHA256SUMS" {
			if info, err := entry.Info(); err != nil || info.Mode().Perm() != 0o644 {
				return errors.New("artifact file metadata is not canonical")
			}
			continue
		}
		expected, exists := checksums[entry.Name()]
		if !exists {
			return errors.New("artifact file is unexpected")
		}
		entryInfo, err := entry.Info()
		if err != nil || entryInfo.Mode().Perm() != 0o644 {
			return errors.New("artifact file metadata is not canonical")
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != expected {
			return errors.New("artifact checksum mismatch")
		}
		if strings.HasSuffix(entry.Name(), ".tar.gz") {
			artifact, exists := artifacts[entry.Name()]
			if !exists || artifact.SHA256 != expected || artifact.Size != int64(len(data)) {
				return errors.New("release manifest artifact does not match archive")
			}
			contract := archiveComponentContracts[artifact.Component]
			if err := inspectArchive(data, expectedArchiveMembers(contract), epoch); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateInspectionManifest(manifest packageManifest, checksums map[string]string) (map[string]packagedArtifact, error) {
	if manifest.GOOS != "darwin" || (manifest.GOARCH != "arm64" && manifest.GOARCH != "amd64") || len(checksums) != len(archiveComponentContracts)+1 {
		return nil, errors.New("release manifest platform or checksum set is invalid")
	}
	artifacts := make(map[string]packagedArtifact, len(manifest.Artifacts))
	seenComponents := make(map[string]struct{}, len(manifest.Artifacts))
	expectedComponents := firstPartyReleaseComponentIDs()
	if len(manifest.Artifacts) != len(expectedComponents) {
		return nil, errors.New("release manifest component set is incomplete")
	}
	for index, artifact := range manifest.Artifacts {
		contract, exists := archiveComponentContracts[artifact.Component]
		if !exists || artifact.Component != expectedComponents[index] || artifact.Version == "" || strings.TrimSpace(artifact.Version) != artifact.Version || strings.ContainsAny(artifact.Version, "/\\") || artifact.Tag != contract.TagPrefix+artifact.Version {
			return nil, errors.New("release manifest component is invalid")
		}
		filename := fmt.Sprintf("%s_%s_%s_%s.tar.gz", contract.Binary, artifact.Version, manifest.GOOS, manifest.GOARCH)
		if artifact.Filename != filename || artifact.Size < 1 || len(artifact.SHA256) != sha256.Size*2 {
			return nil, errors.New("release manifest artifact is invalid")
		}
		decoded, err := hex.DecodeString(artifact.SHA256)
		if err != nil || hex.EncodeToString(decoded) != artifact.SHA256 || checksums[artifact.Filename] != artifact.SHA256 {
			return nil, errors.New("release manifest artifact checksum is invalid")
		}
		if _, exists := seenComponents[artifact.Component]; exists {
			return nil, errors.New("release manifest component is duplicated")
		}
		seenComponents[artifact.Component] = struct{}{}
		artifacts[artifact.Filename] = artifact
	}
	if len(seenComponents) != len(archiveComponentContracts) {
		return nil, errors.New("release manifest component set is incomplete")
	}
	if _, exists := checksums["release-manifest.json"]; !exists {
		return nil, errors.New("release manifest checksum is missing")
	}
	return artifacts, nil
}

func firstPartyReleaseComponentIDs() []string {
	components := []string{"bsbctl"}
	for _, descriptor := range firstpartyplugins.All() {
		components = append(components, descriptor.ID)
	}
	return components
}

func expectedArchiveMembers(contract archiveComponentContract) map[string]int64 {
	members := map[string]int64{
		contract.Binary:          0o755,
		contract.MetadataName:    0o644,
		"LICENSE":                0o644,
		"NOTICE":                 0o644,
		"THIRD_PARTY_NOTICES.md": 0o644,
		"sbom.cdx.json":          0o644,
	}
	for name := range reviewedLegalArtifacts {
		members[name] = 0o644
	}
	if contract.ConfigSchemaPath != "" {
		members[configschema.FileName] = 0o644
	}
	for _, declaration := range contract.Assets {
		members[declaration.Source] = 0o644
	}
	return members
}

func parseChecksums(data []byte) (map[string]string, error) {
	result := make(map[string]string)
	previous := ""
	scanner := bufio.NewScanner(bytesReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 67 || line[64:66] != "  " {
			return nil, errors.New("checksum line is invalid")
		}
		digest, name := line[:64], line[66:]
		if _, err := hex.DecodeString(digest); err != nil || filepath.Base(name) != name || name == "" || name <= previous {
			return nil, errors.New("checksum entry is invalid")
		}
		if _, exists := result[name]; exists {
			return nil, errors.New("checksum entry is duplicated")
		}
		result[name] = digest
		previous = name
	}
	if scanner.Err() != nil || len(result) == 0 || data[len(data)-1] != '\n' {
		return nil, errors.New("checksum document is invalid")
	}
	return result, nil
}

func bytesReader(data []byte) io.Reader { return strings.NewReader(string(data)) }

func inspectArchive(data []byte, expected map[string]int64, epoch time.Time) error {
	reader, err := gzip.NewReader(bytesReader(data))
	if err != nil || !reader.ModTime.Equal(epoch) || reader.Name != "" || reader.Comment != "" || len(reader.Extra) != 0 || reader.OS != 255 {
		return errors.New("archive gzip header is invalid")
	}
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	slices.Sort(names)
	tarReader := tar.NewReader(reader)
	count := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if count >= len(names) || header.Name != names[count] || header.Typeflag != tar.TypeReg || filepath.Clean(header.Name) != header.Name || filepath.IsAbs(header.Name) || !header.ModTime.Equal(epoch) || header.AccessTime.IsZero() == false || header.ChangeTime.IsZero() == false || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" || header.Linkname != "" || header.Devmajor != 0 || header.Devminor != 0 || header.Format != tar.FormatUSTAR || header.Mode != expected[header.Name] || len(header.PAXRecords) != 0 || len(header.Xattrs) != 0 {
			return errors.New("archive entry header is invalid")
		}
		destination := io.Writer(io.Discard)
		digest := sha256.New()
		legalArtifact, verifyLegalArtifact := reviewedLegalArtifacts[header.Name]
		if verifyLegalArtifact {
			destination = digest
		}
		if _, err := io.Copy(destination, tarReader); err != nil {
			return err
		}
		if verifyLegalArtifact && (header.Size != legalArtifact.Size || hex.EncodeToString(digest.Sum(nil)) != legalArtifact.SHA256) {
			return errors.New("archive legal artifact is invalid")
		}
		count++
	}
	if count != len(names) {
		return errors.New("archive entry set is invalid")
	}
	return reader.Close()
}
