package daemon

import (
	"context"
	"errors"
)

func (s *Reconciler) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.live.mu.Lock()
		s.live.closing = true
		s.live.epoch++
		if s.live.activeCancel != nil {
			s.live.activeCancel()
		}
		s.live.mu.Unlock()
		s.retryCancel()
		go func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), serviceShutdownTimeout)
			defer cancel()
			err := s.plugins.Close(shutdownCtx)
			s.live.mu.Lock()
			s.pluginCloseErr = err
			s.live.mu.Unlock()
			close(s.pluginCloseDone)
		}()
	})
	s.live.mu.RLock()
	attempts := make([]<-chan struct{}, 0, len(s.live.attempts))
	for _, done := range s.live.attempts {
		attempts = append(attempts, done)
	}
	s.live.mu.RUnlock()
	for _, done := range attempts {
		select {
		case <-done:
		case <-ctx.Done():
			s.live.mu.RLock()
			pluginErr := s.pluginCloseErr
			s.live.mu.RUnlock()
			return errors.Join(ctx.Err(), pluginErr)
		}
	}
	s.live.mu.RLock()
	retryDone := (<-chan struct{})(s.retryDone)
	s.live.mu.RUnlock()
	pluginDone := (<-chan struct{})(s.pluginCloseDone)
	for retryDone != nil || pluginDone != nil {
		select {
		case <-retryDone:
			retryDone = nil
		case <-pluginDone:
			pluginDone = nil
		case <-ctx.Done():
			s.live.mu.RLock()
			pluginErr := s.pluginCloseErr
			s.live.mu.RUnlock()
			return errors.Join(ctx.Err(), pluginErr)
		}
	}
	s.live.mu.RLock()
	err := s.pluginCloseErr
	s.live.mu.RUnlock()
	return err
}
