package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/lxdb/bsbctl/internal/cliinput"
)

var version = "0.1.0-dev"

const (
	exitSuccess     = 0
	exitUsage       = 2
	exitRejected    = 3
	exitOperational = 4
	exitCanceled    = 5
	exitPartial     = 6
)

type commandError struct {
	code    int
	message string
}

func (err *commandError) Error() string { return err.message }

func commandFailure(code int, message string) error {
	return &commandError{code: code, message: message}
}

func main() {
	os.Exit(realMain())
}

func realMain() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return executeProcess(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// The process owns stdin. Library-style execute callers retain their readers.
func executeProcess(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) (code int) {
	input := cliinput.New(ctx, stdin)
	defer func() {
		if err := input.Close(); err != nil {
			_, _ = fmt.Fprintln(stderr, "bsbctl: restore standard input failed")
			if code == exitSuccess {
				code = exitOperational
			}
		}
	}()
	return execute(ctx, args, input, stdout, stderr)
}

func execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	err := run(ctx, args, stdin, stdout, stderr)
	if err == nil {
		return exitSuccess
	}
	code, message := classifyCommandError(err)
	if ctx.Err() != nil && code != exitPartial {
		code, message = classifyCommandError(ctx.Err())
	}
	if message != "" {
		_, _ = fmt.Fprintln(stderr, "bsbctl:", message)
	}
	return code
}

func classifyCommandError(err error) (int, string) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return exitCanceled, "operation canceled"
	}
	if typed, ok := errors.AsType[*commandError](err); ok {
		return typed.code, typed.message
	}
	return exitOperational, "operation failed"
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return commandFailure(exitUsage, "command is required; run bsbctl help")
	}
	switch args[0] {
	case "daemon":
		return runDaemon(ctx, args[1:], stderr)
	case "setup":
		return runSetup(ctx, args[1:], stdin, stdout, stderr)
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "status":
		return callStatus(ctx, args[1:], stdout, stderr)
	case "plugin":
		return runPlugin(ctx, args[1:], stdin, stdout, stderr)
	case "app":
		return runApp(ctx, args[1:], stdin, stdout, stderr)
	case "attention":
		return runAttention(ctx, args[1:], stdout, stderr)
	case "device":
		return runDevice(ctx, args[1:], stdout)
	case "service":
		return runService(ctx, args[1:], stdout, stderr)
	case "version", "--version":
		if len(args) != 1 {
			return commandFailure(exitUsage, "version does not accept arguments")
		}
		if _, err := fmt.Fprintln(stdout, version); err != nil {
			return commandFailure(exitOperational, "write output failed")
		}
		return nil
	case "help", "--help", "-h":
		if len(args) != 1 {
			return commandFailure(exitUsage, "help does not accept arguments")
		}
		return usage(stdout)
	default:
		return commandFailure(exitUsage, "invalid command; run bsbctl help")
	}
}

func usage(out io.Writer) error {
	_, err := fmt.Fprintln(out, `usage: bsbctl <command> [options]

Core commands:
  bsbctl setup [--apps APP-ID,...|none] [--device-url URL] [--device-bootstrap-keychain REF] [--device-token-keychain REF]
  bsbctl init [--plugin /absolute/path]... [--device-url URL] [--device-token-keychain keychain://service/account]
  bsbctl daemon [--config PATH] [--socket PATH] [--log ABSOLUTE_PATH]
  bsbctl status [--socket PATH]
  bsbctl version

Apps and plugins:
  bsbctl app list [--socket PATH]
  bsbctl app status <app-id> [--socket PATH]
  bsbctl app enable <app-id> [--socket PATH]
  bsbctl app disable <app-id> [--socket PATH]
  bsbctl app create <built-in-app-id> [--enabled true|false] [--socket PATH]
  bsbctl app create <app-id> --plugin PLUGIN-ID --file PATH|- [--enabled true|false] [--socket PATH]
  bsbctl app delete <app-id> [--socket PATH]
  bsbctl app config <app-id> --file PATH|- [--socket PATH]
  bsbctl app setup <app-id> --file CONFIG [--token-stdin]
  bsbctl app launch <app-id> [action] [--socket PATH]
  bsbctl app query <app-id> <operation> [--file PATH|-] [--socket PATH]
  bsbctl app command <app-id> <operation> [--file PATH|-] [--socket PATH]
  bsbctl plugin list [--socket PATH]
  bsbctl plugin status <plugin-id> [--socket PATH]
  bsbctl plugin install <plugin-id> --catalog FILE --signature FILE --version VERSION [--socket PATH]
  bsbctl plugin update <plugin-id> --catalog FILE --signature FILE --version VERSION [--socket PATH]
  bsbctl plugin rollback <plugin-id> [--version VERSION] [--socket PATH]
  bsbctl plugin verify --manifest PATH --fixture PATH [--executable PATH]

Attention:
  bsbctl attention status [--socket PATH]
  bsbctl attention explain <observation-id> [--socket PATH]
  bsbctl attention acknowledge <observation-id> [--socket PATH]
  bsbctl attention history [--limit N] [--since DURATION] [--socket PATH]

Device:
  bsbctl device screenshot [--display front|back|both] [--count N] [--interval-ms N] [--out DIR] [--config PATH]
    Defaults: --display both --count 1 --interval-ms 500 --config ~/.bsbctl/config.json
    --count must not exceed 1000.
    Sequences require --interval-ms 500 or greater.
    Without --out, creates a unique /tmp/bsbctl-screenshot-* directory.

Service:
  bsbctl service install [--config PATH] [--socket PATH] [--plist PATH] [--log ABSOLUTE_PATH] [--stdout-path PATH] [--stderr-path PATH]
  bsbctl service restart [--plist PATH]
  bsbctl service uninstall [--plist PATH]
  bsbctl service status [--plist PATH]

Exit codes:
  0  Success
  2  Invalid usage or input
  3  Valid request rejected
  4  Operational dependency or activation failure
  5  Cancellation or deadline
  6  Partial result or recovery required`)
	if err != nil {
		return commandFailure(exitOperational, "write output failed")
	}
	return nil
}
