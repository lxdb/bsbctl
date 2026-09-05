package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/installer"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestLocalSelectionRefreshesLocalPackagesWithoutConvertingCatalogPackages(t *testing.T) {
	document := config.Document{Plugins: map[string]config.Plugin{
		"dev.bsbctl.codex":    {ID: "dev.bsbctl.codex"},
		"dev.bsbctl.calendar": {ID: "dev.bsbctl.calendar"},
	}}
	state := installer.InstallState{Plugins: map[string]installer.PluginInstallState{
		"dev.bsbctl.calendar": {Active: &installer.ReleaseRef{ID: "dev.bsbctl.calendar", Version: "0.1.0"}},
	}}
	for _, test := range []struct {
		apps      string
		want      []string
		wantError bool
	}{
		{want: []string{"codex"}},
		{apps: "mac-resources", want: []string{"codex", "mac-resources"}},
		{apps: "none", want: []string{}},
		{apps: "calendar", wantError: true},
		{apps: "unknown", wantError: true},
		{apps: "codex,,mac-resources", wantError: true},
		{apps: "none,codex", wantError: true},
	} {
		t.Run(test.apps, func(t *testing.T) {
			selected, err := selectLocalPlugins(document, state, test.apps)
			if (err != nil) != test.wantError {
				t.Fatalf("selection error = %v, want error %v", err, test.wantError)
			}
			if test.wantError {
				return
			}
			got := make([]string, 0, len(selected))
			for _, descriptor := range selected {
				got = append(got, descriptor.DefaultApp.ID)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("selected apps = %v, want %v", got, test.want)
			}
		})
	}
}

func TestLocalReconcilePreservesUserSettingsAndOnlyAddsExplicitApps(t *testing.T) {
	descriptor, _ := firstpartyplugins.LookupAppID("mac-resources")
	definition := descriptor.DefinitionForVersion(descriptor.DevelopmentVersion)
	plugin := config.Plugin{ID: descriptor.ID, Version: definition.Version, Executable: "/old/bsbctl-plugin-mac-resources", ProtocolVersion: protocol.Version,
		ExecutionModes: definition.Contract.ExecutionModes, Channels: definition.Contract.Channels, Operations: definition.Contract.Operations}
	app := descriptor.DefaultApp
	app.ID = "my-resources"
	app.Enabled = false
	app.Generation = 4
	app.Config = json.RawMessage(`{"interval_seconds":15}`)
	app.Secrets = map[string]string{"token": "keychain://bsbctl/test"}
	app.LaunchAction = "custom-open"
	document := config.Document{Version: config.CurrentVersion, Generation: 7,
		Device:  config.Device{BaseURL: "http://192.0.2.10", AccessTokenSecret: "keychain://bsbctl/device/access-token"},
		Plugins: map[string]config.Plugin{plugin.ID: plugin}, Apps: map[string]config.App{app.ID: app}}
	builtPlugin := plugin
	builtPlugin.Executable = "/new/bsbctl-plugin-mac-resources"
	builtPlugin.SHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	built := config.Document{Plugins: map[string]config.Plugin{plugin.ID: builtPlugin}}
	for _, explicit := range []string{"", "mac-resources"} {
		next, err := reconcileLocalDocument(document, built, explicit)
		if err != nil {
			t.Fatal(err)
		}
		if next.Generation != 8 || next.Device != document.Device || !reflect.DeepEqual(next.Apps[app.ID], app) {
			t.Fatalf("existing settings changed: %#v", next)
		}
		if next.Plugins[plugin.ID].Executable != "/new/bsbctl-plugin-mac-resources" || next.Plugins[plugin.ID].SHA256 != builtPlugin.SHA256 {
			t.Fatal("new executable and digest were not registered together")
		}
		added, exists := next.Apps["mac-resources"]
		if exists != (explicit != "") || (exists && (!added.Enabled || added.Generation != 8)) {
			t.Fatalf("explicit app creation = %#v, exists %v", added, exists)
		}
	}
	if document.Plugins[plugin.ID].Executable != "/old/bsbctl-plugin-mac-resources" || len(document.Apps) != 1 {
		t.Fatal("pre-install document was mutated")
	}
}
