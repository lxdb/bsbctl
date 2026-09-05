package calendar

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

type observationHost interface {
	PublishObservation(context.Context, protocol.Observation) error
}

type calendarPublisher struct {
	mu        sync.Mutex
	host      observationHost
	instance  protocol.InstanceRef
	config    Config
	now       func() time.Time
	scene     func(calendarCard) protocol.Scene
	revision  uint64
	active    map[string]cardIdentity
	delivered map[string]uint64
	pending   *pendingPublication
}

type cardIdentity struct {
	channel string
	key     string
}

type pendingPublication struct {
	observation protocol.Observation
	identity    string
	card        *calendarCard
	resolved    bool
}

func newCalendarPublisher(
	host observationHost,
	instance protocol.InstanceRef,
	config Config,
	now func() time.Time,
	scene func(calendarCard) protocol.Scene,
) *calendarPublisher {
	if now == nil {
		now = time.Now
	}
	return &calendarPublisher{
		host: host, instance: instance, config: config, now: now, scene: scene,
		active: make(map[string]cardIdentity), delivered: make(map[string]uint64),
	}
}

func (p *calendarPublisher) Publish(ctx context.Context, selected selectedEvents) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host == nil || p.instance.Validate() != nil || p.scene == nil {
		return errors.New("Calendar observation publisher is not configured")
	}
	cards := cardsFromSelection(selected, p.config)
	desired := make(map[string]calendarCard, len(cards))
	for _, card := range cards {
		identity := cardIdentityKey(card.Channel, card.Key)
		if _, duplicate := desired[identity]; duplicate {
			return errors.New("Calendar cards contain a duplicate identity")
		}
		desired[identity] = card
	}

	skipIdentity := ""
	if p.pending != nil {
		pending := p.pending
		if !pending.resolved && !pending.observation.ValidUntil.After(p.now().UTC()) {
			p.pending = nil
		} else {
			if pending.card != nil && pending.observation.Audio != nil && !pending.observation.Audio.ExpiresAt.After(p.now().UTC()) {
				p.revision = max(p.revision, pending.observation.Revision)
				pending.observation = p.observationForCard(*pending.card)
			}
			if err := p.host.PublishObservation(ctx, pending.observation); err != nil {
				if !retryCalendarPublication(err) {
					p.pending = nil
				}
				return err
			}
			p.commit(*pending)
			if pending.card != nil {
				if current, exists := desired[pending.identity]; exists && sameCalendarCard(current, *pending.card) {
					skipIdentity = pending.identity
				}
			}
			p.pending = nil
		}
	}

	for _, card := range cards {
		identity := cardIdentityKey(card.Channel, card.Key)
		if identity == skipIdentity {
			continue
		}
		observation := p.observationForCard(card)
		pendingCard := card
		operation := pendingPublication{observation: observation, identity: identity, card: &pendingCard}
		if err := p.host.PublishObservation(ctx, observation); err != nil {
			if retryCalendarPublication(err) {
				p.pending = &operation
			}
			return err
		}
		p.commit(operation)
	}

	for _, identity := range sortedCalendarIdentities(p.active) {
		if _, exists := desired[identity]; exists {
			continue
		}
		card := p.active[identity]
		now := p.now().UTC()
		observation := protocol.Observation{
			Instance: p.instance, Channel: card.channel, Key: card.key, Revision: p.revision + 1,
			Disposition: protocol.DispositionResolved, Impact: protocol.ImpactNormal,
			ReasonCode: "calendar_state_cleared", ObservedAt: now, UpdatedAt: now,
		}
		operation := pendingPublication{observation: observation, identity: identity, resolved: true}
		if err := p.host.PublishObservation(ctx, observation); err != nil {
			if retryCalendarPublication(err) {
				p.pending = &operation
			}
			return err
		}
		p.commit(operation)
	}
	return nil
}

func retryCalendarPublication(err error) bool {
	domain, ok := errors.AsType[*protocol.DomainError](err)
	if !ok {
		return true
	}
	return domain.Kind() == protocol.ErrorNotReady
}

func sameCalendarCard(left, right calendarCard) bool {
	leftTimer, rightTimer := left.BusyTimer, right.BusyTimer
	leftCue, rightCue := left.AudioCue, right.AudioCue
	left.BusyTimer, right.BusyTimer, left.AudioCue, right.AudioCue = nil, nil, nil, nil
	if left != right {
		return false
	}
	if (leftTimer == nil) != (rightTimer == nil) || (leftCue == nil) != (rightCue == nil) {
		return false
	}
	if leftTimer != nil && *leftTimer != *rightTimer {
		return false
	}
	return leftCue == nil || *leftCue == *rightCue
}

func (p *calendarPublisher) Matches(channel, key string, revision uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	identity := cardIdentityKey(channel, key)
	card, exists := p.active[identity]
	if !exists || card.channel != channel || card.key != key {
		return false
	}
	if p.pending != nil && p.pending.identity == identity && p.pending.resolved {
		return false
	}
	return p.delivered[identity] == revision
}

func (p *calendarPublisher) observationForCard(card calendarCard) protocol.Observation {
	now := p.now().UTC()
	observation := protocol.Observation{
		Instance: p.instance, Channel: card.Channel, Key: card.Key, Revision: p.revision + 1,
		Disposition: card.Disposition, Impact: card.Impact, ReasonCode: card.ReasonCode,
		ObservedAt: card.ObservedAt.UTC(), UpdatedAt: now, ValidUntil: card.ValidUntil.UTC(), BusyTimer: card.BusyTimer,
	}
	if card.AudioCue != nil && card.AudioCue.ExpiresAt.After(now) {
		cue := *card.AudioCue
		observation.Audio = &cue
	}
	if card.BusyTimer == nil {
		observation.Scene = new(p.scene(card))
	}
	return observation
}

func (p *calendarPublisher) commit(operation pendingPublication) {
	p.revision = operation.observation.Revision
	if operation.resolved {
		delete(p.active, operation.identity)
		delete(p.delivered, operation.identity)
		return
	}
	p.active[operation.identity] = cardIdentity{channel: operation.observation.Channel, key: operation.observation.Key}
	p.delivered[operation.identity] = operation.observation.Revision
}

func cardIdentityKey(channel, key string) string { return channel + "\x00" + key }

func sortedCalendarIdentities(values map[string]cardIdentity) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
