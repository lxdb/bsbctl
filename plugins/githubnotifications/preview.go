//go:build preview

package githubnotifications

import (
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

// PreviewScenes returns deterministic production presentations built from
// public mock notification data. It performs no provider or desktop effects.
func PreviewScenes(now time.Time) []protocol.Scene {
	now = now.UTC()
	c := testPreviewConfig(false)
	state := newState(c, Identity{ID: 42})
	const title = "GitHub Notifications release"
	review := previewNotification("17", "review_requested", title, now)
	state.apply(fetchResult{Complete: true, Items: []notification{review}}, nil, now)
	scenes := []protocol.Scene{
		attentionScene(c, state.ordered()[0]),
	}
	mention := previewNotification("17", "mention", title, now.Add(time.Second))
	state.apply(fetchResult{Complete: true, Items: []notification{mention}}, nil, now.Add(time.Second))
	return append(scenes, attentionScene(c, state.ordered()[0]))
}

func testPreviewConfig(rearDetails bool) Config {
	return Config{
		Repositories: []Repository{{Name: "lxdb/bsbctl", Alias: "bsbctl"}},
		Label:        "GH", RearDetails: rearDetails, PollInterval: time.Minute, Configured: true,
	}
}

func previewNotification(id, reason, title string, updated time.Time) notification {
	return notification{
		ThreadID: id, Reason: reason, Unread: true, RepositoryID: 7, Repository: "lxdb/bsbctl", Alias: "bsbctl",
		SubjectType: "PullRequest", Title: title, SubjectURL: "https://api.github.com/repos/lxdb/bsbctl/pulls/17", UpdatedAt: updated,
	}
}
