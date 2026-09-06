package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestPublicationFreshnessRetryAndEpisodeIdentity(t *testing.T) {
	_, w, host := panelFixture(t)
	now := fixtureNow
	w.now = func() time.Time { return now }
	host.failPublish = true
	if err := w.publishResident(t.Context()); err == nil {
		t.Fatal("publication failure hidden")
	}
	host.failPublish = false
	now = now.Add(15 * time.Second)
	w.live()
	if err := w.publishResident(t.Context()); err != nil {
		t.Fatal(err)
	}
	var first protocol.Observation
	for _, o := range host.observations {
		if err := o.Validate(now); err != nil {
			t.Fatal(err)
		}
		if o.Channel == ChannelAttention {
			first = o
		}
	}
	if first.Revision == 0 || !first.ValidUntil.Equal(fixtureNow.Add(45*time.Second)) {
		t.Fatalf("initial lease %+v", first)
	}
	now = now.Add(15 * time.Second)
	if err := w.publishResident(t.Context()); err != nil {
		t.Fatal(err)
	}
	var renewed protocol.Observation
	for _, o := range host.observations {
		if o.Channel == ChannelAttention {
			renewed = o
		}
	}
	if renewed.Revision <= first.Revision || !renewed.ObservedAt.Equal(first.ObservedAt) || !renewed.ValidUntil.Equal(first.ValidUntil) {
		t.Fatalf("renewal changes episode/deadline: %+v", renewed)
	}
	now = fixtureNow.Add(45 * time.Second)
	if err := w.publishResident(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(host.withdrawals) != 1 {
		t.Fatalf("stale withdrawals: %v", host.withdrawals)
	}
	last := host.observations[len(host.observations)-1]
	raw, _ := json.Marshal(last)
	if last.Channel != ChannelConnection || !strings.Contains(string(raw), "Slack activity may be incomplete") {
		t.Fatalf("no stale warning %s", raw)
	}
	w.live()
	if err := w.publishResident(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func TestPublisherExpiresWithoutSourceNotification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, w, host := panelFixture(t)
		w.now = time.Now
		w.live()
		done := make(chan struct{})
		go func() { w.runPublisher(); close(done) }()
		synctest.Wait()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		host.pubMu.Lock()
		withdrawals := len(host.withdrawals)
		host.pubMu.Unlock()
		if withdrawals != 1 {
			t.Fatalf("expiry without source event: %d", withdrawals)
		}
		w.cancel()
		<-done
	})
}

func TestResidentPublicationIsQuietForAmbientChannelActivity(t *testing.T) {
	_, w, host := panelFixture(t)
	w.reduce(callback("EvChannel", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"2.000001","text":"ordinary channel update"}`))
	if err := w.publishResident(t.Context()); err != nil {
		t.Fatal(err)
	}
	attention := 0
	for _, observation := range host.observations {
		if observation.Channel == ChannelSummary {
			t.Fatal("Slack published an ambient summary")
		}
		if observation.Channel == ChannelAttention {
			attention++
		}
	}
	if attention != 1 {
		t.Fatalf("attention observations = %d, want only the direct message", attention)
	}
	for _, publication := range w.publications.current {
		if publication.observation.Channel == ChannelAttention && publication.target.Kind != "dm" {
			t.Fatalf("ambient activity entered attention: %+v", publication.target)
		}
	}
	if got := len(pendingItems(w.snapshot().Items)); got != 2 {
		t.Fatalf("manual pending list = %d, want both retained activities", got)
	}
}
func TestPublicationAttentionBoundAndPrivacy(t *testing.T) {
	h, w, host := panelFixture(t)
	for i := 2; i <= 40; i++ {
		w.reduce(callback(fmt.Sprint(i), fmt.Sprintf(`{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"%d.000001","text":"private-canary"}`, i)))
	}
	if err := w.publishResident(t.Context()); err != nil {
		t.Fatal(err)
	}
	attention := 0
	for _, o := range host.observations {
		if o.Channel == ChannelAttention {
			attention++
		}
		raw, _ := json.Marshal(o)
		for _, secret := range []string{"private-canary", "D123", "T123", "U456", "slack://"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("observation leaks %q", secret)
			}
		}
	}
	if attention != 32 {
		t.Fatalf("attention=%d", attention)
	}
	for _, query := range []string{"status", "items"} {
		r, err := h.InvokeOperation(t.Context(), protocol.OperationRequest{Instance: w.instance.Ref(), Operation: query, Payload: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"private-canary", "D123", "T123", "U456", "slack://"} {
			if strings.Contains(string(r.Payload), secret) {
				t.Fatalf("query leaks %q", secret)
			}
		}
	}
}
func TestObservationTriggerCapturesExactPublishedRevision(t *testing.T) {
	h, w, host := panelFixture(t)
	if err := w.publishResident(t.Context()); err != nil {
		t.Fatal(err)
	}
	var o protocol.Observation
	for _, v := range host.observations {
		if v.Channel == ChannelAttention {
			o = v
		}
	}
	trigger := &protocol.SessionTrigger{Kind: protocol.SessionTriggerObservation, Observation: &protocol.ObservationRef{Channel: o.Channel, Key: o.Key, Revision: o.Revision}}
	startPanel(t, h, w, trigger)
	if w.panel.level != panelDetail || w.panel.target.ID != o.Key {
		t.Fatal("observation opened wrong item")
	}
	if err := h.EndSession(t.Context(), protocol.SessionEndRequest{Instance: w.instance.Ref(), SessionToken: "session-1"}); err != nil {
		t.Fatal(err)
	}
	trigger.Observation.Revision++
	if err := h.StartSession(t.Context(), protocol.SessionStartRequest{Instance: w.instance.Ref(), SessionToken: "session-2", Action: "open", Trigger: trigger}); err == nil {
		t.Fatal("accepted wrong observation revision")
	}
}
func TestLiveRefreshDoesNotFollowEveryTransportHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, w, host := panelFixture(t)
		w.now = time.Now
		w.live()
		startPanel(t, h, w, nil)
		done := make(chan struct{})
		go func() { w.runPublisher(); close(done) }()
		synctest.Wait()
		for range 4 {
			time.Sleep(time.Second)
			w.live()
			synctest.Wait()
		}
		host.pubMu.Lock()
		live := 0
		for _, o := range host.observations {
			if o.Channel == ChannelLive {
				live++
			}
		}
		host.pubMu.Unlock()
		w.cancel()
		<-done
		if live != 1 {
			t.Fatalf("live scene refreshed %d times in four seconds", live)
		}
	})
}

func TestFailedPublicationDoesNotRetryOnEveryHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, w, host := panelFixture(t)
		w.now = time.Now
		w.live()
		calls := 0
		host.publish = func(context.Context, protocol.Observation) error { calls++; return errPublication }
		done := make(chan struct{})
		go func() { w.runPublisher(); close(done) }()
		synctest.Wait()
		for range 4 {
			time.Sleep(time.Second)
			w.live()
			synctest.Wait()
		}
		w.cancel()
		<-done
		if calls != 1 {
			t.Fatalf("failed host retried %d times without domain change", calls)
		}
	})
}

func TestPublicationRechecksSourceAfterBlockedHostBoundary(t *testing.T) {
	_, w, host := panelFixture(t)
	host.publish = func(context.Context, protocol.Observation) error { w.disconnected("auth_required"); return nil }
	if err := w.publishResident(t.Context()); err == nil {
		t.Fatal("source revocation during publish was ignored")
	}
	if len(host.observations) != 1 || len(host.withdrawals) != 1 {
		t.Fatalf("continued fresh publication or failed to retract: published=%d withdrawn=%d", len(host.observations), len(host.withdrawals))
	}
}
func TestFailedLiveRenewalDoesNotFollowHeartbeats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, w, host := panelFixture(t)
		w.now = time.Now
		w.live()
		startPanel(t, h, w, nil)
		calls := 0
		host.publish = func(_ context.Context, o protocol.Observation) error {
			if o.Channel == ChannelLive {
				calls++
				return errPublication
			}
			return nil
		}
		done := make(chan struct{})
		go func() { w.runPublisher(); close(done) }()
		synctest.Wait()
		time.Sleep(15 * time.Second)
		w.live()
		synctest.Wait()
		for range 4 {
			time.Sleep(time.Second)
			w.live()
			synctest.Wait()
		}
		w.cancel()
		<-done
		if calls != 1 {
			t.Fatalf("failed live renewal retried %d times", calls)
		}
	})
}
