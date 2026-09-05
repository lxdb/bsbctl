package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lxdb/bsbctl/internal/catalog"
	"golang.org/x/sys/unix"
)

type Promotion string

const (
	PromotionInstalled                    Promotion = "installed"
	PromotionAlreadyInstalled             Promotion = "already_installed"
	PromotionInstalledDurabilityUncertain Promotion = "installed_durability_uncertain"
)

type InstalledRelease struct {
	ID             string                  `json:"id"`
	Version        string                  `json:"version"`
	OS             string                  `json:"os"`
	Arch           string                  `json:"arch"`
	Manifest       catalog.PackageManifest `json:"manifest"`
	ManifestSHA256 string                  `json:"manifest_sha256"`
	Files          map[string]FileRecord   `json:"files"`
	Root           string                  `json:"-"`
}

type PackageStore struct {
	root           string
	syncDirectory  func(string) error
	syncDescriptor func(int, string) error
}

var safePluginToken = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,127}$`)
var safeVersionToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
var errPublicInstallPathChanged = errors.New("public install path identity changed")

func NewPackageStore(root string) *PackageStore {
	return &PackageStore{
		root: filepath.Clean(root), syncDirectory: syncDirectory,
		syncDescriptor: func(fd int, _ string) error { return unix.Fsync(fd) },
	}
}

func (store *PackageStore) StagingDir() (string, error) {
	if store == nil || !filepath.IsAbs(store.root) {
		return "", errorCode(CodePackageInvalid)
	}
	directory := filepath.Join(store.root, "plugins", ".staging")
	if err := ensureDirectoryPathWithSync(store.root, directory, store.sync); err != nil {
		return "", errorCode(CodePackageInvalid)
	}
	return directory, nil
}

func (store *PackageStore) Promote(verified VerifiedPackage) (InstalledRelease, Promotion, error) {
	staging, err := store.StagingDir()
	if err != nil {
		return InstalledRelease{}, "", err
	}
	owned := verified.root != nil && !verified.root.closed && verified.root.path == filepath.Clean(verified.Root) && verified.root.parentPath == staging
	if !owned {
		return InstalledRelease{}, "", errorCode(CodePackageInvalid)
	}
	defer verified.Close()
	if !validReleaseIdentity(verified.Entry.ID, verified.Entry.Version, verified.Entry.OS, verified.Entry.Arch) || verified.Manifest.ID != verified.Entry.ID || verified.Manifest.Version != verified.Entry.Version {
		return InstalledRelease{}, "", errorCode(CodePackageInvalid)
	}
	target, err := store.pinPromotionTarget(verified.Entry)
	if err != nil {
		if errors.Is(err, errPublicInstallPathChanged) {
			return InstalledRelease{}, "", errorCode(CodeInstallConflict)
		}
		return InstalledRelease{}, "", errorCode(CodePackageInvalid)
	}
	defer target.close()
	existing, err := target.openExisting()
	if err != nil {
		return InstalledRelease{}, "", errorCode(CodeInstallConflict)
	}
	if existing {
		targetRoot := &extractionRoot{fd: target.targetFD, path: target.path, identity: target.targetIdentity}
		if verifyExistingPackageRoot(targetRoot, target.path, verified) != nil {
			return InstalledRelease{}, "", errorCode(CodeInstallConflict)
		}
		release := installedRelease(target.path, verified)
		if err := target.syncAndVerify(store.syncPinned); err != nil {
			return release, PromotionInstalledDurabilityUncertain, errorCode(CodeStateFailed)
		}
		return release, PromotionAlreadyInstalled, nil
	}
	if err := target.rename(verified.root); err != nil {
		if _, statErr := os.Lstat(target.path); statErr == nil {
			return InstalledRelease{}, "", errorCode(CodeInstallConflict)
		}
		return InstalledRelease{}, "", errorCode(CodePackageInvalid)
	}
	release := installedRelease(target.path, verified)
	if err := target.syncAndVerify(store.syncPinned); err != nil {
		return release, PromotionInstalledDurabilityUncertain, errorCode(CodeStateFailed)
	}
	return release, PromotionInstalled, nil
}

func (store *PackageStore) VerifyInstalled(release InstalledRelease) error {
	root := store.releaseRoot(release.Ref())
	verified := VerifiedPackage{
		Root:     root,
		Entry:    catalog.Entry{ID: release.ID, Version: release.Version, OS: release.OS, Arch: release.Arch, Executable: release.Manifest.Executable, Manifest: "manifest.json"},
		Manifest: release.Manifest, ManifestSHA256: release.ManifestSHA256, Files: cloneRecords(release.Files),
	}
	if err := verifyExistingPackage(root, verified); err != nil {
		return errorCode(CodeInstallConflict)
	}
	return nil
}

func (store *PackageStore) releaseRoot(ref ReleaseRef) string {
	return filepath.Join(store.root, "plugins", ref.ID, ref.Version, ref.OS+"-"+ref.Arch)
}

func installedRelease(root string, verified VerifiedPackage) InstalledRelease {
	return InstalledRelease{
		ID: verified.Entry.ID, Version: verified.Entry.Version, OS: verified.Entry.OS, Arch: verified.Entry.Arch,
		Manifest: verified.Manifest, ManifestSHA256: verified.ManifestSHA256, Files: cloneRecords(verified.Files), Root: root,
	}
}

func validReleaseIdentity(id, version, goos, goarch string) bool {
	return safePluginToken.MatchString(id) && id != "." && id != ".." && safeVersionToken.MatchString(version) && version != "." && version != ".." && goos == "darwin" && (goarch == "arm64" || goarch == "amd64")
}

func (store *PackageStore) sync(path string) error {
	if store.syncDirectory == nil {
		return syncDirectory(path)
	}
	return store.syncDirectory(path)
}

func (store *PackageStore) syncPinned(fd int, path string) error {
	if store.syncDescriptor == nil {
		return unix.Fsync(fd)
	}
	return store.syncDescriptor(fd, path)
}

func ensureDirectoryPathWithSync(root, target string, syncParent func(string) error) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || filepath.IsAbs(relative) {
		return errors.New("directory escapes install root")
	}
	current := root
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(root, 0o700); err != nil {
			return err
		}
	} else if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("install root is invalid")
	}
	if err := syncParent(filepath.Dir(root)); err != nil {
		return err
	}
	for _, component := range splitPath(relative) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
		} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("install path crosses non-directory")
		}
		if err := syncParent(filepath.Dir(current)); err != nil {
			return err
		}
	}
	return nil
}

func splitPath(relative string) []string {
	if relative == "." || relative == "" {
		return nil
	}
	return strings.Split(filepath.ToSlash(relative), "/")
}

type pinnedDirectory struct {
	fd       int
	path     string
	identity directoryIdentity
}

type pinnedDirectoryChain struct {
	rootPath string
	nodes    []pinnedDirectory
	closed   bool
}

type promotionTarget struct {
	chain          *pinnedDirectoryChain
	name           string
	path           string
	targetFD       int
	targetIdentity directoryIdentity
	closed         bool
}

func (store *PackageStore) pinPromotionTarget(entry catalog.Entry) (*promotionTarget, error) {
	chain, err := store.pinDirectoryChain([]string{"plugins", entry.ID, entry.Version})
	if err != nil {
		return nil, err
	}
	return &promotionTarget{chain: chain, name: entry.OS + "-" + entry.Arch, path: filepath.Join(store.root, "plugins", entry.ID, entry.Version, entry.OS+"-"+entry.Arch), targetFD: -1}, nil
}

func (store *PackageStore) pinDirectoryChain(components []string) (*pinnedDirectoryChain, error) {
	info, err := os.Lstat(store.root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("install root is invalid")
	}
	rootFD, err := unix.Open(store.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	identity, err := identityForFD(rootFD)
	if err != nil {
		unix.Close(rootFD)
		return nil, err
	}
	chain := &pinnedDirectoryChain{rootPath: store.root, nodes: []pinnedDirectory{{fd: rootFD, path: store.root, identity: identity}}}
	for _, component := range components {
		parent := chain.nodes[len(chain.nodes)-1]
		mkdirErr := unix.Mkdirat(parent.fd, component, 0o700)
		if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
			chain.close()
			return nil, mkdirErr
		}
		childFD, err := unix.Openat(parent.fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			chain.close()
			return nil, err
		}
		if err := unix.Fchmod(childFD, 0o700); err != nil {
			unix.Close(childFD)
			chain.close()
			return nil, err
		}
		childIdentity, err := identityForFD(childFD)
		if err != nil {
			unix.Close(childFD)
			chain.close()
			return nil, err
		}
		childPath := filepath.Join(parent.path, component)
		chain.nodes = append(chain.nodes, pinnedDirectory{fd: childFD, path: childPath, identity: childIdentity})
		if err := store.syncPinned(parent.fd, parent.path); err != nil {
			chain.close()
			return nil, err
		}
	}
	if err := chain.verifyPublic(); err != nil {
		chain.close()
		return nil, err
	}
	return chain, nil
}

func (chain *pinnedDirectoryChain) verifyPublic() error {
	current, err := chain.openVerifiedPublicEnd()
	if err != nil {
		return err
	}
	return unix.Close(current)
}

func (chain *pinnedDirectoryChain) openVerifiedPublicEnd() (int, error) {
	if chain == nil || chain.closed || len(chain.nodes) == 0 {
		return -1, errors.New("pinned directory chain is closed")
	}
	current, err := unix.Open(chain.rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, errors.Join(errPublicInstallPathChanged, err)
	}
	identity, err := identityForFD(current)
	if err != nil || identity != chain.nodes[0].identity {
		unix.Close(current)
		return -1, errPublicInstallPathChanged
	}
	for index := 1; index < len(chain.nodes); index++ {
		component := filepath.Base(chain.nodes[index].path)
		next, err := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			unix.Close(current)
			return -1, errors.Join(errPublicInstallPathChanged, err)
		}
		unix.Close(current)
		current = next
		identity, err := identityForFD(current)
		if err != nil || identity != chain.nodes[index].identity {
			unix.Close(current)
			return -1, errPublicInstallPathChanged
		}
	}
	return current, nil
}

func (chain *pinnedDirectoryChain) close() error {
	if chain == nil || chain.closed {
		return nil
	}
	chain.closed = true
	var result error
	for index := len(chain.nodes) - 1; index >= 0; index-- {
		result = errors.Join(result, unix.Close(chain.nodes[index].fd))
	}
	return result
}

func (target *promotionTarget) version() pinnedDirectory {
	return target.chain.nodes[len(target.chain.nodes)-1]
}

func (target *promotionTarget) openExisting() (bool, error) {
	if err := target.chain.verifyPublic(); err != nil {
		return false, err
	}
	fd, err := unix.Openat(target.version().fd, target.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	identity, err := identityForFD(fd)
	if err != nil {
		unix.Close(fd)
		return false, err
	}
	target.targetFD, target.targetIdentity = fd, identity
	if err := target.verifyPublicTarget(); err != nil {
		unix.Close(fd)
		target.targetFD = -1
		return false, err
	}
	return true, nil
}

func (target *promotionTarget) rename(source *extractionRoot) error {
	if target == nil || target.closed || source == nil || source.closed || !source.matchesOriginalPath() {
		return errors.New("promotion source is invalid")
	}
	if err := target.chain.verifyPublic(); err != nil {
		return err
	}
	var existing unix.Stat_t
	if err := unix.Fstatat(target.version().fd, target.name, &existing, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return errors.New("promotion target already exists")
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	if err := unix.Renameat(source.parentFD, source.name, target.version().fd, target.name); err != nil {
		return err
	}
	fd, err := unix.Openat(target.version().fd, target.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	identity, err := identityForFD(fd)
	if err != nil || identity != source.identity {
		unix.Close(fd)
		return errors.New("promoted directory identity differs")
	}
	target.targetFD, target.targetIdentity = fd, identity
	source.removeOnClose = false
	source.path = target.path
	return nil
}

func (target *promotionTarget) syncAndVerify(syncDescriptor func(int, string) error) error {
	if target == nil || target.closed || target.targetFD < 0 {
		return errors.New("promotion target is not open")
	}
	if err := syncDescriptor(target.targetFD, target.path); err != nil {
		return err
	}
	version := target.version()
	if err := syncDescriptor(version.fd, version.path); err != nil {
		return err
	}
	return target.verifyPublicTarget()
}

func (target *promotionTarget) verifyPublicTarget() error {
	publicVersionFD, err := target.chain.openVerifiedPublicEnd()
	if err != nil {
		return err
	}
	defer unix.Close(publicVersionFD)
	fd, err := unix.Openat(publicVersionFD, target.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	identity, err := identityForFD(fd)
	if err != nil || identity != target.targetIdentity {
		return errors.New("public promotion target identity changed")
	}
	return nil
}

func (target *promotionTarget) close() error {
	if target == nil || target.closed {
		return nil
	}
	target.closed = true
	var targetErr error
	if target.targetFD >= 0 {
		targetErr = unix.Close(target.targetFD)
		target.targetFD = -1
	}
	return errors.Join(targetErr, target.chain.close())
}

func verifyExistingPackage(root string, verified VerifiedPackage) error {
	ownedRoot, err := openExtractionRoot(root)
	if err != nil {
		return err
	}
	defer ownedRoot.close()
	return verifyExistingPackageRoot(ownedRoot, root, verified)
}

func verifyExistingPackageRoot(root *extractionRoot, logicalRoot string, verified VerifiedPackage) error {
	manifestData, err := root.readRegular("manifest.json", maxCatalogManifestBytes)
	if err != nil {
		return err
	}
	manifestHash := sha256.Sum256(manifestData)
	if hex.EncodeToString(manifestHash[:]) != verified.ManifestSHA256 {
		return errors.New("installed manifest differs")
	}
	if _, err := catalog.VerifyPackageManifest(manifestData, verified.Entry, logicalRoot); err != nil {
		return err
	}
	records, err := root.verifyAndSyncRecords(verified.Manifest.Executable)
	if err != nil || !equalRecords(records, verified.Files) {
		return errors.New("installed package content differs")
	}
	return nil
}

func equalRecords(left, right map[string]FileRecord) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
