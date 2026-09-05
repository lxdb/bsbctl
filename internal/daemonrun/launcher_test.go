package daemonrun

import (
	"io"
	"reflect"
	"testing"

	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	busyinput "github.com/lxdb/bsbctl/internal/input"
	"github.com/lxdb/bsbctl/internal/pluginlog"
)

func TestLauncherAppCarriesCanonicalFirstPartyDisplayName(t *testing.T) {
	t.Parallel()
	descriptor, ok := firstpartyplugins.LookupAppID("calendar")
	if !ok {
		t.Fatal("Calendar descriptor is unavailable")
	}

	got, fallback := launcherApp(daemon.LaunchableApp{ID: "work-calendar", PluginID: descriptor.ID, Action: "options"})
	want := busyinput.App{ID: "work-calendar", DisplayName: descriptor.DisplayName, Action: "options"}
	if got != want || fallback {
		t.Fatalf("launcher app = %#v, fallback=%v, want %#v without fallback", got, fallback, want)
	}
}

func TestLauncherAppsUseRegistryOrderAndDisambiguateInstances(t *testing.T) {
	t.Parallel()
	descriptors := firstpartyplugins.All()
	got, fallbackIDs := launcherApps([]daemon.LaunchableApp{
		{ID: "unknown-app", PluginID: "dev.example.unknown", Action: "open"},
		{ID: "codex-work", PluginID: descriptors[1].ID, Action: "open"},
		{ID: "calendar", PluginID: descriptors[0].ID, Action: "options"},
		{ID: "codex-personal", PluginID: descriptors[1].ID, Action: "open"},
	})
	want := []busyinput.App{
		{ID: "calendar", DisplayName: "Calendar", Action: "options"},
		{ID: "codex-personal", DisplayName: "Codex 1", Action: "open"},
		{ID: "codex-work", DisplayName: "Codex 2", Action: "open"},
		{ID: "unknown-app", DisplayName: "unknown app", Action: "open"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("launcher apps = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(fallbackIDs, []string{"unknown-app"}) {
		t.Fatalf("fallback IDs = %#v", fallbackIDs)
	}
}

func TestLauncherAppFallbackIsNeverEmpty(t *testing.T) {
	t.Parallel()
	got, fallback := launcherApp(daemon.LaunchableApp{ID: "---___", PluginID: "dev.example.unknown", Action: "open"})
	if !fallback || got.DisplayName != "---___" {
		t.Fatalf("launcher fallback = %#v, fallback=%v", got, fallback)
	}
}

func TestLauncherFallbackDiagnosticCacheTracksOnlyCurrentApps(t *testing.T) {
	t.Parallel()
	adapter := launcherAdapter{logs: pluginlog.New(io.Discard, pluginlog.Options{}), fallbackLogged: make(map[string]struct{})}
	adapter.logDisplayNameFallbacks([]string{"removed", "retained"})
	adapter.logDisplayNameFallbacks([]string{"retained", "added"})
	want := map[string]struct{}{"retained": {}, "added": {}}
	if !reflect.DeepEqual(adapter.fallbackLogged, want) {
		t.Fatalf("fallback cache = %#v, want %#v", adapter.fallbackLogged, want)
	}
}
