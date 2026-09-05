package daemon

import (
	"context"
	"errors"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/localstate"
)

// ConfigStoreActivator is used only during installer recovery before the
// runtime Reconciler (and therefore any plugin child) is constructed.
type ConfigStoreActivator struct {
	store ConfigurationStore
}

func NewConfigStoreActivator(store ConfigurationStore) *ConfigStoreActivator {
	return &ConfigStoreActivator{store: store}
}

func (a *ConfigStoreActivator) DesiredPlugin(ctx context.Context, pluginID string) (*config.Plugin, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil || a.store == nil {
		return nil, errors.New("configuration store is unavailable")
	}
	document, err := a.store.Load()
	if err != nil {
		return nil, err
	}
	plugin, exists := document.Plugins[pluginID]
	if !exists {
		return nil, nil
	}
	copy := clonePlugin(plugin)
	return &copy, nil
}

func (a *ConfigStoreActivator) ActivatePlugin(ctx context.Context, plugin config.Plugin) (localstate.CommitOutcome, error) {
	if err := ctx.Err(); err != nil {
		return localstate.NotCommitted, err
	}
	if a == nil || a.store == nil {
		return localstate.NotCommitted, errors.New("configuration store is unavailable")
	}
	document, err := a.store.Load()
	if err != nil {
		return localstate.NotCommitted, err
	}
	_, outcome, err := a.store.Update(document.Generation, func(next *config.Document) error {
		next.Plugins[plugin.ID] = clonePlugin(plugin)
		return nil
	})
	return outcome, err
}
