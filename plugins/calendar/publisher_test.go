package calendar

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestCalendarPublisherPublishesSelectedCardsAndResolvesRemovedCards(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	upcoming := calendarEvent{CalendarID: "work", EventID: "next", Title: "Next", Start: now.Add(2 * time.Minute), End: now.Add(time.Hour)}
	active := calendarEvent{CalendarID: "work", EventID: "active", Title: "Active", Start: now.Add(-time.Minute), End: now.Add(20 * time.Minute)}
	host := &publisherHost{}
	publisher := newCalendarPublisher(host, protocol.InstanceRef{ID: AppID, Generation: 1}, mustCalendarConfig(t), func() time.Time { return now }, testCalendarScene)

	if err := publisher.Publish(context.Background(), selectedEvents{Upcoming: &upcoming, Active: &active}); err != nil {
		t.Fatal(err)
	}
	if len(host.observations) != 2 {
		t.Fatalf("observations = %#v", host.observations)
	}
	first, second := host.observations[0], host.observations[1]
	if first.Instance != (protocol.InstanceRef{ID: AppID, Generation: 1}) || first.Channel != ChannelUpcoming || first.Key != observationKey(upcoming) || first.Revision != 1 || first.Disposition != protocol.DispositionActionable || !first.ObservedAt.Equal(upcoming.Start.Add(-5*time.Minute)) || !first.ValidUntil.Equal(upcoming.Start) {
		t.Fatalf("upcoming observation = %#v", first)
	}
	if second.Channel != ChannelActive || second.Key != observationKey(active) || second.Revision != 2 || second.Disposition != protocol.DispositionActionable || !second.ObservedAt.Equal(active.Start) || !second.ValidUntil.Equal(active.End) {
		t.Fatalf("active observation = %#v", second)
	}
	if second.BusyTimer != nil || second.Scene == nil || len(second.Scene.Elements) == 0 {
		t.Fatalf("active presentation = %#v", second)
	}
	if !publisher.Matches(ChannelUpcoming, observationKey(upcoming), first.Revision) {
		t.Fatal("publisher did not recognize the delivered upcoming revision")
	}
	if publisher.Matches(ChannelUpcoming, observationKey(upcoming), first.Revision+1) || publisher.Matches(ChannelActive, observationKey(upcoming), first.Revision) {
		t.Fatal("publisher accepted a stale or mismatched observation trigger")
	}

	if err := publisher.Publish(context.Background(), selectedEvents{}); err != nil {
		t.Fatal(err)
	}
	if len(host.observations) != 4 {
		t.Fatalf("observations after withdrawal = %#v", host.observations)
	}
	for _, resolved := range host.observations[2:] {
		if resolved.Disposition != protocol.DispositionResolved || resolved.ReasonCode != "calendar_state_cleared" || resolved.Scene != nil {
			t.Fatalf("resolution = %#v", resolved)
		}
	}
	if publisher.Matches(ChannelUpcoming, observationKey(upcoming), first.Revision) {
		t.Fatal("publisher retained a resolved observation trigger")
	}
}

func TestCalendarPublisherRetriesTheExactObservationAfterUncertainDelivery(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	upcoming := calendarEvent{CalendarID: "work", EventID: "next", Title: "Next", Start: now.Add(2 * time.Minute), End: now.Add(time.Hour)}
	host := &publisherHost{failures: 1}
	publisher := newCalendarPublisher(host, protocol.InstanceRef{ID: AppID, Generation: 1}, mustCalendarConfig(t), func() time.Time { return now }, testCalendarScene)

	if err := publisher.Publish(context.Background(), selectedEvents{Upcoming: &upcoming}); err == nil {
		t.Fatal("first publication unexpectedly succeeded")
	}
	if err := publisher.Publish(context.Background(), selectedEvents{Upcoming: &upcoming}); err != nil {
		t.Fatal(err)
	}
	if len(host.observations) != 2 || !reflect.DeepEqual(host.observations[0], host.observations[1]) {
		t.Fatalf("publication retry = %#v", host.observations)
	}
}

func TestCalendarPublisherSupersedesExpiredPendingAudioWithoutMutatingAttemptedRevision(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	clock := now
	active := calendarEvent{CalendarID: "work", EventID: "active", Title: "Active", Start: now, End: now.Add(time.Hour)}
	host := &publisherHost{failures: 1}
	publisher := newCalendarPublisher(host, protocol.InstanceRef{ID: AppID, Generation: 1}, mustCalendarConfig(t), func() time.Time { return clock }, testCalendarScene)

	if err := publisher.Publish(t.Context(), selectedEvents{Active: &active}); err == nil {
		t.Fatal("first publication unexpectedly succeeded")
	}
	clock = now.Add(calendarSoundWindow + time.Second)
	if err := publisher.Publish(t.Context(), selectedEvents{Active: &active}); err != nil {
		t.Fatal(err)
	}
	if len(host.observations) != 2 || host.observations[0].Revision != 1 || host.observations[0].Audio == nil {
		t.Fatalf("first attempted observation = %#v", host.observations)
	}
	if host.observations[1].Revision != 2 || host.observations[1].Audio != nil || host.observations[1].Scene == nil {
		t.Fatalf("visual-only successor = %#v", host.observations[1])
	}
}

func TestCalendarPublisherDoesNotRetryDeterministicallyRejectedObservation(t *testing.T) {
	now := time.Date(2026, time.August, 23, 17, 0, 0, 0, time.UTC)
	first := calendarEvent{CalendarID: "work", EventID: "first", Title: "First", Start: now, End: now.Add(time.Hour)}
	second := calendarEvent{CalendarID: "work", EventID: "second", Title: "Second", Start: now, End: now.Add(time.Hour)}
	host := &publisherHost{errors: []error{protocol.NewDomainError(protocol.ErrorInvalidArgument, errors.New("invalid"))}}
	publisher := newCalendarPublisher(host, protocol.InstanceRef{ID: AppID, Generation: 1}, mustCalendarConfig(t), func() time.Time { return now }, testCalendarScene)

	if err := publisher.Publish(t.Context(), selectedEvents{Active: &first}); err == nil {
		t.Fatal("deterministically invalid publication unexpectedly succeeded")
	}
	if err := publisher.Publish(t.Context(), selectedEvents{Active: &second}); err != nil {
		t.Fatal(err)
	}
	if len(host.observations) != 2 || host.observations[1].Key != observationKey(second) {
		t.Fatalf("deterministic failure was retried: %#v", host.observations)
	}
}

func testCalendarScene(card calendarCard) protocol.Scene {
	return protocol.Scene{Elements: []protocol.Element{{ID: "state", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: card.State}}}}
}

type publisherHost struct {
	observations []protocol.Observation
	failures     int
	errors       []error
}

func (h *publisherHost) PublishObservation(_ context.Context, observation protocol.Observation) error {
	h.observations = append(h.observations, observation)
	if len(h.errors) > 0 {
		err := h.errors[0]
		h.errors = h.errors[1:]
		return err
	}
	if h.failures > 0 {
		h.failures--
		return errors.New("delivery uncertain")
	}
	return nil
}
