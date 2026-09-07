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
	case s.Truncated:
		return "Local history is truncated"
	case s.Gap:
		return "Earlier activity may be incomplete"
	case len(attentionItems(s.Items)) > 32:
		return "Some attention is list-only"
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
	case s.Phase == "ready" && s.CoverageIncomplete:
		return "Earlier activity may be incomplete"
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
	} else if !s.Fresh || s.Phase != "ready" || s.OpenUnsaved {
		main = connectionText(s)
		context += " | coverage gap"
	}
	return withContext(textScene(main, []string{"SLACK PENDING", fmt.Sprintf("%d MENTIONS / %d PENDING", c.Mentions, c.Pending), fmt.Sprintf("DM %d / CHANNEL %d / THREAD %d", c.DMs, c.Channels, c.Threads), coverage(s), "PLAY BROWSE"}, s), context)
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
func detailScene(cfg config, s workerSnapshot, a activity, action panelAction, now time.Time) protocol.Scene {
	actionText := "PLAY: OPEN IN SLACK"
	if action == panelHandle {
		actionText = "PLAY: DISMISS"
	} else if action == panelRead {
		actionText = "PLAY: READ MESSAGE"
	}
	return activityScene(cfg, s, a, actionText, "TURN ACTION / BACK LIST", now)
}

func activityScene(cfg config, s workerSnapshot, a activity, actionText, navigation string, now time.Time) protocol.Scene {
	alias := a.Alias
	if alias == "" {
		alias = "DIRECT"
	}
	main := activityText(a)
	if cfg.frontMessagePreview && a.Preview != "" {
		main += ": " + sanitizePreview(a.Preview)
	}
	lines := []string{activityText(a) + " / " + alias, fmt.Sprintf("%d MIN AGO / %d MESSAGES", max(0, int(now.Sub(a.UpdatedAt).Minutes())), a.Count), actionText, navigation}
	if !s.Fresh {
		main = connectionText(s)
	}
	scene := withContext(textScene(main, lines, s), alias+" | "+actionText)
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
	return withContext(textScene(connectionText(s), []string{"SLACK CONNECTION", coverage(s), "ACTIVITY MAY BE INCOMPLETE", fmt.Sprintf("%d PENDING ITEMS", c.Pending), "PLAY BROWSE"}, s), "CHECK CONNECTION")
}
func panelScene(cfg config, s workerSnapshot, p *panelSession, now time.Time) protocol.Scene {
	if p.failure != "" {
		return textScene("Slack item changed - select it again", []string{"BACK TO LIST"}, s)
	}
	if p.level == panelList {
		items := pendingItems(s.Items)
		if p.target.ID == "" {
			if len(items) == 0 {
				return summaryScene(cfg, s)
			}
			return textScene("Turn to select a Slack item", []string{"SLACK PENDING", fmt.Sprintf("%d PENDING ITEMS", len(items)), "TURN TO SELECT", "BACK CLOSE"}, s)
		}
		for index, item := range items {
			if item.ID == p.target.ID {
				return withListPosition(listScene(cfg, s, item, now), index+1, len(items))
			}
		}
		return textScene("Slack item changed - turn to select", []string{"BACK TO CLOSE"}, s)
	}
	if p.level == panelReader {
		return readerScene(s, p, now)
	}
	return detailScene(cfg, s, p.target, p.action, now)
}

func readerScene(s workerSnapshot, p *panelSession, now time.Time) protocol.Scene {
	preview := sanitizePreview(p.target.Preview)
	pages := readerPageCount(p.target.Preview)
	page := min(p.page, pages-1)
	part := preview[page*48 : min((page+1)*48, len(preview))]
	alias := p.target.Alias
	if alias == "" {
		alias = "DIRECT"
	}
	return withContext(textScene(activityText(p.target), []string{activityText(p.target) + " / " + alias, part[:min(24, len(part))], part[min(24, len(part)):], fmt.Sprintf("PAGE %d/%d / TURN SCROLL", page+1, pages), "BACK ACTIONS"}, s), alias+" | READ")
}

func listScene(cfg config, s workerSnapshot, a activity, now time.Time) protocol.Scene {
	return activityScene(cfg, s, a, "PLAY: DETAILS", "TURN SELECT / BACK CLOSE", now)
}

func readerPageCount(preview string) int {
	return max(1, (len(sanitizePreview(preview))+47)/48)
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
