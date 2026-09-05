package device

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	busylib "github.com/lxdb/busylib-go"
)

func TestOutputSerializesAllDeviceOperations(t *testing.T) {
	backend := &recordingOutputBackend{}
	output := NewOutput(backend, OutputOptions{QueueSize: 8, CallTimeout: time.Second})
	defer func() { _ = output.Close(context.Background()) }()

	if err := output.Draw(context.Background(), busylib.DisplayElements{}); err != nil {
		t.Fatal(err)
	}
	if err := output.UploadFile(context.Background(), "app", "path", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := output.ReadTo(context.Background(), "path", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := output.Remove(context.Background(), "path"); err != nil {
		t.Fatal(err)
	}
	if err := output.Clear(context.Background(), "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := output.BusySnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := output.SetBusySnapshot(context.Background(), busylib.BusySnapshot{}); err != nil {
		t.Fatal(err)
	}
	if _, err := output.DeviceTime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := output.PlayAudio(context.Background(), busylib.NewStockAudio("bsbctl", "shared/tone.snd")); err != nil {
		t.Fatal(err)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	want := "draw,upload,read,remove,clear,busy-snapshot,busy-set,time-now,audio-play"
	if got := bytes.NewBufferString(""); func() bool {
		for i, op := range backend.ops {
			if i > 0 {
				got.WriteByte(',')
			}
			got.WriteString(op)
		}
		return got.String() != want
	}() {
		t.Fatalf("operations = %q, want %q", got.String(), want)
	}
}

func TestOutputAdmissionHonorsContextWhileDeviceCallIsBlocked(t *testing.T) {
	backend := &recordingOutputBackend{block: make(chan struct{}), started: make(chan string, 1)}
	output := NewOutput(backend, OutputOptions{QueueSize: 1, CallTimeout: time.Second})
	defer func() { close(backend.block); _ = output.Close(context.Background()) }()
	go func() { _ = output.Clear(context.Background(), "one") }()
	awaitOutputOperation(t, backend.started, "clear")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := output.Clear(ctx, "two"); err == nil {
		t.Fatal("blocked output call ignored caller deadline")
	}
}

func TestOutputReadToNeverRetainsCallerWriterAfterCancellation(t *testing.T) {
	backend := &lateReadBackend{started: make(chan struct{}), release: make(chan struct{})}
	output := NewOutput(backend, OutputOptions{QueueSize: 2, CallTimeout: time.Second, ReadLimit: 32})
	external := &lockedBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := output.ReadTo(ctx, "asset", external); done <- err }()
	<-backend.started
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadTo error = %v", err)
	}
	close(backend.release)
	if err := output.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := external.String(); got != "" {
		t.Fatalf("caller writer mutated after return: %q", got)
	}
}

func TestOutputCanceledSnapshotRepliesRemainOwnedByWorker(t *testing.T) {
	for _, method := range []string{"snapshot", "time"} {
		t.Run(method, func(t *testing.T) {
			backend := &canceledSnapshotBackend{started: make(chan struct{})}
			output := NewOutput(backend, OutputOptions{})
			t.Cleanup(func() { _ = output.Close(context.Background()) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if method == "snapshot" {
					value, err := output.BusySnapshot(ctx)
					if !reflect.DeepEqual(value, busylib.BusySnapshot{}) {
						done <- errors.New("canceled call exposed an uncommitted snapshot")
						return
					}
					done <- err
					return
				}
				value, err := output.DeviceTime(ctx)
				if !reflect.DeepEqual(value, busylib.TimestampInfo{}) {
					done <- errors.New("canceled call exposed an uncommitted timestamp")
					return
				}
				done <- err
			}()
			<-backend.started
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled %s: %v", method, err)
			}
		})
	}
}

type canceledSnapshotBackend struct {
	recordingOutputBackend
	started chan struct{}
}

func (b *canceledSnapshotBackend) BusySnapshot(ctx context.Context) (busylib.BusySnapshot, error) {
	close(b.started)
	<-ctx.Done()
	return busylib.BusySnapshot{Snapshot: busylib.BusySnapshotData{CardID: "uncommitted"}}, ctx.Err()
}

func (b *canceledSnapshotBackend) DeviceTime(ctx context.Context) (busylib.TimestampInfo, error) {
	close(b.started)
	<-ctx.Done()
	return busylib.TimestampInfo{Timestamp: "uncommitted"}, ctx.Err()
}

func TestOutputReadToCopiesExactOwnedReplyAndBoundsIt(t *testing.T) {
	backend := &payloadReadBackend{payload: []byte("verified")}
	output := NewOutput(backend, OutputOptions{ReadLimit: 16})
	var destination bytes.Buffer
	n, err := output.ReadTo(context.Background(), "asset", &destination)
	if err != nil || n != 8 || destination.String() != "verified" {
		t.Fatalf("read = %d/%q, %v", n, destination.String(), err)
	}
	backend.payload = []byte("this payload is over limit")
	destination.Reset()
	if _, err := output.ReadTo(context.Background(), "asset", &destination); !errors.Is(err, ErrOutputReadLimit) {
		t.Fatalf("over-limit error = %v", err)
	}
	if destination.Len() != 0 {
		t.Fatalf("over-limit destination = %q", destination.String())
	}
	if got := output.Status().LastErrorCode; got != "device_read_limit_exceeded" {
		t.Fatalf("status code = %q", got)
	}
	_ = output.Close(context.Background())
}

func TestOutputCloseHonorsContextWithBlockedAdmissionsAndCanRetry(t *testing.T) {
	backend := &recordingOutputBackend{block: make(chan struct{}), started: make(chan string, 1)}
	output := NewOutput(backend, OutputOptions{QueueSize: 1, CallTimeout: time.Second})
	first := make(chan error, 1)
	go func() { first <- output.Clear(context.Background(), "running") }()
	awaitOutputOperation(t, backend.started, "clear")
	queued := make(chan error, 1)
	go func() { queued <- output.Clear(context.Background(), "queued") }()
	awaitOutputCondition(t, func() bool { return len(output.queue) == 1 }, "queued output command")
	producer := make(chan error, 1)
	go func() { producer <- output.Clear(context.Background(), "blocked") }()
	awaitOutputCondition(t, func() bool {
		output.lifecycleMu.Lock()
		defer output.lifecycleMu.Unlock()
		return output.admissions == 1
	}, "blocked output admission")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := output.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("Close ignored context for %s", elapsed)
	}
	if err := <-producer; !errors.Is(err, ErrOutputClosed) {
		t.Fatalf("blocked producer = %v", err)
	}
	close(backend.block)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-queued; err != nil {
		t.Fatal(err)
	}
	if err := output.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOutputConcurrentCloseWaitersCannotLoseShutdownCoordinator(t *testing.T) {
	backend := &recordingOutputBackend{block: make(chan struct{}), started: make(chan string, 1)}
	output := NewOutput(backend, OutputOptions{QueueSize: 1, CallTimeout: time.Second})
	first := make(chan error, 1)
	queued := make(chan error, 1)
	go func() { first <- output.Clear(context.Background(), "running") }()
	awaitOutputOperation(t, backend.started, "clear")
	go func() { queued <- output.Clear(context.Background(), "queued") }()
	awaitOutputCondition(t, func() bool { return len(output.queue) == 1 }, "queued output command")
	shortCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	shortClose := make(chan error, 1)
	go func() { shortClose <- output.Close(shortCtx) }()
	backgroundClose := make(chan error, 1)
	go func() { backgroundClose <- output.Close(context.Background()) }()
	if err := <-shortClose; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short Close = %v", err)
	}
	close(backend.block)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-queued; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-backgroundClose:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("background Close lost the shutdown stop")
	}
}

type recordingOutputBackend struct {
	mu      sync.Mutex
	ops     []string
	block   chan struct{}
	started chan string
	drawErr error
}

type lateReadBackend struct {
	started chan struct{}
	release chan struct{}
}

func (b *lateReadBackend) Draw(context.Context, busylib.DisplayElements) error      { return nil }
func (b *lateReadBackend) Clear(context.Context, string) error                      { return nil }
func (b *lateReadBackend) UploadFile(context.Context, string, string, string) error { return nil }
func (b *lateReadBackend) ReadTo(_ context.Context, _ string, writer io.Writer) (int64, error) {
	close(b.started)
	<-b.release
	n, err := writer.Write([]byte("late"))
	return int64(n), err
}
func (b *lateReadBackend) Remove(context.Context, string) error { return nil }

type payloadReadBackend struct{ payload []byte }

func (*payloadReadBackend) Draw(context.Context, busylib.DisplayElements) error      { return nil }
func (*payloadReadBackend) Clear(context.Context, string) error                      { return nil }
func (*payloadReadBackend) UploadFile(context.Context, string, string, string) error { return nil }
func (b *payloadReadBackend) ReadTo(_ context.Context, _ string, writer io.Writer) (int64, error) {
	n, err := writer.Write(b.payload)
	return int64(n), err
}
func (*payloadReadBackend) Remove(context.Context, string) error { return nil }

type lockedBuffer struct {
	mu    sync.Mutex
	value bytes.Buffer
}

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.value.Write(value)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.value.String() }

func (b *recordingOutputBackend) add(op string) {
	if b.started != nil {
		b.started <- op
	}
	if b.block != nil {
		<-b.block
	}
	b.mu.Lock()
	b.ops = append(b.ops, op)
	b.mu.Unlock()
}

func awaitOutputOperation(t *testing.T, started <-chan string, want string) {
	t.Helper()
	select {
	case got := <-started:
		if got != want {
			t.Fatalf("started operation = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", want)
	}
}

func awaitOutputCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if !condition() {
		t.Fatalf("timed out waiting for %s", description)
	}
}
func (b *recordingOutputBackend) Draw(context.Context, busylib.DisplayElements) error {
	b.add("draw")
	return b.drawErr
}

func (b *recordingOutputBackend) BusySnapshot(context.Context) (busylib.BusySnapshot, error) {
	b.add("busy-snapshot")
	return busylib.BusySnapshot{}, nil
}

func (b *recordingOutputBackend) SetBusySnapshot(context.Context, busylib.BusySnapshot) error {
	b.add("busy-set")
	return nil
}

func (b *recordingOutputBackend) DeviceTime(context.Context) (busylib.TimestampInfo, error) {
	b.add("time-now")
	return busylib.TimestampInfo{}, nil
}
func (b *recordingOutputBackend) PlayAudio(context.Context, busylib.PlayAudio) error {
	b.add("audio-play")
	return nil
}
func (b *recordingOutputBackend) Clear(context.Context, string) error { b.add("clear"); return nil }
func (b *recordingOutputBackend) UploadFile(context.Context, string, string, string) error {
	b.add("upload")
	return nil
}
func (b *recordingOutputBackend) ReadTo(context.Context, string, io.Writer) (int64, error) {
	b.add("read")
	return 0, nil
}
func (b *recordingOutputBackend) Remove(context.Context, string) error { b.add("remove"); return nil }
