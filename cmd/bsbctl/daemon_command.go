package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"

	"github.com/lxdb/bsbctl/internal/daemonrun"
)

func runDaemon(ctx context.Context, args []string, stderr io.Writer) error {
	options, positionals, err := parseOptions(args, "config", "socket", "log")
	if err != nil {
		return commandFailure(exitUsage, "invalid daemon flags")
	}
	if len(positionals) != 0 {
		return commandFailure(exitUsage, "daemon does not accept positional arguments")
	}
	configPath, err := resolveStatePath(options, "config", "config.json")
	if err != nil {
		return err
	}
	socketPath, err := resolveStatePath(options, "socket", "ctl.sock")
	if err != nil {
		return err
	}
	if logPath := options["log"]; logPath != "" && !filepath.IsAbs(logPath) {
		return commandFailure(exitUsage, "daemon log path must be absolute")
	}
	err = daemonrun.Run(ctx, daemonrun.Options{
		Version: version, ConfigPath: configPath, SocketPath: socketPath,
		LogPath: options["log"], Stderr: stderr,
	})
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	typed, ok := errors.AsType[*daemonrun.Error](err)
	if !ok {
		return err
	}
	switch typed.Kind {
	case daemonrun.ErrorInvalidInput:
		return commandFailure(exitUsage, typed.Message)
	case daemonrun.ErrorPartial:
		return commandFailure(exitPartial, typed.Message)
	default:
		return commandFailure(exitOperational, typed.Message)
	}
}
