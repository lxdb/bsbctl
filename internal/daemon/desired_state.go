package daemon

import (
	"errors"
	"sync"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/localstate"
)

type ConfigurationStore interface {
	Load() (config.Document, error)
	Update(uint64, func(*config.Document) error) (config.Document, localstate.CommitOutcome, error)
}

// DesiredState owns durable configuration transactions, schema validation,
// and persistence health. It never starts plugins or performs device I/O.
type DesiredState struct {
	mu          sync.Mutex
	statusMu    sync.RWMutex
	store       ConfigurationStore
	validator   DesiredStateValidator
	persistence ConfigPersistenceStatus
}

func NewDesiredState(store ConfigurationStore, validator DesiredStateValidator) (*DesiredState, error) {
	if store == nil {
		return nil, errors.New("configuration store is required")
	}
	return &DesiredState{store: store, validator: validator}, nil
}

func (d *DesiredState) recordOutcome(outcome localstate.CommitOutcome) {
	d.statusMu.Lock()
	defer d.statusMu.Unlock()
	if outcome == localstate.CommittedDurabilityUncertain {
		d.persistence = ConfigPersistenceStatus{LastErrorCode: ConfigDurabilityUncertainCode}
		return
	}
	if outcome == localstate.Committed {
		d.persistence = ConfigPersistenceStatus{}
	}
}

func (d *DesiredState) PersistenceStatus() ConfigPersistenceStatus {
	d.statusMu.RLock()
	defer d.statusMu.RUnlock()
	return d.persistence
}
