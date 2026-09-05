package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/presentation"
	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

var helperPlugin = flag.Bool("test.bsbctl-plugin-helper", false, "run as the pluginhost child fixture")

var helperRawOutput = flag.Bool("test.bsbctl-plugin-raw-output", false, "write child fixture stdout and stderr")

var helperDescendantPIDPath = flag.String("test.bsbctl-plugin-descendant-pid", "", "write the child fixture descendant PID")

var helperHostileInitialize = flag.Bool("test.bsbctl-hostile-initialize-helper", false, "return a hostile initialize error")

var helperUnknownInitialize = flag.Bool("test.bsbctl-unknown-initialize-helper", false, "return an initialize result with an unknown field")

func TestHostileInitializePluginHelperProcess(t *testing.T) {
	if !*helperHostileInitialize {
		return
	}
	file := os.NewFile(3, "bsbctl-hostile-initialize-rpc")
	if file == nil {
		t.Fatal("open inherited RPC descriptor")
	}
	conn, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	peer := rpc.NewPeer(conn)
	if err := peer.Handle("plugin.initialize", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		return nil, &rpc.Error{Code: -32099, Message: hostileRPCSecret, Data: json.RawMessage(`{"secret":"hostile-data-canary"}`)}
	}); err != nil {
		t.Fatal(err)
	}
	_ = peer.Serve(context.Background())
}

func TestUnknownInitializePluginHelperProcess(t *testing.T) {
	if !*helperUnknownInitialize {
		return
	}
	file := os.NewFile(3, "bsbctl-unknown-initialize-rpc")
	if file == nil {
		t.Fatal("open inherited RPC descriptor")
	}
	conn, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	peer := rpc.NewPeer(conn)
	if err := peer.Handle("plugin.initialize", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		return json.RawMessage(`{"plugin_id":"dev.bsbctl.test","plugin_version":"test","protocol_version":"1.0","execution_modes":["interactive"],"channels":[{"id":"main"}],"unknown":true}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := peer.Handle("plugin.instances.replace", func(context.Context, json.RawMessage) (any, *rpc.Error) {
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = peer.Serve(context.Background())
}

func TestProcessPublishesThroughRealChildAndStopsItsProcessGroup(t *testing.T) {
	published := make(chan observation.Record, 1)
	process, err := Start(context.Background(), "test", Spec{
		ID: "dev.bsbctl.test", Version: "test", Executable: os.Args[0],
		ProtocolVersion: protocol.Version,
		Args:            []string{"-test.run=^TestPluginHelperProcess$", "-test.bsbctl-plugin-helper=true"},
		ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
		Channels:        []protocol.Channel{{ID: "main"}},
		Instances: []Instance{{
			ID: "test", Generation: 1, Config: []byte(`{}`),
			Policies: map[string]presentation.PolicyConfig{"main": {
				Policy: presentation.PolicyWhenRelevant, DevicePriority: 55, HoldMS: 9000, CooldownMS: 12000,
			}},
		}},
	}, Callbacks{Observe: func(source observation.Source, value protocol.Observation) error {
		published <- observation.Record{PluginID: source.PluginID, Generation: source.Generation, Observation: value}
		return nil
	}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	invokeCtx, cancelInvoke := context.WithTimeout(context.Background(), time.Second)
	defer cancelInvoke()
	if err := process.Invoke(invokeCtx, InvokeRequest{InstanceID: "test", Generation: 1, Action: "start", SessionToken: "session"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	select {
	case record := <-published:
		if record.PluginID != "dev.bsbctl.test" || record.Observation.Instance.ID != "test" || record.Generation != 1 {
			t.Fatalf("published observation = %#v", record)
		}
		if record.Observation.Disposition != protocol.DispositionActionable || record.Observation.Impact != protocol.ImpactCritical {
			t.Fatalf("observation domain state = %#v", record.Observation)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for child publication")
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStop()
	if err := process.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestProcessAndSDKPreserveHostObservationDomainErrorsEndToEnd(t *testing.T) {
	const causeCanary = "authorization=Bearer callback-domain-secret-canary"
	for _, kind := range []protocol.ErrorKind{protocol.ErrorNotReady, protocol.ErrorGenerationConflict} {
		t.Run(string(kind), func(t *testing.T) {
			process, err := Start(t.Context(), "test", Spec{
				ID: "dev.bsbctl.test", Version: "test", Executable: os.Args[0],
				ProtocolVersion: protocol.Version,
				Args:            []string{"-test.run=^TestPluginHelperProcess$", "-test.bsbctl-plugin-helper=true"},
				ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
				Channels:        []protocol.Channel{{ID: "main"}},
				Instances: []Instance{{
					ID: "test", Generation: 1, Config: json.RawMessage(`{}`),
					Policies: map[string]presentation.PolicyConfig{"main": {Policy: presentation.PolicyInteractive}},
				}},
			}, Callbacks{Observe: func(observation.Source, protocol.Observation) error {
				return protocol.NewDomainError(kind, errors.New(causeCanary))
			}})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			stopped := false
			t.Cleanup(func() {
				if stopped {
					return
				}
				stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = process.Stop(stopCtx)
			})
			invokeCtx, cancelInvoke := context.WithTimeout(t.Context(), time.Second)
			defer cancelInvoke()
			err = process.Invoke(invokeCtx, InvokeRequest{
				InstanceID: "test", Generation: 1, Action: "start", SessionToken: "session",
			})
			domain, ok := errors.AsType[*protocol.DomainError](err)
			if !ok || domain.Kind() != kind {
				t.Fatalf("Invoke error = %v, want typed %s", err, kind)
			}
			if strings.Contains(err.Error(), causeCanary) {
				t.Fatalf("end-to-end error leaked callback cause: %v", err)
			}
			if _, pingErr := process.Ping(invokeCtx); pingErr != nil {
				t.Fatalf("domain error terminated peer: %v", pingErr)
			}
			stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelStop()
			if err := process.Stop(stopCtx); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			stopped = true
		})
	}
}

func TestPluginHelperProcess(t *testing.T) {
	if !*helperPlugin {
		return
	}
	if *helperRawOutput {
		_, _ = fmt.Fprint(os.Stdout, "plugin stdout")
		_, _ = fmt.Fprint(os.Stderr, "plugin stderr")
	}
	definition := pluginsdk.Definition{
		ID: "dev.bsbctl.test", Version: "test",
		Contract: pluginsdk.Contract{
			ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
			Channels:       []protocol.Channel{{ID: "main"}},
		},
		New: func(host *pluginsdk.Host) pluginsdk.Plugin { return &helperHandler{host: host} },
	}
	if err := pluginsdk.Run(context.Background(), definition); err != nil {
		t.Fatalf("plugin Run: %v", err)
	}
}

type helperHandler struct {
	host       *pluginsdk.Host
	generation uint64
	descendant *exec.Cmd
}

func (h *helperHandler) ReplaceInstances(_ context.Context, instances []protocol.Instance) error {
	if len(instances) > 0 {
		h.generation = instances[0].Generation
	}
	if path := *helperDescendantPIDPath; path != "" && h.descendant == nil {
		h.descendant = exec.Command("/bin/sleep", "30")
		if err := h.descendant.Start(); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(strconv.Itoa(h.descendant.Process.Pid)), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (h *helperHandler) StartSession(ctx context.Context, request protocol.SessionStartRequest) error {
	now := time.Now().UTC()
	return h.host.PublishObservation(ctx, protocol.Observation{
		Instance: request.Instance, Channel: "main", Key: "active", Revision: 1,
		Disposition: protocol.DispositionActionable, Impact: protocol.ImpactCritical,
		ReasonCode: "test_active", ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(time.Minute),
		Scene: &presentation.Scene{Elements: []presentation.Element{{ID: "text", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: "test", Font: "normal"}}}},
	})
}

func (*helperHandler) EndSession(context.Context, protocol.SessionEndRequest) error { return nil }

func (*helperHandler) HandleSessionInput(context.Context, protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, nil
}

func (*helperHandler) Health(_ context.Context) protocol.HealthResult {
	return protocol.HealthResult{Healthy: true, ObservedAt: time.Now().UTC()}
}

func (*helperHandler) Shutdown(context.Context) error { return nil }
