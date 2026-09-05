package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/launchagent"
	"github.com/lxdb/bsbctl/sdk/rpc"
)

type controlClient interface {
	Call(context.Context, string, any, any) error
	Close() error
}

var dialControl = func(ctx context.Context, path string) (controlClient, error) { return control.Dial(ctx, path) }

func parseOptions(args []string, allowed ...string) (map[string]string, []string, error) {
	known := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		known[name] = struct{}{}
	}
	options := make(map[string]string, len(allowed))
	positionals := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			positionals = append(positionals, arg)
			continue
		}
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			return nil, nil, errors.New("invalid flag")
		}
		nameValue := strings.TrimPrefix(arg, "--")
		name, value, hasValue := strings.Cut(nameValue, "=")
		if _, exists := known[name]; !exists || name == "" {
			return nil, nil, errors.New("unknown flag")
		}
		if _, duplicate := options[name]; duplicate {
			return nil, nil, errors.New("duplicate flag")
		}
		if !hasValue {
			index++
			if index >= len(args) || strings.HasPrefix(args[index], "--") {
				return nil, nil, errors.New("flag value is required")
			}
			value = args[index]
		}
		if value == "" {
			return nil, nil, errors.New("flag value is required")
		}
		options[name] = value
	}
	return options, positionals, nil
}

func optionDefault(options map[string]string, name, fallback string) string {
	if value := options[name]; value != "" {
		return value
	}
	return fallback
}

func callDaemon(ctx context.Context, socketPath, method string, params, result any) error {
	_, err := callDaemonResult(ctx, socketPath, method, params, result)
	return err
}

func callDaemonResult(ctx context.Context, socketPath, method string, params, result any) (bool, error) {
	client, err := dialControl(ctx, socketPath)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, commandFailure(exitOperational, "daemon is unavailable")
	}
	callErr := client.Call(ctx, method, params, result)
	closeErr := client.Close()
	if callErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		if rpcErr, ok := errors.AsType[*rpc.Error](callErr); ok {
			switch rpcErr.Code {
			case -32602:
				return false, commandFailure(exitUsage, "daemon rejected invalid input")
			case -32046, -32051, -32052, -32054:
				return false, commandFailure(exitRejected, "daemon rejected the operation")
			}
		}
		return false, commandFailure(exitOperational, "daemon operation failed")
	}
	if closeErr != nil {
		return true, nil
	}
	return false, nil
}

func writeJSON(writer io.Writer, value any) error {
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		return commandFailure(exitOperational, "write output failed")
	}
	return nil
}

func resolveStatePath(options map[string]string, option, name string) (string, error) {
	if value := options[option]; value != "" {
		return value, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", commandFailure(exitOperational, "resolve default state directory failed")
	}
	return filepath.Join(home, ".bsbctl", name), nil
}

func defaultLaunchAgentPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return launchagent.Label + ".plist"
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchagent.Label+".plist")
}
