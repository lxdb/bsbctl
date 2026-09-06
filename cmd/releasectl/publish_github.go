package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type githubReleaseRemote struct {
	client     *http.Client
	apiBase    string
	repository string
	token      string
}

func newGitHubReleaseRemote() (releaseRemote, error) {
	repository := os.Getenv("GITHUB_REPOSITORY")
	token := os.Getenv("GH_TOKEN")
	apiBase := os.Getenv("GITHUB_API_URL")
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	parsed, err := url.Parse(apiBase)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !githubRepositoryPattern.MatchString(repository) || token == "" {
		return nil, errors.New("GitHub release environment is invalid")
	}
	return &githubReleaseRemote{
		client: &http.Client{Timeout: 30 * time.Second}, apiBase: strings.TrimRight(parsed.String(), "/"),
		repository: repository, token: token,
	}, nil
}

func (remote *githubReleaseRemote) Get(ctx context.Context, tag string) (remoteRelease, bool, error) {
	endpoint := remote.repositoryEndpoint("/releases/tags/" + url.PathEscape(tag))
	data, status, err := remote.request(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return remoteRelease{}, false, err
	}
	if status == http.StatusNotFound {
		// GitHub's by-tag endpoint only returns published releases.
		return remote.findReleaseInList(ctx, tag)
	}
	if status != http.StatusOK {
		return remoteRelease{}, false, errors.New("GitHub release lookup failed")
	}
	release, err := decodeRemoteRelease(data)
	return release, err == nil, err
}

func (remote *githubReleaseRemote) findReleaseInList(ctx context.Context, tag string) (remoteRelease, bool, error) {
	const pageSize = 30
	for page := 1; ; page++ {
		endpoint := remote.repositoryEndpoint(fmt.Sprintf("/releases?per_page=%d&page=%d", pageSize, page))
		data, status, err := remote.request(ctx, http.MethodGet, endpoint, nil, "")
		if err != nil {
			return remoteRelease{}, false, err
		}
		if status != http.StatusOK {
			return remoteRelease{}, false, errors.New("GitHub release lookup failed")
		}
		var releases []remoteRelease
		if err := json.Unmarshal(data, &releases); err != nil {
			return remoteRelease{}, false, errors.New("GitHub release response is invalid")
		}
		for _, release := range releases {
			if release.Tag == tag {
				return release, true, nil
			}
		}
		if len(releases) < pageSize {
			return remoteRelease{}, false, nil
		}
	}
}

func (remote *githubReleaseRemote) DownloadAsset(ctx context.Context, tag, name string) ([]byte, error) {
	release, exists, err := remote.Get(ctx, tag)
	if err != nil || !exists || release.Draft {
		return nil, errors.New("published GitHub release asset is unavailable")
	}
	var assetID int64
	for _, asset := range release.Assets {
		if asset.Name != name {
			continue
		}
		if assetID != 0 || asset.ID < 1 {
			return nil, errors.New("published GitHub release asset is ambiguous")
		}
		assetID = asset.ID
	}
	if assetID == 0 {
		return nil, errors.New("published GitHub release asset is unavailable")
	}
	endpoint := remote.repositoryEndpoint(fmt.Sprintf("/releases/assets/%d", assetID))
	data, status, err := remote.requestWithAccept(ctx, http.MethodGet, endpoint, nil, "", "application/octet-stream")
	if err != nil || status != http.StatusOK || len(data) == 0 {
		return nil, errors.New("published GitHub release asset download failed")
	}
	return data, nil
}

func (remote *githubReleaseRemote) CreateDraft(ctx context.Context, spec releaseSpec, builtCommit string) (remoteRelease, error) {
	body, err := json.Marshal(struct {
		TagName              string `json:"tag_name"`
		TargetCommitish      string `json:"target_commitish"`
		Name                 string `json:"name"`
		Draft                bool   `json:"draft"`
		Prerelease           bool   `json:"prerelease"`
		GenerateReleaseNotes bool   `json:"generate_release_notes"`
	}{spec.Tag, builtCommit, spec.Title, true, false, false})
	if err != nil {
		return remoteRelease{}, err
	}
	data, status, err := remote.request(ctx, http.MethodPost, remote.repositoryEndpoint("/releases"), bytes.NewReader(body), "application/json")
	if err != nil || status != http.StatusCreated {
		return remoteRelease{}, errors.New("GitHub release draft creation failed")
	}
	return decodeRemoteRelease(data)
}

func (remote *githubReleaseRemote) UploadAsset(ctx context.Context, release remoteRelease, asset releaseAsset) error {
	current, err := releaseAssetFromFile(asset.Path, asset.Name)
	if err != nil || current.Size != asset.Size || current.SHA256 != asset.SHA256 {
		return errors.New("release asset changed before upload")
	}
	uploadURL, err := remote.validUploadURL(release.UploadURL, asset.Name)
	if err != nil {
		return err
	}
	file, err := os.Open(asset.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, file)
	if err != nil {
		return errors.New("GitHub release upload request is invalid")
	}
	request.ContentLength = asset.Size
	_, status, err := remote.send(request, "application/octet-stream", "application/vnd.github+json")
	if err != nil || status != http.StatusCreated {
		return errors.New("GitHub release asset upload failed")
	}
	return nil
}

func (remote *githubReleaseRemote) DeleteAsset(ctx context.Context, release remoteRelease, asset remoteReleaseAsset) error {
	if release.ID < 1 || asset.ID < 1 || asset.Name == "" {
		return errors.New("GitHub release asset deletion target is invalid")
	}
	endpoint := remote.repositoryEndpoint(fmt.Sprintf("/releases/assets/%d", asset.ID))
	_, status, err := remote.request(ctx, http.MethodDelete, endpoint, nil, "")
	if err != nil || status != http.StatusNoContent {
		return errors.New("GitHub release asset deletion failed")
	}
	return nil
}

func (remote *githubReleaseRemote) Publish(ctx context.Context, release remoteRelease) error {
	body := bytes.NewReader([]byte(`{"draft":false}`))
	_, status, err := remote.request(ctx, http.MethodPatch, remote.repositoryEndpoint(fmt.Sprintf("/releases/%d", release.ID)), body, "application/json")
	if err != nil || status != http.StatusOK {
		return errors.New("GitHub release publication failed")
	}
	return nil
}

func (remote *githubReleaseRemote) repositoryEndpoint(suffix string) string {
	return remote.apiBase + "/repos/" + remote.repository + suffix
}

func (remote *githubReleaseRemote) validUploadURL(template, name string) (string, error) {
	const suffix = "{?name,label}"
	if !strings.HasSuffix(template, suffix) {
		return "", errors.New("GitHub release upload URL is invalid")
	}
	parsed, err := url.Parse(strings.TrimSuffix(template, suffix))
	api, apiErr := url.Parse(remote.apiBase)
	allowedHost := api.Host
	if api.Host == "api.github.com" {
		allowedHost = "uploads.github.com"
	}
	if err != nil || apiErr != nil || parsed.Scheme != "https" || parsed.Host != allowedHost || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("GitHub release upload URL is invalid")
	}
	query := parsed.Query()
	query.Set("name", name)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func (remote *githubReleaseRemote) request(ctx context.Context, method, endpoint string, body io.Reader, contentType string) ([]byte, int, error) {
	return remote.requestWithAccept(ctx, method, endpoint, body, contentType, "application/vnd.github+json")
}

func (remote *githubReleaseRemote) requestWithAccept(ctx context.Context, method, endpoint string, body io.Reader, contentType, accept string) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, 0, errors.New("GitHub release request is invalid")
	}
	return remote.send(request, contentType, accept)
}

func (remote *githubReleaseRemote) send(request *http.Request, contentType, accept string) ([]byte, int, error) {
	request.Header.Set("Accept", accept)
	request.Header.Set("Authorization", "Bearer "+remote.token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "bsbctl-releasectl")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := remote.client.Do(request)
	if err != nil {
		return nil, 0, errors.New("GitHub release request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, githubReleaseResponseLimit+1))
	if err != nil || len(data) > githubReleaseResponseLimit {
		return nil, 0, errors.New("GitHub release response is invalid")
	}
	return data, response.StatusCode, nil
}
