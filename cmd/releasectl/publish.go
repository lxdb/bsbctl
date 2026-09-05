package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
)

var (
	releaseSHA256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	githubRepositoryPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	verifyPublicationTags     = verifyReleaseTags
	buildPublicationSpecs     = buildReleaseSpecs
	verifyPublicationSequence = verifyCatalogPublicationSequence
	releaseRemoteFactory      = newGitHubReleaseRemote
)

const githubReleaseResponseLimit = 1 << 20

type releaseAsset struct {
	Name   string
	Path   string
	Size   int64
	SHA256 string
}

type releaseSpec struct {
	Tag    string
	Title  string
	Assets []releaseAsset
}

type remoteReleaseAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

type remoteRelease struct {
	ID        int64                `json:"id"`
	Tag       string               `json:"tag_name"`
	Title     string               `json:"name"`
	Draft     bool                 `json:"draft"`
	UploadURL string               `json:"upload_url"`
	Assets    []remoteReleaseAsset `json:"assets"`
}

type releaseRemote interface {
	Get(context.Context, string) (remoteRelease, bool, error)
	DownloadAsset(context.Context, string, string) ([]byte, error)
	CreateDraft(context.Context, releaseSpec, string) (remoteRelease, error)
	UploadAsset(context.Context, remoteRelease, releaseAsset) error
	DeleteAsset(context.Context, remoteRelease, remoteReleaseAsset) error
	Publish(context.Context, remoteRelease) error
}

func runPublishReleases(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("publish-releases")
	root := flags.String("root", ".", "repository root")
	commit := flags.String("commit", "", "built commit SHA")
	catalogPath := flags.String("catalog", "", "verified catalog JSON")
	signaturePath := flags.String("signature", "", "verified catalog signature")
	var artifactDirectories stringListFlag
	flags.Var(&artifactDirectories, "artifacts", "verified architecture artifact directory (repeat twice)")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || len(artifactDirectories) != 2 || !releaseCommitPattern.MatchString(*commit) || *catalogPath == "" || *signaturePath == "" {
		_, _ = fmt.Fprintln(stderr, "releasectl: invalid publish-releases arguments")
		return exitFailure
	}
	if _, err := verifyPublicationTags(*root, *commit); err != nil {
		_, _ = fmt.Fprintln(stderr, "release publication reconciliation failed")
		return exitFailure
	}
	specs, err := buildPublicationSpecs(*root, artifactDirectories, *catalogPath, *signaturePath, catalogVerificationClock().UTC())
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "release publication reconciliation failed")
		return exitFailure
	}
	remote, err := releaseRemoteFactory()
	if err != nil || verifyPublicationSequence(ctx, *root, *catalogPath, *signaturePath, specs, remote, catalogVerificationClock().UTC()) != nil || reconcileReleases(ctx, remote, specs, *commit) != nil {
		_, _ = fmt.Fprintln(stderr, "release publication reconciliation failed")
		return exitFailure
	}
	_, _ = fmt.Fprintf(stdout, "release publication: reconciled %d release(s)\n", len(specs))
	return exitSuccess
}
