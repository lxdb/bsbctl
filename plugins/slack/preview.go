//go:build preview

package slack

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

// PreviewScenes returns public-safe fixtures through the production reducer and
// scene builders. It performs no provider, desktop, or configuration I/O.
// Order: unconfigured, empty, mention, DM, opted-in preview, list, Dismiss, stale, gap, auth.
func PreviewScenes(now time.Time) []protocol.Scene {
	cfg := config{configured: true, appID: "A123", workspaceID: "T123", userID: "U123", label: "SLK", channels: map[string]string{"C123": "BUILD"}, directMessages: true, watchParticipatedThreads: true, frontMessagePreview: true}
	state := newState(cfg, "U123")
	snap := workerSnapshot{Phase: "ready", Fresh: true, LastSuccess: now, FreshUntil: now.Add(30 * time.Second)}
	idle := config{label: "SLK"}
	scenes := []protocol.Scene{
		panelScene(idle, workerSnapshot{Phase: "unconfigured"}, &panelSession{level: panelList}, now),
		panelScene(cfg, snap, &panelSession{level: panelList}, now),
	}
	timestamp := strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)
	events := []string{
		`{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"` + timestamp + `.000001","event_ts":"` + timestamp + `.000001","text":"Release checklist is ready"}`,
		`{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"` + timestamp + `.000002","event_ts":"` + timestamp + `.000002","text":"<@U123> please review the release checklist"}`,
	}
	for i, event := range events {
		raw, _ := json.Marshal(map[string]any{"type": "event_callback", "api_app_id": "A123", "team_id": "T123", "event_id": []string{"preview-dm", "preview-mention"}[i], "authorizations": []any{map[string]any{"team_id": "T123", "user_id": "U123", "is_bot": false}}, "event": json.RawMessage(event)})
		_, _ = state.apply(raw, now.Add(-time.Minute))
	}
	snap.Items = state.items()
	mention, direct := snap.Items[0], snap.Items[0]
	for _, item := range snap.Items {
		if item.Mention {
			mention = item
		}
		if item.Kind == "dm" {
			direct = item
			break
		}
	}
	cfg.frontMessagePreview = false
	scenes = append(scenes, detailScene(cfg, snap, mention, 0, now), detailScene(cfg, snap, direct, 0, now))
	cfg.frontMessagePreview = true
	panel := &panelSession{level: panelDetail, target: direct}
	scenes = append(scenes, panelScene(cfg, snap, panel, now))
	cfg.frontMessagePreview = false
	scenes = append(scenes, panelScene(cfg, snap, &panelSession{level: panelList}, now))
	panel.level = panelDismiss
	scenes = append(scenes, panelScene(cfg, snap, panel, now))
	snap.Fresh = false
	snap.Phase = "degraded"
	scenes = append(scenes, connectionScene(snap))
	snap.Fresh = true
	snap.Gap = true
	scenes = append(scenes, connectionScene(snap))
	snap.Fresh = false
	snap.Phase = "auth_required"
	scenes = append(scenes, connectionScene(snap))
	return scenes
}
