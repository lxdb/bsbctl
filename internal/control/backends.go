package control

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/installer"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type AppsBackend interface {
	SetEnabled(context.Context, string, bool) (daemon.EnableResult, error)
	CreateAppInstance(context.Context, config.App) (daemon.AppInstanceResult, error)
	DeleteAppInstance(context.Context, string) (daemon.AppInstanceResult, error)
	ReplaceAppConfiguration(context.Context, string, daemon.AppConfiguration) (config.Document, localstate.CommitOutcome, error)
}

type CatalogBackend interface {
	CatalogInstall(context.Context, installer.InstallRequest, bool) (installer.Result, error)
	CatalogRollback(context.Context, installer.RollbackRequest) (installer.Result, error)
	CatalogStatus(context.Context, string) (installer.Snapshot, error)
}

type OperationsBackend interface {
	Launch(context.Context, string, string, json.RawMessage) error
	PluginOperation(context.Context, string, protocol.OperationKind, string, json.RawMessage) (protocol.OperationResult, error)
}

type AttentionBackend interface {
	AttentionSnapshot() (attention.Trace, bool)
	AttentionExplain(string) (attention.Evaluation, bool)
	AttentionHistory(int, time.Time) []attention.Trace
	AcknowledgeAttention(string) error
}

type StatusBackend interface {
	Document() (config.Document, bool)
	Status() []pluginhost.PluginStatus
	RuntimeDiagnostics() daemon.RuntimeDiagnostics
}

type Backends struct {
	Apps       AppsBackend
	Catalog    CatalogBackend
	Operations OperationsBackend
	Attention  AttentionBackend
	Status     StatusBackend
}

func (b Backends) validate() error {
	required := []struct {
		name    string
		backend any
	}{
		{name: "apps", backend: b.Apps},
		{name: "catalog", backend: b.Catalog},
		{name: "operations", backend: b.Operations},
		{name: "attention", backend: b.Attention},
		{name: "status", backend: b.Status},
	}
	for _, requirement := range required {
		if requirement.backend == nil {
			return fmt.Errorf("%s control backend is required", requirement.name)
		}
	}
	return nil
}
