package device

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	busyinput "github.com/lxdb/bsbctl/internal/input"
	"github.com/lxdb/busylib-go/proto/inputpb"
	publicstream "github.com/lxdb/busylib-go/stream"
)

type discardStreamHealthObserver struct{}

func (discardStreamHealthObserver) ObserveStatusStream(StreamHealth) {}

func newTestStatusSubscriber(t testing.TB, options StatusSubscriberOptions) *StatusSubscriber {
	t.Helper()
	if options.OnConnected == nil {
		options.OnConnected = func() {}
	}
	if options.Observer == nil {
		options.Observer = discardStreamHealthObserver{}
	}
	subscriber, err := NewStatusSubscriber(options)
	if err != nil {
		t.Fatal(err)
	}
	return subscriber
}

func TestStatusSubscriberConstructorRejectsEveryMissingRequiredDependency(t *testing.T) {
	factory := StatusStreamFactory(func() (publicstream.Stream, error) { return newFakeStream(), nil })
	submit := func(*inputpb.InputEvent) bool { return true }
	connected := func() {}
	observer := StreamHealthObserver(discardStreamHealthObserver{})
	tests := []struct {
		name    string
		options StatusSubscriberOptions
	}{
		{name: "factory", options: StatusSubscriberOptions{Submit: submit, OnConnected: connected, Observer: observer}},
		{name: "submit", options: StatusSubscriberOptions{Factory: factory, OnConnected: connected, Observer: observer}},
		{name: "connection handler", options: StatusSubscriberOptions{Factory: factory, Submit: submit, Observer: observer}},
		{name: "observer", options: StatusSubscriberOptions{Factory: factory, Submit: submit, OnConnected: connected}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewStatusSubscriber(test.options); err == nil {
				t.Fatal("NewStatusSubscriber accepted a missing required dependency")
			}
		})
	}
}

func TestStatusSubscriberDrainContinuesWhileInputHandlerIsBlocked(t *testing.T) {
	statusStream := newFakeStream()
	block := make(chan struct{})
	started := make(chan struct{})
	dispatcher := busyinput.NewDispatcherWithClearCapture(func(context.Context, *inputpb.InputEvent) error {
		close(started)
		<-block
		return nil
	}, func() func(context.Context) { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- dispatcher.Run(ctx) }()
	observer := &recordingStreamObserver{updates: make(chan StreamHealth, 16)}
	subscriber := newTestStatusSubscriber(t, StatusSubscriberOptions{
		Factory: func() (publicstream.Stream, error) { return statusStream, nil }, Submit: dispatcher.Submit, Observer: observer,
	})
	subscriberDone := make(chan error, 1)
	go func() { subscriberDone <- subscriber.Run(ctx) }()
	statusStream.messages <- publicstream.Message{Updates: []publicstream.Update{publicstream.InputUpdate{Value: encoderInput(1)}}}
	<-started
	statusStream.statuses <- publicstream.Status{Lifecycle: publicstream.LifecycleConnected}
	awaitStreamHealth(t, observer.updates, func(update StreamHealth) bool {
		return update.Phase == StreamStatusTransition && update.Lifecycle == publicstream.LifecycleConnected
	})
	close(block)
	cancel()
	if err := <-subscriberDone; err != nil {
		t.Fatal(err)
	}
	if err := <-dispatchDone; err != nil {
		t.Fatal(err)
	}
}

func encoderInput(delta int32) *inputpb.InputEvent {
	return &inputpb.InputEvent{Event: &inputpb.InputEvent_EncoderEvent{EncoderEvent: &inputpb.EncoderEvent{Delta: delta}}}
}

func TestStatusSubscriberPreservesOrderedPhysicalInput(t *testing.T) {
	t.Parallel()
	statusStream := newFakeStream()
	want := []*inputpb.InputEvent{
		{Event: &inputpb.InputEvent_SwitchEvent{SwitchEvent: &inputpb.SwitchEvent{Position: inputpb.SwitchPosition_APPS}}},
		encoderInput(1),
		encoderInput(-1),
		{Event: &inputpb.InputEvent_ButtonEvent{ButtonEvent: &inputpb.ButtonEvent{Button: inputpb.Button_OK, Action: inputpb.ButtonAction_PRESS}}},
		{Event: &inputpb.InputEvent_ButtonEvent{ButtonEvent: &inputpb.ButtonEvent{Button: inputpb.Button_OK, Action: inputpb.ButtonAction_RELEASE}}},
		{Event: &inputpb.InputEvent_ButtonEvent{ButtonEvent: &inputpb.ButtonEvent{Button: inputpb.Button_BACK, Action: inputpb.ButtonAction_PRESS}}},
		{Event: &inputpb.InputEvent_ButtonEvent{ButtonEvent: &inputpb.ButtonEvent{Button: inputpb.Button_BACK, Action: inputpb.ButtonAction_RELEASE}}},
		{Event: &inputpb.InputEvent_ButtonEvent{ButtonEvent: &inputpb.ButtonEvent{Button: inputpb.Button_START, Action: inputpb.ButtonAction_PRESS}}},
		{Event: &inputpb.InputEvent_ButtonEvent{ButtonEvent: &inputpb.ButtonEvent{Button: inputpb.Button_START, Action: inputpb.ButtonAction_RELEASE}}},
	}
	got := make(chan *inputpb.InputEvent, len(want))
	subscriber := newTestStatusSubscriber(t, StatusSubscriberOptions{
		Factory: func() (publicstream.Stream, error) { return statusStream, nil },
		Submit: func(event *inputpb.InputEvent) bool {
			got <- event
			return true
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- subscriber.Run(ctx) }()

	updates := []publicstream.Update{publicstream.DeviceNameUpdate{}, publicstream.InputUpdate{}}
	for _, event := range want {
		updates = append(updates, publicstream.InputUpdate{Value: event})
	}
	statusStream.messages <- publicstream.Message{Updates: updates}
	for index, expected := range want {
		select {
		case actual := <-got:
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("input %d = %#v, want %#v", index, actual, expected)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for input %d", index)
		}
	}
	select {
	case extra := <-got:
		t.Fatalf("unexpected input = %#v", extra)
	default:
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStatusSubscriberDrainsInputAndRecreatesTerminalStream(t *testing.T) {
	t.Parallel()
	first := newFakeStream()
	second := newFakeStream()
	createdSecond := make(chan struct{})
	var mu sync.Mutex
	created := 0
	factory := func() (publicstream.Stream, error) {
		mu.Lock()
		defer mu.Unlock()
		created++
		if created == 1 {
			return first, nil
		}
		close(createdSecond)
		return second, nil
	}
	handled := make(chan *inputpb.InputEvent, 1)
	connections := make(chan struct{}, 2)
	subscriber := newTestStatusSubscriber(t, StatusSubscriberOptions{
		Factory: factory,
		Submit: func(event *inputpb.InputEvent) bool {
			handled <- event
			return true
		},
		OnConnected: func() { connections <- struct{}{} },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- subscriber.Run(ctx) }()
	select {
	case <-connections:
	case <-time.After(time.Second):
		t.Fatal("first successful stream did not trigger the connection handler")
	}

	event := &inputpb.InputEvent{Event: &inputpb.InputEvent_EncoderEvent{EncoderEvent: &inputpb.EncoderEvent{Delta: 2}}}
	first.messages <- publicstream.Message{Updates: []publicstream.Update{publicstream.InputUpdate{Value: event}}}
	select {
	case got := <-handled:
		if got.GetEncoderEvent().GetDelta() != 2 {
			t.Fatalf("event = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("input was not drained")
	}
	first.finish(errors.New("terminal"))
	select {
	case <-createdSecond:
	case <-time.After(time.Second):
		t.Fatal("terminal stream was not recreated")
	}
	select {
	case <-connections:
	case <-time.After(time.Second):
		t.Fatal("second successful stream did not trigger the connection handler")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not stop")
	}
}

func TestStatusSubscriberReportsSanitizedStreamLifecycle(t *testing.T) {
	statusStream := newFakeStream()
	observer := &recordingStreamObserver{updates: make(chan StreamHealth, 16)}
	subscriber := newTestStatusSubscriber(t, StatusSubscriberOptions{
		Factory: func() (publicstream.Stream, error) { return statusStream, nil },
		Submit:  func(*inputpb.InputEvent) bool { return true }, Backoff: time.Hour, Observer: observer,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- subscriber.Run(ctx) }()

	awaitStreamHealth(t, observer.updates, func(update StreamHealth) bool { return update.Phase == StreamStarting })
	connectedAt := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	statusStream.statuses <- publicstream.Status{
		Lifecycle: publicstream.LifecycleConnected, Access: publicstream.AccessAccepted, Attempt: 2, ConnectedAt: connectedAt,
	}
	connected := awaitStreamHealth(t, observer.updates, func(update StreamHealth) bool {
		return update.Phase == StreamStatusTransition && update.Lifecycle == publicstream.LifecycleConnected
	})
	if connected.Access != publicstream.AccessAccepted || connected.Attempt != 2 || !connected.ConnectedAt.Equal(connectedAt) {
		t.Fatalf("connected health = %#v", connected)
	}
	statusStream.statuses <- publicstream.Status{
		Lifecycle: publicstream.LifecycleReconnecting, Attempt: 3, LastError: errors.New("token=secret transport failure"),
	}
	reconnecting := awaitStreamHealth(t, observer.updates, func(update StreamHealth) bool {
		return update.Phase == StreamStatusTransition && update.Lifecycle == publicstream.LifecycleReconnecting
	})
	if reconnecting.ErrorCode != streamStatusErrorCode {
		t.Fatalf("reconnecting health = %#v", reconnecting)
	}
	statusStream.finish(errors.New("token=secret terminal failure"))
	terminal := awaitStreamHealth(t, observer.updates, func(update StreamHealth) bool { return update.Phase == StreamTerminal })
	if terminal.ErrorCode != streamTerminalErrorCode {
		t.Fatalf("terminal health = %#v", terminal)
	}
	backoff := awaitStreamHealth(t, observer.updates, func(update StreamHealth) bool { return update.Phase == StreamBackoff })
	if backoff.ErrorCode != streamTerminalErrorCode || backoff.RetryAt.IsZero() {
		t.Fatalf("backoff health = %#v", backoff)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type fakeStream struct {
	messages chan publicstream.Message
	statuses chan publicstream.Status
	done     chan struct{}
	waitErr  error
	finishMu sync.Once
	stopped  bool
}

type recordingStreamObserver struct{ updates chan StreamHealth }

func (o *recordingStreamObserver) ObserveStatusStream(update StreamHealth) { o.updates <- update }

func awaitStreamHealth(t *testing.T, updates <-chan StreamHealth, match func(StreamHealth) bool) StreamHealth {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case update := <-updates:
			if match(update) {
				return update
			}
		case <-deadline:
			t.Fatal("timed out waiting for stream health update")
		}
	}
}

func newFakeStream() *fakeStream {
	return &fakeStream{
		messages: make(chan publicstream.Message, 2), statuses: make(chan publicstream.Status, 2), done: make(chan struct{}),
	}
}

func (s *fakeStream) finish(err error) {
	s.finishMu.Do(func() {
		s.waitErr = err
		close(s.messages)
		close(s.statuses)
		close(s.done)
	})
}

func (*fakeStream) Start(context.Context) error             { return nil }
func (s *fakeStream) Stop() error                           { s.stopped = true; return nil }
func (*fakeStream) RequestSnapshot(context.Context) error   { return nil }
func (s *fakeStream) Messages() <-chan publicstream.Message { return s.messages }
func (s *fakeStream) Statuses() <-chan publicstream.Status  { return s.statuses }
func (*fakeStream) Status() publicstream.Status             { return publicstream.Status{} }
func (s *fakeStream) Wait() error                           { <-s.done; return s.waitErr }
