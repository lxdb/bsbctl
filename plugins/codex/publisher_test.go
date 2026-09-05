package codex

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/plugins/codex/internal/appserver"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type recordingObservationHost struct {
	observations []protocol.Observation
}

func (h *recordingObservationHost) PublishObservation(_ context.Context, observation protocol.Observation) error {
	h.observations = append(h.observations, observation)
	return nil
}

func TestCardPublisherPublishesAndResolvesExactCardIdentities(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	host := &recordingObservationHost{}
	publisher := newCardPublisher(host, protocol.InstanceRef{ID: "codex-main", Generation: 7}, func() time.Time { return now })
	wait := Card{
		Channel: ChannelAttention, Key: "request.abc", StateWord: "WAIT CMD",
		ContextLine: "Codex request", DetailLine: "Command approval",
		Disposition: protocol.DispositionActionable, Impact: protocol.ImpactCritical,
		ReasonCode: "codex_wait_command", ObservedAt: now.Add(-time.Second), ValidUntil: now.Add(45 * time.Second),
	}
	if err := publisher.Publish(context.Background(), []Card{wait}); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(host.observations) != 2 {
		t.Fatalf("observations = %#v", host.observations)
	}
	published, resolved := host.observations[0], host.observations[1]
	if published.Instance != (protocol.InstanceRef{ID: "codex-main", Generation: 7}) || published.Channel != wait.Channel || published.Key != wait.Key {
		t.Fatalf("published identity = %#v", published)
	}
	if published.Disposition != protocol.DispositionActionable || len(published.Scene.Elements) == 0 {
		t.Fatalf("published observation = %#v", published)
	}
	if resolved.Channel != wait.Channel || resolved.Key != wait.Key || resolved.Disposition != protocol.DispositionResolved {
		t.Fatalf("resolved observation = %#v", resolved)
	}
	if resolved.Scene != nil || resolved.Revision <= published.Revision {
		t.Fatalf("resolved scene/revision = %#v", resolved)
	}
}

func TestCardPublisherDoesNotResolveSafeActivityDuringReconnectGrace(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress","items":[]}}`),
	}})
	host := &recordingObservationHost{}
	publisher := newCardPublisher(host, protocol.InstanceRef{ID: "codex-main", Generation: 7}, func() time.Time { return now })
	if err := publisher.Publish(context.Background(), reducer.Cards()); err != nil {
		t.Fatal(err)
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerDisconnected})
	if err := publisher.Publish(context.Background(), reducer.Cards()); err != nil {
		t.Fatal(err)
	}
	for _, observation := range host.observations {
		if observation.Channel == ChannelActivity && observation.Disposition == protocol.DispositionResolved {
			t.Fatalf("activity resolved during reconnect grace: %#v", observation)
		}
	}
}

func TestCardPublisherRenewsUnchangedCardsWithIncreasingRevisions(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	host := &recordingObservationHost{}
	publisher := newCardPublisher(host, protocol.InstanceRef{ID: "codex-main", Generation: 7}, func() time.Time { return now })
	card := connectionCard(true, now)
	if err := publisher.Publish(context.Background(), []Card{card}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Second)
	card = connectionCard(true, now)
	if err := publisher.Publish(context.Background(), []Card{card}); err != nil {
		t.Fatal(err)
	}
	if len(host.observations) != 2 || host.observations[1].Revision <= host.observations[0].Revision {
		t.Fatalf("observations = %#v", host.observations)
	}
	if got := host.observations[1].UpdatedAt; !got.Equal(now) {
		t.Fatalf("updated_at = %s, want %s", got, now)
	}
}

func TestCardPublisherLooksUpOnlyExactPublishedRevisionAndKeepsDetailSeparate(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	host := &recordingObservationHost{}
	publisher := newCardPublisher(host, protocol.InstanceRef{ID: "codex-main", Generation: 7}, func() time.Time { return now })
	card := connectionCard(true, now)
	if err := publisher.Publish(context.Background(), []Card{card}); err != nil {
		t.Fatal(err)
	}
	published := host.observations[len(host.observations)-1]
	if got, ok := publisher.Lookup(published.Channel, published.Key, published.Revision); !ok || got.Key != card.Key {
		t.Fatalf("exact lookup = %#v/%v", got, ok)
	}
	if _, ok := publisher.Lookup(published.Channel, published.Key, published.Revision+1); ok {
		t.Fatal("stale/unpublished revision resolved")
	}
	detail := Card{Channel: ChannelDetail, Key: "session.test", StateWord: "CODEX ON", ContextLine: "Detail", DetailLine: "Display only", Disposition: protocol.DispositionSnapshot, Impact: protocol.ImpactNormal, ReasonCode: "codex_detail", ObservedAt: now, ValidUntil: now.Add(time.Hour)}
	if err := publisher.PublishDetail(context.Background(), detail); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), []Card{card}); err != nil {
		t.Fatal(err)
	}
	detailPublished := host.observations[len(host.observations)-2]
	if _, ok := publisher.Lookup(detailPublished.Channel, detailPublished.Key, detailPublished.Revision); !ok {
		t.Fatal("regular refresh resolved the active detail card")
	}
	if err := publisher.ResolveDetail(context.Background(), detail.Key); err != nil {
		t.Fatal(err)
	}
	if _, ok := publisher.Lookup(detailPublished.Channel, detailPublished.Key, detailPublished.Revision); ok {
		t.Fatal("resolved detail card remained activatable")
	}
}
