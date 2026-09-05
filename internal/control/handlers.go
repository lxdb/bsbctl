package control

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/installer"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/secrets"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

var runtimePlatform = func() (string, string) { return runtime.GOOS, runtime.GOARCH }

const maxReplaceConfigParams = 256 << 10

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	peer := s.newPeer(conn)
	defer peer.Close()
	s.registerStatusHandlers(peer)
	s.registerAppHandlers(peer)
	s.registerCatalogHandlers(peer)
	s.registerOperationHandlers(peer)
	s.registerAttentionHandlers(peer)
	_ = peer.Serve(ctx)
}

func (s *Server) registerStatusHandlers(peer controlPeer) {
	_ = s.handle(peer, "daemon.status", func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
		if err := protocol.ValidateEmptyParams(raw); err != nil {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		document, loaded := s.backends.Status.Document()
		if !loaded {
			return nil, &rpc.Error{Code: -32040, Message: "daemon configuration is not loaded"}
		}
		apps := make([]AppStatus, 0, len(document.Apps))
		for _, app := range document.Apps {
			apps = append(apps, AppStatus{AppID: app.ID, PluginID: app.PluginID, RuntimeGeneration: app.Generation, Enabled: app.Enabled})
		}
		slices.SortFunc(apps, func(left, right AppStatus) int { return cmp.Compare(left.AppID, right.AppID) })
		diagnostics := s.backends.Status.RuntimeDiagnostics()
		status := Status{
			Version: s.version, Generation: document.Generation, Apps: apps, Plugins: s.backends.Status.Status(),
			Assets: diagnostics.Assets, SessionInput: diagnostics.SessionInput, Input: diagnostics.Input,
			Readiness: diagnostics.Readiness, Device: diagnostics.Device, Output: diagnostics.Output, Audio: diagnostics.Audio,
			AttentionRecorder: diagnostics.AttentionRecorder, AttentionState: diagnostics.AttentionState, PluginLogs: diagnostics.PluginLogs,
			PresentationCooldown: diagnostics.PresentationCooldown,
			Observations:         diagnostics.Observations,
			Configuration:        diagnostics.Configuration, Checkpoints: diagnostics.Checkpoints, Session: diagnostics.Session,
		}
		return status, nil
	})
}

func (s *Server) registerAppHandlers(peer controlPeer) {
	_ = s.handle(peer, "app.create", func(callCtx context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request CreateAppRequest
		if len(raw) > maxReplaceConfigParams || strictDecode(raw, &request) != nil || !validCreateAppRequest(request) {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		operation, err := s.backends.Apps.CreateAppInstance(callCtx, config.App{
			ID: request.AppID, PluginID: request.PluginID, Enabled: request.Enabled,
			Config: append(json.RawMessage(nil), request.Config...), Secrets: request.Secrets,
			Policies: request.Policies, LaunchAction: request.LaunchAction,
		})
		if err != nil && !operation.Outcome.IsCommitted() {
			if errors.Is(err, daemon.ErrInvalidAppConfiguration) {
				return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
			}
			if errors.Is(err, daemon.ErrAppAlreadyExists) || errors.Is(err, config.ErrConflict) {
				return nil, &rpc.Error{Code: -32046, Message: "app creation rejected"}
			}
			return nil, &rpc.Error{Code: -32045, Message: "app creation failed"}
		}
		status := MutationCreated
		if operation.Outcome == localstate.CommittedDurabilityUncertain {
			status = MutationDurabilityUncertain
		} else if operation.ReconciliationError != nil || err != nil {
			status = MutationPartial
		}
		return AppInstanceResult{Status: status, AppID: operation.AppID, PluginID: operation.PluginID, Enabled: operation.Enabled, Generation: operation.Generation}, nil
	})
	_ = s.handle(peer, "app.delete", func(callCtx context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request DeleteAppRequest
		if strictDecode(raw, &request) != nil || request.AppID == "" {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		operation, err := s.backends.Apps.DeleteAppInstance(callCtx, request.AppID)
		if err != nil && !operation.Outcome.IsCommitted() {
			if errors.Is(err, daemon.ErrAppNotFound) || errors.Is(err, config.ErrConflict) {
				return nil, &rpc.Error{Code: -32046, Message: "app deletion rejected"}
			}
			return nil, &rpc.Error{Code: -32045, Message: "app deletion failed"}
		}
		status := MutationDeleted
		if operation.Outcome == localstate.CommittedDurabilityUncertain {
			status = MutationDurabilityUncertain
		} else if operation.ReconciliationError != nil || err != nil {
			status = MutationPartial
		}
		return AppInstanceResult{Status: status, AppID: operation.AppID, PluginID: operation.PluginID, Enabled: false, Generation: operation.Generation}, nil
	})
	_ = s.handle(peer, "app.set_enabled", func(callCtx context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request SetEnabledRequest
		if err := strictDecode(raw, &request); err != nil || request.AppID == "" {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		operation, err := s.backends.Apps.SetEnabled(callCtx, request.AppID, request.Enabled)
		if err != nil {
			if errors.Is(err, daemon.ErrAppNotFound) || errors.Is(err, config.ErrConflict) {
				return nil, &rpc.Error{Code: -32046, Message: "app enablement rejected"}
			}
			return nil, &rpc.Error{Code: -32041, Message: "app enablement failed"}
		}
		status := MutationUnchanged
		if operation.Changed {
			status = MutationUpdated
		}
		if operation.ReconciliationError != nil {
			status = MutationPartial
		} else if operation.Outcome == localstate.CommittedDurabilityUncertain {
			status = MutationDurabilityUncertain
		}
		return AppMutationResult{Status: status, AppID: request.AppID, Enabled: request.Enabled, Generation: operation.Generation}, nil
	})
	_ = s.handle(peer, "app.replace_config", func(callCtx context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request ReplaceConfigRequest
		if len(raw) > maxReplaceConfigParams {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		if err := strictDecode(raw, &request); err != nil || !validReplaceConfigRequest(request) {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		document, outcome, err := s.backends.Apps.ReplaceAppConfiguration(callCtx, request.AppID, daemon.AppConfiguration{
			Config: append(json.RawMessage(nil), request.Config...), Secrets: request.Secrets,
			Policies: request.Policies, LaunchAction: request.LaunchAction,
		})
		if err != nil && !outcome.IsCommitted() {
			if errors.Is(err, daemon.ErrInvalidAppConfiguration) {
				return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
			}
			if errors.Is(err, daemon.ErrAppNotFound) || errors.Is(err, config.ErrConflict) {
				return nil, &rpc.Error{Code: -32046, Message: "app configuration rejected"}
			}
			return nil, &rpc.Error{Code: -32045, Message: "app configuration failed"}
		}
		status := MutationUpdated
		if outcome == localstate.CommittedDurabilityUncertain {
			status = MutationDurabilityUncertain
		} else if err != nil {
			status = MutationPartial
		}
		return AppConfigResult{Status: status, AppID: request.AppID, Generation: document.Generation}, nil
	})
}

func (s *Server) registerCatalogHandlers(peer controlPeer) {
	_ = s.handle(peer, "plugin.install", s.catalogInstallHandler(false))
	_ = s.handle(peer, "plugin.update", s.catalogInstallHandler(true))
	_ = s.handle(peer, "plugin.rollback", func(callCtx context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request CatalogRollbackRequest
		if err := strictDecode(raw, &request); err != nil || request.PluginID == "" || !matchesRuntimePlatform(request.OS, request.Arch) {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		result, err := s.backends.Catalog.CatalogRollback(callCtx, installer.RollbackRequest{PluginID: request.PluginID, Version: request.Version, OS: request.OS, Arch: request.Arch})
		return catalogOperationResponse(result, err), nil
	})
	_ = s.handle(peer, "plugin.status", func(callCtx context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request CatalogStatusRequest
		if len(raw) != 0 && string(raw) != "null" {
			if err := strictDecode(raw, &request); err != nil {
				return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
			}
		}
		result, err := s.backends.Catalog.CatalogStatus(callCtx, request.PluginID)
		if err != nil {
			return nil, &rpc.Error{Code: -32062, Message: "plugin installer status failed"}
		}
		return result, nil
	})
}

func (s *Server) registerOperationHandlers(peer controlPeer) {
	_ = s.handle(peer, "app.launch", func(callCtx context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request LaunchRequest
		if err := strictDecode(raw, &request); err != nil || request.AppID == "" ||
			protocol.ValidateJSONObject("action payload", request.Payload, true) != nil {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		if err := s.backends.Operations.Launch(callCtx, request.AppID, request.Action, request.Payload); err != nil {
			if errors.Is(err, daemon.ErrAppNotEnabled) || errors.Is(err, daemon.ErrAppNotFound) {
				return nil, &rpc.Error{Code: -32046, Message: "app launch rejected"}
			}
			return nil, &rpc.Error{Code: -32042, Message: "app launch failed"}
		}
		return struct{}{}, nil
	})
	_ = s.handle(peer, "app.operation", func(callCtx context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request PluginOperationRequest
		if strictDecode(raw, &request) != nil || request.AppID == "" || request.Operation == "" ||
			(request.Kind != protocol.OperationQuery && request.Kind != protocol.OperationCommand) ||
			protocol.ValidateJSONObject("operation payload", request.Payload, true) != nil {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		result, err := s.backends.Operations.PluginOperation(callCtx, request.AppID, request.Kind, request.Operation, request.Payload)
		if err != nil {
			return nil, &rpc.Error{Code: -32047, Message: "plugin operation failed"}
		}
		if err := result.Validate(); err != nil {
			return nil, &rpc.Error{Code: -32047, Message: "plugin operation failed"}
		}
		return result, nil
	})
}

func (s *Server) registerAttentionHandlers(peer controlPeer) {
	_ = s.handle(peer, "attention.snapshot", func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
		if err := protocol.ValidateEmptyParams(raw); err != nil {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		trace, exists := s.backends.Attention.AttentionSnapshot()
		if !exists {
			return nil, &rpc.Error{Code: -32051, Message: "attention history is empty"}
		}
		return trace, nil
	})
	_ = s.handle(peer, "attention.explain", func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request AttentionExplainRequest
		if err := strictDecode(raw, &request); err != nil || request.ObservationID == "" {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		value, exists := s.backends.Attention.AttentionExplain(request.ObservationID)
		if !exists {
			return nil, &rpc.Error{Code: -32052, Message: "observation is not present in retained history"}
		}
		return value, nil
	})
	_ = s.handle(peer, "attention.history", func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request AttentionHistoryRequest
		if len(raw) != 0 && string(raw) != "null" {
			if err := strictDecode(raw, &request); err != nil {
				return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
			}
		}
		if request.Limit < 0 || request.Limit > 2048 {
			return nil, &rpc.Error{Code: -32602, Message: "limit must be between 0 and 2048"}
		}
		return boundAttentionHistory(s.backends.Attention.AttentionHistory(request.Limit, request.Since))
	})
	_ = s.handle(peer, "attention.acknowledge", func(_ context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request AttentionAcknowledgeRequest
		if err := strictDecode(raw, &request); err != nil || request.ObservationID == "" {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		if err := s.backends.Attention.AcknowledgeAttention(request.ObservationID); err != nil {
			if errors.Is(err, daemon.ErrObservationNotFound) || errors.Is(err, daemon.ErrObservationNotAcknowledgeable) {
				return nil, &rpc.Error{Code: -32054, Message: "attention acknowledgement rejected"}
			}
			return nil, &rpc.Error{Code: -32053, Message: "attention acknowledgement failed"}
		}
		return struct{}{}, nil
	})
}

func validReplaceConfigRequest(request ReplaceConfigRequest) bool {
	if request.AppID == "" || protocol.ValidateJSONObject("config", request.Config, false) != nil {
		return false
	}
	for name, reference := range request.Secrets {
		if strings.TrimSpace(name) == "" {
			return false
		}
		if _, err := secrets.ParseReference(reference); err != nil {
			return false
		}
		parsed, err := url.Parse(reference)
		if err != nil {
			return false
		}
		escapedAccount := strings.TrimPrefix(parsed.EscapedPath(), "/")
		if escapedAccount == "" || strings.Contains(escapedAccount, "/") {
			return false
		}
	}
	return true
}

func validCreateAppRequest(request CreateAppRequest) bool {
	return request.PluginID != "" && validReplaceConfigRequest(ReplaceConfigRequest{
		AppID: request.AppID, Config: request.Config, Secrets: request.Secrets,
		Policies: request.Policies, LaunchAction: request.LaunchAction,
	})
}

func (s *Server) catalogInstallHandler(update bool) rpc.Handler {
	return func(callCtx context.Context, raw json.RawMessage) (any, *rpc.Error) {
		var request CatalogInstallRequest
		if err := strictDecode(raw, &request); err != nil || !validCatalogInstallRequest(request) {
			return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
		}
		catalogData, err := readRegularBoundedDigest(request.CatalogPath, 1<<20, request.CatalogSHA256)
		if err != nil {
			if errors.Is(err, errInvalidCatalogInput) {
				return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
			}
			return nil, &rpc.Error{Code: -32063, Message: "catalog input failed"}
		}
		envelope, err := readRegularBoundedDigest(request.SignaturePath, 16<<10, request.SignatureSHA256)
		if err != nil {
			if errors.Is(err, errInvalidCatalogInput) {
				return nil, &rpc.Error{Code: -32602, Message: "invalid params"}
			}
			return nil, &rpc.Error{Code: -32063, Message: "catalog input failed"}
		}
		result, operationErr := s.backends.Catalog.CatalogInstall(callCtx, installer.InstallRequest{
			Catalog: catalogData, Envelope: envelope, PluginID: request.PluginID, Version: request.Version, OS: request.OS, Arch: request.Arch,
		}, update)
		return catalogOperationResponse(result, operationErr), nil
	}
}

func matchesRuntimePlatform(goos, goarch string) bool {
	runtimeOS, runtimeArch := runtimePlatform()
	return runtimeOS == "darwin" && (runtimeArch == "arm64" || runtimeArch == "amd64") && goos == runtimeOS && goarch == runtimeArch
}

func validCatalogInstallRequest(request CatalogInstallRequest) bool {
	if request.PluginID == "" || request.Version == "" || !matchesRuntimePlatform(request.OS, request.Arch) ||
		!filepath.IsAbs(request.CatalogPath) || !filepath.IsAbs(request.SignaturePath) {
		return false
	}
	for _, digest := range []string{request.CatalogSHA256, request.SignatureSHA256} {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size || digest != strings.ToLower(digest) {
			return false
		}
	}
	return true
}

func catalogOperationResponse(result installer.Result, err error) CatalogOperationResponse {
	code := installer.CodeOf(err)
	if err != nil && code == "" {
		code = CatalogDependencyFailed
	}
	return CatalogOperationResponse{Result: result, ErrorCode: code}
}
