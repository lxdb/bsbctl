package main

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestPluginConfigurationFromDefinitionCopiesCanonicalWireContract(t *testing.T) {
	definition := pluginsdk.Definition{
		ID: "dev.bsbctl.test", Version: "9.8.7",
		Contract: pluginsdk.Contract{
			ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident},
			Channels:       []protocol.Channel{{ID: "summary"}},
			Operations:     []protocol.OperationDescriptor{{ID: "inspect", Kind: protocol.OperationQuery}},
		},
	}
	got := pluginConfigurationFromDefinition(definition, "/tmp/bsbctl-plugin-test")
	definition.Contract.ExecutionModes[0] = protocol.ExecutionModeInteractive
	definition.Contract.Channels[0].ID = "changed"
	definition.Contract.Operations[0].ID = "changed"
	if got.ID != "dev.bsbctl.test" || got.Version != "9.8.7" || got.Executable != "/tmp/bsbctl-plugin-test" ||
		got.ProtocolVersion != protocol.Version ||
		!reflect.DeepEqual(got.ExecutionModes, []protocol.ExecutionMode{protocol.ExecutionModeResident}) ||
		!reflect.DeepEqual(got.Channels, []protocol.Channel{{ID: "summary"}}) ||
		!reflect.DeepEqual(got.Operations, []protocol.OperationDescriptor{{ID: "inspect", Kind: protocol.OperationQuery}}) {
		t.Fatalf("projected plugin = %#v", got)
	}
}

func TestPluginVerifyRequiresManifestAndFixtureWithoutResolvingDaemonSocket(t *testing.T) {
	t.Parallel()
	err := runPlugin(context.Background(), []string{"verify", "--manifest", "manifest.json"}, strings.NewReader(""), io.Discard, io.Discard)
	commandErr, ok := errors.AsType[*commandError](err)
	if !ok || commandErr.code != exitUsage || commandErr.message != "plugin verify requires --manifest PATH and --fixture PATH" {
		t.Fatalf("runPlugin verify error = %#v, want usage", err)
	}
}
