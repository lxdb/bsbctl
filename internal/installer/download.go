package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/lxdb/bsbctl/internal/catalog"
)

type Downloader struct {
	Client  *http.Client
	Timeout time.Duration
}

// Download returns an authenticated artifact as an open descriptor whose
// private temporary pathname has been unlinked. The caller owns the descriptor
// and must close it. The descriptor is initially positioned at the beginning,
// but consumers may change its offset.
func (downloader Downloader) Download(ctx context.Context, directory string, entry catalog.Entry) (result *os.File, resultErr error) {
	if entry.CompressedSize < 1 || entry.CompressedSize > catalog.MaxArtifactBytes || !safeHTTPS(entry.URL) {
		return nil, errorCode(CodeDownloadFailed)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errorCode(CodeDownloadFailed)
	}
	if downloader.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, downloader.Timeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.URL, nil)
	if err != nil {
		return nil, errorCode(CodeDownloadFailed)
	}
	client := http.DefaultClient
	if downloader.Client != nil {
		client = downloader.Client
	}
	clientCopy := *client
	previousRedirect := client.CheckRedirect
	clientCopy.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !safeURL(request.URL) || len(via) >= 10 {
			return errorCode(CodeDownloadFailed)
		}
		if previousRedirect != nil {
			if err := previousRedirect(request, via); err != nil {
				return errorCode(CodeDownloadFailed)
			}
		}
		return nil
	}
	response, err := clientCopy.Do(request)
	if err != nil {
		return nil, errorCode(CodeDownloadFailed)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || (response.ContentLength >= 0 && response.ContentLength != entry.CompressedSize) {
		return nil, errorCode(CodeDownloadFailed)
	}
	temporary, err := os.CreateTemp(directory, ".bsbctl-download-*")
	if err != nil {
		return nil, errorCode(CodeDownloadFailed)
	}
	temporaryPath := temporary.Name()
	closed := false
	keep := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return nil, errorCode(CodeDownloadFailed)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(response.Body, entry.CompressedSize+1))
	if err != nil || written != entry.CompressedSize || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		return nil, errorCode(CodeDownloadFailed)
	}
	if err := temporary.Sync(); err != nil {
		return nil, errorCode(CodeDownloadFailed)
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		return nil, errorCode(CodeDownloadFailed)
	}
	if err := os.Remove(temporaryPath); err != nil {
		return nil, errorCode(CodeDownloadFailed)
	}
	closed = true
	keep = true
	return temporary, nil
}

func safeHTTPS(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && safeURL(parsed)
}

func safeURL(parsed *url.URL) bool {
	return parsed != nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Opaque == "" && parsed.Fragment == ""
}
