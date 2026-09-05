package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/plugins/codexquota"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"reflect"
	"strings"
	"testing"
)

func TestAppCreateAndDeleteUseDaemonTransactions(t *testing.T) {
	methods := make([]string, 0, 2)
	client := &fakeCLIClient{call: func(_ context.Context, method string, params, result any) error {
		methods = append(methods, method)
		switch method {
		case "app.create":
			request := params.(control.CreateAppRequest)
			if request.AppID != "codex-secondary" || request.PluginID != codexquota.PluginID || !request.Enabled || string(request.Config) != `{"credentials_home":"/tmp/secondary","badge":"S"}` {
				t.Fatalf("create request = %#v", request)
			}
			*result.(*control.AppInstanceResult) = control.AppInstanceResult{Status: control.MutationCreated, AppID: request.AppID, PluginID: request.PluginID, Enabled: true, Generation: 7}
		case "app.delete":
			request := params.(control.DeleteAppRequest)
			if request.AppID != "codex-secondary" {
				t.Fatalf("delete request = %#v", request)
			}
			*result.(*control.AppInstanceResult) = control.AppInstanceResult{Status: control.MutationDeleted, AppID: request.AppID, PluginID: codexquota.PluginID, Generation: 8}
		default:
			t.Fatalf("method = %q", method)
		}
		return nil
	}}
	restore := installCLIClient(t, client)
	defer restore()
	input := `{"config":{"credentials_home":"/tmp/secondary","badge":"S"},"policies":{"summary":{"policy":"rotation","rotation_interval_ms":60000},"pressure":{"policy":"when_relevant"}}}`
	var created, stderr bytes.Buffer
	code := execute(context.Background(), []string{"app", "create", "codex-secondary", "--plugin", codexquota.PluginID, "--file", "-", "--enabled", "true", "--socket", "/unused"}, strings.NewReader(input), &created, &stderr)
	if code != exitSuccess || stderr.String() != "" || !strings.Contains(created.String(), `"status":"created"`) {
		t.Fatalf("create = code %d stdout %q stderr %q", code, created.String(), stderr.String())
	}
	var deleted bytes.Buffer
	code = execute(context.Background(), []string{"app", "delete", "codex-secondary", "--socket", "/unused"}, strings.NewReader(""), &deleted, &stderr)
	if code != exitSuccess || !strings.Contains(deleted.String(), `"status":"deleted"`) {
		t.Fatalf("delete = code %d stdout %q stderr %q", code, deleted.String(), stderr.String())
	}
	if !reflect.DeepEqual(methods, []string{"app.create", "app.delete"}) {
		t.Fatalf("methods = %v", methods)
	}
}

func TestAppCreateUsesRegistryDefaultsWithoutReadingConfiguration(t *testing.T) {
	for _, descriptor := range firstpartyplugins.All() {
		t.Run(descriptor.DefaultApp.ID, func(t *testing.T) {
			var request control.CreateAppRequest
			client := &fakeCLIClient{call: func(_ context.Context, method string, params, result any) error {
				if method != "app.create" {
					t.Fatalf("method = %q", method)
				}
				request = params.(control.CreateAppRequest)
				*result.(*control.AppInstanceResult) = control.AppInstanceResult{
					Status: control.MutationCreated, AppID: request.AppID, PluginID: request.PluginID,
					Enabled: request.Enabled, Generation: 7,
				}
				return nil
			}}
			restore := installCLIClient(t, client)
			defer restore()

			var stdout, stderr bytes.Buffer
			reader := &unexpectedInputReader{t: t}
			code := execute(t.Context(), []string{"app", "create", descriptor.DefaultApp.ID, "--socket", "/unused"}, reader, &stdout, &stderr)
			if code != exitSuccess || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"status":"created"`) {
				t.Fatalf("create = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
			}
			want := control.CreateAppRequest{
				AppID: descriptor.DefaultApp.ID, PluginID: descriptor.ID, Enabled: true,
				Config: descriptor.DefaultApp.Config, Secrets: descriptor.DefaultApp.Secrets,
				Policies: descriptor.DefaultApp.Policies, LaunchAction: descriptor.DefaultApp.LaunchAction,
			}
			if !reflect.DeepEqual(request, want) {
				t.Fatalf("request = %#v, want %#v", request, want)
			}
		})
	}
}

func TestAppCreateDefaultCanBeDisabled(t *testing.T) {
	client := &fakeCLIClient{call: func(_ context.Context, method string, params, result any) error {
		if method != "app.create" {
			t.Fatalf("method = %q", method)
		}
		request := params.(control.CreateAppRequest)
		if request.AppID != "mac-resources" || request.Enabled {
			t.Fatalf("request = %#v", request)
		}
		*result.(*control.AppInstanceResult) = control.AppInstanceResult{
			Status: control.MutationCreated, AppID: request.AppID, PluginID: request.PluginID, Enabled: false, Generation: 7,
		}
		return nil
	}}
	restore := installCLIClient(t, client)
	defer restore()
	var stdout, stderr bytes.Buffer
	code := execute(t.Context(), []string{"app", "create", "mac-resources", "--enabled", "false", "--socket", "/unused"}, &unexpectedInputReader{t: t}, &stdout, &stderr)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("create disabled = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestAppCreateRejectsUnknownDefaultAndPartialCustomFlagsBeforeDial(t *testing.T) {
	previous := dialControl
	dialControl = func(context.Context, string) (controlClient, error) {
		t.Fatal("dialed daemon for invalid app creation")
		return nil, nil
	}
	defer func() { dialControl = previous }()
	tests := [][]string{
		{"app", "create", "unknown"},
		{"app", "create", "custom", "--plugin", "example.plugin"},
		{"app", "create", "custom", "--file", "-"},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		code := execute(t.Context(), args, &unexpectedInputReader{t: t}, &stdout, &stderr)
		if code != exitUsage || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("execute(%v) = code %d stdout %q stderr %q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestAppCreateRejectsInvalidEnabledValueBeforeDial(t *testing.T) {
	previous := dialControl
	dialControl = func(context.Context, string) (controlClient, error) {
		t.Fatal("dialed daemon for invalid input")
		return nil, nil
	}
	defer func() { dialControl = previous }()
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{"app", "create", "second", "--plugin", "plugin", "--file", "-", "--enabled", "maybe"}, strings.NewReader(`{"config":{}}`), &stdout, &stderr)
	if code != exitUsage || stdout.String() != "" || stderr.String() == "" {
		t.Fatalf("invalid create = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestAppOperationsRouteKindIdentityAndPayload(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		file        string
		stdin       string
		wantKind    protocol.OperationKind
		wantPayload string
	}{
		{name: "query default payload", command: "query", wantKind: protocol.OperationQuery, wantPayload: `{}`},
		{name: "command stdin payload", command: "command", file: "-", stdin: `{"confirm":true}`, wantKind: protocol.OperationCommand, wantPayload: `{"confirm":true}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeCLIClient{call: func(_ context.Context, method string, params, result any) error {
				if method != "app.operation" {
					t.Fatalf("method = %q, want app.operation", method)
				}
				request := params.(control.PluginOperationRequest)
				if request.AppID != "codex" || request.Operation != "inspect" || request.Kind != test.wantKind || string(request.Payload) != test.wantPayload {
					t.Fatalf("request = %#v, want kind %q payload %s", request, test.wantKind, test.wantPayload)
				}
				result.(*protocol.OperationResult).Payload = json.RawMessage(`{"sessions":[]}`)
				return nil
			}}
			restore := installCLIClient(t, client)
			t.Cleanup(restore)

			args := []string{"app", test.command, "codex", "inspect", "--socket", "/ignored"}
			if test.file != "" {
				args = append(args, "--file", test.file)
			}
			var stdout, stderr bytes.Buffer
			if code := execute(t.Context(), args, strings.NewReader(test.stdin), &stdout, &stderr); code != 0 || stdout.String() != `{"sessions":[]}`+"\n" || stderr.Len() != 0 {
				t.Fatalf("execute = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestAppOperationRejectsInvalidInputAndOutputWithoutLeakingData(t *testing.T) {
	invalid := []struct {
		name    string
		payload json.RawMessage
	}{
		{name: "scalar", payload: json.RawMessage(`"private"`)},
		{name: "array", payload: json.RawMessage(`["private"]`)},
		{name: "null", payload: json.RawMessage(`null`)},
		{name: "malformed", payload: json.RawMessage(`{"token":"private"`)},
		{name: "over limit", payload: cliJSONObjectOfSize(t, protocol.MaxJSONObjectBytes+1)},
	}
	for _, test := range invalid {
		t.Run("input "+test.name, func(t *testing.T) {
			called := false
			restore := installCLIClient(t, &fakeCLIClient{call: func(context.Context, string, any, any) error {
				called = true
				return nil
			}})
			t.Cleanup(restore)
			var stdout, stderr bytes.Buffer
			code := execute(t.Context(), []string{"app", "query", "codex", "inspect", "--file", "-", "--socket", "/ignored"}, bytes.NewReader(test.payload), &stdout, &stderr)
			if code != exitUsage || called || stdout.Len() != 0 || strings.Contains(stderr.String(), "private") {
				t.Fatalf("invalid input = code %d called=%t stdout=%q stderr=%q", code, called, stdout.String(), stderr.String())
			}
		})

		t.Run("output "+test.name, func(t *testing.T) {
			restore := installCLIClient(t, &fakeCLIClient{call: func(_ context.Context, _ string, _ any, result any) error {
				result.(*protocol.OperationResult).Payload = append(json.RawMessage(nil), test.payload...)
				return nil
			}})
			t.Cleanup(restore)
			var stdout, stderr bytes.Buffer
			code := execute(t.Context(), []string{"app", "query", "codex", "inspect", "--socket", "/ignored"}, strings.NewReader(""), &stdout, &stderr)
			if code != exitOperational || stdout.Len() != 0 || strings.Contains(stderr.String(), "private") {
				t.Fatalf("invalid output = code %d stdout-bytes=%d stderr=%q", code, stdout.Len(), stderr.String())
			}
		})
	}

	t.Run("exact limit", func(t *testing.T) {
		atLimit := cliJSONObjectOfSize(t, protocol.MaxJSONObjectBytes)
		restore := installCLIClient(t, &fakeCLIClient{call: func(_ context.Context, _ string, params, result any) error {
			request := params.(control.PluginOperationRequest)
			if !bytes.Equal(request.Payload, atLimit) {
				t.Fatalf("request payload bytes = %d, want exact %d-byte object", len(request.Payload), len(atLimit))
			}
			result.(*protocol.OperationResult).Payload = append(json.RawMessage(nil), atLimit...)
			return nil
		}})
		t.Cleanup(restore)
		var stdout, stderr bytes.Buffer
		code := execute(t.Context(), []string{"app", "query", "codex", "inspect", "--file", "-", "--socket", "/ignored"}, bytes.NewReader(atLimit), &stdout, &stderr)
		if code != exitSuccess || stdout.String() != string(atLimit)+"\n" || stderr.Len() != 0 {
			t.Fatalf("exact-limit operation = code %d stdout-bytes %d stderr %q", code, stdout.Len(), stderr.String())
		}
	})
}

type unexpectedInputReader struct{ t *testing.T }

func (reader *unexpectedInputReader) Read([]byte) (int, error) {
	reader.t.Fatal("unexpected configuration read")
	return 0, errors.New("unexpected configuration read")
}
