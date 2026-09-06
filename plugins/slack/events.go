package slack

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

const maxEventBytes = 256 * 1024

var (
	errUnsupportedEvent = errors.New("unsupported Slack event")
	errEvent            = errors.New("invalid Slack event")
	errAuthorization    = errors.New("unproven Slack event authorization")
)

type eventCallback struct {
	Type           string `json:"type"`
	APIAppID       string `json:"api_app_id"`
	TeamID         string `json:"team_id"`
	EventID        string `json:"event_id"`
	Authorizations []struct {
		TeamID string `json:"team_id"`
		UserID string `json:"user_id"`
		IsBot  *bool  `json:"is_bot"`
	} `json:"authorizations"`
	Event messageEvent `json:"event"`
}

type messageEvent struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	Channel     string          `json:"channel"`
	ChannelType string          `json:"channel_type"`
	User        string          `json:"user"`
	BotID       string          `json:"bot_id"`
	TS          string          `json:"ts"`
	ThreadTS    string          `json:"thread_ts"`
	EventTS     string          `json:"event_ts"`
	DeletedTS   string          `json:"deleted_ts"`
	Text        string          `json:"text"`
	Blocks      json.RawMessage `json:"blocks"`
	Edited      struct {
		TS string `json:"ts"`
	} `json:"edited"`
	Message *messageEvent `json:"message"`
}

type normalizedEvent struct {
	callbackID  string
	channelID   string
	channelType string
	ts          string
	rootTS      string
	version     string
	digest      string
	mention     bool
	own         bool
	deleted     bool
	preview     string
}

func normalizeEvent(raw json.RawMessage, appID, workspaceID, userID string, rearDetails bool) (normalizedEvent, bool, error) {
	if len(raw) == 0 || len(raw) > maxEventBytes {
		return normalizedEvent{}, false, errEvent
	}
	var callback eventCallback
	if err := json.Unmarshal(raw, &callback); err != nil {
		return normalizedEvent{}, false, errEvent
	}
	if callback.Type != "event_callback" {
		return normalizedEvent{}, false, errUnsupportedEvent
	}
	if callback.APIAppID != appID || callback.TeamID != workspaceID {
		return normalizedEvent{}, false, errAuthorization
	}
	authorized := false
	for _, auth := range callback.Authorizations {
		if auth.TeamID == workspaceID && auth.UserID == userID && auth.IsBot != nil && !*auth.IsBot {
			authorized = true
			break
		}
	}
	if !authorized {
		return normalizedEvent{}, false, errAuthorization
	}
	if len(callback.EventID) == 0 || len(callback.EventID) > 128 {
		return normalizedEvent{}, false, errEvent
	}
	msg := callback.Event
	if msg.Type != "message" {
		return normalizedEvent{}, false, errUnsupportedEvent
	}
	event := normalizedEvent{callbackID: hashParts(workspaceID, callback.EventID), channelID: msg.Channel, channelType: msg.ChannelType}
	if !validID(event.channelID, "CDG") {
		return normalizedEvent{}, false, errEvent
	}
	switch msg.Subtype {
	case "message_deleted":
		event.ts = msg.DeletedTS
		event.version = msg.EventTS
		event.deleted = true
	case "message_changed":
		if msg.Message == nil {
			return normalizedEvent{}, false, errEvent
		}
		update := *msg.Message
		event.version = cmp.Or(update.Edited.TS, msg.EventTS)
		msg = update
	case "", "thread_broadcast", "file_share":
	case "bot_message":
		return normalizedEvent{}, false, nil
	default:
		return normalizedEvent{}, false, errUnsupportedEvent
	}
	if !event.deleted {
		if msg.BotID != "" || msg.Subtype == "bot_message" {
			return normalizedEvent{}, false, nil
		}
		if !validID(msg.User, "UW") {
			return normalizedEvent{}, false, errEvent
		}
		event.ts = msg.TS
		event.rootTS = cmp.Or(msg.ThreadTS, msg.TS)
		event.version = cmp.Or(event.version, msg.Edited.TS, msg.TS)
		event.own = msg.User == userID
		event.mention = strings.Contains(msg.Text, "<@"+userID+">")
		richMention, richText, err := extractRichText(msg.Blocks, userID)
		if err != nil {
			return normalizedEvent{}, false, err
		}
		event.mention = event.mention || richMention
		event.digest = hashParts(msg.Text, string(msg.Blocks), msg.User, event.rootTS)
		if rearDetails {
			event.preview = sanitizePreview(cmp.Or(msg.Text, richText))
		}
	}
	if !timestampPattern.MatchString(event.ts) || !timestampPattern.MatchString(event.version) || (!event.deleted && !timestampPattern.MatchString(event.rootTS)) {
		return normalizedEvent{}, false, errEvent
	}
	if event.deleted {
		event.digest = hashParts("deleted")
	}
	return event, true, nil
}

// Only Slack's rich-text tree is traversed; arbitrary JSON strings are not
// interpreted as text or user references. Both depth and node work are bounded.
func extractRichText(raw json.RawMessage, userID string) (bool, string, error) {
	if len(raw) == 0 {
		return false, "", nil
	}
	type node struct {
		Type     string            `json:"type"`
		UserID   string            `json:"user_id"`
		Text     string            `json:"text"`
		Elements []json.RawMessage `json:"elements"`
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return false, "", errEvent
	}
	count := 0
	mention := false
	var text strings.Builder
	var walk func(json.RawMessage, int) error
	walk = func(raw json.RawMessage, depth int) error {
		count++
		if count > 1024 || depth > 16 {
			return errEvent
		}
		var value node
		if err := json.Unmarshal(raw, &value); err != nil {
			return errEvent
		}
		switch value.Type {
		case "rich_text", "rich_text_section", "rich_text_list", "rich_text_quote", "rich_text_preformatted":
			for _, child := range value.Elements {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case "user":
			mention = mention || value.UserID == userID
		case "text":
			if text.Len() < 640 {
				text.WriteString(value.Text[:min(len(value.Text), 640-text.Len())])
			}
		}
		return nil
	}
	for _, block := range blocks {
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(block, &kind); err != nil {
			return false, "", errEvent
		}
		if kind.Type == "rich_text" {
			if err := walk(block, 0); err != nil {
				return false, "", err
			}
		}
	}
	return mention, text.String(), nil
}

func sanitizePreview(text string) string {
	var out strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			r = ' '
		}
		// Device fonts have a smaller repertoire than Unicode. Keep predictable
		// printable ASCII rather than leaking controls or relying on missing glyphs.
		if r < 32 || r > 126 {
			r = '?'
		}
		if out.Len() >= 160 {
			break
		}
		out.WriteRune(r)
	}
	return out.String()
}

func hashParts(parts ...string) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Slack timestamps are decimal strings, never floating point. Integer widths
// need not match in fixtures or older data.
func compareTS(a, b string) int {
	ai, af, _ := strings.Cut(a, ".")
	bi, bf, _ := strings.Cut(b, ".")
	ai = strings.TrimLeft(ai, "0")
	bi = strings.TrimLeft(bi, "0")
	return cmp.Or(cmp.Compare(len(ai), len(bi)), strings.Compare(ai, bi), strings.Compare(af, bf))
}
