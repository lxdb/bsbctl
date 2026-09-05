package assets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	busylib "github.com/lxdb/busylib-go"
)

func trimStoragePrefix(path string) string {
	return strings.TrimPrefix(path, "/ext/user_assets/"+ApplicationName+"/")
}

func TestReconcilerUploadsVerifiesResolvesAndRemovesPackagePaths(t *testing.T) {
	root := t.TempDir()
	content := []byte("animation")
	if err := os.MkdirAll(filepath.Join(root, "animations"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "animations", "shake.anim"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	device := &fakeDevice{files: make(map[string][]byte)}
	reconciler := NewReconciler(device)
	plugin := Package{PluginID: "ball8", Version: "1", Root: root, Enabled: true, Assets: []Declaration{{Source: "animations/shake.anim", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content)), MediaType: "application/x-busy-animation"}}}
	reconciler.Reconcile(context.Background(), []Package{plugin})
	if !reconciler.Ready("ball8") {
		t.Fatalf("state = %#v", reconciler.Status())
	}
	wantPath, err := derivedDevicePath(plugin.PluginID, plugin.Assets[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(device.files[wantPath], content) {
		t.Fatalf("uploaded %q = %q", wantPath, device.files[wantPath])
	}
	publicScene := presentation.Scene{Elements: []presentation.Element{{ID: "animation", Display: protocol.DisplayFront, Animation: &protocol.AnimationElement{Asset: protocol.AssetRef{PackagePath: "animations/shake.anim"}}}}}
	scene, err := reconciler.ResolveScene("ball8", publicScene)
	if err != nil || scene.Elements[0].Path != wantPath {
		t.Fatalf("resolved scene = %#v, %v", scene, err)
	}
	if publicScene.Elements[0].Animation.Asset.PackagePath != "animations/shake.anim" {
		t.Fatalf("public scene mutated during resolution: %#v", publicScene)
	}
	publicCue := presentation.AudioCue{ID: "cue", Asset: protocol.AssetRef{StockName: "calendar_event_starts.snd"}}
	resolvedCue, err := reconciler.ResolveAudioCue("ball8", publicCue)
	if err != nil || resolvedCue.Path != "shared/sounds/calendar_event_starts.snd" {
		t.Fatalf("resolved audio cue = %#v, %v", resolvedCue, err)
	}
	if publicCue.Asset.StockName != "calendar_event_starts.snd" {
		t.Fatalf("public audio cue mutated during resolution: %#v", publicCue)
	}

	plugin.Enabled = false
	reconciler.Reconcile(context.Background(), []Package{plugin})
	if device.removed != storageAssetPath(wantPath) {
		t.Fatalf("removed = %q", device.removed)
	}
}

func TestReconnectDiscardsInFlightDeviceVerification(t *testing.T) {
	value := testPackage(t, t.TempDir(), "1", "icon", "icon.png", []byte("icon"))
	path, err := derivedDevicePath(value.PluginID, value.Assets[0])
	if err != nil {
		t.Fatal(err)
	}
	backend := &inFlightReadDevice{
		fakeDevice: fakeDevice{files: map[string][]byte{path: []byte("icon")}},
		read:       make(chan struct{}), release: make(chan struct{}),
	}
	reconciler := NewReconciler(backend)
	done := make(chan struct{})
	go func() { reconciler.Reconcile(t.Context(), []Package{value}); close(done) }()
	<-backend.read
	reconciler.InvalidateConnection()
	delete(backend.files, path)
	close(backend.release)
	<-done
	if reconciler.ReadyFor(value) {
		t.Fatal("verification from the old connection restored readiness after firmware storage was cleared")
	}
	reconciler.Reconcile(t.Context(), []Package{value})
	if !reconciler.ReadyFor(value) || !bytes.Equal(backend.files[path], []byte("icon")) {
		t.Fatalf("reconnect retry did not restore content: state=%v, bytes=%q", reconciler.Status(), backend.files[path])
	}
}

type inFlightReadDevice struct {
	fakeDevice
	once    sync.Once
	read    chan struct{}
	release chan struct{}
}

func (d *inFlightReadDevice) ReadTo(ctx context.Context, path string, out io.Writer) (int64, error) {
	var data bytes.Buffer
	n, err := d.fakeDevice.ReadTo(ctx, path, &data)
	d.once.Do(func() { close(d.read); <-d.release })
	if err != nil {
		return n, err
	}
	return io.Copy(out, &data)
}

func TestReconcilerKeepsOfflineEnablePendingAndRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	content := []byte("icon")
	if err := os.WriteFile(filepath.Join(root, "icon.png"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	device := &fakeDevice{files: make(map[string][]byte), uploadErr: errors.New("offline")}
	reconciler := NewReconciler(device)
	reconciler.Reconcile(context.Background(), []Package{{PluginID: "github", Version: "1", Root: root, Enabled: true, Assets: []Declaration{{Source: "icon.png", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content)), MediaType: "image/png"}}}})
	if reconciler.Ready("github") || reconciler.Status()[0].Phase != PhasePending {
		t.Fatalf("state = %#v", reconciler.Status())
	}

	err := ValidatePackage(Package{PluginID: "bad", Version: "1", Root: root, Enabled: true, Assets: []Declaration{{Source: "../secret", SHA256: hex.EncodeToString(digest[:]), Size: 1, MediaType: "image/png"}}})
	if err == nil {
		t.Fatal("traversal unexpectedly validated")
	}
}

func TestDerivedPackagePathFitsProductionBusylibLimits(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	digest := sha256.Sum256([]byte("asset"))
	declaration := Declaration{
		Source: "codex-mark.anim",
		SHA256: hex.EncodeToString(digest[:]), Size: 5, MediaType: "application/x-busy-animation",
	}
	err := ValidatePackage(Package{
		PluginID: "plugin", Version: "1", Root: root, Enabled: true,
		Assets: []Declaration{declaration},
	})
	if err != nil {
		t.Fatal(err)
	}
	path, err := derivedDevicePath("plugin", declaration)
	if err != nil {
		t.Fatal(err)
	}
	body := busylib.BytesBody([]byte("asset"), "application/octet-stream")
	if err := (busylib.UploadAssetRequest{ApplicationName: ApplicationName, File: path, Body: body}).Validate(); err != nil {
		t.Fatalf("derived upload path %q is not representable: %v", path, err)
	}
	if err := (busylib.WriteStorageFileRequest{Path: storageAssetPath(path), Body: body}).Validate(); err != nil {
		t.Fatalf("derived storage path %q is not representable: %v", storageAssetPath(path), err)
	}
}

func TestPluginHashCollisionValidationRejectsDifferentIDsSharingPrefix(t *testing.T) {
	err := validatePluginHashCollisions([]string{"dev.bsbctl.codex", "dev.bsbctl.codex-quota"}, func(string) string { return "sameprefix" })
	if err == nil {
		t.Fatal("different plugin IDs sharing a physical prefix were accepted")
	}
	if err := validatePluginHashCollisions([]string{"dev.bsbctl.codex", "dev.bsbctl.codex"}, func(string) string { return "sameprefix" }); err != nil {
		t.Fatalf("duplicate platform entries for one plugin ID were rejected: %v", err)
	}
}

func TestResolverRejectsPackageAssetMediaKindMismatch(t *testing.T) {
	root := t.TempDir()
	image := testPackage(t, root, "1", "mark", "mark.png", []byte("png"))
	device := &fakeDevice{files: make(map[string][]byte)}
	reconciler := NewReconciler(device)
	reconciler.Reconcile(t.Context(), []Package{image})

	animationScene := presentation.Scene{Elements: []presentation.Element{{
		ID: "animation", Display: protocol.DisplayFront,
		Animation: &protocol.AnimationElement{Asset: protocol.AssetRef{PackagePath: image.Assets[0].Source}},
	}}}
	if _, err := reconciler.ResolveScene(image.PluginID, animationScene); !errors.Is(err, ErrAssetKindMismatch) {
		t.Fatalf("ResolveScene error = %v, want ErrAssetKindMismatch", err)
	}
	audio := presentation.AudioCue{ID: "cue", Asset: protocol.AssetRef{PackagePath: image.Assets[0].Source}}
	if _, err := reconciler.ResolveAudioCue(image.PluginID, audio); !errors.Is(err, ErrAssetKindMismatch) {
		t.Fatalf("ResolveAudioCue error = %v, want ErrAssetKindMismatch", err)
	}

	animation := testPackageWithMediaType(t, root, "2", "motion", "motion.anim", []byte("anim"), "application/x-busy-animation")
	reconciler.Reconcile(t.Context(), []Package{animation})
	imageScene := presentation.Scene{Elements: []presentation.Element{{
		ID: "image", Display: protocol.DisplayFront,
		Image: &protocol.ImageElement{Asset: protocol.AssetRef{PackagePath: animation.Assets[0].Source}},
	}}}
	if _, err := reconciler.ResolveScene(animation.PluginID, imageScene); !errors.Is(err, ErrAssetKindMismatch) {
		t.Fatalf("ResolveScene error = %v, want ErrAssetKindMismatch", err)
	}
}

func TestResolverRejectsUndeclaredPackageAssetAfterPackageIsReady(t *testing.T) {
	reconciler := NewReconciler(nil)
	reconciler.Reconcile(t.Context(), []Package{{PluginID: "plugin", Version: "1", Enabled: true}})
	scene := presentation.Scene{Elements: []presentation.Element{{
		ID: "image", Display: protocol.DisplayFront,
		Image: &protocol.ImageElement{Asset: protocol.AssetRef{PackagePath: "missing.png"}},
	}}}
	if _, err := reconciler.ResolveScene("plugin", scene); !errors.Is(err, ErrAssetUndeclared) {
		t.Fatalf("ResolveScene error = %v, want ErrAssetUndeclared", err)
	}
}

func TestReconcilerStatusExposesOnlySafeErrorCode(t *testing.T) {
	root := t.TempDir()
	plugin := testPackage(t, root, "1", "icon", "icon.png", []byte("icon"))
	device := &fakeDevice{files: make(map[string][]byte), uploadErr: errors.New("token=secret /Users/private")}
	reconciler := NewReconciler(device)
	reconciler.Reconcile(context.Background(), []Package{plugin})
	encoded, err := json.Marshal(reconciler.Status())
	if err != nil {
		t.Fatal(err)
	}
	value := string(encoded)
	if !strings.Contains(value, `"last_error_code":"device_unavailable"`) || strings.Contains(value, "secret") || strings.Contains(value, "/Users/") || strings.Contains(value, `"last_error":`) {
		t.Fatalf("unsafe asset status = %s", value)
	}
}

func TestReconcilerReadyManifestAndAbsentManifestAreIOIdempotent(t *testing.T) {
	root := t.TempDir()
	plugin := testPackage(t, root, "1", "icon", "old.png", []byte("old"))
	device := &fakeDevice{files: make(map[string][]byte)}
	reconciler := NewReconciler(device)
	reconciler.Reconcile(context.Background(), []Package{plugin})
	first := len(device.operations)
	reconciler.Reconcile(context.Background(), []Package{plugin})
	if got := len(device.operations); got != first {
		t.Fatalf("same ready manifest added I/O: %v", device.operations)
	}
	plugin.Enabled = false
	reconciler.Reconcile(context.Background(), []Package{plugin})
	disabled := len(device.operations)
	reconciler.Reconcile(context.Background(), []Package{plugin})
	if got := len(device.operations); got != disabled {
		t.Fatalf("same absent manifest added I/O: %v", device.operations)
	}
}

func TestReconcilerReconnectInvalidatesReadyAndRequiresReadback(t *testing.T) {
	root := t.TempDir()
	plugin := testPackage(t, root, "1", "icon", "icon.png", []byte("verified"))
	device := &fakeDevice{files: make(map[string][]byte)}
	reconciler := NewReconciler(device)
	reconciler.Reconcile(t.Context(), []Package{plugin})
	if !reconciler.Ready(plugin.PluginID) {
		t.Fatal("initial reconciliation did not become ready")
	}
	path, err := derivedDevicePath(plugin.PluginID, plugin.Assets[0])
	if err != nil {
		t.Fatal(err)
	}
	device.operations = nil
	reconciler.InvalidateConnection()
	if reconciler.Ready(plugin.PluginID) {
		t.Fatal("package readiness survived a new device connection")
	}
	if status := reconciler.Status(); len(status) != 1 || status[0].ValidationInvalidations != 1 {
		t.Fatalf("connection invalidation status = %#v", status)
	}
	reconciler.Reconcile(t.Context(), []Package{plugin})
	if want := []string{"read:" + path}; !equalStrings(device.operations, want) {
		t.Fatalf("reconnect operations = %v, want %v", device.operations, want)
	}
	if !reconciler.Ready(plugin.PluginID) {
		t.Fatal("verified reconnect readback did not restore readiness")
	}
	if status := reconciler.Status(); len(status) != 1 || status[0].ValidationInvalidations != 1 {
		t.Fatalf("reconciled invalidation status = %#v", status)
	}
}

func TestValidatePackageRejectsNoncanonicalSourcePath(t *testing.T) {
	digest := sha256.Sum256([]byte("icon"))
	err := ValidatePackage(Package{
		PluginID: "plugin", Version: "1", Root: t.TempDir(), Enabled: true,
		Assets: []Declaration{{Source: "assets/./icon.png", SHA256: hex.EncodeToString(digest[:]), Size: 4, MediaType: "image/png"}},
	})
	if err == nil {
		t.Fatal("noncanonical package source path was accepted")
	}
}

func TestReconcilerUpgradeVerifiesNewBeforeRemovingObsoleteAndRecovers(t *testing.T) {
	root := t.TempDir()
	old := testPackage(t, root, "1", "icon", "old.png", []byte("old"))
	device := &fakeDevice{files: make(map[string][]byte)}
	reconciler := NewReconciler(device)
	reconciler.Reconcile(context.Background(), []Package{old})
	device.operations = nil
	oldPath, _ := derivedDevicePath(old.PluginID, old.Assets[0])

	upgraded := testPackage(t, root, "2", "icon", "new.png", []byte("new"))
	newPath, _ := derivedDevicePath(upgraded.PluginID, upgraded.Assets[0])
	if oldPath == newPath {
		t.Fatalf("content change reused active path %q", oldPath)
	}
	device.removeErr = errors.New("offline")
	reconciler.Reconcile(context.Background(), []Package{upgraded})
	want := []string{"read:" + newPath, "upload:" + newPath, "read:" + newPath}
	if !equalStrings(device.operations, want) {
		t.Fatalf("failed upgrade operations = %v, want %v", device.operations, want)
	}
	if !reconciler.Ready("ball8") {
		t.Fatal("verified content was not activated before deferred GC")
	}
	device.operations = nil
	reconciler.CollectGarbage(context.Background(), []Package{upgraded})
	if want := []string{"remove:" + oldPath}; !equalStrings(device.operations, want) {
		t.Fatalf("failed GC operations = %v, want %v", device.operations, want)
	}
	if !reconciler.Ready("ball8") {
		t.Fatalf("GC failure degraded ready state: %#v", reconciler.Status())
	}
	device.operations = nil
	device.removeErr = nil
	reconciler.CollectGarbage(context.Background(), []Package{upgraded})
	if want := []string{"remove:" + oldPath}; !equalStrings(device.operations, want) {
		t.Fatalf("retry operations = %v, want %v", device.operations, want)
	}
	if !reconciler.Ready("ball8") {
		t.Fatalf("state = %#v", reconciler.Status())
	}
}

func TestReconcilerReadbackMismatchKeepsPackagePendingAndMappingInactive(t *testing.T) {
	root := t.TempDir()
	plugin := testPackage(t, root, "1", "icon", "icon.png", []byte("verified"))
	device := &fakeDevice{files: make(map[string][]byte), readOverride: []byte("corrupt")}
	reconciler := NewReconciler(device)
	reconciler.Reconcile(t.Context(), []Package{plugin})
	if reconciler.Ready(plugin.PluginID) {
		t.Fatal("package became ready after full SHA readback mismatch")
	}
	status := reconciler.Status()
	if len(status) != 1 || status[0].Phase != PhasePending || status[0].LastErrorCode != ErrorDeviceVerification {
		t.Fatalf("state = %#v, want pending device verification failure", status)
	}
	scene := presentation.Scene{Elements: []presentation.Element{{
		ID: "icon", Display: protocol.DisplayFront,
		Image: &protocol.ImageElement{Asset: protocol.AssetRef{PackagePath: plugin.Assets[0].Source}},
	}}}
	if _, err := reconciler.ResolveScene(plugin.PluginID, scene); !errors.Is(err, ErrAssetsNotReady) {
		t.Fatalf("ResolveScene error = %v, want ErrAssetsNotReady", err)
	}
}

func TestReconcilerInterruptedUpgradePreservesOldFileWithoutActivatingOldMapping(t *testing.T) {
	root := t.TempDir()
	old := testPackage(t, root, "1", "icon", "old.png", []byte("old"))
	device := &fakeDevice{files: make(map[string][]byte)}
	reconciler := NewReconciler(device)
	reconciler.Reconcile(t.Context(), []Package{old})
	oldPath, _ := derivedDevicePath(old.PluginID, old.Assets[0])
	oldBytes := append([]byte(nil), device.files[oldPath]...)

	upgraded := testPackage(t, root, "2", "icon", "new.png", []byte("new"))
	newPath, _ := derivedDevicePath(upgraded.PluginID, upgraded.Assets[0])
	device.uploadErr = errors.New("interrupted upload")
	reconciler.Reconcile(t.Context(), []Package{upgraded})
	if reconciler.Ready(upgraded.PluginID) {
		t.Fatal("interrupted upgrade retained a ready mapping")
	}
	if !bytes.Equal(device.files[oldPath], oldBytes) {
		t.Fatalf("old content-derived file %q was changed or removed", oldPath)
	}
	if _, exists := device.files[newPath]; exists {
		t.Fatalf("failed upload left new path %q active", newPath)
	}
	scene := presentation.Scene{Elements: []presentation.Element{{
		ID: "icon", Display: protocol.DisplayFront,
		Image: &protocol.ImageElement{Asset: protocol.AssetRef{PackagePath: upgraded.Assets[0].Source}},
	}}}
	if _, err := reconciler.ResolveScene(upgraded.PluginID, scene); !errors.Is(err, ErrAssetsNotReady) {
		t.Fatalf("ResolveScene error = %v, want ErrAssetsNotReady", err)
	}
}

func testPackage(t *testing.T, root, version, id, devicePath string, content []byte) Package {
	return testPackageWithMediaType(t, root, version, id, devicePath, content, "image/png")
}

func testPackageWithMediaType(t *testing.T, root, version, id, devicePath string, content []byte, mediaType string) Package {
	t.Helper()
	source := version + "-" + devicePath
	if err := os.WriteFile(filepath.Join(root, source), content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return Package{PluginID: "ball8", Version: version, Root: root, Enabled: true, Assets: []Declaration{{Source: source, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content)), MediaType: mediaType}}}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type fakeDevice struct {
	files        map[string][]byte
	removed      string
	uploadErr    error
	removeErr    error
	readOverride []byte
	operations   []string
}

func (d *fakeDevice) UploadFile(_ context.Context, _, path, local string) error {
	d.operations = append(d.operations, "upload:"+path)
	if d.uploadErr != nil {
		return d.uploadErr
	}
	value, err := os.ReadFile(local)
	if err == nil {
		d.files[path] = value
	}
	return err
}
func (d *fakeDevice) ReadTo(_ context.Context, path string, writer io.Writer) (int64, error) {
	trimmed := trimStoragePrefix(path)
	d.operations = append(d.operations, "read:"+trimmed)
	value := d.files[trimmed]
	if d.readOverride != nil {
		value = d.readOverride
	}
	n, err := writer.Write(value)
	return int64(n), err
}
func (d *fakeDevice) Remove(_ context.Context, path string) error {
	d.removed = path
	d.operations = append(d.operations, "remove:"+trimStoragePrefix(path))
	if d.removeErr != nil {
		return d.removeErr
	}
	delete(d.files, trimStoragePrefix(path))
	return nil
}
