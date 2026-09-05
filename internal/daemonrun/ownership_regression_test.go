package daemonrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/pluginlog"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestRunRestoresAttentionBeforeLivePluginAdmission(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "bctl-restore-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	configPath, socketPath := filepath.Join(directory, "config.json"), filepath.Join(directory, "ctl.sock")
	document := serviceMainDocument()
	document.Device = config.Device{BaseURL: "http://busybar.invalid", AccessTokenSecret: "keychain://test/unavailable"}
	document.Plugins["resident"] = config.Plugin{
		ID: "resident", Version: "1", Executable: "/test/resident", ProtocolVersion: protocol.Version,
		ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident}, Channels: []protocol.Channel{{ID: "main"}},
	}
	document.Apps["resident"] = config.App{
		ID: "resident", PluginID: "resident", Enabled: true, Config: []byte(`{}`),
		Policies: map[string]presentation.PolicyConfig{"main": {Policy: presentation.PolicyWhenRelevant}},
	}
	store := config.NewStore(configPath)
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	document, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	identity := attention.StateIdentity{PluginID: "resident", InstanceID: "resident", Generation: document.Apps["resident"].Generation, Channel: "main", Key: "state"}
	want := attention.StateDocument{
		Version:          attention.StateVersion,
		Acknowledgements: []attention.AcknowledgementState{{Identity: identity, ObservedAt: now, TouchedAt: now}},
		LastShown:        []attention.LastShownState{{Identity: identity, ShownAt: now}},
	}
	state := attention.NewStateStore(filepath.Join(directory, "attention-state.json"))
	if _, err := state.Save(want); err != nil {
		t.Fatal(err)
	}
	dependencies := testDaemonDependencies()
	dependencies.newSecretResolver = func() secretResolver {
		return deviceSecretResolverFunc(func(context.Context, string) (string, error) { return "", errors.New("test credential unavailable") })
	}
	dependencies.newPluginRuntime = func(pluginhost.Callbacks) pluginRuntime { return &fakePluginRuntime{} }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Options{Version: "test", ConfigPath: configPath, SocketPath: socketPath, Stderr: io.Discard}, dependencies)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("daemon did not join")
		}
	})
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		client, err := control.Dial(ctx, socketPath)
		if err == nil {
			var status control.Status
			err = client.Call(ctx, "daemon.status", nil, &status)
			_ = client.Close()
			if err != nil {
				t.Fatal(err)
			}
			if status.AttentionState.RestoredEntries != 2 || status.AttentionState.DiscardedEntries != 0 || status.AttentionState.AcknowledgementEntries != 1 || status.AttentionState.LastShownEntries != 1 {
				t.Fatalf("startup discarded current durable scheduling state: %#v", status.AttentionState)
			}
			got, err := state.Load()
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("startup changed durable state: %#v, %v", got, err)
			}
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("control socket did not start: %v", err)
		}
	}
}

type serialSecretRuntime struct {
	fakePluginRuntime
	mu     sync.Mutex
	sink   *pluginlog.Sink
	secret string
}

func (r *serialSecretRuntime) Apply(_ context.Context, specs []pluginhost.Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secret = specs[0].Instances[0].Secrets["key"]
	runtime.Gosched()
	r.sink.Log("test", protocol.LogNotification{Level: protocol.LogLevelInfo, Event: "apply", Message: r.secret})
	return nil
}

func (r *serialSecretRuntime) Close(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	runtime.Gosched()
	r.sink.Log("test", protocol.LogNotification{Level: protocol.LogLevelInfo, Event: "close", Message: r.secret})
	r.secret = ""
	return nil
}

func TestConcurrentPluginLifecycleKeepsDeliveredSecretsRedacted(t *testing.T) {
	var output bytes.Buffer
	sink := pluginlog.New(&output, pluginlog.Options{QueueCapacity: 128})
	owner := &redactingPluginRuntime{pluginRuntime: &serialSecretRuntime{sink: sink}, logs: sink}
	var operations sync.WaitGroup
	start := make(chan struct{})
	for index := range 32 {
		operations.Go(func() {
			<-start
			secret := fmt.Sprintf("opaque-lifecycle-canary-%02d", index)
			if err := owner.Apply(t.Context(), []pluginhost.Spec{{ID: "test", Instances: []pluginhost.Instance{{ID: "app", Secrets: map[string]string{"key": secret}}}}}); err != nil {
				t.Error(err)
			}
		})
		if index%4 == 0 {
			operations.Go(func() {
				<-start
				if err := owner.Close(t.Context()); err != nil {
					t.Error(err)
				}
			})
		}
	}
	close(start)
	operations.Wait()
	if err := sink.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "opaque-lifecycle-canary") || strings.Count(output.String(), `"event":"apply"`) != 32 || strings.Count(output.String(), "[REDACTED]") < 32 {
		t.Fatalf("concurrent lifecycle lost redaction or diagnostics: %s", output.String())
	}
}
