// Command releasectl builds, verifies, and publishes release artifacts.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/lxdb/bsbctl/internal/cliinput"
	"github.com/lxdb/bsbctl/internal/releasecheck"
)

const (
	exitSuccess = 0
	exitBlocked = 1
	exitFailure = 2
)

func main() {
	os.Exit(realMain())
}

func realMain() (code int) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	input := cliinput.New(ctx, os.Stdin)
	defer func() {
		if err := input.Close(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "releasectl: restore standard input failed")
			if code == exitSuccess {
				code = exitFailure
			}
		}
	}()
	return runWithInput(ctx, os.Args[1:], input, os.Stdout, os.Stderr)
}

func runWithInput(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "releasectl: command is required")
		return exitFailure
	}
	switch args[0] {
	case "help", "--help", "-h":
		if len(args) != 1 {
			_, _ = fmt.Fprintln(stderr, "releasectl: help does not accept arguments")
			return exitFailure
		}
		return writeUsage(stdout)
	case "preflight":
		return runPreflight(args[1:], stdout, stderr)
	case "package":
		return runPackage(ctx, args[1:], stdout, stderr)
	case "inspect":
		return runInspect(args[1:], stdout, stderr)
	case "verify":
		return runVerify(ctx, args[1:], stdout, stderr)
	case "sign-catalog":
		return runSignCatalog(args[1:], stdin, stdout, stderr)
	case "verify-catalog":
		return runVerifyCatalog(args[1:], stdout, stderr)
	case "publish-releases":
		return runPublishReleases(ctx, args[1:], stdout, stderr)
	case "catalog":
		return runCatalog(args[1:], stdout, stderr)
	case "verify-tags":
		return runVerifyTags(args[1:], stdout, stderr)
	case "release-tags":
		return runReleaseTags(args[1:], stdout, stderr)
	case "soak":
		return runSoak(ctx, args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintln(stderr, "releasectl: unsupported command")
		return exitFailure
	}
}

func writeUsage(out io.Writer) int {
	_, err := fmt.Fprintln(out, `usage: releasectl <command> [options]

Commands:
  preflight         Report release blockers
  package           Build deterministic component archives
  inspect           Inspect an artifact directory
  verify            Verify release inputs and artifacts
  sign-catalog      Sign a catalog from standard input
  verify-catalog    Verify a catalog signature
  publish-releases  Reconcile and publish protected releases
  catalog           Build a release catalog
  verify-tags       Verify release-tag bindings
  release-tags      Print validated release tag refspecs
  soak              Run the device-free performance soak

Exit codes:
  0  Success
  1  Release preflight blocked
  2  Invalid input or operation failure`)
	if err != nil {
		return exitFailure
	}
	return exitSuccess
}

func newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func runPreflight(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("preflight")
	root := flags.String("root", ".", "repository root")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "releasectl: invalid preflight arguments")
		return exitFailure
	}
	report, err := releasecheck.Check(*root)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "releasectl: release preflight input is invalid")
		return exitFailure
	}
	if len(report.Findings) == 0 {
		_, _ = fmt.Fprintln(stdout, "release preflight: ready")
		return exitSuccess
	}
	writeReleaseFindings(stdout, report.Findings)
	_, _ = fmt.Fprintf(stdout, "release preflight: blocked by %d finding(s)\n", len(report.Findings))
	return exitBlocked
}
