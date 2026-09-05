package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProcessReplaceInstancesRejectsNewEffectsUntilCommit(t *testing.T) {
	tests := []struct {
		name       string
		initial    []Instance
		fail       bool
		wantDuring []uint64
		wantAfter  []uint64
	}{
		{name: "first replacement rejects proposed effects until commit"},
		{name: "successful replacement rejects active and proposed effects", initial: []Instance{testProcessInstance(1)}},
		{name: "failed replacement rejects active and proposed effects", initial: []Instance{testProcessInstance(1)}, fail: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			leftConn, rightConn := net.Pipe()
			process := &Process{
				spec: Spec{ID: "dev.bsbctl.test"}, peer: rpc.NewPeer(leftConn),
				instances: instanceMap(test.initial),
			}
			remote := rpc.NewPeer(rightConn)
			t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })

			var effectsMu sync.Mutex
			var effects []uint64
			if err := process.register(Callbacks{Observe: func(source observation.Source, _ protocol.Observation) error {
				effectsMu.Lock()
				effects = append(effects, source.Generation)
				effectsMu.Unlock()
				return nil
			}}); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() { _ = process.peer.Serve(ctx) }()
			go func() { _ = remote.Serve(ctx) }()

			var handlerErr error
			if err := remote.Handle("plugin.instances.replace", func(callCtx context.Context, _ json.RawMessage) (any, *rpc.Error) {
				if len(test.initial) != 0 {
					activeErr := remote.Call(callCtx, "host.observation.publish", protocol.PublishRequest{Observation: testResolvedObservation(1)}, nil)
					rpcErr, matched := errors.AsType[*rpc.Error](activeErr)
					var data protocol.ErrorData
					if !matched || protocol.DecodeStrict(rpcErr.Data, &data) != nil || data.Kind != protocol.ErrorNotReady {
						handlerErr = fmt.Errorf("active generation error = %v, want not_ready", activeErr)
						return nil, &rpc.Error{Code: -32099, Message: "active generation was accepted during replacement"}
					}
				}
				proposedErr := remote.Call(callCtx, "host.observation.publish", protocol.PublishRequest{Observation: testResolvedObservation(2)}, nil)
				rpcErr, matched := errors.AsType[*rpc.Error](proposedErr)
				var data protocol.ErrorData
				if !matched || protocol.DecodeStrict(rpcErr.Data, &data) != nil || data.Kind != protocol.ErrorNotReady {
					handlerErr = fmt.Errorf("proposed generation error = %v, want not_ready", proposedErr)
					return nil, &rpc.Error{Code: -32099, Message: "proposed generation was not held until commit"}
				}
				if got := snapshotGenerations(&effectsMu, &effects); !equalUint64s(got, test.wantDuring) {
					handlerErr = fmt.Errorf("effects during replacement = %v, want %v", got, test.wantDuring)
					return nil, &rpc.Error{Code: -32099, Message: "effect committed before replacement result"}
				}
				if test.fail {
					return nil, &rpc.Error{Code: -32603, Message: "internal error"}
				}
				return struct{}{}, nil
			}); err != nil {
				t.Fatal(err)
			}

			err := process.ReplaceInstances(ctx, []Instance{testProcessInstance(2)})
			if test.fail == (err == nil) {
				t.Fatalf("ReplaceInstances error = %v, fail=%v", err, test.fail)
			}
			if handlerErr != nil {
				t.Fatal(handlerErr)
			}
			if got := snapshotGenerations(&effectsMu, &effects); !equalUint64s(got, test.wantAfter) {
				t.Fatalf("effects after replacement = %v, want %v", got, test.wantAfter)
			}

			oldErr := remote.Call(ctx, "host.observation.publish", protocol.PublishRequest{Observation: testResolvedObservation(1)}, nil)
			newErr := remote.Call(ctx, "host.observation.publish", protocol.PublishRequest{Observation: testResolvedObservation(2)}, nil)
			if test.fail {
				if oldErr != nil || newErr == nil {
					t.Fatalf("after failed replacement old error=%v new error=%v", oldErr, newErr)
				}
			} else if oldErr == nil || newErr != nil {
				t.Fatalf("after committed replacement old error=%v new error=%v", oldErr, newErr)
			}
		})
	}
}

func TestProcessReplaceInstancesAdmitsUnchangedSiblingEffects(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	active := testProcessInstance(1)
	sibling := testProcessInstance(1)
	sibling.ID = "sibling"
	observed := make(chan protocol.InstanceRef, 1)
	withdrawn := make(chan uint64, 1)
	callbacks := Callbacks{
		Observe: func(_ observation.Source, value protocol.Observation) error {
			observed <- value.Instance
			return nil
		},
		WithdrawGeneration: func(_ string, generation uint64) { withdrawn <- generation },
	}
	process := &Process{
		spec: Spec{ID: "dev.bsbctl.test"}, peer: rpc.NewPeer(leftConn),
		instances: instanceMap([]Instance{active, sibling}), callbacks: callbacks,
	}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })

	if err := process.register(callbacks); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()

	var handlerErr error
	if err := remote.Handle("plugin.instances.replace", func(callCtx context.Context, _ json.RawMessage) (any, *rpc.Error) {
		value := testResolvedObservation(1)
		value.Instance.ID = sibling.ID
		if err := remote.Call(callCtx, "host.observation.publish", protocol.PublishRequest{Observation: value}, nil); err != nil {
			handlerErr = fmt.Errorf("unchanged sibling effect: %w", err)
			return nil, &rpc.Error{Code: -32099, Message: "unchanged sibling was blocked"}
		}
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	active.Generation = 2
	if err := process.ReplaceInstances(ctx, []Instance{active, sibling}); err != nil {
		t.Fatal(err)
	}
	if handlerErr != nil {
		t.Fatal(handlerErr)
	}
	select {
	case got := <-observed:
		if got != (protocol.InstanceRef{ID: "sibling", Generation: 1}) {
			t.Fatalf("observed sibling = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("unchanged sibling effect was not admitted")
	}
	select {
	case generation := <-withdrawn:
		t.Fatalf("shared generation %d was withdrawn", generation)
	default:
	}
}

func TestProcessReplaceInstancesDoesNotWaitForAdmittedUnchangedSiblingEffect(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	active := testProcessInstance(1)
	sibling := testProcessInstance(1)
	sibling.ID = "sibling"
	effectStarted := make(chan struct{})
	releaseEffect := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseEffect:
		default:
			close(releaseEffect)
		}
	})
	process := &Process{
		spec: Spec{ID: "dev.bsbctl.test"}, peer: rpc.NewPeer(leftConn),
		instances: instanceMap([]Instance{active, sibling}),
	}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	callbacks := Callbacks{Observe: func(_ observation.Source, value protocol.Observation) error {
		if value.Instance.ID == sibling.ID {
			close(effectStarted)
			<-releaseEffect
		}
		return nil
	}}
	if err := process.register(callbacks); err != nil {
		t.Fatal(err)
	}
	if err := remote.Handle("plugin.instances.replace", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()

	value := testResolvedObservation(1)
	value.Instance.ID = sibling.ID
	effectDone := make(chan error, 1)
	go func() {
		effectDone <- remote.Call(ctx, "host.observation.publish", protocol.PublishRequest{Observation: value}, nil)
	}()
	select {
	case <-effectStarted:
	case <-time.After(time.Second):
		t.Fatal("unchanged sibling effect was not admitted")
	}
	active.Generation = 2
	replacementDone := make(chan error, 1)
	go func() { replacementDone <- process.ReplaceInstances(ctx, []Instance{active, sibling}) }()
	select {
	case err := <-replacementDone:
		if err != nil {
			t.Fatalf("ReplaceInstances: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unchanged sibling effect blocked replacement completion")
	}
	close(releaseEffect)
	select {
	case err := <-effectDone:
		if err != nil {
			t.Fatalf("unchanged sibling effect: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unchanged sibling effect did not complete")
	}
}

func TestProcessReplaceInstancesWaitsForAdmittedEffectAndLeavesRetirementToSupervisor(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	effectStarted := make(chan struct{})
	releaseEffect := make(chan struct{})
	var withdrawals atomic.Int64
	callbacks := Callbacks{
		Observe: func(observation.Source, protocol.Observation) error {
			close(effectStarted)
			<-releaseEffect
			return nil
		},
		WithdrawGeneration: func(string, uint64) { withdrawals.Add(1) },
	}
	process := &Process{
		spec: Spec{ID: "dev.bsbctl.test"}, peer: rpc.NewPeer(leftConn),
		instances: instanceMap([]Instance{testProcessInstance(1)}), callbacks: callbacks,
	}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })

	if err := process.register(callbacks); err != nil {
		t.Fatal(err)
	}
	replacementHandled := make(chan struct{})
	if err := remote.Handle("plugin.instances.replace", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		close(replacementHandled)
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()

	effectDone := make(chan error, 1)
	go func() {
		effectDone <- remote.Call(ctx, "host.observation.publish", protocol.PublishRequest{Observation: testResolvedObservation(1)}, nil)
	}()
	<-effectStarted
	replacementDone := make(chan error, 1)
	go func() {
		replacementDone <- process.ReplaceInstances(ctx, []Instance{testProcessInstance(2)})
	}()
	select {
	case <-replacementHandled:
		t.Fatal("replacement reached the plugin while an admitted effect was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseEffect)
	if err := <-effectDone; err != nil {
		t.Fatalf("admitted effect: %v", err)
	}
	select {
	case <-replacementHandled:
	case <-time.After(time.Second):
		t.Fatal("replacement did not start after the admitted effect drained")
	}
	if err := <-replacementDone; err != nil {
		t.Fatalf("ReplaceInstances: %v", err)
	}
	if got := withdrawals.Load(); got != 0 {
		t.Fatalf("Process emitted %d replacement-retirement callbacks; supervisor must own retirement", got)
	}
}

func TestProcessReplaceInstancesRejectsProposedEffectsUntilCommit(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{
		spec: Spec{ID: "dev.bsbctl.test"}, peer: rpc.NewPeer(leftConn),
		instances: instanceMap([]Instance{testProcessInstance(1)}),
	}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })

	var effects atomic.Int64
	if err := process.register(Callbacks{Observe: func(observation.Source, protocol.Observation) error {
		effects.Add(1)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	var proposedErr error
	if err := remote.Handle("plugin.instances.replace", func(callCtx context.Context, _ json.RawMessage) (any, *rpc.Error) {
		proposedErr = remote.Call(callCtx, "host.observation.publish", protocol.PublishRequest{Observation: testResolvedObservation(2)}, nil)
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()

	if err := process.ReplaceInstances(ctx, []Instance{testProcessInstance(2)}); err != nil {
		t.Fatalf("ReplaceInstances: %v", err)
	}
	rpcErr, matched := errors.AsType[*rpc.Error](proposedErr)
	if !matched {
		t.Fatalf("proposed effect error = %v, want typed RPC error", proposedErr)
	}
	var data protocol.ErrorData
	if protocol.DecodeStrict(rpcErr.Data, &data) != nil || data.Kind != protocol.ErrorNotReady {
		t.Fatalf("proposed effect data = %s, want not_ready", rpcErr.Data)
	}
	if got := effects.Load(); got != 0 {
		t.Fatalf("proposed effects committed during replacement = %d, want 0", got)
	}
}

func testResolvedObservation(generation uint64) protocol.Observation {
	now := time.Now().UTC()
	return protocol.Observation{
		Instance: protocol.InstanceRef{ID: "app", Generation: generation}, Channel: "main", Key: "state", Revision: 1,
		Disposition: protocol.DispositionResolved, Impact: protocol.ImpactNormal, ReasonCode: "test_state", ObservedAt: now, UpdatedAt: now,
	}
}

func snapshotGenerations(mu *sync.Mutex, values *[]uint64) []uint64 {
	mu.Lock()
	defer mu.Unlock()
	return append([]uint64(nil), (*values)...)
}

func equalUint64s(left, right []uint64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestProcessValidatesAndDeliversStructuredPluginLogs(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{
		spec: Spec{ID: "dev.bsbctl.test"}, peer: rpc.NewPeer(leftConn),
		instances: map[string]Instance{"configured": {ID: "configured", Generation: 7}},
	}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	delivered := make(chan protocol.LogNotification, 1)
	if err := process.register(Callbacks{Log: func(pluginID string, notification protocol.LogNotification) {
		if pluginID != "dev.bsbctl.test" {
			t.Errorf("plugin ID = %q", pluginID)
		}
		delivered <- notification
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()

	callCtx, callCancel := context.WithTimeout(ctx, time.Second)
	defer callCancel()
	want := protocol.LogNotification{
		Level: protocol.LogLevelInfo, Event: "sync.completed", Instance: protocol.InstanceRef{ID: "configured", Generation: 7},
		Message: "sync completed", Fields: map[string]string{"item_count": "12"},
	}
	if err := remote.Call(callCtx, "host.log", want, nil); err != nil {
		t.Fatalf("valid host.log: %v", err)
	}
	select {
	case got := <-delivered:
		if got.Event != want.Event || got.Instance != want.Instance || got.Fields["item_count"] != "12" {
			t.Fatalf("delivered log = %#v, want %#v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for log callback")
	}

	for name, params := range map[string]any{
		"unknown field": json.RawMessage(`{"level":"info","event":"sync.completed","extra":true}`),
		"invalid value": protocol.LogNotification{Level: protocol.LogLevelInfo, Event: "Bad Event"},
		"unknown instance": protocol.LogNotification{
			Level: protocol.LogLevelInfo, Event: "sync.completed", Instance: protocol.InstanceRef{ID: "other", Generation: 7},
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalidCtx, invalidCancel := context.WithTimeout(ctx, time.Second)
			defer invalidCancel()
			err := remote.Call(invalidCtx, "host.log", params, nil)
			if _, ok := errors.AsType[*rpc.Error](err); !ok {
				t.Fatalf("invalid log error = %v, want RPC rejection", err)
			}
			select {
			case got := <-delivered:
				t.Fatalf("invalid log reached callback: %#v", got)
			default:
			}
		})
	}
}

func TestProcessAuthenticatesStrictGenerationScopedCheckpointCallback(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{
		spec: Spec{ID: "dev.bsbctl.test"}, peer: rpc.NewPeer(leftConn),
		instances: map[string]Instance{
			"configured": {ID: "configured", Generation: 7},
		},
	}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	delivered := make(chan protocol.CheckpointRequest, 1)
	if err := process.register(Callbacks{Checkpoint: func(pluginID string, request protocol.CheckpointRequest) error {
		if pluginID != "dev.bsbctl.test" {
			t.Errorf("plugin ID = %q", pluginID)
		}
		delivered <- request
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()
	valid := protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: "configured", Generation: 7}, Data: json.RawMessage(`{"cursor":"next"}`)}
	callCtx, callCancel := context.WithTimeout(ctx, time.Second)
	defer callCancel()
	if err := remote.Call(callCtx, "host.checkpoint.save", valid, nil); err != nil {
		t.Fatalf("valid checkpoint: %v", err)
	}
	select {
	case got := <-delivered:
		if got.Instance != valid.Instance || string(got.Data) != string(valid.Data) {
			t.Fatalf("checkpoint callback = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for checkpoint callback")
	}
	atLimit := valid
	atLimit.Data = pluginObjectOfSize(t, protocol.MaxJSONObjectBytes)
	boundaryCtx, boundaryCancel := context.WithTimeout(ctx, time.Second)
	defer boundaryCancel()
	if err := remote.Call(boundaryCtx, "host.checkpoint.save", atLimit, nil); err != nil {
		t.Fatalf("checkpoint at exact limit: %v", err)
	}
	select {
	case got := <-delivered:
		if len(got.Data) != protocol.MaxJSONObjectBytes {
			t.Fatalf("checkpoint callback data bytes = %d, want %d", len(got.Data), protocol.MaxJSONObjectBytes)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for exact-limit checkpoint callback")
	}

	for name, params := range map[string]any{
		"unknown field":     json.RawMessage(`{"instance":{"id":"configured","generation":7},"data":{},"extra":true}`),
		"missing data":      json.RawMessage(`{"instance":{"id":"configured","generation":7}}`),
		"scalar data":       protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: "configured", Generation: 7}, Data: json.RawMessage(`"value"`)},
		"array data":        protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: "configured", Generation: 7}, Data: json.RawMessage(`[]`)},
		"null data":         protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: "configured", Generation: 7}, Data: json.RawMessage(`null`)},
		"oversized data":    protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: "configured", Generation: 7}, Data: pluginObjectOfSize(t, protocol.MaxJSONObjectBytes+1)},
		"unknown instance":  protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: "missing", Generation: 7}, Data: json.RawMessage(`{}`)},
		"stale generation":  protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: "configured", Generation: 6}, Data: json.RawMessage(`{}`)},
		"future generation": protocol.CheckpointRequest{Instance: protocol.InstanceRef{ID: "configured", Generation: 8}, Data: json.RawMessage(`{}`)},
	} {
		t.Run(name, func(t *testing.T) {
			invalidCtx, invalidCancel := context.WithTimeout(ctx, time.Second)
			defer invalidCancel()
			err := remote.Call(invalidCtx, "host.checkpoint.save", params, nil)
			if _, ok := errors.AsType[*rpc.Error](err); !ok {
				t.Fatalf("checkpoint error = %v, want RPC rejection", err)
			}
			select {
			case got := <-delivered:
				t.Fatalf("rejected checkpoint reached callback: %#v", got)
			default:
			}
		})
	}
}

func TestProcessReportsCheckpointPersistenceFailureGenerically(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{
		spec: Spec{ID: "dev.bsbctl.test"}, peer: rpc.NewPeer(leftConn),
		instances: map[string]Instance{"configured": {ID: "configured", Generation: 7}},
	}
	remote := rpc.NewPeer(rightConn)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	if err := process.register(Callbacks{Checkpoint: func(string, protocol.CheckpointRequest) error {
		return errors.New("secret persistence failure canary")
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()
	callCtx, callCancel := context.WithTimeout(ctx, time.Second)
	defer callCancel()
	err := remote.Call(callCtx, "host.checkpoint.save", protocol.CheckpointRequest{
		Instance: protocol.InstanceRef{ID: "configured", Generation: 7}, Data: json.RawMessage(`{}`),
	}, nil)
	rpcErr, ok := errors.AsType[*rpc.Error](err)
	var data protocol.ErrorData
	if !ok || rpcErr.Code != -32000 || rpcErr.Message != "bsbctl request failed" ||
		protocol.DecodeStrict(rpcErr.Data, &data) != nil || data.Kind != protocol.ErrorNotReady || strings.Contains(err.Error(), "canary") {
		t.Fatalf("checkpoint persistence error = %#v", err)
	}
}

func TestProcessRegistersOnlyHostMetricAsLossyInbound(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{spec: Spec{ID: "dev.bsbctl.test"}, peer: rpc.NewPeer(leftConn)}
	remote := rpc.NewPeer(rightConn)
	release := make(chan struct{})
	started := make(chan struct{}, 32)
	t.Cleanup(func() {
		close(release)
		_ = process.peer.Close()
		_ = remote.Close()
	})
	if err := process.register(Callbacks{Metric: func(protocol.MetricNotification) {
		started <- struct{}{}
		<-release
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = process.peer.Serve(ctx) }()
	go func() { _ = remote.Serve(ctx) }()

	for i := 0; i < 32; i++ {
		notifyCtx, notifyCancel := context.WithTimeout(ctx, time.Second)
		err := remote.Notify(notifyCtx, "host.metric", protocol.MetricNotification{Name: "active"})
		notifyCancel()
		if err != nil {
			t.Fatalf("active metric %d: %v", i, err)
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("metric handler %d did not start", i)
		}
	}
	for i := 0; i < 64; i++ {
		notifyCtx, notifyCancel := context.WithTimeout(ctx, time.Second)
		err := remote.Notify(notifyCtx, "host.metric", protocol.MetricNotification{Name: "overflow"})
		notifyCancel()
		if err != nil {
			t.Fatalf("overflow metric %d: %v", i, err)
		}
	}
	select {
	case <-process.peer.Done():
		t.Fatal("lossy host.metric saturation closed the process peer")
	default:
	}
}

func TestProcessDropsInvalidMetricBeforeCallback(t *testing.T) {
	leftConn, rightConn := net.Pipe()
	process := &Process{spec: Spec{ID: "dev.bsbctl.test"}, peer: rpc.NewPeer(leftConn)}
	remote := rpc.NewPeer(rightConn)
	called := make(chan struct{}, 1)
	t.Cleanup(func() { _ = process.peer.Close(); _ = remote.Close() })
	if err := process.register(Callbacks{Metric: func(protocol.MetricNotification) { called <- struct{}{} }}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = process.peer.Serve(t.Context()) }()
	go func() { _ = remote.Serve(t.Context()) }()
	if err := remote.Notify(t.Context(), "host.metric", json.RawMessage(`{"name":"active","value":1,"unexpected":true}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
		t.Fatal("invalid metric reached callback")
	case <-time.After(20 * time.Millisecond):
	}
}
