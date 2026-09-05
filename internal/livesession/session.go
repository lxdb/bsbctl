// Package livesession owns the shared lifecycle of plugin foreground sessions.
package livesession

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

type Host interface {
	PublishObservation(context.Context, protocol.Observation) error
	CompleteSession(context.Context, protocol.CompleteSessionRequest) error
}

type Session struct {
	mu       sync.Mutex
	host     Host
	instance protocol.InstanceRef
	channel  string
	key      string
	validFor time.Duration
	now      func() time.Time
	token    string
	revision uint64
	current  *protocol.Scene
}

func New(host Host, instance protocol.InstanceRef, channel, key string, validFor time.Duration, now func() time.Time) *Session {
	if now == nil {
		now = time.Now
	}
	return &Session{host: host, instance: instance, channel: channel, key: key, validFor: validFor, now: now}
}

func (s *Session) Start(ctx context.Context, token string, fallback protocol.Scene, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" {
		return errors.New("interaction session is already active")
	}
	s.token = token
	scene := &fallback
	if s.current != nil {
		scene = s.current
	}
	if err := s.publishLocked(ctx, scene, protocol.DispositionSnapshot, reason, s.now().UTC()); err != nil {
		s.token = ""
		return err
	}
	return nil
}

func (s *Session) SetCurrent(ctx context.Context, scene protocol.Scene, reason string, observedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = new(scene)
	if s.token == "" {
		return nil
	}
	return s.publishLocked(ctx, s.current, protocol.DispositionSnapshot, reason, observedAt.UTC())
}

func (s *Session) PublishTransient(ctx context.Context, scene protocol.Scene, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == "" {
		return nil
	}
	return s.publishLocked(ctx, &scene, protocol.DispositionSnapshot, reason, s.now().UTC())
}

func (s *Session) Finish(ctx context.Context, token string, notify bool, reason string) error {
	s.mu.Lock()
	if s.token == "" || token != "" && s.token != token {
		s.mu.Unlock()
		return nil
	}
	completedToken := s.token
	s.token = ""
	resolveErr := s.publishLocked(ctx, nil, protocol.DispositionResolved, reason, s.now().UTC())
	s.mu.Unlock()
	if !notify || s.host == nil {
		return resolveErr
	}
	completeErr := s.host.CompleteSession(ctx, protocol.CompleteSessionRequest{Instance: s.instance, SessionToken: completedToken})
	return errors.Join(resolveErr, completeErr)
}

func (s *Session) publishLocked(ctx context.Context, scene *protocol.Scene, disposition protocol.Disposition, reason string, now time.Time) error {
	if s.host == nil {
		return errors.New("live session host is unavailable")
	}
	s.revision++
	value := protocol.Observation{
		Instance: s.instance, Channel: s.channel, Key: s.key, Revision: s.revision,
		Disposition: disposition, Impact: protocol.ImpactNormal, ReasonCode: reason,
		ObservedAt: now.UTC(), UpdatedAt: now.UTC(), Scene: scene,
	}
	if scene != nil {
		value.ValidUntil = now.Add(s.validFor).UTC()
	}
	return s.host.PublishObservation(ctx, value)
}
