package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/cliinput"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/installer"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/releasekeys"
	"github.com/lxdb/bsbctl/internal/secrets"
	busylib "github.com/lxdb/busylib-go"
)

const setupReleaseBaseURL = "https://github.com/lxdb/bsbctl/releases/download"

const (
	defaultSetupTokenReference = "keychain://bsbctl/device/access-token"
	setupTokenNamePrefix       = "bsbctl-"
)

var setupStableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var setupAccessToken = regexp.MustCompile(`^[A-Za-z0-9]{32}$`)
var setupAccessTokenShortID = regexp.MustCompile(`^[A-Za-z0-9]{8}$`)

var (
	setupHTTPClient     = http.DefaultClient
	setupCatalogKeyring = releasekeys.CatalogKeyring
	setupNow            = time.Now
)

var setupHasTerminal = func(reader io.Reader) bool {
	if owned, ok := reader.(*cliinput.Reader); ok {
		reader = owned.File()
	}
	file, ok := reader.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

type setupConfigurationResult struct {
	DeviceToken     *setupTokenResult
	Changed         bool
	RestartRequired bool
}

type setupDependencies struct {
	loadCatalog   func(context.Context) (setupCatalog, error)
	configure     func(context.Context, string, string, string) (setupConfigurationResult, error)
	service       func(context.Context, []string, io.Writer, io.Writer) error
	waitStatus    func(context.Context, time.Duration) (control.Status, error)
	reconcileApps func(context.Context, []firstpartyplugins.Descriptor, setupCatalog, control.Status, io.Writer) ([]setupAppResult, error)
}

func runSetup(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runSetupWithDependencies(ctx, args, stdin, stdout, stderr, productionSetupDependencies())
}

func productionSetupDependencies() setupDependencies {
	tokenDependencies := productionSetupTokenDependencies()
	return setupDependencies{
		loadCatalog: loadSetupCatalog,
		configure: func(ctx context.Context, deviceURL, bootstrapReference, tokenReference string) (setupConfigurationResult, error) {
			return ensureSetupConfiguration(ctx, deviceURL, bootstrapReference, tokenReference, tokenDependencies)
		},
		service:       runService,
		waitStatus:    waitSetupStatus,
		reconcileApps: reconcileSetupApps,
	}
}

func runSetupWithDependencies(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, dependencies setupDependencies) error {
	options, positionals, err := parseOptions(args, "apps", "device-url", "device-bootstrap-keychain", "device-token-keychain")
	if err != nil || len(positionals) != 0 {
		return commandFailure(exitUsage, "invalid setup flags")
	}
	var selected []firstpartyplugins.Descriptor
	if rawApps := options["apps"]; rawApps != "" {
		selected, err = explicitSetupApps(rawApps)
		if err != nil {
			return commandFailure(exitUsage, "setup app selection is invalid")
		}
	} else if !setupHasTerminal(stdin) {
		return commandFailure(exitUsage, "setup requires a terminal or --apps")
	} else {
		selected, err = promptSetupApps(stdin, stderr)
		if err != nil {
			return commandFailure(exitUsage, "setup app selection is invalid")
		}
	}
	var release setupCatalog
	if len(selected) != 0 {
		release, err = dependencies.loadCatalog(ctx)
		if err != nil {
			return commandFailure(exitOperational, "setup release catalog is unavailable or invalid")
		}
	}
	configuration, err := dependencies.configure(
		ctx,
		options["device-url"],
		options["device-bootstrap-keychain"],
		options["device-token-keychain"],
	)
	result := setupResult{Status: "configured", Version: version, Apps: []setupAppResult{}, DeviceToken: configuration.DeviceToken}
	if err != nil {
		if configuration.RestartRequired {
			if serviceErr := dependencies.service(ctx, []string{"install"}, io.Discard, stderr); serviceErr != nil {
				return finishSetupRun(stdout, result, configuration.Changed, serviceErr, "setup configuration changed but service installation failed")
			}
			if serviceErr := dependencies.service(ctx, []string{"restart"}, io.Discard, stderr); serviceErr != nil {
				return finishSetupRun(stdout, result, configuration.Changed, serviceErr, "setup service restart failed after configuration changed")
			}
		}
		return finishSetupRun(stdout, result, configuration.Changed, err, "setup configuration partially completed")
	}
	changed := configuration.Changed
	if err := dependencies.service(ctx, []string{"install"}, io.Discard, stderr); err != nil {
		return finishSetupRun(stdout, result, changed, err, "setup configuration was saved but service installation failed")
	}
	changed = true
	// Installing an identical plist does not re-execute a binary replaced at the
	// same path, including a new build that still reports the same version.
	if err := dependencies.service(ctx, []string{"restart"}, io.Discard, stderr); err != nil {
		return finishSetupRun(stdout, result, changed, err, "setup service restart failed")
	}
	status, err := dependencies.waitStatus(ctx, 5*time.Second)
	if err == nil && status.Version != version {
		err = errors.New("setup daemon version does not match installed CLI")
	}
	if err != nil {
		return finishSetupRun(stdout, result, changed, err, "setup service state changed but readiness is unknown")
	}
	if len(selected) != 0 {
		apps, err := dependencies.reconcileApps(ctx, selected, release, status, stderr)
		if apps != nil {
			result.Apps = apps
		}
		changed = changed || setupAppsChanged(apps)
		if err != nil {
			return finishSetupRun(stdout, result, changed, err, "setup partially completed; inspect app, plugin, and device-token status")
		}
		status, err = dependencies.waitStatus(ctx, 5*time.Second)
		if err == nil && status.Version != version {
			err = errors.New("setup daemon version does not match installed CLI")
		}
		if err != nil {
			return finishSetupRun(stdout, result, changed, err, "setup app state changed but readiness is unknown")
		}
	}
	if status.Device.Phase != device.PhaseReady {
		result.Warnings = []string{"BUSY Bar device is not ready"}
	}
	return writeJSON(stdout, result)
}

func finishSetupRun(stdout io.Writer, result setupResult, changed bool, err error, message string) error {
	code, _ := classifyCommandError(err)
	if !changed && code != exitPartial {
		return err
	}
	result.Status = "partial"
	if writeErr := writeJSON(stdout, result); writeErr != nil {
		return writeErr
	}
	if code == exitPartial {
		return err
	}
	return commandFailure(exitPartial, message)
}

func setupTokenChanged(result *setupTokenResult) bool {
	return result != nil && result.Status != "existing"
}

func setupAppsChanged(results []setupAppResult) bool {
	for _, result := range results {
		if result.PluginStatus != "unchanged" || result.AppStatus == "created" || result.AppStatus == "unknown" {
			return true
		}
	}
	return false
}

func reconcileSetupApps(ctx context.Context, selected []firstpartyplugins.Descriptor, release setupCatalog, status control.Status, stderr io.Writer) ([]setupAppResult, error) {
	directory, err := os.MkdirTemp("", "bsbctl-setup-")
	if err != nil {
		return nil, commandFailure(exitOperational, "create setup workspace failed")
	}
	defer os.RemoveAll(directory)
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, commandFailure(exitOperational, "secure setup workspace failed")
	}
	catalogPath := filepath.Join(directory, "catalog.json")
	signaturePath := filepath.Join(directory, "catalog.sig")
	if err := os.WriteFile(catalogPath, release.Data, 0o600); err != nil {
		return nil, commandFailure(exitOperational, "write setup catalog failed")
	}
	if err := os.WriteFile(signaturePath, release.Signature, 0o600); err != nil {
		return nil, commandFailure(exitOperational, "write setup signature failed")
	}
	catalogDigest := fmt.Sprintf("%x", sha256.Sum256(release.Data))
	signatureDigest := fmt.Sprintf("%x", sha256.Sum256(release.Signature))

	var snapshot installer.Snapshot
	if err := callDaemon(ctx, defaultSocketPath(), "plugin.status", control.CatalogStatusRequest{}, &snapshot); err != nil {
		return nil, err
	}
	if snapshot.RecoveryRequired {
		return nil, commandFailure(exitPartial, "installer recovery is required")
	}
	plugins := make(map[string]installer.PluginSnapshot, len(snapshot.Plugins))
	for _, plugin := range snapshot.Plugins {
		plugins[plugin.PluginID] = plugin
	}
	apps := make(map[string]control.AppStatus, len(status.Apps))
	for _, app := range status.Apps {
		apps[app.AppID] = app
	}
	for _, descriptor := range selected {
		plugin := plugins[descriptor.ID]
		if plugin.Active == nil && plugin.Configured {
			return nil, commandFailure(exitRejected, "setup will not replace an unmanaged plugin package")
		}
		if app, exists := apps[descriptor.DefaultApp.ID]; exists && app.PluginID != descriptor.ID {
			return nil, commandFailure(exitRejected, "setup app ID conflicts with another plugin")
		}
	}

	results := make([]setupAppResult, 0, len(selected))
	mutationConfirmed := false
	for _, descriptor := range selected {
		entry := release.Entries[descriptor.ID]
		_, appExists := apps[descriptor.DefaultApp.ID]
		appStatus := "pending"
		if appExists {
			appStatus = "existing"
		}
		result := setupAppResult{
			AppID: descriptor.DefaultApp.ID, PluginID: descriptor.ID, Version: entry.Version,
			PluginStatus: "unchanged", AppStatus: appStatus,
		}
		plugin := plugins[descriptor.ID]
		method := ""
		pluginSuccessStatus := ""
		if plugin.Active == nil {
			method = "plugin.install"
			result.PluginStatus = "pending"
			pluginSuccessStatus = "installed"
		} else if plugin.Active.Version != entry.Version || plugin.Active.OS != runtime.GOOS || plugin.Active.Arch != runtime.GOARCH {
			method = "plugin.update"
			result.PluginStatus = "pending"
			pluginSuccessStatus = "updated"
		}
		if method != "" {
			request := control.CatalogInstallRequest{
				CatalogPath: catalogPath, SignaturePath: signaturePath,
				CatalogSHA256: catalogDigest, SignatureSHA256: signatureDigest,
				PluginID: descriptor.ID, Version: entry.Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
			}
			var response control.CatalogOperationResponse
			if err := callDaemon(ctx, defaultSocketPath(), method, request, &response); err != nil {
				result.PluginStatus = "unknown"
				return finishSetupFailure(results, result, mutationConfirmed, err)
			}
			if err := finishCatalogOperation(io.Discard, response); err != nil {
				result.PluginStatus = "unknown"
				return finishSetupFailure(results, result, mutationConfirmed, err)
			}
			result.PluginStatus = pluginSuccessStatus
			mutationConfirmed = true
			ref := response.Result.Release
			plugin.Active = &ref
			plugins[descriptor.ID] = plugin
		}

		if !appExists {
			app := descriptor.DefaultApp
			request := control.CreateAppRequest{
				AppID: app.ID, PluginID: app.PluginID, Enabled: app.Enabled, Config: app.Config,
				Secrets: app.Secrets, Policies: app.Policies, LaunchAction: app.LaunchAction,
			}
			var response control.AppInstanceResult
			closeWarning, callErr := callDaemonResult(ctx, defaultSocketPath(), "app.create", request, &response)
			if err := finishAppInstanceMutation(io.Discard, stderr, response, closeWarning, callErr); err != nil {
				result.AppStatus = "unknown"
				return finishSetupFailure(results, result, mutationConfirmed, err)
			}
			apps[app.ID] = control.AppStatus{AppID: app.ID, PluginID: app.PluginID, Enabled: app.Enabled}
			result.AppStatus = "created"
			mutationConfirmed = true
		}
		results = append(results, result)
	}
	return results, nil
}

func finishSetupFailure(results []setupAppResult, current setupAppResult, mutationConfirmed bool, err error) ([]setupAppResult, error) {
	if !mutationConfirmed {
		return nil, err
	}
	return append(results, current), commandFailure(exitPartial, "setup partially completed; inspect app and plugin status")
}

func defaultSocketPath() string {
	path, _ := resolveStatePath(nil, "socket", "ctl.sock")
	return path
}

type setupCatalog struct {
	Data      []byte
	Signature []byte
	Entries   map[string]catalog.Entry
}

func loadSetupCatalog(ctx context.Context) (setupCatalog, error) {
	if runtime.GOOS != "darwin" || (runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64") || !setupStableVersion.MatchString(version) {
		return setupCatalog{}, errors.New("setup release identity is unsupported")
	}
	downloadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := downloadSetupAsset(downloadCtx, "catalog.json", 1<<20)
	if err != nil {
		return setupCatalog{}, err
	}
	signature, err := downloadSetupAsset(downloadCtx, "catalog.sig", 16<<10)
	if err != nil {
		return setupCatalog{}, err
	}
	keyring, err := setupCatalogKeyring()
	if err != nil {
		return setupCatalog{}, err
	}
	document, err := catalog.Verify(data, signature, keyring, 0, setupNow().UTC())
	if err != nil {
		return setupCatalog{}, err
	}
	entries := make(map[string]catalog.Entry, len(firstpartyplugins.All()))
	for _, descriptor := range firstpartyplugins.All() {
		for _, entry := range document.Plugins {
			if entry.ID != descriptor.ID || entry.OS != runtime.GOOS || entry.Arch != runtime.GOARCH {
				continue
			}
			if _, duplicate := entries[descriptor.ID]; duplicate {
				return setupCatalog{}, errors.New("setup catalog repeats a first-party plugin")
			}
			entries[descriptor.ID] = entry
		}
		if _, exists := entries[descriptor.ID]; !exists {
			return setupCatalog{}, errors.New("setup catalog omits a first-party plugin")
		}
	}
	return setupCatalog{Data: data, Signature: signature, Entries: entries}, nil
}

func downloadSetupAsset(ctx context.Context, name string, limit int64) ([]byte, error) {
	endpoint := setupReleaseBaseURL + "/v" + version + "/" + name
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	response, err := setupHTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > limit {
		return nil, errors.New("setup release asset is unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		return nil, errors.New("setup release asset is invalid")
	}
	return data, nil
}

type setupResult struct {
	Status      string            `json:"status"`
	Version     string            `json:"version"`
	Apps        []setupAppResult  `json:"apps"`
	DeviceToken *setupTokenResult `json:"device_token,omitempty"`
	Warnings    []string          `json:"warnings,omitempty"`
}

type setupTokenResult struct {
	Status   string `json:"status"`
	ShortID  string `json:"short_id,omitempty"`
	Keychain string `json:"keychain,omitempty"`
}

type setupTokenDependencies struct {
	resolve  func(context.Context, string) (string, error)
	store    func(context.Context, string, string) error
	validate func(context.Context, string, string) error
	list     func(context.Context, string, string) ([]busylib.StoredAccessToken, error)
	mint     func(context.Context, string, string, string) (busylib.MintedAccessToken, error)
	revoke   func(context.Context, string, string, string) error
}

func productionSetupTokenDependencies() setupTokenDependencies {
	keychain := secrets.NewKeychain(nil)
	newClient := func(baseURL, accessToken string) (*busylib.Client, error) {
		return busylib.NewClient(busylib.WithBaseURL(baseURL), busylib.WithLocalAccessToken(accessToken))
	}
	return setupTokenDependencies{
		resolve: keychain.Resolve,
		store:   keychain.Store,
		validate: func(ctx context.Context, baseURL, accessToken string) error {
			client, err := newClient(baseURL, accessToken)
			if err != nil {
				return err
			}
			if _, err = client.APISemVer(ctx); err != nil {
				return err
			}
			info, err := client.Settings().AccessTokens(ctx)
			if err != nil {
				return err
			}
			shortID := accessToken[:8]
			for _, token := range info.Tokens {
				if token.ShortID == shortID {
					return nil
				}
			}
			return errors.New("credential is not present in the device access-token inventory")
		},
		list: func(ctx context.Context, baseURL, accessToken string) ([]busylib.StoredAccessToken, error) {
			client, err := newClient(baseURL, accessToken)
			if err != nil {
				return nil, err
			}
			info, err := client.Settings().AccessTokens(ctx)
			return info.Tokens, err
		},
		mint: func(ctx context.Context, baseURL, bootstrapToken, name string) (busylib.MintedAccessToken, error) {
			client, err := newClient(baseURL, bootstrapToken)
			if err != nil {
				return busylib.MintedAccessToken{}, err
			}
			return client.Settings().MintAccessToken(ctx, name)
		},
		revoke: func(ctx context.Context, baseURL, bootstrapToken, shortID string) error {
			client, err := newClient(baseURL, bootstrapToken)
			if err != nil {
				return err
			}
			return client.Settings().RevokeAccessToken(ctx, shortID)
		},
	}
}

type setupAppResult struct {
	AppID        string `json:"app_id"`
	PluginID     string `json:"plugin_id"`
	Version      string `json:"version"`
	PluginStatus string `json:"plugin_status"`
	AppStatus    string `json:"app_status"`
}

func ensureSetupConfiguration(ctx context.Context, deviceURL, bootstrapReference, tokenReference string, dependencies setupTokenDependencies) (setupConfigurationResult, error) {
	var result setupConfigurationResult
	provisionToken := bootstrapReference != "" || tokenReference != ""
	configPath, err := resolveStatePath(nil, "config", "config.json")
	if err != nil {
		return result, err
	}
	store := config.NewStore(configPath)
	document, loadErr := store.Load()
	if loadErr == nil {
		if deviceURL != "" && document.Device.BaseURL != deviceURL {
			return result, commandFailure(exitRejected, "setup device URL conflicts with existing configuration")
		}
		if deviceURL == "" {
			deviceURL = document.Device.BaseURL
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return result, commandFailure(exitOperational, "load setup configuration failed")
	}
	if deviceURL == "" {
		deviceURL = busylib.DefaultLocalBaseURL
	}
	if bootstrapReference != "" && tokenReference == "" {
		tokenReference = defaultSetupTokenReference
	}
	if !provisionToken && loadErr == nil {
		tokenReference = document.Device.AccessTokenSecret
	}
	if bootstrapReference != "" && !validSetupKeychainReference(bootstrapReference) {
		return result, commandFailure(exitUsage, "--device-bootstrap-keychain must be a keychain reference")
	}
	if tokenReference != "" && !validSetupKeychainReference(tokenReference) {
		return result, commandFailure(exitUsage, "--device-token-keychain must be a keychain reference")
	}
	if bootstrapReference != "" && bootstrapReference == tokenReference {
		return result, commandFailure(exitUsage, "device bootstrap and token Keychain references must differ")
	}

	if provisionToken {
		result.DeviceToken, err = provisionSetupToken(ctx, deviceURL, bootstrapReference, tokenReference, dependencies)
		result.Changed = setupTokenChanged(result.DeviceToken)
		if err != nil {
			return result, err
		}
	}

	if loadErr == nil && document.Device.BaseURL == deviceURL && document.Device.AccessTokenSecret == tokenReference {
		return result, nil
	}
	restartRequired := loadErr == nil && (document.Device.BaseURL != deviceURL || document.Device.AccessTokenSecret != tokenReference)
	var outcome localstate.CommitOutcome
	if loadErr == nil {
		document.Generation++
		document.Device.BaseURL = deviceURL
		document.Device.AccessTokenSecret = tokenReference
		outcome, err = store.ReplaceWithOutcome(document.Generation-1, document)
	} else {
		document = config.Document{
			Version: config.CurrentVersion, Generation: 1,
			Device:  config.Device{BaseURL: deviceURL, AccessTokenSecret: tokenReference},
			Plugins: map[string]config.Plugin{}, Apps: map[string]config.App{},
		}
		outcome, err = store.ReplaceWithOutcome(0, document)
	}
	if outcome.IsCommitted() {
		result.Changed = true
		result.RestartRequired = restartRequired
	}
	if err != nil {
		if result.Changed {
			return result, commandFailure(exitPartial, "setup configuration durability is uncertain")
		}
		return result, commandFailure(exitOperational, "write setup configuration failed")
	}
	return result, nil
}

func provisionSetupToken(ctx context.Context, deviceURL, bootstrapReference, tokenReference string, dependencies setupTokenDependencies) (*setupTokenResult, error) {
	accessToken, resolveErr := dependencies.resolve(ctx, tokenReference)
	switch {
	case resolveErr == nil:
		if !setupAccessToken.MatchString(accessToken) {
			accessToken = ""
			return nil, commandFailure(exitRejected, "device token Keychain item is not an access token")
		}
		if err := dependencies.validate(ctx, deviceURL, accessToken); err != nil {
			accessToken = ""
			return nil, setupDependencyError(err, "validate device access token failed")
		}
		result := &setupTokenResult{Status: "existing", ShortID: accessTokenShortID(accessToken), Keychain: tokenReference}
		accessToken = ""
		return result, nil
	case !errors.Is(resolveErr, secrets.ErrItemNotFound):
		return nil, setupDependencyError(resolveErr, "inspect device access token failed")
	case bootstrapReference == "":
		return nil, commandFailure(exitUsage, "missing device access token requires --device-bootstrap-keychain")
	}

	bootstrapToken, err := dependencies.resolve(ctx, bootstrapReference)
	if err != nil {
		return nil, setupDependencyError(err, "resolve device bootstrap credential failed")
	}
	var previousTokens []busylib.StoredAccessToken
	if dependencies.list != nil {
		previousTokens, err = dependencies.list(ctx, deviceURL, bootstrapToken)
		if err != nil {
			bootstrapToken = ""
			return nil, setupDependencyError(err, "inspect device access tokens before mint failed")
		}
	}
	tokenName, err := newSetupTokenName()
	if err != nil {
		bootstrapToken = ""
		return nil, commandFailure(exitOperational, "create device access token identity failed")
	}
	minted, err := dependencies.mint(ctx, deviceURL, bootstrapToken, tokenName)
	if err != nil {
		return reconcileSetupMintFailure(ctx, dependencies, deviceURL, bootstrapToken, tokenName, previousTokens, err)
	}
	shortID, validMintedToken := validatedMintedSetupToken(minted)
	if !validMintedToken {
		minted.Token = ""
		if shortID == "" {
			bootstrapToken = ""
			return &setupTokenResult{Status: "cleanup_required", Keychain: tokenReference}, commandFailure(exitPartial, "minted device access token cannot be safely identified for cleanup")
		}
		return cleanupSetupToken(ctx, dependencies, deviceURL, bootstrapToken, tokenReference, shortID, errors.New("invalid minted token response"), "validate minted device access token failed")
	}
	if err := dependencies.validate(ctx, deviceURL, minted.Token); err != nil {
		minted.Token = ""
		return cleanupSetupToken(ctx, dependencies, deviceURL, bootstrapToken, tokenReference, shortID, err, "validate minted device access token failed")
	}
	if storeErr := dependencies.store(ctx, tokenReference, minted.Token); storeErr != nil {
		storedToken, resolveErr := resolveSetupTokenAfterStoreError(ctx, dependencies, tokenReference)
		if resolveErr == nil && subtle.ConstantTimeCompare([]byte(storedToken), []byte(minted.Token)) == 1 {
			storedToken = ""
			if errors.Is(storeErr, context.Canceled) || errors.Is(storeErr, context.DeadlineExceeded) {
				minted.Token = ""
				bootstrapToken = ""
				return &setupTokenResult{Status: "created", ShortID: shortID, Keychain: tokenReference}, commandFailure(exitPartial, "device access token was stored before cancellation")
			}
		} else {
			storedToken = ""
			minted.Token = ""
			if resolveErr != nil && !errors.Is(resolveErr, secrets.ErrItemNotFound) {
				return &setupTokenResult{Status: "unknown", ShortID: shortID, Keychain: tokenReference}, commandFailure(exitPartial, "device access token storage outcome is unknown")
			}
			return cleanupSetupToken(ctx, dependencies, deviceURL, bootstrapToken, tokenReference, shortID, storeErr, "store device access token failed")
		}
	}
	minted.Token = ""
	bootstrapToken = ""
	return &setupTokenResult{Status: "created", ShortID: shortID, Keychain: tokenReference}, nil
}

func validatedMintedSetupToken(minted busylib.MintedAccessToken) (string, bool) {
	if setupAccessToken.MatchString(minted.Token) {
		shortID := minted.Token[:8]
		return shortID, minted.ShortID == shortID
	}
	if setupAccessTokenShortID.MatchString(minted.ShortID) {
		return minted.ShortID, false
	}
	return "", false
}

func newSetupTokenName() (string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return setupTokenNamePrefix + hex.EncodeToString(suffix[:]), nil
}

func reconcileSetupMintFailure(
	ctx context.Context,
	dependencies setupTokenDependencies,
	deviceURL string,
	bootstrapToken string,
	tokenName string,
	previous []busylib.StoredAccessToken,
	cause error,
) (*setupTokenResult, error) {
	if dependencies.list == nil {
		bootstrapToken = ""
		return nil, commandFailure(exitPartial, "mint device access token outcome is unknown")
	}
	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	current, err := dependencies.list(reconcileCtx, deviceURL, bootstrapToken)
	if err != nil {
		bootstrapToken = ""
		return &setupTokenResult{Status: "unknown"}, commandFailure(exitPartial, "mint device access token outcome is unknown")
	}
	known := make(map[string]struct{}, len(previous))
	for _, token := range previous {
		known[token.ShortID] = struct{}{}
	}
	created := make([]busylib.StoredAccessToken, 0, 1)
	for _, token := range current {
		if _, existed := known[token.ShortID]; !existed && token.Name == tokenName {
			created = append(created, token)
		}
	}
	if len(created) == 0 {
		bootstrapToken = ""
		return nil, setupDependencyError(cause, "mint device access token failed")
	}
	if len(created) != 1 {
		bootstrapToken = ""
		return &setupTokenResult{Status: "unknown"}, commandFailure(exitPartial, "mint device access token outcome is ambiguous")
	}
	shortID := created[0].ShortID
	if !setupAccessTokenShortID.MatchString(shortID) {
		bootstrapToken = ""
		return &setupTokenResult{Status: "unknown"}, commandFailure(exitPartial, "mint device access token outcome is invalid")
	}
	if err := dependencies.revoke(reconcileCtx, deviceURL, bootstrapToken, shortID); err != nil {
		bootstrapToken = ""
		return &setupTokenResult{Status: "cleanup_required", ShortID: shortID}, commandFailure(exitPartial, fmt.Sprintf("mint device access token cleanup failed for token %s", shortID))
	}
	bootstrapToken = ""
	return nil, setupDependencyError(cause, "mint device access token failed")
}

func setupDependencyError(err error, message string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return commandFailure(exitOperational, message)
}

func resolveSetupTokenAfterStoreError(ctx context.Context, dependencies setupTokenDependencies, reference string) (string, error) {
	resolveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return dependencies.resolve(resolveCtx, reference)
}

func validSetupKeychainReference(reference string) bool {
	_, err := secrets.ParseReference(reference)
	return err == nil
}

func accessTokenShortID(accessToken string) string {
	if len(accessToken) < 8 {
		return ""
	}
	return accessToken[:8]
}

func cleanupSetupToken(ctx context.Context, dependencies setupTokenDependencies, deviceURL, bootstrapToken, tokenReference, shortID string, cause error, message string) (*setupTokenResult, error) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := dependencies.revoke(cleanupCtx, deviceURL, bootstrapToken, shortID)
	bootstrapToken = ""
	if err != nil {
		return &setupTokenResult{Status: "cleanup_required", ShortID: shortID, Keychain: tokenReference}, commandFailure(exitPartial, fmt.Sprintf("%s; cleanup failed for token %s", message, shortID))
	}
	return nil, setupDependencyError(cause, message)
}

func waitSetupStatus(ctx context.Context, timeout time.Duration) (control.Status, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	socketPath, err := resolveStatePath(nil, "socket", "ctl.sock")
	if err != nil {
		return control.Status{}, err
	}
	for {
		var status control.Status
		if err := callDaemon(waitCtx, socketPath, "daemon.status", nil, &status); err == nil && status.Version == version {
			return status, nil
		}
		select {
		case <-waitCtx.Done():
			return control.Status{}, waitCtx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func explicitSetupApps(raw string) ([]firstpartyplugins.Descriptor, error) {
	if raw == "none" {
		return []firstpartyplugins.Descriptor{}, nil
	}
	byID := make(map[string]firstpartyplugins.Descriptor)
	for _, descriptor := range firstpartyplugins.All() {
		byID[descriptor.DefaultApp.ID] = descriptor
	}
	selected := make([]firstpartyplugins.Descriptor, 0, len(byID))
	seen := make(map[string]struct{}, len(byID))
	for appID := range strings.SplitSeq(raw, ",") {
		descriptor, exists := byID[appID]
		if !exists {
			return nil, errInvalidSetupApps
		}
		if _, duplicate := seen[appID]; duplicate {
			return nil, errInvalidSetupApps
		}
		seen[appID] = struct{}{}
		selected = append(selected, descriptor)
	}
	if len(selected) == 0 {
		return nil, errInvalidSetupApps
	}
	return selected, nil
}

func promptSetupApps(input io.Reader, output io.Writer) ([]firstpartyplugins.Descriptor, error) {
	reader := bufio.NewReader(input)
	selected := make([]firstpartyplugins.Descriptor, 0, len(firstpartyplugins.All()))
	for _, descriptor := range firstpartyplugins.All() {
		if _, err := fmt.Fprintf(output, "\n%s\n  %s\n  Requires: %s\nInstall %s? [y/N] ", descriptor.DisplayName, descriptor.Description, descriptor.Requirement, descriptor.DisplayName); err != nil {
			return nil, err
		}
		answer, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "":
		case "n", "no":
		case "y", "yes":
			selected = append(selected, descriptor)
		default:
			return nil, errInvalidSetupApps
		}
	}
	return selected, nil
}

var errInvalidSetupApps = errors.New("invalid setup app selection")
