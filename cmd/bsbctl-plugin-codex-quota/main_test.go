package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/plugins/codexquota"
	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

var helperProcess = flag.Bool("test.bsbctl-codex-quota-helper", false, "run as the Codex quota child fixture")

func TestCodexQuotaPluginRunsAsSupervisedJSONRPCChild(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer test-access" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = fmt.Fprint(writer, `{"rate_limit":{"primary_window":{"used_percent":38,"reset_at":2000000000,"limit_window_seconds":18000}}}`)
	}))
	defer server.Close()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"access_token":"test-access"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`chatgpt_base_url = "`+server.URL+`"`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	published := make(chan protocol.Observation, 1)
	process, err := pluginhost.Start(context.Background(), "test", pluginhost.Spec{
		ID: codexquota.PluginID, Version: codexquota.PluginVersion, Executable: os.Args[0],
		ProtocolVersion: protocol.Version,
		Args:            []string{"-test.run=^TestCodexQuotaPluginHelperProcess$", "-test.bsbctl-codex-quota-helper=true"},
		ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeResident, protocol.ExecutionModeInteractive},
		Channels:        []protocol.Channel{{ID: codexquota.ChannelSummary}, {ID: codexquota.ChannelPressure}, {ID: codexquota.ChannelLive}},
		Instances: []pluginhost.Instance{{
			ID: codexquota.AppID, Generation: 1,
			Config: []byte(`{"label":"MAIN","badge":"M"}`),
			Policies: map[string]presentation.PolicyConfig{
				codexquota.ChannelSummary:  {Policy: presentation.PolicyRotation},
				codexquota.ChannelPressure: {Policy: presentation.PolicyWhenRelevant},
				codexquota.ChannelLive:     {Policy: presentation.PolicyInteractive},
			},
		}},
	}, pluginhost.Callbacks{Observe: func(_ observation.Source, value protocol.Observation) error {
		published <- value
		return nil
	}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = process.Stop(ctx)
	}()
	select {
	case value := <-published:
		if value.Channel != codexquota.ChannelSummary || value.Disposition != protocol.DispositionNotable {
			t.Fatalf("initial observation = %#v", value)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for initial Codex quota summary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	health, err := process.Ping(ctx)
	if err != nil || !health.Healthy {
		t.Fatalf("Ping = %#v, %v", health, err)
	}
}

func TestCodexQuotaPluginHelperProcess(t *testing.T) {
	if !*helperProcess {
		return
	}
	if err := pluginsdk.Run(context.Background(), codexquota.DefinitionForVersion(codexquota.PluginVersion)); err != nil {
		t.Fatalf("plugin Run: %v", err)
	}
}
