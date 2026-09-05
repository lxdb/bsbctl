package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/plugins/calendar"
)

func TestRunInitRegistersSelectedPackagesWithoutCreatingApps(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	pluginRoot := filepath.Join(directory, "calendar")
	if err := os.Mkdir(pluginRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(pluginRoot, "bsbctl-plugin-calendar")
	if err := os.WriteFile(executable, []byte("test executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, "config.schema.json"), []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{"--config", configPath, "--plugin", executable}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	document, err := config.NewStore(configPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	plugin, ok := document.Plugins[calendar.PluginID]
	if !ok || plugin.Executable != executable || plugin.PackageRoot != pluginRoot || plugin.ConfigSchema == nil {
		t.Fatalf("registered plugin = %#v", plugin)
	}
	if len(document.Apps) != 0 {
		t.Fatalf("init created app instances: %#v", document.Apps)
	}
}

func TestRunInitRejectsPackageWithoutConfigurationSchema(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	executable := filepath.Join(directory, "bsbctl-plugin-calendar")
	if err := os.WriteFile(executable, []byte("test executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{"--config", filepath.Join(directory, "config.json"), "--plugin", executable}, io.Discard, io.Discard); err == nil {
		t.Fatal("runInit accepted a package without config.schema.json")
	}
}

func TestLocalFirstPartyPluginMaterializesEveryRegistryPackage(t *testing.T) {
	t.Parallel()
	repositoryRoot := filepath.Join("..", "..")
	for _, descriptor := range firstpartyplugins.All() {
		t.Run(descriptor.ID, func(t *testing.T) {
			t.Parallel()
			packageRoot := t.TempDir()
			executable := filepath.Join(packageRoot, descriptor.Binary)
			if err := os.WriteFile(executable, []byte("test executable"), 0o755); err != nil {
				t.Fatal(err)
			}
			schema, err := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(descriptor.SchemaPath)))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(packageRoot, "config.schema.json"), schema, 0o644); err != nil {
				t.Fatal(err)
			}
			for _, declaration := range descriptor.Assets {
				data, err := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(descriptor.AssetRoot), filepath.FromSlash(declaration.Source)))
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(packageRoot, filepath.FromSlash(declaration.Source))
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			plugin, err := localFirstPartyPlugin(executable)
			if err != nil {
				t.Fatal(err)
			}
			if plugin.ID != descriptor.ID || plugin.Version != descriptor.DevelopmentVersion || plugin.ConfigSchema == nil {
				t.Fatalf("materialized plugin = %#v", plugin)
			}
		})
	}
}

func TestRunInitAllowsNoPluginPackages(t *testing.T) {
	t.Parallel()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := runInit([]string{"--config", configPath}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	document, err := config.NewStore(configPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Plugins) != 0 || len(document.Apps) != 0 {
		t.Fatalf("empty init = plugins:%#v apps:%#v", document.Plugins, document.Apps)
	}
}
