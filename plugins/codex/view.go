package codex

import (
	"strings"
	"unicode/utf8"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	canvas                = "#071522FF"
	surface               = "#171A21FF"
	textColor             = "#EAF4F2FF"
	secondary             = "#9AAFB2FF"
	signature             = "#2AC7B5FF"
	information           = "#42D6E5FF"
	success               = "#35D07FFF"
	warning               = "#F2B84BFF"
	counterpoint          = "#F47A3EFF"
	danger                = "#FF786FFF"
	cardMarqueeRate       = 1000
	cardMarqueeStartDelay = 1000
	cardMarqueeRepeat     = 2500
)

func typedQuestionScene(_ Card, questionPosition, questionText, optionPosition string, option requestOption) protocol.Scene {
	questionText = safeLine(questionText)
	description := fitDisplayLine(option.Description, 96)
	elements := []protocol.Element{
		cardRectangle("front-background", "front", 0, 0, 72, 16, canvas),
		cardImage("front-codex-mark", "front", codexMarkSource, 1, 1),
		cardMarquee("front-option-label", "front", option.Label, "normal", textColor, 18, 1, 53),
		cardText("front-option-position", "front", optionPosition, "tiny", warning, 18, 10, ""),

		cardRectangle("back-background", "back", 0, 0, 160, 80, canvas),
		cardText("back-question-position", "back", questionPosition, "tiny", warning, 8, 9, ""),
		cardText("back-option-position", "back", optionPosition, "tiny", secondary, 140, 9, "top_right"),
		cardMarquee("back-question", "back", questionText, "normal", textColor, 8, 18, 132),
		cardMarquee("back-option-label", "back", option.Label, "normal", textColor, 8, 42, 132),
		cardText("back-option-description", "back", description, "tiny", secondary, 8, 61, ""),
	}
	return protocol.Scene{Elements: elements}
}

func cardScene(card Card) protocol.Scene {
	state := fitDisplayLine(card.StateWord, 16)
	if state == "" {
		state = "CODEX"
	}
	contextLine := fitDisplayLine(card.ContextLine, 28)
	if contextLine == "" {
		contextLine = "Codex"
	}
	sessionLine := safeLine(card.SessionLine)
	projectLine := safeLine(card.ProjectLine)
	frontStateFont, frontStateX, frontStateY, frontStateAlign := frontStateFont(state), 44, 8, "center"
	sessionLabel, workdirLabel := "", ""
	if sessionLine != "" || projectLine != "" {
		contextLine = ""
		sessionLabel, workdirLabel = "SESSION", "WORKDIR"
	}
	if projectLine != "" {
		frontStateFont, frontStateX, frontStateY, frontStateAlign = "tiny", 18, 10, ""
	}
	detailLine := fitDisplayLine(card.DetailLine, 34)
	accent := cardAccent(state)
	return protocol.Scene{Elements: []protocol.Element{
		cardRectangle("front-background", "front", 0, 0, 72, 16, canvas),
		cardImage("front-codex-mark", "front", codexMarkSource, 1, 1),
		cardMarquee("front-workdir", "front", projectLine, "normal", textColor, 18, 0, 53),
		cardText("front-state", "front", state, frontStateFont, accent, frontStateX, frontStateY, frontStateAlign),

		cardRectangle("back-background", "back", 0, 0, 160, 80, canvas),
		cardRectangle("back-surface", "back", 4, 13, 140, 60, surface),
		cardText("back-eyebrow", "back", "CODEX / APP SERVER", "tiny", secondary, 8, 3, ""),
		cardText("back-state", "back", state, backStateFont(state), accent, 8, 17, ""),
		cardText("back-context", "back", contextLine, "normal", textColor, 8, 37, ""),
		cardText("back-workdir-label", "back", workdirLabel, "tiny", secondary, 8, 34, ""),
		cardMarquee("back-workdir", "back", projectLine, "normal", textColor, 50, 34, 90),
		cardText("back-session-label", "back", sessionLabel, "tiny", secondary, 8, 49, ""),
		cardMarquee("back-session", "back", sessionLine, "small", textColor, 50, 49, 90),
		cardText("back-detail", "back", detailLine, "small", secondary, 8, 65, ""),
	}}
}

func cardAccent(state string) string {
	switch {
	case state == "DONE", state == "COMPACTED":
		return success
	case state == "STOP":
		return counterpoint
	case state == "FAIL", state == "CODEX OFF":
		return danger
	case state == "ASK", strings.HasPrefix(state, "WAIT"):
		return warning
	case strings.HasPrefix(state, "PLAN"), strings.HasPrefix(state, "COMPACT"):
		return information
	default:
		return signature
	}
}

func frontStateFont(state string) string {
	if utf8.RuneCountInString(state) > 8 {
		return "small"
	}
	return "normal"
}

func backStateFont(state string) string {
	if utf8.RuneCountInString(state) > 10 {
		return "normal"
	}
	return "large"
}

func fitDisplayLine(value string, limit int) string {
	value = safeLine(value)
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit])
	}
	return value
}

func cardRectangle(id, display string, x, y, width, height int, color string) protocol.Element {
	return protocol.Element{ID: id, Display: protocol.Display(display), X: x, Y: y,
		Rectangle: &protocol.RectangleElement{Width: width, Height: height, Color: color}}
}

func cardText(id, display, value, font, color string, x, y int, align string) protocol.Element {
	return protocol.Element{ID: id, Display: protocol.Display(display), X: x, Y: y,
		Text: &protocol.TextElement{Value: value, Font: font, Color: color, Align: align}}
}

func cardMarquee(id, display, value, font, color string, x, y, width int) protocol.Element {
	element := cardText(id, display, value, font, color, x, y, "")
	element.Text.Width = width
	element.Text.Marquee = &protocol.Marquee{PixelsPerMinute: cardMarqueeRate, StartDelayMilliseconds: cardMarqueeStartDelay, RepeatDelayMilliseconds: cardMarqueeRepeat}
	return element
}

func cardImage(id, display, assetPath string, x, y int) protocol.Element {
	return protocol.Element{ID: id, Display: protocol.Display(display), X: x, Y: y,
		Image: &protocol.ImageElement{Asset: protocol.AssetRef{PackagePath: assetPath}}}
}
