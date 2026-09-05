package codex

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

type cardPublisher struct {
	host     observationHost
	instance protocol.InstanceRef
	now      func() time.Time
	mu       sync.Mutex
	revision uint64
	active   map[string]publishedCard
}

type publishedCard struct {
	card     Card
	revision uint64
	detail   bool
}

func newCardPublisher(host observationHost, instance protocol.InstanceRef, now func() time.Time) *cardPublisher {
	if now == nil {
		now = time.Now
	}
	return &cardPublisher{
		host: host, instance: instance, now: now,
		active: make(map[string]publishedCard),
	}
}

func (p *cardPublisher) Publish(ctx context.Context, cards []Card) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host == nil || p.instance.Validate() != nil {
		return errors.New("Codex observation publisher is not configured")
	}
	desired := make(map[string]Card, len(cards))
	for _, card := range cards {
		identity := observationIdentity(card.Channel, card.Key)
		if _, exists := desired[identity]; exists {
			return errors.New("Codex cards contain a duplicate identity")
		}
		desired[identity] = card
	}
	for _, identity := range sortedCardKeys(desired) {
		card := desired[identity]
		scene := cardPresentation(card)
		p.revision++
		if err := p.host.PublishObservation(ctx, protocol.Observation{
			Instance: p.instance, Channel: card.Channel, Key: card.Key, Revision: p.revision,
			Disposition: card.Disposition, Impact: card.Impact, ReasonCode: card.ReasonCode,
			ObservedAt: card.ObservedAt.UTC(), UpdatedAt: p.now().UTC(), ValidUntil: card.ValidUntil.UTC(),
			Scene: new(scene),
		}); err != nil {
			return err
		}
		p.active[identity] = publishedCard{card: card, revision: p.revision}
	}
	for _, identity := range sortedActiveKeys(p.active) {
		published := p.active[identity]
		if published.detail {
			continue
		}
		if _, exists := desired[identity]; exists {
			continue
		}
		now := p.now().UTC()
		p.revision++
		if err := p.host.PublishObservation(ctx, protocol.Observation{
			Instance: p.instance, Channel: published.card.Channel, Key: published.card.Key, Revision: p.revision,
			Disposition: protocol.DispositionResolved, Impact: protocol.ImpactNormal,
			ReasonCode: "codex_state_cleared", ObservedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		delete(p.active, identity)
	}
	return nil
}

func (p *cardPublisher) PublishDetail(ctx context.Context, card Card) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host == nil || p.instance.Validate() != nil || card.Channel != ChannelDetail || card.Key == "" {
		return errors.New("Codex detail observation publisher is not configured")
	}
	p.revision++
	scene := cardPresentation(card)
	if err := p.host.PublishObservation(ctx, protocol.Observation{
		Instance: p.instance, Channel: card.Channel, Key: card.Key, Revision: p.revision,
		Disposition: card.Disposition, Impact: card.Impact, ReasonCode: card.ReasonCode,
		ObservedAt: card.ObservedAt.UTC(), UpdatedAt: p.now().UTC(), ValidUntil: card.ValidUntil.UTC(),
		Scene: new(scene),
	}); err != nil {
		return err
	}
	p.active[observationIdentity(card.Channel, card.Key)] = publishedCard{card: card, revision: p.revision, detail: true}
	return nil
}

func cardPresentation(card Card) protocol.Scene {
	if len(card.Scene.Elements) != 0 {
		return card.Scene
	}
	return cardScene(card)
}

func (p *cardPublisher) ResolveDetail(ctx context.Context, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	identity := observationIdentity(ChannelDetail, key)
	published, exists := p.active[identity]
	if !exists || !published.detail {
		return nil
	}
	now := p.now().UTC()
	p.revision++
	if err := p.host.PublishObservation(ctx, protocol.Observation{
		Instance: p.instance, Channel: ChannelDetail, Key: key, Revision: p.revision,
		Disposition: protocol.DispositionResolved, Impact: protocol.ImpactNormal,
		ReasonCode: "codex_detail_closed", ObservedAt: now, UpdatedAt: now,
	}); err != nil {
		return err
	}
	delete(p.active, identity)
	return nil
}

// Lookup resolves only the latest successfully published observation revision.
func (p *cardPublisher) Lookup(channel, key string, revision uint64) (Card, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	published, ok := p.active[observationIdentity(channel, key)]
	if !ok || published.revision != revision {
		return Card{}, false
	}
	return published.card, true
}

func observationIdentity(channel, key string) string { return channel + "\x00" + key }

func sortedCardKeys(values map[string]Card) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func sortedActiveKeys(values map[string]publishedCard) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
