package slack

import (
	"fmt"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	slackCanvas    = "#1A1D21FF"
	slackText      = "#FFFFFFFF"
	slackSecondary = "#ABABADFF"
	slackWarning   = "#ECB22EFF"
	slackError     = "#E01E5AFF"
)

func coverage(s workerSnapshot) string {
	switch {
	case s.Phase == "unconfigured":
		return "Setup required"
	case s.Phase == "auth_required":
		return "Access expired"
	case !s.Fresh:
		return "Waiting for fresh activity"
	case s.Gap || s.Truncated || len(attentionItems(s.Items)) > 32:
		return "Coverage gap"
	case s.Phase == "degraded":
		return "Activity may be incomplete"
	default:
		return "Since connection"
	}
}
func connectionText(s workerSnapshot) string {
	switch {
	case s.Phase == "unconfigured":
		return "Slack setup required - run bsbctl app config slack"
	case s.Phase == "auth_required":
		return "Slack access expired - update the saved Slack tokens"
	case s.OpenUnsaved:
		return "Slack opened; local pending state is not saved yet"
	case s.ErrorCode == "checkpoint_failed":
		return "Slack local state is not saved yet - retrying"
	case s.ErrorCode == "missing_scope":
		return "Slack access is incomplete - update the app subscriptions and user scopes"
	case s.ErrorCode == "throttled":
		return "Slack rate limit reached - activity may be incomplete"
	default:
		return "Slack activity may be incomplete - reconnecting"
	}
}

type pendingCounts struct{ Pending, Mentions, DMs, Channels, Threads int }

func countPending(items []activity) pendingCounts {
	var c pendingCounts
	for _, a := range items {
		if a.Handled {
			continue
		}
		c.Pending++
		if a.Mention {
			c.Mentions++
		}
		switch a.Kind {
		case "dm":
			c.DMs++
		case "channel":
			c.Channels++
		case "thread":
			c.Threads++
		}
	}
	return c
}
func summaryScene(cfg config, s workerSnapshot) protocol.Scene {
	c := countPending(s.Items)
	main := fmt.Sprintf("%d mentions | %d DMs | %d channels | %d threads", c.Mentions, c.DMs, c.Channels, c.Threads)
	context := cfg.label
	if context == "" {
		context = "SLK"
	}
	if c.Pending == 0 {
		main = "No pending Slack activity"
	}
	if s.Phase == "unconfigured" {
		main = connectionText(s)
		context = "SETUP"
	} else if s.Phase == "auth_required" {
		main = connectionText(s)
		context = "TOKEN"
	} else if !s.Fresh || s.Gap || s.Truncated || s.OpenUnsaved {
		main = connectionText(s)
		context += " | coverage gap"
	}
	return withContext(textScene(main, []string{"SLACK PENDING", fmt.Sprintf("%d MENTIONS / %d PENDING", c.Mentions, c.Pending), fmt.Sprintf("DM %d / CHANNEL %d / THREAD %d", c.DMs, c.Channels, c.Threads), coverage(s), "OK BROWSE"}, s), context)
}
func activityText(a activity) string {
	if a.Mention {
		return "Mentioned"
	}
	switch a.Kind {
	case "dm":
		return "Direct message"
	case "channel":
		return "Channel message"
	default:
		return "Thread reply"
	}
}
func detailScene(cfg config, s workerSnapshot, a activity, page int, now time.Time) protocol.Scene {
	alias := a.Alias
	if alias == "" {
		alias = "DIRECT"
	}
	main := activityText(a)
	if cfg.frontMessagePreview && a.Preview != "" {
		main += ": " + sanitizePreview(a.Preview)
	}
	lines := []string{activityText(a) + " / " + alias, fmt.Sprintf("%d MIN AGO / %d MESSAGES", max(0, int(now.Sub(a.UpdatedAt).Minutes())), a.Count), coverage(s), "START OPEN / TURN DISMISS", "BACK LIST"}
	if cfg.rearDetails && a.Preview != "" {
		preview := sanitizePreview(a.Preview)
		pages := (len(preview) + 47) / 48
		page = wrapIndex(page, pages)
		part := preview[page*48 : min((page+1)*48, len(preview))]
		lines = []string{activityText(a) + " / " + alias, part[:min(24, len(part))], part[min(24, len(part)):], fmt.Sprintf("PAGE %d/%d / OK MORE", page+1, pages), "START OPEN / TURN DISMISS"}
	}
	if !s.Fresh {
		main = connectionText(s)
	}
	scene := withContext(textScene(main, lines, s), alias+" | START OPEN")
	for index := range scene.Elements {
		if scene.Elements[index].ID == "front-label" {
			scene.Elements[index].Text.Color = slackWarning
			break
		}
	}
	return scene
}

func connectionScene(s workerSnapshot) protocol.Scene {
	c := countPending(s.Items)
	return withContext(textScene(connectionText(s), []string{"SLACK CONNECTION", coverage(s), "ACTIVITY MAY BE INCOMPLETE", fmt.Sprintf("%d PENDING ITEMS", c.Pending), "OK BROWSE"}, s), "CHECK CONNECTION")
}
func panelScene(cfg config, s workerSnapshot, p *panelSession, now time.Time) protocol.Scene {
	if p.level == panelList {
		items := pendingItems(s.Items)
		if len(items) == 0 {
			return summaryScene(cfg, s)
		}
		index := wrapIndex(p.index, len(items))
		a := items[index]
		scene := detailScene(cfg, s, a, 0, now)
		return withListPosition(scene, index+1, len(items))
	}
	if p.failure != "" {
		return textScene("Slack item changed - select it again", []string{"BACK TO LIST"}, s)
	}
	if p.level == panelDismiss {
		alias := p.target.Alias
		if alias == "" {
			alias = "DIRECT"
		}
		return withContext(textScene("Dismiss this Slack item?", []string{"REMOVE FROM LOCAL PENDING", "START CONFIRM / BACK CANCEL", "SLACK READ STATE IS UNCHANGED"}, s), alias)
	}
	return detailScene(cfg, s, p.target, p.page, now)
}
func withListPosition(scene protocol.Scene, index, count int) protocol.Scene {
	scene.Elements = append(scene.Elements, protocol.Element{ID: "back-position", Display: protocol.DisplayBack, X: 4, Y: 72, Text: &protocol.TextElement{Value: fmt.Sprintf("ITEM %d OF %d / TURN SELECT", index, count), Font: "tiny", Color: slackText, Width: 152}})
	return scene
}
func withContext(scene protocol.Scene, value string) protocol.Scene {
	text := &protocol.TextElement{Value: sanitizePreview(value), Font: "tiny", Color: slackSecondary, Width: 54}
	if len(text.Value)*4 > 54 {
		text.Marquee = &protocol.Marquee{PixelsPerMinute: 1000, StartDelayMilliseconds: 1000, RepeatDelayMilliseconds: 2500}
	}
	scene.Elements = append(scene.Elements, protocol.Element{ID: "front-context", Display: protocol.DisplayFront, X: 18, Y: 9, Text: text})
	return scene
}
func textScene(front string, lines []string, s workerSnapshot) protocol.Scene {
	color := slackText
	if !s.Fresh || s.Gap || s.Truncated {
		color = slackWarning
	}
	if s.Phase == "auth_required" {
		color = slackError
	}
	main := &protocol.TextElement{Value: sanitizePreview(front), Font: "normal", Color: slackText, Width: 54}
	if len(main.Value)*8 > 54 {
		main.Marquee = &protocol.Marquee{PixelsPerMinute: 1000, StartDelayMilliseconds: 1000, RepeatDelayMilliseconds: 2500}
	}
	elements := []protocol.Element{
		{ID: "front-background", Display: protocol.DisplayFront, Rectangle: &protocol.RectangleElement{Width: 72, Height: 16, Color: slackCanvas}},
		{ID: "front-icon", Display: protocol.DisplayFront, Image: &protocol.ImageElement{Asset: protocol.AssetRef{PackagePath: "assets/slack-mark.png"}}},
		{ID: "front-label", Display: protocol.DisplayFront, X: 18, Y: 0, Text: main},
		{ID: "back-background", Display: protocol.DisplayBack, Rectangle: &protocol.RectangleElement{Width: 160, Height: 80, Color: slackCanvas}},
	}
	for i, line := range lines[:min(5, len(lines))] {
		line = sanitizePreview(line)
		if line != "" {
			elements = append(elements, protocol.Element{ID: fmt.Sprintf("back-line-%d", i), Display: protocol.DisplayBack, X: 4, Y: 4 + i*14, Text: &protocol.TextElement{Value: line[:min(30, len(line))], Font: "small", Color: color, Width: 152}})
		}
	}
	return protocol.Scene{Elements: elements}
}
