package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func reconcileReleases(ctx context.Context, remote releaseRemote, specs []releaseSpec, builtCommit string) error {
	if remote == nil || len(specs) == 0 || !releaseCommitPattern.MatchString(builtCommit) {
		return errors.New("release reconciliation inputs are invalid")
	}
	states := make(map[string]remoteRelease, len(specs))
	replacements := make(map[string][]remoteReleaseAsset, len(specs))
	seenTags := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		if err := validateReleaseSpec(spec); err != nil {
			return err
		}
		if _, duplicate := seenTags[spec.Tag]; duplicate {
			return errors.New("release reconciliation repeats a tag")
		}
		seenTags[spec.Tag] = struct{}{}
		state, exists, err := remote.Get(ctx, spec.Tag)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if state.Draft {
			refresh, err := inspectDraftRelease(state, spec)
			if err != nil {
				return err
			}
			replacements[spec.Tag] = refresh
		} else if err := validateRemoteRelease(state, spec, true); err != nil {
			return err
		}
		states[spec.Tag] = state
	}

	for _, spec := range specs {
		state, exists := states[spec.Tag]
		if !exists {
			var err error
			state, err = remote.CreateDraft(ctx, spec, builtCommit)
			if err != nil {
				return err
			}
			if err := validateRemoteRelease(state, spec, false); err != nil || !state.Draft {
				return errors.New("created release draft is invalid")
			}
		}
		if !state.Draft {
			states[spec.Tag] = state
			continue
		}
		for _, asset := range replacements[spec.Tag] {
			if err := remote.DeleteAsset(ctx, state, asset); err != nil {
				return err
			}
		}
		if len(replacements[spec.Tag]) != 0 {
			var err error
			state, exists, err = remote.Get(ctx, spec.Tag)
			if err != nil || !exists || !state.Draft {
				return errors.New("release draft is unavailable after catalog refresh")
			}
			if refresh, err := inspectDraftRelease(state, spec); err != nil || len(refresh) != 0 {
				return errors.New("release draft catalog refresh is incomplete")
			}
		}
		missing, err := missingReleaseAssets(state.Assets, spec.Assets)
		if err != nil {
			return err
		}
		for _, asset := range missing {
			if err := remote.UploadAsset(ctx, state, asset); err != nil {
				return err
			}
			state.Assets = append(state.Assets, remoteReleaseAsset{Name: asset.Name, Size: asset.Size, Digest: "sha256:" + asset.SHA256})
		}
		states[spec.Tag] = state
	}

	for _, spec := range specs {
		state, exists, err := remote.Get(ctx, spec.Tag)
		if err != nil || !exists {
			return errors.New("staged release draft is unavailable")
		}
		if err := validateRemoteRelease(state, spec, true); err != nil {
			return err
		}
		states[spec.Tag] = state
	}

	for _, spec := range specs {
		state := states[spec.Tag]
		if state.Draft {
			if err := remote.Publish(ctx, state); err != nil {
				return err
			}
		}
	}
	for _, spec := range specs {
		state, exists, err := remote.Get(ctx, spec.Tag)
		if err != nil || !exists || state.Draft {
			return errors.New("published release reconciliation is incomplete")
		}
		if err := validateRemoteRelease(state, spec, true); err != nil {
			return err
		}
	}
	return nil
}

func inspectDraftRelease(state remoteRelease, spec releaseSpec) ([]remoteReleaseAsset, error) {
	if state.ID < 1 || state.Tag != spec.Tag || state.Title != spec.Title || !state.Draft {
		return nil, fmt.Errorf("release %q conflicts with the tracked release plan", spec.Tag)
	}
	expected := make(map[string]releaseAsset, len(spec.Assets))
	refreshable := make(map[string]struct{}, 2)
	for _, asset := range spec.Assets {
		expected[asset.Name] = asset
	}
	if strings.HasPrefix(spec.Tag, "v") {
		if _, catalogExists := expected["catalog.json"]; catalogExists {
			if _, signatureExists := expected["catalog.sig"]; signatureExists {
				refreshable["catalog.json"] = struct{}{}
				refreshable["catalog.sig"] = struct{}{}
			}
		}
	}
	seen := make(map[string]struct{}, len(state.Assets))
	var replacements []remoteReleaseAsset
	for _, asset := range state.Assets {
		expectation, exists := expected[asset.Name]
		if !exists {
			return nil, errors.New("remote release contains a conflicting asset")
		}
		if _, duplicate := seen[asset.Name]; duplicate {
			return nil, errors.New("remote release repeats an asset")
		}
		seen[asset.Name] = struct{}{}
		if asset.Size == expectation.Size && asset.Digest == "sha256:"+expectation.SHA256 {
			continue
		}
		if _, allowed := refreshable[asset.Name]; !allowed || asset.ID < 1 {
			return nil, errors.New("remote release contains a conflicting asset")
		}
		replacements = append(replacements, asset)
	}
	return replacements, nil
}

func validateReleaseSpec(spec releaseSpec) error {
	if spec.Tag == "" || spec.Title == "" || len(spec.Assets) == 0 {
		return errors.New("release specification is invalid")
	}
	seen := make(map[string]struct{}, len(spec.Assets))
	for _, asset := range spec.Assets {
		if asset.Name == "" || asset.Path == "" || asset.Size < 1 || !releaseSHA256Pattern.MatchString(asset.SHA256) {
			return errors.New("release asset specification is invalid")
		}
		if _, duplicate := seen[asset.Name]; duplicate {
			return errors.New("release asset specification repeats a name")
		}
		seen[asset.Name] = struct{}{}
	}
	return nil
}

func validateRemoteRelease(state remoteRelease, spec releaseSpec, requireComplete bool) error {
	if state.ID < 1 || state.Tag != spec.Tag || state.Title != spec.Title {
		return fmt.Errorf("release %q conflicts with the tracked release plan", spec.Tag)
	}
	missing, err := missingReleaseAssets(state.Assets, spec.Assets)
	if err != nil {
		return err
	}
	if requireComplete && len(missing) != 0 {
		return fmt.Errorf("release %q does not contain the exact asset set", spec.Tag)
	}
	return nil
}

func missingReleaseAssets(remoteAssets []remoteReleaseAsset, expected []releaseAsset) ([]releaseAsset, error) {
	expectedByName := make(map[string]releaseAsset, len(expected))
	for _, asset := range expected {
		expectedByName[asset.Name] = asset
	}
	seen := make(map[string]struct{}, len(remoteAssets))
	for _, asset := range remoteAssets {
		expectation, exists := expectedByName[asset.Name]
		if !exists || asset.Size != expectation.Size || asset.Digest != "sha256:"+expectation.SHA256 {
			return nil, errors.New("remote release contains a conflicting asset")
		}
		if _, duplicate := seen[asset.Name]; duplicate {
			return nil, errors.New("remote release repeats an asset")
		}
		seen[asset.Name] = struct{}{}
	}
	missing := make([]releaseAsset, 0, len(expected)-len(seen))
	for _, asset := range expected {
		if _, exists := seen[asset.Name]; !exists {
			missing = append(missing, asset)
		}
	}
	return missing, nil
}
