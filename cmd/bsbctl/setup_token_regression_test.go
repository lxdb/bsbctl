package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/secrets"
	busylib "github.com/lxdb/busylib-go"
)

const setupTestToken = "AAMTBO0fvAxB5ZO8ds8bA1JofGWn4fID"

func TestEnsureSetupConfigurationRejectsIdenticalBootstrapAndTokenReferences(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	called := false
	reference := "keychain://bsbctl/device/credential"
	_, err := ensureSetupConfiguration(t.Context(), "", reference, reference, setupTokenDependencies{
		resolve: func(context.Context, string) (string, error) {
			called = true
			return setupTestToken, nil
		},
		validate: func(context.Context, string, string) error { return nil },
	})
	code, _ := classifyCommandError(err)
	if code != exitUsage || called {
		t.Fatalf("exit=%d resolve-called=%t err=%v, want usage before Keychain access", code, called, err)
	}
}

func TestEnsureSetupConfigurationRejectsAdministratorKeyAsExistingToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	result, err := ensureSetupConfiguration(t.Context(), "", "", defaultSetupTokenReference, setupTokenDependencies{
		resolve:  func(context.Context, string) (string, error) { return "12345678", nil },
		validate: func(context.Context, string, string) error { return nil },
	})
	code, _ := classifyCommandError(err)
	if code != exitRejected || result.DeviceToken != nil || result.Changed {
		t.Fatalf("result=%#v exit=%d err=%v, want rejected without token metadata", result, code, err)
	}
}

func TestEnsureSetupConfigurationPreservesCancellationBeforeMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := ensureSetupConfiguration(t.Context(), "", "", defaultSetupTokenReference, setupTokenDependencies{
		resolve: func(context.Context, string) (string, error) { return "", context.Canceled },
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
}

func TestEnsureSetupConfigurationReportsAmbiguousMintAsPartial(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dependencies := setupTokenDependencies{
		resolve: func(_ context.Context, reference string) (string, error) {
			if reference == defaultSetupTokenReference {
				return "", secrets.ErrItemNotFound
			}
			return "bootstrap-secret", nil
		},
		mint: func(context.Context, string, string, string) (busylib.MintedAccessToken, error) {
			return busylib.MintedAccessToken{}, errors.New("response lost")
		},
	}
	result, err := ensureSetupConfiguration(t.Context(), "", "keychain://bsbctl/device-bootstrap", "", dependencies)
	code, _ := classifyCommandError(err)
	if code != exitPartial || result.DeviceToken != nil {
		t.Fatalf("result=%#v exit=%d err=%v, want partial unknown mint outcome", result, code, err)
	}
}

func TestEnsureSetupConfigurationAcceptsKeychainStoreCommitReportedAsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stored := false
	revoked := false
	dependencies := setupTokenDependencies{
		resolve: func(_ context.Context, reference string) (string, error) {
			switch {
			case reference == defaultSetupTokenReference && stored:
				return setupTestToken, nil
			case reference == defaultSetupTokenReference:
				return "", secrets.ErrItemNotFound
			default:
				return "bootstrap-secret", nil
			}
		},
		store: func(context.Context, string, string) error {
			stored = true
			return errors.New("security exited after committing")
		},
		validate: func(context.Context, string, string) error { return nil },
		mint: func(context.Context, string, string, string) (busylib.MintedAccessToken, error) {
			return busylib.MintedAccessToken{
				StoredAccessToken: busylib.StoredAccessToken{ShortID: "AAMTBO0f"},
				Token:             setupTestToken,
			}, nil
		},
		revoke: func(context.Context, string, string, string) error {
			revoked = true
			return nil
		},
	}
	result, err := ensureSetupConfiguration(t.Context(), "", "keychain://bsbctl/device-bootstrap", "", dependencies)
	if err != nil || revoked {
		t.Fatalf("result=%#v revoked=%t err=%v, want committed Keychain value retained", result, revoked, err)
	}
	if result.DeviceToken == nil || result.DeviceToken.Status != "created" || result.DeviceToken.ShortID != "AAMTBO0f" {
		t.Fatalf("result=%#v, want created token metadata", result)
	}
}

func TestProductionSetupTokenValidationRejectsCredentialMissingFromTokenInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/version":
			_, _ = writer.Write([]byte(`{"api_semver":"27.5.0"}`))
		case "/api/access/tokens":
			_, _ = writer.Write([]byte(`{"tokens":[]}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	err := productionSetupTokenDependencies().validate(t.Context(), server.URL, setupTestToken)
	if err == nil {
		t.Fatal("validation accepted a credential absent from the device token inventory")
	}
}

func TestEnsureSetupConfigurationReconcilesAmbiguousMintBeforeReturningFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	listCalls := 0
	mintedName := ""
	revoked := ""
	dependencies := setupTokenDependencies{
		resolve: func(_ context.Context, reference string) (string, error) {
			if reference == defaultSetupTokenReference {
				return "", secrets.ErrItemNotFound
			}
			return "bootstrap-secret", nil
		},
		list: func(context.Context, string, string) ([]busylib.StoredAccessToken, error) {
			listCalls++
			if listCalls == 1 {
				return nil, nil
			}
			return []busylib.StoredAccessToken{{ShortID: "AAMTBO0f", Name: mintedName}}, nil
		},
		mint: func(_ context.Context, _, _, name string) (busylib.MintedAccessToken, error) {
			mintedName = name
			return busylib.MintedAccessToken{}, errors.New("response lost after commit")
		},
		revoke: func(_ context.Context, _, _, shortID string) error {
			revoked = shortID
			return nil
		},
	}
	result, err := ensureSetupConfiguration(t.Context(), "", "keychain://bsbctl/device-bootstrap", "", dependencies)
	code, _ := classifyCommandError(err)
	if result.DeviceToken != nil || code != exitOperational || revoked != "AAMTBO0f" {
		t.Fatalf("result=%#v exit=%d revoked=%q err=%v, want reconciled cleanup and operational failure", result, code, revoked, err)
	}
	if !strings.HasPrefix(mintedName, "bsbctl-") || mintedName == "bsbctl-" {
		t.Fatalf("minted name=%q, want unique bsbctl operation name", mintedName)
	}
}

func TestRunSetupRestartsServiceWhenDeviceConfigurationChanges(t *testing.T) {
	serviceCommands := make([]string, 0, 2)
	dependencies := setupDependencies{
		loadCatalog: func(context.Context) (setupCatalog, error) { return setupCatalog{}, nil },
		configure: func(context.Context, string, string, string) (setupConfigurationResult, error) {
			return setupConfigurationResult{
				DeviceToken:     &setupTokenResult{Status: "existing", ShortID: "AAMTBO0f", Keychain: defaultSetupTokenReference},
				Changed:         true,
				RestartRequired: true,
			}, nil
		},
		service: func(_ context.Context, args []string, _, _ io.Writer) error {
			serviceCommands = append(serviceCommands, args[0])
			return nil
		},
		waitStatus: func(context.Context, time.Duration) (control.Status, error) {
			return control.Status{Version: version, Device: device.RuntimeStatus{Phase: device.PhaseReady}}, nil
		},
	}
	var stdout bytes.Buffer
	err := runSetupWithDependencies(t.Context(), []string{"--apps", "none"}, strings.NewReader(""), &stdout, io.Discard, dependencies)
	if err != nil {
		t.Fatalf("runSetupWithDependencies: %v", err)
	}
	if !reflect.DeepEqual(serviceCommands, []string{"install", "restart"}) {
		t.Fatalf("service commands=%v, want install then restart", serviceCommands)
	}
}

func TestRunSetupWritesPartialResultWhenConfigurationReturnsKnownMutation(t *testing.T) {
	dependencies := setupDependencies{
		loadCatalog: func(context.Context) (setupCatalog, error) { return setupCatalog{}, nil },
		configure: func(context.Context, string, string, string) (setupConfigurationResult, error) {
			return setupConfigurationResult{
				DeviceToken: &setupTokenResult{Status: "created", ShortID: "AAMTBO0f", Keychain: defaultSetupTokenReference},
				Changed:     true,
			}, commandFailure(exitPartial, "configuration durability is uncertain")
		},
	}
	var stdout bytes.Buffer
	err := runSetupWithDependencies(t.Context(), []string{"--apps", "none"}, strings.NewReader(""), &stdout, io.Discard, dependencies)
	code, _ := classifyCommandError(err)
	if code != exitPartial || !strings.Contains(stdout.String(), `"status":"partial"`) || !strings.Contains(stdout.String(), `"short_id":"AAMTBO0f"`) {
		t.Fatalf("exit=%d stdout=%q err=%v, want structured partial result", code, stdout.String(), err)
	}
}

func TestSetupReexecutesSameVersionAndRejectsWrongDaemonBeforeApps(t *testing.T) {
	for _, runningVersion := range []string{version, "different-build"} {
		t.Run(runningVersion, func(t *testing.T) {
			var calls []string
			selected := firstpartyplugins.All()[0]
			dependencies := setupDependencies{
				loadCatalog: func(context.Context) (setupCatalog, error) { return setupCatalog{}, nil },
				configure: func(context.Context, string, string, string) (setupConfigurationResult, error) {
					return setupConfigurationResult{}, nil
				},
				service: func(_ context.Context, args []string, _, _ io.Writer) error {
					calls = append(calls, args[0])
					return nil
				},
				waitStatus: func(context.Context, time.Duration) (control.Status, error) {
					calls = append(calls, "status")
					return control.Status{Version: runningVersion}, nil
				},
				reconcileApps: func(context.Context, []firstpartyplugins.Descriptor, setupCatalog, control.Status, io.Writer) ([]setupAppResult, error) {
					calls = append(calls, "apps")
					return nil, nil
				},
			}
			var stdout bytes.Buffer
			err := runSetupWithDependencies(t.Context(), []string{"--apps", selected.DefaultApp.ID}, strings.NewReader(""), &stdout, io.Discard, dependencies)
			want := []string{"install", "restart", "status", "apps", "status"}
			if runningVersion != version {
				want = want[:3]
				if code, _ := classifyCommandError(err); code != exitPartial || !strings.Contains(stdout.String(), `"status":"partial"`) {
					t.Fatalf("wrong daemon result = %v, %q", err, stdout.String())
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("setup order = %v, want %v", calls, want)
			}
		})
	}
}

func TestRunSetupRestartsServiceAfterCommittedConfigurationWithUncertainDurability(t *testing.T) {
	serviceCommands := make([]string, 0, 2)
	dependencies := setupDependencies{
		loadCatalog: func(context.Context) (setupCatalog, error) { return setupCatalog{}, nil },
		configure: func(context.Context, string, string, string) (setupConfigurationResult, error) {
			return setupConfigurationResult{
				DeviceToken:     &setupTokenResult{Status: "existing", ShortID: "AAMTBO0f", Keychain: defaultSetupTokenReference},
				Changed:         true,
				RestartRequired: true,
			}, commandFailure(exitPartial, "configuration durability is uncertain")
		},
		service: func(_ context.Context, args []string, _, _ io.Writer) error {
			serviceCommands = append(serviceCommands, args[0])
			return nil
		},
	}
	var stdout bytes.Buffer
	err := runSetupWithDependencies(t.Context(), []string{"--apps", "none"}, strings.NewReader(""), &stdout, io.Discard, dependencies)
	code, _ := classifyCommandError(err)
	if code != exitPartial || !reflect.DeepEqual(serviceCommands, []string{"install", "restart"}) {
		t.Fatalf("exit=%d service commands=%v err=%v, want activation before partial result", code, serviceCommands, err)
	}
	if !strings.Contains(stdout.String(), `"status":"partial"`) {
		t.Fatalf("stdout=%q, want structured partial result", stdout.String())
	}
}

func TestRunSetupReportsSecondReadinessFailureAfterAppMutationAsPartial(t *testing.T) {
	waits := 0
	selected := firstpartyplugins.All()[0]
	dependencies := setupDependencies{
		loadCatalog: func(context.Context) (setupCatalog, error) {
			return setupCatalog{}, nil
		},
		configure: func(context.Context, string, string, string) (setupConfigurationResult, error) {
			return setupConfigurationResult{}, nil
		},
		service: func(context.Context, []string, io.Writer, io.Writer) error { return nil },
		waitStatus: func(context.Context, time.Duration) (control.Status, error) {
			waits++
			if waits == 1 {
				return control.Status{Version: version, Device: device.RuntimeStatus{Phase: device.PhaseReady}}, nil
			}
			return control.Status{}, context.DeadlineExceeded
		},
		reconcileApps: func(context.Context, []firstpartyplugins.Descriptor, setupCatalog, control.Status, io.Writer) ([]setupAppResult, error) {
			return []setupAppResult{{AppID: selected.DefaultApp.ID, PluginID: selected.ID, PluginStatus: "installed", AppStatus: "created"}}, nil
		},
	}
	var stdout bytes.Buffer
	err := runSetupWithDependencies(t.Context(), []string{"--apps", selected.DefaultApp.ID}, strings.NewReader(""), &stdout, io.Discard, dependencies)
	code, _ := classifyCommandError(err)
	if code != exitPartial || !strings.Contains(stdout.String(), `"status":"partial"`) || !strings.Contains(stdout.String(), `"plugin_status":"installed"`) {
		t.Fatalf("exit=%d stdout=%q err=%v, want app mutation as partial result", code, stdout.String(), err)
	}
}

func TestEnsureSetupConfigurationRequiresRestartAfterChangingTokenReference(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".bsbctl", "config.json")
	document := config.Document{
		Version: config.CurrentVersion, Generation: 1,
		Device:  config.Device{BaseURL: busylib.DefaultLocalBaseURL, AccessTokenSecret: "keychain://bsbctl/device/old-token"},
		Plugins: map[string]config.Plugin{}, Apps: map[string]config.App{},
	}
	if _, err := config.NewStore(path).ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	result, err := ensureSetupConfiguration(t.Context(), "", "", defaultSetupTokenReference, setupTokenDependencies{
		resolve:  func(context.Context, string) (string, error) { return setupTestToken, nil },
		validate: func(context.Context, string, string) error { return nil },
	})
	if err != nil {
		t.Fatalf("ensureSetupConfiguration: %v", err)
	}
	if !result.Changed || !result.RestartRequired || result.DeviceToken == nil || result.DeviceToken.Status != "existing" {
		t.Fatalf("result=%#v, want changed configuration requiring restart", result)
	}
}

func TestEnsureSetupConfigurationRejectsMalformedMintedCredentialBeforeStorage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stored := false
	revoked := ""
	dependencies := setupTokenDependencies{
		resolve: func(_ context.Context, reference string) (string, error) {
			if reference == defaultSetupTokenReference {
				return "", secrets.ErrItemNotFound
			}
			return "bootstrap-secret", nil
		},
		list: func(context.Context, string, string) ([]busylib.StoredAccessToken, error) { return nil, nil },
		mint: func(context.Context, string, string, string) (busylib.MintedAccessToken, error) {
			return busylib.MintedAccessToken{StoredAccessToken: busylib.StoredAccessToken{ShortID: "AAMTBO0f"}, Token: "not-a-token"}, nil
		},
		validate: func(context.Context, string, string) error { return nil },
		store: func(context.Context, string, string) error {
			stored = true
			return nil
		},
		revoke: func(_ context.Context, _, _, shortID string) error {
			revoked = shortID
			return nil
		},
	}
	result, err := ensureSetupConfiguration(t.Context(), "", "keychain://bsbctl/device-bootstrap", "", dependencies)
	code, _ := classifyCommandError(err)
	if code != exitOperational || stored || revoked != "AAMTBO0f" || result.Changed {
		t.Fatalf("result=%#v exit=%d stored=%t revoked=%q err=%v, want malformed token cleaned before storage", result, code, stored, revoked, err)
	}
}

func TestEnsureSetupConfigurationReportsCleanupRequiredWhenMintResponseHasNoSafeID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	revoked := false
	dependencies := setupTokenDependencies{
		resolve: func(_ context.Context, reference string) (string, error) {
			if reference == defaultSetupTokenReference {
				return "", secrets.ErrItemNotFound
			}
			return "bootstrap-secret", nil
		},
		list: func(context.Context, string, string) ([]busylib.StoredAccessToken, error) { return nil, nil },
		mint: func(context.Context, string, string, string) (busylib.MintedAccessToken, error) {
			return busylib.MintedAccessToken{StoredAccessToken: busylib.StoredAccessToken{ShortID: "unsafe-id"}, Token: "not-a-token"}, nil
		},
		revoke: func(context.Context, string, string, string) error {
			revoked = true
			return nil
		},
	}
	result, err := ensureSetupConfiguration(t.Context(), "", "keychain://bsbctl/device-bootstrap", "", dependencies)
	code, message := classifyCommandError(err)
	if code != exitPartial || revoked || !result.Changed || result.DeviceToken == nil || result.DeviceToken.Status != "cleanup_required" || result.DeviceToken.ShortID != "" {
		t.Fatalf("result=%#v exit=%d revoked=%t err=%v, want cleanup required without unsafe ID", result, code, revoked, err)
	}
	if strings.Contains(message, "unsafe-id") || strings.Contains(message, "not-a-token") {
		t.Fatalf("message=%q exposes malformed device response", message)
	}
}

func TestEnsureSetupConfigurationReportsPartialWhenCanceledStoreCommitted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stored := false
	dependencies := setupTokenDependencies{
		resolve: func(_ context.Context, reference string) (string, error) {
			switch {
			case reference == defaultSetupTokenReference && stored:
				return setupTestToken, nil
			case reference == defaultSetupTokenReference:
				return "", secrets.ErrItemNotFound
			default:
				return "bootstrap-secret", nil
			}
		},
		list:     func(context.Context, string, string) ([]busylib.StoredAccessToken, error) { return nil, nil },
		validate: func(context.Context, string, string) error { return nil },
		mint: func(context.Context, string, string, string) (busylib.MintedAccessToken, error) {
			return busylib.MintedAccessToken{StoredAccessToken: busylib.StoredAccessToken{ShortID: "AAMTBO0f"}, Token: setupTestToken}, nil
		},
		store: func(context.Context, string, string) error {
			stored = true
			return context.Canceled
		},
	}
	result, err := ensureSetupConfiguration(t.Context(), "", "keychain://bsbctl/device-bootstrap", "", dependencies)
	code, _ := classifyCommandError(err)
	if code != exitPartial || !result.Changed || result.DeviceToken == nil || result.DeviceToken.Status != "created" {
		t.Fatalf("result=%#v exit=%d err=%v, want committed token as partial result", result, code, err)
	}
}

func TestEnsureSetupConfigurationReturnsCleanupMetadataWhenRevokeFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dependencies := setupTokenDependencies{
		resolve: func(_ context.Context, reference string) (string, error) {
			if reference == defaultSetupTokenReference {
				return "", secrets.ErrItemNotFound
			}
			return "bootstrap-secret", nil
		},
		list:     func(context.Context, string, string) ([]busylib.StoredAccessToken, error) { return nil, nil },
		validate: func(context.Context, string, string) error { return errors.New("token rejected") },
		mint: func(context.Context, string, string, string) (busylib.MintedAccessToken, error) {
			return busylib.MintedAccessToken{StoredAccessToken: busylib.StoredAccessToken{ShortID: "AAMTBO0f"}, Token: setupTestToken}, nil
		},
		revoke: func(context.Context, string, string, string) error { return errors.New("revoke failed") },
	}
	result, err := ensureSetupConfiguration(t.Context(), "", "keychain://bsbctl/device-bootstrap", "", dependencies)
	code, _ := classifyCommandError(err)
	if code != exitPartial || !result.Changed || result.DeviceToken == nil || result.DeviceToken.Status != "cleanup_required" || result.DeviceToken.ShortID != "AAMTBO0f" {
		t.Fatalf("result=%#v exit=%d err=%v, want safe cleanup-required metadata", result, code, err)
	}
}

func TestEnsureSetupConfigurationPreservesCanceledMintWhenInventoryProvesNoCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dependencies := setupTokenDependencies{
		resolve: func(_ context.Context, reference string) (string, error) {
			if reference == defaultSetupTokenReference {
				return "", secrets.ErrItemNotFound
			}
			return "bootstrap-secret", nil
		},
		list: func(context.Context, string, string) ([]busylib.StoredAccessToken, error) { return nil, nil },
		mint: func(context.Context, string, string, string) (busylib.MintedAccessToken, error) {
			return busylib.MintedAccessToken{}, context.Canceled
		},
	}
	result, err := ensureSetupConfiguration(t.Context(), "", "keychain://bsbctl/device-bootstrap", "", dependencies)
	if !errors.Is(err, context.Canceled) || result.Changed {
		t.Fatalf("result=%#v err=%v, want cancellation after proven non-commit", result, err)
	}
}

func TestEnsureSetupConfigurationDoesNotExposeMalformedReconciledTokenID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const malformedID = "secret-canary-too-long"
	listCalls := 0
	tokenName := ""
	revoked := false
	dependencies := setupTokenDependencies{
		resolve: func(_ context.Context, reference string) (string, error) {
			if reference == defaultSetupTokenReference {
				return "", secrets.ErrItemNotFound
			}
			return "bootstrap-secret", nil
		},
		list: func(context.Context, string, string) ([]busylib.StoredAccessToken, error) {
			listCalls++
			if listCalls == 1 {
				return nil, nil
			}
			return []busylib.StoredAccessToken{{ShortID: malformedID, Name: tokenName}}, nil
		},
		mint: func(_ context.Context, _, _, name string) (busylib.MintedAccessToken, error) {
			tokenName = name
			return busylib.MintedAccessToken{}, errors.New("response lost after commit")
		},
		revoke: func(context.Context, string, string, string) error {
			revoked = true
			return nil
		},
	}
	result, err := ensureSetupConfiguration(t.Context(), "", "keychain://bsbctl/device-bootstrap", "", dependencies)
	code, message := classifyCommandError(err)
	if code != exitPartial || revoked || result.DeviceToken == nil || result.DeviceToken.Status != "unknown" {
		t.Fatalf("result=%#v exit=%d revoked=%t err=%v, want unknown outcome without cleanup", result, code, revoked, err)
	}
	if strings.Contains(message, malformedID) {
		t.Fatalf("message=%q exposes malformed device-controlled token ID", message)
	}
}
