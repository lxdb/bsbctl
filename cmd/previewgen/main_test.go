//go:build preview

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCaptureGeneratesFixturesChecksumsAndPublishableArtifactsInOneCommand(t *testing.T) {
	fixtureDirectory := filepath.Join(t.TempDir(), "fixtures")
	output := filepath.Join(t.TempDir(), "previews")
	var stdout, stderr bytes.Buffer
	digest := strings.TrimSpace(calendarFixtureSHA256)

	err := runWithDependencies(
		t.Context(),
		[]string{"--capture", "--fixtures", fixtureDirectory, "--out", output},
		&stdout,
		&stderr,
		runDependencies{capture: func(context.Context, options) ([]mockFixture, error) {
			return capturedPreviewCopies(digest), nil
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	for _, name := range capturedPreviewArtifactNames {
		content, err := os.ReadFile(filepath.Join(fixtureDirectory, name))
		if err != nil || !bytes.Equal(content, calendarFixtureGIF) {
			t.Fatalf("captured fixture %s: content matches=%v err=%v", name, bytes.Equal(content, calendarFixtureGIF), err)
		}
		checksum, err := os.ReadFile(filepath.Join(fixtureDirectory, name+".sha256"))
		if err != nil || string(checksum) != digest+"\n" {
			t.Fatalf("captured checksum %s = %q, err=%v", name, checksum, err)
		}
	}
	for _, name := range previewArtifactNames {
		if info, err := os.Stat(filepath.Join(output, name)); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("artifact %s: info=%v err=%v", name, info, err)
		}
	}
}

func TestRunCaptureDoesNotReplaceFixturesWhenPublicationValidationFails(t *testing.T) {
	fixtureDirectory := t.TempDir()
	output := filepath.Join(t.TempDir(), "previews")
	calendarPath := filepath.Join(fixtureDirectory, "calendar-front.gif")
	if err := os.WriteFile(calendarPath, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	captured := capturedPreviewCopies(strings.TrimSpace(calendarFixtureSHA256))
	err := runWithDependencies(
		t.Context(),
		[]string{"--capture", "--fixtures", fixtureDirectory, "--out", output},
		&stdout,
		&stderr,
		runDependencies{
			capture: func(context.Context, options) ([]mockFixture, error) { return captured, nil },
			generate: func([]mockFixture) (map[string][]byte, error) {
				return map[string][]byte{
					"calendar-front.gif":      []byte("calendar"),
					"codex-front.gif":         make([]byte, maximumPreviewBytes+1),
					"codex-quota-front.gif":   []byte("quota"),
					"mac-resources-front.gif": []byte("resources"),
				}, nil
			},
		},
	)
	if err == nil {
		t.Fatal("oversized publication set was accepted")
	}
	content, readErr := os.ReadFile(calendarPath)
	if readErr != nil || string(content) != "existing" {
		t.Fatalf("fixture after failed publication = %q, err=%v", content, readErr)
	}
}

func TestRunCaptureRollsBackFixturesWhenArtifactPublicationFails(t *testing.T) {
	fixtureDirectory := t.TempDir()
	calendarPath := filepath.Join(fixtureDirectory, "calendar-front.gif")
	if err := os.WriteFile(calendarPath, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(output, []byte("blocked"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimSpace(calendarFixtureSHA256)
	captured := capturedPreviewCopies(digest)
	var stdout, stderr bytes.Buffer

	err := runWithDependencies(
		t.Context(),
		[]string{"--capture", "--fixtures", fixtureDirectory, "--out", output},
		&stdout,
		&stderr,
		runDependencies{capture: func(context.Context, options) ([]mockFixture, error) { return captured, nil }},
	)
	if err == nil {
		t.Fatal("artifact output failure was accepted")
	}
	content, readErr := os.ReadFile(calendarPath)
	if readErr != nil || string(content) != "existing" {
		t.Fatalf("fixture after artifact output failure = %q, err=%v; want original content", content, readErr)
	}
}

func capturedPreviewCopies(digest string) []mockFixture {
	fixtures := make([]mockFixture, 0, len(capturedPreviewArtifactNames))
	for _, name := range capturedPreviewArtifactNames {
		fixtures = append(fixtures, mockFixture{name: name, format: "gif", data: calendarFixtureGIF, sha256: digest})
	}
	return fixtures
}

func TestRunGeneratesReviewedMockArtifactsWithoutReadingLiveConfiguration(t *testing.T) {
	output := filepath.Join(t.TempDir(), "previews")
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer

	if err := run(t.Context(), []string{"--out", output}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	for _, name := range previewArtifactNames {
		if info, err := os.Stat(filepath.Join(output, name)); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("artifact %s: info=%v err=%v", name, info, err)
		}
	}
}
