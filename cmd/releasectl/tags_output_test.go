package main

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRunReleaseTagsEmitsEveryValidatedTagAsFetchRefspec(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := runReleaseTags([]string{"--root", filepath.Join("..", "..")}, &stdout, &stderr)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("release-tags = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	want := []string{
		"refs/tags/v0.1.0:refs/tags/v0.1.0",
		"refs/tags/plugin/calendar/v0.1.0:refs/tags/plugin/calendar/v0.1.0",
		"refs/tags/plugin/codex/v0.1.0:refs/tags/plugin/codex/v0.1.0",
		"refs/tags/plugin/codex-quota/v0.1.0:refs/tags/plugin/codex-quota/v0.1.0",
		"refs/tags/plugin/github-notifications/v0.1.0:refs/tags/plugin/github-notifications/v0.1.0",
		"refs/tags/plugin/mac-resources/v0.1.0:refs/tags/plugin/mac-resources/v0.1.0",
		"refs/tags/plugin/slack/v0.1.0:refs/tags/plugin/slack/v0.1.0",
	}
	if got := strings.Fields(stdout.String()); !reflect.DeepEqual(got, want) {
		t.Fatalf("release tag refspecs = %q, want %q", got, want)
	}
}
