package daemonrun

import (
	"context"
	"errors"
	"io"
	"testing"
)

func TestRunRejectsIncompleteCommandInputsBeforeConstruction(t *testing.T) {
	valid := Options{Version: "test", ConfigPath: "/tmp/config.json", SocketPath: "/tmp/control.sock", Stderr: io.Discard}
	tests := []struct {
		name    string
		ctx     context.Context
		options Options
		message string
	}{
		{name: "context", options: valid, message: "daemon context is required"},
		{name: "version", ctx: t.Context(), options: Options{ConfigPath: valid.ConfigPath, SocketPath: valid.SocketPath, Stderr: io.Discard}, message: "daemon version is required"},
		{name: "configuration", ctx: t.Context(), options: Options{Version: valid.Version, SocketPath: valid.SocketPath, Stderr: io.Discard}, message: "daemon configuration path is required"},
		{name: "socket", ctx: t.Context(), options: Options{Version: valid.Version, ConfigPath: valid.ConfigPath, Stderr: io.Discard}, message: "daemon socket path is required"},
		{name: "diagnostics", ctx: t.Context(), options: Options{Version: valid.Version, ConfigPath: valid.ConfigPath, SocketPath: valid.SocketPath}, message: "daemon diagnostic stream is required"},
		{name: "relative log", ctx: t.Context(), options: Options{Version: valid.Version, ConfigPath: valid.ConfigPath, SocketPath: valid.SocketPath, LogPath: "daemon.jsonl", Stderr: io.Discard}, message: "daemon log path must be absolute"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := run(test.ctx, test.options, dependencies{})
			typed, ok := errors.AsType[*Error](err)
			if !ok || typed.Kind != ErrorInvalidInput || typed.Message != test.message {
				t.Fatalf("run error = %#v, want invalid input %q", err, test.message)
			}
		})
	}
}

func TestErrorRendersOnlyStableMessageAndRetainsCause(t *testing.T) {
	cause := errors.New("secret=/private/sensitive-value")
	err := failure(ErrorOperational, "daemon startup failed", cause)
	if got := err.Error(); got != "daemon startup failed" {
		t.Fatalf("rendered error = %q", got)
	}
	if !errors.Is(err, cause) {
		t.Fatal("daemon error did not retain its diagnostic cause")
	}
}
