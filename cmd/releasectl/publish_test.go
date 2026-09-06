package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/catalog"
)

func exactRemoteAssets(remoteAssets []remoteReleaseAsset, expected []releaseAsset) bool {
	missing, err := missingReleaseAssets(remoteAssets, expected)
	return err == nil && len(missing) == 0
}

func TestBuildReleaseSpecsBindsExactInspectedAssetsAndVerifiedCatalog(t *testing.T) {
	root := packageFixture(t)
	arm64 := filepath.Join(t.TempDir(), "arm64")
	amd64 := filepath.Join(t.TempDir(), "amd64")
	previousBuilder := buildReleaseComponent
	buildReleaseComponent = func(_ context.Context, request buildRequest) ([]byte, error) {
		return []byte(fmt.Sprintf("binary:%s:%s:%s\n", request.Component.ID, request.Component.Version, request.GOARCH)), nil
	}
	t.Cleanup(func() { buildReleaseComponent = previousBuilder })
	epoch := time.Unix(1700000000, 0).UTC()
	if _, err := packageComponents(context.Background(), root, arm64, "darwin", "arm64", epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := packageComponents(context.Background(), root, amd64, "darwin", "amd64", epoch); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	signaturePath := filepath.Join(t.TempDir(), "catalog.sig")
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"catalog", "--root", root, "--artifacts", arm64, "--artifacts", amd64,
		"--base-url", "https://github.com/lxdb/bsbctl/releases/download", "--sequence", "7",
		"--generated-at", "2026-08-22T11:00:00Z", "--out", catalogPath,
	}, &stdout, &stderr); code != exitSuccess {
		t.Fatalf("catalog exit=%d stderr=%q", code, stderr.String())
	}
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x61}, ed25519.SeedSize))
	installReleaseKeyring(t, catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)})
	stdout.Reset()
	if code := runWithInput(context.Background(), []string{"sign-catalog", "--catalog", catalogPath, "--key-id", "stable", "--out", signaturePath}, strings.NewReader(base64.StdEncoding.EncodeToString(private)), &stdout, &stderr); code != exitSuccess {
		t.Fatalf("sign exit=%d stderr=%q", code, stderr.String())
	}

	specs, err := buildReleaseSpecs(root, []string{arm64, amd64}, catalogPath, signaturePath, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 7 || specs[0].Tag != "plugin/calendar/v0.5.0" || specs[1].Tag != "plugin/codex-quota/v0.2.0" || specs[2].Tag != "plugin/codex/v0.4.0" || specs[3].Tag != "plugin/github-notifications/v0.1.0" || specs[4].Tag != "plugin/mac-resources/v0.3.0" || specs[5].Tag != "plugin/slack/v0.1.0" || specs[6].Tag != "v0.1.0" {
		t.Fatalf("release specs = %#v", specs)
	}
	if len(specs[0].Assets) != 2 || len(specs[1].Assets) != 2 || len(specs[2].Assets) != 2 || len(specs[3].Assets) != 2 || len(specs[4].Assets) != 2 || len(specs[5].Assets) != 2 || len(specs[6].Assets) != 7 {
		t.Fatalf("release asset counts = %d, %d, %d, %d, %d, %d, %d", len(specs[0].Assets), len(specs[1].Assets), len(specs[2].Assets), len(specs[3].Assets), len(specs[4].Assets), len(specs[5].Assets), len(specs[6].Assets))
	}
	coreAssetNames := make([]string, 0, len(specs[6].Assets))
	for _, asset := range specs[6].Assets {
		coreAssetNames = append(coreAssetNames, asset.Name)
	}
	wantCoreAssetNames := []string{
		"SHA256SUMS-darwin-amd64", "SHA256SUMS-darwin-arm64",
		"bsbctl_0.1.0_darwin_amd64.tar.gz", "bsbctl_0.1.0_darwin_arm64.tar.gz",
		"catalog.json", "catalog.sig", "install.sh",
	}
	if !reflect.DeepEqual(coreAssetNames, wantCoreAssetNames) {
		t.Fatalf("core release assets = %v, want %v", coreAssetNames, wantCoreAssetNames)
	}
	for _, spec := range specs {
		for _, asset := range spec.Assets {
			data, err := os.ReadFile(asset.Path)
			if err != nil || int64(len(data)) != asset.Size || len(asset.SHA256) != 64 {
				t.Fatalf("asset %q is not bound to exact local bytes: size=%d digest=%q err=%v", asset.Name, asset.Size, asset.SHA256, err)
			}
		}
	}

	if err := os.WriteFile(signaturePath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildReleaseSpecs(root, []string{arm64, amd64}, catalogPath, signaturePath, now); err == nil {
		t.Fatal("buildReleaseSpecs accepted an unverified catalog signature")
	}
}

func TestRunPublishReleasesVerifiesTagsAndReconcilesWithoutDirectGitHubCalls(t *testing.T) {
	previousTags := verifyPublicationTags
	previousSpecs := buildPublicationSpecs
	previousSequence := verifyPublicationSequence
	previousRemote := releaseRemoteFactory
	t.Cleanup(func() {
		verifyPublicationTags = previousTags
		buildPublicationSpecs = previousSpecs
		verifyPublicationSequence = previousSequence
		releaseRemoteFactory = previousRemote
	})
	commit := strings.Repeat("c", 40)
	verifyPublicationTags = func(root, gotCommit string) (int, error) {
		if root != "/repository" || gotCommit != commit {
			t.Fatalf("tag verification root=%q commit=%q", root, gotCommit)
		}
		return 3, nil
	}
	specs := releaseReconciliationFixture()
	buildPublicationSpecs = func(root string, artifacts []string, catalogPath, signaturePath string, _ time.Time) ([]releaseSpec, error) {
		if root != "/repository" || !strings.HasSuffix(catalogPath, "catalog.json") || !strings.HasSuffix(signaturePath, "catalog.sig") || len(artifacts) != 2 {
			t.Fatalf("publication inputs root=%q artifacts=%#v catalog=%q signature=%q", root, artifacts, catalogPath, signaturePath)
		}
		return specs, nil
	}
	remote := newFakeReleaseRemote()
	verifyPublicationSequence = func(context.Context, string, string, string, []releaseSpec, releaseRemote, time.Time) error {
		return nil
	}
	releaseRemoteFactory = func() (releaseRemote, error) { return remote, nil }

	var stdout, stderr bytes.Buffer
	code := runWithContext(context.Background(), []string{
		"publish-releases", "--root", "/repository", "--commit", commit,
		"--artifacts", "/artifacts/arm64", "--artifacts", "/artifacts/amd64",
		"--catalog", "/artifacts/catalog.json", "--signature", "/artifacts/catalog.sig",
	}, &stdout, &stderr)
	if code != exitSuccess || stdout.String() != "release publication: reconciled 3 release(s)\n" || stderr.Len() != 0 {
		t.Fatalf("publish exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, spec := range specs {
		if remote.releases[spec.Tag].Draft {
			t.Fatalf("release %q remains a draft", spec.Tag)
		}
	}
}

func TestRunPublishReleasesChecksCatalogSequenceBeforeAnyRemoteMutation(t *testing.T) {
	previousTags := verifyPublicationTags
	previousSpecs := buildPublicationSpecs
	previousSequence := verifyPublicationSequence
	previousRemote := releaseRemoteFactory
	t.Cleanup(func() {
		verifyPublicationTags = previousTags
		buildPublicationSpecs = previousSpecs
		verifyPublicationSequence = previousSequence
		releaseRemoteFactory = previousRemote
	})
	verifyPublicationTags = func(string, string) (int, error) { return 3, nil }
	buildPublicationSpecs = func(string, []string, string, string, time.Time) ([]releaseSpec, error) {
		return releaseReconciliationFixture(), nil
	}
	remote := newFakeReleaseRemote()
	releaseRemoteFactory = func() (releaseRemote, error) { return remote, nil }
	verifyPublicationSequence = func(context.Context, string, string, string, []releaseSpec, releaseRemote, time.Time) error {
		return errors.New("stale signed catalog")
	}

	var stdout, stderr bytes.Buffer
	code := runWithContext(context.Background(), []string{
		"publish-releases", "--root", t.TempDir(), "--commit", strings.Repeat("d", 40),
		"--artifacts", "/artifacts/arm64", "--artifacts", "/artifacts/amd64",
		"--catalog", "/artifacts/catalog.json", "--signature", "/artifacts/catalog.sig",
	}, &stdout, &stderr)
	if code != exitFailure || remote.mutations != 0 {
		t.Fatalf("publish exit=%d mutations=%d", code, remote.mutations)
	}
}

func TestVerifyCatalogPublicationSequenceAuthenticatesExactPredecessorAndFirstRelease(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x73}, ed25519.SeedSize))
	installReleaseKeyring(t, catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)})
	priorCatalog, priorSignature := signedPublicationCatalog(private, 7,
		time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC))
	currentCatalog, currentSignature := signedPublicationCatalog(private, 8,
		time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC))

	t.Run("exact predecessor establishes monotonic sequence", func(t *testing.T) {
		root, catalogPath, signaturePath := publicationSequenceFixture(t, currentCatalog, currentSignature,
			fmt.Sprintf(`{"schema_version":1,"tag":"v0.0.9","catalog_sha256":"%x","signature_sha256":"%x"}`, sha256.Sum256(priorCatalog), sha256.Sum256(priorSignature)))
		remote := newFakeReleaseRemote()
		installFakeCatalogPredecessor(remote, "v0.0.9", priorCatalog, priorSignature)
		specs := publicationCatalogSpecs(catalogPath, signaturePath, currentCatalog, currentSignature)
		if err := verifyCatalogPublicationSequence(context.Background(), root, catalogPath, signaturePath, specs, remote, now); err != nil {
			t.Fatal(err)
		}
		remote.downloads["v0.0.9/catalog.json"] = append([]byte(nil), currentCatalog...)
		if err := verifyCatalogPublicationSequence(context.Background(), root, catalogPath, signaturePath, specs, remote, now); err == nil {
			t.Fatal("publication accepted prior bytes that did not match tracked digest")
		}
	})

	t.Run("stale current sequence", func(t *testing.T) {
		root, catalogPath, signaturePath := publicationSequenceFixture(t, priorCatalog, priorSignature,
			fmt.Sprintf(`{"schema_version":1,"tag":"v0.0.9","catalog_sha256":"%x","signature_sha256":"%x"}`, sha256.Sum256(priorCatalog), sha256.Sum256(priorSignature)))
		remote := newFakeReleaseRemote()
		installFakeCatalogPredecessor(remote, "v0.0.9", priorCatalog, priorSignature)
		if err := verifyCatalogPublicationSequence(context.Background(), root, catalogPath, signaturePath,
			publicationCatalogSpecs(catalogPath, signaturePath, priorCatalog, priorSignature), remote, now); err == nil {
			t.Fatal("publication accepted a non-increasing current sequence")
		}
	})

	t.Run("explicit first release requires sequence one", func(t *testing.T) {
		firstCatalog, firstSignature := signedPublicationCatalog(private, 1,
			time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC))
		root, catalogPath, signaturePath := publicationSequenceFixture(t, firstCatalog, firstSignature, `{"schema_version":1,"first_release":true}`)
		if err := verifyCatalogPublicationSequence(context.Background(), root, catalogPath, signaturePath,
			publicationCatalogSpecs(catalogPath, signaturePath, firstCatalog, firstSignature), newFakeReleaseRemote(), now); err != nil {
			t.Fatal(err)
		}
		root, catalogPath, signaturePath = publicationSequenceFixture(t, currentCatalog, currentSignature, `{"schema_version":1,"first_release":true}`)
		if err := verifyCatalogPublicationSequence(context.Background(), root, catalogPath, signaturePath,
			publicationCatalogSpecs(catalogPath, signaturePath, currentCatalog, currentSignature), newFakeReleaseRemote(), now); err == nil {
			t.Fatal("first release accepted a sequence other than one")
		}
	})
}

func TestCatalogSequenceGateAndReleaseReconciliationAreExactByteIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x74}, ed25519.SeedSize))
	installReleaseKeyring(t, catalog.Keyring{"stable": private.Public().(ed25519.PublicKey)})
	priorCatalog, priorSignature := signedPublicationCatalog(private, 1, now.Add(-48*time.Hour))
	currentCatalog, currentSignature := signedPublicationCatalog(private, 2, now.Add(-time.Hour))
	root, catalogPath, signaturePath := publicationSequenceFixture(t, currentCatalog, currentSignature,
		fmt.Sprintf(`{"schema_version":1,"tag":"v0.0.9","catalog_sha256":"%x","signature_sha256":"%x"}`, sha256.Sum256(priorCatalog), sha256.Sum256(priorSignature)))
	remote := newFakeReleaseRemote()
	installFakeCatalogPredecessor(remote, "v0.0.9", priorCatalog, priorSignature)
	specs := publicationCatalogSpecs(catalogPath, signaturePath, currentCatalog, currentSignature)
	remote.releases[specs[0].Tag] = exactRemoteRelease(10, specs[0], false)
	if err := verifyCatalogPublicationSequence(context.Background(), root, catalogPath, signaturePath, specs, remote, now); err != nil {
		t.Fatal(err)
	}
	if err := reconcileReleases(context.Background(), remote, specs, strings.Repeat("e", 40)); err != nil {
		t.Fatal(err)
	}
	if remote.mutations != 0 {
		t.Fatalf("exact rerun mutations = %d, want 0", remote.mutations)
	}
}

func TestRunPublishRefreshesOnlyCatalogPairInOwnedPartialDraftAfterSequenceVerification(t *testing.T) {
	previousTags := verifyPublicationTags
	previousSpecs := buildPublicationSpecs
	previousSequence := verifyPublicationSequence
	previousRemote := releaseRemoteFactory
	t.Cleanup(func() {
		verifyPublicationTags = previousTags
		buildPublicationSpecs = previousSpecs
		verifyPublicationSequence = previousSequence
		releaseRemoteFactory = previousRemote
	})
	verifyPublicationTags = func(string, string) (int, error) { return 3, nil }
	specs := releaseReconciliationFixture()
	buildPublicationSpecs = func(string, []string, string, string, time.Time) ([]releaseSpec, error) { return specs, nil }
	sequenceVerified := false
	verifyPublicationSequence = func(context.Context, string, string, string, []releaseSpec, releaseRemote, time.Time) error {
		sequenceVerified = true
		return nil
	}
	remote := newFakeReleaseRemote()
	releaseRemoteFactory = func() (releaseRemote, error) { return remote, nil }
	remote.beforeDelete = func() error {
		if !sequenceVerified {
			return errors.New("catalog asset deletion preceded sequence verification")
		}
		return nil
	}
	remote.releases[specs[0].Tag] = exactRemoteRelease(11, specs[0], false)
	remote.releases[specs[1].Tag] = exactRemoteRelease(12, specs[1], true)
	core := exactRemoteRelease(13, specs[2], true)
	for index := range core.Assets {
		switch core.Assets[index].Name {
		case "catalog.json":
			core.Assets[index] = remoteReleaseAsset{ID: 131, Name: "catalog.json", Size: 19, Digest: "sha256:" + strings.Repeat("9", 64)}
		case "catalog.sig":
			core.Assets[index] = remoteReleaseAsset{ID: 132, Name: "catalog.sig", Size: 20, Digest: "sha256:" + strings.Repeat("a", 64)}
		}
	}
	remote.releases[specs[2].Tag] = core

	var stdout, stderr bytes.Buffer
	code := runWithContext(context.Background(), []string{
		"publish-releases", "--root", "/repository", "--commit", strings.Repeat("f", 40),
		"--artifacts", "/artifacts/arm64", "--artifacts", "/artifacts/amd64",
		"--catalog", "/artifacts/catalog.json", "--signature", "/artifacts/catalog.sig",
	}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("partial refresh exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if remote.deleteCalls[specs[2].Tag+"/catalog.json"] != 1 || remote.deleteCalls[specs[2].Tag+"/catalog.sig"] != 1 {
		t.Fatalf("catalog refresh deletions = %v", remote.deleteCalls)
	}
	if remote.publishCalls[specs[0].Tag] != 0 {
		t.Fatal("already-published exact release was republished")
	}
	for _, spec := range specs {
		state := remote.releases[spec.Tag]
		if state.Draft || !exactRemoteAssets(state.Assets, spec.Assets) {
			t.Fatalf("release %q after refresh = %#v", spec.Tag, state)
		}
	}
}

func TestReconcileRejectsCatalogRefreshForPublishedReleaseAndOtherDraftAssets(t *testing.T) {
	specs := releaseReconciliationFixture()
	for name, mutate := range map[string]func(*fakeReleaseRemote){
		"published catalog": func(remote *fakeReleaseRemote) {
			state := exactRemoteRelease(21, specs[2], false)
			state.Assets[len(state.Assets)-1].Digest = "sha256:" + strings.Repeat("e", 64)
			remote.releases[specs[2].Tag] = state
		},
		"non-catalog draft asset": func(remote *fakeReleaseRemote) {
			state := exactRemoteRelease(22, specs[0], true)
			state.Assets[0].Digest = "sha256:" + strings.Repeat("e", 64)
			remote.releases[specs[0].Tag] = state
		},
	} {
		t.Run(name, func(t *testing.T) {
			remote := newFakeReleaseRemote()
			mutate(remote)
			if err := reconcileReleases(context.Background(), remote, specs, strings.Repeat("a", 40)); err == nil {
				t.Fatalf("reconcile accepted conflicting %s", name)
			}
			if remote.mutations != 0 || len(remote.deleteCalls) != 0 {
				t.Fatalf("conflicting %s mutations=%d deletes=%v", name, remote.mutations, remote.deleteCalls)
			}
		})
	}
}

func TestDecodeRemoteReleaseAcceptsOfficialResponseFieldsOutsideReconciliationContract(t *testing.T) {
	data := []byte(`{"id":42,"tag_name":"v0.1.0","name":"bsbctl v0.1.0","draft":true,"upload_url":"https://uploads.github.com/repos/lxdb/bsbctl/releases/42/assets{?name,label}","html_url":"https://github.com/lxdb/bsbctl/releases/tag/v0.1.0","assets":[{"name":"bsbctl.tar.gz","size":10,"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","browser_download_url":"https://example.invalid/bsbctl.tar.gz"}]}`)
	release, err := decodeRemoteRelease(data)
	if err != nil {
		t.Fatal(err)
	}
	if release.ID != 42 || len(release.Assets) != 1 || release.Assets[0].Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("release = %#v", release)
	}
}

func TestReconcileReleasesStagesAndVerifiesEveryDraftBeforePublishing(t *testing.T) {
	specs := releaseReconciliationFixture()
	remote := newFakeReleaseRemote()
	remote.beforePublish = func() error {
		for _, spec := range specs {
			state, exists := remote.releases[spec.Tag]
			if !exists || !exactRemoteAssets(state.Assets, spec.Assets) {
				return errors.New("publish began before every draft had exact assets")
			}
		}
		return nil
	}

	if err := reconcileReleases(context.Background(), remote, specs, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	for _, spec := range specs {
		state := remote.releases[spec.Tag]
		if state.Draft || !exactRemoteAssets(state.Assets, spec.Assets) {
			t.Fatalf("release %q = %#v", spec.Tag, state)
		}
	}
}

func TestReconcileReleasesRejectsConflictsBeforeAnyMutation(t *testing.T) {
	specs := releaseReconciliationFixture()
	remote := newFakeReleaseRemote()
	remote.releases[specs[1].Tag] = remoteRelease{
		ID: 7, Tag: specs[1].Tag, Title: specs[1].Title, Draft: true,
		Assets: []remoteReleaseAsset{{Name: "unexpected.tar.gz", Size: 1, Digest: "sha256:" + strings.Repeat("f", 64)}},
	}

	if err := reconcileReleases(context.Background(), remote, specs, strings.Repeat("a", 40)); err == nil {
		t.Fatal("reconcile accepted a conflicting remote draft")
	}
	if remote.mutations != 0 {
		t.Fatalf("remote mutations = %d, want 0", remote.mutations)
	}
}

func TestReconcileReleasesResumesAfterPartialPublication(t *testing.T) {
	specs := releaseReconciliationFixture()
	remote := newFakeReleaseRemote()
	remote.releases[specs[0].Tag] = exactRemoteRelease(11, specs[0], false)
	remote.releases[specs[1].Tag] = remoteRelease{
		ID: 12, Tag: specs[1].Tag, Title: specs[1].Title, Draft: true,
		Assets: []remoteReleaseAsset{remoteAsset(specs[1].Assets[0])},
	}

	if err := reconcileReleases(context.Background(), remote, specs, strings.Repeat("b", 40)); err != nil {
		t.Fatal(err)
	}
	if remote.publishCalls[specs[0].Tag] != 0 {
		t.Fatal("already-published exact release was published again")
	}
	for _, spec := range specs {
		state := remote.releases[spec.Tag]
		if state.Draft || !exactRemoteAssets(state.Assets, spec.Assets) {
			t.Fatalf("release %q = %#v", spec.Tag, state)
		}
	}
}

func releaseReconciliationFixture() []releaseSpec {
	return []releaseSpec{
		{Tag: "plugin/codex-quota/v0.1.0", Title: "Codex quota plugin v0.1.0", Assets: []releaseAsset{
			{Name: "codex-arm64.tar.gz", Path: "/artifacts/codex-arm64.tar.gz", Size: 11, SHA256: strings.Repeat("1", 64)},
			{Name: "codex-amd64.tar.gz", Path: "/artifacts/codex-amd64.tar.gz", Size: 12, SHA256: strings.Repeat("2", 64)},
		}},
		{Tag: "plugin/mac-resources/v0.1.0", Title: "macOS resources plugin v0.1.0", Assets: []releaseAsset{
			{Name: "mac-arm64.tar.gz", Path: "/artifacts/mac-arm64.tar.gz", Size: 13, SHA256: strings.Repeat("3", 64)},
			{Name: "mac-amd64.tar.gz", Path: "/artifacts/mac-amd64.tar.gz", Size: 14, SHA256: strings.Repeat("4", 64)},
		}},
		{Tag: "v0.1.0", Title: "bsbctl v0.1.0", Assets: []releaseAsset{
			{Name: "bsbctl-arm64.tar.gz", Path: "/artifacts/bsbctl-arm64.tar.gz", Size: 15, SHA256: strings.Repeat("5", 64)},
			{Name: "bsbctl-amd64.tar.gz", Path: "/artifacts/bsbctl-amd64.tar.gz", Size: 16, SHA256: strings.Repeat("6", 64)},
			{Name: "catalog.json", Path: "/artifacts/catalog.json", Size: 17, SHA256: strings.Repeat("7", 64)},
			{Name: "catalog.sig", Path: "/artifacts/catalog.sig", Size: 18, SHA256: strings.Repeat("8", 64)},
		}},
	}
}

func signedPublicationCatalog(private ed25519.PrivateKey, sequence uint64, generatedAt time.Time) ([]byte, []byte) {
	catalogData := []byte(fmt.Sprintf(`{"version":1,"channel":"stable","sequence":%d,"generated_at":%q,"plugins":[{"id":"dev.bsbctl.codex-quota","version":"0.1.0","os":"darwin","arch":"arm64","url":"https://example.invalid/plugin.tar.gz","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","compressed_size":1,"archive_format":"tar.gz","executable":"bsbctl-plugin-codex-quota","manifest":"manifest.json"}]}`,
		sequence, generatedAt.UTC().Format(time.RFC3339)))
	signature := ed25519.Sign(private, catalogData)
	signatureData := []byte(fmt.Sprintf(`{"key_id":"stable","algorithm":"ed25519","signature":%q}`,
		base64.StdEncoding.EncodeToString(signature)))
	return catalogData, signatureData
}

func publicationSequenceFixture(t *testing.T, catalogData, signatureData []byte, predecessor string) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	catalogPath := filepath.Join(root, "artifacts", "catalog.json")
	signaturePath := filepath.Join(root, "artifacts", "catalog.sig")
	writeReleaseFile(t, catalogPath, string(catalogData))
	writeReleaseFile(t, signaturePath, string(signatureData))
	writeReleaseFile(t, filepath.Join(root, "release", "catalog-predecessor.json"), predecessor)
	return root, catalogPath, signaturePath
}

func publicationCatalogSpecs(catalogPath, signaturePath string, catalogData, signatureData []byte) []releaseSpec {
	catalogDigest := sha256.Sum256(catalogData)
	signatureDigest := sha256.Sum256(signatureData)
	return []releaseSpec{{
		Tag: "v0.1.0", Title: "bsbctl v0.1.0",
		Assets: []releaseAsset{
			{Name: "catalog.json", Path: catalogPath, Size: int64(len(catalogData)), SHA256: fmt.Sprintf("%x", catalogDigest)},
			{Name: "catalog.sig", Path: signaturePath, Size: int64(len(signatureData)), SHA256: fmt.Sprintf("%x", signatureDigest)},
		},
	}}
}

func installFakeCatalogPredecessor(remote *fakeReleaseRemote, tag string, catalogData, signatureData []byte) {
	catalogDigest := sha256.Sum256(catalogData)
	signatureDigest := sha256.Sum256(signatureData)
	remote.releases[tag] = remoteRelease{
		ID: 9, Tag: tag, Title: "prior", Draft: false,
		Assets: []remoteReleaseAsset{
			{ID: 91, Name: "catalog.json", Size: int64(len(catalogData)), Digest: "sha256:" + fmt.Sprintf("%x", catalogDigest)},
			{ID: 92, Name: "catalog.sig", Size: int64(len(signatureData)), Digest: "sha256:" + fmt.Sprintf("%x", signatureDigest)},
		},
	}
	remote.downloads[tag+"/catalog.json"] = append([]byte(nil), catalogData...)
	remote.downloads[tag+"/catalog.sig"] = append([]byte(nil), signatureData...)
}

type fakeReleaseRemote struct {
	releases      map[string]remoteRelease
	downloads     map[string][]byte
	nextID        int64
	mutations     int
	publishCalls  map[string]int
	beforePublish func() error
	deleteCalls   map[string]int
	beforeDelete  func() error
}

func newFakeReleaseRemote() *fakeReleaseRemote {
	return &fakeReleaseRemote{
		releases: make(map[string]remoteRelease), downloads: make(map[string][]byte),
		nextID: 100, publishCalls: make(map[string]int), deleteCalls: make(map[string]int),
	}
}

func (remote *fakeReleaseRemote) Get(_ context.Context, tag string) (remoteRelease, bool, error) {
	release, exists := remote.releases[tag]
	return release, exists, nil
}

func (remote *fakeReleaseRemote) DownloadAsset(_ context.Context, tag, name string) ([]byte, error) {
	data, exists := remote.downloads[tag+"/"+name]
	if !exists {
		return nil, errors.New("asset is unavailable")
	}
	return append([]byte(nil), data...), nil
}

func (remote *fakeReleaseRemote) CreateDraft(_ context.Context, spec releaseSpec, _ string) (remoteRelease, error) {
	remote.mutations++
	remote.nextID++
	release := remoteRelease{ID: remote.nextID, Tag: spec.Tag, Title: spec.Title, Draft: true}
	remote.releases[spec.Tag] = release
	return release, nil
}

func (remote *fakeReleaseRemote) UploadAsset(_ context.Context, release remoteRelease, asset releaseAsset) error {
	remote.mutations++
	release.Assets = append(release.Assets, remoteAsset(asset))
	remote.releases[release.Tag] = release
	return nil
}

func (remote *fakeReleaseRemote) DeleteAsset(_ context.Context, release remoteRelease, asset remoteReleaseAsset) error {
	if remote.beforeDelete != nil {
		if err := remote.beforeDelete(); err != nil {
			return err
		}
	}
	state, exists := remote.releases[release.Tag]
	if !exists || state.ID != release.ID || asset.ID < 1 {
		return errors.New("asset deletion target is invalid")
	}
	found := -1
	for index, current := range state.Assets {
		if current.ID == asset.ID && current.Name == asset.Name {
			if found != -1 {
				return errors.New("asset deletion target is ambiguous")
			}
			found = index
		}
	}
	if found == -1 {
		return errors.New("asset deletion target is unavailable")
	}
	remote.mutations++
	remote.deleteCalls[release.Tag+"/"+asset.Name]++
	state.Assets = append(state.Assets[:found], state.Assets[found+1:]...)
	remote.releases[release.Tag] = state
	return nil
}

func (remote *fakeReleaseRemote) Publish(_ context.Context, release remoteRelease) error {
	if remote.beforePublish != nil {
		if err := remote.beforePublish(); err != nil {
			return err
		}
	}
	remote.mutations++
	remote.publishCalls[release.Tag]++
	release.Draft = false
	remote.releases[release.Tag] = release
	return nil
}

func exactRemoteRelease(id int64, spec releaseSpec, draft bool) remoteRelease {
	assets := make([]remoteReleaseAsset, 0, len(spec.Assets))
	for _, asset := range spec.Assets {
		assets = append(assets, remoteAsset(asset))
	}
	return remoteRelease{ID: id, Tag: spec.Tag, Title: spec.Title, Draft: draft, Assets: assets}
}

func remoteAsset(asset releaseAsset) remoteReleaseAsset {
	return remoteReleaseAsset{Name: asset.Name, Size: asset.Size, Digest: "sha256:" + asset.SHA256}
}
