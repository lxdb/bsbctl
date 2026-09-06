package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestGitHubReleaseLookupFindsDraftBeyondFirstPage(t *testing.T) {
	want := remoteRelease{
		ID: 42, Tag: "plugin/calendar/v0.1.0", Title: "Calendar plugin v0.1.0", Draft: true,
		Assets: []remoteReleaseAsset{{ID: 7, Name: "calendar.tar.gz", Size: 123, Digest: "sha256:reviewed"}},
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/releases/tags/" + want.Tag:
			// GitHub's by-tag endpoint does not return draft releases.
			http.NotFound(w, r)
		case "/repos/owner/repo/releases":
			switch r.URL.Query().Get("page") {
			case "1":
				published := make([]remoteRelease, 30)
				for i := range published {
					published[i] = remoteRelease{ID: int64(i + 100), Tag: fmt.Sprintf("v0.0.%d", i)}
				}
				_ = json.NewEncoder(w).Encode(published)
			case "2":
				_ = json.NewEncoder(w).Encode([]remoteRelease{want})
			default:
				t.Errorf("unexpected release-list page: %s", r.URL.RawQuery)
				http.Error(w, "unexpected page", http.StatusBadRequest)
			}
		default:
			t.Errorf("unexpected lookup endpoint: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	remote := githubReleaseRemote{client: server.Client(), apiBase: server.URL, repository: "owner/repo", token: "test-token"}
	got, exists, err := remote.Get(t.Context(), want.Tag)
	if err != nil || !exists || !reflect.DeepEqual(got, want) {
		t.Fatalf("draft lookup = %#v, exists=%v, error=%v; want %#v", got, exists, err, want)
	}
}

func TestGitHubReleaseLookupDoesNotTreatListFailuresAsMissingDrafts(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "missing", status: http.StatusOK, body: `[]`},
		{name: "forbidden", status: http.StatusForbidden, body: `{}`, wantErr: true},
		{name: "invalid response", status: http.StatusOK, body: `[`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/owner/repo/releases/tags/v0.1.0" {
					http.NotFound(w, r)
					return
				}
				if r.URL.Path != "/repos/owner/repo/releases" {
					t.Errorf("unexpected lookup endpoint: %s", r.URL)
				}
				w.WriteHeader(test.status)
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			remote := githubReleaseRemote{client: server.Client(), apiBase: server.URL, repository: "owner/repo", token: "test-token"}
			_, exists, err := remote.Get(t.Context(), "v0.1.0")
			if exists || (err != nil) != test.wantErr {
				t.Fatalf("lookup exists=%v error=%v; want missing with error=%v", exists, err, test.wantErr)
			}
		})
	}
}
