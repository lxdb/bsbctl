package slack

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestRetirementJoinsAdmittedOpenAndPublisher(t *testing.T) {
	for _, boundary := range []string{"open", "publish"} {
		t.Run(boundary, func(t *testing.T) {
			h, w, host := panelFixture(t)
			w.client = newSlackClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })})
			w.dial = blockedDial
			entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			block := func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				close(canceled)
				<-release
				return ctx.Err()
			}
			inputDone := make(chan struct{})
			if boundary == "open" {
				h.open = func(ctx context.Context, _ string) error { return block(ctx) }
				startPanel(t, h, w, nil)
				_, _ = press(h, w, protocol.ButtonOK)
			} else {
				host.publish = func(ctx context.Context, _ protocol.Observation) error { return block(ctx) }
			}
			go w.run()
			if boundary == "open" {
				go func() { _, _ = press(h, w, protocol.ButtonStart); close(inputDone) }()
			}
			<-entered
			retired := make(chan error, 1)
			go func() { retired <- h.ReplaceInstances(t.Context(), nil) }()
			<-canceled
			select {
			case err := <-retired:
				t.Fatalf("returned before %s joined: %v", boundary, err)
			default:
			}
			close(release)
			if err := <-retired; err != nil {
				t.Fatal(err)
			}
			if boundary == "open" {
				<-inputDone
				if host.completes != 1 {
					t.Fatal("admitted canceled session not completed")
				}
			}
			select {
			case <-w.done:
			default:
				t.Fatal("worker still running")
			}
		})
	}
}

func TestCanceledReplacementFinishesCommittedRetirement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, w, host := panelFixture(t)
		w.client = newSlackClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})})
		w.dial = blockedDial
		entered, canceled, releaseWorker := make(chan struct{}), make(chan struct{}), make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseWorker) })
		defer release()
		host.publish = func(ctx context.Context, _ protocol.Observation) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-releaseWorker
			return ctx.Err()
		}
		go w.run()
		<-entered
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() { result <- h.ReplaceInstances(ctx, nil) }()
		<-canceled
		cancel()
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("replacement reported rollback after retirement began: %v", err)
		default:
		}
		release()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if _, err := h.lookup(w.instance.Ref()); err == nil {
			t.Fatal("committed retirement left canceled worker installed")
		}
	})
}
func TestExecutionRechecksFreshnessAfterGrantAndCompletesAfterTimeout(t *testing.T) {
	t.Run("expires-during-grant", func(t *testing.T) {
		h, w, host := panelFixture(t)
		opens := 0
		h.open = func(context.Context, string) error { opens++; return nil }
		host.grant = func(context.Context) error { w.disconnected("auth_required"); return nil }
		startPanel(t, h, w, nil)
		_, _ = press(h, w, protocol.ButtonOK)
		if _, err := press(h, w, protocol.ButtonStart); err == nil {
			t.Fatal("expired grant opened")
		}
		if opens != 0 || host.completes != 1 {
			t.Fatalf("opens=%d completes=%d", opens, host.completes)
		}
	})
	t.Run("five-second-budget", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h, w, host := panelFixture(t)
			w.now = time.Now
			w.live()
			h.open = func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }
			startPanel(t, h, w, nil)
			_, _ = press(h, w, protocol.ButtonOK)
			at := time.Now()
			if _, err := press(h, w, protocol.ButtonStart); err == nil {
				t.Fatal("timeout succeeded")
			}
			if time.Since(at) != 5*time.Second || host.completes != 1 {
				t.Fatalf("duration=%s completes=%d", time.Since(at), host.completes)
			}
		})
	})
}
func TestConfirmedHandlingCommitsEvenIfSourceExpiresDuringSave(t *testing.T) {
	h, w, host := panelFixture(t)
	host.save = func(context.Context, protocol.CheckpointRequest) error {
		w.disconnected("auth_required")
		return nil
	}
	startPanel(t, h, w, nil)
	_, _ = press(h, w, protocol.ButtonOK)
	_, _ = h.HandleSessionInput(t.Context(), protocol.SessionInputRequest{Instance: w.instance.Ref(), SessionToken: "session-1", Sequence: testInputSequence.Add(1), OccurredAt: w.now().UTC(), Input: protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: 1}}})
	if _, err := press(h, w, protocol.ButtonStart); err != nil {
		t.Fatal(err)
	}
	if !w.snapshot().Items[0].Handled || host.recordCount() != 1 || host.completes != 1 {
		t.Fatal("confirmed durable effect treated as failed")
	}
	if err := w.publishResident(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, o := range host.observations {
		if o.Channel == ChannelAttention {
			t.Fatal("stale handled activity published")
		}
	}
}
func TestCompletionFailureCannotRepeatOpenOrCrossSessions(t *testing.T) {
	h, w, host := panelFixture(t)
	opens := 0
	h.open = func(context.Context, string) error { opens++; return nil }
	host.complete = func() error { return errors.New("completion failed") }
	startPanel(t, h, w, nil)
	_, _ = press(h, w, protocol.ButtonOK)
	for _, r := range []protocol.SessionInputRequest{
		{Instance: protocol.InstanceRef{ID: "slack", Generation: 2}, SessionToken: "session-1"},
		{Instance: protocol.InstanceRef{ID: "other", Generation: 1}, SessionToken: "session-1"},
		{Instance: w.instance.Ref(), SessionToken: "other-session"},
	} {
		r.Input = protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonStart, Action: protocol.ButtonPress}}
		_, _ = h.HandleSessionInput(t.Context(), r)
	}
	if opens != 0 {
		t.Fatal("cross-session effect")
	}
	if _, err := press(h, w, protocol.ButtonStart); err == nil {
		t.Fatal("completion failure hidden")
	}
	_, _ = press(h, w, protocol.ButtonStart)
	if opens != 1 || host.completes != 1 {
		t.Fatal("effect repeated after completion failure")
	}
}
