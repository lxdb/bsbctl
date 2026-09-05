package daemonrun

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	busyinput "github.com/lxdb/bsbctl/internal/input"
	"github.com/lxdb/bsbctl/internal/pluginlog"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type launcherAdapter struct {
	reconciler     *daemon.Reconciler
	logs           *pluginlog.Sink
	mu             sync.Mutex
	fallbackLogged map[string]struct{}
}

func (a *launcherAdapter) Apps() []busyinput.App {
	result, fallbackIDs := launcherApps(a.reconciler.LaunchableApps())
	a.logDisplayNameFallbacks(fallbackIDs)
	return result
}

func launcherApps(apps []daemon.LaunchableApp) ([]busyinput.App, []string) {
	descriptors := firstpartyplugins.All()
	pluginOrder := make(map[string]int, len(descriptors))
	for index, descriptor := range descriptors {
		pluginOrder[descriptor.ID] = index
	}
	apps = slices.Clone(apps)
	slices.SortFunc(apps, func(left, right daemon.LaunchableApp) int {
		leftOrder, leftKnown := pluginOrder[left.PluginID]
		rightOrder, rightKnown := pluginOrder[right.PluginID]
		if leftKnown != rightKnown {
			if leftKnown {
				return -1
			}
			return 1
		}
		if leftKnown && leftOrder != rightOrder {
			return cmp.Compare(leftOrder, rightOrder)
		}
		if left.PluginID != right.PluginID {
			return cmp.Compare(left.PluginID, right.PluginID)
		}
		return cmp.Compare(left.ID, right.ID)
	})

	instanceCounts := make(map[string]int, len(apps))
	for _, app := range apps {
		instanceCounts[app.PluginID]++
	}
	instanceOrdinals := make(map[string]int, len(instanceCounts))
	result := make([]busyinput.App, 0, len(apps))
	fallbackIDs := make([]string, 0)
	for _, app := range apps {
		entry, fallback := launcherApp(app)
		if instanceCounts[app.PluginID] > 1 {
			instanceOrdinals[app.PluginID]++
			entry.DisplayName = fmt.Sprintf("%s %d", entry.DisplayName, instanceOrdinals[app.PluginID])
		}
		result = append(result, entry)
		if fallback {
			fallbackIDs = append(fallbackIDs, app.ID)
		}
	}
	return result, fallbackIDs
}

func launcherApp(app daemon.LaunchableApp) (busyinput.App, bool) {
	displayName := ""
	if descriptor, ok := firstpartyplugins.LookupID(app.PluginID); ok {
		displayName = strings.TrimSpace(descriptor.DisplayName)
	}
	fallback := displayName == ""
	if fallback {
		displayName = strings.Join(strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(app.ID)), " ")
		if displayName == "" {
			displayName = strings.TrimSpace(app.ID)
		}
		if displayName == "" {
			displayName = "Unnamed app"
		}
	}
	return busyinput.App{ID: app.ID, DisplayName: displayName, Action: app.Action}, fallback
}

func (a *launcherAdapter) logDisplayNameFallbacks(appIDs []string) {
	current := make(map[string]struct{}, len(appIDs))
	for _, appID := range appIDs {
		current[appID] = struct{}{}
	}

	a.mu.Lock()
	for appID := range a.fallbackLogged {
		if _, exists := current[appID]; !exists {
			delete(a.fallbackLogged, appID)
		}
	}
	newFallbacks := make([]string, 0, len(appIDs))
	for _, appID := range appIDs {
		if _, exists := a.fallbackLogged[appID]; exists {
			continue
		}
		a.fallbackLogged[appID] = struct{}{}
		newFallbacks = append(newFallbacks, appID)
	}
	a.mu.Unlock()

	for _, appID := range newFallbacks {
		a.logs.Log("bsbctl", protocol.LogNotification{
			Level: protocol.LogLevelWarn, Event: "launcher_display_name_fallback",
			Fields: map[string]string{"app_id": appID},
		})
	}
}

func (a *launcherAdapter) Launch(ctx context.Context, appID, action string) error {
	return a.reconciler.Launch(ctx, appID, action, nil)
}
