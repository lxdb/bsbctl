package githubnotifications

import (
	"fmt"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const canvasColor = "#071522FF"
const textColor = "#EAF4F2FF"
const attentionColor = "#F2B84BFF"
const sourceColor = "#2AC7B5FF"

// notificationScene keeps provider identity fixed while the text rows scroll independently.
func notificationScene(front string, lines ...string) protocol.Scene {
	headline := &protocol.TextElement{Value: safeText(front, 512), Font: "normal", Color: textColor, Width: 54}
	if len(headline.Value)*8 > 54 {
		headline.Marquee = &protocol.Marquee{PixelsPerMinute: 1000, StartDelayMilliseconds: 1000, RepeatDelayMilliseconds: 2500}
	}
	scene := protocol.Scene{Elements: []protocol.Element{
		{ID: "front-bg", Display: protocol.DisplayFront, Rectangle: &protocol.RectangleElement{Width: 72, Height: 16, Color: canvasColor}},
		{ID: "front-text", Display: protocol.DisplayFront, X: 18, Y: 0, Text: headline},
		{ID: "front-icon", Display: protocol.DisplayFront, Image: &protocol.ImageElement{Asset: protocol.AssetRef{PackagePath: "assets/github-mark.png"}}},
		{ID: "back-bg", Display: protocol.DisplayBack, Rectangle: &protocol.RectangleElement{Width: 160, Height: 80, Color: canvasColor}},
	}}
	for index, line := range lines {
		if index == 5 {
			break
		}
		scene.Elements = append(scene.Elements, protocol.Element{ID: fmt.Sprintf("back-line-%d", index), Display: protocol.DisplayBack, X: 4, Y: 4 + index*14, Text: &protocol.TextElement{Value: safeText(line, 30), Font: "small", Color: textColor, Width: 152}})
	}
	return scene
}
func withContext(scene protocol.Scene, context string) protocol.Scene {
	text := &protocol.TextElement{Value: safeText(context, 100), Font: "tiny", Color: "#9AAFB2FF", Width: 54}
	if len(text.Value)*4 > 54 {
		text.Marquee = &protocol.Marquee{PixelsPerMinute: 1000, StartDelayMilliseconds: 1000, RepeatDelayMilliseconds: 2500}
	}
	scene.Elements = append(scene.Elements, protocol.Element{ID: "front-context", Display: protocol.DisplayFront, X: 18, Y: 9, Text: text})
	return scene
}

func reasonText(reason string) string {
	switch reason {
	case "approval_requested":
		return "Deployment approval requested"
	case "assign":
		return "Assigned"
	case "invitation":
		return "Repository invitation"
	case "mention":
		return "Mentioned"
	case "review_requested":
		return "Review requested"
	case "security_alert":
		return "Security alert"
	case "team_mention":
		return "Team mentioned"
	case "author":
		return "Update on your thread"
	case "ci_activity":
		return "Workflow completed"
	case "comment":
		return "New comment"
	case "manual":
		return "Subscribed thread update"
	case "member_feature_requested":
		return "Feature enablement requested"
	case "security_advisory_credit":
		return "Security advisory credit"
	case "state_change":
		return "State changed"
	case "subscribed":
		return "Watched repository update"
	default:
		return "GitHub update"
	}
}
func attentionScene(c Config, i item) protocol.Scene {
	lines := []string{reasonText(i.Reason), i.Alias + " / " + i.SubjectType, "START: OPEN AND MARK READ", "TURN: DISMISS"}
	if c.RearDetails {
		lines = []string{reasonText(i.Reason), i.Repository, i.Title, "START: OPEN AND MARK READ", "TURN: DISMISS"}
	}
	scene := withContext(notificationScene(reasonText(i.Reason)+": "+i.Title, lines...), i.Repository)
	scene.Elements[1].Text.Color = attentionColor
	return scene
}

func connectionText(w *worker) string {
	switch w.actionError {
	case "read_unknown":
		return "GitHub read status unknown - checking again"
	case "opened_read_failed":
		return "Opened GitHub, but could not mark this notification read"
	case "open_failed":
		return "GitHub did not open - press START to try again"
	}
	code := w.actionError
	if code == "" {
		code = w.state.lastError
	}
	switch code {
	case "auth_required", "notification_access_required":
		return "GitHub token cannot access notifications - update the saved token"
	case "repository_access_required":
		return "GitHub token cannot access a selected repository - update the saved token"
	case "throttled":
		return "GitHub rate limit reached - checking again later"
	case "target_unavailable", "unsafe_api_url", "unsafe_browser_url":
		return "GitHub notification target is unavailable - Dismiss is still available"
	}
	if !w.config.Configured {
		return "GitHub setup required - run bsbctl app setup github-notifications"
	}
	if w.checkpointError {
		return "GitHub local state is not saved yet - retrying"
	}
	return "GitHub notification coverage may be incomplete - reconnecting"
}
func (w *worker) sessionScene() protocol.Scene {
	s := w.session
	if s == nil {
		return notificationScene("GitHub panel closed")
	}
	if !w.config.Configured {
		return withContext(notificationScene(connectionText(w), "GITHUB SETUP", "Run app setup"), "SETUP")
	}
	if w.actionError != "" {
		return withContext(notificationScene(connectionText(w), "GITHUB NOTIFICATIONS", "BACK: RETURN"), "RETRY")
	}
	if s.selected.ID == "" {
		if w.state.phase != "ready" {
			return withContext(notificationScene(connectionText(w), "GITHUB CONNECTION"), "CHECK CONNECTION")
		}
		return notificationScene("No unread GitHub notifications", "GITHUB NOTIFICATIONS", "BACK: CLOSE")
	}
	i := s.selected
	current, exists := w.state.items[i.ID]
	if !exists || current.Revision != i.Revision {
		return withContext(notificationScene("GitHub notification changed - select it again", "BACK: RETURN"), i.Repository)
	}
	if !w.state.fresh(w.now()) {
		return withContext(notificationScene(connectionText(w), "RETAINED NOTIFICATIONS", "Wait for fresh data"), i.Repository)
	}
	if s.level == panelList {
		position := 0
		for index, v := range w.state.ordered() {
			if v.ID == i.ID {
				position = index + 1
			}
		}
		return withContext(notificationScene(reasonText(i.Reason)+": "+i.Title, fmt.Sprintf("NOTIFICATIONS %d/%d", position, len(w.state.items)), i.Alias, "TURN SELECT / OK DETAIL", "START OPEN AND MARK READ"), i.Repository)
	}
	if s.level == panelConfirm {
		return withContext(notificationScene("Dismiss this GitHub notification?", "MARK THIS THREAD READ", "START: CONFIRM", "BACK: CANCEL"), i.Repository)
	}
	return attentionScene(w.config, i)
}
