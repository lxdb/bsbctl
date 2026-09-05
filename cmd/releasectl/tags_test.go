package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunVerifyTagsDereferencesEveryReleaseTagToTheBuiltCommit(t *testing.T) {
	root, builtCommit := releaseTagRepository(t)
	gitCommand(t, root, "tag", "v0.1.0", builtCommit)
	gitCommand(t, root, "tag", "plugin/calendar/v0.1.0", builtCommit)
	gitCommand(t, root, "tag", "plugin/codex/v0.1.0", builtCommit)
	gitCommand(t, root, "tag", "-a", "plugin/codex-quota/v0.1.0", "-m", "codex quota v0.1.0", builtCommit)
	gitCommand(t, root, "tag", "plugin/mac-resources/v0.1.0", builtCommit)

	var stdout, stderr bytes.Buffer
	code := run([]string{"verify-tags", "--root", root, "--commit", builtCommit}, &stdout, &stderr)
	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("verify-tags exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	want := "release tags: all 5 resolve to " + builtCommit + "\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunVerifyTagsRejectsAnyTagAtAnotherCommitOrMissingTag(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string, string)
	}{
		{
			name: "different commit",
			setup: func(t *testing.T, root, builtCommit string) {
				previousCommit := strings.TrimSpace(gitCommand(t, root, "rev-parse", builtCommit+"^"))
				gitCommand(t, root, "tag", "v0.1.0", builtCommit)
				gitCommand(t, root, "tag", "plugin/calendar/v0.1.0", builtCommit)
				gitCommand(t, root, "tag", "plugin/codex/v0.1.0", builtCommit)
				gitCommand(t, root, "tag", "plugin/codex-quota/v0.1.0", previousCommit)
				gitCommand(t, root, "tag", "plugin/mac-resources/v0.1.0", builtCommit)
			},
		},
		{
			name: "missing tag",
			setup: func(t *testing.T, root, builtCommit string) {
				gitCommand(t, root, "tag", "v0.1.0", builtCommit)
				gitCommand(t, root, "tag", "plugin/calendar/v0.1.0", builtCommit)
				gitCommand(t, root, "tag", "plugin/codex/v0.1.0", builtCommit)
				gitCommand(t, root, "tag", "plugin/codex-quota/v0.1.0", builtCommit)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, builtCommit := releaseTagRepository(t)
			test.setup(t, root, builtCommit)
			var stdout, stderr bytes.Buffer
			code := run([]string{"verify-tags", "--root", root, "--commit", builtCommit}, &stdout, &stderr)
			if code != exitFailure || stdout.Len() != 0 || stderr.String() != "releasectl: release tag binding failed\n" {
				t.Fatalf("verify-tags exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func releaseTagRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	gitCommand(t, root, "init", "--quiet")
	gitCommand(t, root, "config", "user.name", "Release Test")
	gitCommand(t, root, "config", "user.email", "release-test@example.invalid")
	gitCommand(t, root, "config", "commit.gpgsign", "false")
	gitCommand(t, root, "config", "tag.gpgsign", "false")
	writeReleaseFile(t, filepath.Join(root, "tracked"), "first\n")
	gitCommand(t, root, "add", "tracked")
	gitCommand(t, root, "commit", "--quiet", "-m", "first")
	writeReleaseFile(t, filepath.Join(root, "release", "versions.json"), `{"schema_version":1,"components":[{"id":"bsbctl","kind":"core","version":"0.1.0","tag":"v0.1.0","package":"./cmd/bsbctl","binary":"bsbctl"},{"id":"dev.bsbctl.calendar","kind":"plugin","version":"0.1.0","tag":"plugin/calendar/v0.1.0","package":"./cmd/bsbctl-plugin-calendar","binary":"bsbctl-plugin-calendar"},{"id":"dev.bsbctl.codex","kind":"plugin","version":"0.1.0","tag":"plugin/codex/v0.1.0","package":"./cmd/bsbctl-plugin-codex","binary":"bsbctl-plugin-codex"},{"id":"dev.bsbctl.codex-quota","kind":"plugin","version":"0.1.0","tag":"plugin/codex-quota/v0.1.0","package":"./cmd/bsbctl-plugin-codex-quota","binary":"bsbctl-plugin-codex-quota"},{"id":"dev.bsbctl.mac-resources","kind":"plugin","version":"0.1.0","tag":"plugin/mac-resources/v0.1.0","package":"./cmd/bsbctl-plugin-mac-resources","binary":"bsbctl-plugin-mac-resources"}]}`)
	gitCommand(t, root, "add", "release/versions.json")
	gitCommand(t, root, "commit", "--quiet", "-m", "release plan")
	return root, strings.TrimSpace(gitCommand(t, root, "rev-parse", "HEAD"))
}

func gitCommand(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}
