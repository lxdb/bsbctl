package installer

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/configschema"
	"golang.org/x/sys/unix"
	"golang.org/x/text/cases"
)

const (
	maxArchiveEntries          = 512
	maxArchiveFileBytes  int64 = 128 << 20
	maxArchiveTotalBytes       = 384 << 20
	maxArchivePathBytes        = 240
)

type FileRecord struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type VerifiedPackage struct {
	Root           string
	Entry          catalog.Entry
	Manifest       catalog.PackageManifest
	ManifestSHA256 string
	Files          map[string]FileRecord
	root           *extractionRoot
}

// Close releases the verified package's owned directory handles. It is safe to
// call more than once. A package promoted into the immutable store remains
// installed; an unpromoted package's staging directory is removed when its
// original pathname still identifies the owned directory.
func (verified VerifiedPackage) Close() error {
	if verified.root == nil {
		return nil
	}
	return verified.root.closeAndRemove()
}

// ExtractAndVerifyFile authenticates and extracts from artifact without taking
// ownership of it. The caller must close artifact; the function seeks it and
// may leave its offset changed. On success, the returned VerifiedPackage owns
// staging directory handles and must be closed unless passed to Promote, which
// closes it before returning.
func ExtractAndVerifyFile(artifact *os.File, stagingParent string, entry catalog.Entry) (VerifiedPackage, error) {
	if err := authenticateArtifactFile(artifact, entry); err != nil {
		return VerifiedPackage{}, errorCode(CodePackageInvalid)
	}
	parentInfo, err := os.Lstat(stagingParent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return VerifiedPackage{}, errorCode(CodePackageInvalid)
	}
	staging, err := os.MkdirTemp(stagingParent, ".bsbctl-stage-*")
	if err != nil {
		return VerifiedPackage{}, errorCode(CodePackageInvalid)
	}
	keep := false
	if err := os.Chmod(staging, 0o700); err != nil {
		_ = os.RemoveAll(staging)
		return VerifiedPackage{}, errorCode(CodePackageInvalid)
	}
	root, err := openExtractionRoot(staging)
	if err != nil {
		return VerifiedPackage{}, errorCode(CodePackageInvalid)
	}
	defer func() {
		if !keep {
			_ = root.closeAndRemove()
		}
	}()
	records, err := extractArchiveRoot(artifact, root)
	if err != nil {
		return VerifiedPackage{}, errorCode(CodePackageInvalid)
	}
	verified, err := verifyExtractedPackageRoot(root, staging, entry, records)
	if err != nil {
		return VerifiedPackage{}, errorCode(CodePackageInvalid)
	}
	keep = true
	return verified, nil
}

func authenticateArtifactFile(artifact *os.File, entry catalog.Entry) error {
	if artifact == nil {
		return errors.New("invalid artifact")
	}
	info, err := artifact.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != entry.CompressedSize || info.Size() < 1 || info.Size() > catalog.MaxArtifactBytes {
		return errors.New("invalid artifact")
	}
	if _, err := artifact.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(artifact, catalog.MaxArtifactBytes+1))
	if err != nil || written != entry.CompressedSize || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		return errors.New("invalid artifact")
	}
	_, err = artifact.Seek(0, io.SeekStart)
	return err
}

func extractArchiveRoot(artifact *os.File, root *extractionRoot) (map[string]FileRecord, error) {
	gzipReader, err := gzip.NewReader(artifact)
	if err != nil {
		return nil, err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	seen := make(map[string]struct{})
	foldedPrefixes := make(map[string]archivePathIdentity)
	records := make(map[string]FileRecord)
	var total int64
	entries := 0
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		entries++
		if entries > maxArchiveEntries || header.Format == tar.FormatPAX || header.Format == tar.FormatGNU || len(header.PAXRecords) != 0 || len(header.Xattrs) != 0 {
			return nil, errors.New("unsupported archive metadata")
		}
		relative, err := normalizedArchivePath(header.Name, header.Typeflag == tar.TypeDir)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[relative]; exists {
			return nil, errors.New("duplicate archive path")
		}
		if err := trackArchivePath(foldedPrefixes, relative, header.Typeflag == tar.TypeDir); err != nil {
			return nil, err
		}
		seen[relative] = struct{}{}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 || root.mkdirAll(relative) != nil {
				return nil, errors.New("invalid archive directory")
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := validateRegularFileLimit(header.Size); err != nil {
				return nil, err
			}
			if err := validateTotalFileLimit(total, header.Size); err != nil {
				return nil, err
			}
			total += header.Size
			if err := root.mkdirAll(path.Dir(relative)); err != nil {
				return nil, err
			}
			file, err := root.createFile(relative, 0o600)
			if err != nil {
				return nil, err
			}
			hash := sha256.New()
			written, copyErr := io.CopyN(io.MultiWriter(file, hash), tarReader, header.Size)
			if copyErr == nil {
				copyErr = file.Chmod(0o600)
			}
			if copyErr == nil {
				copyErr = file.Sync()
			}
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil || written != header.Size {
				return nil, errors.New("archive file is incomplete")
			}
			records[relative] = FileRecord{SHA256: hex.EncodeToString(hash.Sum(nil)), Size: written}
		default:
			return nil, errors.New("unsupported archive entry")
		}
	}
	return records, nil
}

func foldArchivePath(relative string) string {
	return cases.Fold().String(relative)
}

type archivePathIdentity struct {
	original  string
	directory bool
}

func trackArchivePath(identities map[string]archivePathIdentity, relative string, directory bool) error {
	components := strings.Split(relative, "/")
	for index := range components {
		prefix := strings.Join(components[:index+1], "/")
		identity := archivePathIdentity{original: prefix, directory: index < len(components)-1 || directory}
		folded := foldArchivePath(prefix)
		if existing, exists := identities[folded]; exists {
			if existing != identity {
				return errors.New("case-fold-colliding archive path component")
			}
			continue
		}
		identities[folded] = identity
	}
	return nil
}

type extractionRoot struct {
	fd            int
	parentFD      int
	path          string
	parentPath    string
	name          string
	identity      directoryIdentity
	closed        bool
	removeOnClose bool
}

type directoryIdentity struct {
	device uint64
	inode  uint64
}

func openExtractionRoot(root string) (*extractionRoot, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	parentPath := filepath.Dir(root)
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	identity, err := identityForFD(fd)
	if err != nil {
		unix.Close(parentFD)
		unix.Close(fd)
		return nil, err
	}
	return &extractionRoot{fd: fd, parentFD: parentFD, path: root, parentPath: parentPath, name: filepath.Base(root), identity: identity, removeOnClose: true}, nil
}

func (root *extractionRoot) close() error {
	if root == nil || root.closed {
		return nil
	}
	root.closed = true
	return errors.Join(unix.Close(root.fd), unix.Close(root.parentFD))
}

func (root *extractionRoot) closeAndRemove() error {
	if root == nil || root.closed {
		return nil
	}
	var removeErr error
	if root.removeOnClose && root.matchesOriginalPath() {
		removeErr = os.RemoveAll(root.path)
	}
	return errors.Join(removeErr, root.close())
}

func identityForFD(fd int) (directoryIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return directoryIdentity{}, err
	}
	return directoryIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, nil
}

func (root *extractionRoot) matchesOriginalPath() bool {
	if root == nil || root.closed {
		return false
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(root.parentFD, root.name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	return uint64(stat.Dev) == root.identity.device && uint64(stat.Ino) == root.identity.inode && stat.Mode&unix.S_IFMT == unix.S_IFDIR
}

func (root *extractionRoot) mkdirAll(relative string) error {
	if relative == "." || relative == "" {
		return nil
	}
	current, err := unix.Dup(root.fd)
	if err != nil {
		return err
	}
	for _, component := range strings.Split(relative, "/") {
		if err := unix.Mkdirat(current, component, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			unix.Close(current)
			return err
		}
		next, err := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			unix.Close(current)
			return err
		}
		if err := unix.Fchmod(next, 0o700); err != nil {
			unix.Close(next)
			unix.Close(current)
			return err
		}
		unix.Close(current)
		current = next
	}
	return unix.Close(current)
}

func (root *extractionRoot) createFile(relative string, mode os.FileMode) (*os.File, error) {
	components := strings.Split(relative, "/")
	current, err := unix.Dup(root.fd)
	if err != nil {
		return nil, err
	}
	for _, component := range components[:len(components)-1] {
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(current)
		if openErr != nil {
			return nil, openErr
		}
		current = next
	}
	fd, err := unix.Openat(current, components[len(components)-1], unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
	unix.Close(current)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), relative), nil
}

func (root *extractionRoot) openRegular(relative string, flags int) (*os.File, error) {
	components := strings.Split(relative, "/")
	current, err := unix.Dup(root.fd)
	if err != nil {
		return nil, err
	}
	for _, component := range components[:len(components)-1] {
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(current)
		if openErr != nil {
			return nil, openErr
		}
		current = next
	}
	fd, err := unix.Openat(current, components[len(components)-1], flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	unix.Close(current)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		return nil, errors.New("package path is not a regular file")
	}
	return os.NewFile(uintptr(fd), relative), nil
}

func (root *extractionRoot) readRegular(relative string, limit int64) ([]byte, error) {
	file, err := root.openRegular(relative, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() < 0 || info.Size() > limit {
		return nil, errors.New("package file exceeds limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) != info.Size() {
		return nil, errors.New("package file changed while reading")
	}
	return data, nil
}

func (root *extractionRoot) chmodRegular(relative string, mode os.FileMode) error {
	file, err := root.openRegular(relative, unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Chmod(mode)
}

func (root *extractionRoot) verifyAndSyncRecords(executable string) (map[string]FileRecord, error) {
	records := make(map[string]FileRecord)
	if err := scanPackageDirectory(root.fd, "", executable, records); err != nil {
		return nil, err
	}
	return records, nil
}

func scanPackageDirectory(directoryFD int, prefix, executable string, records map[string]FileRecord) error {
	scanFD, err := unix.Openat(directoryFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(scanFD), prefix)
	entries, err := directory.ReadDir(-1)
	if err != nil {
		_ = directory.Close()
		return err
	}
	for _, entry := range entries {
		relative := entry.Name()
		if prefix != "" {
			relative = prefix + "/" + relative
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(scanFD, entry.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			_ = directory.Close()
			return err
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if _, err := normalizedArchivePath(relative, true); err != nil || os.FileMode(stat.Mode).Perm() != 0o700 {
				_ = directory.Close()
				return errors.New("installed directory is invalid")
			}
			childFD, err := unix.Openat(scanFD, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				_ = directory.Close()
				return err
			}
			err = scanPackageDirectory(childFD, relative, executable, records)
			unix.Close(childFD)
			if err != nil {
				_ = directory.Close()
				return err
			}
		case unix.S_IFREG:
			if _, err := normalizedArchivePath(relative, false); err != nil || len(records) >= maxArchiveEntries {
				_ = directory.Close()
				return errors.New("installed file path is invalid")
			}
			wantMode := os.FileMode(0o600)
			if relative == executable {
				wantMode = 0o700
			}
			if os.FileMode(stat.Mode).Perm() != wantMode {
				_ = directory.Close()
				return errors.New("installed file mode is invalid")
			}
			fileFD, err := unix.Openat(scanFD, entry.Name(), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				_ = directory.Close()
				return err
			}
			record, err := hashRegularDescriptor(fileFD, maxArchiveFileBytes)
			if err != nil {
				_ = directory.Close()
				return err
			}
			records[relative] = record
		default:
			_ = directory.Close()
			return errors.New("installed package contains special file")
		}
	}
	syncErr := unix.Fsync(scanFD)
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func hashRegularDescriptor(fd int, limit int64) (FileRecord, error) {
	file := os.NewFile(uintptr(fd), "package-file")
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > limit {
		return FileRecord{}, errors.New("file is invalid")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, limit+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || written != before.Size() || !os.SameFile(before, after) || after.Size() != before.Size() {
		return FileRecord{}, errors.New("file changed while reading")
	}
	return FileRecord{SHA256: hex.EncodeToString(hash.Sum(nil)), Size: written}, nil
}

func normalizedArchivePath(name string, directory bool) (string, error) {
	if !utf8.ValidString(name) || len(name) == 0 || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) {
		return "", errors.New("unsafe archive path")
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return "", errors.New("unsafe archive path")
		}
	}
	normalized := name
	if directory {
		normalized = strings.TrimSuffix(normalized, "/")
	} else if strings.HasSuffix(normalized, "/") {
		return "", errors.New("unsafe archive path")
	}
	if len(normalized) > maxArchivePathBytes || normalized == "" || normalized == "." || normalized == ".." || path.Clean(normalized) != normalized || strings.HasPrefix(normalized, "../") {
		return "", errors.New("unsafe archive path")
	}
	return normalized, nil
}

func validateRegularFileLimit(size int64) error {
	if size < 0 || size > maxArchiveFileBytes {
		return errors.New("archive file exceeds limit")
	}
	return nil
}

func validateTotalFileLimit(total, next int64) error {
	if next < 0 || total < 0 || next > maxArchiveTotalBytes-total {
		return errors.New("archive total exceeds limit")
	}
	return nil
}

func verifyExtractedPackageRoot(root *extractionRoot, logicalRoot string, entry catalog.Entry, records map[string]FileRecord) (VerifiedPackage, error) {
	manifestRecord, ok := records[entry.Manifest]
	if !ok || entry.Manifest != "manifest.json" || manifestRecord.Size > maxCatalogManifestBytes {
		return VerifiedPackage{}, errors.New("root manifest is missing")
	}
	manifestData, err := root.readRegular("manifest.json", maxCatalogManifestBytes)
	if err != nil || int64(len(manifestData)) != manifestRecord.Size {
		return VerifiedPackage{}, errors.New("root manifest is invalid")
	}
	manifest, err := catalog.VerifyPackageManifest(manifestData, entry, logicalRoot)
	if err != nil {
		return VerifiedPackage{}, err
	}
	allowed := map[string]struct{}{"manifest.json": {}, manifest.Executable: {}}
	executableRecord, ok := records[manifest.Executable]
	if !ok || executableRecord.Size != manifest.ExecutableSize || executableRecord.SHA256 != manifest.ExecutableSHA256 {
		return VerifiedPackage{}, errors.New("executable is invalid")
	}
	if declaration := manifest.ConfigSchema; declaration != nil {
		if declaration.Source == "manifest.json" || declaration.Source == manifest.Executable {
			return VerifiedPackage{}, errors.New("config schema overlaps package control file")
		}
		record, exists := records[declaration.Source]
		if !exists || record.Size != declaration.Size || record.SHA256 != declaration.SHA256 {
			return VerifiedPackage{}, errors.New("declared config schema is invalid")
		}
		schemaData, err := root.readRegular(declaration.Source, declaration.Size)
		if err != nil || int64(len(schemaData)) != declaration.Size {
			return VerifiedPackage{}, errors.New("declared config schema is invalid")
		}
		if _, err := configschema.Compile(schemaData); err != nil {
			return VerifiedPackage{}, errors.New("declared config schema is invalid")
		}
		allowed[declaration.Source] = struct{}{}
	}
	for _, declaration := range manifest.Assets {
		if declaration.Source == "manifest.json" || declaration.Source == manifest.Executable {
			return VerifiedPackage{}, errors.New("asset overlaps package control file")
		}
		if _, exists := allowed[declaration.Source]; exists {
			return VerifiedPackage{}, errors.New("duplicate declared asset")
		}
		allowed[declaration.Source] = struct{}{}
		record, exists := records[declaration.Source]
		if !exists || record.Size != declaration.Size || record.SHA256 != declaration.SHA256 {
			return VerifiedPackage{}, errors.New("declared asset is invalid")
		}
	}
	for relative, record := range records {
		if _, exists := allowed[relative]; !exists && !packageDocumentation(relative, record.Size) {
			return VerifiedPackage{}, errors.New("archive contains undeclared regular file")
		}
	}
	if err := root.chmodRegular(manifest.Executable, 0o700); err != nil {
		return VerifiedPackage{}, err
	}
	verifiedRecords, err := root.verifyAndSyncRecords(manifest.Executable)
	if err != nil || !equalRecords(verifiedRecords, records) {
		return VerifiedPackage{}, errors.New("extracted package content differs")
	}
	manifestHash := sha256.Sum256(manifestData)
	return VerifiedPackage{Root: logicalRoot, Entry: entry, Manifest: manifest, ManifestSHA256: hex.EncodeToString(manifestHash[:]), Files: cloneRecords(records), root: root}, nil
}

const maxCatalogManifestBytes int64 = 1 << 20

// Documentation is authenticated by the signed archive digest, retained in the
// installed inventory, and never treated as executable code or device assets.
func packageDocumentation(relative string, size int64) bool {
	if size < 1 || size > 1<<20 {
		return false
	}
	switch relative {
	case "LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md", "sbom.cdx.json":
		return true
	}
	return path.Dir(relative) == "LICENSES" && strings.HasSuffix(relative, ".txt")
}

func cloneRecords(source map[string]FileRecord) map[string]FileRecord {
	result := make(map[string]FileRecord, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
