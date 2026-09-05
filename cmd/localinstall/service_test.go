package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/launchagent"
	"github.com/lxdb/bsbctl/internal/pluginhost"
)

func TestLocalServiceRejectsNondefaultTargetsBeforeChangingService(t *testing.T) {
	for _, wrong := range []int{-1, 0, 3, 5} {
		t.Run(string(rune('a'+wrong+1)), func(t *testing.T) {
			home := t.TempDir()
			args := []string{filepath.Join(home, ".local/bin/bsbctl"), "daemon", "--config", filepath.Join(home, ".bsbctl/config.json"), "--socket", filepath.Join(home, ".bsbctl/ctl.sock")}
			if wrong >= 0 {
				args[wrong] = "/another/installation"
			}
			service := localService{home: home,
				status: func(context.Context, string) (launchagent.Result, error) {
					return launchagent.Result{Status: launchagent.StateLoaded, PlistMatches: true}, nil
				},
				command: func(_ context.Context, name string, commandArgs ...string) ([]byte, error) {
					if name != "/usr/bin/plutil" {
						t.Fatalf("unexpected service command %s %v", name, commandArgs)
					}
					return json.Marshal(args)
				},
				listening: func(context.Context) (bool, error) { return true, nil },
			}
			loaded, err := service.inspect(t.Context())
			if (err != nil) != (wrong >= 0) || (err == nil && !loaded) {
				t.Fatalf("loaded=%v, err=%v", loaded, err)
			}
		})
	}
}

func TestLocalServiceDoesNotMistakeControlSocketFailureForStoppedDaemon(t *testing.T) {
	service := localService{home: t.TempDir(),
		status: func(context.Context, string) (launchagent.Result, error) {
			return launchagent.Result{Status: launchagent.StateNotInstalled}, nil
		},
		listening: func(context.Context) (bool, error) { return false, errors.New("permission denied") },
	}
	if _, err := service.inspect(t.Context()); err == nil {
		t.Fatal("socket access failure was treated as a stopped daemon")
	}
	service.listening = func(context.Context) (bool, error) { return true, nil }
	if _, err := service.inspect(t.Context()); err == nil {
		t.Fatal("foreground daemon was accepted for offline configuration mutation")
	}
}

func TestLocalShutdownReconcilesFailedBootoutBeforeResuming(t *testing.T) {
	for _, unloaded := range []bool{false, true} {
		t.Run(map[bool]string{false: "not_committed", true: "committed"}[unloaded], func(t *testing.T) {
			home := t.TempDir()
			loaded, resumed := true, false
			service := localService{home: home,
				status: func(context.Context, string) (launchagent.Result, error) {
					state := launchagent.StateLoaded
					if !loaded {
						state = launchagent.StateInstalledNotLoaded
					}
					return launchagent.Result{Status: state, PlistMatches: true}, nil
				},
				listening: func(context.Context) (bool, error) { return loaded, nil },
				command: func(_ context.Context, name string, args ...string) ([]byte, error) {
					if name == "/usr/bin/plutil" {
						return json.Marshal([]string{filepath.Join(home, ".local/bin/bsbctl"), "daemon", "--config", filepath.Join(home, ".bsbctl/config.json"), "--socket", filepath.Join(home, ".bsbctl/ctl.sock")})
					}
					switch args[0] {
					case "print":
						return []byte("state = waiting\n"), nil
					case "bootout":
						loaded = !unloaded
						return nil, errors.New("launchctl reply lost")
					case "bootstrap":
						resumed = true
						loaded = true
						return nil, nil
					default:
						t.Fatalf("unexpected command: %s %v", name, args)
						return nil, nil
					}
				},
			}
			if err := service.stop(t.Context()); err == nil {
				t.Fatal("failed bootout was reported as confirmed shutdown")
			}
			if !loaded || resumed != unloaded {
				t.Fatalf("shutdown recovery: loaded=%v, resumed=%v", loaded, resumed)
			}
		})
	}
}

func TestLocalReadinessRequiresNewGenerationDeviceAndEveryEnabledApp(t *testing.T) {
	document := config.Document{Generation: 8, Apps: map[string]config.App{
		"enabled": {ID: "enabled", PluginID: "example", Enabled: true}, "disabled": {ID: "disabled", Enabled: false},
	}, Plugins: map[string]config.Plugin{"example": {ID: "example", Version: "0.1.0", Assets: []assets.Declaration{{Source: "icon.png"}}}}}
	ready := control.Status{Version: "0.1.0-dev", Generation: 8, Device: device.RuntimeStatus{Phase: device.PhaseReady},
		Readiness: []daemon.AppReadiness{{AppID: "enabled", PluginID: "example", Phase: daemon.AppReady}},
		Plugins:   []pluginhost.PluginStatus{{ID: "example", Running: true, Healthy: true}},
		Assets:    []assets.State{{PluginID: "example", ObservedVersion: "0.1.0", Phase: assets.PhaseReady}},
	}
	for _, test := range []struct {
		name   string
		change func(*control.Status)
	}{
		{"stale_generation", func(s *control.Status) { s.Generation = 7 }},
		{"wrong_binary_version", func(s *control.Status) { s.Version = "old" }},
		{"device_pending", func(s *control.Status) { s.Device.Phase = device.PhaseConnecting }},
		{"app_missing", func(s *control.Status) { s.Readiness = nil }},
		{"plugin_missing", func(s *control.Status) { s.Plugins = nil }},
		{"plugin_unhealthy", func(s *control.Status) { s.Plugins = []pluginhost.PluginStatus{{ID: "example", Running: true}} }},
		{"wrong_plugin", func(s *control.Status) {
			s.Plugins = []pluginhost.PluginStatus{{ID: "unrelated", Running: true, Healthy: true}}
		}},
		{"assets_missing", func(s *control.Status) { s.Assets = nil }},
		{"assets_pending", func(s *control.Status) {
			s.Assets = []assets.State{{PluginID: "example", ObservedVersion: "0.1.0", Phase: assets.PhasePending}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := ready
			test.change(&status)
			if localStatusReady(status, document, "0.1.0-dev") {
				t.Fatal("incomplete readiness accepted")
			}
		})
	}
	if !localStatusReady(ready, document, "0.1.0-dev") {
		t.Fatal("healthy enabled app with a distinct plugin ID was rejected, or a disabled app blocked readiness")
	}
}
