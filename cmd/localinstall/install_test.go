package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestLocalInstallBuildFailureLeavesRunningInstallationUntouched(t *testing.T) {
	home, document := localInstallFixture(t)
	deps := installDependencies{
		build: func(context.Context, string, []firstpartyplugins.Descriptor) (config.Document, string, error) {
			return config.Document{}, "", errors.New("compiler failed")
		},
		inspect: func(context.Context) (bool, error) { return true, nil },
		stop:    func(context.Context) error { t.Fatal("stopped service after failed build"); return nil },
	}
	if _, err := installLocal(t.Context(), home, "", io.Discard, deps); err == nil {
		t.Fatal("failed build was accepted")
	}
	assertLocalFile(t, filepath.Join(home, ".local/bin/bsbctl"), "old core")
	current, err := config.NewStore(filepath.Join(home, ".bsbctl/config.json")).Load()
	if err != nil || !reflect.DeepEqual(current, document) {
		t.Fatalf("configuration changed on build failure: %#v, %v", current, err)
	}
}

func TestLocalInstallPreservesLateConfigurationAndStoppedService(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "stopped", true: "running"}[running], func(t *testing.T) {
			home, _ := localInstallFixture(t)
			store := config.NewStore(filepath.Join(home, ".bsbctl/config.json"))
			var stopped, started, ready bool
			deps := installDependencies{
				build: func(_ context.Context, stage string, selected []firstpartyplugins.Descriptor) (config.Document, string, error) {
					if stopped {
						t.Fatal("build occurred after stopping the service")
					}
					if len(selected) != 0 {
						t.Fatalf("unexpected plugins: %v", selected)
					}
					writeLocalTestFile(t, filepath.Join(stage, "bsbctl"), "new core", 0o755)
					_, _, err := store.Update(1, func(document *config.Document) error {
						document.Device.BaseURL = "http://192.0.2.20"
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					return config.Document{Plugins: map[string]config.Plugin{}}, "0.1.0-dev", nil
				},
				inspect: func(context.Context) (bool, error) { return running, nil },
				stop: func(context.Context) error {
					assertLocalFile(t, filepath.Join(home, ".local/bin/bsbctl"), "old core")
					stopped = true
					return nil
				},
				start: func(context.Context) error {
					if !stopped {
						t.Fatal("started a previously stopped service")
					}
					assertLocalFile(t, filepath.Join(home, ".local/bin/bsbctl"), "new core")
					current, err := store.Load()
					if err != nil || current.Generation != 3 || current.Device.BaseURL != "http://192.0.2.20" {
						t.Fatalf("started before new configuration committed: %#v, %v", current, err)
					}
					started = true
					return nil
				},
				waitReady: func(_ context.Context, document config.Document, version string) error {
					if !started || document.Generation != 3 || version != "0.1.0-dev" {
						t.Fatal("wrong readiness target")
					}
					ready = true
					return nil
				},
			}
			result, err := installLocal(t.Context(), home, "", io.Discard, deps)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Installed || result.Running != running || stopped != running || started != running || ready != running {
				t.Fatalf("wrong lifecycle: result=%#v, stopped=%v, started=%v, ready=%v", result, stopped, started, ready)
			}
			assertLocalFile(t, filepath.Join(result.Directory, "previous/bsbctl"), "old core")
			assertLocalFile(t, filepath.Join(home, ".bsbctl/checkpoints-preserved"), "checkpoint")
			previous, err := config.NewStore(filepath.Join(result.Directory, "previous/config.json")).Load()
			if err != nil || previous.Generation != 2 {
				t.Fatalf("late configuration missing from backup: %#v, %v", previous, err)
			}
		})
	}
}

func TestLocalInstallReadinessFailureReportsInstalledStateAndKeepsRecoveryFiles(t *testing.T) {
	home, _ := localInstallFixture(t)
	deps := installDependencies{
		build: func(_ context.Context, stage string, _ []firstpartyplugins.Descriptor) (config.Document, string, error) {
			writeLocalTestFile(t, filepath.Join(stage, "bsbctl"), "new core", 0o755)
			return config.Document{Plugins: map[string]config.Plugin{}}, "0.1.0-dev", nil
		},
		inspect:   func(context.Context) (bool, error) { return true, nil },
		stop:      func(context.Context) error { return nil },
		start:     func(context.Context) error { return nil },
		waitReady: func(context.Context, config.Document, string) error { return errors.New("device unavailable") },
	}
	result, err := installLocal(t.Context(), home, "", io.Discard, deps)
	if err == nil || !result.Installed {
		t.Fatalf("readiness failure lost partial outcome: %#v, %v", result, err)
	}
	assertLocalFile(t, filepath.Join(home, ".local/bin/bsbctl"), "new core")
	assertLocalFile(t, filepath.Join(result.Directory, "previous/bsbctl"), "old core")
}

func TestLocalInstallRestoresCoreWhenConfigurationCommitFails(t *testing.T) {
	home, current := localInstallFixture(t)
	store := config.NewStore(filepath.Join(home, ".bsbctl/config.json"))
	descriptor, _ := firstpartyplugins.LookupAppID("mac-resources")
	definition := descriptor.DefinitionForVersion(descriptor.DevelopmentVersion)
	plugin := config.Plugin{ID: descriptor.ID, Version: definition.Version, Executable: "/old/bsbctl-plugin-mac-resources", ProtocolVersion: protocol.Version,
		ExecutionModes: definition.Contract.ExecutionModes, Channels: definition.Contract.Channels, Operations: definition.Contract.Operations}
	current.Generation = 2
	current.Plugins[plugin.ID] = plugin
	// The old document fits the store's 1 MiB limit. Adding the explicitly
	// requested default app crosses it, exercising an actual persistence error
	// after the core replacement rather than a mocked configuration writer.
	for i := range 18 {
		padding := 60000
		if i == 17 {
			padding = 0
		}
		id := fmt.Sprintf("instance-%d", i)
		current.Apps[id] = config.App{ID: id, PluginID: plugin.ID, Generation: 2, Config: json.RawMessage(`{"padding":"` + strings.Repeat("x", padding) + `"}`)}
	}
	data, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	last := current.Apps["instance-17"]
	last.Config = json.RawMessage(`{"padding":"` + strings.Repeat("x", (1<<20)-128-len(data)) + `"}`)
	current.Apps[last.ID] = last
	if _, err := store.ReplaceWithOutcome(1, current); err != nil {
		t.Fatal(err)
	}
	resumed := false
	deps := installDependencies{
		build: func(_ context.Context, stage string, _ []firstpartyplugins.Descriptor) (config.Document, string, error) {
			writeLocalTestFile(t, filepath.Join(stage, "bsbctl"), "new core", 0o755)
			return config.Document{Plugins: map[string]config.Plugin{plugin.ID: plugin}}, "0.1.0-dev", nil
		},
		inspect: func(context.Context) (bool, error) { return true, nil },
		stop:    func(context.Context) error { return nil },
		start: func(context.Context) error {
			assertLocalFile(t, filepath.Join(home, ".local/bin/bsbctl"), "old core")
			restored, err := store.Load()
			if err != nil || !reflect.DeepEqual(restored, current) {
				t.Fatalf("old configuration was not restored: %v", err)
			}
			resumed = true
			return nil
		},
	}
	result, err := installLocal(t.Context(), home, "mac-resources", io.Discard, deps)
	if err == nil || result.Installed || !resumed {
		t.Fatalf("failed commit did not restore old installation: %#v, %v, resumed=%v", result, err, resumed)
	}
}

func TestLocalInstallCancellationAndConcurrentInstallDoNotStopService(t *testing.T) {
	home, _ := localInstallFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	deps := installDependencies{
		build: func(_ context.Context, stage string, _ []firstpartyplugins.Descriptor) (config.Document, string, error) {
			if _, err := installLocal(t.Context(), home, "", io.Discard, installDependencies{}); err == nil {
				t.Fatal("concurrent installation acquired the same state")
			}
			writeLocalTestFile(t, filepath.Join(stage, "bsbctl"), "new core", 0o755)
			cancel()
			return config.Document{Plugins: map[string]config.Plugin{}}, "0.1.0-dev", nil
		},
		inspect: func(context.Context) (bool, error) { return true, nil },
		stop:    func(context.Context) error { t.Fatal("canceled installation stopped service"); return nil },
	}
	if _, err := installLocal(ctx, home, "", io.Discard, deps); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was lost: %v", err)
	}
	assertLocalFile(t, filepath.Join(home, ".local/bin/bsbctl"), "old core")
}

func TestLocalInstallHonorsConfigurationCommitOutcome(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "generation_conflict", true: "durability_uncertain"}[committed], func(t *testing.T) {
			home, _ := localInstallFixture(t)
			store := config.NewStore(filepath.Join(home, ".bsbctl/config.json"))
			resumed := false
			deps := installDependencies{
				store: &faultingLocalStore{Store: store, replace: func(expected uint64, next config.Document) (localstate.CommitOutcome, error) {
					assertLocalFile(t, filepath.Join(home, ".local/bin/bsbctl"), "new core")
					if committed {
						if _, err := store.ReplaceWithOutcome(expected, next); err != nil {
							t.Fatal(err)
						}
						return localstate.CommittedDurabilityUncertain, errors.New("directory sync failed after rename")
					}
					if _, _, err := store.Update(expected, func(document *config.Document) error { document.Device.BaseURL = "http://192.0.2.99"; return nil }); err != nil {
						t.Fatal(err)
					}
					return store.ReplaceWithOutcome(expected, next)
				}},
				build: func(_ context.Context, stage string, _ []firstpartyplugins.Descriptor) (config.Document, string, error) {
					writeLocalTestFile(t, filepath.Join(stage, "bsbctl"), "new core", 0o755)
					return config.Document{Plugins: map[string]config.Plugin{}}, "0.1.0-dev", nil
				},
				inspect: func(context.Context) (bool, error) { return true, nil },
				stop:    func(context.Context) error { return nil },
				start:   func(context.Context) error { resumed = true; return nil },
			}
			result, err := installLocal(t.Context(), home, "", io.Discard, deps)
			if err == nil || result.Installed != committed || resumed == committed {
				t.Fatalf("commit outcome was lost: result=%#v err=%v resumed=%v", result, err, resumed)
			}
			current, err := store.Load()
			if err != nil || current.Generation != 2 {
				t.Fatalf("configuration rolled back across the commit boundary: generation=%d err=%v", current.Generation, err)
			}
			wantCore := "new core"
			if !committed {
				wantCore = "old core"
				if current.Device.BaseURL != "http://192.0.2.99" {
					t.Fatal("generation conflict overwrote another writer's configuration")
				}
			}
			assertLocalFile(t, filepath.Join(home, ".local/bin/bsbctl"), wantCore)
			assertLocalFile(t, filepath.Join(result.Directory, "previous/bsbctl"), "old core")
		})
	}
}

type faultingLocalStore struct {
	*config.Store
	replace func(uint64, config.Document) (localstate.CommitOutcome, error)
}

func (s *faultingLocalStore) ReplaceWithOutcome(expected uint64, next config.Document) (localstate.CommitOutcome, error) {
	return s.replace(expected, next)
}

func localInstallFixture(t *testing.T) (string, config.Document) {
	t.Helper()
	home := t.TempDir()
	writeLocalTestFile(t, filepath.Join(home, ".local/bin/bsbctl"), "old core", 0o755)
	writeLocalTestFile(t, filepath.Join(home, ".bsbctl/checkpoints-preserved"), "checkpoint", 0o600)
	document := config.Document{Version: config.CurrentVersion, Generation: 1,
		Device:  config.Device{BaseURL: "http://192.0.2.10", AccessTokenSecret: "keychain://bsbctl/device/access-token"},
		Plugins: map[string]config.Plugin{}, Apps: map[string]config.App{}}
	if _, err := config.NewStore(filepath.Join(home, ".bsbctl/config.json")).ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	return home, document
}

func writeLocalTestFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func assertLocalFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
	}
}
