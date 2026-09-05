package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/installer"
	"github.com/lxdb/bsbctl/internal/launchagent"
	"github.com/lxdb/bsbctl/internal/secrets"
	busylib "github.com/lxdb/busylib-go"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSetupRejectsUnknownAppsBeforeExternalWork(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := execute(t.Context(), []string{"setup", "--apps", "not-an-app"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "setup app selection is invalid") {
		t.Fatalf("setup = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestSetupRequiresTerminalWithoutExplicitApps(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := execute(t.Context(), []string{"setup"}, strings.NewReader("n\n"), &stdout, &stderr)
	if code != exitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "setup requires a terminal or --apps") {
		t.Fatalf("setup = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestSetupPromptsForEveryAvailableAppWithNoDefaultSelection(t *testing.T) {
	var output bytes.Buffer
	selected, err := promptSetupApps(strings.NewReader("\n\n\n\n"), &output)
	if err != nil || len(selected) != 0 {
		t.Fatalf("prompt selection = %#v, error %v", selected, err)
	}
	text := output.String()
	for _, descriptor := range firstpartyplugins.All() {
		for _, want := range []string{descriptor.DisplayName, descriptor.Description, descriptor.Requirement, "Install " + descriptor.DisplayName + "? [y/N]"} {
			if !strings.Contains(text, want) {
				t.Errorf("setup prompt does not contain %q:\n%s", want, text)
			}
		}
	}
	if strings.Count(text, "? [y/N]") != len(firstpartyplugins.All()) {
		t.Fatalf("setup prompts = %q", text)
	}
}

func TestSetupCoreOnlyInitializesAndInstallsService(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	manager := &fakeServiceManager{installResult: launchagent.Result{Status: launchagent.StateLoaded}}
	restoreManager := installServiceManager(t, manager)
	defer restoreManager()
	client := &fakeCLIClient{call: func(_ context.Context, method string, _ any, result any) error {
		if method != "daemon.status" {
			return fmt.Errorf("unexpected method %s", method)
		}
		*(result.(*control.Status)) = control.Status{Version: version, Device: device.RuntimeStatus{Phase: device.PhaseReady}}
		return nil
	}}
	restoreClient := installCLIClient(t, client)
	defer restoreClient()

	var stdout, stderr bytes.Buffer
	code := execute(t.Context(), []string{"setup", "--apps", "none"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("setup = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	if stdout.String() != `{"status":"configured","version":"`+version+`","apps":[]}`+"\n" {
		t.Fatalf("setup stdout = %q", stdout.String())
	}
	document, err := config.NewStore(filepath.Join(home, ".bsbctl", "config.json")).Load()
	if err != nil || document.Generation != 1 || len(document.Plugins) != 0 || len(document.Apps) != 0 {
		t.Fatalf("setup document = %#v, %v", document, err)
	}
	if manager.installConfig.ConfigPath != filepath.Join(home, ".bsbctl", "config.json") || manager.installConfig.SocketPath != filepath.Join(home, ".bsbctl", "ctl.sock") {
		t.Fatalf("service config = %#v", manager.installConfig)
	}
}

func TestEnsureSetupConfigurationMintsAndStoresDeviceToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var storedReference, storedToken string
	mintCalls := 0
	dependencies := setupTokenDependencies{
		resolve: func(_ context.Context, reference string) (string, error) {
			if reference == defaultSetupTokenReference {
				return "", secrets.ErrItemNotFound
			}
			if reference != "keychain://bsbctl/device-bootstrap" {
				t.Fatalf("unexpected reference %q", reference)
			}
			return "bootstrap-secret", nil
		},
		store: func(_ context.Context, reference, token string) error {
			storedReference, storedToken = reference, token
			return nil
		},
		validate: func(_ context.Context, baseURL, token string) error {
			if baseURL != "http://busybar.test" || token != setupTestToken {
				t.Fatalf("validate = %q %q", baseURL, token)
			}
			return nil
		},
		mint: func(_ context.Context, baseURL, bootstrap, name string) (busylib.MintedAccessToken, error) {
			mintCalls++
			if baseURL != "http://busybar.test" || bootstrap != "bootstrap-secret" || !strings.HasPrefix(name, setupTokenNamePrefix) {
				t.Fatalf("mint = %q %q %q", baseURL, bootstrap, name)
			}
			return busylib.MintedAccessToken{StoredAccessToken: busylib.StoredAccessToken{ShortID: "AAMTBO0f"}, Token: setupTestToken}, nil
		},
	}
	result, err := ensureSetupConfiguration(t.Context(), "http://busybar.test", "keychain://bsbctl/device-bootstrap", "", dependencies)
	if err != nil {
		t.Fatalf("ensureSetupConfiguration: %v", err)
	}
	if mintCalls != 1 || storedReference != defaultSetupTokenReference || storedToken != setupTestToken {
		t.Fatalf("mint calls=%d stored=%q %q", mintCalls, storedReference, storedToken)
	}
	if result.DeviceToken == nil || result.DeviceToken.Status != "created" || result.DeviceToken.ShortID != "AAMTBO0f" || result.DeviceToken.Keychain != defaultSetupTokenReference {
		t.Fatalf("token result = %#v", result)
	}
	document, err := config.NewStore(filepath.Join(home, ".bsbctl", "config.json")).Load()
	if err != nil || document.Device.AccessTokenSecret != defaultSetupTokenReference {
		t.Fatalf("configuration = %#v, %v", document, err)
	}
}

func TestEnsureSetupConfigurationReusesValidatedDeviceToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	minted := false
	dependencies := setupTokenDependencies{
		resolve: func(context.Context, string) (string, error) { return setupTestToken, nil },
		validate: func(_ context.Context, baseURL, token string) error {
			if baseURL != busylib.DefaultLocalBaseURL || token != setupTestToken {
				t.Fatalf("validate = %q %q", baseURL, token)
			}
			return nil
		},
		mint: func(context.Context, string, string, string) (busylib.MintedAccessToken, error) {
			minted = true
			return busylib.MintedAccessToken{}, nil
		},
	}
	result, err := ensureSetupConfiguration(t.Context(), "", "", defaultSetupTokenReference, dependencies)
	if err != nil || minted {
		t.Fatalf("result=%#v minted=%t err=%v", result, minted, err)
	}
	if result.DeviceToken == nil || result.DeviceToken.Status != "existing" || result.DeviceToken.ShortID != "AAMTBO0f" {
		t.Fatalf("token result = %#v", result)
	}
}

func TestEnsureSetupConfigurationRevokesTokenWhenKeychainStoreFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	revoked := ""
	dependencies := setupTokenDependencies{
		resolve: func(_ context.Context, reference string) (string, error) {
			if reference == defaultSetupTokenReference {
				return "", secrets.ErrItemNotFound
			}
			return "bootstrap-secret", nil
		},
		store:    func(context.Context, string, string) error { return errors.New("store failed") },
		validate: func(context.Context, string, string) error { return nil },
		mint: func(context.Context, string, string, string) (busylib.MintedAccessToken, error) {
			return busylib.MintedAccessToken{StoredAccessToken: busylib.StoredAccessToken{ShortID: "AAMTBO0f"}, Token: setupTestToken}, nil
		},
		revoke: func(_ context.Context, _, bootstrap, shortID string) error {
			if bootstrap != "bootstrap-secret" {
				t.Fatal("cleanup did not use bootstrap credential")
			}
			revoked = shortID
			return nil
		},
	}
	_, err := ensureSetupConfiguration(t.Context(), "", "keychain://bsbctl/device-bootstrap", "", dependencies)
	code, _ := classifyCommandError(err)
	if code != exitOperational || revoked != "AAMTBO0f" {
		t.Fatalf("exit=%d revoked=%q err=%v", code, revoked, err)
	}
}

func TestEnsureSetupConfigurationReportsPartialWhenTokenCleanupFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dependencies := setupTokenDependencies{
		resolve: func(_ context.Context, reference string) (string, error) {
			if reference == defaultSetupTokenReference {
				return "", secrets.ErrItemNotFound
			}
			return "bootstrap-secret", nil
		},
		store:    func(context.Context, string, string) error { return errors.New("store failed") },
		validate: func(context.Context, string, string) error { return nil },
		mint: func(context.Context, string, string, string) (busylib.MintedAccessToken, error) {
			return busylib.MintedAccessToken{StoredAccessToken: busylib.StoredAccessToken{ShortID: "AAMTBO0f"}, Token: setupTestToken}, nil
		},
		revoke: func(context.Context, string, string, string) error { return errors.New("revoke failed") },
	}
	_, err := ensureSetupConfiguration(t.Context(), "", "keychain://bsbctl/device-bootstrap", "", dependencies)
	code, message := classifyCommandError(err)
	if code != exitPartial || !strings.Contains(message, "AAMTBO0f") || strings.Contains(message, setupTestToken) {
		t.Fatalf("exit=%d message=%q err=%v", code, message, err)
	}
}

func TestEnsureSetupConfigurationDoesNotMintAfterAmbiguousKeychainFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	minted := false
	dependencies := setupTokenDependencies{
		resolve: func(context.Context, string) (string, error) { return "", errors.New("keychain unavailable") },
		mint: func(context.Context, string, string, string) (busylib.MintedAccessToken, error) {
			minted = true
			return busylib.MintedAccessToken{}, nil
		},
	}
	_, err := ensureSetupConfiguration(t.Context(), "", "keychain://bsbctl/device-bootstrap", "", dependencies)
	code, _ := classifyCommandError(err)
	if code != exitOperational || minted {
		t.Fatalf("exit=%d minted=%t err=%v", code, minted, err)
	}
}

func TestEnsureSetupConfigurationPreservesConfiguredTokenWithoutProvisioningFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".bsbctl", "config.json")
	wantReference := "keychain://bsbctl/device/access-token"
	document := config.Document{
		Version: config.CurrentVersion, Generation: 1,
		Device:  config.Device{BaseURL: "http://busybar.test", AccessTokenSecret: wantReference},
		Plugins: map[string]config.Plugin{}, Apps: map[string]config.App{},
	}
	if _, err := config.NewStore(path).ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	result, err := ensureSetupConfiguration(t.Context(), "", "", "", setupTokenDependencies{})
	if err != nil || result.DeviceToken != nil || result.Changed {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	got, err := config.NewStore(path).Load()
	if err != nil || got.Generation != 1 || got.Device.AccessTokenSecret != wantReference {
		t.Fatalf("configuration = %#v, %v", got, err)
	}
}

func TestSetupFetchesCatalogFromMatchingCoreReleaseBeforeMutation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	originalVersion := version
	version = "1.2.3"
	t.Cleanup(func() { version = originalVersion })
	originalTransport := http.DefaultClient.Transport
	requests := make([]string, 0, 2)
	http.DefaultClient.Transport = setupRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.String())
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), ContentLength: 2, Header: make(http.Header), Request: request}, nil
	})
	t.Cleanup(func() { http.DefaultClient.Transport = originalTransport })

	var stdout, stderr bytes.Buffer
	code := execute(t.Context(), []string{"setup", "--apps", "mac-resources"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitOperational || stdout.Len() != 0 {
		t.Fatalf("setup = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	want := []string{
		"https://github.com/lxdb/bsbctl/releases/download/v1.2.3/catalog.json",
		"https://github.com/lxdb/bsbctl/releases/download/v1.2.3/catalog.sig",
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("setup requests = %v, want %v", requests, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".bsbctl", "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup mutated configuration before catalog verification: %v", err)
	}
}

func TestSetupInstallsEveryExplicitlySelectedAppFromOneCatalog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	originalVersion := version
	version = "1.2.3"
	t.Cleanup(func() { version = originalVersion })
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("u", ed25519.SeedSize)))
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	catalogData, signature := signedSetupCatalog(t, private, now)
	originalClient := setupHTTPClient
	setupHTTPClient = &http.Client{Transport: setupRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := catalogData
		if strings.HasSuffix(request.URL.Path, "/catalog.sig") {
			body = signature
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Header: make(http.Header), Request: request}, nil
	})}
	t.Cleanup(func() { setupHTTPClient = originalClient })
	originalKeyring := setupCatalogKeyring
	setupCatalogKeyring = func() (catalog.Keyring, error) {
		return catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)}, nil
	}
	t.Cleanup(func() { setupCatalogKeyring = originalKeyring })
	originalNow := setupNow
	setupNow = func() time.Time { return now }
	t.Cleanup(func() { setupNow = originalNow })
	manager := &fakeServiceManager{installResult: launchagent.Result{Status: launchagent.StateLoaded}}
	restoreManager := installServiceManager(t, manager)
	defer restoreManager()

	installed := make(map[string]string)
	apps := make(map[string]string)
	client := &fakeCLIClient{call: func(_ context.Context, method string, params, result any) error {
		switch method {
		case "daemon.status":
			status := control.Status{Version: version, Device: device.RuntimeStatus{Phase: device.PhaseReady}}
			for appID, pluginID := range apps {
				status.Apps = append(status.Apps, control.AppStatus{AppID: appID, PluginID: pluginID, Enabled: true})
			}
			*(result.(*control.Status)) = status
		case "plugin.status":
			snapshot := installer.Snapshot{Plugins: make([]installer.PluginSnapshot, 0, len(installed))}
			for pluginID, pluginVersion := range installed {
				snapshot.Plugins = append(snapshot.Plugins, installer.PluginSnapshot{PluginID: pluginID, Active: &installer.ReleaseRef{ID: pluginID, Version: pluginVersion, OS: runtime.GOOS, Arch: runtime.GOARCH}})
			}
			*(result.(*installer.Snapshot)) = snapshot
		case "plugin.install":
			request := params.(control.CatalogInstallRequest)
			installed[request.PluginID] = request.Version
			*(result.(*control.CatalogOperationResponse)) = control.CatalogOperationResponse{Result: installer.Result{Status: installer.StatusInstalled, Release: installer.ReleaseRef{ID: request.PluginID, Version: request.Version, OS: request.OS, Arch: request.Arch}}}
		case "app.create":
			request := params.(control.CreateAppRequest)
			apps[request.AppID] = request.PluginID
			*(result.(*control.AppInstanceResult)) = control.AppInstanceResult{Status: control.MutationCreated, AppID: request.AppID, PluginID: request.PluginID, Enabled: true, Generation: uint64(len(apps) + 1)}
		default:
			return fmt.Errorf("unexpected method %s", method)
		}
		return nil
	}}
	restoreClient := installCLIClient(t, client)
	defer restoreClient()

	var stdout, stderr bytes.Buffer
	code := execute(t.Context(), []string{"setup", "--apps", "mac-resources,codex-quota"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("setup = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	var result setupResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "configured" || result.Version != "1.2.3" || len(result.Apps) != 2 || result.Apps[0].AppID != "mac-resources" || result.Apps[1].AppID != "codex-quota" {
		t.Fatalf("setup result = %#v", result)
	}
	for _, app := range result.Apps {
		if app.PluginStatus != "installed" || app.AppStatus != "created" || installed[app.PluginID] != "1.2.3" || apps[app.AppID] != app.PluginID {
			t.Fatalf("setup app = %#v installed=%v apps=%v", app, installed, apps)
		}
	}
}

func TestSetupPreflightsEverySelectedAppBeforeMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	selected, err := explicitSetupApps("mac-resources,codex-quota")
	if err != nil {
		t.Fatal(err)
	}
	release := setupCatalog{
		Data:      []byte(`{}`),
		Signature: []byte(`{}`),
		Entries: map[string]catalog.Entry{
			selected[0].ID: {ID: selected[0].ID, Version: "1.2.3"},
			selected[1].ID: {ID: selected[1].ID, Version: "1.2.3"},
		},
	}
	mutations := 0
	client := &fakeCLIClient{call: func(_ context.Context, method string, params, result any) error {
		switch method {
		case "plugin.status":
			*(result.(*installer.Snapshot)) = installer.Snapshot{}
		case "plugin.install":
			mutations++
			request := params.(control.CatalogInstallRequest)
			*(result.(*control.CatalogOperationResponse)) = control.CatalogOperationResponse{Result: installer.Result{
				Status: installer.StatusInstalled,
				Release: installer.ReleaseRef{
					ID: request.PluginID, Version: request.Version, OS: request.OS, Arch: request.Arch,
				},
			}}
		case "app.create":
			mutations++
			request := params.(control.CreateAppRequest)
			*(result.(*control.AppInstanceResult)) = control.AppInstanceResult{
				Status: control.MutationCreated, AppID: request.AppID, PluginID: request.PluginID, Enabled: request.Enabled,
			}
		default:
			return fmt.Errorf("unexpected method %s", method)
		}
		return nil
	}}
	restoreClient := installCLIClient(t, client)
	defer restoreClient()

	_, err = reconcileSetupApps(t.Context(), selected, release, control.Status{Apps: []control.AppStatus{{
		AppID: selected[1].DefaultApp.ID, PluginID: selected[0].ID,
	}}}, io.Discard)
	code, _ := classifyCommandError(err)
	if code != exitRejected {
		t.Fatalf("setup conflict exit = %d, want %d: %v", code, exitRejected, err)
	}
	if mutations != 0 {
		t.Fatalf("setup performed %d mutation(s) before rejecting a later app conflict", mutations)
	}
}

func TestSetupReportsConfirmedMutationWhenLaterOperationFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	selected, err := explicitSetupApps("mac-resources")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := selected[0]
	release := setupCatalog{
		Data:      []byte(`{}`),
		Signature: []byte(`{}`),
		Entries: map[string]catalog.Entry{
			descriptor.ID: {ID: descriptor.ID, Version: "1.2.3"},
		},
	}
	client := &fakeCLIClient{call: func(_ context.Context, method string, params, result any) error {
		switch method {
		case "plugin.status":
			*(result.(*installer.Snapshot)) = installer.Snapshot{}
		case "plugin.install":
			request := params.(control.CatalogInstallRequest)
			*(result.(*control.CatalogOperationResponse)) = control.CatalogOperationResponse{Result: installer.Result{
				Status: installer.StatusInstalled,
				Release: installer.ReleaseRef{
					ID: request.PluginID, Version: request.Version, OS: request.OS, Arch: request.Arch,
				},
			}}
		case "app.create":
			return commandFailure(exitOperational, "app creation failed")
		default:
			return fmt.Errorf("unexpected method %s", method)
		}
		return nil
	}}
	restoreClient := installCLIClient(t, client)
	defer restoreClient()

	results, err := reconcileSetupApps(t.Context(), selected, release, control.Status{}, io.Discard)
	code, _ := classifyCommandError(err)
	if code != exitPartial {
		t.Fatalf("setup failure exit = %d, want %d: %v", code, exitPartial, err)
	}
	if len(results) != 1 || results[0].AppID != descriptor.DefaultApp.ID || results[0].PluginStatus != "installed" || results[0].AppStatus != "unknown" {
		t.Fatalf("setup partial results = %#v", results)
	}
}

func signedSetupCatalog(t *testing.T, private ed25519.PrivateKey, now time.Time) ([]byte, []byte) {
	t.Helper()
	entries := make([]catalog.Entry, 0, len(firstpartyplugins.All()))
	for _, descriptor := range firstpartyplugins.All() {
		entries = append(entries, catalog.Entry{
			ID: descriptor.ID, Version: "1.2.3", OS: runtime.GOOS, Arch: runtime.GOARCH,
			URL: "https://example.invalid/" + descriptor.Binary + ".tar.gz", SHA256: strings.Repeat("0", 64),
			CompressedSize: 1, ArchiveFormat: "tar.gz", Executable: descriptor.Binary, Manifest: "manifest.json",
		})
	}
	data, err := json.Marshal(catalog.Catalog{Version: 1, Channel: "stable", Sequence: 1, GeneratedAt: now.Add(-time.Hour), Plugins: entries})
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(private, data)
	envelope := []byte(`{"key_id":"stable","algorithm":"ed25519","signature":"` + base64.StdEncoding.EncodeToString(signature) + `"}`)
	return data, envelope
}

type setupRoundTripFunc func(*http.Request) (*http.Response, error)

func (function setupRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
