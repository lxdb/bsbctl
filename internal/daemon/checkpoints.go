package daemon

import (
	"cmp"
	"encoding/json"
	"errors"
	"slices"
	"sync"

	"github.com/lxdb/bsbctl/internal/checkpoint"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type CheckpointStore interface {
	Save(checkpoint.Key, json.RawMessage) (localstate.CommitOutcome, error)
	Load(checkpoint.Key) (json.RawMessage, bool, error)
	Reconcile([]checkpoint.Key) error
	Status() checkpoint.Status
}

// Checkpoints serializes non-secret plugin checkpoint persistence. Admission
// always uses the exact active generation from LiveState.
type Checkpoints struct {
	mu    sync.Mutex
	store CheckpointStore
	live  *LiveState
}

func NewCheckpoints(store CheckpointStore, live *LiveState) (*Checkpoints, error) {
	if store == nil {
		return nil, errors.New("checkpoint store is required")
	}
	if live == nil {
		return nil, errors.New("live state is required")
	}
	return &Checkpoints{store: store, live: live}, nil
}

func (c *Checkpoints) SaveCheckpoint(pluginID string, request protocol.CheckpointRequest) error {
	if err := request.Validate(); err != nil {
		return &checkpoint.Error{Code: checkpoint.InvalidCode, Outcome: localstate.NotCommitted, Err: err}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live.mu.RLock()
	app, exists := c.live.document.Apps[request.Instance.ID]
	authorized := c.live.loaded && exists && app.Enabled && app.PluginID == pluginID &&
		c.live.generations.matches(pluginID, request.Instance.ID, request.Instance.Generation)
	c.live.mu.RUnlock()
	if !authorized {
		return &checkpoint.Error{
			Code: checkpoint.InvalidCode, Outcome: localstate.NotCommitted,
			Err: errors.New("checkpoint identity is not active"),
		}
	}
	_, err := c.store.Save(checkpoint.Key{
		PluginID: pluginID, InstanceID: request.Instance.ID, Generation: request.Instance.Generation,
	}, slices.Clone(request.Data))
	return err
}

func (c *Checkpoints) reconcile(document config.Document) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.store.Reconcile(activeCheckpointKeys(document))
}

func (c *Checkpoints) attach(plan *ReconciliationPlan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for specIndex := range plan.Specs {
		for instanceIndex := range plan.Specs[specIndex].Instances {
			instance := &plan.Specs[specIndex].Instances[instanceIndex]
			data, found, err := c.store.Load(checkpoint.Key{
				PluginID: plan.Specs[specIndex].ID, InstanceID: instance.ID, Generation: instance.Generation,
			})
			if err != nil || !found {
				instance.Checkpoint = nil
				continue
			}
			instance.Checkpoint = &pluginhost.CheckpointRestore{
				Generation: instance.Generation, Data: slices.Clone(data),
			}
		}
	}
}

func (c *Checkpoints) Status() checkpoint.Status { return c.store.Status() }

func activeCheckpointKeys(document config.Document) []checkpoint.Key {
	result := make([]checkpoint.Key, 0, len(document.Apps))
	for _, app := range document.Apps {
		if app.Enabled {
			result = append(result, checkpoint.Key{PluginID: app.PluginID, InstanceID: app.ID, Generation: app.Generation})
		}
	}
	slices.SortFunc(result, func(left, right checkpoint.Key) int {
		return cmp.Or(cmp.Compare(left.PluginID, right.PluginID), cmp.Compare(left.InstanceID, right.InstanceID))
	})
	return result
}
