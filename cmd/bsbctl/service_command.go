package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/lxdb/bsbctl/internal/launchagent"
)

type serviceManager interface {
	Install(context.Context, string, launchagent.Config) (launchagent.Result, error)
	Restart(context.Context, string) (launchagent.Result, error)
	Uninstall(context.Context, string) (launchagent.Result, error)
	Status(context.Context, string) (launchagent.Result, error)
}

var newServiceManager = func() serviceManager { return launchagent.NewManager(nil, os.Getuid()) }

func runService(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return commandFailure(exitUsage, "service command requires install, restart, uninstall, or status")
	}
	command := args[0]
	allowed := []string{"plist"}
	if command == "install" {
		allowed = append(allowed, "config", "socket", "log", "stdout-path", "stderr-path")
	}
	options, positionals, err := parseOptions(args[1:], allowed...)
	if err != nil || len(positionals) != 0 {
		return commandFailure(exitUsage, "invalid service arguments")
	}
	plistPath := optionDefault(options, "plist", defaultLaunchAgentPath())
	manager := newServiceManager()
	var result launchagent.Result
	switch command {
	case "install":
		configPath, pathErr := resolveStatePath(options, "config", "config.json")
		if pathErr != nil {
			return pathErr
		}
		socketPath, pathErr := resolveStatePath(options, "socket", "ctl.sock")
		if pathErr != nil {
			return pathErr
		}
		stdoutPath, stderrPath := options["stdout-path"], options["stderr-path"]
		logPath := options["log"]
		if !filepath.IsAbs(configPath) || !filepath.IsAbs(socketPath) || !filepath.IsAbs(plistPath) || (logPath != "" && !filepath.IsAbs(logPath)) || (stdoutPath != "" && !filepath.IsAbs(stdoutPath)) || (stderrPath != "" && !filepath.IsAbs(stderrPath)) {
			return commandFailure(exitUsage, "service paths must be absolute")
		}
		executable, executableErr := os.Executable()
		if executableErr != nil {
			return commandFailure(exitOperational, "resolve executable failed")
		}
		executable, executableErr = filepath.EvalSymlinks(executable)
		if executableErr != nil {
			return commandFailure(exitOperational, "resolve executable failed")
		}
		result, err = manager.Install(ctx, plistPath, launchagent.Config{
			Executable: executable, ConfigPath: configPath, SocketPath: socketPath, LogPath: logPath,
			StdoutPath: stdoutPath, StderrPath: stderrPath,
		})
	case "uninstall":
		if !filepath.IsAbs(plistPath) {
			return commandFailure(exitUsage, "service plist path must be absolute")
		}
		result, err = manager.Uninstall(ctx, plistPath)
	case "restart":
		if !filepath.IsAbs(plistPath) {
			return commandFailure(exitUsage, "service plist path must be absolute")
		}
		result, err = manager.Restart(ctx, plistPath)
	case "status":
		if !filepath.IsAbs(plistPath) {
			return commandFailure(exitUsage, "service plist path must be absolute")
		}
		result, err = manager.Status(ctx, plistPath)
	default:
		return commandFailure(exitUsage, "invalid service command")
	}
	if err != nil && !errors.Is(err, launchagent.ErrPartial) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return commandFailure(exitOperational, "service operation failed")
	}
	if writeErr := writeJSON(stdout, result); writeErr != nil {
		return writeErr
	}
	if errors.Is(err, launchagent.ErrPartial) {
		return commandFailure(exitPartial, "service operation partially completed")
	}
	return nil
}
