package installer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/lxdb/bsbctl/internal/catalog"
)

func ExtractAndVerify(artifactPath, stagingParent string, entry catalog.Entry) (VerifiedPackage, error) {
	artifact, err := os.Open(artifactPath)
	if err != nil {
		return VerifiedPackage{}, errorCode(CodePackageInvalid)
	}
	defer artifact.Close()
	return ExtractAndVerifyFile(artifact, stagingParent, entry)
}

func extractArchiveFile(artifact *os.File, staging string) (map[string]FileRecord, error) {
	root, err := openExtractionRoot(staging)
	if err != nil {
		return nil, err
	}
	defer root.close()
	return extractArchiveRoot(artifact, root)
}

func TestExtractAndVerifyAuthenticatesCompletePackage(t *testing.T) {
	executable := []byte("executable")
	asset := []byte("animation")
	manifest := packageManifest(executable, asset)
	artifact := tarGzip(t,
		tarItem{name: "manifest.json", body: manifest, mode: 0o777},
		tarItem{name: "bsbctl-plugin-ball8", body: executable, mode: 0o666},
		tarItem{name: "animations/", kind: tar.TypeDir, mode: 0o777},
		tarItem{name: "animations/shake.anim", body: asset, mode: 0o777},
	)
	artifactPath, entry := writeArtifact(t, artifact)
	parent := t.TempDir()

	verified, err := ExtractAndVerify(artifactPath, parent, entry)
	if err != nil {
		t.Fatalf("ExtractAndVerify: %v", err)
	}
	defer verified.Close()
	if filepath.Dir(verified.Root) != parent || verified.Manifest.ID != entry.ID || len(verified.Files) != 3 {
		t.Fatalf("verified package = %#v", verified)
	}
	assertMode(t, verified.Root, 0o700)
	assertMode(t, filepath.Join(verified.Root, "manifest.json"), 0o600)
	assertMode(t, filepath.Join(verified.Root, "animations"), 0o700)
	assertMode(t, filepath.Join(verified.Root, "animations", "shake.anim"), 0o600)
	assertMode(t, filepath.Join(verified.Root, "bsbctl-plugin-ball8"), 0o700)
}

func TestExtractAndVerifyRejectsArchiveAttacks(t *testing.T) {
	tests := []struct {
		name  string
		items []tarItem
	}{
		{name: "absolute", items: []tarItem{{name: "/absolute", body: []byte("x")}}},
		{name: "traversal", items: []tarItem{{name: "../escape", body: []byte("x")}}},
		{name: "embedded traversal", items: []tarItem{{name: "dir/../escape", body: []byte("x")}}},
		{name: "backslash", items: []tarItem{{name: `dir\escape`, body: []byte("x")}}},
		{name: "control", items: []tarItem{{name: "dir/line\nbreak", body: []byte("x")}}},
		{name: "invalid utf8", items: []tarItem{{name: string([]byte{0xff}), body: []byte("x")}}},
		{name: "path too long", items: []tarItem{{name: strings.Repeat("a", 241), body: []byte("x")}}},
		{name: "duplicate", items: []tarItem{{name: "same", body: []byte("x")}, {name: "same", body: []byte("x")}}},
		{name: "case fold collision", items: []tarItem{{name: "Asset", body: []byte("x")}, {name: "asset", body: []byte("x")}}},
		{name: "symlink", items: []tarItem{{name: "link", kind: tar.TypeSymlink, link: "target"}}},
		{name: "hardlink", items: []tarItem{{name: "link", kind: tar.TypeLink, link: "target"}}},
		{name: "character device", items: []tarItem{{name: "device", kind: tar.TypeChar}}},
		{name: "block device", items: []tarItem{{name: "device", kind: tar.TypeBlock}}},
		{name: "fifo", items: []tarItem{{name: "pipe", kind: tar.TypeFifo}}},
		{name: "unknown type", items: []tarItem{{name: "unknown", kind: 'Z'}}},
		{name: "pax", items: []tarItem{{name: "pax", body: []byte("x"), format: tar.FormatPAX, pax: map[string]string{"VENDOR.value": "x"}}}},
		{name: "gnu", items: []tarItem{{name: "gnu", body: []byte("x"), format: tar.FormatGNU}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifactPath, entry := writeArtifact(t, tarGzip(t, test.items...))
			artifact, err := os.Open(artifactPath)
			if err != nil {
				t.Fatal(err)
			}
			defer artifact.Close()
			if _, err := extractArchiveFile(artifact, t.TempDir()); err == nil {
				t.Fatal("archive attack passed extraction; a missing manifest must not be the rejection oracle")
			}
			parent := t.TempDir()
			if _, err := ExtractAndVerify(artifactPath, parent, entry); CodeOf(err) != CodePackageInvalid {
				t.Fatalf("error = %v, code = %q", err, CodeOf(err))
			}
			assertEmptyDirectory(t, parent)
		})
	}
}

func TestExtractAndVerifyEnforcesEveryArchiveLimit(t *testing.T) {
	items := make([]tarItem, 513)
	for index := range items {
		items[index] = tarItem{name: fmt.Sprintf("d%03d/", index), kind: tar.TypeDir}
	}
	artifactPath, entry := writeArtifact(t, tarGzip(t, items...))
	artifactFile, err := os.Open(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer artifactFile.Close()
	if _, err := extractArchiveFile(artifactFile, t.TempDir()); err == nil {
		t.Fatal("513 entries passed extraction")
	}
	controlPath, _ := writeArtifact(t, tarGzip(t, items[:512]...))
	control, err := os.Open(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := extractArchiveFile(control, t.TempDir()); err != nil {
		t.Fatalf("512 supported directory entries rejected: %v", err)
	}
	if _, err := ExtractAndVerify(artifactPath, t.TempDir(), entry); CodeOf(err) != CodePackageInvalid {
		t.Fatalf("entry count error = %v", err)
	}

	if err := validateRegularFileLimit((128 << 20) + 1); err == nil {
		t.Fatal("per-file limit accepted 128 MiB plus one byte")
	}
	if err := validateTotalFileLimit((384<<20)-1, 2); err == nil {
		t.Fatal("total limit accepted 384 MiB plus one byte")
	}
}

func TestNormalizedArchivePathAppliesLimitAfterDirectoryNormalization(t *testing.T) {
	name := strings.Repeat("a", 240)
	if got, err := normalizedArchivePath(name+"/", true); err != nil || got != name {
		t.Fatalf("240-byte normalized directory = %q, %v", got, err)
	}
	if _, err := normalizedArchivePath(name+"a/", true); err == nil {
		t.Fatal("241-byte normalized directory was accepted")
	}
	if _, err := normalizedArchivePath("nul\x00path", false); err == nil {
		t.Fatal("NUL path was accepted")
	}
}

func TestArchivePathFoldCollidesUnicodeCaseEquivalents(t *testing.T) {
	capitalSigma := foldArchivePath("assets/\u03a3.anim")
	finalSigma := foldArchivePath("assets/\u03c2.anim")
	if capitalSigma != finalSigma {
		t.Fatalf("Unicode folds differ: %q != %q", capitalSigma, finalSigma)
	}
}

func TestExtractArchiveRejectsUnicodeFoldEquivalentAncestorSpellings(t *testing.T) {
	tests := []struct {
		name      string
		secondDir string
		wantError bool
	}{
		{name: "fold-equivalent ancestor", secondDir: "\u03c2", wantError: true},
		{name: "same-spelling ancestor reuse", secondDir: "\u03a3", wantError: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact := tarGzipWithRawNames(t,
				[]tarItem{{name: "assets/AA/a.anim", body: []byte("a")}, {name: "assets/BB/b.anim", body: []byte("b")}},
				map[string]string{"assets/AA/a.anim": "assets/\u03a3/a.anim", "assets/BB/b.anim": "assets/" + test.secondDir + "/b.anim"},
			)
			artifactPath, _ := writeArtifact(t, artifact)
			file, err := os.Open(artifactPath)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			_, err = extractArchiveFile(file, t.TempDir())
			if test.wantError && (err == nil || !strings.Contains(err.Error(), "case-fold-colliding")) {
				t.Fatalf("fold-equivalent ancestor error = %v", err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("same-spelling parent reuse: %v", err)
			}
		})
	}
}

func TestExtractAndVerifyRejectsTamperingAndIncompleteDeclarations(t *testing.T) {
	executable := []byte("executable")
	asset := []byte("animation")
	validManifest := packageManifest(executable, asset)
	tests := []struct {
		name  string
		items []tarItem
	}{
		{name: "missing root manifest", items: []tarItem{{name: "bsbctl-plugin-ball8", body: executable}}},
		{name: "tampered executable", items: []tarItem{{name: "manifest.json", body: validManifest}, {name: "bsbctl-plugin-ball8", body: []byte("tampered")}, {name: "animations/shake.anim", body: asset}}},
		{name: "tampered asset", items: []tarItem{{name: "manifest.json", body: validManifest}, {name: "bsbctl-plugin-ball8", body: executable}, {name: "animations/shake.anim", body: []byte("tampered")}}},
		{name: "missing asset", items: []tarItem{{name: "manifest.json", body: validManifest}, {name: "bsbctl-plugin-ball8", body: executable}}},
		{name: "undeclared regular file", items: []tarItem{{name: "manifest.json", body: validManifest}, {name: "bsbctl-plugin-ball8", body: executable}, {name: "animations/shake.anim", body: asset}, {name: "secret.txt", body: []byte("secret")}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifactPath, entry := writeArtifact(t, tarGzip(t, test.items...))
			parent := t.TempDir()
			if _, err := ExtractAndVerify(artifactPath, parent, entry); CodeOf(err) != CodePackageInvalid {
				t.Fatalf("error = %v, code = %q", err, CodeOf(err))
			}
			assertEmptyDirectory(t, parent)
		})
	}
}

func TestExtractAndVerifyAcceptsAuthenticatedConfigurationSchema(t *testing.T) {
	executable := []byte("executable")
	schema := []byte(`{"type":"object","additionalProperties":false}`)
	artifactPath, entry := writeArtifact(t, tarGzip(t,
		tarItem{name: "manifest.json", body: packageManifestWithConfigSchema(executable, schema)},
		tarItem{name: "bsbctl-plugin-ball8", body: executable},
		tarItem{name: "config.schema.json", body: schema},
	))

	verified, err := ExtractAndVerify(artifactPath, t.TempDir(), entry)
	if err != nil {
		t.Fatalf("ExtractAndVerify: %v", err)
	}
	defer verified.Close()
	if verified.Manifest.ConfigSchema == nil || verified.Manifest.ConfigSchema.SHA256 == "" {
		t.Fatalf("config schema = %#v", verified.Manifest.ConfigSchema)
	}
	if data, err := os.ReadFile(filepath.Join(verified.Root, "config.schema.json")); err != nil || !bytes.Equal(data, schema) {
		t.Fatalf("config schema data = %q, err = %v", data, err)
	}
}

func TestExtractAndVerifyRejectsInvalidConfigurationSchema(t *testing.T) {
	executable := []byte("executable")
	schema := []byte(`{"type":"not-a-json-schema-type"}`)
	artifactPath, entry := writeArtifact(t, tarGzip(t,
		tarItem{name: "manifest.json", body: packageManifestWithConfigSchema(executable, schema)},
		tarItem{name: "bsbctl-plugin-ball8", body: executable},
		tarItem{name: "config.schema.json", body: schema},
	))

	if _, err := ExtractAndVerify(artifactPath, t.TempDir(), entry); CodeOf(err) != CodePackageInvalid {
		t.Fatalf("error = %v, code = %q", err, CodeOf(err))
	}
}

func TestExtractAndVerifyRejectsArtifactBeforeExtraction(t *testing.T) {
	artifact := tarGzip(t, tarItem{name: "manifest.json", body: []byte("{}")})
	artifactPath, entry := writeArtifact(t, artifact)
	entry.SHA256 = strings.Repeat("0", 64)
	parent := t.TempDir()
	if _, err := ExtractAndVerify(artifactPath, parent, entry); CodeOf(err) != CodePackageInvalid {
		t.Fatalf("error = %v", err)
	}
	assertEmptyDirectory(t, parent)
}

func TestExtractAndVerifyUsesAuthenticatedOpenArtifactAfterPathReplacement(t *testing.T) {
	executable := []byte("authenticated executable")
	asset := []byte("authenticated animation")
	original := tarGzip(t,
		tarItem{name: "manifest.json", body: packageManifest(executable, asset)},
		tarItem{name: "bsbctl-plugin-ball8", body: executable},
		tarItem{name: "animations/shake.anim", body: asset},
	)
	artifactPath, entry := writeArtifact(t, original)
	artifact, err := os.Open(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()

	replacement := tarGzip(t, tarItem{name: "replacement", body: []byte("unauthenticated")})
	replacementPath := filepath.Join(filepath.Dir(artifactPath), "replacement.tar.gz")
	if err := os.WriteFile(replacementPath, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementPath, artifactPath); err != nil {
		t.Fatal(err)
	}

	verified, err := ExtractAndVerifyFile(artifact, t.TempDir(), entry)
	if err != nil {
		t.Fatalf("ExtractAndVerifyFile: %v", err)
	}
	defer verified.Close()
	data, err := os.ReadFile(filepath.Join(verified.Root, "bsbctl-plugin-ball8"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, executable) {
		t.Fatalf("extracted executable = %q", data)
	}
	if _, err := os.Stat(filepath.Join(verified.Root, "replacement")); !os.IsNotExist(err) {
		t.Fatalf("replacement pathname content was parsed: %v", err)
	}
}

func TestExtractionRootDoesNotFollowComponentReplacedBySymlink(t *testing.T) {
	staging := t.TempDir()
	outside := t.TempDir()
	root, err := openExtractionRoot(staging)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	if err := root.mkdirAll("animations"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(staging, "animations"), filepath.Join(staging, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(staging, "animations")); err != nil {
		t.Fatal(err)
	}

	file, err := root.createFile("animations/payload.anim", 0o600)
	if err == nil {
		file.Close()
		t.Fatal("createFile followed a replaced symlink component")
	}
	if _, err := os.Stat(filepath.Join(outside, "payload.anim")); !os.IsNotExist(err) {
		t.Fatalf("outside payload exists: %v", err)
	}
}

func TestVerificationUsesOwnedRootAfterStagingPathReplacement(t *testing.T) {
	executable := []byte("authenticated executable")
	asset := []byte("authenticated animation")
	artifactPath, entry := writeArtifact(t, tarGzip(t,
		tarItem{name: "manifest.json", body: packageManifest(executable, asset)},
		tarItem{name: "bsbctl-plugin-ball8", body: executable},
		tarItem{name: "animations/shake.anim", body: asset},
	))
	artifact, err := os.Open(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	stagingParent := t.TempDir()
	staging, err := os.MkdirTemp(stagingParent, ".bsbctl-stage-*")
	if err != nil {
		t.Fatal(err)
	}
	records, err := extractArchiveFile(artifact, staging)
	if err != nil {
		t.Fatal(err)
	}
	root, err := openExtractionRoot(staging)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	originalPath := staging + "-original"
	if err := os.Rename(staging, originalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	verified, err := verifyExtractedPackageRoot(root, staging, entry, records)
	if err != nil {
		t.Fatalf("verify descriptor-backed root: %v", err)
	}
	expectedDigest := sha256.Sum256(executable)
	if verified.Manifest.ExecutableSHA256 != hex.EncodeToString(expectedDigest[:]) {
		t.Fatalf("verified replacement rather than owned root: %#v", verified.Manifest)
	}
}

type tarItem struct {
	name   string
	body   []byte
	kind   byte
	link   string
	mode   int64
	format tar.Format
	pax    map[string]string
}

func tarGzip(t *testing.T, items ...tarItem) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, item := range items {
		kind := item.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		mode := item.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{Name: item.name, Typeflag: kind, Linkname: item.link, Mode: mode, Size: int64(len(item.body)), Format: item.format, PAXRecords: item.pax}
		if kind != tar.TypeReg && kind != tar.TypeRegA {
			header.Size = 0
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("WriteHeader(%q): %v", item.name, err)
		}
		if len(item.body) > 0 {
			if _, err := tarWriter.Write(item.body); err != nil {
				t.Fatalf("Write(%q): %v", item.name, err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func tarGzipWithRawNames(t *testing.T, items []tarItem, replacements map[string]string) []byte {
	t.Helper()
	compressed := tarGzip(t, items...)
	gzipReader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(gzipReader)
	if err != nil {
		t.Fatal(err)
	}
	if err := gzipReader.Close(); err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset+512 <= len(raw); {
		header := raw[offset : offset+512]
		if len(bytes.Trim(header, "\x00")) == 0 {
			break
		}
		name := string(bytes.TrimRight(header[:100], "\x00"))
		if replacement, exists := replacements[name]; exists {
			if len(replacement) != len(name) {
				t.Fatalf("raw tar replacement length %d != %d", len(replacement), len(name))
			}
			copy(header[:100], replacement)
			for index := 148; index < 156; index++ {
				header[index] = ' '
			}
			var checksum int64
			for _, value := range header {
				checksum += int64(value)
			}
			copy(header[148:156], fmt.Sprintf("%06o\x00 ", checksum))
		}
		size, err := strconv.ParseInt(strings.Trim(string(header[124:136]), " \x00"), 8, 64)
		if err != nil {
			t.Fatal(err)
		}
		offset += 512 + int((size+511)/512)*512
	}
	var result bytes.Buffer
	gzipWriter := gzip.NewWriter(&result)
	if _, err := gzipWriter.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func writeArtifact(t *testing.T, artifact []byte) (string, catalog.Entry) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(path, artifact, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(artifact)
	entry := catalog.Entry{ID: "dev.bsbctl.ball8", Version: "1.0.0", OS: "darwin", Arch: "arm64", URL: "https://example.invalid/archive.tar.gz", SHA256: hex.EncodeToString(digest[:]), CompressedSize: int64(len(artifact)), ArchiveFormat: "tar.gz", Executable: "bsbctl-plugin-ball8", Manifest: "manifest.json"}
	return path, entry
}

func packageManifest(executable, asset []byte) []byte {
	executableDigest := sha256.Sum256(executable)
	assetDigest := sha256.Sum256(asset)
	return []byte(`{"id":"dev.bsbctl.ball8","version":"1.0.0","protocol_version":"1.0","executable":"bsbctl-plugin-ball8","executable_sha256":"` + hex.EncodeToString(executableDigest[:]) + `","executable_size":` + fmt.Sprint(len(executable)) + `,"execution_modes":["interactive"],"channels":[{"id":"answer"}],"assets":[{"source":"animations/shake.anim","sha256":"` + hex.EncodeToString(assetDigest[:]) + `","size":` + fmt.Sprint(len(asset)) + `,"media_type":"application/x-busy-animation"}]}`)
}

func packageManifestWithConfigSchema(executable, schema []byte) []byte {
	executableDigest := sha256.Sum256(executable)
	schemaDigest := sha256.Sum256(schema)
	return []byte(`{"id":"dev.bsbctl.ball8","version":"1.0.0","protocol_version":"1.0","executable":"bsbctl-plugin-ball8","executable_sha256":"` + hex.EncodeToString(executableDigest[:]) + `","executable_size":` + fmt.Sprint(len(executable)) + `,"execution_modes":["interactive"],"channels":[{"id":"answer"}],"config_schema":{"source":"config.schema.json","sha256":"` + hex.EncodeToString(schemaDigest[:]) + `","size":` + fmt.Sprint(len(schema)) + `},"assets":[]}`)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("mode for %q = %o, want %o", filepath.Base(path), info.Mode().Perm(), want)
	}
}

func assertEmptyDirectory(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory contains %d owned staging entries", len(entries))
	}
}
