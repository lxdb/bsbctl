package main

import (
	"context"
	"encoding/json"
	"github.com/lxdb/bsbctl/internal/launchagent"
	"strings"
	"testing"
)

func cliJSONObjectOfSize(t *testing.T, size int) json.RawMessage {
	t.Helper()
	const shell = `{"x":""}`
	if size < len(shell) {
		t.Fatalf("JSON object size %d is smaller than minimum %d", size, len(shell))
	}
	value := json.RawMessage(`{"x":"` + strings.Repeat("x", size-len(shell)) + `"}`)
	if len(value) != size || !json.Valid(value) {
		t.Fatalf("invalid JSON object fixture: %d bytes", len(value))
	}
	return value
}

type fakeCLIClient struct {
	call     func(context.Context, string, any, any) error
	closeErr error
}

func (c *fakeCLIClient) Call(ctx context.Context, method string, params, result any) error {
	return c.call(ctx, method, params, result)
}

func (c *fakeCLIClient) Close() error { return c.closeErr }

func installCLIClient(t *testing.T, client controlClient) func() {
	t.Helper()
	previous := dialControl
	dialControl = func(context.Context, string) (controlClient, error) { return client, nil }
	return func() { dialControl = previous }
}

type fakeServiceManager struct {
	installResult launchagent.Result
	installErr    error
	installConfig launchagent.Config
	restartResult launchagent.Result
	restartErr    error
}

func (m *fakeServiceManager) Install(_ context.Context, _ string, config launchagent.Config) (launchagent.Result, error) {
	m.installConfig = config
	return m.installResult, m.installErr
}

func (*fakeServiceManager) Uninstall(context.Context, string) (launchagent.Result, error) {
	return launchagent.Result{Status: launchagent.StateNotInstalled}, nil
}

func (*fakeServiceManager) Status(context.Context, string) (launchagent.Result, error) {
	return launchagent.Result{Status: launchagent.StateNotInstalled}, nil
}

func (m *fakeServiceManager) Restart(context.Context, string) (launchagent.Result, error) {
	return m.restartResult, m.restartErr
}

func installServiceManager(t *testing.T, manager serviceManager) func() {
	t.Helper()
	previous := newServiceManager
	newServiceManager = func() serviceManager { return manager }
	return func() { newServiceManager = previous }
}
