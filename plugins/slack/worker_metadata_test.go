package slack

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestWorkerResolvesChannelNamesWithoutBlockingReduction(t *testing.T) {
	lookupStarted := make(chan struct{})
	releaseLookup := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseLookup) })
	defer release()
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/apps.connections.open":
			_, _ = io.WriteString(w, `{"ok":true,"url":"wss://wss-primary.slack.com/socket?ticket=canary"}`)
		case "/api/conversations.info":
			if r.Header.Get("Authorization") != "Bearer user-canary" {
				t.Errorf("metadata authorization = %q", r.Header.Get("Authorization"))
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("channel") == "C999" {
				close(lookupStarted)
				<-releaseLookup
				_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"C999","name":"engineering-platform"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"G999","name":"private-platform"}}`)
		default:
			t.Errorf("unexpected Slack API path %q", r.URL.Path)
		}
	})
	cfg, err := decodeConfig([]byte(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","all_channels":true}`))
	if err != nil {
		t.Fatal(err)
	}
	w := newWorker(protocol.Instance{ID: "slack", Generation: 1, Secrets: map[string]string{"app_token": "app-canary", "user_token": "user-canary"}}, cfg, &checkpointHost{}, client, blockedDial, time.Now)
	go w.run()
	t.Cleanup(func() {
		w.cancel()
		<-w.done
	})

	w.queue <- callback("Ev1", `{"type":"message","channel":"C999","channel_type":"channel","user":"U456","ts":"1.000001","text":"first"}`)
	select {
	case <-lookupStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("channel metadata lookup did not start")
	}
	w.queue <- callback("Ev2", `{"type":"message","channel":"G999","channel_type":"group","user":"U456","ts":"2.000001","text":"second"}`)
	waitSnapshot(t, w, func(s workerSnapshot) bool { return len(s.Items) == 2 })
	release()
	waitSnapshot(t, w, func(s workerSnapshot) bool {
		for _, item := range s.Items {
			if item.ChannelID == "C999" {
				return item.Alias == "engineering-platform"
			}
		}
		return false
	})
}

func TestWorkerBoundsChannelMetadataToRetainedActivity(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	cfg := config{configured: true, workspaceID: "T123", userID: "U123", allChannels: true, channels: map[string]string{}}
	w := &worker{
		cfg: cfg, now: func() time.Time { return now }, state: newState(cfg, cfg.userID),
		nameQueue: make(chan string, channelNameQueue), nameRetry: map[string]time.Time{},
	}
	for index := 0; index < 2*maxRetained; index++ {
		channelID := fmt.Sprintf("C%08d", index)
		clear(w.state.aggregates)
		w.state.aggregates["current"] = activity{ChannelID: channelID}
		if !w.state.setChannelName(channelID, fmt.Sprintf("channel-%d", index)) {
			t.Fatalf("channel %q name was not applied", channelID)
		}
		w.queueChannelNameLocked(channelID, "channel")
		<-w.nameQueue
	}
	if len(w.state.channelNames) > maxRetained || len(w.nameRetry) > maxRetained {
		t.Fatalf("channel metadata grew beyond retention: names=%d retries=%d", len(w.state.channelNames), len(w.nameRetry))
	}
}
