//go:build preview

package main

import (
	"bytes"
	"errors"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewedGIFFixturesDecodeToOpaqueFramebuffers(t *testing.T) {
	t.Parallel()
	for _, fixture := range reviewedMockFixtures {
		if fixture.format != "gif" {
			continue
		}
		frames, err := decodeMockGIF(fixture.data)
		if err != nil {
			t.Fatalf("decode %s: %v", fixture.name, err)
		}
		for frameIndex, frame := range frames {
			for y := range frame.Image.Bounds().Dy() {
				for x := range frame.Image.Bounds().Dx() {
					if alpha := color.RGBAModel.Convert(frame.Image.At(x, y)).(color.RGBA).A; alpha != 0xff {
						t.Fatalf("%s frame %d pixel (%d,%d) alpha = %d, want opaque framebuffer", fixture.name, frameIndex, x, y, alpha)
					}
				}
			}
		}
	}
}

func TestReviewedMockFrameFixturesGenerateEveryPublishableArtifact(t *testing.T) {
	t.Parallel()
	artifacts, err := generateFixtureArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != len(previewArtifactNames) {
		t.Fatalf("artifacts = %d, want %d", len(artifacts), len(previewArtifactNames))
	}
	for _, name := range []string{"calendar-front.gif", "codex-front.gif", "codex-quota-front.gif", "mac-resources-front.gif"} {
		if len(artifacts[name]) > maximumPreviewBytes {
			t.Fatalf("%s bytes = %d, want at most %d", name, len(artifacts[name]), maximumPreviewBytes)
		}
		animation, err := gif.DecodeAll(bytes.NewReader(artifacts[name]))
		if err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if animation.Config.Width != 768 || animation.Config.Height != 248 || animation.LoopCount != 0 || len(animation.Image) < 2 {
			t.Fatalf("%s has invalid publishable animation metadata", name)
		}
		if name == "codex-quota-front.gif" {
			duration := 0
			for _, delay := range animation.Delay {
				duration += delay
			}
			if len(animation.Image) != 2 || duration != 400 {
				t.Fatalf("%s frames/duration = %d/%dcs, want two 2s quota states", name, len(animation.Image), duration)
			}
		}
	}
}

func TestWriteArtifactsDoesNotReplaceFilesWhenTheSetIsIncomplete(t *testing.T) {
	t.Parallel()
	output := t.TempDir()
	calendarPath := filepath.Join(output, "calendar-front.gif")
	if err := os.WriteFile(calendarPath, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := writeArtifacts(output, map[string][]byte{
		"calendar-front.gif": []byte("replacement"),
	})
	if err == nil {
		t.Fatal("incomplete artifact set was accepted")
	}
	content, readErr := os.ReadFile(calendarPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(content) != "existing" {
		t.Fatalf("existing artifact = %q, want unchanged", content)
	}
}

func TestWriteArtifactsReplacesOnlyTheExpectedFiles(t *testing.T) {
	t.Parallel()
	output := t.TempDir()
	unrelatedPath := filepath.Join(output, "notes.txt")
	if err := os.WriteFile(unrelatedPath, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifacts := map[string][]byte{
		"calendar-front.gif":      []byte("calendar"),
		"codex-front.gif":         []byte("codex"),
		"codex-quota-front.gif":   []byte("quota"),
		"mac-resources-front.gif": []byte("resources"),
	}
	if err := writeArtifacts(output, artifacts); err != nil {
		t.Fatal(err)
	}
	for name, want := range artifacts {
		content, err := os.ReadFile(filepath.Join(output, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != string(want) {
			t.Fatalf("%s = %q, want %q", name, content, want)
		}
	}
	if content, err := os.ReadFile(unrelatedPath); err != nil || string(content) != "keep" {
		t.Fatalf("unrelated file = %q, %v", content, err)
	}
}

func TestWriteFileSetsRollsBackEveryDirectoryAfterMidCommitFailure(t *testing.T) {
	firstDirectory := t.TempDir()
	secondDirectory := t.TempDir()
	firstPath := filepath.Join(firstDirectory, "first.txt")
	secondPath := filepath.Join(secondDirectory, "second.txt")
	if err := os.WriteFile(firstPath, []byte("first-old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("second-old"), 0o644); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected rename failure")
	failed := false

	err := writeFileSets([]fileSet{
		{output: firstDirectory, files: map[string][]byte{"first.txt": []byte("first-new")}},
		{output: secondDirectory, files: map[string][]byte{"second.txt": []byte("second-new")}},
	}, func(oldPath, newPath string) error {
		if !failed && newPath == secondPath && strings.HasPrefix(filepath.Base(oldPath), ".preview-") {
			failed = true
			return injected
		}
		return os.Rename(oldPath, newPath)
	})
	if !errors.Is(err, injected) {
		t.Fatalf("write error = %v, want injected rename failure", err)
	}
	for path, want := range map[string]string{firstPath: "first-old", secondPath: "second-old"} {
		content, readErr := os.ReadFile(path)
		if readErr != nil || string(content) != want {
			t.Fatalf("%s after rollback = %q, err=%v; want %q", path, content, readErr, want)
		}
	}
}

func TestWriteArtifactsRejectsDirectoryDestinationWithoutRemovingIt(t *testing.T) {
	output := t.TempDir()
	destination := filepath.Join(output, "calendar-front.gif")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(destination, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifacts := map[string][]byte{
		"calendar-front.gif":      []byte("calendar"),
		"codex-front.gif":         []byte("codex"),
		"codex-quota-front.gif":   []byte("quota"),
		"mac-resources-front.gif": []byte("resources"),
	}

	if err := writeArtifacts(output, artifacts); err == nil {
		t.Fatal("directory destination was replaced")
	}
	content, err := os.ReadFile(sentinel)
	if err != nil || string(content) != "keep" {
		t.Fatalf("directory sentinel after rejected write = %q, err=%v", content, err)
	}
}

func TestWriteFileSetsRetainsOriginalWhenRollbackCannotRestore(t *testing.T) {
	output := t.TempDir()
	destination := filepath.Join(output, "preview.gif")
	if err := os.WriteFile(destination, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	var backup string
	err := writeFileSets([]fileSet{{output: output, files: map[string][]byte{"preview.gif": []byte("replacement")}}}, func(oldPath, newPath string) error {
		if oldPath == destination {
			backup = newPath
			return os.Rename(oldPath, newPath)
		}
		return errors.New("destination unavailable")
	})
	if err == nil || backup == "" || !strings.Contains(err.Error(), backup) {
		t.Fatalf("failed rollback did not identify recovery copy: %v (backup %q)", err, backup)
	}
	content, readErr := os.ReadFile(backup)
	if readErr != nil || string(content) != "original" {
		t.Fatalf("recoverable original = %q, %v", content, readErr)
	}
}
