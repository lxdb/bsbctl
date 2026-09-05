package input

import (
	"context"
	"sync"

	"github.com/lxdb/busylib-go/proto/inputpb"
	"google.golang.org/protobuf/proto"
)

const InputQueueCapacity = 64

type DispatcherStatus struct {
	QueueDepth    int    `json:"queue_depth"`
	Accepted      uint64 `json:"accepted"`
	Handled       uint64 `json:"handled"`
	Discarded     uint64 `json:"discarded"`
	Overruns      uint64 `json:"overruns"`
	LastErrorCode string `json:"last_error_code,omitempty"`
}

// Dispatcher isolates the device stream from all input-triggered core work.
// Submit is non-blocking; one Run call owns ordered handler execution.
type Dispatcher struct {
	queue        chan queuedInput
	cancel       chan struct{}
	handle       func(context.Context, *inputpb.InputEvent) error
	captureClear func() func(context.Context)

	mu           sync.Mutex
	context      uint64
	pendingClear func(context.Context)
	status       DispatcherStatus
}

type queuedInput struct {
	context uint64
	event   *inputpb.InputEvent
}

// NewDispatcherWithClearCapture snapshots the exact foreground cleanup target
// at overflow time so delayed recovery cannot clear a later session.
func NewDispatcherWithClearCapture(handle func(context.Context, *inputpb.InputEvent) error, captureClear func() func(context.Context)) *Dispatcher {
	return &Dispatcher{
		queue: make(chan queuedInput, InputQueueCapacity), cancel: make(chan struct{}, 1),
		handle: handle, captureClear: captureClear,
	}
}

func (d *Dispatcher) Submit(event *inputpb.InputEvent) bool {
	if event == nil {
		return true
	}
	value, _ := proto.Clone(event).(*inputpb.InputEvent)
	d.mu.Lock()
	select {
	case d.queue <- queuedInput{context: d.context, event: value}:
		d.status.Accepted++
		d.status.QueueDepth = len(d.queue)
		d.mu.Unlock()
		return true
	default:
		d.status.Overruns++
		d.status.LastErrorCode = "input_overrun"
		if d.captureClear != nil {
			d.pendingClear = d.captureClear()
		}
		hasClear := d.pendingClear != nil
		d.advanceContextLocked()
		d.mu.Unlock()
		if hasClear {
			d.signalInvalidation()
		}
		return false
	}
}

// InvalidateContext atomically retires every queued event accepted before the
// foreground transition. Inputs submitted after it returns use a fresh epoch.
func (d *Dispatcher) InvalidateContext() {
	d.mu.Lock()
	d.advanceContextLocked()
	d.mu.Unlock()
}

func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-d.cancel:
			d.clearPendingContext(ctx)
			continue
		default:
		}
		select {
		case <-ctx.Done():
			return nil
		case <-d.cancel:
			d.clearPendingContext(ctx)
		case value := <-d.queue:
			d.clearPendingContext(ctx)
			d.mu.Lock()
			stale := value.context != d.context
			if stale {
				d.status.Discarded++
				d.status.QueueDepth = len(d.queue)
			}
			d.mu.Unlock()
			if stale {
				continue
			}
			var err error
			if d.handle != nil {
				err = d.handle(ctx, value.event)
			}
			d.mu.Lock()
			d.status.Handled++
			d.status.QueueDepth = len(d.queue)
			if err != nil {
				d.status.LastErrorCode = "input_handler_failed"
			}
			d.mu.Unlock()
		}
	}
}

func (d *Dispatcher) advanceContextLocked() {
	d.context++
	discarded := uint64(0)
	for {
		select {
		case <-d.queue:
			discarded++
		default:
			d.status.Discarded += discarded
			d.status.QueueDepth = len(d.queue)
			return
		}
	}
}

func (d *Dispatcher) signalInvalidation() {
	select {
	case d.cancel <- struct{}{}:
	default:
	}
}

func (d *Dispatcher) clearPendingContext(ctx context.Context) {
	d.mu.Lock()
	clear := d.pendingClear
	d.pendingClear = nil
	d.status.QueueDepth = len(d.queue)
	d.mu.Unlock()
	if clear != nil {
		clear(ctx)
	}
}

func (d *Dispatcher) Status() DispatcherStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	value := d.status
	value.QueueDepth = len(d.queue)
	return value
}
