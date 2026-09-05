package installer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func (target *promotionTarget) descriptors() []int {
	result := make([]int, 0, len(target.chain.nodes)+1)
	for _, node := range target.chain.nodes {
		result = append(result, node.fd)
	}
	if target.targetFD >= 0 {
		result = append(result, target.targetFD)
	}
	return result
}

func TestPackageStorePromotesToImmutablePlatformPath(t *testing.T) {
	root := t.TempDir()
	store := NewPackageStore(root)
	staging, err := store.StagingDir()
	if err != nil {
		t.Fatal(err)
	}
	verified := extractedFixture(t, staging)

	release, outcome, err := store.Promote(verified)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if outcome != PromotionInstalled {
		t.Fatalf("outcome = %q", outcome)
	}
	want := filepath.Join(root, "plugins", "dev.bsbctl.ball8", "1.0.0", "darwin-arm64")
	if release.Root != want {
		t.Fatalf("release root = %q, want %q", release.Root, want)
	}
	if _, err := os.Stat(filepath.Join(want, "bsbctl-plugin-ball8")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(verified.Root); !os.IsNotExist(err) {
		t.Fatalf("staging still exists: %v", err)
	}
}

func TestPackageStoreExistingPackageIsIdempotentOnlyAfterFullVerification(t *testing.T) {
	root := t.TempDir()
	store := NewPackageStore(root)
	staging, err := store.StagingDir()
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := store.Promote(extractedFixture(t, staging))
	if err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Stat(first.Root)
	if err != nil {
		t.Fatal(err)
	}

	second, outcome, err := store.Promote(extractedFixture(t, staging))
	if err != nil {
		t.Fatalf("idempotent Promote: %v", err)
	}
	if outcome != PromotionAlreadyInstalled || second.Root != first.Root {
		t.Fatalf("outcome = %q, release = %#v", outcome, second)
	}
	secondInfo, err := os.Stat(second.Root)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Fatal("idempotent install replaced immutable directory")
	}

	if err := os.WriteFile(filepath.Join(first.Root, "bsbctl-plugin-ball8"), []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	incoming := extractedFixture(t, staging)
	if _, _, err := store.Promote(incoming); CodeOf(err) != CodeInstallConflict {
		t.Fatalf("tampered existing package error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(first.Root, "bsbctl-plugin-ball8"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "tampered" {
		t.Fatal("conflict replaced existing immutable directory")
	}
	if _, err := os.Stat(incoming.Root); !os.IsNotExist(err) {
		t.Fatalf("incoming staging was not cleaned: %v", err)
	}
}

func TestPackageStoreRejectsUnsafeIdentityWithoutEscapingRoot(t *testing.T) {
	root := t.TempDir()
	store := NewPackageStore(root)
	staging, err := store.StagingDir()
	if err != nil {
		t.Fatal(err)
	}
	verified := extractedFixture(t, staging)
	verified.Entry.ID = "../escape"
	verified.Manifest.ID = "../escape"
	if _, _, err := store.Promote(verified); CodeOf(err) != CodePackageInvalid {
		t.Fatalf("unsafe identity error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
		t.Fatalf("unsafe identity escaped root: %v", err)
	}
}

func TestPackageStoreRejectsStagingRootReplacedAfterVerification(t *testing.T) {
	root := t.TempDir()
	store := NewPackageStore(root)
	staging, err := store.StagingDir()
	if err != nil {
		t.Fatal(err)
	}
	verified := extractedFixture(t, staging)
	originalPath := verified.Root + "-original"
	if err := os.Rename(verified.Root, originalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(verified.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(verified.Root, "replacement"), []byte("unauthenticated"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.Promote(verified); CodeOf(err) != CodePackageInvalid {
		t.Fatalf("Promote replacement error = %v", err)
	}
	target := filepath.Join(root, "plugins", "dev.bsbctl.ball8", "1.0.0", "darwin-arm64")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("replacement was promoted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verified.Root, "replacement")); err != nil {
		t.Fatalf("replacement staging path was removed or moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(originalPath, "manifest.json")); err != nil {
		t.Fatalf("owned staging root was lost: %v", err)
	}
}

func TestPinnedPromotionRejectsVersionParentReplacementBeforeRename(t *testing.T) {
	root := t.TempDir()
	store := NewPackageStore(root)
	staging, err := store.StagingDir()
	if err != nil {
		t.Fatal(err)
	}
	verified := extractedFixture(t, staging)
	target, err := store.pinPromotionTarget(verified.Entry)
	if err != nil {
		t.Fatal(err)
	}
	defer target.close()
	versionPath := filepath.Join(root, "plugins", verified.Entry.ID, verified.Entry.Version)
	hiddenVersion := versionPath + "-pinned"
	if err := os.Rename(versionPath, hiddenVersion); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(versionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	publicTarget := filepath.Join(versionPath, verified.Entry.OS+"-"+verified.Entry.Arch)
	if err := os.Mkdir(publicTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publicTarget, "replacement"), []byte("attacker bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := target.rename(verified.root); err == nil {
		t.Fatal("rename succeeded through a replaced public version parent")
	}
	if _, err := os.Stat(filepath.Join(publicTarget, "replacement")); err != nil {
		t.Fatalf("public replacement was changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hiddenVersion, verified.Entry.OS+"-"+verified.Entry.Arch)); !os.IsNotExist(err) {
		t.Fatalf("authenticated root was renamed into hidden version parent: %v", err)
	}
}

func TestPinnedPromotionRejectsVersionParentReplacementBeforeDurability(t *testing.T) {
	root := t.TempDir()
	store := NewPackageStore(root)
	staging, err := store.StagingDir()
	if err != nil {
		t.Fatal(err)
	}
	verified := extractedFixture(t, staging)
	target, err := store.pinPromotionTarget(verified.Entry)
	if err != nil {
		t.Fatal(err)
	}
	defer target.close()
	if err := target.rename(verified.root); err != nil {
		t.Fatalf("rename: %v", err)
	}
	versionPath := filepath.Join(root, "plugins", verified.Entry.ID, verified.Entry.Version)
	hiddenVersion := versionPath + "-pinned"
	if err := os.Rename(versionPath, hiddenVersion); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(versionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	publicTarget := filepath.Join(versionPath, verified.Entry.OS+"-"+verified.Entry.Arch)
	if err := os.Mkdir(publicTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publicTarget, "replacement"), []byte("attacker bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := target.syncAndVerify(store.syncPinned); err == nil {
		t.Fatal("durability succeeded after the public version parent was replaced")
	}
	if _, err := os.Stat(filepath.Join(publicTarget, "replacement")); err != nil {
		t.Fatalf("public replacement was changed: %v", err)
	}
}

func TestPinnedPromotionSucceedsForUnchangedPublicChainAndClosesDescriptors(t *testing.T) {
	root := t.TempDir()
	store := NewPackageStore(root)
	staging, err := store.StagingDir()
	if err != nil {
		t.Fatal(err)
	}
	verified := extractedFixture(t, staging)
	target, err := store.pinPromotionTarget(verified.Entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.rename(verified.root); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := target.syncAndVerify(store.syncPinned); err != nil {
		t.Fatalf("syncAndVerify: %v", err)
	}
	fds := append(target.descriptors(), verified.root.fd, verified.root.parentFD)
	if err := target.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := target.close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := verified.Close(); err != nil {
		t.Fatalf("verified close: %v", err)
	}
	for _, fd := range fds {
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); !errors.Is(err, unix.EBADF) {
			t.Fatalf("descriptor %d remains open: %v", fd, err)
		}
	}
	publicTarget := filepath.Join(root, "plugins", verified.Entry.ID, verified.Entry.Version, verified.Entry.OS+"-"+verified.Entry.Arch)
	if _, err := os.Stat(filepath.Join(publicTarget, verified.Manifest.Executable)); err != nil {
		t.Fatalf("authenticated executable is not public: %v", err)
	}
}

func TestEnsureDirectoryPathSyncsEachParentAfterCreatingChild(t *testing.T) {
	root := filepath.Join(t.TempDir(), "install-root")
	target := filepath.Join(root, "plugins", "dev.bsbctl.ball8", "1.0.0")
	var synced []string
	if err := ensureDirectoryPathWithSync(root, target, func(path string) error {
		synced = append(synced, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Dir(root),
		root,
		filepath.Join(root, "plugins"),
		filepath.Join(root, "plugins", "dev.bsbctl.ball8"),
	}
	if !reflect.DeepEqual(synced, want) {
		t.Fatalf("synced parents = %#v, want %#v", synced, want)
	}
}

func TestEnsureDirectoryPathStopsAtEveryAncestorSyncFailure(t *testing.T) {
	for failAt := 1; failAt <= 4; failAt++ {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "install-root")
			target := filepath.Join(root, "plugins", "dev.bsbctl.ball8", "1.0.0")
			calls := 0
			err := ensureDirectoryPathWithSync(root, target, func(string) error {
				calls++
				if calls == failAt {
					return errors.New("sync fault")
				}
				return nil
			})
			if err == nil || calls != failAt {
				t.Fatalf("error = %v, sync calls = %d", err, calls)
			}
		})
	}
}

func extractedFixture(t *testing.T, staging string) VerifiedPackage {
	t.Helper()
	executable := []byte("executable")
	asset := []byte("animation")
	artifactPath, entry := writeArtifact(t, tarGzip(t,
		tarItem{name: "manifest.json", body: packageManifest(executable, asset)},
		tarItem{name: "bsbctl-plugin-ball8", body: executable},
		tarItem{name: "animations/shake.anim", body: asset},
	))
	verified, err := ExtractAndVerify(artifactPath, staging, entry)
	if err != nil {
		t.Fatalf("ExtractAndVerify fixture: %v", err)
	}
	t.Cleanup(func() { _ = verified.Close() })
	return verified
}
