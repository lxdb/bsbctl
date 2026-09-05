// Package assets verifies and reconciles plugin-owned device assets.
package assets

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	busylib "github.com/lxdb/busylib-go"
)

const (
	ApplicationName                 = "bsbctl"
	MaxAssets                       = 128
	MaxAssetBytes             int64 = 64 << 20
	MaxPackageAssetBytes      int64 = 256 << 20
	physicalContentHashLength       = 14
)

var (
	ErrAssetsNotReady    = errors.New("plugin assets are not ready")
	ErrAssetUndeclared   = errors.New("package asset is not declared")
	ErrAssetKindMismatch = errors.New("package asset media type does not match presentation kind")
)

type Declaration struct {
	Source    string `json:"source"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}

type Package struct {
	PluginID string
	Version  string
	Root     string
	Enabled  bool
	Assets   []Declaration
}

type Device interface {
	UploadFile(context.Context, string, string, string) error
	ReadTo(context.Context, string, io.Writer) (int64, error)
	Remove(context.Context, string) error
}

type Phase string

const (
	PhaseAbsent  Phase = "absent"
	PhasePending Phase = "assets_pending"
	PhaseReady   Phase = "ready"
	PhaseError   Phase = "error"
)

type State struct {
	PluginID                string    `json:"plugin_id"`
	DesiredVersion          string    `json:"desired_version,omitempty"`
	ObservedVersion         string    `json:"observed_version,omitempty"`
	Phase                   Phase     `json:"phase"`
	FilesVerified           int       `json:"files_verified"`
	BytesVerified           int64     `json:"bytes_verified"`
	ValidationInvalidations uint64    `json:"validation_invalidations"`
	RetryAt                 time.Time `json:"retry_at,omitempty"`
	LastErrorCode           string    `json:"last_error_code,omitempty"`
}

const (
	ErrorManifestInvalid    = "asset_manifest_invalid"
	ErrorDeviceUnavailable  = "device_unavailable"
	ErrorLocalVerification  = "local_asset_verification_failed"
	ErrorDeviceVerification = "device_asset_verification_failed"
)

type Reconciler struct {
	mu              sync.RWMutex
	reconcileMu     sync.Mutex
	device          Device
	states          map[string]State
	paths           map[string]map[string]string
	fingerprints    map[string][sha256.Size]byte
	observed        map[string]map[string]string
	connectionEpoch uint64
}

func NewReconciler(device Device) *Reconciler {
	return &Reconciler{
		device: device, states: make(map[string]State), paths: make(map[string]map[string]string),
		fingerprints: make(map[string][sha256.Size]byte), observed: make(map[string]map[string]string),
	}
}

// Reconcile applies desired packages serially. Device failures become pending
// state so an offline BUSY Bar does not invalidate durable enablement.
func (r *Reconciler) Reconcile(ctx context.Context, packages []Package) {
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
	for _, value := range packages {
		r.reconcile(ctx, value)
	}
}

func (r *Reconciler) reconcile(ctx context.Context, value Package) {
	r.mu.RLock()
	epoch := r.connectionEpoch
	previousState, known := r.states[value.PluginID]
	previousFingerprint := r.fingerprints[value.PluginID]
	observed := clonePaths(r.observed[value.PluginID])
	r.mu.RUnlock()
	state := State{
		PluginID: value.PluginID, DesiredVersion: value.Version, Phase: PhasePending,
		ValidationInvalidations: previousState.ValidationInvalidations,
	}
	if err := ValidatePackage(value); err != nil {
		state.Phase, state.LastErrorCode = PhaseError, ErrorManifestInvalid
		r.set(epoch, value.PluginID, state, nil)
		return
	}
	fingerprint := manifestFingerprint(value)
	if known && previousFingerprint == fingerprint && ((value.Enabled && previousState.Phase == PhaseReady) || (!value.Enabled && previousState.Phase == PhaseAbsent)) {
		return
	}
	if !value.Enabled {
		if len(observed) == 0 && len(value.Assets) == 0 {
			state.Phase = PhaseAbsent
			r.commit(epoch, value.PluginID, state, nil, nil, fingerprint)
			return
		}
		for path := range observed {
			if r.device == nil || r.device.Remove(ctx, storageAssetPath(path)) != nil {
				state.Phase, state.RetryAt, state.LastErrorCode = PhasePending, time.Now().UTC().Add(2*time.Second), ErrorDeviceUnavailable
				r.pendingObserved(epoch, value.PluginID, state, observed)
				return
			}
			delete(observed, path)
		}
		state.Phase = PhaseAbsent
		r.commit(epoch, value.PluginID, state, nil, nil, fingerprint)
		return
	}
	if len(value.Assets) == 0 && len(observed) == 0 {
		state.Phase, state.ObservedVersion = PhaseReady, value.Version
		r.commit(epoch, value.PluginID, state, nil, observed, fingerprint)
		return
	}
	if r.device == nil {
		state.RetryAt, state.LastErrorCode = time.Now().UTC().Add(2*time.Second), ErrorDeviceUnavailable
		r.pending(epoch, value.PluginID, state)
		return
	}
	if len(value.Assets) == 0 {
		obsolete := make([]string, 0, len(observed))
		for path := range observed {
			obsolete = append(obsolete, path)
		}
		slices.Sort(obsolete)
		for _, path := range obsolete {
			if err := r.device.Remove(ctx, storageAssetPath(path)); err != nil {
				state.RetryAt, state.LastErrorCode = time.Now().UTC().Add(2*time.Second), ErrorDeviceUnavailable
				r.pendingObserved(epoch, value.PluginID, state, observed)
				return
			}
			delete(observed, path)
		}
		state.Phase, state.ObservedVersion = PhaseReady, value.Version
		r.commit(epoch, value.PluginID, state, nil, observed, fingerprint)
		return
	}
	resolved := make(map[string]string, len(value.Assets))
	for _, asset := range value.Assets {
		localPath, err := verifiedLocalPath(value.Root, asset)
		if err != nil {
			state.Phase, state.LastErrorCode = PhaseError, ErrorLocalVerification
			r.pending(epoch, value.PluginID, state)
			return
		}
		devicePath, err := derivedDevicePath(value.PluginID, asset)
		if err != nil {
			state.Phase, state.LastErrorCode = PhaseError, ErrorManifestInvalid
			r.pending(epoch, value.PluginID, state)
			return
		}
		if observed[devicePath] != asset.SHA256 {
			if deviceAssetMatches(ctx, r.device, devicePath, asset) {
				observed[devicePath] = asset.SHA256
			} else if err := r.device.UploadFile(ctx, ApplicationName, devicePath, localPath); err != nil {
				state.RetryAt, state.LastErrorCode = time.Now().UTC().Add(2*time.Second), ErrorDeviceUnavailable
				r.pendingObserved(epoch, value.PluginID, state, observed)
				return
			} else if !deviceAssetMatches(ctx, r.device, devicePath, asset) {
				state.RetryAt, state.LastErrorCode = time.Now().UTC().Add(2*time.Second), ErrorDeviceVerification
				r.pendingObserved(epoch, value.PluginID, state, observed)
				return
			} else {
				observed[devicePath] = asset.SHA256
			}
		}
		resolved[asset.Source] = devicePath
		state.FilesVerified++
		state.BytesVerified += asset.Size
	}
	state.Phase, state.ObservedVersion = PhaseReady, value.Version
	r.commit(epoch, value.PluginID, state, resolved, observed, fingerprint)
}

// CollectGarbage removes obsolete content-derived files only after the daemon
// has applied the verified package's desired plugin state.
func (r *Reconciler) CollectGarbage(ctx context.Context, packages []Package) {
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
	for _, value := range packages {
		if !value.Enabled || len(value.Assets) == 0 || !r.ReadyFor(value) || r.device == nil {
			continue
		}
		desired := make(map[string]struct{}, len(value.Assets))
		for _, asset := range value.Assets {
			path, err := derivedDevicePath(value.PluginID, asset)
			if err != nil {
				continue
			}
			desired[path] = struct{}{}
		}
		r.mu.RLock()
		epoch := r.connectionEpoch
		observed := clonePaths(r.observed[value.PluginID])
		r.mu.RUnlock()
		obsolete := make([]string, 0)
		for path := range observed {
			if _, wanted := desired[path]; !wanted {
				obsolete = append(obsolete, path)
			}
		}
		slices.Sort(obsolete)
		for _, path := range obsolete {
			if err := r.device.Remove(ctx, storageAssetPath(path)); err != nil {
				r.mu.Lock()
				if r.connectionEpoch != epoch {
					r.mu.Unlock()
					break
				}
				state := r.states[value.PluginID]
				state.LastErrorCode = ErrorDeviceUnavailable
				r.states[value.PluginID] = state
				r.observed[value.PluginID] = clonePaths(observed)
				r.mu.Unlock()
				break
			}
			delete(observed, path)
			r.mu.Lock()
			if r.connectionEpoch != epoch {
				r.mu.Unlock()
				break
			}
			r.observed[value.PluginID] = clonePaths(observed)
			state := r.states[value.PluginID]
			state.LastErrorCode = ""
			r.states[value.PluginID] = state
			r.mu.Unlock()
		}
	}
}

func ValidatePackage(value Package) error {
	if strings.TrimSpace(value.PluginID) == "" || strings.TrimSpace(value.Version) == "" {
		return errors.New("plugin id and version are required")
	}
	if len(value.Assets) == 0 {
		return nil
	}
	if !filepath.IsAbs(value.Root) {
		return errors.New("asset root must be absolute")
	}
	if len(value.Assets) > MaxAssets {
		return errors.New("too many assets")
	}
	sources, paths := make(map[string]struct{}, len(value.Assets)), make(map[string]struct{}, len(value.Assets))
	var total int64
	for _, asset := range value.Assets {
		if protocol.ValidatePackagePath(asset.Source) != nil {
			return errors.New("asset source must be a safe relative path")
		}
		if _, ok := sources[asset.Source]; ok {
			return fmt.Errorf("duplicate asset source %q", asset.Source)
		}
		sources[asset.Source] = struct{}{}
		devicePath, err := derivedDevicePath(value.PluginID, asset)
		if err != nil {
			return err
		}
		if err := validatePhysicalPath(devicePath); err != nil {
			return err
		}
		if _, ok := paths[devicePath]; ok {
			return fmt.Errorf("duplicate device path %q", devicePath)
		}
		paths[devicePath] = struct{}{}
		if len(asset.SHA256) != sha256.Size*2 || !lowerHex(asset.SHA256) {
			return errors.New("invalid asset sha256")
		}
		if asset.Size < 1 || asset.Size > MaxAssetBytes {
			return errors.New("invalid asset size")
		}
		total += asset.Size
		if total > MaxPackageAssetBytes {
			return errors.New("package assets exceed size limit")
		}
	}
	return nil
}

func (r *Reconciler) Ready(pluginID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.states[pluginID].Phase == PhaseReady
}

// ReadyFor reports readiness only when the verified device mapping belongs to
// this exact package identity and asset declaration set.
func (r *Reconciler) ReadyFor(value Package) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.states[value.PluginID].Phase == PhaseReady && r.fingerprints[value.PluginID] == manifestFingerprint(value)
}

// InvalidateConnection forces package assets through device readback on the
// next reconciliation because firmware exposes no persistent storage epoch.
func (r *Reconciler) InvalidateConnection() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connectionEpoch++
	for pluginID, state := range r.states {
		if len(r.paths[pluginID]) == 0 && len(r.observed[pluginID]) == 0 {
			continue
		}
		state.Phase = PhasePending
		state.ObservedVersion = ""
		state.FilesVerified = 0
		state.BytesVerified = 0
		state.ValidationInvalidations++
		r.states[pluginID] = state
		delete(r.fingerprints, pluginID)
		for path := range r.observed[pluginID] {
			r.observed[pluginID][path] = ""
		}
	}
}
func (r *Reconciler) Status() []State {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]State, 0, len(r.states))
	for _, value := range r.states {
		result = append(result, value)
	}
	slices.SortFunc(result, func(left, right State) int { return cmp.Compare(left.PluginID, right.PluginID) })
	return result
}
func (r *Reconciler) ResolveScene(pluginID string, scene presentation.Scene) (presentation.ResolvedScene, error) {
	r.mu.RLock()
	paths := r.paths[pluginID]
	ready := r.states[pluginID].Phase == PhaseReady
	r.mu.RUnlock()
	result := presentation.ResolveScene(scene)
	for index := range result.Elements {
		asset := resolvedElementAsset(result.Elements[index].Element)
		if asset == nil {
			continue
		}
		if asset.StockName != "" {
			kind := "image"
			if result.Elements[index].Animation != nil {
				kind = "animation"
			}
			result.Elements[index].Path = StockPath(asset.StockName, kind)
			continue
		}
		if !ready {
			return presentation.ResolvedScene{}, ErrAssetsNotReady
		}
		path, ok := paths[asset.PackagePath]
		if !ok {
			return presentation.ResolvedScene{}, fmt.Errorf("%w: %q", ErrAssetUndeclared, asset.PackagePath)
		}
		kind := "image"
		if result.Elements[index].Animation != nil {
			kind = "animation"
		}
		if !packagePathMatchesKind(path, kind) {
			return presentation.ResolvedScene{}, fmt.Errorf("%w: %q cannot be used as %s", ErrAssetKindMismatch, asset.PackagePath, kind)
		}
		result.Elements[index].Path = path
	}
	return result, nil
}

func (r *Reconciler) ResolveAudioCue(pluginID string, cue presentation.AudioCue) (presentation.ResolvedAudioCue, error) {
	result := presentation.ResolveAudioCue(cue)
	if result.Asset.StockName != "" {
		result.Path = StockPath(result.Asset.StockName, "audio")
		return result, nil
	}
	return presentation.ResolvedAudioCue{}, fmt.Errorf("%w: package audio is unavailable", ErrAssetKindMismatch)
}

// StockPath translates a validated firmware basename according to its element
// kind. Firmware remains responsible for lookup and fallback within that tree.
func StockPath(name, kind string) string {
	switch kind {
	case "animation":
		return "shared/animations/" + name
	case "audio":
		return "shared/sounds/" + name
	default:
		return "shared/images/external/" + name
	}
}

func resolvedElementAsset(element protocol.Element) *protocol.AssetRef {
	if element.Image != nil {
		return &element.Image.Asset
	}
	if element.Animation != nil {
		return &element.Animation.Asset
	}
	return nil
}

func packagePathMatchesKind(path, kind string) bool {
	extension := filepath.Ext(path)
	switch kind {
	case "image":
		return extension == ".png"
	case "animation":
		return extension == ".anim"
	default:
		return false
	}
}

func (r *Reconciler) set(epoch uint64, pluginID string, state State, paths map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.connectionEpoch != epoch {
		return
	}
	r.states[pluginID] = state
	if paths == nil {
		delete(r.paths, pluginID)
	} else {
		r.paths[pluginID] = paths
	}
}

func (r *Reconciler) pending(epoch uint64, pluginID string, state State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.connectionEpoch != epoch {
		return
	}
	r.states[pluginID] = state
}

func (r *Reconciler) pendingObserved(epoch uint64, pluginID string, state State, observed map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.connectionEpoch != epoch {
		return
	}
	r.states[pluginID] = state
	r.observed[pluginID] = clonePaths(observed)
}

func (r *Reconciler) commit(epoch uint64, pluginID string, state State, paths, observed map[string]string, fingerprint [sha256.Size]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.connectionEpoch != epoch {
		return
	}
	r.states[pluginID] = state
	if paths == nil {
		delete(r.paths, pluginID)
	} else {
		r.paths[pluginID] = clonePaths(paths)
	}
	if observed == nil {
		delete(r.observed, pluginID)
	} else {
		r.observed[pluginID] = clonePaths(observed)
	}
	r.fingerprints[pluginID] = fingerprint
}

func clonePaths(source map[string]string) map[string]string {
	if source == nil {
		return make(map[string]string)
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func manifestFingerprint(value Package) [sha256.Size]byte {
	// Package contains only deterministic scalar fields and an ordered asset
	// declaration slice. JSON provides an unambiguous length-delimited encoding.
	payload, _ := json.Marshal(value)
	return sha256.Sum256(payload)
}

func verifiedLocalPath(root string, asset Declaration) (string, error) {
	path := filepath.Join(root, filepath.FromSlash(asset.Source))
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("asset escapes root")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != asset.Size {
		return "", errors.New("asset is not the declared regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, MaxAssetBytes+1))
	if err != nil || n != asset.Size || hex.EncodeToString(hash.Sum(nil)) != asset.SHA256 {
		return "", errors.New("asset digest mismatch")
	}
	return path, nil
}

func derivedDevicePath(pluginID string, asset Declaration) (string, error) {
	extension := map[string]string{
		"image/png":                    ".png",
		"application/x-busy-animation": ".anim",
	}[asset.MediaType]
	if extension == "" {
		return "", fmt.Errorf("unsupported package asset media_type %q", asset.MediaType)
	}
	digest, err := hex.DecodeString(asset.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return "", errors.New("invalid asset sha256")
	}
	pluginHash := pluginPathPrefix(pluginID)
	contentHash := base64.RawURLEncoding.EncodeToString(digest)[:physicalContentHashLength]
	return "p" + pluginHash + "_" + contentHash + extension, nil
}

func validatePhysicalPath(path string) error {
	body := busylib.BytesBody([]byte{0}, "application/octet-stream")
	if err := (busylib.UploadAssetRequest{ApplicationName: ApplicationName, File: path, Body: body}).Validate(); err != nil {
		return fmt.Errorf("device asset upload path: %w", err)
	}
	if err := (busylib.WriteStorageFileRequest{Path: storageAssetPath(path), Body: body}).Validate(); err != nil {
		return fmt.Errorf("device asset storage path: %w", err)
	}
	return nil
}

// ValidatePluginHashCollisions rejects two configured plugin IDs that would
// share the truncated physical asset path prefix.
func ValidatePluginHashCollisions(pluginIDs []string) error {
	return validatePluginHashCollisions(pluginIDs, pluginPathPrefix)
}

func validatePluginHashCollisions(pluginIDs []string, prefixFor func(string) string) error {
	seen := make(map[string]string, len(pluginIDs))
	for _, pluginID := range pluginIDs {
		prefix := prefixFor(pluginID)
		if previous, exists := seen[prefix]; exists && previous != pluginID {
			return fmt.Errorf("plugin ids %q and %q share asset path prefix %q", previous, pluginID, prefix)
		}
		seen[prefix] = pluginID
	}
	return nil
}

func pluginPathPrefix(pluginID string) string {
	digest := sha256.Sum256([]byte(pluginID))
	return base64.RawURLEncoding.EncodeToString(digest[:])[:10]
}

func deviceAssetMatches(ctx context.Context, device Device, path string, asset Declaration) bool {
	hash := sha256.New()
	n, err := device.ReadTo(ctx, storageAssetPath(path), hash)
	return err == nil && n == asset.Size && hex.EncodeToString(hash.Sum(nil)) == asset.SHA256
}

func storageAssetPath(path string) string { return "/ext/user_assets/" + ApplicationName + "/" + path }
func lowerHex(value string) bool {
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
