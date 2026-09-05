package main

import (
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/installer"
)

func explicitLocalApps(raw string) ([]firstpartyplugins.Descriptor, error) {
	if raw == "" || raw == "none" {
		return nil, nil
	}
	var selected []firstpartyplugins.Descriptor
	seen := make(map[string]bool)
	for id := range strings.SplitSeq(raw, ",") {
		descriptor, ok := firstpartyplugins.LookupAppID(id)
		if !ok || seen[id] {
			return nil, fmt.Errorf("invalid or duplicate app ID %q", id)
		}
		selected = append(selected, descriptor)
		seen[id] = true
	}
	return selected, nil
}

func selectLocalPlugins(document config.Document, state installer.InstallState, raw string) ([]firstpartyplugins.Descriptor, error) {
	explicit, err := explicitLocalApps(raw)
	if err != nil {
		return nil, err
	}
	requested := make(map[string]bool)
	for _, descriptor := range explicit {
		if state.Plugins[descriptor.ID].Active != nil {
			return nil, fmt.Errorf("%s is catalog-managed; local installation will not replace its signed release", descriptor.DefaultApp.ID)
		}
		requested[descriptor.ID] = true
	}
	var selected []firstpartyplugins.Descriptor
	for _, descriptor := range firstpartyplugins.All() {
		_, configured := document.Plugins[descriptor.ID]
		if requested[descriptor.ID] || (raw != "none" && configured && state.Plugins[descriptor.ID].Active == nil) {
			selected = append(selected, descriptor)
		}
	}
	return selected, nil
}

func reconcileLocalDocument(current, built config.Document, raw string) (config.Document, error) {
	next := current
	next.Generation++
	next.Plugins = maps.Clone(current.Plugins)
	next.Apps = maps.Clone(current.Apps)
	if next.Apps == nil {
		next.Apps = make(map[string]config.App)
	}
	if next.Plugins == nil {
		next.Plugins = make(map[string]config.Plugin)
	}
	maps.Copy(next.Plugins, built.Plugins)
	explicit, err := explicitLocalApps(raw)
	if err != nil {
		return config.Document{}, err
	}
	for _, descriptor := range explicit {
		if _, exists := built.Plugins[descriptor.ID]; !exists {
			return config.Document{}, errors.New("selected local package was not built")
		}
		if existing, exists := next.Apps[descriptor.DefaultApp.ID]; exists {
			if existing.PluginID != descriptor.ID {
				return config.Document{}, fmt.Errorf("app %s belongs to another plugin", existing.ID)
			}
			continue
		}
		app := descriptor.DefaultApp
		app.Generation = next.Generation
		next.Apps[app.ID] = app
	}
	if err := next.Validate(); err != nil {
		return config.Document{}, fmt.Errorf("current settings are incompatible with the local packages: %w", err)
	}
	return next, nil
}
