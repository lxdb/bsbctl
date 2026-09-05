//go:build preview

// Command previewgen regenerates README previews from reviewed mock-only
// framebuffer fixtures. Its explicit capture mode renders the same mock-only
// production scenes through the configured BUSY Bar before regeneration.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

type options struct {
	Output   string
	Fixtures string
	Config   string
	Capture  bool
}

type runDependencies struct {
	capture  func(context.Context, options) ([]mockFixture, error)
	generate func([]mockFixture) (map[string][]byte, error)
}

func main() {
	os.Exit(realMain())
}

func realMain() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runWithDependencies(ctx, args, stdout, stderr, runDependencies{
		capture:  captureDeviceFixtures,
		generate: generateFixtureArtifactsFrom,
	})
}

func runWithDependencies(ctx context.Context, args []string, stdout, stderr io.Writer, dependencies runDependencies) error {
	options, err := parseOptions(args, stderr)
	if err != nil {
		return err
	}
	fixtures := reviewedMockFixtures
	var captured []mockFixture
	if options.Capture {
		if dependencies.capture == nil {
			return errors.New("preview capture is unavailable")
		}
		captured, err = dependencies.capture(ctx, options)
		if err != nil {
			return fmt.Errorf("capture preview fixtures: %w", err)
		}
		fixtures, err = mergeCapturedFixtures(captured)
		if err != nil {
			return err
		}
	}
	generate := dependencies.generate
	if generate == nil {
		generate = generateFixtureArtifactsFrom
	}
	artifacts, err := generate(fixtures)
	if err != nil {
		return err
	}
	if err := validateArtifacts(artifacts); err != nil {
		return fmt.Errorf("validate preview artifacts: %w", err)
	}
	if options.Capture {
		if err := writeCaptureOutputs(options.Fixtures, captured, options.Output, artifacts); err != nil {
			return fmt.Errorf("write captured preview fixtures and artifacts: %w", err)
		}
	} else if err := writeArtifacts(options.Output, artifacts); err != nil {
		return fmt.Errorf("write preview artifacts: %w", err)
	}
	for _, name := range previewArtifactNames {
		fmt.Fprintln(stdout, filepath.Join(options.Output, name))
	}
	return nil
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	result := options{Output: "docs/previews", Fixtures: "cmd/previewgen/fixtures"}
	flags := flag.NewFlagSet("previewgen", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&result.Output, "out", result.Output, "preview output directory")
	flags.StringVar(&result.Fixtures, "fixtures", result.Fixtures, "reviewed framebuffer fixture directory")
	flags.StringVar(&result.Config, "config", "", "bsbctl configuration path used only with --capture")
	flags.BoolVar(&result.Capture, "capture", false, "render and capture the feature previews from the configured BUSY Bar")
	if err := flags.Parse(args); err != nil {
		return options{}, errors.New("invalid preview generator flags")
	}
	if flags.NArg() != 0 || result.Output == "" || result.Fixtures == "" {
		return options{}, errors.New("preview output and fixture paths must be non-empty and no positional arguments are accepted")
	}
	return result, nil
}
