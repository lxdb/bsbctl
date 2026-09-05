package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginCommandsProduceDeterministicRedactedJSON(t *testing.T) {
	client := &fakeCLIClient{}
	client.call = func(_ context.Context, method string, params, result any) error {
		switch method {
		case "daemon.status":
			value := result.(*control.Status)
			*value = control.Status{
				Generation: 7,
				Apps:       []control.AppStatus{{AppID: "zeta", PluginID: "plugin-z", RuntimeGeneration: 3, Enabled: false}, {AppID: "alpha", PluginID: "plugin-a", RuntimeGeneration: 5, Enabled: true}},
				Plugins:    []pluginhost.PluginStatus{{ID: "plugin-a", Phase: pluginhost.PhaseRunning, Healthy: true}, {ID: "plugin-z", Phase: pluginhost.PhaseBackoff, LastErrorCode: "safe_code"}},
				Readiness:  []daemon.AppReadiness{{AppID: "alpha", PluginID: "plugin-a", Phase: daemon.AppReady}, {AppID: "zeta", PluginID: "plugin-z", Phase: daemon.AppDisabled}},
			}
		case "app.set_enabled":
			request := params.(control.SetEnabledRequest)
			*(result.(*control.AppMutationResult)) = control.AppMutationResult{Status: control.MutationUpdated, AppID: request.AppID, Enabled: request.Enabled, Generation: 8}
		default:
			return errors.New("unexpected method")
		}
		return nil
	}
	restore := installCLIClient(t, client)
	defer restore()

	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"app", "list", "--socket", "/ignored"}, want: `{"apps":[{"app_id":"alpha","plugin_id":"plugin-a","enabled":true,"generation":5,"readiness":"ready","plugin_phase":"running","healthy":true},{"app_id":"zeta","plugin_id":"plugin-z","enabled":false,"generation":3,"readiness":"disabled","plugin_phase":"backoff","healthy":false}]}` + "\n"},
		{args: []string{"app", "status", "alpha", "--socket", "/ignored"}, want: `{"app_id":"alpha","plugin_id":"plugin-a","enabled":true,"generation":5,"readiness":"ready","plugin_phase":"running","healthy":true}` + "\n"},
		{args: []string{"app", "enable", "alpha", "--socket", "/ignored"}, want: `{"status":"updated","app_id":"alpha","enabled":true,"generation":8}` + "\n"},
		{args: []string{"app", "disable", "alpha", "--socket", "/ignored"}, want: `{"status":"updated","app_id":"alpha","enabled":false,"generation":8}` + "\n"},
	} {
		var stdout, stderr bytes.Buffer
		if code := execute(context.Background(), test.args, strings.NewReader(""), &stdout, &stderr); code != 0 || stdout.String() != test.want || stderr.Len() != 0 {
			t.Fatalf("execute(%v) = code %d stdout %q stderr %q", test.args, code, stdout.String(), stderr.String())
		}
	}
}

func TestPluginConfigStrictInputAndNoEcho(t *testing.T) {
	directory := t.TempDir()
	validPath := filepath.Join(directory, "app.json")
	valid := `{"config":{"sentinel":"private"},"secrets":{"token":"keychain://bsbctl/account"},"policies":{"answer":{"policy":"interactive"}},"launch_action":"ask"}`
	if err := os.WriteFile(validPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &fakeCLIClient{call: func(_ context.Context, method string, params, result any) error {
		if method != "app.replace_config" {
			return errors.New("unexpected method")
		}
		request := params.(control.ReplaceConfigRequest)
		configuredRequest := request.Secrets["token"] == "keychain://bsbctl/account" && request.Policies["answer"].Policy == presentation.PolicyInteractive
		boundaryRequest := len(request.Config) == protocol.MaxJSONObjectBytes && len(request.Secrets) == 0 && len(request.Policies) == 0
		if request.AppID != "ball8" || (!configuredRequest && !boundaryRequest) {
			t.Fatalf("request app = %q, config bytes = %d, secrets = %d, policies = %d", request.AppID, len(request.Config), len(request.Secrets), len(request.Policies))
		}
		*(result.(*control.AppConfigResult)) = control.AppConfigResult{Status: control.MutationUpdated, AppID: "ball8", Generation: 9}
		return nil
	}}
	restore := installCLIClient(t, client)
	defer restore()
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{"app", "config", "ball8", "--file", validPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stdout.String() != `{"status":"updated","app_id":"ball8","generation":9}`+"\n" || stderr.Len() != 0 || strings.Contains(stdout.String(), "private") || strings.Contains(stdout.String(), "keychain") {
		t.Fatalf("valid config = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}

	invalidInputs := []string{
		`[]`,
		`{"config":"private"}`,
		`{"config":[]}`,
		`{"config":null}`,
		`{"config":{},"config":{}}`,
		`{"config":{"duplicate":1,"duplicate":2}}`,
		`{"config":{},"unknown":true}`,
		`{"config":{},"secrets":{"token":"raw-secret"}}`,
		`{"config":{},"secrets":{"token":"keychain://bsbctl/team/account"}}`,
		`{"config":{}} trailing`,
		`{"config":` + string(cliJSONObjectOfSize(t, protocol.MaxJSONObjectBytes+1)) + `}`,
	}
	for index, input := range invalidInputs {
		stdout.Reset()
		stderr.Reset()
		code := execute(context.Background(), []string{"app", "config", "ball8", "--file", "-"}, strings.NewReader(input), &stdout, &stderr)
		if code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("invalid input %d = code %d stdout %q stderr %q", index, code, stdout.String(), stderr.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	atLimit := `{"config":` + string(cliJSONObjectOfSize(t, protocol.MaxJSONObjectBytes)) + `}`
	code = execute(context.Background(), []string{"app", "config", "ball8", "--file", "-"}, strings.NewReader(atLimit), &stdout, &stderr)
	if code != exitSuccess || stdout.String() != `{"status":"updated","app_id":"ball8","generation":9}`+"\n" || stderr.Len() != 0 {
		t.Fatalf("exact-limit config = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}
