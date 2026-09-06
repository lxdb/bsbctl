package slack

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestResidentBatchDoesNotPublishPendingEpisodeAfterHandleCommit(t *testing.T) {
	h, w, host := panelFixture(t)
	w.reduce(callback("Ev2", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"2.000001","text":"second"}`))
	entered := make(chan protocol.Observation, 1)
	release := make(chan struct{})
	var first sync.Once
	host.publish = func(_ context.Context, o protocol.Observation) error {
		first.Do(func() { entered <- o; <-release })
		return nil
	}
	batch := make(chan error, 1)
	go func() { batch <- w.publishResident(t.Context()) }()
	blocked := <-entered
	// Whichever map entry is sent first, choose another captured activity that
	// is still pending in the batch. Navigation has separate public-path tests.
	var selected activity
	for _, a := range w.snapshot().Items {
		if a.ID != blocked.Key {
			selected = a
			break
		}
	}
	if selected.ID == "" {
		t.Fatal("fixture has no pending target")
	}
	w.panelMu.Lock()
	w.panel = &panelSession{token: "session-1", started: fixtureNow, level: panelDismiss, target: selected}
	w.panelMu.Unlock()
	completed := make(chan struct{})
	host.complete = func() error { close(completed); return nil }
	input := make(chan error, 1)
	go func() { _, err := press(h, w, protocol.ButtonStart); input <- err }()
	<-completed
	if !w.snapshot().Fresh || host.recordCount() != 1 {
		t.Fatal("handling was not committed while source stayed fresh")
	}
	close(release)
	<-batch
	if err := <-input; err != nil {
		t.Fatal(err)
	}
	host.pubMu.Lock()
	defer host.pubMu.Unlock()
	for _, o := range host.observations {
		if o.Channel == ChannelAttention && o.Key == selected.ID {
			t.Fatal("resident batch published the pending episode after its durable Handle commit")
		}
	}
}

func TestInflightObsoleteAttentionRetractsBeforeNextBatchPublication(t *testing.T) {
	for _, transition := range []string{"delete", "handle"} {
		t.Run(transition, func(t *testing.T) {
			h, w, host := panelFixture(t)
			w.reduce(callback("Ev2", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"2.000001","text":"second"}`))
			entered := make(chan protocol.Observation, 1)
			release := make(chan struct{})
			var first sync.Once
			var logMu sync.Mutex
			var log []string
			host.publish = func(_ context.Context, o protocol.Observation) error {
				if o.Channel == ChannelAttention {
					first.Do(func() { entered <- o; <-release })
				}
				logMu.Lock()
				log = append(log, "publish:"+o.Key)
				logMu.Unlock()
				return nil
			}
			host.withdraw = func(_ context.Context, r protocol.WithdrawRequest) error {
				logMu.Lock()
				log = append(log, "withdraw:"+r.Key)
				logMu.Unlock()
				return nil
			}
			batch := make(chan error, 1)
			go func() { batch <- w.publishResident(t.Context()) }()
			blocked := <-entered
			var selected activity
			for _, a := range w.snapshot().Items {
				if a.ID == blocked.Key {
					selected = a
					break
				}
			}
			var input chan error
			if transition == "delete" {
				w.reduce(callback("EvDelete", fmt.Sprintf(`{"type":"message","subtype":"message_deleted","channel":"D123","channel_type":"im","deleted_ts":%q,"event_ts":"3.000001"}`, selected.MessageTS)))
			} else {
				w.panelMu.Lock()
				w.panel = &panelSession{token: "session-1", started: fixtureNow, level: panelDismiss, target: selected}
				w.panelMu.Unlock()
				completed := make(chan struct{})
				host.complete = func() error { close(completed); return nil }
				input = make(chan error, 1)
				go func() { _, err := press(h, w, protocol.ButtonStart); input <- err }()
				<-completed
			}
			if !w.snapshot().Fresh {
				t.Fatal("test accidentally lost source freshness")
			}
			close(release)
			<-batch
			if input != nil {
				if err := <-input; err != nil {
					t.Fatal(err)
				}
			}
			logMu.Lock()
			defer logMu.Unlock()
			accepted := -1
			for i, event := range log {
				if event == "publish:"+blocked.Key {
					accepted = i
					break
				}
			}
			if accepted < 0 || accepted+1 >= len(log) || log[accepted+1] != "withdraw:"+blocked.Key {
				t.Fatalf("obsolete acceptance was not immediately retracted: %v", log)
			}
			w.publications.mu.Lock()
			defer w.publications.mu.Unlock()
			if old, ok := w.publications.current[ChannelAttention+"/"+blocked.Key]; ok && old.confirmed {
				t.Fatal("obsolete episode remained confirmed")
			}
		})
	}
}
