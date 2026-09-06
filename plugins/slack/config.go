package slack

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	protocoljson "github.com/lxdb/bsbctl/sdk/protocol"
)

const maxConfigBytes = 64 * 1024
const maxLabelBytes = 100

var (
	errConfig        = errors.New("invalid Slack configuration")
	errSecrets       = errors.New("invalid Slack secret set")
	slackIDPattern   = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,63}$`)
	timestampPattern = regexp.MustCompile(`^[0-9]{1,16}\.[0-9]{6}$`)
)

type config struct {
	configured               bool
	appID                    string
	workspaceID              string
	userID                   string
	channels                 map[string]string
	allChannels              bool
	directMessages           bool
	groupDirectMessages      bool
	watchedThreads           []threadRoot
	watchParticipatedThreads bool
	label                    string
	rearDetails              bool
	frontMessagePreview      bool
}

type threadRoot struct {
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts"`
}

type configJSON struct {
	AppID       string `json:"app_id"`
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
	Channels    []struct {
		ID    string `json:"id"`
		Alias string `json:"alias"`
	} `json:"channels"`
	AllChannels              bool         `json:"all_channels"`
	DirectMessages           *bool        `json:"direct_messages"`
	GroupDirectMessages      bool         `json:"group_direct_messages"`
	WatchedThreads           []threadRoot `json:"watched_threads"`
	WatchParticipatedThreads *bool        `json:"watch_participated_threads"`
	Label                    *string      `json:"label"`
	RearDetails              bool         `json:"rear_details"`
	FrontMessagePreview      bool         `json:"front_message_preview"`
}

func decodeConfig(raw json.RawMessage) (config, error) {
	if len(raw) == 0 || len(raw) > maxConfigBytes {
		return config{}, errConfig
	}
	var wire configJSON
	if err := protocoljson.DecodeStrict(raw, &wire); err != nil {
		return config{}, errConfig
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return config{}, errConfig
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return config{}, errConfig
		}
	}
	cfg := config{channels: make(map[string]string), label: "SLK", directMessages: true, watchParticipatedThreads: true}
	if len(fields) == 0 {
		return cfg, nil
	}
	if !validID(wire.AppID, "A") || !validID(wire.WorkspaceID, "T") || !validID(wire.UserID, "UW") || len(wire.Channels) > 32 || len(wire.WatchedThreads) > 32 {
		return config{}, errConfig
	}
	cfg.configured = true
	cfg.appID = wire.AppID
	cfg.workspaceID = wire.WorkspaceID
	cfg.userID = wire.UserID
	cfg.allChannels = wire.AllChannels
	if wire.Label != nil {
		cfg.label = *wire.Label
	}
	if !validLabel(cfg.label) {
		return config{}, errConfig
	}
	if wire.DirectMessages != nil {
		cfg.directMessages = *wire.DirectMessages
	}
	if wire.WatchParticipatedThreads != nil {
		cfg.watchParticipatedThreads = *wire.WatchParticipatedThreads
	}
	cfg.groupDirectMessages = wire.GroupDirectMessages
	cfg.rearDetails = wire.RearDetails
	cfg.frontMessagePreview = wire.FrontMessagePreview
	for _, channel := range wire.Channels {
		if !validID(channel.ID, "CG") || !validLabel(channel.Alias) {
			return config{}, errConfig
		}
		if _, exists := cfg.channels[channel.ID]; exists {
			return config{}, errConfig
		}
		cfg.channels[channel.ID] = channel.Alias
	}
	seen := make(map[threadRoot]bool)
	for _, root := range wire.WatchedThreads {
		if !timestampPattern.MatchString(root.ThreadTS) || !validID(root.ChannelID, "CDG") || seen[root] {
			return config{}, errConfig
		}
		_, selected := cfg.channels[root.ChannelID]
		// G IDs can denote private channels or group DMs. Events still have to pass
		// channel_type admission; configuration alone cannot identify a G domain.
		if !selected && !(cfg.allChannels && validID(root.ChannelID, "CG")) && !(strings.HasPrefix(root.ChannelID, "D") && cfg.directMessages) && !(strings.HasPrefix(root.ChannelID, "G") && cfg.groupDirectMessages) {
			return config{}, errConfig
		}
		seen[root] = true
	}
	cfg.watchedThreads = wire.WatchedThreads
	return cfg, nil
}

func (c config) validateSecrets(secrets map[string]string) error {
	if !c.configured {
		if len(secrets) != 0 {
			return errSecrets
		}
		return nil
	}
	want := 1
	if c.allChannels {
		want = 2
	}
	if len(secrets) != want || strings.TrimSpace(secrets["app_token"]) == "" || c.allChannels && strings.TrimSpace(secrets["user_token"]) == "" {
		return errSecrets
	}
	return nil
}

func validID(id, prefixes string) bool {
	return slackIDPattern.MatchString(id) && strings.ContainsRune(prefixes, rune(id[0]))
}
func validLabel(value string) bool {
	if len(value) < 1 || len(value) > maxLabelBytes {
		return false
	}
	for _, r := range value {
		if r < 32 || r > 126 {
			return false
		}
	}
	return strings.TrimSpace(value) != ""
}
