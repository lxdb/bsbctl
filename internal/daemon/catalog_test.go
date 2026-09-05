package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/installer"
)

func TestPackageOpsSerializesCatalogOperationsThroughOneInstaller(t *testing.T) {
	backend := &recordingInstaller{entered: make(chan string, 2), release: make(chan struct{})}
	packages, err := NewPackageOps(backend, staticDocumentSource{})
	if err != nil {
		t.Fatal(err)
	}
	request := installer.InstallRequest{PluginID: "plugin", Version: "2", OS: "darwin", Arch: "arm64"}
	done := make(chan error, 2)
	go func() { _, err := packages.CatalogInstall(context.Background(), request, false); done <- err }()
	select {
	case operation := <-backend.entered:
		if operation != "install" {
			t.Fatalf("first operation = %q", operation)
		}
	case <-time.After(time.Second):
		t.Fatal("install did not enter backend")
	}
	go func() { _, err := packages.CatalogInstall(context.Background(), request, true); done <- err }()
	select {
	case operation := <-backend.entered:
		t.Fatalf("concurrent operation entered backend: %q", operation)
	case <-time.After(30 * time.Millisecond):
	}
	close(backend.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := backend.operations(); len(got) != 2 || got[0] != "install" || got[1] != "update" {
		t.Fatalf("operations = %#v", got)
	}
}

func TestCatalogStatusIncludesConfiguredLocalAndCatalogPackages(t *testing.T) {
	backend := &recordingInstaller{snapshot: installer.Snapshot{Plugins: []installer.PluginSnapshot{{
		PluginID: "dev.bsbctl.catalog", Active: &installer.ReleaseRef{ID: "dev.bsbctl.catalog", Version: "1.2.3", OS: "darwin", Arch: "arm64"},
	}}}}
	documents := staticDocumentSource{document: config.Document{
		Plugins: map[string]config.Plugin{
			"dev.bsbctl.catalog": {ID: "dev.bsbctl.catalog", Version: "1.2.3"},
			"dev.bsbctl.local":   {ID: "dev.bsbctl.local", Version: "0.0.0-dev"},
		},
		Apps: map[string]config.App{
			"local-one": {ID: "local-one", PluginID: "dev.bsbctl.local"},
		},
	}}
	packages, err := NewPackageOps(backend, documents)
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := packages.CatalogStatus(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Plugins) != 2 {
		t.Fatalf("plugins = %#v", snapshot.Plugins)
	}
	catalog, local := snapshot.Plugins[0], snapshot.Plugins[1]
	if catalog.PluginID != "dev.bsbctl.catalog" || !catalog.Configured || catalog.ConfiguredSource != "catalog" || catalog.ConfiguredVersion != "1.2.3" {
		t.Fatalf("catalog snapshot = %#v", catalog)
	}
	if local.PluginID != "dev.bsbctl.local" || !local.Configured || local.ConfiguredSource != "local" || local.ConfiguredVersion != "0.0.0-dev" || local.AppCount != 1 || local.Active != nil {
		t.Fatalf("local snapshot = %#v", local)
	}
}

func TestNewPackageOpsRejectsMissingDependencies(t *testing.T) {
	backend := &recordingInstaller{}
	if _, err := NewPackageOps(nil, staticDocumentSource{}); err == nil {
		t.Fatal("NewPackageOps accepted a nil installer")
	}
	if _, err := NewPackageOps(backend, nil); err == nil {
		t.Fatal("NewPackageOps accepted a nil document source")
	}
}

type staticDocumentSource struct{ document config.Document }

func (s staticDocumentSource) Document() (config.Document, bool) { return s.document, true }

type recordingInstaller struct {
	mu       sync.Mutex
	values   []string
	entered  chan string
	release  chan struct{}
	snapshot installer.Snapshot
}

func (i *recordingInstaller) record(operation string) (installer.Result, error) {
	i.mu.Lock()
	i.values = append(i.values, operation)
	i.mu.Unlock()
	i.entered <- operation
	<-i.release
	return installer.Result{Status: installer.StatusInstalled}, nil
}

func (i *recordingInstaller) InstallFirst(context.Context, installer.InstallRequest) (installer.Result, error) {
	return i.record("install")
}
func (i *recordingInstaller) Update(context.Context, installer.InstallRequest) (installer.Result, error) {
	return i.record("update")
}
func (*recordingInstaller) Rollback(context.Context, installer.RollbackRequest) (installer.Result, error) {
	return installer.Result{}, nil
}
func (i *recordingInstaller) Snapshot(context.Context, string) (installer.Snapshot, error) {
	return i.snapshot, nil
}
func (i *recordingInstaller) operations() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.values...)
}
