package slack

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestSessionCallbacksRespectDeadlineDuringResidentBatch(t *testing.T) {
	for _, name := range []string{"start", "live-input", "end", "admitted-start"} {
		t.Run(name, func(t *testing.T) {
			h, w, host := panelFixture(t)
			for i := 2; i <= 12; i++ {
				w.reduce(callback(fmt.Sprint(i), fmt.Sprintf(`{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"%d.000001","text":"fixture"}`, i)))
			}
			if name != "start" {
				startPanel(t, h, w, nil)
			}
			opens := 0
			h.open = func(context.Context, string) error { opens++; return nil }
			if name == "admitted-start" {
				_, _ = press(h, w, protocol.ButtonOK)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var first sync.Once
			successful := 0
			host.publish = func(ctx context.Context, o protocol.Observation) error {
				if o.Channel == ChannelLive {
					return nil
				}
				first.Do(func() { close(entered); <-release })
				select {
				case <-time.After(time.Millisecond):
					successful++
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			batch := make(chan error, 1)
			go func() { batch <- w.publishResident(t.Context()) }()
			<-entered
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
			defer cancel()
			callbackDone := make(chan error, 1)
			go func() {
				var err error
				switch name {
				case "start":
					err = h.StartSession(ctx, protocol.SessionStartRequest{Instance: w.instance.Ref(), Action: "open", SessionToken: "session-1", Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher}})
				case "live-input":
					_, err = h.HandleSessionInput(ctx, protocol.SessionInputRequest{Instance: w.instance.Ref(), SessionToken: "session-1", Sequence: testInputSequence.Add(1), OccurredAt: w.now().UTC(), Input: protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: 1}}})
				case "end":
					err = h.EndSession(ctx, protocol.SessionEndRequest{Instance: w.instance.Ref(), SessionToken: "session-1"})
				default:
					_, err = h.HandleSessionInput(ctx, protocol.SessionInputRequest{Instance: w.instance.Ref(), SessionToken: "session-1", Sequence: testInputSequence.Add(1), OccurredAt: w.now().UTC(), Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonStart, Action: protocol.ButtonPress}}})
				}
				callbackDone <- err
			}()
			<-ctx.Done()
			// The resident callback still has a valid independent two-second budget.
			// Cancellation must release the foreground caller before that host returns.
			var callbackErr error
			blocked := false
			select {
			case callbackErr = <-callbackDone:
			case <-time.After(200 * time.Millisecond):
				blocked = true
			}
			close(release)
			if batchErr := <-batch; batchErr != nil && name != "admitted-start" {
				t.Fatal(batchErr)
			}
			if blocked {
				callbackErr = <-callbackDone
			}
			if successful < 8 && name != "admitted-start" {
				t.Fatal("fixture did not finish its successful resident batch")
			}
			if blocked {
				t.Fatal("expired callback waited for the resident batch instead of its own deadline")
			}
			if !errors.Is(callbackErr, context.DeadlineExceeded) {
				t.Fatalf("cancellation result: %v", callbackErr)
			}
			if name == "admitted-start" {
				if opens != 1 || host.completes != 1 {
					t.Fatalf("effect/completion changed under cleanup contention: %d/%d", opens, host.completes)
				}
			}
		})
	}
}

func TestBackgroundLiveCopyCannotOverwriteNavigationOrEnd(t *testing.T) {
	for _, transition := range []string{"navigate", "end"} {
		t.Run(transition, func(t *testing.T) {
			h, w, host := panelFixture(t)
			startPanel(t, h, w, nil)
			w.now = func() time.Time { return fixtureNow.Add(15 * time.Second) }
			entered, release := make(chan struct{}), make(chan struct{})
			var first sync.Once
			var logMu sync.Mutex
			var log []string
			host.publish = func(_ context.Context, o protocol.Observation) error {
				if o.Channel == ChannelLive {
					first.Do(func() { close(entered); <-release })
					label := ""
					for _, e := range o.Scene.Elements {
						if e.ID == "front-label" {
							label = "detail"
						}
					}
					for _, e := range o.Scene.Elements {
						if e.ID == "back-position" {
							label = "list"
						}
					}
					logMu.Lock()
					log = append(log, "publish:"+label)
					logMu.Unlock()
				}
				return nil
			}
			host.withdraw = func(_ context.Context, r protocol.WithdrawRequest) error {
				if r.Channel == ChannelLive {
					logMu.Lock()
					log = append(log, "withdraw")
					logMu.Unlock()
				}
				return nil
			}
			background := make(chan error, 1)
			go func() { background <- w.publishCurrentPanel(t.Context()) }()
			<-entered
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
			defer cancel()
			var err error
			if transition == "navigate" {
				_, err = h.HandleSessionInput(ctx, protocol.SessionInputRequest{Instance: w.instance.Ref(), SessionToken: "session-1", Sequence: testInputSequence.Add(1), OccurredAt: w.now().UTC(), Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonOK, Action: protocol.ButtonPress}}})
			} else {
				err = h.EndSession(ctx, protocol.SessionEndRequest{Instance: w.instance.Ref(), SessionToken: "session-1"})
			}
			close(release)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("foreground blocked or ignored cancellation: %v", err)
			}
			if err := <-background; err == nil {
				t.Fatal("obsolete live copy was confirmed")
			}
			if err := w.publishCurrentPanel(t.Context()); err != nil {
				t.Fatal(err)
			}
			logMu.Lock()
			defer logMu.Unlock()
			want := []string{"publish:list", "withdraw"}
			if transition == "navigate" {
				want = append(want, "publish:detail")
			}
			if !slices.Equal(log, want) {
				t.Fatalf("live ordering: %v, want %v", log, want)
			}
			if transition == "end" {
				w.publications.mu.Lock()
				defer w.publications.mu.Unlock()
				if _, exists := w.publications.current[ChannelLive+"/"+w.observationKey("live")]; exists {
					t.Fatal("ended panel retained a live observation")
				}
			}
		})
	}
}

func TestResidentPublicationYieldsToForegroundBetweenHostCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, w, host := panelFixture(t)
		w.now = time.Now
		w.live()
		for i := 2; i <= 12; i++ {
			w.reduce(callback(fmt.Sprint(i), fmt.Sprintf(`{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"%d.000001","text":"fixture"}`, i)))
		}
		entered := make(chan struct{})
		var first sync.Once
		host.publish = func(ctx context.Context, o protocol.Observation) error {
			if o.Channel == ChannelLive {
				return nil
			}
			first.Do(func() { close(entered) })
			select {
			case <-time.After(time.Second):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		batch := make(chan error, 1)
		go func() { batch <- w.publishResident(t.Context()) }()
		<-entered
		ctx, cancel := context.WithTimeout(t.Context(), 7*time.Second)
		defer cancel()
		at := time.Now()
		err := h.StartSession(ctx, protocol.SessionStartRequest{Instance: w.instance.Ref(), Action: "open", SessionToken: "session-1", Trigger: &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher}})
		elapsed := time.Since(at)
		if err != nil || elapsed >= 7*time.Second {
			t.Fatalf("foreground did not receive a host slot between resident calls: %s, %v", elapsed, err)
		}
		select {
		case <-batch:
			t.Fatal("foreground had to wait for the whole resident batch")
		default:
		}
		if err := <-batch; err != nil {
			t.Fatal(err)
		}
	})
}
