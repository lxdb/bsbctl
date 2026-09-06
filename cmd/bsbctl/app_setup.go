package main

import (
	"context"
	"errors"
	"io"

	"github.com/lxdb/bsbctl/internal/appsetup"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/secrets"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

type productionAppSetupHost struct {
	keychain *secrets.Keychain
}

func newProductionAppSetupHost() *productionAppSetupHost {
	return &productionAppSetupHost{keychain: secrets.NewKeychain(nil)}
}

func (host *productionAppSetupHost) ReadConfiguration(path string, stdin io.Reader) (appsetup.Configuration, error) {
	input, err := readAppConfiguration(path, stdin)
	if err != nil {
		if failure, ok := errors.AsType[*inputFailure](err); ok && failure.operational {
			return appsetup.Configuration{}, appsetup.Failure(appsetup.Operational, "read app setup configuration failed")
		}
		return appsetup.Configuration{}, appsetup.Failure(appsetup.Usage, "app setup configuration is invalid")
	}
	return appsetup.Configuration{
		Config: input.Config, Secrets: input.Secrets, Policies: input.Policies,
		LaunchAction: input.LaunchAction, LaunchActionProvided: input.launchActionProvided,
	}, nil
}

func (*productionAppSetupHost) LoadDocument() (config.Document, error) {
	path, err := resolveStatePath(nil, "config", "config.json")
	if err != nil {
		return config.Document{}, err
	}
	return config.NewStore(path).Load()
}

func (*productionAppSetupHost) DaemonStatus(ctx context.Context) (appsetup.Status, error) {
	path, err := resolveStatePath(nil, "socket", "ctl.sock")
	if err != nil {
		return appsetup.Status{}, err
	}
	var status control.Status
	if err := callDaemon(ctx, path, "daemon.status", nil, &status); err != nil {
		return appsetup.Status{}, err
	}
	result := appsetup.Status{Generation: status.Generation, Apps: make([]appsetup.AppStatus, len(status.Apps))}
	for index, app := range status.Apps {
		result.Apps[index] = appsetup.AppStatus{
			AppID: app.AppID, PluginID: app.PluginID, RuntimeGeneration: app.RuntimeGeneration,
		}
	}
	return result, nil
}

func (host *productionAppSetupHost) ResolveSecret(ctx context.Context, reference string) (string, error) {
	value, err := host.keychain.Resolve(ctx, reference)
	if errors.Is(err, secrets.ErrItemNotFound) {
		return "", appsetup.ErrSecretNotFound
	}
	return value, err
}

func (host *productionAppSetupHost) StoreSecret(ctx context.Context, reference, value string) error {
	return host.keychain.Store(ctx, reference, value)
}

func (*productionAppSetupHost) ReplaceConfiguration(ctx context.Context, request appsetup.ReplaceConfigRequest) appsetup.Mutation {
	path, err := resolveStatePath(nil, "socket", "ctl.sock")
	if err != nil {
		return appsetup.Mutation{Outcome: appsetup.MutationKnown, Err: err}
	}
	client, err := dialControl(ctx, path)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		} else {
			err = commandFailure(exitOperational, "daemon is unavailable")
		}
		return appsetup.Mutation{Outcome: appsetup.MutationKnown, Err: err}
	}
	var result control.AppConfigResult
	callErr := client.Call(ctx, "app.replace_config", control.ReplaceConfigRequest{
		AppID: request.AppID, ExpectedGeneration: request.ExpectedGeneration, Config: request.Config,
		Secrets: request.Secrets, Policies: request.Policies, LaunchAction: request.LaunchAction,
	}, &result)
	closeErr := client.Close()
	if callErr == nil {
		return appsetup.Mutation{Result: appsetup.ConfigResult{
			Status: string(result.Status), AppID: result.AppID, Generation: result.Generation,
		}, Outcome: appsetup.MutationKnown, CloseWarning: closeErr != nil}
	}
	if rpcErr, ok := errors.AsType[*rpc.Error](callErr); ok {
		return appsetup.Mutation{Outcome: appsetup.MutationKnown, Err: mapAppSetupRPCError(rpcErr)}
	}
	return appsetup.Mutation{
		Outcome: appsetup.MutationUnknown,
		Err:     appsetup.Failure(appsetup.Operational, "daemon configuration response was not received"),
	}
}

func mapAppSetupRPCError(err *rpc.Error) error {
	switch err.Code {
	case -32602:
		return appsetup.Failure(appsetup.Usage, "daemon rejected invalid input")
	case -32046:
		return appsetup.Failure(appsetup.Rejected, "daemon rejected the operation")
	default:
		return appsetup.Failure(appsetup.Operational, "daemon operation failed")
	}
}

func runAppSetup(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return commandFailure(exitUsage, "app setup requires APP-ID")
	}
	host := newProductionAppSetupHost()
	document, err := host.LoadDocument()
	if err != nil {
		return commandFailure(exitOperational, "load daemon configuration failed")
	}
	descriptor, ok := resolveAppSetupDescriptor(document, args[0])
	if !ok || descriptor.Setup == nil {
		return commandFailure(exitRejected, "app does not support dedicated setup")
	}
	err = descriptor.Setup(ctx, args, stdin, stdout, stderr, host)
	if err == nil {
		return nil
	}
	if failure, ok := errors.AsType[*appsetup.Error](err); ok {
		codes := map[appsetup.Kind]int{
			appsetup.Usage: exitUsage, appsetup.Rejected: exitRejected,
			appsetup.Operational: exitOperational, appsetup.Partial: exitPartial,
		}
		return commandFailure(codes[failure.Kind], failure.Message)
	}
	return err
}

func resolveAppSetupDescriptor(document config.Document, appID string) (firstpartyplugins.Descriptor, bool) {
	app, ok := document.Apps[appID]
	if !ok {
		return firstpartyplugins.Descriptor{}, false
	}
	return firstpartyplugins.LookupID(app.PluginID)
}
