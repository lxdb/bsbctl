package device

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	busylib "github.com/lxdb/busylib-go"
)

var (
	ErrOutputClosed    = errors.New("device output is closed")
	ErrOutputReadLimit = errors.New("device readback exceeds limit")
)

const DefaultOutputReadLimit int64 = 64 << 20

type OutputBackend interface {
	Draw(context.Context, busylib.DisplayElements) error
	Clear(context.Context, string) error
	UploadFile(context.Context, string, string, string) error
	ReadTo(context.Context, string, io.Writer) (int64, error)
	Remove(context.Context, string) error
}

type BusyTimerBackend interface {
	BusySnapshot(context.Context) (busylib.BusySnapshot, error)
	SetBusySnapshot(context.Context, busylib.BusySnapshot) error
	DeviceTime(context.Context) (busylib.TimestampInfo, error)
}

type AudioBackend interface {
	PlayAudio(context.Context, busylib.PlayAudio) error
}

type OutputOptions struct {
	QueueSize   int
	CallTimeout time.Duration
	ReadLimit   int64
}

type OutputPhase string

const (
	OutputReady  OutputPhase = "ready"
	OutputBusy   OutputPhase = "busy"
	OutputClosed OutputPhase = "closed"
)

type OutputStatus struct {
	Phase         OutputPhase `json:"phase"`
	QueueDepth    int         `json:"queue_depth"`
	LastErrorCode string      `json:"last_error_code,omitempty"`
}

type outputResult struct {
	n   int64
	err error
}

type outputCommand struct {
	ctx       context.Context
	run       func(context.Context) (int64, error)
	done      chan outputResult
	operation outputOperation
	stop      bool
}

type outputOperation uint8

const (
	outputOperationGeneric outputOperation = iota
	outputOperationDraw
)

// Output is the sole serialized owner of physical display and asset I/O.
// Callers wait for their command only; a slow device never occupies their
// subsystem's actor or lock while another command is admitted.
type Output struct {
	backend   OutputBackend
	timeout   time.Duration
	readLimit int64
	queue     chan outputCommand
	done      chan struct{}

	lifecycleMu      sync.Mutex
	closing          bool
	shutdown         chan struct{}
	admissions       int
	admissionsDone   chan struct{}
	admissionsClosed bool
	statusMu         sync.RWMutex
	status           OutputStatus
}

func NewOutput(backend OutputBackend, options OutputOptions) *Output {
	if options.QueueSize < 1 {
		options.QueueSize = 64
	}
	if options.CallTimeout <= 0 {
		options.CallTimeout = 3 * time.Second
	}
	if options.ReadLimit <= 0 {
		options.ReadLimit = DefaultOutputReadLimit
	}
	output := &Output{
		backend: backend, timeout: options.CallTimeout, readLimit: options.ReadLimit,
		queue: make(chan outputCommand, options.QueueSize), done: make(chan struct{}),
		shutdown: make(chan struct{}), admissionsDone: make(chan struct{}),
		status: OutputStatus{Phase: OutputReady},
	}
	go output.run()
	return output
}

func (o *Output) run() {
	defer close(o.done)
	for command := range o.queue {
		if command.stop {
			o.setStatus(OutputStatus{Phase: OutputClosed})
			return
		}
		if err := command.ctx.Err(); err != nil {
			command.done <- outputResult{err: err}
			continue
		}
		o.setStatus(OutputStatus{Phase: OutputBusy})
		callCtx, cancel := context.WithTimeout(command.ctx, o.timeout)
		n, err := command.run(callCtx)
		cancel()
		status := OutputStatus{Phase: OutputReady}
		if err != nil {
			status.LastErrorCode = outputErrorCode(command.operation, err)
		}
		o.setStatus(status)
		command.done <- outputResult{n: n, err: err}
	}
}

func (o *Output) execute(ctx context.Context, run func(context.Context) (int64, error)) outputResult {
	return o.executeOperation(ctx, outputOperationGeneric, run)
}

func (o *Output) executeOperation(ctx context.Context, operation outputOperation, run func(context.Context) (int64, error)) outputResult {
	if ctx == nil {
		ctx = context.Background()
	}
	command := outputCommand{ctx: ctx, run: run, done: make(chan outputResult, 1), operation: operation}
	if !o.beginAdmission() {
		return outputResult{err: ErrOutputClosed}
	}
	select {
	case o.queue <- command:
		o.finishAdmission()
	case <-o.shutdown:
		o.finishAdmission()
		return outputResult{err: ErrOutputClosed}
	case <-ctx.Done():
		o.finishAdmission()
		return outputResult{err: ctx.Err()}
	}
	select {
	case result := <-command.done:
		return result
	case <-ctx.Done():
		return outputResult{err: ctx.Err()}
	}
}

func outputErrorCode(operation outputOperation, err error) string {
	if errors.Is(err, ErrOutputReadLimit) {
		return "device_read_limit_exceeded"
	}
	if operation == outputOperationDraw {
		if apiErr, ok := errors.AsType[*busylib.APIError](err); ok && apiErr.StatusCode == http.StatusConflict {
			return ""
		}
	}
	return "device_operation_failed"
}

func (o *Output) beginAdmission() bool {
	o.lifecycleMu.Lock()
	defer o.lifecycleMu.Unlock()
	if o.closing {
		return false
	}
	o.admissions++
	return true
}

func (o *Output) finishAdmission() {
	o.lifecycleMu.Lock()
	o.admissions--
	if o.closing && o.admissions == 0 && !o.admissionsClosed {
		close(o.admissionsDone)
		o.admissionsClosed = true
	}
	o.lifecycleMu.Unlock()
}

func (o *Output) Draw(ctx context.Context, value busylib.DisplayElements) error {
	return o.executeOperation(ctx, outputOperationDraw, func(callCtx context.Context) (int64, error) {
		return 0, o.backend.Draw(callCtx, value)
	}).err
}
func (o *Output) Clear(ctx context.Context, application string) error {
	return o.execute(ctx, func(callCtx context.Context) (int64, error) { return 0, o.backend.Clear(callCtx, application) }).err
}
func (o *Output) UploadFile(ctx context.Context, application, path, localPath string) error {
	return o.execute(ctx, func(callCtx context.Context) (int64, error) {
		return 0, o.backend.UploadFile(callCtx, application, path, localPath)
	}).err
}
func (o *Output) ReadTo(ctx context.Context, path string, writer io.Writer) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	owned := &boundedReadBuffer{limit: o.readLimit}
	result := o.execute(ctx, func(callCtx context.Context) (int64, error) {
		n, err := o.backend.ReadTo(callCtx, path, owned)
		if owned.exceeded {
			return n, ErrOutputReadLimit
		}
		return n, err
	})
	if result.err != nil {
		return 0, result.err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if result.n != int64(owned.buffer.Len()) {
		return 0, errors.New("device readback count mismatch")
	}
	n, err := writer.Write(owned.buffer.Bytes())
	if err == nil && n != owned.buffer.Len() {
		err = io.ErrShortWrite
	}
	return int64(n), err
}
func (o *Output) Remove(ctx context.Context, path string) error {
	return o.execute(ctx, func(callCtx context.Context) (int64, error) { return 0, o.backend.Remove(callCtx, path) }).err
}

func (o *Output) BusySnapshot(ctx context.Context) (busylib.BusySnapshot, error) {
	backend, ok := o.backend.(BusyTimerBackend)
	if !ok {
		return busylib.BusySnapshot{}, errors.New("device output does not support BUSY timers")
	}
	var value busylib.BusySnapshot
	result := o.execute(ctx, func(callCtx context.Context) (int64, error) {
		var err error
		value, err = backend.BusySnapshot(callCtx)
		return 0, err
	})
	if result.err != nil {
		return busylib.BusySnapshot{}, result.err
	}
	return value, nil
}

func (o *Output) SetBusySnapshot(ctx context.Context, value busylib.BusySnapshot) error {
	backend, ok := o.backend.(BusyTimerBackend)
	if !ok {
		return errors.New("device output does not support BUSY timers")
	}
	return o.execute(ctx, func(callCtx context.Context) (int64, error) {
		return 0, backend.SetBusySnapshot(callCtx, value)
	}).err
}

func (o *Output) DeviceTime(ctx context.Context) (busylib.TimestampInfo, error) {
	backend, ok := o.backend.(BusyTimerBackend)
	if !ok {
		return busylib.TimestampInfo{}, errors.New("device output does not support device time")
	}
	var value busylib.TimestampInfo
	result := o.execute(ctx, func(callCtx context.Context) (int64, error) {
		var err error
		value, err = backend.DeviceTime(callCtx)
		return 0, err
	})
	if result.err != nil {
		return busylib.TimestampInfo{}, result.err
	}
	return value, nil
}

func (o *Output) PlayAudio(ctx context.Context, value busylib.PlayAudio) error {
	backend, ok := o.backend.(AudioBackend)
	if !ok {
		return errors.New("device output does not support audio")
	}
	return o.execute(ctx, func(callCtx context.Context) (int64, error) {
		return 0, backend.PlayAudio(callCtx, value)
	}).err
}

type boundedReadBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	exceeded bool
}

func (b *boundedReadBuffer) Write(value []byte) (int, error) {
	remaining := b.limit - int64(b.buffer.Len())
	if remaining <= 0 {
		b.exceeded = true
		return 0, ErrOutputReadLimit
	}
	if int64(len(value)) > remaining {
		_, _ = b.buffer.Write(value[:int(remaining)])
		b.exceeded = true
		return int(remaining), ErrOutputReadLimit
	}
	return b.buffer.Write(value)
}

func (o *Output) Status() OutputStatus {
	o.statusMu.RLock()
	status := o.status
	o.statusMu.RUnlock()
	status.QueueDepth = len(o.queue)
	return status
}

func (o *Output) setStatus(status OutputStatus) {
	o.statusMu.Lock()
	o.status = status
	o.statusMu.Unlock()
}

// Close rejects new work, drains already admitted commands, and joins the
// actor. Its context bounds admission and joining but does not reorder work.
func (o *Output) Close(ctx context.Context) error {
	o.lifecycleMu.Lock()
	startCoordinator := false
	if !o.closing {
		o.closing = true
		close(o.shutdown)
		if o.admissions == 0 && !o.admissionsClosed {
			close(o.admissionsDone)
			o.admissionsClosed = true
		}
		startCoordinator = true
	}
	o.lifecycleMu.Unlock()
	if startCoordinator {
		go o.coordinateShutdown()
	}
	select {
	case <-o.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// coordinateShutdown owns the one process-lifetime stop command. Its progress
// is independent of any individual Close caller's deadline.
func (o *Output) coordinateShutdown() {
	<-o.admissionsDone
	o.queue <- outputCommand{stop: true}
	<-o.done
}
