package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/lxdb/bsbctl/internal/releaseartifact"
)

func runReleaseTags(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("release-tags")
	root := flags.String("root", ".", "repository root")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "releasectl: invalid release-tags arguments")
		return exitFailure
	}
	data, err := os.ReadFile(filepath.Join(*root, "release", "versions.json"))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "releasectl: release version metadata is unavailable")
		return exitFailure
	}
	plan, err := releaseartifact.DecodePlan(data)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "releasectl: release version metadata is invalid")
		return exitFailure
	}
	for _, component := range plan.Components {
		ref := "refs/tags/" + component.Tag
		if _, err := fmt.Fprintf(stdout, "%s:%s\n", ref, ref); err != nil {
			return exitFailure
		}
	}
	return exitSuccess
}
