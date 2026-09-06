package releasecheck

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var actionReferencePattern = regexp.MustCompile(`uses:\s+([^\s@]+)@([^\s#]+)`)

func TestNonPublishingWorkflowsArePinnedReadOnlyAndUseCanonicalPhases(t *testing.T) {
	workflows := map[string][]string{
		"ci.yml": {
			"pull_request:", "workflow_dispatch:", "concurrency:", "timeout-minutes:",
			`scripts/install-shellcheck.sh "$RUNNER_TEMP/bin"`,
			"scripts/verify.sh format", "scripts/verify.sh metadata", "scripts/verify.sh vet",
			"scripts/verify.sh dead-code", "scripts/verify.sh docs", "scripts/verify.sh repository",
			"scripts/verify.sh test", "scripts/verify.sh preview", "scripts/verify.sh race", "scripts/verify.sh coverage",
		},
		"contracts.yml": {
			"pull_request:", "workflow_dispatch:", "concurrency:", "timeout-minutes:",
			"scripts/verify.sh linux-pluginhost", "scripts/verify.sh preflight",
			"releasectl package", "releasectl inspect", "shasum -a 256 -c SHA256SUMS",
			"macos-15", "macos-15-intel", "darwin/arm64", "darwin/amd64", "sbom.cdx.json",
		},
		"fuzz.yml": {
			"workflow_dispatch:", "schedule:", "cancel-in-progress: false", "scripts/verify.sh fuzz",
			"scripts/verify.sh depth",
		},
		"security.yml": {
			"pull_request:", "schedule:", "workflow_dispatch:", "scripts/verify.sh security",
		},
	}
	for name, requiredValues := range workflows {
		workflow := readWorkflow(t, name)
		for _, forbidden := range []string{"pull_request_target", "contents: write", "secrets.", "environment: release", "@latest"} {
			if strings.Contains(workflow, forbidden) {
				t.Fatalf("%s contains forbidden value %q", name, forbidden)
			}
		}
		for _, required := range append([]string{"permissions:\n  contents: read", "persist-credentials: false", "go-version-file: go.mod"}, requiredValues...) {
			if !strings.Contains(workflow, required) {
				t.Fatalf("%s is missing %q", name, required)
			}
		}
		if got, want := strings.Count(workflow, "uses: actions/checkout@"), strings.Count(workflow, "persist-credentials: false"); got != want {
			t.Fatalf("%s checkout count = %d, persist-credentials count = %d", name, got, want)
		}
		assertSetupGoUsesModuleToolchain(t, name, workflow)
		assertPinnedActions(t, workflow)
	}
}

func TestConventionalCommitWorkflowOnlyReadsPullRequestMetadata(t *testing.T) {
	workflow := readWorkflow(t, "conventional-commits.yml")
	for _, required := range []string{
		"pull_request_target:", "pull-requests: read", "timeout-minutes: 5",
		"amannn/action-semantic-pull-request@48f256284bd46cdaab1048c3721360e808335d50",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("Conventional Commit workflow is missing %q", required)
		}
	}
	for _, forbidden := range []string{"actions/checkout", "contents: write", "pull-requests: write", "run:"} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("Conventional Commit workflow contains forbidden value %q", forbidden)
		}
	}
	assertPinnedActions(t, workflow)
}

func TestReleaseWorkflowSeparatesReadOnlyBuildFromProtectedPublish(t *testing.T) {
	release := readWorkflow(t, "release.yml")
	if strings.Contains(release, "pull_request") || strings.Contains(release, "pull_request_target") {
		t.Fatal("release workflow is eligible for pull-request execution")
	}
	for _, required := range []string{
		"workflow_dispatch:", "permissions:\n  contents: read", "persist-credentials: false",
		"go-version-file: go.mod", "concurrency:", "timeout-minutes:",
		"build:", "publish:", "needs: build", "environment: release", "contents: write",
		"scripts/verify.sh preflight", "releasectl package", "releasectl inspect", "actions/upload-artifact", "actions/download-artifact",
		"secrets.CATALOG_SIGNING_PRIVATE_KEY_B64", "sign-catalog", "releasectl verify-catalog", "releasectl publish-releases",
	} {
		if !strings.Contains(release, required) {
			t.Fatalf("release workflow is missing %q", required)
		}
	}
	buildStart := strings.Index(release, "\n  build:")
	publishStart := strings.Index(release, "\n  publish:")
	if buildStart < 0 || publishStart <= buildStart {
		t.Fatal("release build and publish jobs are not ordered independently")
	}
	buildSection := release[buildStart:publishStart]
	if strings.Contains(buildSection, "contents: write") || strings.Contains(buildSection, "secrets.") || strings.Contains(buildSection, "environment:") {
		t.Fatal("release build job has publish or signing authority")
	}
	assertSetupGoUsesModuleToolchain(t, "release.yml", release)
	assertPinnedActions(t, release)
}

func assertSetupGoUsesModuleToolchain(t *testing.T, name, workflow string) {
	t.Helper()
	if strings.Contains(workflow, "go-version:") {
		t.Fatalf("%s duplicates the exact toolchain instead of reading go.mod", name)
	}
	if got, want := strings.Count(workflow, "go-version-file: go.mod"), strings.Count(workflow, "uses: actions/setup-go@"); got != want {
		t.Fatalf("%s go-version-file count = %d, setup-go count = %d", name, got, want)
	}
}

func TestReleaseWorkflowRemovesSigningKeyFromChildEnvironments(t *testing.T) {
	t.Parallel()
	release := readWorkflow(t, "release.yml")
	signStart := strings.Index(release, "- name: Sign catalog from protected stdin only")
	verifyStart := strings.Index(release, "- name: Verify the signed catalog")
	if signStart < 0 || verifyStart <= signStart {
		t.Fatal("release workflow signing step is missing")
	}
	beforeSign := release[:signStart]
	signStep := release[signStart:verifyStart]
	if !strings.Contains(beforeSign, `go build -trimpath -o "$RUNNER_TEMP/releasectl" ./cmd/releasectl`) {
		t.Fatal("release workflow does not prebuild the signer before loading signing material")
	}
	copyIndex := strings.Index(signStep, `catalog_signing_private_key_b64="$CATALOG_SIGNING_PRIVATE_KEY_B64"`)
	unsetIndex := strings.Index(signStep, "unset CATALOG_SIGNING_PRIVATE_KEY_B64")
	invokeIndex := strings.Index(signStep, `| "$RUNNER_TEMP/releasectl" sign-catalog`)
	clearIndex := strings.LastIndex(signStep, "catalog_signing_private_key_b64=")
	if copyIndex < 0 || unsetIndex <= copyIndex || invokeIndex <= unsetIndex || clearIndex <= invokeIndex {
		t.Fatal("release workflow does not copy, unexport, consume, and clear signing material in order")
	}
	if strings.Contains(signStep, "go run") {
		t.Fatal("release workflow invokes the Go toolchain while signing material is loaded")
	}
}

func TestReleaseWorkflowFetchesExactTagsAndBindsThemToBuiltCommitBeforeSigning(t *testing.T) {
	release := readWorkflow(t, "release.yml")
	for _, forbidden := range []string{"git fetch --tags", "+refs/tags/", "git tag -f"} {
		if strings.Contains(release, forbidden) {
			t.Fatalf("release workflow contains unsafe tag fetch/mutation %q", forbidden)
		}
	}
	for _, required := range []string{
		`"$RUNNER_TEMP/releasectl" release-tags --root .`,
		`test "${#release_tag_refspecs[@]}" -gt 0`,
		"git fetch --no-tags --no-recurse-submodules origin",
		`"${release_tag_refspecs[@]}"`,
		`releasectl verify-tags --root . --commit "$GITHUB_SHA"`,
	} {
		if !strings.Contains(release, required) {
			t.Fatalf("release workflow is missing exact tag binding %q", required)
		}
	}
	verifyIndex := strings.Index(release, "releasectl verify-tags")
	signIndex := strings.Index(release, "sign-catalog")
	verifyCatalogIndex := strings.Index(release, "releasectl verify-catalog")
	publishIndex := strings.Index(release, "releasectl publish-releases")
	if verifyIndex < 0 || signIndex <= verifyIndex || verifyCatalogIndex <= signIndex || publishIndex <= verifyCatalogIndex {
		t.Fatal("release tags and signed catalog are not verified before publication")
	}
}

func TestReleaseRunbookStatesProtectedTagControlsWithoutClaimingSignedTags(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "docs", "release.md"))
	if err != nil {
		t.Fatal(err)
	}
	runbook := string(data)
	for _, required := range []string{
		"protected tag creation and update controls",
		"does not verify GPG or SSH tag signatures",
		"release/catalog-predecessor.json",
	} {
		if !strings.Contains(runbook, required) {
			t.Fatalf("release runbook is missing %q", required)
		}
	}
}

func TestReleaseWorkflowUsesResumableReconciliationInsteadOfSequentialCreate(t *testing.T) {
	release := readWorkflow(t, "release.yml")
	if strings.Contains(release, "gh release create") {
		t.Fatal("release workflow still uses non-resumable sequential release creation")
	}
	for _, required := range []string{
		"go run ./cmd/releasectl publish-releases",
		`--commit "$GITHUB_SHA"`,
		"--artifacts release-artifacts/release-darwin-arm64",
		"--artifacts release-artifacts/release-darwin-amd64",
		"--catalog release-artifacts/catalog.json",
		"--signature release-artifacts/catalog.sig",
	} {
		if !strings.Contains(release, required) {
			t.Fatalf("release workflow is missing resumable publication input %q", required)
		}
	}
}

func TestReleaseWorkflowConsumesOnlyExactArtifactsFromTheSameRun(t *testing.T) {
	release := readWorkflow(t, "release.yml")
	publishStart := strings.Index(release, "\n  publish:")
	if publishStart < 0 {
		t.Fatal("release publish job is missing")
	}
	publish := release[publishStart:]
	if strings.Count(publish, "uses: actions/download-artifact@") != 2 {
		t.Fatal("release publish job must download the two architecture artifacts by exact name")
	}
	for _, required := range []string{
		"name: release-darwin-arm64\n          path: release-artifacts/release-darwin-arm64",
		"name: release-darwin-amd64\n          path: release-artifacts/release-darwin-amd64",
	} {
		if !strings.Contains(publish, required) {
			t.Fatalf("release publish job is missing same-run artifact binding %q", required)
		}
	}
	for _, forbidden := range []string{"pattern:", "run-id:", "github-token:", "repository:"} {
		if strings.Contains(publish, forbidden) {
			t.Fatalf("release publish job can select artifacts outside the exact current-run handoff via %q", forbidden)
		}
	}
}

func assertPinnedActions(t *testing.T, workflow string) {
	t.Helper()
	allowed := map[string]string{
		"actions/checkout":                    "11d5960a326750d5838078e36cf38b85af677262",
		"actions/setup-go":                    "b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
		"actions/upload-artifact":             "043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
		"actions/download-artifact":           "3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c",
		"amannn/action-semantic-pull-request": "48f256284bd46cdaab1048c3721360e808335d50",
	}
	matches := actionReferencePattern.FindAllStringSubmatch(workflow, -1)
	if len(matches) == 0 {
		t.Fatal("workflow has no action references")
	}
	for _, match := range matches {
		want, exists := allowed[match[1]]
		if !exists || match[2] != want {
			t.Fatalf("action reference %s@%s is not an approved immutable upstream commit", match[1], match[2])
		}
	}
}

func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
	if err != nil {
		t.Fatalf("read workflow %s: %v", name, err)
	}
	return string(data)
}
