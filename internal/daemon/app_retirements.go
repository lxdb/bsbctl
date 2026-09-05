package daemon

import (
	"context"
	"errors"
	"sync"
)

// appRetirements separates committed desired-state transactions from the
// slower runtime cleanup that follows deletion. Only mutations for the same
// app ID wait for that cleanup to finish.
type appRetirements struct {
	mu     sync.Mutex
	active map[string]chan struct{}
}

func (r *appRetirements) begin(appID string) func() {
	done := make(chan struct{})
	r.mu.Lock()
	if r.active == nil {
		r.active = make(map[string]chan struct{})
	}
	r.active[appID] = done
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		if r.active[appID] == done {
			delete(r.active, appID)
			close(done)
		}
		r.mu.Unlock()
	}
}

func (r *appRetirements) lookup(appID string) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[appID]
}

// lockDesiredApp waits for an earlier deletion of appID, then returns with the
// desired-state transaction mutex held. Checking the fence while holding that
// mutex closes the race with a deletion registering its fence after commit.
func (s *Reconciler) lockDesiredApp(ctx context.Context, appID string) error {
	waited := false
	for {
		s.desired.mu.Lock()
		done := s.appRetirements.lookup(appID)
		if done == nil {
			if waited {
				select {
				case <-ctx.Done():
					s.desired.mu.Unlock()
					return ctx.Err()
				case <-s.retryCtx.Done():
					s.desired.mu.Unlock()
					return errors.New("daemon is closing")
				default:
				}
			}
			return nil
		}
		s.desired.mu.Unlock()
		waited = true
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		case <-s.retryCtx.Done():
			return errors.New("daemon is closing")
		}
	}
}
