package main

import (
	"context"
	"errors"
	"io"
	"runtime"

	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/installer"
)

func runPluginPackages(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return commandFailure(exitUsage, "plugin command requires list, status, install, update, rollback, or verify")
	}
	if runtime.GOOS != "darwin" || (runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64") {
		return commandFailure(exitUsage, "catalog operations are unsupported on this build")
	}
	command := args[0]
	allowed := []string{"socket"}
	switch command {
	case "install", "update":
		allowed = append(allowed, "catalog", "signature", "version")
	case "rollback":
		allowed = append(allowed, "version")
	case "list", "status":
	default:
		return commandFailure(exitUsage, "invalid plugin command")
	}
	options, positionals, err := parseOptions(args[1:], allowed...)
	if err != nil || (command == "list" && len(positionals) != 0) || (command != "list" && len(positionals) != 1) {
		return commandFailure(exitUsage, "invalid plugin arguments")
	}
	socketPath, err := resolveStatePath(options, "socket", "ctl.sock")
	if err != nil {
		return err
	}
	switch command {
	case "install", "update":
		if options["catalog"] == "" || options["signature"] == "" || options["version"] == "" {
			return commandFailure(exitUsage, "plugin operation requires catalog, signature, plugin ID, and version")
		}
		catalogInput, err := readJSONInput(options["catalog"], 1<<20)
		if err != nil {
			return catalogInputFailure(err)
		}
		signatureInput, err := readJSONInput(options["signature"], 16<<10)
		if err != nil {
			return catalogInputFailure(err)
		}
		request := control.CatalogInstallRequest{
			CatalogPath: catalogInput.path, SignaturePath: signatureInput.path, CatalogSHA256: catalogInput.digest, SignatureSHA256: signatureInput.digest,
			PluginID: positionals[0], Version: options["version"], OS: runtime.GOOS, Arch: runtime.GOARCH,
		}
		var response control.CatalogOperationResponse
		if err := callDaemon(ctx, socketPath, "plugin."+command, request, &response); err != nil {
			return err
		}
		return finishCatalogOperation(stdout, response)
	case "rollback":
		var response control.CatalogOperationResponse
		if err := callDaemon(ctx, socketPath, "plugin.rollback", control.CatalogRollbackRequest{
			PluginID: positionals[0], Version: options["version"], OS: runtime.GOOS, Arch: runtime.GOARCH,
		}, &response); err != nil {
			return err
		}
		return finishCatalogOperation(stdout, response)
	case "list", "status":
		pluginID := ""
		if command == "status" {
			pluginID = positionals[0]
		}
		var result installer.Snapshot
		if err := callDaemon(ctx, socketPath, "plugin.status", control.CatalogStatusRequest{PluginID: pluginID}, &result); err != nil {
			return err
		}
		if err := writeJSON(stdout, result); err != nil {
			return err
		}
		if result.RecoveryRequired {
			return commandFailure(exitPartial, "installer recovery is required")
		}
		return nil
	}
	return commandFailure(exitUsage, "invalid plugin command")
}

func finishCatalogOperation(stdout io.Writer, response control.CatalogOperationResponse) error {
	if response.ErrorCode == "" && !validCatalogMutationSuccess(response.Result) {
		return commandFailure(exitOperational, "daemon returned an invalid catalog result")
	}
	if hasInstallerResult(response.Result) {
		if err := writeJSON(stdout, response.Result); err != nil {
			return err
		}
	}
	switch response.ErrorCode {
	case "":
		return nil
	case installer.CodeInstallConflict, installer.CodeNotInstalled:
		return commandFailure(exitRejected, "catalog operation was rejected")
	case installer.CodeCatalogInvalid:
		return commandFailure(exitUsage, "catalog input is invalid")
	case installer.CodeRecoveryRequired, installer.CodeStateFailed:
		return commandFailure(exitPartial, "installer recovery is required")
	case installer.CodeActivationFailed:
		if response.Result.ActivationOutcome.IsCommitted() {
			return commandFailure(exitPartial, "plugin activation durability is uncertain")
		}
		return commandFailure(exitOperational, "plugin activation failed")
	case installer.CodePackageInvalid:
		return commandFailure(exitRejected, "plugin package was rejected")
	default:
		return commandFailure(exitOperational, "catalog dependency failed")
	}
}

func validCatalogMutationSuccess(result installer.Result) bool {
	switch result.Status {
	case installer.StatusInstalled, installer.StatusUpdated, installer.StatusRolledBack,
		installer.StatusRecoveredTarget, installer.StatusRecoveredPrior:
	default:
		return false
	}
	return result.Release.ID != "" && result.Release.Version != "" && result.Release.OS != "" && result.Release.Arch != ""
}

func hasInstallerResult(result installer.Result) bool {
	return result.Status != "" || result.Release.ID != "" || result.Release.Version != "" || result.Release.OS != "" || result.Release.Arch != "" ||
		result.Promotion != "" || result.IntentOutcome != "" || result.ActivationOutcome != "" || result.StateOutcome != "" || result.CleanupOutcome != ""
}

func catalogInputFailure(err error) error {
	if failure, ok := errors.AsType[*inputFailure](err); ok && failure.operational {
		return commandFailure(exitOperational, "read catalog input failed")
	}
	return commandFailure(exitUsage, "catalog input file is invalid")
}
