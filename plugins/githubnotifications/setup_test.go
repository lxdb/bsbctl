package githubnotifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lxdb/bsbctl/internal/appsetup"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/presentation"
)

const appSetupCanary = "github_pat_FAKE_CANARY_DO_NOT_USE_0123456789"

func TestAppSetupRoutesInstalledGitHubNotificationsConfiguration(t *testing.T) {
	inputPath := writeAppSetupInput(t, `{"config":{"repositories":[{"name":"acme/widget","alias":"W"}],"label":"GH"}}`)
	deps := validAppSetupDependencies(t)
	deps.resolveSecret = func(context.Context, string) (string, error) { return "", appsetup.ErrSecretNotFound }
	deps.githubToken = func(context.Context) (string, error) { return appSetupCanary, nil }
	deps.authorize = func(_ context.Context, _ *http.Client, token string, repositories []Repository) (Identity, error) {
		if token != appSetupCanary || !reflect.DeepEqual(repositories, []Repository{{Name: "acme/widget", Alias: "W"}}) {
			t.Fatalf("authorize token/repositories = %q %#v", token, repositories)
		}
		return Identity{ID: 7, Login: "octocat"}, nil
	}
	deps.newReference = func() (string, error) { return "keychain://bsbctl/github-notifications-fresh", nil }
	deps.storeSecret = func(_ context.Context, reference, token string) error {
		if reference != "keychain://bsbctl/github-notifications-fresh" || token != appSetupCanary {
			t.Fatalf("store = %q %q", reference, token)
		}
		return nil
	}
	readbacks := 0
	deps.resolveSecret = func(_ context.Context, reference string) (string, error) {
		if reference == "keychain://bsbctl/github-notifications-old" {
			return "", appsetup.ErrSecretNotFound
		}
		readbacks++
		return appSetupCanary, nil
	}
	var request appsetup.ReplaceConfigRequest
	deps.replaceConfig = func(_ context.Context, got appsetup.ReplaceConfigRequest) appSetupMutation {
		request = got
		return appSetupMutation{Result: appsetup.ConfigResult{Status: appsetup.MutationUpdated, AppID: "github", Generation: 10}, Outcome: appSetupMutationKnown}
	}

	var stdout, stderr bytes.Buffer
	err := runAppSetupWithDependencies(t.Context(), []string{"github", "--file", inputPath}, strings.NewReader(""), &stdout, &stderr, deps)
	if err != nil || stderr.Len() != 0 {
		t.Fatalf("setup error = %v, stderr = %q", err, stderr.String())
	}
	if stdout.String() != `{"status":"configured","app_id":"github","generation":10,"secret_reference":"keychain://bsbctl/github-notifications-fresh"}`+"\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if readbacks != 1 || request.AppID != "github" || string(request.Config) != `{"repositories":[{"name":"acme/widget","alias":"W"}],"label":"GH"}` ||
		request.ExpectedGeneration != 9 || !reflect.DeepEqual(request.Secrets, map[string]string{"token": "keychain://bsbctl/github-notifications-fresh"}) {
		t.Fatalf("readbacks=%d request=%#v", readbacks, request)
	}
	wantPolicies := map[string]presentation.PolicyConfig{"summary": {Policy: presentation.PolicyRotation, RotationIntervalMS: 60_000}}
	if !reflect.DeepEqual(request.Policies, wantPolicies) || request.LaunchAction != "open" {
		t.Fatalf("omitted policy state was not preserved: %#v", request)
	}
}

func TestAppSetupCredentialPrecedence(t *testing.T) {
	tests := []struct {
		name              string
		inputSecrets      string
		existingReference string
		wantReference     string
		wantToken         string
		wantGHCalls       int
		wantManualCalls   int
		wantStores        int
	}{
		{name: "explicit reference", inputSecrets: `,"secrets":{"token":"keychain://bsbctl/explicit"}`, existingReference: "keychain://bsbctl/existing", wantReference: "keychain://bsbctl/explicit", wantToken: "explicit-token"},
		{name: "existing reference", existingReference: "keychain://bsbctl/existing", wantReference: "keychain://bsbctl/existing", wantToken: "existing-token"},
		{name: "gh import", wantReference: "keychain://bsbctl/fresh", wantToken: "gh-token", wantGHCalls: 1, wantStores: 1},
		{name: "manual fallback", wantReference: "keychain://bsbctl/fresh", wantToken: "manual-token", wantGHCalls: 1, wantManualCalls: 1, wantStores: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inputPath := writeAppSetupInput(t, `{"config":{"repositories":[{"name":"acme/widget","alias":"W"}]}`+test.inputSecrets+`}`)
			deps := validAppSetupDependencies(t)
			document, _ := deps.loadDocument()
			app := document.Apps["github"]
			if test.existingReference == "" {
				app.Secrets = nil
			} else {
				app.Secrets = map[string]string{"token": test.existingReference}
			}
			document.Apps["github"] = app
			deps.loadDocument = func() (config.Document, error) { return document, nil }
			stored := ""
			deps.resolveSecret = func(_ context.Context, reference string) (string, error) {
				switch reference {
				case "keychain://bsbctl/explicit":
					return "explicit-token", nil
				case "keychain://bsbctl/existing":
					return "existing-token", nil
				case "keychain://bsbctl/fresh":
					if stored == "" {
						return "", appsetup.ErrSecretNotFound
					}
					return stored, nil
				default:
					return "", appsetup.ErrSecretNotFound
				}
			}
			ghCalls := 0
			deps.githubToken = func(context.Context) (string, error) {
				ghCalls++
				if test.name == "manual fallback" {
					return "", errGitHubCLIUnavailable
				}
				return "gh-token", nil
			}
			manualCalls := 0
			deps.readManualToken = func(context.Context, io.Reader, io.Writer, bool) (string, error) {
				manualCalls++
				return "manual-token", nil
			}
			stores := 0
			deps.newReference = func() (string, error) { return "keychain://bsbctl/fresh", nil }
			deps.storeSecret = func(_ context.Context, _ string, token string) error {
				stores++
				stored = token
				return nil
			}
			deps.authorize = func(_ context.Context, _ *http.Client, token string, _ []Repository) (Identity, error) {
				if token != test.wantToken {
					t.Fatalf("authorized token = %q, want %q", token, test.wantToken)
				}
				return Identity{ID: 7, Login: "octocat"}, nil
			}
			deps.replaceConfig = func(_ context.Context, request appsetup.ReplaceConfigRequest) appSetupMutation {
				if got := request.Secrets["token"]; got != test.wantReference {
					t.Fatalf("reference = %q, want %q", got, test.wantReference)
				}
				return appSetupMutation{Result: appsetup.ConfigResult{Status: appsetup.MutationUpdated, AppID: "github", Generation: 10}, Outcome: appSetupMutationKnown}
			}

			if err := runAppSetupWithDependencies(t.Context(), []string{"github", "--file", inputPath}, strings.NewReader(""), io.Discard, io.Discard, deps); err != nil {
				t.Fatal(err)
			}
			if ghCalls != test.wantGHCalls || manualCalls != test.wantManualCalls || stores != test.wantStores {
				t.Fatalf("calls gh=%d manual=%d store=%d", ghCalls, manualCalls, stores)
			}
		})
	}
}

func TestAppSetupExplicitReferenceFailureNeverSubstitutesAnotherCredential(t *testing.T) {
	inputPath := writeAppSetupInput(t, `{"config":{"repositories":[{"name":"acme/widget","alias":"W"}]},"secrets":{"token":"keychain://bsbctl/explicit"}}`)
	deps := validAppSetupDependencies(t)
	deps.resolveSecret = func(_ context.Context, reference string) (string, error) {
		if reference != "keychain://bsbctl/explicit" {
			t.Fatalf("resolved unexpected reference %q", reference)
		}
		return "", appsetup.ErrSecretNotFound
	}
	deps.githubToken = func(context.Context) (string, error) { t.Fatal("called gh after explicit failure"); return "", nil }
	deps.readManualToken = func(context.Context, io.Reader, io.Writer, bool) (string, error) {
		t.Fatal("prompted after explicit failure")
		return "", nil
	}
	deps.replaceConfig = func(context.Context, appsetup.ReplaceConfigRequest) appSetupMutation {
		t.Fatal("mutated configuration after explicit failure")
		return appSetupMutation{}
	}

	var stdout, stderr bytes.Buffer
	err := runAppSetupWithDependencies(t.Context(), []string{"github", "--file", inputPath}, strings.NewReader(""), &stdout, &stderr, deps)
	commandErr, ok := errors.AsType[*commandError](err)
	if !ok || commandErr.Kind != exitRejected || stdout.Len() != 0 || strings.Contains(stderr.String()+err.Error(), appSetupCanary) {
		t.Fatalf("error=%#v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

func TestAppSetupManualEndpointRejectionDoesNotPersistCredential(t *testing.T) {
	inputPath := writeAppSetupInput(t, `{"config":{"repositories":[{"name":"acme/widget","alias":"W"}]}}`)
	deps := validAppSetupDependencies(t)
	deps.readManualToken = func(context.Context, io.Reader, io.Writer, bool) (string, error) { return appSetupCanary, nil }
	deps.authorize = func(context.Context, *http.Client, string, []Repository) (Identity, error) {
		return Identity{}, providerRepositorySetupError(t)
	}
	deps.storeSecret = func(context.Context, string, string) error {
		t.Fatal("stored rejected credential")
		return nil
	}

	var stdout, stderr bytes.Buffer
	err := runAppSetupWithDependencies(t.Context(), []string{"github", "--file", inputPath, "--token-stdin"}, strings.NewReader(appSetupCanary+"\n"), &stdout, &stderr, deps)
	commandErr, ok := errors.AsType[*commandError](err)
	if !ok || commandErr.Kind != exitRejected || stdout.Len() != 0 || strings.Contains(stderr.String()+err.Error(), appSetupCanary) {
		t.Fatalf("error=%#v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

func TestAppSetupRejectedExistingAndGitHubCredentialsFallBackOnlyOnDefinitiveFailures(t *testing.T) {
	inputPath := writeAppSetupInput(t, `{"config":{"repositories":[{"name":"acme/widget","alias":"W"}]}}`)
	for _, test := range []struct {
		name         string
		existingErr  error
		ghErr        error
		wantManual   int
		wantCode     appsetup.Kind
		wantSafeHint string
	}{
		{name: "missing existing and unavailable gh", existingErr: appsetup.ErrSecretNotFound, ghErr: errGitHubCLIUnavailable, wantManual: 1},
		{name: "rejected existing and unauthorized gh", existingErr: providerSetupError(t, http.StatusUnauthorized), ghErr: providerSetupError(t, http.StatusForbidden), wantManual: 1, wantSafeHint: "GitHub CLI credential lacks notification access"},
		{name: "repository access from gh", existingErr: appsetup.ErrSecretNotFound, ghErr: providerRepositorySetupError(t), wantManual: 1, wantSafeHint: "GitHub CLI credential lacks configured repository access"},
		{name: "throttled existing stops", existingErr: providerThrottledSetupError(t), wantCode: exitOperational},
		{name: "locked keychain stops", existingErr: errors.New("locked " + appSetupCanary), wantCode: exitOperational},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps := validAppSetupDependencies(t)
			deps.resolveSecret = func(_ context.Context, reference string) (string, error) {
				if reference == "keychain://bsbctl/github-notifications-old" {
					if errors.Is(test.existingErr, appsetup.ErrSecretNotFound) {
						return "", test.existingErr
					}
					if strings.Contains(test.name, "existing") && !strings.Contains(test.name, "missing") {
						return "existing-token", nil
					}
					return "", test.existingErr
				}
				return "manual-token", nil
			}
			deps.authorize = func(_ context.Context, _ *http.Client, token string, _ []Repository) (Identity, error) {
				if token == "manual-token" {
					return Identity{ID: 7, Login: "octocat"}, nil
				}
				if token == "existing-token" {
					return Identity{}, test.existingErr
				}
				return Identity{}, test.ghErr
			}
			deps.githubToken = func(context.Context) (string, error) {
				if test.ghErr != nil && !errors.Is(test.ghErr, errGitHubCLIUnavailable) {
					return "gh-token", nil
				}
				return "", test.ghErr
			}
			manual := 0
			deps.readManualToken = func(context.Context, io.Reader, io.Writer, bool) (string, error) {
				manual++
				return "manual-token", nil
			}
			deps.newReference = func() (string, error) { return "keychain://bsbctl/fresh", nil }
			deps.storeSecret = func(context.Context, string, string) error { return nil }
			deps.replaceConfig = func(_ context.Context, _ appsetup.ReplaceConfigRequest) appSetupMutation {
				return appSetupMutation{Result: appsetup.ConfigResult{Status: appsetup.MutationUpdated, AppID: "github", Generation: 10}, Outcome: appSetupMutationKnown}
			}

			var stdout, stderr bytes.Buffer
			err := runAppSetupWithDependencies(t.Context(), []string{"github", "--file", inputPath}, strings.NewReader(""), &stdout, &stderr, deps)
			if test.wantCode != 0 {
				commandErr, ok := errors.AsType[*commandError](err)
				if !ok || commandErr.Kind != test.wantCode || manual != 0 || stdout.Len() != 0 || strings.Contains(stderr.String(), appSetupCanary) {
					t.Fatalf("error=%#v manual=%d stdout=%q stderr=%q", err, manual, stdout.String(), stderr.String())
				}
				return
			}
			if err != nil || manual != test.wantManual || strings.Contains(stdout.String()+stderr.String(), appSetupCanary) ||
				(test.wantSafeHint != "" && !strings.Contains(stderr.String(), test.wantSafeHint)) {
				t.Fatalf("error=%v manual=%d stdout=%q stderr=%q", err, manual, stdout.String(), stderr.String())
			}
		})
	}
}

func TestAppSetupTokenStdinBypassesExistingAndGitHubAndRejectsAmbiguousInput(t *testing.T) {
	inputPath := writeAppSetupInput(t, `{"config":{"repositories":[{"name":"acme/widget","alias":"W"}]}}`)
	deps := validAppSetupDependencies(t)
	deps.resolveSecret = func(_ context.Context, reference string) (string, error) {
		if reference == "keychain://bsbctl/fresh" {
			return appSetupCanary, nil
		}
		t.Fatalf("resolved precedence source %q", reference)
		return "", nil
	}
	deps.githubToken = func(context.Context) (string, error) { t.Fatal("called gh"); return "", nil }
	deps.readManualToken = func(_ context.Context, input io.Reader, _ io.Writer, hidden bool) (string, error) {
		if hidden {
			t.Fatal("token stdin used hidden input")
		}
		return readAppSetupTokenInput(input)
	}
	deps.authorize = func(_ context.Context, _ *http.Client, token string, _ []Repository) (Identity, error) {
		if token != appSetupCanary {
			t.Fatalf("manual token = %q", token)
		}
		return Identity{ID: 7, Login: "octocat"}, nil
	}
	deps.newReference = func() (string, error) { return "keychain://bsbctl/fresh", nil }
	deps.storeSecret = func(context.Context, string, string) error { return nil }
	deps.replaceConfig = func(_ context.Context, _ appsetup.ReplaceConfigRequest) appSetupMutation {
		return appSetupMutation{Result: appsetup.ConfigResult{Status: appsetup.MutationUpdated, AppID: "github", Generation: 10}, Outcome: appSetupMutationKnown}
	}
	if err := runAppSetupWithDependencies(t.Context(), []string{"github", "--file", inputPath, "--token-stdin"}, strings.NewReader(appSetupCanary+"\n"), io.Discard, io.Discard, deps); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name  string
		args  []string
		input string
	}{
		{name: "config stdin collision", args: []string{"github", "--file", "-", "--token-stdin"}},
		{name: "explicit reference collision", args: []string{"github", "--file", writeAppSetupInput(t, `{"config":{"repositories":[{"name":"acme/widget","alias":"W"}]},"secrets":{"token":"keychain://bsbctl/explicit"}}`), "--token-stdin"}},
		{name: "duplicate boolean", args: []string{"github", "--file", inputPath, "--token-stdin", "--token-stdin"}},
		{name: "valued boolean", args: []string{"github", "--file", inputPath, "--token-stdin=true"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := runAppSetupWithDependencies(t.Context(), test.args, strings.NewReader(test.input), &stdout, &stderr, validAppSetupDependencies(t))
			commandErr, ok := errors.AsType[*commandError](err)
			if !ok || commandErr.Kind != exitUsage || stdout.Len() != 0 {
				t.Fatalf("error=%#v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
		})
	}
}

func TestHiddenAppSetupTokenRestoresEchoAfterCancellationAndRestoreFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		readErr    error
		restoreErr error
	}{
		{name: "canceled read", readErr: context.Canceled},
		{name: "restore failure", restoreErr: errors.New("restore failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			restored := 0
			disable := func(io.Reader) (func() error, error) {
				return func() error { restored++; return test.restoreErr }, nil
			}
			reader := &appSetupFailingReader{err: test.readErr}
			var prompt bytes.Buffer
			_, err := readHiddenAppSetupToken(t.Context(), reader, &prompt, disable)
			if err == nil || restored != 1 || prompt.String() != "GitHub token: " || strings.Contains(err.Error(), appSetupCanary) {
				t.Fatalf("token error=%v restored=%d prompt=%q", err, restored, prompt.String())
			}
		})
	}
}

func TestAppSetupValidatesInstalledIdentityBeforeCredentialAccess(t *testing.T) {
	inputPath := writeAppSetupInput(t, `{"config":{"repositories":[{"name":"acme/widget","alias":"W"}]}}`)
	for _, mutate := range []func(*config.Document, *appsetup.Status){
		func(document *config.Document, _ *appsetup.Status) {
			app := document.Apps["github"]
			app.PluginID = "dev.other"
			document.Apps["github"] = app
		},
		func(_ *config.Document, status *appsetup.Status) { status.Generation++ },
		func(_ *config.Document, status *appsetup.Status) { status.Apps[0].RuntimeGeneration++ },
	} {
		deps := validAppSetupDependencies(t)
		document, _ := deps.loadDocument()
		status, _ := deps.daemonStatus(t.Context())
		mutate(&document, &status)
		deps.loadDocument = func() (config.Document, error) { return document, nil }
		deps.daemonStatus = func(context.Context) (appsetup.Status, error) { return status, nil }
		deps.resolveSecret = func(context.Context, string) (string, error) { t.Fatal("credential accessed"); return "", nil }
		err := runAppSetupWithDependencies(t.Context(), []string{"github", "--file", inputPath}, strings.NewReader(""), io.Discard, io.Discard, deps)
		if commandErr, ok := errors.AsType[*commandError](err); !ok || (commandErr.Kind != exitRejected && commandErr.Kind != exitOperational) {
			t.Fatalf("identity error = %#v", err)
		}
	}
}

func TestAppSetupStorageReadbackAndConfigurationOutcomesRemainTruthfulAndRedacted(t *testing.T) {
	inputPath := writeAppSetupInput(t, `{"config":{"repositories":[{"name":"acme/widget","alias":"W"}]}}`)
	for _, test := range []struct {
		name       string
		storeErr   error
		readback   string
		readErr    error
		mutation   appSetupMutation
		wantCode   appsetup.Kind
		wantStatus string
		wantConfig string
	}{
		{name: "store definitely failed", storeErr: errors.New("store failed " + appSetupCanary), readErr: appsetup.ErrSecretNotFound, wantCode: exitOperational},
		{name: "store outcome unknown", storeErr: errors.New("store failed " + appSetupCanary), readErr: errors.New("readback failed " + appSetupCanary), wantCode: exitPartial, wantStatus: "partial", wantConfig: "not_attempted"},
		{name: "successful store unreadable", readErr: errors.New("readback failed " + appSetupCanary), wantCode: exitPartial, wantStatus: "partial", wantConfig: "not_attempted"},
		{name: "configuration partial", readback: appSetupCanary, mutation: appSetupMutation{Result: appsetup.ConfigResult{Status: appsetup.MutationPartial, AppID: "github", Generation: 10}, Outcome: appSetupMutationKnown}, wantCode: exitPartial, wantStatus: "partial", wantConfig: "partial"},
		{name: "configuration durability uncertain", readback: appSetupCanary, mutation: appSetupMutation{Result: appsetup.ConfigResult{Status: appsetup.MutationDurabilityUncertain, AppID: "github", Generation: 10}, Outcome: appSetupMutationKnown}, wantCode: exitPartial, wantStatus: "partial", wantConfig: "durability_uncertain"},
		{name: "configuration rejected after store", readback: appSetupCanary, mutation: appSetupMutation{Outcome: appSetupMutationKnown, Err: commandFailure(exitRejected, "rejected")}, wantCode: exitPartial, wantStatus: "partial", wantConfig: "not_applied"},
		{name: "configuration response lost", readback: appSetupCanary, mutation: appSetupMutation{Outcome: appSetupMutationUnknown, Err: errors.New("rpc failed " + appSetupCanary)}, wantCode: exitPartial, wantStatus: "partial", wantConfig: "unknown"},
		{name: "configuration response invalid", readback: appSetupCanary, mutation: appSetupMutation{Outcome: appSetupMutationKnown}, wantCode: exitPartial, wantStatus: "partial", wantConfig: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps := validAppSetupDependencies(t)
			document, _ := deps.loadDocument()
			app := document.Apps["github"]
			app.Secrets = nil
			document.Apps["github"] = app
			deps.loadDocument = func() (config.Document, error) { return document, nil }
			deps.githubToken = func(context.Context) (string, error) { return appSetupCanary, nil }
			deps.authorize = func(context.Context, *http.Client, string, []Repository) (Identity, error) {
				return Identity{ID: 7, Login: "octocat"}, nil
			}
			deps.newReference = func() (string, error) { return "keychain://bsbctl/fresh", nil }
			deps.storeSecret = func(context.Context, string, string) error { return test.storeErr }
			deps.resolveSecret = func(context.Context, string) (string, error) {
				return test.readback, test.readErr
			}
			calledReplace := false
			deps.replaceConfig = func(context.Context, appsetup.ReplaceConfigRequest) appSetupMutation {
				calledReplace = true
				return test.mutation
			}
			var stdout, stderr bytes.Buffer
			err := runAppSetupWithDependencies(t.Context(), []string{"github", "--file", inputPath}, strings.NewReader(""), &stdout, &stderr, deps)
			commandErr, ok := errors.AsType[*commandError](err)
			if !ok || commandErr.Kind != test.wantCode || strings.Contains(stdout.String()+stderr.String()+err.Error(), appSetupCanary) {
				t.Fatalf("error=%#v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			if test.wantStatus == "" {
				if stdout.Len() != 0 || calledReplace {
					t.Fatalf("definite failure stdout=%q replace=%t", stdout.String(), calledReplace)
				}
				return
			}
			var output appSetupResult
			if json.Unmarshal(stdout.Bytes(), &output) != nil || output.Status != test.wantStatus || output.ConfigurationStatus != test.wantConfig || output.SecretReference != "keychain://bsbctl/fresh" {
				t.Fatalf("output=%q decoded=%#v", stdout.String(), output)
			}
			if test.wantConfig == "not_attempted" && calledReplace {
				t.Fatal("configuration mutation attempted without verified readback")
			}
		})
	}
}

func TestAppSetupReusedReferencePreservesKnownConfigurationFailure(t *testing.T) {
	for _, test := range []struct {
		name         string
		explicit     bool
		mutationErr  error
		wantKind     appsetup.Kind
		wantCanceled bool
	}{
		{name: "explicit invalid", explicit: true, mutationErr: commandFailure(exitUsage, "invalid"), wantKind: exitUsage},
		{name: "existing rejected", mutationErr: commandFailure(exitRejected, "rejected"), wantKind: exitRejected},
		{name: "explicit operational", explicit: true, mutationErr: commandFailure(exitOperational, "operational"), wantKind: exitOperational},
		{name: "existing canceled", mutationErr: context.Canceled, wantCanceled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			secretsInput := ""
			if test.explicit {
				secretsInput = `,"secrets":{"token":"keychain://bsbctl/explicit"}`
			}
			inputPath := writeAppSetupInput(t, `{"config":{"repositories":[{"name":"acme/widget","alias":"W"}]}`+secretsInput+`}`)
			deps := validAppSetupDependencies(t)
			deps.storeSecret = func(context.Context, string, string) error {
				t.Fatal("stored a reused credential")
				return nil
			}
			deps.replaceConfig = func(context.Context, appsetup.ReplaceConfigRequest) appSetupMutation {
				return appSetupMutation{Outcome: appSetupMutationKnown, Err: test.mutationErr}
			}
			var stdout bytes.Buffer
			err := runAppSetupWithDependencies(t.Context(), []string{"github", "--file", inputPath}, strings.NewReader(""), &stdout, io.Discard, deps)
			failure, classified := errors.AsType[*appsetup.Error](err)
			if classified != (test.wantKind != 0) || classified && failure.Kind != test.wantKind || errors.Is(err, context.Canceled) != test.wantCanceled || stdout.Len() != 0 {
				t.Fatalf("error=%v stdout=%q", err, stdout.String())
			}
		})
	}
}

func TestFinishAppSetupMutationReportsPartialForFreshSecretOrUnknownConfiguration(t *testing.T) {
	for _, test := range []struct {
		name         string
		freshlySaved bool
		outcome      appSetupMutationOutcome
		wantStatus   string
	}{
		{name: "fresh secret and known noncommit", freshlySaved: true, outcome: appSetupMutationKnown, wantStatus: "not_applied"},
		{name: "reused secret and unknown outcome", outcome: appSetupMutationUnknown, wantStatus: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := finishAppSetupMutation(&stdout, io.Discard, "github", "keychain://bsbctl/reference", test.freshlySaved, appSetupMutation{
				Outcome: test.outcome,
				Err:     commandFailure(exitRejected, "rejected"),
			})
			failure, ok := errors.AsType[*appsetup.Error](err)
			var result appSetupResult
			decodeErr := json.Unmarshal(stdout.Bytes(), &result)
			if !ok || failure.Kind != exitPartial || decodeErr != nil || result.Status != "partial" || result.ConfigurationStatus != test.wantStatus || result.SecretReference != "keychain://bsbctl/reference" {
				t.Fatalf("error=%v stdout=%q result=%#v decode=%v", err, stdout.String(), result, decodeErr)
			}
		})
	}
}

func TestCaptureGitHubTokenUsesBoundedExactCommandAndRedactsRunnerFailures(t *testing.T) {
	for _, test := range []struct {
		name      string
		output    string
		err       error
		wantToken string
	}{
		{name: "token", output: appSetupCanary + "\n", wantToken: appSetupCanary},
		{name: "runner error", output: appSetupCanary, err: errors.New("subprocess failed " + appSetupCanary)},
		{name: "oversized", output: strings.Repeat("x", maxAppSetupTokenBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := func(_ context.Context, name string, args []string, stdout io.Writer) error {
				if name != "gh" || !reflect.DeepEqual(args, []string{"auth", "token", "--hostname", "github.com"}) {
					t.Fatalf("command = %q %q", name, args)
				}
				_, _ = io.WriteString(stdout, test.output)
				return test.err
			}
			token, err := captureGitHubToken(t.Context(), runner)
			if token != test.wantToken {
				t.Fatalf("token = %q", token)
			}
			if test.wantToken == "" && (err == nil || strings.Contains(err.Error(), appSetupCanary)) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func validAppSetupDependencies(t *testing.T) appSetupDependencies {
	t.Helper()
	policies := map[string]presentation.PolicyConfig{"summary": {Policy: presentation.PolicyRotation, RotationIntervalMS: 60_000}}
	document := config.Document{
		Version: config.CurrentVersion, Generation: 9,
		Plugins: map[string]config.Plugin{PluginID: {ID: PluginID}},
		Apps: map[string]config.App{"github": {
			ID: "github", PluginID: PluginID, Generation: 9, Enabled: true, LaunchAction: "open",
			Config:  json.RawMessage(`{"repositories":[{"name":"acme/old","alias":"OLD"}]}`),
			Secrets: map[string]string{"token": "keychain://bsbctl/github-notifications-old"}, Policies: policies,
		}},
	}
	status := appsetup.Status{Generation: 9, Apps: []appsetup.AppStatus{{AppID: "github", PluginID: PluginID, RuntimeGeneration: 9}}}
	return appSetupDependencies{
		readConfiguration: readTestAppSetupConfiguration,
		loadDocument:      func() (config.Document, error) { return document, nil },
		daemonStatus:      func(context.Context) (appsetup.Status, error) { return status, nil },
		resolveSecret:     func(context.Context, string) (string, error) { return "existing-token", nil },
		storeSecret:       func(context.Context, string, string) error { return nil },
		authorize: func(context.Context, *http.Client, string, []Repository) (Identity, error) {
			return Identity{ID: 7, Login: "octocat"}, nil
		},
		githubToken:     func(context.Context) (string, error) { return "gh-token", nil },
		readManualToken: func(context.Context, io.Reader, io.Writer, bool) (string, error) { return "manual-token", nil },
		newReference:    func() (string, error) { return "keychain://bsbctl/fresh", nil },
		replaceConfig: func(_ context.Context, _ appsetup.ReplaceConfigRequest) appSetupMutation {
			return appSetupMutation{Result: appsetup.ConfigResult{Status: appsetup.MutationUpdated, AppID: "github", Generation: 10}, Outcome: appSetupMutationKnown}
		},
	}
}

func readTestAppSetupConfiguration(path string, _ io.Reader) (appsetup.Configuration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return appsetup.Configuration{}, err
	}
	var input struct {
		Config       json.RawMessage                      `json:"config"`
		Secrets      map[string]string                    `json:"secrets,omitempty"`
		Policies     map[string]presentation.PolicyConfig `json:"policies,omitempty"`
		LaunchAction *string                              `json:"launch_action,omitempty"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return appsetup.Configuration{}, err
	}
	result := appsetup.Configuration{Config: input.Config, Secrets: input.Secrets, Policies: input.Policies}
	if input.LaunchAction != nil {
		result.LaunchAction = *input.LaunchAction
		result.LaunchActionProvided = true
	}
	return result, nil
}

func writeAppSetupInput(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "setup.json")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type appSetupFailingReader struct{ err error }

func (r *appSetupFailingReader) Read([]byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	return 0, io.EOF
}

type appSetupRoundTripper func(*http.Request) (*http.Response, error)

func (f appSetupRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func providerSetupError(t *testing.T, status int) error {
	t.Helper()
	_, err := Authorize(t.Context(), &http.Client{Transport: appSetupRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}, "candidate", []Repository{{Name: "acme/widget", Alias: "W"}})
	return err
}

func providerRepositorySetupError(t *testing.T) error {
	t.Helper()
	calls := 0
	_, err := Authorize(t.Context(), &http.Client{Transport: appSetupRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		body, status := `[{"id":1}]`, http.StatusOK
		if calls == 1 {
			body = `{"id":7,"login":"octocat"}`
		} else if calls == 3 {
			body, status = `{}`, http.StatusNotFound
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}, "candidate", []Repository{{Name: "acme/widget", Alias: "W"}})
	return err
}

func providerThrottledSetupError(t *testing.T) error {
	t.Helper()
	_, err := Authorize(t.Context(), &http.Client{Transport: appSetupRoundTripper(func(*http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Retry-After", "60")
		return &http.Response{StatusCode: http.StatusForbidden, Header: header, Body: io.NopCloser(strings.NewReader(`{"message":"rate limit"}`))}, nil
	})}, "candidate", []Repository{{Name: "acme/widget", Alias: "W"}})
	return err
}
