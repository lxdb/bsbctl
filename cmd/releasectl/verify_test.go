package main

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/lxdb/bsbctl/internal/releasecheck"
)

func TestVerifyDeterministicArtifactsProductionBuilds(t *testing.T) {
	if os.Getenv("BSBCTL_RUN_RELEASE_ARTIFACT_INTEGRATION") != "1" {
		t.Skip("set BSBCTL_RUN_RELEASE_ARTIFACT_INTEGRATION=1 for real dual-architecture builds")
	}
	if err := verifyDeterministicArtifacts(context.Background(), "../.."); err != nil {
		t.Fatal(err)
	}
}

func TestRunVerifyReportsPreflightBlockersBeforeArtifacts(t *testing.T) {
	previousArtifacts := verifyReleaseArtifacts
	previousPreflight := verifyReleasePreflight
	t.Cleanup(func() {
		verifyReleaseArtifacts = previousArtifacts
		verifyReleasePreflight = previousPreflight
	})

	artifactCalls := 0
	verifyReleaseArtifacts = func(context.Context, string) error {
		artifactCalls++
		return nil
	}
	verifyReleasePreflight = func(string) (releasecheck.Report, error) {
		return releasecheck.Report{Findings: []releasecheck.Finding{
			{ID: "legal", Message: "permission missing", OperatorAction: "obtain permission"},
			{ID: "key", Message: "key missing", OperatorAction: "provision key"},
		}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := runWithContext(t.Context(), []string{"verify", "--root", "."}, &stdout, &stderr)
	if code != exitBlocked || stderr.Len() != 0 {
		t.Fatalf("verify exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if artifactCalls != 0 {
		t.Fatalf("artifact verification calls = %d, want 0", artifactCalls)
	}
	output := stdout.String()
	for _, fragment := range []string{
		"BLOCKER legal: permission missing\n",
		"ACTION legal: obtain permission\n",
		"BLOCKER key: key missing\n",
		"release verification: blocked by 2 finding(s)\n",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("stdout %q does not contain %q", output, fragment)
		}
	}
}

func TestRunVerifyRunsPreflightBeforeDeterministicArtifacts(t *testing.T) {
	previousArtifacts := verifyReleaseArtifacts
	previousPreflight := verifyReleasePreflight
	t.Cleanup(func() {
		verifyReleaseArtifacts = previousArtifacts
		verifyReleasePreflight = previousPreflight
	})

	var events []string
	verifyReleasePreflight = func(string) (releasecheck.Report, error) {
		events = append(events, "preflight")
		return releasecheck.Report{}, nil
	}
	verifyReleaseArtifacts = func(context.Context, string) error {
		events = append(events, "artifacts")
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := runWithContext(t.Context(), []string{"verify", "--root", "."}, &stdout, &stderr)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("verify exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !reflect.DeepEqual(events, []string{"preflight", "artifacts"}) {
		t.Fatalf("events = %v, want preflight before artifacts", events)
	}
	for _, fragment := range []string{
		"verification: preflight passed\n",
		"verification: deterministic artifacts passed\n",
		"release verification: ready\n",
	} {
		if !strings.Contains(stdout.String(), fragment) {
			t.Fatalf("stdout %q does not contain %q", stdout.String(), fragment)
		}
	}
}
