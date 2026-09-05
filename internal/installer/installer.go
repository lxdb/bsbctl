package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/localstate"
)

type Activator interface {
	DesiredPlugin(context.Context, string) (*config.Plugin, error)
	ActivatePlugin(context.Context, config.Plugin) (localstate.CommitOutcome, error)
}

type stateBackend interface {
	LoadState() (InstallState, error)
	WriteState(InstallState) (localstate.CommitOutcome, error)
	LoadIntent() (*Intent, error)
	WriteIntent(Intent) (localstate.CommitOutcome, error)
	ClearIntent() (localstate.CommitOutcome, error)
}

type Options struct {
	Root      string
	Keyring   catalog.Keyring
	Client    *http.Client
	Timeout   time.Duration
	Activator Activator
	Now       func() time.Time
	State     stateBackend
}

// RecoveryOptions contains only the dependencies needed to reconcile an
// interrupted installer transaction before the live runtime exists.
type RecoveryOptions struct {
	Root      string
	Activator Activator
	State     stateBackend
}

type Installer struct {
	mu         sync.Mutex
	root       string
	keyring    catalog.Keyring
	downloader Downloader
	activator  Activator
	now        func() time.Time
	state      stateBackend
	packages   *PackageStore
}

type InstallRequest struct {
	Catalog  []byte
	Envelope []byte
	PluginID string
	Version  string
	OS       string
	Arch     string
}

type RollbackRequest struct {
	PluginID string
	Version  string
	OS       string
	Arch     string
}

type Status string

const (
	StatusInstalled       Status = "installed"
	StatusUpdated         Status = "updated"
	StatusRolledBack      Status = "rolled_back"
	StatusRecoveredTarget Status = "recovered_target"
	StatusRecoveredPrior  Status = "recovered_prior"
	StatusNoRecovery      Status = "no_recovery"
)

type Result struct {
	Status            Status                   `json:"status"`
	Release           ReleaseRef               `json:"release"`
	Promotion         Promotion                `json:"promotion,omitempty"`
	IntentOutcome     localstate.CommitOutcome `json:"intent_outcome,omitempty"`
	ActivationOutcome localstate.CommitOutcome `json:"activation_outcome,omitempty"`
	StateOutcome      localstate.CommitOutcome `json:"state_outcome,omitempty"`
	CleanupOutcome    localstate.CommitOutcome `json:"cleanup_outcome,omitempty"`
}

type PluginSnapshot struct {
	PluginID          string      `json:"plugin_id"`
	Configured        bool        `json:"configured"`
	ConfiguredSource  string      `json:"configured_source,omitempty"`
	ConfiguredVersion string      `json:"configured_version,omitempty"`
	AppCount          int         `json:"app_count"`
	Active            *ReleaseRef `json:"active,omitempty"`
	Previous          *ReleaseRef `json:"previous,omitempty"`
}

type Snapshot struct {
	CatalogSequence  uint64           `json:"catalog_sequence"`
	RecoveryRequired bool             `json:"recovery_required"`
	Plugins          []PluginSnapshot `json:"plugins"`
}

func New(options Options) (*Installer, error) {
	if !filepath.IsAbs(options.Root) || options.Activator == nil {
		return nil, errorCode(CodeStateFailed)
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.State == nil {
		options.State = NewStateStore(options.Root)
	}
	return &Installer{
		root: options.Root, keyring: cloneKeyring(options.Keyring), downloader: Downloader{Client: options.Client, Timeout: options.Timeout},
		activator: options.Activator, now: options.Now, state: options.State, packages: NewPackageStore(options.Root),
	}, nil
}

func (installer *Installer) Install(ctx context.Context, request InstallRequest) (Result, error) {
	return installer.install(ctx, request, installAny)
}

// InstallFirst is the public first-install operation. An identical active
// target is idempotent; a different active target is a conflict.
func (installer *Installer) InstallFirst(ctx context.Context, request InstallRequest) (Result, error) {
	return installer.install(ctx, request, installFirst)
}

// Update is the public update operation. It requires an existing active
// release and treats a repeated completed update as idempotent.
func (installer *Installer) Update(ctx context.Context, request InstallRequest) (Result, error) {
	return installer.install(ctx, request, installUpdate)
}

type installMode uint8

const (
	installAny installMode = iota
	installFirst
	installUpdate
)

func (installer *Installer) install(ctx context.Context, request InstallRequest, mode installMode) (Result, error) {
	installer.mu.Lock()
	defer installer.mu.Unlock()
	if intent, err := installer.state.LoadIntent(); err != nil {
		return Result{}, err
	} else if intent != nil {
		return Result{}, errorCode(CodeRecoveryRequired)
	}
	state, err := installer.state.LoadState()
	if err != nil {
		return Result{}, err
	}
	target := ReleaseRef{ID: request.PluginID, Version: request.Version, OS: request.OS, Arch: request.Arch}
	if !validReleaseIdentity(target.ID, target.Version, target.OS, target.Arch) {
		return Result{}, errorCode(CodeCatalogInvalid)
	}
	activeState := state.Plugins[request.PluginID]
	if activeState.Active != nil && *activeState.Active == target {
		activeRelease, exists := state.Installed[target.Key()]
		if !exists || activeRelease.Ref() != target {
			return Result{}, errorCode(CodePackageInvalid)
		}
		activeRelease.Root = installer.packages.releaseRoot(target)
		if err := installer.packages.VerifyInstalled(activeRelease); err != nil {
			return Result{}, errorCode(CodePackageInvalid)
		}
		if mode == installUpdate && activeState.Previous == nil {
			return Result{}, errorCode(CodeInstallConflict)
		}
		status := StatusInstalled
		if mode == installUpdate || (mode == installAny && activeState.Previous != nil) {
			status = StatusUpdated
		}
		return Result{Status: status, Release: target, Promotion: PromotionAlreadyInstalled}, nil
	}
	if mode == installFirst && activeState.Active != nil {
		return Result{}, errorCode(CodeInstallConflict)
	}
	if mode == installUpdate && activeState.Active == nil {
		return Result{}, errorCode(CodeNotInstalled)
	}
	catalogDigest := sha256.Sum256(request.Catalog)
	catalogSHA256 := hex.EncodeToString(catalogDigest[:])
	lastSequence := state.CatalogSequence
	if lastSequence > 0 && state.CatalogSHA256 == catalogSHA256 {
		lastSequence--
	}
	verifiedCatalog, err := catalog.Verify(request.Catalog, request.Envelope, installer.keyring, lastSequence, installer.now().UTC())
	if err != nil {
		return Result{}, errorCode(CodeCatalogInvalid)
	}
	entry, err := verifiedCatalog.Resolve(request.PluginID, request.Version, request.OS, request.Arch)
	if err != nil {
		return Result{}, errorCode(CodeCatalogInvalid)
	}
	staging, err := installer.packages.StagingDir()
	if err != nil {
		return Result{}, err
	}
	artifact, err := installer.downloader.Download(ctx, staging, entry)
	if err != nil {
		return Result{}, err
	}
	defer artifact.Close()
	verified, err := ExtractAndVerifyFile(artifact, staging, entry)
	if err != nil {
		return Result{}, err
	}
	release, promotion, err := installer.packages.Promote(verified)
	if err != nil {
		return Result{Release: release.Ref(), Promotion: promotion}, err
	}
	status := StatusInstalled
	if mode == installUpdate || (mode == installAny && state.Plugins[release.ID].Active != nil && *state.Plugins[release.ID].Active != release.Ref()) {
		status = StatusUpdated
	}
	result := Result{Status: status, Release: release.Ref(), Promotion: promotion}
	return installer.transact(ctx, state, release, OperationInstall, verifiedCatalog.Sequence, catalogSHA256, result)
}

// Snapshot returns only installer identities and transaction state; package
// roots, catalog URLs, and dependency errors never cross this boundary.
func (installer *Installer) Snapshot(ctx context.Context, pluginID string) (Snapshot, error) {
	installer.mu.Lock()
	defer installer.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	intent, err := installer.state.LoadIntent()
	if err != nil {
		return Snapshot{}, err
	}
	state, err := installer.state.LoadState()
	if err != nil {
		return Snapshot{}, err
	}
	ids := make([]string, 0, len(state.Plugins))
	for id := range state.Plugins {
		if pluginID == "" || id == pluginID {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	result := Snapshot{CatalogSequence: state.CatalogSequence, RecoveryRequired: intent != nil, Plugins: make([]PluginSnapshot, 0, len(ids))}
	for _, id := range ids {
		plugin := state.Plugins[id]
		result.Plugins = append(result.Plugins, PluginSnapshot{PluginID: id, Active: cloneRef(plugin.Active), Previous: cloneRef(plugin.Previous)})
	}
	return result, nil
}

func (installer *Installer) Rollback(ctx context.Context, request RollbackRequest) (Result, error) {
	installer.mu.Lock()
	defer installer.mu.Unlock()
	if intent, err := installer.state.LoadIntent(); err != nil {
		return Result{}, err
	} else if intent != nil {
		return Result{}, errorCode(CodeRecoveryRequired)
	}
	state, err := installer.state.LoadState()
	if err != nil {
		return Result{}, err
	}
	plugin, exists := state.Plugins[request.PluginID]
	if !exists || plugin.Active == nil {
		return Result{}, errorCode(CodeNotInstalled)
	}
	var targetRef *ReleaseRef
	if request.Version == "" {
		targetRef = plugin.Previous
	} else {
		goos, goarch := request.OS, request.Arch
		if goos == "" {
			goos = plugin.Active.OS
		}
		if goarch == "" {
			goarch = plugin.Active.Arch
		}
		value := ReleaseRef{ID: request.PluginID, Version: request.Version, OS: goos, Arch: goarch}
		targetRef = &value
	}
	if targetRef == nil || *targetRef == *plugin.Active {
		return Result{}, errorCode(CodeNotInstalled)
	}
	release, exists := state.Installed[targetRef.Key()]
	if !exists || installer.packages.VerifyInstalled(release) != nil {
		return Result{}, errorCode(CodeNotInstalled)
	}
	release.Root = installer.packages.releaseRoot(release.Ref())
	result := Result{Status: StatusRolledBack, Release: release.Ref(), Promotion: PromotionAlreadyInstalled}
	return installer.transact(ctx, state, release, OperationRollback, state.CatalogSequence, state.CatalogSHA256, result)
}

func (installer *Installer) transact(ctx context.Context, state InstallState, target InstalledRelease, operation Operation, catalogSequence uint64, catalogSHA256 string, result Result) (Result, error) {
	before := clonePluginInstallState(state.Plugins[target.ID])
	_, targetWasInstalled := state.Installed[target.Ref().Key()]
	intent := Intent{
		Version: 1, Kind: operation, PluginID: target.ID, Target: target, Before: before,
		BeforeCatalogSequence: state.CatalogSequence, BeforeCatalogSHA256: state.CatalogSHA256,
		TargetWasInstalled: targetWasInstalled, CatalogSequence: catalogSequence, CatalogSHA256: catalogSHA256,
	}
	next := applyTargetState(state, intent)
	if validateIntent(intent) != nil || validateSerializedDocument(intent) != nil || validateInstallState(next) != nil || validateSerializedDocument(next) != nil {
		return result, errorCode(CodeStateFailed)
	}
	intentOutcome, intentErr := installer.state.WriteIntent(intent)
	result.IntentOutcome = intentOutcome
	if intentErr != nil || !result.IntentOutcome.IsCommitted() {
		return result, errorCode(CodeStateFailed)
	}
	plugin := configFromRelease(target)
	activationOutcome, activationErr := installer.activator.ActivatePlugin(ctx, plugin)
	result.ActivationOutcome = activationOutcome
	if activationErr != nil || !activationOutcome.IsCommitted() {
		return result, errorCode(CodeActivationFailed)
	}
	stateOutcome, stateErr := installer.state.WriteState(next)
	result.StateOutcome = stateOutcome
	if stateErr != nil || !stateOutcome.IsCommitted() {
		return result, errorCode(CodeStateFailed)
	}
	cleanupOutcome, cleanupErr := installer.state.ClearIntent()
	result.CleanupOutcome = cleanupOutcome
	if cleanupErr != nil {
		return result, errorCode(CodeStateFailed)
	}
	return result, nil
}

// Recover reconciles one interrupted transaction using a short-lived recovery
// object. The returned object cannot be reused for live package operations.
func Recover(ctx context.Context, options RecoveryOptions) (Result, error) {
	if !filepath.IsAbs(options.Root) || options.Activator == nil {
		return Result{}, errorCode(CodeStateFailed)
	}
	if options.State == nil {
		options.State = NewStateStore(options.Root)
	}
	value := &Installer{
		root:      options.Root,
		activator: options.Activator,
		state:     options.State,
		packages:  NewPackageStore(options.Root),
	}
	return value.recover(ctx)
}

func (installer *Installer) recover(ctx context.Context) (Result, error) {
	installer.mu.Lock()
	defer installer.mu.Unlock()
	intent, err := installer.state.LoadIntent()
	if err != nil {
		return Result{}, err
	}
	if intent == nil {
		return Result{Status: StatusNoRecovery}, nil
	}
	state, err := installer.state.LoadState()
	if err != nil {
		return Result{}, err
	}
	target := intent.Target
	target.Root = installer.packages.releaseRoot(target.Ref())
	desired, err := installer.activator.DesiredPlugin(ctx, intent.PluginID)
	if err != nil {
		return Result{}, errorCode(CodeRecoveryRequired)
	}
	targetConfig := configFromRelease(target)
	var next InstallState
	status := StatusRecoveredTarget
	if desired != nil && equalConfigPlugin(*desired, targetConfig) {
		if installer.packages.VerifyInstalled(target) != nil {
			return Result{}, errorCode(CodeRecoveryRequired)
		}
		copyIntent := *intent
		copyIntent.Target = target
		next = applyTargetState(state, copyIntent)
	} else {
		prior, priorState := priorConfig(state, *intent, installer.packages)
		if (desired == nil && priorState == priorAbsent) || (desired != nil && priorState == priorVerified && equalConfigPlugin(*desired, prior)) {
			next = restorePriorState(state, *intent)
			status = StatusRecoveredPrior
		} else {
			return Result{}, errorCode(CodeRecoveryRequired)
		}
	}
	result := Result{Status: status, Release: target.Ref()}
	result.StateOutcome, err = installer.state.WriteState(next)
	if err != nil || !result.StateOutcome.IsCommitted() {
		return result, errorCode(CodeStateFailed)
	}
	result.CleanupOutcome, err = installer.state.ClearIntent()
	if err != nil {
		return result, errorCode(CodeStateFailed)
	}
	return result, nil
}

func applyTargetState(state InstallState, intent Intent) InstallState {
	next := cloneInstallState(state)
	target := intent.Target
	target.Root = ""
	ref := target.Ref()
	next.Installed[ref.Key()] = target
	plugin := PluginInstallState{Active: cloneRef(&ref)}
	if intent.Before.Active != nil && *intent.Before.Active != ref {
		plugin.Previous = cloneRef(intent.Before.Active)
	} else {
		plugin.Previous = cloneRef(intent.Before.Previous)
	}
	next.Plugins[target.ID] = plugin
	if intent.Kind == OperationInstall {
		next.CatalogSequence = intent.CatalogSequence
		next.CatalogSHA256 = intent.CatalogSHA256
	}
	return next
}

func restorePriorState(state InstallState, intent Intent) InstallState {
	next := cloneInstallState(state)
	next.CatalogSequence = intent.BeforeCatalogSequence
	next.CatalogSHA256 = intent.BeforeCatalogSHA256
	if intent.Before.Active == nil && intent.Before.Previous == nil {
		delete(next.Plugins, intent.PluginID)
	} else {
		next.Plugins[intent.PluginID] = clonePluginInstallState(intent.Before)
	}
	if !intent.TargetWasInstalled {
		delete(next.Installed, intent.Target.Ref().Key())
	}
	return next
}

type priorResolution uint8

const (
	priorAbsent priorResolution = iota
	priorVerified
	priorInvalid
)

func priorConfig(state InstallState, intent Intent, packages *PackageStore) (config.Plugin, priorResolution) {
	if intent.Before.Active == nil {
		return config.Plugin{}, priorAbsent
	}
	release, exists := state.Installed[intent.Before.Active.Key()]
	if !exists {
		return config.Plugin{}, priorInvalid
	}
	release.Root = packages.releaseRoot(release.Ref())
	if packages.VerifyInstalled(release) != nil {
		return config.Plugin{}, priorInvalid
	}
	return configFromRelease(release), priorVerified
}

func configFromRelease(release InstalledRelease) config.Plugin {
	manifest := release.Manifest
	return config.Plugin{
		ID: release.ID, Version: release.Version, Executable: filepath.Join(release.Root, filepath.FromSlash(manifest.Executable)),
		ProtocolVersion: manifest.ProtocolVersion,
		SHA256:          manifest.ExecutableSHA256, ExecutionModes: slices.Clone(manifest.ExecutionModes),
		Channels:     slices.Clone(manifest.Channels),
		Operations:   slices.Clone(manifest.Operations),
		ConfigSchema: cloneConfigSchema(manifest.ConfigSchema),
		PackageRoot:  release.Root, Assets: append([]assets.Declaration{}, manifest.Assets...),
	}
}

func cloneConfigSchema(source *catalog.ConfigSchemaDeclaration) *catalog.ConfigSchemaDeclaration {
	if source == nil {
		return nil
	}
	copy := *source
	return &copy
}

func equalConfigPlugin(left, right config.Plugin) bool { return reflect.DeepEqual(left, right) }

func cloneInstallState(state InstallState) InstallState {
	result := state
	result.Installed = make(map[string]InstalledRelease, len(state.Installed))
	for key, release := range state.Installed {
		release.Files = cloneRecords(release.Files)
		result.Installed[key] = release
	}
	result.Plugins = make(map[string]PluginInstallState, len(state.Plugins))
	for key, plugin := range state.Plugins {
		result.Plugins[key] = clonePluginInstallState(plugin)
	}
	return result
}

func clonePluginInstallState(state PluginInstallState) PluginInstallState {
	return PluginInstallState{Active: cloneRef(state.Active), Previous: cloneRef(state.Previous)}
}

func cloneRef(ref *ReleaseRef) *ReleaseRef {
	if ref == nil {
		return nil
	}
	copy := *ref
	return &copy
}

func cloneKeyring(source catalog.Keyring) catalog.Keyring {
	result := make(catalog.Keyring, len(source))
	for keyID, key := range source {
		result[keyID] = slices.Clone(key)
	}
	return result
}
