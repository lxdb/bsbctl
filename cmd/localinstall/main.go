// localinstall is the checkout-only implementation of install.sh --local.
// It is not included in release packages or the installed CLI command tree.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
)

func main() { os.Exit(runLocalInstaller()) }

func runLocalInstaller() int {
	flags := flag.NewFlagSet("install.sh --local", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	apps := flags.String("apps", "", "also install these first-party apps; none builds core only")
	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "local installer does not accept positional arguments")
		return 2
	}
	if _, err := explicitLocalApps(*apps); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(os.Stderr, "local installation supports macOS only")
		return 2
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		fmt.Fprintln(os.Stderr, "an absolute home directory is required")
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	service := newLocalService(home)
	result, err := installLocal(ctx, home, *apps, os.Stderr, installDependencies{
		build: func(ctx context.Context, stage string, selected []firstpartyplugins.Descriptor) (config.Document, string, error) {
			return buildLocalPackages(ctx, root, stage, selected, os.Stderr)
		},
		inspect: service.inspect, stop: service.stop, start: service.start, waitReady: service.waitReady,
	})
	if result.Directory != "" {
		fmt.Fprintln(os.Stderr, "Local build and recovery files:", result.Directory)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bsbctl local installer:", err)
		if result.Installed {
			return 6
		}
		if ctx.Err() != nil {
			return 5
		}
		return 4
	}
	message := "Installed local build. Service remains stopped."
	if result.Running {
		message = "Installed local build. Service restarted; daemon, enabled apps, and device are ready."
	}
	if _, err := fmt.Fprintln(os.Stdout, message); err != nil {
		return 6
	}
	return 0
}
