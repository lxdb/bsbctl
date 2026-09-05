package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/installer"
)

type InstallerController interface {
	InstallFirst(context.Context, installer.InstallRequest) (installer.Result, error)
	Update(context.Context, installer.InstallRequest) (installer.Result, error)
	Rollback(context.Context, installer.RollbackRequest) (installer.Result, error)
	Snapshot(context.Context, string) (installer.Snapshot, error)
}

type DocumentSource interface {
	Document() (config.Document, bool)
}

// PackageOps serializes catalog mutations and enriches installer state with
// the current desired configuration. It owns no plugin runtime state.
type PackageOps struct {
	mu        sync.Mutex
	installer InstallerController
	documents DocumentSource
}

func NewPackageOps(installerController InstallerController, documents DocumentSource) (*PackageOps, error) {
	if installerController == nil {
		return nil, errors.New("plugin installer is required")
	}
	if documents == nil {
		return nil, errors.New("desired state document source is required")
	}
	return &PackageOps{installer: installerController, documents: documents}, nil
}

func (p *PackageOps) CatalogInstall(ctx context.Context, request installer.InstallRequest, update bool) (installer.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if update {
		return p.installer.Update(ctx, request)
	}
	return p.installer.InstallFirst(ctx, request)
}

func (p *PackageOps) CatalogRollback(ctx context.Context, request installer.RollbackRequest) (installer.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.installer.Rollback(ctx, request)
}

func (p *PackageOps) CatalogStatus(ctx context.Context, pluginID string) (installer.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snapshot, err := p.installer.Snapshot(ctx, pluginID)
	if err != nil {
		return installer.Snapshot{}, err
	}
	document, _ := p.documents.Document()
	byID := make(map[string]int, len(snapshot.Plugins))
	for index := range snapshot.Plugins {
		byID[snapshot.Plugins[index].PluginID] = index
	}
	for id, configured := range document.Plugins {
		if pluginID != "" && id != pluginID {
			continue
		}
		index, exists := byID[id]
		if !exists {
			snapshot.Plugins = append(snapshot.Plugins, installer.PluginSnapshot{PluginID: id})
			index = len(snapshot.Plugins) - 1
			byID[id] = index
		}
		entry := &snapshot.Plugins[index]
		entry.Configured = true
		entry.ConfiguredVersion = configured.Version
		entry.ConfiguredSource = "local"
		if entry.Active != nil && entry.Active.ID == id && entry.Active.Version == configured.Version {
			entry.ConfiguredSource = "catalog"
		}
	}
	for _, app := range document.Apps {
		if index, exists := byID[app.PluginID]; exists {
			snapshot.Plugins[index].AppCount++
		}
	}
	slices.SortFunc(snapshot.Plugins, func(left, right installer.PluginSnapshot) int {
		return strings.Compare(left.PluginID, right.PluginID)
	})
	return snapshot, nil
}
