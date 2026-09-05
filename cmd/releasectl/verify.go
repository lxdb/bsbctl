package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/internal/releasecheck"
)

const verificationOutputLimit = 4 << 20

var (
	verifyReleaseArtifacts = verifyDeterministicArtifacts
	verifyReleasePreflight = releasecheck.Check
)

func runVerify(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("verify")
	root := flags.String("root", ".", "repository root")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(*root) == "" {
		_, _ = fmt.Fprintln(stderr, "releasectl: invalid verify arguments")
		return exitFailure
	}
	report, err := verifyReleasePreflight(*root)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "release verification failed: preflight input is invalid")
		return exitFailure
	}
	if len(report.Findings) != 0 {
		writeReleaseFindings(stdout, report.Findings)
		_, _ = fmt.Fprintf(stdout, "release verification: blocked by %d finding(s)\n", len(report.Findings))
		return exitBlocked
	}
	_, _ = fmt.Fprintln(stdout, "verification: preflight passed")
	if err := verifyReleaseArtifacts(ctx, *root); err != nil {
		_, _ = fmt.Fprintf(stderr, "release verification failed: deterministic-artifacts: %v\n", err)
		return exitFailure
	}
	_, _ = fmt.Fprintln(stdout, "verification: deterministic artifacts passed")
	_, _ = fmt.Fprintln(stdout, "release verification: ready")
	return exitSuccess
}

func offlineEnvironment() []string {
	blocked := slices.Clone(ambientBuildVariables)
	blocked = append(blocked, "CGO_ENABLED", "GOARCH", "GOFLAGS", "GOOS")
	return isolatedGoEnvironment(blocked...)
}

type boundedOutput struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	original := len(data)
	if len(data) > output.remaining {
		data = data[:output.remaining]
		output.truncated = true
	}
	_, _ = output.buffer.Write(data)
	output.remaining -= len(data)
	return original, nil
}

func (output *boundedOutput) String() string {
	if output.truncated {
		return output.buffer.String() + "\n[output truncated]"
	}
	return output.buffer.String()
}

func commandError(commandErr error, output string) error {
	output = strings.TrimSpace(output)
	if output == "" {
		return commandErr
	}
	return fmt.Errorf("%w: %s", commandErr, output)
}

func verifyDeterministicArtifacts(ctx context.Context, root string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("supported release artifact verification requires Darwin")
	}
	temporary, err := os.MkdirTemp("", "bsbctl-release-verification-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	epoch := time.Unix(1700000000, 0).UTC()
	for _, architecture := range []string{"arm64", "amd64"} {
		first := filepath.Join(temporary, architecture+"-first")
		second := filepath.Join(temporary, architecture+"-second")
		if _, err := packageComponents(ctx, root, first, "darwin", architecture, epoch); err != nil {
			return fmt.Errorf("%s first package: %w", architecture, err)
		}
		if _, err := packageComponents(ctx, root, second, "darwin", architecture, epoch); err != nil {
			return fmt.Errorf("%s second package: %w", architecture, err)
		}
		if err := verifyArtifactDirectory(first); err != nil {
			return fmt.Errorf("%s archive inspection: %w", architecture, err)
		}
		firstDigests, err := directoryDigests(first)
		if err != nil {
			return err
		}
		secondDigests, err := directoryDigests(second)
		if err != nil {
			return err
		}
		if !equalDigests(firstDigests, secondDigests) {
			return fmt.Errorf("%s package rebuild differs: %s", architecture, digestDifferences(firstDigests, secondDigests))
		}
	}
	return nil
}

func directoryDigests(directory string) (map[string][sha256.Size]byte, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	digests := make(map[string][sha256.Size]byte, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil, errors.New("release output contains a non-regular file")
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		digests[entry.Name()] = sha256.Sum256(data)
	}
	return digests, nil
}

func equalDigests(first, second map[string][sha256.Size]byte) bool {
	if len(first) != len(second) {
		return false
	}
	for name, digest := range first {
		if second[name] != digest {
			return false
		}
	}
	return true
}

func digestDifferences(first, second map[string][sha256.Size]byte) string {
	names := make(map[string]struct{}, len(first)+len(second))
	for name := range first {
		names[name] = struct{}{}
	}
	for name := range second {
		names[name] = struct{}{}
	}
	ordered := slices.Sorted(maps.Keys(names))
	parts := make([]string, 0, len(ordered))
	for _, name := range ordered {
		firstDigest, firstExists := first[name]
		secondDigest, secondExists := second[name]
		if firstExists && secondExists && firstDigest == secondDigest {
			continue
		}
		firstValue, secondValue := "missing", "missing"
		if firstExists {
			firstValue = fmt.Sprintf("%x", firstDigest)
		}
		if secondExists {
			secondValue = fmt.Sprintf("%x", secondDigest)
		}
		parts = append(parts, fmt.Sprintf("%s first=%s second=%s", name, firstValue, secondValue))
	}
	return strings.Join(parts, "; ")
}

func writeReleaseFindings(output io.Writer, findings []releasecheck.Finding) {
	for _, finding := range findings {
		_, _ = fmt.Fprintf(output, "BLOCKER %s: %s\n", finding.ID, finding.Message)
		_, _ = fmt.Fprintf(output, "ACTION %s: %s\n", finding.ID, finding.OperatorAction)
	}
}
