// Package eventbus provides bounded, session-bound input delivery.
package eventbus

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const InteractionQueueSize = 64

var (
	ErrClosed              = errors.New("session input queue is closed")
	ErrTargetUnavailable   = errors.New("session input target is not configured")
	ErrSessionInvalidated  = errors.New("session input session is invalidated")
	ErrSessionInputOverrun = errors.New("session input queue overrun")
)

type DeliveryFunc func(context.Context, string, protocol.SessionInputRequest) (protocol.SessionInputResult, error)
type FailureFunc func(context.Context, Failure)

type FailureReason string

const (
	FailureDelivery FailureReason = "delivery_failed"
	FailureOverrun  FailureReason = "overrun"
)

type Failure struct {
	PluginID string
	Request  protocol.SessionInputRequest
	Reason   FailureReason
	Err      error
}

type TargetSet struct {
	PluginID    string
	InstanceIDs []string
}

type Status struct {
	PluginID      string `json:"plugin_id"`
	QueueDepth    int    `json:"queue_depth"`
	LastSequence  uint64 `json:"last_sequence,omitempty"`
	Overruns      uint64 `json:"overruns"`
	LastErrorCode string `json:"last_error_code,omitempty"`
}

type sessionKey struct {
	pluginID   string
	instanceID string
	generation uint64
	token      string
}

type Broker struct {
	mu      sync.Mutex
	deliver DeliveryFunc
	failure FailureFunc
	next    uint64
	targets map[string]string
	workers map[sessionKey]*worker
	invalid map[sessionKey]struct{}
	stats   map[string]*Status
	closed  bool
}

type worker struct {
	mu         sync.Mutex
	key        sessionKey
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	notify     chan struct{}
	queue      []queuedDelivery
	inFlight   bool
	completing bool
	stopped    bool
	deliver    DeliveryFunc
	complete   func(*worker, protocol.SessionInputRequest, error)
}

type queuedDelivery struct {
	request  protocol.SessionInputRequest
	response chan deliveryResult
}

type deliveryResult struct {
	result protocol.SessionInputResult
	err    error
}

func New(deliver DeliveryFunc, failure FailureFunc) *Broker {
	return &Broker{
		deliver: deliver,
		failure: failure,
		targets: make(map[string]string),
		workers: make(map[sessionKey]*worker),
		invalid: make(map[sessionKey]struct{}),
		stats:   make(map[string]*Status),
	}
}

// Apply replaces the instance-to-plugin routing set. Sessions removed by the
// replacement are canceled without replaying or retaining their inputs.
func (b *Broker) Apply(values []TargetSet) {
	next := make(map[string]string)
	plugins := make(map[string]struct{})
	for _, value := range values {
		plugins[value.PluginID] = struct{}{}
		for _, instanceID := range value.InstanceIDs {
			next[instanceID] = value.PluginID
		}
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	retired := make([]*worker, 0)
	for key, current := range b.workers {
		if next[key.instanceID] == key.pluginID {
			continue
		}
		delete(b.invalid, key)
		current.stopNoWait()
		retired = append(retired, current)
	}
	for key := range b.invalid {
		if next[key.instanceID] != key.pluginID {
			delete(b.invalid, key)
		}
	}
	for pluginID := range b.stats {
		if _, ok := plugins[pluginID]; !ok {
			delete(b.stats, pluginID)
		}
	}
	for pluginID := range plugins {
		if b.stats[pluginID] == nil {
			b.stats[pluginID] = &Status{PluginID: pluginID}
		}
	}
	b.targets = next
	b.mu.Unlock()

	for _, current := range retired {
		<-current.done
	}
}

func (b *Broker) PublishSessionInput(target protocol.InstanceRef, sessionToken string, input *protocol.SessionInput, occurredAt time.Time) error {
	return b.publishSessionInput(target, sessionToken, input, occurredAt, nil)
}

// PublishSessionInputAndWait preserves FIFO ordering while waiting for the
// exact callback disposition. It is used only for Back press first refusal.
func (b *Broker) PublishSessionInputAndWait(
	ctx context.Context,
	target protocol.InstanceRef,
	sessionToken string,
	input *protocol.SessionInput,
	occurredAt time.Time,
) (protocol.SessionInputResult, error) {
	response := make(chan deliveryResult, 1)
	if err := b.publishSessionInput(target, sessionToken, input, occurredAt, response); err != nil {
		return protocol.SessionInputResult{}, err
	}
	select {
	case delivery := <-response:
		return delivery.result, delivery.err
	case <-ctx.Done():
		return protocol.SessionInputResult{}, ctx.Err()
	}
}

func (b *Broker) publishSessionInput(
	target protocol.InstanceRef,
	sessionToken string,
	input *protocol.SessionInput,
	occurredAt time.Time,
	response chan deliveryResult,
) error {
	request := protocol.SessionInputRequest{
		OccurredAt:   occurredAt.UTC(),
		Instance:     target,
		SessionToken: sessionToken,
	}
	if input != nil {
		request.Input = cloneInput(*input)
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrClosed
	}
	pluginID := b.targets[target.ID]
	if pluginID == "" {
		b.mu.Unlock()
		return ErrTargetUnavailable
	}
	key := sessionKey{pluginID: pluginID, instanceID: target.ID, generation: target.Generation, token: sessionToken}
	if _, invalid := b.invalid[key]; invalid {
		b.mu.Unlock()
		return ErrSessionInvalidated
	}
	current := b.workers[key]
	if current != nil && current.isUnavailable() {
		b.mu.Unlock()
		return ErrSessionInvalidated
	}
	b.next++
	request.Sequence = b.next
	if err := request.Validate(); err != nil {
		b.next--
		b.mu.Unlock()
		return err
	}
	if current == nil {
		current = newWorker(key, b.deliver, b.complete)
		b.workers[key] = current
		go b.runWorker(current)
	}
	status := b.stats[pluginID]
	if status == nil {
		status = &Status{PluginID: pluginID}
		b.stats[pluginID] = status
	}
	status.LastSequence = request.Sequence
	b.mu.Unlock()

	if err := current.enqueue(queuedDelivery{request: request, response: response}); err != nil {
		if errors.Is(err, ErrSessionInputOverrun) {
			b.fail(current, request, FailureOverrun, err)
		}
		return err
	}
	return nil
}

// Cancel discards all queued input for one exact session and permits a future
// session with the same identity to start cleanly.
func (b *Broker) Cancel(pluginID string, target protocol.InstanceRef, sessionToken string) {
	key := sessionKey{pluginID: pluginID, instanceID: target.ID, generation: target.Generation, token: sessionToken}
	b.mu.Lock()
	current := b.workers[key]
	if current != nil {
		current.stopNoWait()
	}
	delete(b.invalid, key)
	b.mu.Unlock()
}

// Complete retires one exact session after allowing an already-admitted
// callback to return its disposition. Pending callbacks are discarded.
func (b *Broker) Complete(pluginID string, target protocol.InstanceRef, sessionToken string) {
	key := sessionKey{pluginID: pluginID, instanceID: target.ID, generation: target.Generation, token: sessionToken}
	b.mu.Lock()
	current := b.workers[key]
	if current != nil {
		current.completeNoWait()
	}
	delete(b.invalid, key)
	b.mu.Unlock()
}

func (b *Broker) Status() []Status {
	b.mu.Lock()
	result := make([]Status, 0, len(b.stats))
	for _, status := range b.stats {
		result = append(result, *status)
	}
	workers := make([]*worker, 0, len(b.workers))
	for _, current := range b.workers {
		workers = append(workers, current)
	}
	b.mu.Unlock()

	depth := make(map[string]int)
	for _, current := range workers {
		depth[current.key.pluginID] += current.depth()
	}
	for i := range result {
		result[i].QueueDepth = depth[result[i].PluginID]
	}
	slices.SortFunc(result, func(left, right Status) int { return cmp.Compare(left.PluginID, right.PluginID) })
	return result
}

func (b *Broker) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	workers := make([]*worker, 0, len(b.workers))
	for _, current := range b.workers {
		current.stopNoWait()
		workers = append(workers, current)
	}
	b.invalid = make(map[sessionKey]struct{})
	b.mu.Unlock()

	for _, current := range workers {
		<-current.done
	}
}

func (b *Broker) runWorker(current *worker) {
	current.run()
	b.mu.Lock()
	if b.workers[current.key] == current {
		delete(b.workers, current.key)
	}
	b.mu.Unlock()
	close(current.done)
}

func (b *Broker) complete(current *worker, request protocol.SessionInputRequest, err error) {
	if err != nil {
		b.fail(current, request, FailureDelivery, err)
	}
}

func (b *Broker) fail(current *worker, request protocol.SessionInputRequest, reason FailureReason, err error) {
	b.mu.Lock()
	if b.workers[current.key] != current || current.isUnavailable() {
		b.mu.Unlock()
		return
	}
	b.invalid[current.key] = struct{}{}
	status := b.stats[current.key.pluginID]
	if status == nil {
		status = &Status{PluginID: current.key.pluginID}
		b.stats[current.key.pluginID] = status
	}
	status.LastErrorCode = string(reason)
	if reason == FailureOverrun {
		status.Overruns++
	}
	failure := b.failure
	current.stopNoWait()
	b.mu.Unlock()

	if failure != nil {
		failure(context.Background(), Failure{PluginID: current.key.pluginID, Request: cloneRequest(request), Reason: reason, Err: err})
	}
}

func newWorker(key sessionKey, deliver DeliveryFunc, complete func(*worker, protocol.SessionInputRequest, error)) *worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &worker{
		key: key, ctx: ctx, cancel: cancel, done: make(chan struct{}), notify: make(chan struct{}, 1),
		deliver: deliver, complete: complete,
	}
}

func (w *worker) enqueue(delivery queuedDelivery) error {
	w.mu.Lock()
	if w.stopped || w.completing {
		w.mu.Unlock()
		return ErrSessionInvalidated
	}
	if len(w.queue) >= InteractionQueueSize {
		w.mu.Unlock()
		return ErrSessionInputOverrun
	}
	delivery.request = cloneRequest(delivery.request)
	w.queue = append(w.queue, delivery)
	w.mu.Unlock()
	w.wake()
	return nil
}

func (w *worker) run() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-w.notify:
		}
		for {
			delivery, ok := w.begin()
			if !ok {
				break
			}
			var result protocol.SessionInputResult
			var err error
			if w.deliver != nil {
				result, err = w.deliver(w.ctx, w.key.pluginID, cloneRequest(delivery.request))
			}
			if err == nil {
				err = result.Validate()
			}
			accepted, stop := w.finish(delivery.request.Sequence, err)
			if !accepted {
				return
			}
			if delivery.response != nil {
				delivery.response <- deliveryResult{result: result, err: err}
			}
			if err != nil {
				w.complete(w, delivery.request, err)
				return
			}
			if stop {
				return
			}
		}
	}
}

func (w *worker) begin() (queuedDelivery, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.inFlight || len(w.queue) == 0 {
		return queuedDelivery{}, false
	}
	w.inFlight = true
	delivery := w.queue[0]
	delivery.request = cloneRequest(delivery.request)
	return delivery, true
}

func (w *worker) finish(sequence uint64, err error) (bool, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return false, true
	}
	if len(w.queue) == 0 || w.queue[0].request.Sequence != sequence {
		return false, true
	}
	w.queue = w.queue[1:]
	w.inFlight = false
	if w.completing {
		w.stopped = true
		w.queue = nil
		w.cancel()
		return true, true
	}
	return true, err != nil
}

func (w *worker) depth() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.queue)
}

func (w *worker) isUnavailable() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopped || w.completing
}

func (w *worker) completeNoWait() {
	w.mu.Lock()
	if w.stopped || w.completing {
		w.mu.Unlock()
		return
	}
	w.completing = true
	if w.inFlight {
		discarded := slices.Clone(w.queue[1:])
		w.queue = w.queue[:1]
		w.mu.Unlock()
		rejectDeliveries(discarded)
		return
	}
	discarded := slices.Clone(w.queue)
	w.stopped = true
	w.queue = nil
	w.cancel()
	w.mu.Unlock()
	rejectDeliveries(discarded)
}

func (w *worker) wake() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

func (w *worker) stopNoWait() {
	w.mu.Lock()
	var discarded []queuedDelivery
	if !w.stopped {
		discarded = append(discarded, w.queue...)
		w.stopped = true
		w.queue = nil
		w.cancel()
	}
	w.mu.Unlock()
	rejectDeliveries(discarded)
}

func rejectDeliveries(deliveries []queuedDelivery) {
	for _, delivery := range deliveries {
		if delivery.response == nil {
			continue
		}
		delivery.response <- deliveryResult{err: ErrSessionInvalidated}
	}
}

func cloneInput(input protocol.SessionInput) protocol.SessionInput {
	cloned := input
	if input.Button != nil {
		button := *input.Button
		cloned.Button = &button
	}
	if input.Encoder != nil {
		encoder := *input.Encoder
		cloned.Encoder = &encoder
	}
	return cloned
}

func cloneRequest(request protocol.SessionInputRequest) protocol.SessionInputRequest {
	cloned := request
	cloned.Input = cloneInput(request.Input)
	return cloned
}
