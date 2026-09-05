package main

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lxdb/bsbctl/internal/releaseartifact"
)

var releaseCommitPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

func runVerifyTags(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("verify-tags")
	root := flags.String("root", ".", "repository root")
	commit := flags.String("commit", "", "built commit SHA")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !releaseCommitPattern.MatchString(*commit) {
		_, _ = fmt.Fprintln(stderr, "releasectl: release tag binding failed")
		return exitFailure
	}

	count, err := verifyReleaseTags(*root, *commit)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "releasectl: release tag binding failed")
		return exitFailure
	}
	_, _ = fmt.Fprintf(stdout, "release tags: all %d resolve to %s\n", count, *commit)
	return exitSuccess
}

func verifyReleaseTags(root, builtCommit string) (int, error) {
	planData, err := readBoundedFile(filepath.Join(root, "release", "versions.json"))
	if err != nil {
		return 0, err
	}
	plan, err := releaseartifact.DecodePlan(planData)
	if err != nil {
		return 0, err
	}
	for _, component := range plan.Components {
		resolved, err := gitRevParseCommit(root, "refs/tags/"+component.Tag+"^{commit}")
		if err != nil || resolved != builtCommit {
			return 0, errors.New("release tag does not resolve to built commit")
		}
	}
	return len(plan.Components), nil
}

func gitRevParseCommit(root, revision string) (string, error) {
	command := exec.Command("git", "-C", root, "rev-parse", "--verify", revision)
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	resolved := strings.TrimSpace(string(output))
	if !releaseCommitPattern.MatchString(resolved) {
		return "", errors.New("git returned an invalid commit ID")
	}
	return resolved, nil
}
