package githubnotifications

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/internal/appsetup"
	"github.com/lxdb/bsbctl/internal/cliinput"
	"github.com/lxdb/bsbctl/internal/config"
)

const maxAppSetupTokenBytes = 1024

var errGitHubCLIUnavailable = errors.New("GitHub CLI credential is unavailable")

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

const (
	exitUsage       = appsetup.Usage
	exitRejected    = appsetup.Rejected
	exitOperational = appsetup.Operational
	exitPartial     = appsetup.Partial
)

type commandError = appsetup.Error
type appSetupMutation = appsetup.Mutation
type appSetupMutationOutcome = appsetup.MutationOutcome

const (
	appSetupMutationKnown   = appsetup.MutationKnown
	appSetupMutationUnknown = appsetup.MutationUnknown
)

func commandFailure(kind appsetup.Kind, message string) error {
	return appsetup.Failure(kind, message)
}

type appSetupDependencies struct {
	readConfiguration func(string, io.Reader) (appsetup.Configuration, error)
	loadDocument      func() (config.Document, error)
	daemonStatus      func(context.Context) (appsetup.Status, error)
	resolveSecret     func(context.Context, string) (string, error)
	storeSecret       func(context.Context, string, string) error
	authorize         func(context.Context, *http.Client, string, []Repository) (Identity, error)
	httpClient        *http.Client
	githubToken       func(context.Context) (string, error)
	readManualToken   func(context.Context, io.Reader, io.Writer, bool) (string, error)
	newReference      func() (string, error)
	replaceConfig     func(context.Context, appsetup.ReplaceConfigRequest) appSetupMutation
}

type appSetupResult struct {
	Status              string `json:"status"`
	AppID               string `json:"app_id"`
	Generation          uint64 `json:"generation,omitzero"`
	SecretReference     string `json:"secret_reference"`
	ConfigurationStatus string `json:"configuration_status,omitempty"`
}

func productionAppSetupDependencies(host appsetup.Host) appSetupDependencies {
	return appSetupDependencies{
		readConfiguration: host.ReadConfiguration,
		loadDocument:      host.LoadDocument,
		daemonStatus:      host.DaemonStatus,
		resolveSecret:     host.ResolveSecret,
		storeSecret:       host.StoreSecret,
		authorize:         Authorize,
		githubToken: func(ctx context.Context) (string, error) {
			return captureGitHubToken(ctx, runGitHubTokenCommand)
		},
		readManualToken: func(ctx context.Context, input io.Reader, output io.Writer, hidden bool) (string, error) {
			if hidden {
				if !setupHasTerminal(input) {
					return "", commandFailure(exitUsage, "GitHub token requires a terminal or --token-stdin")
				}
				return readHiddenAppSetupToken(ctx, input, output, disableAppSetupTerminalEcho)
			}
			return readAppSetupTokenInput(input)
		},
		newReference:  newAppSetupReference,
		replaceConfig: host.ReplaceConfiguration,
	}
}

// RunSetup configures an installed GitHub Notifications app without exposing
// provider credential policy to the core CLI.
func RunSetup(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, host appsetup.Host) error {
	return runAppSetupWithDependencies(ctx, args, stdin, stdout, stderr, productionAppSetupDependencies(host))
}

func runAppSetupWithDependencies(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, dependencies appSetupDependencies) error {
	appID, inputPath, tokenStdin, err := parseAppSetupArgs(args)
	if err != nil {
		return err
	}
	input, err := dependencies.readConfiguration(inputPath, stdin)
	if err != nil {
		return err
	}
	providerConfig, err := DecodeConfig(input.Config)
	if err != nil || !providerConfig.Configured {
		return commandFailure(exitUsage, "GitHub Notifications configuration must be complete")
	}
	explicitReference, err := explicitAppSetupReference(input.Secrets)
	if err != nil {
		return err
	}
	if tokenStdin && explicitReference != "" {
		return commandFailure(exitUsage, "--token-stdin cannot be combined with secrets.token")
	}

	document, err := dependencies.loadDocument()
	if err != nil {
		return commandFailure(exitOperational, "load daemon configuration failed")
	}
	existing, ok := document.Apps[appID]
	if !ok || existing.PluginID != PluginID {
		return commandFailure(exitRejected, "app setup supports only installed GitHub Notifications instances")
	}
	if _, ok := document.Plugins[PluginID]; !ok {
		return commandFailure(exitRejected, "GitHub Notifications plugin is not installed")
	}
	status, err := dependencies.daemonStatus(ctx)
	if err != nil {
		return err
	}
	if !matchingAppSetupStatus(document, existing, status) {
		return commandFailure(exitOperational, "daemon configuration changed; retry app setup")
	}
	if input.Policies == nil {
		input.Policies = existing.Policies
	}
	if !input.LaunchActionProvided {
		input.LaunchAction = existing.LaunchAction
	}

	reference, imported, err := selectAppSetupCredential(ctx, stdin, stderr, tokenStdin, explicitReference, existing, providerConfig.Repositories, dependencies)
	if err != nil {
		return err
	}
	freshlySaved := false
	if imported != "" {
		var partial bool
		reference, partial, err = storeAppSetupCredential(ctx, imported, reference, dependencies)
		imported = ""
		if err != nil {
			if partial {
				return writeAppSetupPartial(stdout, appID, reference, "not_attempted", "GitHub credential storage outcome requires recovery")
			}
			return err
		}
		freshlySaved = true
	}

	mutation := dependencies.replaceConfig(ctx, appsetup.ReplaceConfigRequest{
		AppID: appID, ExpectedGeneration: document.Generation, Config: input.Config, Secrets: map[string]string{"token": reference},
		Policies: input.Policies, LaunchAction: input.LaunchAction,
	})
	return finishAppSetupMutation(stdout, stderr, appID, reference, freshlySaved, mutation)
}

func parseAppSetupArgs(args []string) (appID, inputPath string, tokenStdin bool, err error) {
	positionals := make([]string, 0, 1)
	fileSeen, tokenSeen := false, false
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			positionals = append(positionals, arg)
			continue
		}
		switch {
		case arg == "--token-stdin":
			if tokenSeen {
				return "", "", false, commandFailure(exitUsage, "invalid app setup flags")
			}
			tokenSeen, tokenStdin = true, true
		case arg == "--file":
			if fileSeen || index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return "", "", false, commandFailure(exitUsage, "invalid app setup flags")
			}
			index++
			fileSeen, inputPath = true, args[index]
		case strings.HasPrefix(arg, "--file="):
			if fileSeen {
				return "", "", false, commandFailure(exitUsage, "invalid app setup flags")
			}
			fileSeen, inputPath = true, strings.TrimPrefix(arg, "--file=")
		default:
			return "", "", false, commandFailure(exitUsage, "invalid app setup flags")
		}
	}
	if len(positionals) != 1 || !fileSeen || inputPath == "" || tokenStdin && inputPath == "-" {
		return "", "", false, commandFailure(exitUsage, "app setup requires APP-ID and --file CONFIG")
	}
	return positionals[0], inputPath, tokenStdin, nil
}

func explicitAppSetupReference(input map[string]string) (string, error) {
	if len(input) == 0 {
		return "", nil
	}
	if len(input) != 1 || input["token"] == "" {
		return "", commandFailure(exitUsage, "GitHub Notifications setup accepts only secrets.token")
	}
	return input["token"], nil
}

func matchingAppSetupStatus(document config.Document, app config.App, status appsetup.Status) bool {
	if status.Generation != document.Generation {
		return false
	}
	for _, current := range status.Apps {
		if current.AppID == app.ID {
			return current.PluginID == app.PluginID && current.RuntimeGeneration == app.Generation
		}
	}
	return false
}

func selectAppSetupCredential(
	ctx context.Context,
	stdin io.Reader,
	stderr io.Writer,
	tokenStdin bool,
	explicitReference string,
	existing config.App,
	repositories []Repository,
	dependencies appSetupDependencies,
) (reference, imported string, err error) {
	if explicitReference != "" {
		token, resolveErr := dependencies.resolveSecret(ctx, explicitReference)
		if resolveErr != nil {
			if errors.Is(resolveErr, appsetup.ErrSecretNotFound) {
				return "", "", commandFailure(exitRejected, "explicit GitHub credential was not found")
			}
			return "", "", appSetupDependencyError(resolveErr, "read explicit GitHub credential failed")
		}
		if _, authorizeErr := dependencies.authorize(ctx, dependencies.httpClient, token, repositories); authorizeErr != nil {
			token = ""
			return "", "", appSetupAuthorizationError(authorizeErr)
		}
		token = ""
		return explicitReference, "", nil
	}
	if !tokenStdin {
		if currentConfig, decodeErr := DecodeConfig(existing.Config); decodeErr == nil && currentConfig.Configured {
			if existingReference := existing.Secrets["token"]; existingReference != "" {
				token, resolveErr := dependencies.resolveSecret(ctx, existingReference)
				switch {
				case resolveErr == nil:
					_, authorizeErr := dependencies.authorize(ctx, dependencies.httpClient, token, repositories)
					token = ""
					if authorizeErr == nil {
						return existingReference, "", nil
					}
					if !definitiveAppSetupRejection(authorizeErr) {
						return "", "", appSetupAuthorizationError(authorizeErr)
					}
				case errors.Is(resolveErr, appsetup.ErrSecretNotFound):
				default:
					return "", "", appSetupDependencyError(resolveErr, "read existing GitHub credential failed")
				}
			}
		}

		token, ghErr := dependencies.githubToken(ctx)
		if ghErr == nil {
			_, authorizeErr := dependencies.authorize(ctx, dependencies.httpClient, token, repositories)
			if authorizeErr == nil {
				return "", token, nil
			}
			token = ""
			if !definitiveAppSetupRejection(authorizeErr) {
				return "", "", appSetupAuthorizationError(authorizeErr)
			}
			message := "GitHub CLI credential lacks notification access; enter a different token"
			if ErrorCode(authorizeErr) == "repository_access_required" {
				message = "GitHub CLI credential lacks configured repository access; enter a different token"
			}
			if _, writeErr := fmt.Fprintln(stderr, message); writeErr != nil {
				return "", "", commandFailure(exitOperational, "write credential prompt failed")
			}
		} else if errors.Is(ghErr, context.Canceled) || errors.Is(ghErr, context.DeadlineExceeded) {
			return "", "", ghErr
		}
	}

	token, readErr := dependencies.readManualToken(ctx, stdin, stderr, !tokenStdin)
	if readErr != nil {
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
			return "", "", readErr
		}
		if typed, ok := errors.AsType[*commandError](readErr); ok {
			return "", "", typed
		}
		return "", "", commandFailure(exitOperational, "read GitHub token failed")
	}
	if _, authorizeErr := dependencies.authorize(ctx, dependencies.httpClient, token, repositories); authorizeErr != nil {
		token = ""
		return "", "", appSetupAuthorizationError(authorizeErr)
	}
	return "", token, nil
}

func definitiveAppSetupRejection(err error) bool {
	return IsCredentialRejected(err) || ErrorCode(err) == "repository_access_required"
}

func appSetupAuthorizationError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if definitiveAppSetupRejection(err) {
		return commandFailure(exitRejected, "GitHub credential lacks required notification or repository access")
	}
	return commandFailure(exitOperational, "GitHub credential validation failed")
}

func appSetupDependencyError(err error, message string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return commandFailure(exitOperational, message)
}

func storeAppSetupCredential(ctx context.Context, token, reference string, dependencies appSetupDependencies) (string, bool, error) {
	if reference == "" {
		var err error
		reference, err = dependencies.newReference()
		if err != nil {
			return "", false, commandFailure(exitOperational, "create GitHub credential reference failed")
		}
	}
	storeErr := dependencies.storeSecret(ctx, reference, token)
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	stored, readErr := dependencies.resolveSecret(readCtx, reference)
	match := readErr == nil && subtle.ConstantTimeCompare([]byte(stored), []byte(token)) == 1
	stored = ""
	token = ""
	if match {
		if storeErr != nil && (errors.Is(storeErr, context.Canceled) || errors.Is(storeErr, context.DeadlineExceeded)) {
			return reference, true, commandFailure(exitPartial, "GitHub credential was stored before cancellation")
		}
		return reference, false, nil
	}
	if storeErr != nil && errors.Is(readErr, appsetup.ErrSecretNotFound) {
		return "", false, appSetupDependencyError(storeErr, "store GitHub credential failed")
	}
	return reference, true, commandFailure(exitPartial, "GitHub credential storage outcome requires recovery")
}

func writeAppSetupPartial(stdout io.Writer, appID, reference, configurationStatus, message string) error {
	if err := writeJSON(stdout, appSetupResult{
		Status: "partial", AppID: appID, SecretReference: reference, ConfigurationStatus: configurationStatus,
	}); err != nil {
		return err
	}
	return commandFailure(exitPartial, message)
}

func finishAppSetupMutation(stdout, stderr io.Writer, appID, reference string, freshlySaved bool, mutation appSetupMutation) error {
	if mutation.Err != nil {
		if mutation.Outcome == appSetupMutationKnown && !freshlySaved {
			return mutation.Err
		}
		status := "not_applied"
		if mutation.Outcome == appSetupMutationUnknown {
			status = "unknown"
		}
		return writeAppSetupPartial(stdout, appID, reference, status, "GitHub Notifications configuration outcome requires recovery")
	}
	if mutation.Result.Status == appsetup.MutationPartial || mutation.Result.Status == appsetup.MutationDurabilityUncertain {
		if err := writeJSON(stdout, appSetupResult{
			Status: "partial", AppID: appID, Generation: mutation.Result.Generation, SecretReference: reference,
			ConfigurationStatus: string(mutation.Result.Status),
		}); err != nil {
			return err
		}
		return commandFailure(exitPartial, "GitHub Notifications configuration requires recovery")
	}
	if mutation.Result.AppID != appID || mutation.Result.Generation == 0 ||
		(mutation.Result.Status != appsetup.MutationUpdated && mutation.Result.Status != appsetup.MutationUnchanged) {
		return writeAppSetupPartial(stdout, appID, reference, "unknown", "GitHub Notifications configuration outcome requires recovery")
	}
	if err := writeJSON(stdout, appSetupResult{
		Status: "configured", AppID: appID, Generation: mutation.Result.Generation, SecretReference: reference,
	}); err != nil {
		return err
	}
	if mutation.CloseWarning {
		_, _ = fmt.Fprintln(stderr, "bsbctl: warning: operation completed but daemon connection close failed")
	}
	return nil
}

type appSetupCommandRunner func(context.Context, string, []string, io.Writer) error

func runGitHubTokenCommand(ctx context.Context, name string, args []string, stdout io.Writer) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = stdout
	command.Stderr = io.Discard
	return command.Run()
}

type boundedAppSetupOutput struct {
	data     []byte
	overflow bool
}

func (output *boundedAppSetupOutput) Write(value []byte) (int, error) {
	remaining := maxAppSetupTokenBytes + 1 - len(output.data)
	if remaining > 0 {
		output.data = append(output.data, value[:min(len(value), remaining)]...)
	}
	if len(value) > remaining || len(output.data) > maxAppSetupTokenBytes {
		output.overflow = true
	}
	return len(value), nil
}

func captureGitHubToken(ctx context.Context, run appSetupCommandRunner) (string, error) {
	var output boundedAppSetupOutput
	if err := run(ctx, "gh", []string{"auth", "token", "--hostname", "github.com"}, &output); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", errGitHubCLIUnavailable
	}
	if output.overflow {
		return "", errGitHubCLIUnavailable
	}
	token, err := parseAppSetupToken(output.data)
	if err != nil {
		return "", errGitHubCLIUnavailable
	}
	return token, nil
}

func readAppSetupTokenInput(input io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxAppSetupTokenBytes+1))
	if err != nil {
		return "", err
	}
	return parseAppSetupToken(data)
}

func readHiddenAppSetupToken(ctx context.Context, input io.Reader, output io.Writer, disableEcho func(io.Reader) (func() error, error)) (token string, err error) {
	restore, err := disableEcho(input)
	if err != nil {
		return "", commandFailure(exitOperational, "disable terminal echo failed")
	}
	defer func() {
		err = errors.Join(err, restore())
	}()
	if _, err := fmt.Fprint(output, "GitHub token: "); err != nil {
		return "", commandFailure(exitOperational, "write credential prompt failed")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(io.LimitReader(input, maxAppSetupTokenBytes+1)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return parseAppSetupToken([]byte(line))
}

func parseAppSetupToken(data []byte) (string, error) {
	if len(data) == 0 || len(data) > maxAppSetupTokenBytes {
		return "", commandFailure(exitRejected, "GitHub token input is invalid")
	}
	data = bytes.TrimSuffix(data, []byte("\n"))
	data = bytes.TrimSuffix(data, []byte("\r"))
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.ContainsAny(data, "\r\n\x00") || !bytes.Equal(trimmed, data) {
		return "", commandFailure(exitRejected, "GitHub token input is invalid")
	}
	return string(data), nil
}

func newAppSetupReference() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return "keychain://bsbctl/github-notifications-" + hex.EncodeToString(suffix[:]), nil
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return commandFailure(exitOperational, "write output failed")
	}
	return nil
}
