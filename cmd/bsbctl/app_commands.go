package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/pluginverify"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/internal/secrets"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

const maxPluginConfigInput = 256 << 10

func callStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	options, positionals, err := parseOptions(args, "socket")
	if err != nil || len(positionals) != 0 {
		return commandFailure(exitUsage, "status accepts only --socket PATH")
	}
	socketPath, err := resolveStatePath(options, "socket", "ctl.sock")
	if err != nil {
		return err
	}
	var status control.Status
	if err := callDaemon(ctx, socketPath, "daemon.status", nil, &status); err != nil {
		return err
	}
	return writeJSON(stdout, status)
}

func runApp(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runAppWithSetup(ctx, args, stdin, stdout, stderr, runAppSetup)
}

func runAppWithSetup(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	setup func(context.Context, []string, io.Reader, io.Writer, io.Writer) error,
) error {
	if len(args) == 0 {
		return commandFailure(exitUsage, "app command requires list, enable, disable, create, delete, setup, or launch")
	}
	switch args[0] {
	case "list", "status", "enable", "disable", "config", "query", "command":
		return runAppInstance(ctx, args, stdin, stdout, stderr)
	case "setup":
		return setup(ctx, args[1:], stdin, stdout, stderr)
	case "launch":
		options, positionals, err := parseOptions(args[1:], "socket")
		if err != nil || len(positionals) < 1 || len(positionals) > 2 {
			return commandFailure(exitUsage, "app launch requires APP-ID and optional ACTION")
		}
		request := control.LaunchRequest{AppID: positionals[0]}
		if len(positionals) == 2 {
			request.Action = positionals[1]
		}
		socketPath, err := resolveStatePath(options, "socket", "ctl.sock")
		if err != nil {
			return err
		}
		if err := callDaemon(ctx, socketPath, "app.launch", request, nil); err != nil {
			return err
		}
		return writeJSON(stdout, struct {
			Status string `json:"status"`
			AppID  string `json:"app_id"`
			Action string `json:"action,omitempty"`
		}{Status: "launched", AppID: request.AppID, Action: request.Action})
	case "create":
		options, positionals, err := parseOptions(args[1:], "socket", "plugin", "file", "enabled")
		if err != nil || len(positionals) != 1 {
			return commandFailure(exitUsage, "app create requires one APP-ID")
		}
		request, err := appCreateRequest(positionals[0], options, stdin)
		if err != nil {
			return err
		}
		socketPath, err := resolveStatePath(options, "socket", "ctl.sock")
		if err != nil {
			return err
		}
		var result control.AppInstanceResult
		closeWarning, callErr := callDaemonResult(ctx, socketPath, "app.create", request, &result)
		return finishAppInstanceMutation(stdout, stderr, result, closeWarning, callErr)
	case "delete":
		options, positionals, err := parseOptions(args[1:], "socket")
		if err != nil || len(positionals) != 1 {
			return commandFailure(exitUsage, "app delete requires one APP-ID")
		}
		socketPath, err := resolveStatePath(options, "socket", "ctl.sock")
		if err != nil {
			return err
		}
		var result control.AppInstanceResult
		closeWarning, callErr := callDaemonResult(ctx, socketPath, "app.delete", control.DeleteAppRequest{AppID: positionals[0]}, &result)
		return finishAppInstanceMutation(stdout, stderr, result, closeWarning, callErr)
	default:
		return commandFailure(exitUsage, "invalid app command")
	}
}

func appCreateRequest(appID string, options map[string]string, stdin io.Reader) (control.CreateAppRequest, error) {
	enabled := true
	if value := options["enabled"]; value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return control.CreateAppRequest{}, commandFailure(exitUsage, "app create enabled value must be true or false")
		}
		enabled = parsed
	}
	pluginID, filePath := options["plugin"], options["file"]
	if (pluginID == "") != (filePath == "") {
		return control.CreateAppRequest{}, commandFailure(exitUsage, "custom app creation requires both --plugin PLUGIN-ID and --file PATH|-")
	}
	if pluginID == "" {
		descriptor, ok := firstpartyplugins.LookupAppID(appID)
		if !ok {
			return control.CreateAppRequest{}, commandFailure(exitUsage, "no built-in default exists for APP-ID; provide --plugin and --file")
		}
		app := descriptor.DefaultApp
		return control.CreateAppRequest{
			AppID: app.ID, PluginID: app.PluginID, Enabled: enabled,
			Config: app.Config, Secrets: app.Secrets, Policies: app.Policies, LaunchAction: app.LaunchAction,
		}, nil
	}
	replacement, err := readAppConfiguration(filePath, stdin)
	if err != nil {
		if failure, ok := errors.AsType[*inputFailure](err); ok && failure.operational {
			return control.CreateAppRequest{}, commandFailure(exitOperational, "read app configuration failed")
		}
		return control.CreateAppRequest{}, commandFailure(exitUsage, "app configuration input is invalid")
	}
	return control.CreateAppRequest{
		AppID: appID, PluginID: pluginID, Enabled: enabled,
		Config: replacement.Config, Secrets: replacement.Secrets, Policies: replacement.Policies,
		LaunchAction: replacement.LaunchAction,
	}, nil
}

func finishAppInstanceMutation(stdout, stderr io.Writer, result control.AppInstanceResult, closeWarning bool, callErr error) error {
	if callErr != nil {
		return callErr
	}
	if err := writeJSON(stdout, result); err != nil {
		return err
	}
	if result.Status == control.MutationDurabilityUncertain || result.Status == control.MutationPartial {
		return commandFailure(exitPartial, "configuration durability is uncertain")
	}
	if closeWarning {
		_, _ = fmt.Fprintln(stderr, "bsbctl: warning: operation completed but daemon connection close failed")
	}
	return nil
}

type appInstanceStatus struct {
	AppID       string                   `json:"app_id"`
	PluginID    string                   `json:"plugin_id"`
	Enabled     bool                     `json:"enabled"`
	Generation  uint64                   `json:"generation"`
	Readiness   daemon.AppReadinessPhase `json:"readiness"`
	PluginPhase pluginhost.Phase         `json:"plugin_phase"`
	Healthy     bool                     `json:"healthy"`
}

func runPlugin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return commandFailure(exitUsage, "plugin command requires list, status, install, update, rollback, or verify")
	}
	if args[0] == "verify" {
		options, positionals, err := parseOptions(args[1:], "manifest", "fixture", "executable")
		if err != nil || len(positionals) != 0 || options["manifest"] == "" || options["fixture"] == "" {
			return commandFailure(exitUsage, "plugin verify requires --manifest PATH and --fixture PATH")
		}
		report, verifyErr := pluginverify.Verify(ctx, pluginverify.Options{
			ManifestPath: options["manifest"], FixturePath: options["fixture"], ExecutablePath: options["executable"], CoreVersion: version,
		})
		if err := writeJSON(stdout, report); err != nil {
			return err
		}
		if verifyErr != nil {
			return commandFailure(exitRejected, "plugin verification failed")
		}
		return nil
	}
	return runPluginPackages(ctx, args, stdout, stderr)
}

func runAppInstance(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	command := args[0]
	allowed := []string{"socket"}
	if command == "config" || command == "query" || command == "command" {
		allowed = append(allowed, "file")
	}
	options, positionals, err := parseOptions(args[1:], allowed...)
	if err != nil {
		return commandFailure(exitUsage, "invalid app flags")
	}
	socketPath, err := resolveStatePath(options, "socket", "ctl.sock")
	if err != nil {
		return err
	}
	switch command {
	case "list", "status":
		if (command == "list" && len(positionals) != 0) || (command == "status" && len(positionals) != 1) {
			return commandFailure(exitUsage, "invalid app status arguments")
		}
		var status control.Status
		if err := callDaemon(ctx, socketPath, "daemon.status", nil, &status); err != nil {
			return err
		}
		apps := publicAppStatuses(status)
		if command == "status" && len(positionals) == 1 {
			for _, app := range apps {
				if app.AppID == positionals[0] {
					return writeJSON(stdout, app)
				}
			}
			return commandFailure(exitRejected, "app instance was not found")
		}
		return writeJSON(stdout, struct {
			Apps []appInstanceStatus `json:"apps"`
		}{Apps: apps})
	case "enable", "disable":
		if len(positionals) != 1 {
			return commandFailure(exitUsage, "app enablement requires one APP-ID")
		}
		var result control.AppMutationResult
		closeWarning, callErr := callDaemonResult(ctx, socketPath, "app.set_enabled", control.SetEnabledRequest{AppID: positionals[0], Enabled: command == "enable"}, &result)
		if callErr != nil {
			return callErr
		}
		if err := writeJSON(stdout, result); err != nil {
			return err
		}
		if result.Status == control.MutationDurabilityUncertain || result.Status == control.MutationPartial {
			return commandFailure(exitPartial, "configuration durability is uncertain")
		}
		if closeWarning {
			_, _ = fmt.Fprintln(stderr, "bsbctl: warning: operation completed but daemon connection close failed")
		}
		return nil
	case "config":
		if len(positionals) != 1 || options["file"] == "" {
			return commandFailure(exitUsage, "app config requires APP-ID and --file PATH|-")
		}
		replacement, err := readAppConfiguration(options["file"], stdin)
		if err != nil {
			if failure, ok := errors.AsType[*inputFailure](err); ok && failure.operational {
				return commandFailure(exitOperational, "read app configuration failed")
			}
			return commandFailure(exitUsage, "app configuration input is invalid")
		}
		request := control.ReplaceConfigRequest{
			AppID: positionals[0], Config: replacement.Config, Secrets: replacement.Secrets,
			Policies: replacement.Policies, LaunchAction: replacement.LaunchAction,
		}
		var result control.AppConfigResult
		closeWarning, callErr := callDaemonResult(ctx, socketPath, "app.replace_config", request, &result)
		if callErr != nil {
			return callErr
		}
		if err := writeJSON(stdout, result); err != nil {
			return err
		}
		if result.Status == control.MutationDurabilityUncertain || result.Status == control.MutationPartial {
			return commandFailure(exitPartial, "configuration durability is uncertain")
		}
		if closeWarning {
			_, _ = fmt.Fprintln(stderr, "bsbctl: warning: operation completed but daemon connection close failed")
		}
		return nil
	case "query", "command":
		if len(positionals) != 2 {
			return commandFailure(exitUsage, "app operation requires APP-ID and OPERATION")
		}
		payload := json.RawMessage(`{}`)
		if path := options["file"]; path != "" {
			var data []byte
			if path == "-" {
				data, err = io.ReadAll(io.LimitReader(stdin, protocol.MaxJSONObjectBytes+1))
			} else {
				input, readErr := readBoundedRegularInput(path, protocol.MaxJSONObjectBytes)
				err = readErr
				data = input.data
			}
			if err != nil {
				return commandFailure(exitOperational, "read plugin operation payload failed")
			}
			if err := protocol.ValidateJSONObject("plugin operation payload", data, false); err != nil {
				return commandFailure(exitUsage, "plugin operation payload is invalid")
			}
			payload = data
		}
		kind := protocol.OperationQuery
		if command == "command" {
			kind = protocol.OperationCommand
		}
		var result protocol.OperationResult
		if err := callDaemon(ctx, socketPath, "app.operation", control.PluginOperationRequest{
			AppID: positionals[0], Operation: positionals[1], Kind: kind, Payload: payload,
		}, &result); err != nil {
			return err
		}
		if err := result.Validate(); err != nil {
			return commandFailure(exitOperational, "plugin operation returned invalid data")
		}
		output := append(slices.Clone(result.Payload), '\n')
		if _, err := stdout.Write(output); err != nil {
			return commandFailure(exitOperational, "write output failed")
		}
		return nil
	default:
		return commandFailure(exitUsage, "invalid app command")
	}
}

func publicAppStatuses(status control.Status) []appInstanceStatus {
	readiness := make(map[string]daemon.AppReadiness)
	for _, value := range status.Readiness {
		readiness[value.AppID] = value
	}
	plugins := make(map[string]pluginhost.PluginStatus)
	for _, value := range status.Plugins {
		plugins[value.ID] = value
	}
	result := make([]appInstanceStatus, 0, len(status.Apps))
	for _, app := range status.Apps {
		result = append(result, appInstanceStatus{
			AppID: app.AppID, PluginID: app.PluginID, Enabled: app.Enabled, Generation: app.RuntimeGeneration,
			Readiness: readiness[app.AppID].Phase, PluginPhase: plugins[app.PluginID].Phase, Healthy: plugins[app.PluginID].Healthy,
		})
	}
	slices.SortFunc(result, func(left, right appInstanceStatus) int { return cmp.Compare(left.AppID, right.AppID) })
	return result
}

type appConfigurationInput struct {
	Config               json.RawMessage                      `json:"config"`
	Secrets              map[string]string                    `json:"secrets,omitempty"`
	Policies             map[string]presentation.PolicyConfig `json:"policies,omitempty"`
	LaunchAction         string                               `json:"launch_action,omitempty"`
	launchActionProvided bool
}

func readAppConfiguration(path string, stdin io.Reader) (appConfigurationInput, error) {
	var data []byte
	if path == "-" {
		var err error
		data, err = io.ReadAll(io.LimitReader(stdin, maxPluginConfigInput+1))
		if err != nil {
			return appConfigurationInput{}, &inputFailure{operational: true}
		}
	} else {
		input, err := readBoundedRegularInput(path, maxPluginConfigInput)
		if err != nil {
			return appConfigurationInput{}, err
		}
		data = input.data
	}
	if len(data) == 0 || len(data) > maxPluginConfigInput {
		return appConfigurationInput{}, &inputFailure{}
	}
	var input appConfigurationInput
	if err := protocol.DecodeStrict(data, &input); err != nil {
		return appConfigurationInput{}, &inputFailure{}
	}
	var fields map[string]json.RawMessage
	if err := protocol.DecodeStrict(data, &fields); err != nil {
		return appConfigurationInput{}, &inputFailure{}
	}
	_, input.launchActionProvided = fields["launch_action"]
	if err := protocol.ValidateJSONObject("config", input.Config, false); err != nil {
		return appConfigurationInput{}, &inputFailure{}
	}
	for name, reference := range input.Secrets {
		if strings.TrimSpace(name) == "" {
			return appConfigurationInput{}, &inputFailure{}
		}
		if !strictKeychainReference(reference) {
			return appConfigurationInput{}, &inputFailure{}
		}
	}
	return input, nil
}

func strictKeychainReference(reference string) bool {
	if _, err := secrets.ParseReference(reference); err != nil {
		return false
	}
	parsed, err := url.Parse(reference)
	if err != nil {
		return false
	}
	escapedAccount := strings.TrimPrefix(parsed.EscapedPath(), "/")
	return escapedAccount != "" && !strings.Contains(escapedAccount, "/")
}
