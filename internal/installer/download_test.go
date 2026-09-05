package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/catalog"
)

func TestDownloaderStreamsAuthenticatedArtifactToOwnedTemporaryFile(t *testing.T) {
	body := []byte("authenticated archive")
	entry := downloadEntry(body)
	directory := t.TempDir()
	downloader := Downloader{Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusOK, io.NopCloser(bytes.NewReader(body)), int64(len(body))), nil
	})}}

	artifact, err := downloader.Download(context.Background(), directory, entry)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer artifact.Close()
	if filepath.Dir(artifact.Name()) != directory {
		t.Fatalf("temporary path %q is not in %q", artifact.Name(), directory)
	}
	if _, err := os.Stat(artifact.Name()); !os.IsNotExist(err) {
		t.Fatalf("private temporary pathname remains linked: %v", err)
	}
	data, err := io.ReadAll(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, body) {
		t.Fatalf("downloaded data = %q", data)
	}
	info, err := artifact.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("temporary mode = %o", info.Mode().Perm())
	}
}

func TestDownloaderRejectsUnsafeResponsesAndRedactsDiagnostics(t *testing.T) {
	body := []byte("authenticated archive")
	valid := downloadEntry(body)
	tests := []struct {
		name     string
		entry    catalog.Entry
		client   *http.Client
		redirect string
	}{
		{
			name:  "status and secret body",
			entry: valid,
			client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return response(request, http.StatusUnauthorized, io.NopCloser(strings.NewReader("token=body-secret")), 17), nil
			})},
		},
		{
			name:  "short body",
			entry: func() catalog.Entry { value := valid; value.CompressedSize++; return value }(),
			client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return response(request, http.StatusOK, io.NopCloser(bytes.NewReader(body)), int64(len(body))), nil
			})},
		},
		{
			name:  "long body",
			entry: func() catalog.Entry { value := valid; value.CompressedSize--; return value }(),
			client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return response(request, http.StatusOK, io.NopCloser(bytes.NewReader(body)), int64(len(body))), nil
			})},
		},
		{
			name:  "wrong digest",
			entry: func() catalog.Entry { value := valid; value.SHA256 = strings.Repeat("0", 64); return value }(),
			client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return response(request, http.StatusOK, io.NopCloser(bytes.NewReader(body)), int64(len(body))), nil
			})},
		},
		{
			name: "credentialed redirect", entry: valid,
			redirect: "https://user:redirect-secret@other.invalid/archive",
		},
		{
			name: "non https redirect", entry: valid,
			redirect: "http://other.invalid/archive",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			client := test.client
			redirected := false
			if test.redirect != "" {
				client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					if request.URL.String() == test.entry.URL {
						response := response(request, http.StatusFound, http.NoBody, 0)
						response.Header.Set("Location", test.redirect)
						return response, nil
					}
					redirected = true
					return response(request, http.StatusOK, io.NopCloser(bytes.NewReader(body)), int64(len(body))), nil
				})}
			}
			artifact, err := (Downloader{Client: client}).Download(t.Context(), directory, test.entry)
			if artifact != nil {
				defer artifact.Close()
			}
			if redirected {
				t.Fatal("unsafe redirect reached the destination transport")
			}
			if CodeOf(err) != CodeDownloadFailed {
				t.Fatalf("error = %v, code = %q", err, CodeOf(err))
			}
			message := err.Error()
			for _, secret := range []string{"example.invalid", "other.invalid", "body-secret", "redirect-secret", test.entry.URL} {
				if secret != "" && strings.Contains(message, secret) {
					t.Fatalf("error exposes %q: %q", secret, message)
				}
			}
			entries, readErr := os.ReadDir(directory)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("failed download left %d files", len(entries))
			}
		})
	}
}

func TestDownloaderEnforcesHardBoundAndContextTimeout(t *testing.T) {
	body := []byte("authenticated archive")
	oversized := downloadEntry(body)
	oversized.CompressedSize = catalog.MaxArtifactBytes + 1
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("oversized signed artifact reached transport")
		return nil, nil
	})}
	if _, err := (Downloader{Client: client}).Download(context.Background(), t.TempDir(), oversized); CodeOf(err) != CodeDownloadFailed {
		t.Fatalf("oversized error = %v", err)
	}

	canceled := make(chan struct{})
	client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		close(canceled)
		return nil, request.Context().Err()
	})}
	_, err := (Downloader{Client: client, Timeout: time.Millisecond}).Download(context.Background(), t.TempDir(), downloadEntry(body))
	if CodeOf(err) != CodeDownloadFailed {
		t.Fatalf("timeout error = %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("request context was not canceled by timeout")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func response(request *http.Request, status int, body io.ReadCloser, length int64) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: body, ContentLength: length, Header: make(http.Header), Request: request}
}

func downloadEntry(body []byte) catalog.Entry {
	digest := sha256.Sum256(body)
	return catalog.Entry{
		ID: "dev.bsbctl.ball8", Version: "1.0.0", OS: "darwin", Arch: "arm64",
		URL: "https://example.invalid/archive.tar.gz", SHA256: hex.EncodeToString(digest[:]),
		CompressedSize: int64(len(body)), ArchiveFormat: "tar.gz", Executable: "bsbctl-plugin-ball8", Manifest: "manifest.json",
	}
}
