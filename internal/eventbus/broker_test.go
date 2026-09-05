package eventbus

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestSessionInputIsFIFOWithOneRequestInFlight(t *testing.T) {
	started := make(chan protocol.SessionInputRequest, 3)
	release := make(chan struct{}, 3)
	broker := New(func(_ context.Context, _ string, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
		started <- request
		<-release
		return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, nil
	}, nil)
	t.Cleanup(func() {
		close(release)
		broker.Close()
	})
	broker.Apply([]TargetSet{{PluginID: "calendar", InstanceIDs: []string{"calendar"}}})

	inputs := []protocol.SessionInput{
		{Encoder: &protocol.EncoderInput{Delta: 1}},
		{Button: &protocol.ButtonInput{Button: protocol.ButtonOK, Action: protocol.ButtonPress}},
		{Encoder: &protocol.EncoderInput{Delta: -1}},
	}
	for index := range inputs {
		if err := broker.PublishSessionInput(protocol.InstanceRef{ID: "calendar", Generation: 7}, "session", &inputs[index], time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	first := awaitRequest(t, started)
	if first.Sequence != 1 || !reflect.DeepEqual(first.Input, inputs[0]) {
		t.Fatalf("first input = %#v, want sequence 1 and %#v", first, inputs[0])
	}
	select {
	case request := <-started:
		t.Fatalf("sequence %d started while the first callback was in flight", request.Sequence)
	case <-time.After(20 * time.Millisecond):
	}
	for want := uint64(2); want <= 3; want++ {
		release <- struct{}{}
		got := awaitRequest(t, started)
		if got.Sequence != want || !reflect.DeepEqual(got.Input, inputs[want-1]) {
			t.Fatalf("input = %#v, want sequence %d and %#v", got, want, inputs[want-1])
		}
	}
	release <- struct{}{}
}

func TestSessionInputCancelIsReentrantFromDeliveryAndDropsQueuedInput(t *testing.T) {
	entered := make(chan protocol.SessionInputRequest, 2)
	releaseCancel := make(chan struct{})
	cancelReturned := make(chan struct{})
	allowDeliveryReturn := make(chan struct{})
	releaseCancelOnce := sync.OnceFunc(func() { close(releaseCancel) })
	releaseDeliveryOnce := sync.OnceFunc(func() { close(allowDeliveryReturn) })
	ref := protocol.InstanceRef{ID: "codex", Generation: 5}
	var broker *Broker
	broker = New(func(_ context.Context, pluginID string, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
		entered <- request
		if request.Sequence == 1 {
			<-releaseCancel
			broker.Cancel(pluginID, request.Instance, request.SessionToken)
			close(cancelReturned)
			<-allowDeliveryReturn
		}
		return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, nil
	}, nil)
	t.Cleanup(func() {
		releaseCancelOnce()
		releaseDeliveryOnce()
		broker.Close()
	})
	broker.Apply([]TargetSet{{PluginID: "codex", InstanceIDs: []string{"codex"}}})
	if err := broker.PublishSessionInput(ref, "session", encoderInput(1), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := awaitRequest(t, entered); got.Sequence != 1 {
		t.Fatalf("first sequence = %d, want 1", got.Sequence)
	}
	if err := broker.PublishSessionInput(ref, "session", encoderInput(2), time.Now()); err != nil {
		t.Fatal(err)
	}
	key := sessionKey{pluginID: "codex", instanceID: ref.ID, generation: ref.Generation, token: "session"}
	broker.mu.Lock()
	predecessor := broker.workers[key]
	broker.mu.Unlock()
	if predecessor == nil {
		t.Fatal("active session worker is missing")
	}
	releaseCancelOnce()
	select {
	case <-cancelReturned:
	case <-time.After(time.Second):
		t.Fatal("reentrant cancellation deadlocked inside delivery")
	}
	broker.mu.Lock()
	retained := broker.workers[key]
	broker.mu.Unlock()
	if retained != predecessor {
		t.Fatal("broker released ownership before the canceled delivery returned")
	}
	if err := broker.PublishSessionInput(ref, "session", encoderInput(3), time.Now()); !errors.Is(err, ErrSessionInvalidated) {
		t.Fatalf("same-key publish during retirement error = %v, want ErrSessionInvalidated", err)
	}
	status := broker.Status()
	if len(status) != 1 || status[0].QueueDepth != 0 {
		t.Fatalf("status after reentrant cancellation = %#v", status)
	}
	releaseDeliveryOnce()
	select {
	case <-predecessor.done:
	case <-time.After(time.Second):
		t.Fatal("canceled session worker did not retire")
	}
	broker.mu.Lock()
	retired := broker.workers[key]
	broker.mu.Unlock()
	if retired != nil {
		t.Fatal("retired session worker remained registered")
	}
	select {
	case request := <-entered:
		t.Fatalf("queued sequence %d delivered after reentrant cancellation", request.Sequence)
	default:
	}
	if err := broker.PublishSessionInput(ref, "session", encoderInput(4), time.Now()); err != nil {
		t.Fatalf("same-key publish after retirement: %v", err)
	}
	if got := awaitRequest(t, entered); got.Sequence != 3 {
		t.Fatalf("replacement worker sequence = %d, want 3", got.Sequence)
	}
}

func TestCloseJoinsReentrantlyCanceledWorker(t *testing.T) {
	entered := make(chan struct{})
	cancelReturned := make(chan struct{})
	allowDeliveryReturn := make(chan struct{})
	releaseDeliveryOnce := sync.OnceFunc(func() { close(allowDeliveryReturn) })
	ref := protocol.InstanceRef{ID: "codex", Generation: 5}
	var broker *Broker
	broker = New(func(_ context.Context, pluginID string, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
		close(entered)
		broker.Cancel(pluginID, request.Instance, request.SessionToken)
		close(cancelReturned)
		<-allowDeliveryReturn
		return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, nil
	}, nil)
	t.Cleanup(func() {
		releaseDeliveryOnce()
		broker.Close()
	})
	broker.Apply([]TargetSet{{PluginID: "codex", InstanceIDs: []string{"codex"}}})
	if err := broker.PublishSessionInput(ref, "session", encoderInput(1), time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("delivery did not start")
	}
	select {
	case <-cancelReturned:
	case <-time.After(time.Second):
		t.Fatal("reentrant cancellation deadlocked inside delivery")
	}
	key := sessionKey{pluginID: "codex", instanceID: ref.ID, generation: ref.Generation, token: "session"}
	broker.mu.Lock()
	retained := broker.workers[key]
	broker.mu.Unlock()
	if retained == nil {
		t.Fatal("broker released ownership before the canceled delivery returned")
	}

	closeStarted := make(chan struct{})
	closeReturned := make(chan struct{})
	go func() {
		close(closeStarted)
		broker.Close()
		close(closeReturned)
	}()
	<-closeStarted
	awaitBrokerClosed(t, broker)
	select {
	case <-closeReturned:
		t.Fatal("Close returned before the canceled delivery")
	case <-time.After(20 * time.Millisecond):
	}
	releaseDeliveryOnce()
	select {
	case <-closeReturned:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after the canceled delivery")
	}
}

func TestSessionInputCallbackErrorInvalidatesQueueWithoutRetry(t *testing.T) {
	delivered := make(chan uint64, 2)
	failed := make(chan Failure, 1)
	releaseFailure := make(chan struct{})
	releaseFailureOnce := sync.OnceFunc(func() { close(releaseFailure) })
	broker := New(func(_ context.Context, _ string, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
		delivered <- request.Sequence
		<-releaseFailure
		return protocol.SessionInputResult{}, errors.New("rejected")
	}, func(_ context.Context, failure Failure) {
		failed <- failure
	})
	t.Cleanup(func() {
		releaseFailureOnce()
		broker.Close()
	})
	broker.Apply([]TargetSet{{PluginID: "codex", InstanceIDs: []string{"codex"}}})

	ref := protocol.InstanceRef{ID: "codex", Generation: 3}
	if err := broker.PublishSessionInput(ref, "session", encoderInput(1), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := awaitSequence(t, delivered); got != 1 {
		t.Fatalf("delivered sequence = %d, want 1", got)
	}
	if err := broker.PublishSessionInput(ref, "session", encoderInput(2), time.Now()); err != nil {
		t.Fatal(err)
	}
	releaseFailureOnce()
	select {
	case failure := <-failed:
		if failure.Reason != FailureDelivery || failure.Request.Instance != ref || failure.Request.SessionToken != "session" {
			t.Fatalf("failure = %#v", failure)
		}
	case <-time.After(time.Second):
		t.Fatal("callback failure did not invalidate the session")
	}
	select {
	case sequence := <-delivered:
		t.Fatalf("queued sequence %d was retried after callback failure", sequence)
	default:
	}
	if err := broker.PublishSessionInput(ref, "session", encoderInput(3), time.Now()); err == nil {
		t.Fatal("invalidated session accepted new input")
	}
}

func TestSessionInputOverrunInvalidatesExactSessionAndClearsQueue(t *testing.T) {
	started := make(chan struct{})
	failed := make(chan Failure, 1)
	broker := New(func(ctx context.Context, _ string, _ protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
		close(started)
		<-ctx.Done()
		return protocol.SessionInputResult{}, ctx.Err()
	}, func(_ context.Context, failure Failure) {
		failed <- failure
	})
	t.Cleanup(broker.Close)
	broker.Apply([]TargetSet{{PluginID: "calendar", InstanceIDs: []string{"calendar"}}})
	ref := protocol.InstanceRef{ID: "calendar", Generation: 9}

	if err := broker.PublishSessionInput(ref, "session", encoderInput(1), time.Now()); err != nil {
		t.Fatal(err)
	}
	<-started
	for range InteractionQueueSize - 1 {
		if err := broker.PublishSessionInput(ref, "session", encoderInput(1), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := broker.PublishSessionInput(ref, "session", encoderInput(1), time.Now()); !errors.Is(err, ErrSessionInputOverrun) {
		t.Fatalf("overrun error = %v", err)
	}
	select {
	case failure := <-failed:
		if failure.Reason != FailureOverrun || failure.Request.Instance != ref {
			t.Fatalf("failure = %#v", failure)
		}
	case <-time.After(time.Second):
		t.Fatal("overrun did not invalidate the exact session")
	}
	status := broker.Status()
	if len(status) != 1 || status[0].QueueDepth != 0 || status[0].Overruns != 1 {
		t.Fatalf("status = %#v", status)
	}
}

func TestSessionInputCancelDropsPendingWork(t *testing.T) {
	started := make(chan uint64, 2)
	broker := New(func(ctx context.Context, _ string, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
		started <- request.Sequence
		<-ctx.Done()
		return protocol.SessionInputResult{}, ctx.Err()
	}, nil)
	t.Cleanup(broker.Close)
	broker.Apply([]TargetSet{{PluginID: "codex", InstanceIDs: []string{"codex"}}})
	ref := protocol.InstanceRef{ID: "codex", Generation: 5}
	if err := broker.PublishSessionInput(ref, "session", encoderInput(1), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := broker.PublishSessionInput(ref, "session", encoderInput(2), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := awaitSequence(t, started); got != 1 {
		t.Fatalf("first sequence = %d", got)
	}
	broker.Cancel("codex", ref, "session")
	select {
	case sequence := <-started:
		t.Fatalf("pending sequence %d delivered after cancellation", sequence)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestSessionInputRejectsUnconfiguredOrInexactTarget(t *testing.T) {
	broker := New(nil, nil)
	t.Cleanup(broker.Close)
	broker.Apply([]TargetSet{{PluginID: "calendar", InstanceIDs: []string{"calendar"}}})
	for _, test := range []struct {
		ref   protocol.InstanceRef
		token string
	}{
		{ref: protocol.InstanceRef{ID: "other", Generation: 1}, token: "session"},
		{ref: protocol.InstanceRef{ID: "calendar"}, token: "session"},
		{ref: protocol.InstanceRef{ID: "calendar", Generation: 1}},
	} {
		if err := broker.PublishSessionInput(test.ref, test.token, encoderInput(1), time.Now()); err == nil {
			t.Fatalf("invalid target accepted: %#v", test)
		}
	}
}

func TestSessionInputAndWaitReturnsStrictDispositionInFIFOOrder(t *testing.T) {
	started := make(chan uint64, 2)
	releaseFirst := make(chan struct{})
	releaseFirstOnce := sync.OnceFunc(func() { close(releaseFirst) })
	broker := New(func(_ context.Context, _ string, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
		started <- request.Sequence
		if request.Sequence == 1 {
			<-releaseFirst
		}
		return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, nil
	}, nil)
	t.Cleanup(func() {
		releaseFirstOnce()
		broker.Close()
	})
	broker.Apply([]TargetSet{{PluginID: "calendar", InstanceIDs: []string{"calendar"}}})
	ref := protocol.InstanceRef{ID: "calendar", Generation: 1}
	if err := broker.PublishSessionInput(ref, "session", encoderInput(1), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := awaitSequence(t, started); got != 1 {
		t.Fatalf("first sequence = %d", got)
	}
	type response struct {
		result protocol.SessionInputResult
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := broker.PublishSessionInputAndWait(t.Context(), ref, "session", &protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonBack, Action: protocol.ButtonPress}}, time.Now())
		done <- response{result: result, err: err}
	}()
	releaseFirstOnce()
	if got := awaitSequence(t, started); got != 2 {
		t.Fatalf("Back sequence = %d, want 2", got)
	}
	select {
	case got := <-done:
		if got.err != nil || got.result.Disposition != protocol.SessionInputConsumed {
			t.Fatalf("Back result = %#v, %v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("synchronous Back result was not delivered")
	}
}

func TestSessionInputCompleteAllowsAdmittedCallbackToReturn(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ref := protocol.InstanceRef{ID: "codex", Generation: 3}
	broker := New(func(_ context.Context, _ string, _ protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
		close(started)
		<-release
		return protocol.SessionInputResult{Disposition: protocol.SessionInputNotConsumed}, nil
	}, nil)
	t.Cleanup(broker.Close)
	broker.Apply([]TargetSet{{PluginID: "codex", InstanceIDs: []string{"codex"}}})
	done := make(chan deliveryResult, 1)
	go func() {
		result, err := broker.PublishSessionInputAndWait(t.Context(), ref, "session", &protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonBack, Action: protocol.ButtonPress}}, time.Now())
		done <- deliveryResult{result: result, err: err}
	}()
	<-started
	broker.Complete("codex", ref, "session")
	close(release)
	select {
	case got := <-done:
		if got.err != nil || got.result.Disposition != protocol.SessionInputNotConsumed {
			t.Fatalf("completed callback result = %#v, %v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("completed callback did not return")
	}
}

func TestSessionInputCancelImmediatelyRejectsWaitingCallback(t *testing.T) {
	started := make(chan struct{})
	ref := protocol.InstanceRef{ID: "codex", Generation: 3}
	broker := New(func(ctx context.Context, _ string, _ protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
		close(started)
		<-ctx.Done()
		return protocol.SessionInputResult{}, ctx.Err()
	}, nil)
	t.Cleanup(broker.Close)
	broker.Apply([]TargetSet{{PluginID: "codex", InstanceIDs: []string{"codex"}}})
	done := make(chan error, 1)
	go func() {
		_, err := broker.PublishSessionInputAndWait(t.Context(), ref, "session", &protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonBack, Action: protocol.ButtonPress}}, time.Now())
		done <- err
	}()
	<-started
	broker.Cancel("codex", ref, "session")
	select {
	case err := <-done:
		if !errors.Is(err, ErrSessionInvalidated) {
			t.Fatalf("canceled callback error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled callback remained blocked")
	}
}

func TestSessionInputRejectsMalformedDisposition(t *testing.T) {
	failed := make(chan Failure, 1)
	broker := New(func(context.Context, string, protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
		return protocol.SessionInputResult{}, nil
	}, func(_ context.Context, failure Failure) { failed <- failure })
	t.Cleanup(broker.Close)
	broker.Apply([]TargetSet{{PluginID: "calendar", InstanceIDs: []string{"calendar"}}})
	ref := protocol.InstanceRef{ID: "calendar", Generation: 1}
	_, err := broker.PublishSessionInputAndWait(t.Context(), ref, "session", &protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonBack, Action: protocol.ButtonPress}}, time.Now())
	if err == nil {
		t.Fatal("malformed disposition was accepted")
	}
	select {
	case failure := <-failed:
		if failure.Reason != FailureDelivery {
			t.Fatalf("failure reason = %q", failure.Reason)
		}
	case <-time.After(time.Second):
		t.Fatal("malformed disposition did not invalidate the session")
	}
}

func encoderInput(delta int32) *protocol.SessionInput {
	return &protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: delta}}
}

func awaitSequence(t *testing.T, values <-chan uint64) uint64 {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for sequence")
		return 0
	}
}

func awaitRequest(t *testing.T, values <-chan protocol.SessionInputRequest) protocol.SessionInputRequest {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for session input")
		return protocol.SessionInputRequest{}
	}
}

func awaitBrokerClosed(t *testing.T, broker *Broker) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		broker.mu.Lock()
		closed := broker.closed
		broker.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-deadline:
			t.Fatal("broker Close did not start")
		default:
			runtime.Gosched()
		}
	}
}
