package pluginhost

import (
	"context"
	"encoding/json"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"sync"
	"testing"
	"time"
)

func startCount(mu *sync.Mutex, starts *int) int {
	mu.Lock()
	defer mu.Unlock()
	return *starts
}

func residentSpec(id string, generation uint64) Spec {
	return Spec{ID: id, Version: "1", Executable: "/plugin", ProtocolVersion: protocol.Version, ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident}, Instances: []Instance{{ID: "one", Generation: generation, Config: json.RawMessage(`{}`)}}}
}

func interactiveSpec(id string, generation uint64) Spec {
	return Spec{ID: id, Version: "1", Executable: "/plugin", ProtocolVersion: protocol.Version, ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive}, Instances: []Instance{{ID: "one", Generation: generation, Config: json.RawMessage(`{}`)}}}
}

func residentInteractiveSpec(id string, generation uint64) Spec {
	spec := residentSpec(id, generation)
	spec.ExecutionModes = append(spec.ExecutionModes, protocol.ExecutionModeInteractive)
	return spec
}

type pingResponse struct {
	healthy bool
	err     error
}

type supervisorChild struct {
	mu               sync.Mutex
	done             chan error
	stopOnce         sync.Once
	instances        []Instance
	pings            []pingResponse
	pingIndex        int
	replaceErr       error
	invokeErr        error
	endSessionErr    error
	endSessionStart  chan struct{}
	endSessionBlock  <-chan struct{}
	eventErr         error
	stopErr          error
	invocations      int
	invokeRequests   []InvokeRequest
	endSessions      []EndSessionRequest
	events           int
	stopped          bool
	invokeStart      chan struct{}
	invokeBlock      <-chan struct{}
	invokeDeadline   chan time.Time
	eventStart       chan struct{}
	eventBlock       <-chan struct{}
	sessionInputHook func(context.Context, protocol.SessionInputRequest) error
	delayDone        bool
}

func newSupervisorChild(spec Spec) *supervisorChild {
	return &supervisorChild{done: make(chan error, 1), instances: cloneInstances(spec.Instances)}
}

func (c *supervisorChild) Invoke(ctx context.Context, request InvokeRequest) error {
	c.mu.Lock()
	c.invocations++
	c.invokeRequests = append(c.invokeRequests, request)
	started, blocked, deadlineChannel, err := c.invokeStart, c.invokeBlock, c.invokeDeadline, c.invokeErr
	c.mu.Unlock()
	if deadlineChannel != nil {
		deadline, _ := ctx.Deadline()
		deadlineChannel <- deadline
	}
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if blocked != nil {
		<-blocked
	}
	return err
}

func (c *supervisorChild) Operation(context.Context, protocol.OperationRequest) (protocol.OperationResult, error) {
	return protocol.OperationResult{Payload: json.RawMessage(`{}`)}, nil
}

func (c *supervisorChild) EndSession(_ context.Context, request EndSessionRequest) error {
	c.mu.Lock()
	c.endSessions = append(c.endSessions, request)
	started, blocked, err := c.endSessionStart, c.endSessionBlock, c.endSessionErr
	c.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if blocked != nil {
		<-blocked
	}
	return err
}

func (c *supervisorChild) ReplaceInstances(_ context.Context, instances []Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.replaceErr != nil {
		return c.replaceErr
	}
	c.instances = cloneInstances(instances)
	return nil
}

func (c *supervisorChild) SessionInput(ctx context.Context, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	c.mu.Lock()
	c.events++
	started, blocked, hook, err := c.eventStart, c.eventBlock, c.sessionInputHook, c.eventErr
	c.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if blocked != nil {
		<-blocked
	}
	if hook != nil {
		return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, hook(ctx, request)
	}
	return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, err
}

func (c *supervisorChild) Ping(context.Context) (protocol.HealthResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	response := pingResponse{healthy: true}
	if c.pingIndex < len(c.pings) {
		response = c.pings[c.pingIndex]
	}
	c.pingIndex++
	return protocol.HealthResult{Healthy: response.healthy}, response.err
}

func (c *supervisorChild) Done() <-chan error { return c.done }

func (c *supervisorChild) Stop(context.Context) error {
	c.mu.Lock()
	c.stopped = true
	err := c.stopErr
	delayDone := c.delayDone
	c.mu.Unlock()
	if !delayDone {
		c.stopOnce.Do(func() { close(c.done) })
	}
	return err
}

func (c *supervisorChild) crash(err error) {
	c.stopOnce.Do(func() {
		c.done <- err
		close(c.done)
	})
}

func (c *supervisorChild) isStopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

func (c *supervisorChild) eventCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events
}

func (c *supervisorChild) invocationCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.invocations
}

func (c *supervisorChild) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.instances[0].Generation
}

func (c *supervisorChild) pingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pingIndex
}

func (c *supervisorChild) setEndSessionError(err error) {
	c.mu.Lock()
	c.endSessionErr = err
	c.mu.Unlock()
}

func (c *supervisorChild) endSessionCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.endSessions)
}

func (c *supervisorChild) finish() {
	c.stopOnce.Do(func() { close(c.done) })
}

func fixedStarter(child Child) StartFunc {
	return func(context.Context, string, Spec, Callbacks) (Child, error) { return child, nil }
}

type supervisorClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*supervisorTimer
}

type supervisorTimer struct {
	clock   *supervisorClock
	when    time.Time
	channel chan time.Time
	active  bool
}

func newSupervisorClock(now time.Time) *supervisorClock { return &supervisorClock{now: now} }

func (c *supervisorClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *supervisorClock) NewTimer(delay time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &supervisorTimer{clock: c, when: c.now.Add(delay), channel: make(chan time.Time, 1), active: true}
	c.timers = append(c.timers, timer)
	return timer
}

func (c *supervisorClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	c.fireDueLocked()
	c.mu.Unlock()
}

func (c *supervisorClock) Jump(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	c.mu.Unlock()
}

func (c *supervisorClock) FireNext(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		var selected *supervisorTimer
		for _, timer := range c.timers {
			if timer.active && (selected == nil || timer.when.Before(selected.when)) {
				selected = timer
			}
		}
		if selected != nil {
			c.now = selected.when
			c.fireDueLocked()
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("no active timer")
		}
		time.Sleep(time.Millisecond)
	}
}

func (c *supervisorClock) FireAll() {
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		var latest time.Time
		for _, timer := range c.timers {
			if timer.active && timer.when.After(latest) {
				latest = timer.when
			}
		}
		if !latest.IsZero() {
			c.now = latest
			c.fireDueLocked()
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (c *supervisorClock) fireDueLocked() {
	for _, timer := range c.timers {
		if timer.active && !timer.when.After(c.now) {
			timer.active = false
			timer.channel <- c.now
		}
	}
}

func (t *supervisorTimer) C() <-chan time.Time { return t.channel }

func (t *supervisorTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	t.active = false
	return wasActive
}

func awaitSignal(t *testing.T, channel <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func awaitStartedChild(t *testing.T, children <-chan *supervisorChild) *supervisorChild {
	t.Helper()
	select {
	case child := <-children:
		return child
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for started child")
		return nil
	}
}

func awaitError(t *testing.T, channel <-chan error) error {
	t.Helper()
	select {
	case err := <-channel:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error result")
		return nil
	}
}

func awaitStatus(t *testing.T, manager *Manager, id string, predicate func(PluginStatus) bool) PluginStatus {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status := statusByID(manager.Status(), id)
		if predicate(status) {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	status := statusByID(manager.Status(), id)
	t.Fatalf("timed out waiting for status, last = %#v", status)
	return PluginStatus{}
}

func statusByID(statuses []PluginStatus, id string) PluginStatus {
	for _, status := range statuses {
		if status.ID == id {
			return status
		}
	}
	return PluginStatus{}
}

func pluginSessionLifecycle(status PluginStatus) (string, time.Time) {
	return status.SessionLifecycleErrorCode, status.SessionLifecycleErrorAt
}

func awaitGeneration(t *testing.T, values <-chan uint64) uint64 {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for generation withdrawal")
		return 0
	}
}

func closeManager(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func testSessionInput(sequence uint64, token SessionToken) protocol.SessionInputRequest {
	return protocol.SessionInputRequest{
		Sequence: sequence, OccurredAt: time.Now().UTC(), Instance: protocol.InstanceRef{ID: "one", Generation: 1},
		SessionToken: string(token), Input: protocol.SessionInput{Encoder: &protocol.EncoderInput{Delta: 1}},
	}
}

func awaitSupervisorQueueDepth(t *testing.T, manager *Manager, id string, depth int) {
	t.Helper()
	manager.mu.Lock()
	current := manager.supervisors[id]
	manager.mu.Unlock()
	if current == nil {
		t.Fatalf("supervisor %q not found", id)
	}
	awaitCondition(t, func() bool { return len(current.commands) >= depth }, "supervisor command admission")
}

func awaitSupervisorQueueDepthExact(t *testing.T, manager *Manager, id string, depth int) {
	t.Helper()
	manager.mu.Lock()
	current := manager.supervisors[id]
	manager.mu.Unlock()
	if current == nil {
		t.Fatalf("supervisor %q not found", id)
	}
	awaitCondition(t, func() bool { return len(current.commands) == depth }, "supervisor command drain")
}

func awaitCondition(t *testing.T, predicate func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
