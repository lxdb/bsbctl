// Package installer authenticates, verifies, installs, activates, and recovers
// independently released bsbctl plugins.
package installer

import "errors"

type Code string

const (
	CodeCatalogInvalid   Code = "catalog_invalid"
	CodeDownloadFailed   Code = "download_failed"
	CodePackageInvalid   Code = "package_invalid"
	CodeInstallConflict  Code = "install_conflict"
	CodeStateFailed      Code = "state_failed"
	CodeActivationFailed Code = "activation_failed"
	CodeRecoveryRequired Code = "recovery_required"
	CodeNotInstalled     Code = "not_installed"
)

type Error struct{ Code Code }

func (err *Error) Error() string { return "plugin installer: " + string(err.Code) }

func errorCode(code Code) error { return &Error{Code: code} }

func CodeOf(err error) Code {
	if installerError, ok := errors.AsType[*Error](err); ok {
		return installerError.Code
	}
	return ""
}
