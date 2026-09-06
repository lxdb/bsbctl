// Package githubnotifications collects a bounded GitHub notification view.
package githubnotifications

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	PluginID          = "dev.bsbctl.github-notifications"
	PluginVersion     = "dev"
	AppID             = "github-notifications"
	ChannelSummary    = "summary"
	ChannelAttention  = "attention"
	ChannelConnection = "connection"
	ChannelLive       = "live"
	OperationStatus   = "status"
	OperationItems    = "items"
)

//go:embed config.schema.json
var ConfigSchema json.RawMessage

// Repository optionally selects one source repository and its public-safe display alias.
type Repository struct {
	Name  string `json:"name"`
	Alias string `json:"alias"`
}

// Config contains validated collection and presentation settings.
type Config struct {
	Repositories        []Repository
	Label               string
	RearDetails         bool
	PollInterval        time.Duration
	Configured          bool
	NotificationReasons []string
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9_.-]{1,100}$`)

func publicAlias(s string) bool {
	if len(s) < 1 || len(s) > 8 {
		return false
	}
	for _, r := range s {
		if r < 32 || r > 126 {
			return false
		}
	}
	return true
}

// ValidateRepositories checks a bounded optional filter of unique GitHub.com names and public aliases.
func ValidateRepositories(repositories []Repository) error {
	if len(repositories) > 32 {
		return errors.New("repositories must contain at most 32 entries")
	}
	names, aliases := map[string]bool{}, map[string]bool{}
	for _, r := range repositories {
		n := strings.ToLower(r.Name)
		if !repositoryPattern.MatchString(r.Name) || strings.HasSuffix(n, "/.") || strings.HasSuffix(n, "/..") {
			return errors.New("repository name must be owner/repo")
		}
		if !publicAlias(r.Alias) {
			return errors.New("repository alias must contain 1-8 printable ASCII characters")
		}
		if names[n] || aliases[r.Alias] {
			return errors.New("repository names and aliases must be unique")
		}
		names[n], aliases[r.Alias] = true, true
	}
	return nil
}

// DecodeConfig accepts an exact empty object or a complete strict configuration.
func DecodeConfig(raw json.RawMessage) (Config, error) {
	c := Config{Label: "GH", PollInterval: 60 * time.Second}
	var fields map[string]json.RawMessage
	if len(raw) > 64<<10 || protocol.DecodeStrict(raw, &fields) != nil || fields == nil {
		return c, errors.New("configuration must be a bounded JSON object")
	}
	if len(fields) == 0 {
		return c, nil
	}
	for _, v := range fields {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return c, errors.New("configuration fields cannot be null")
		}
	}
	var wire struct {
		Repositories []Repository    `json:"repositories"`
		Label        *string         `json:"label"`
		RearDetails  bool            `json:"rear_details"`
		Interval     *int            `json:"poll_interval_seconds"`
		Reasons      json.RawMessage `json:"notification_reasons"`
	}
	if protocol.DecodeStrict(raw, &wire) != nil {
		return c, errors.New("invalid notification configuration")
	}
	if _, ok := fields["repositories"]; !ok {
		return c, errors.New("configured notifications require repositories; use an empty array for all repositories")
	}
	if err := ValidateRepositories(wire.Repositories); err != nil {
		return c, err
	}
	if wire.Label != nil {
		c.Label = *wire.Label
	}
	if !publicAlias(c.Label) {
		return c, errors.New("label must contain 1-8 printable ASCII characters")
	}
	if wire.Interval != nil {
		if *wire.Interval < 60 || *wire.Interval > 900 {
			return c, errors.New("poll interval must be 60-900 seconds")
		}
		c.PollInterval = time.Duration(*wire.Interval) * time.Second
	}
	if len(wire.Reasons) > 0 {
		var mode string
		if json.Unmarshal(wire.Reasons, &mode) == nil {
			if mode != "actionable" && mode != "all" {
				return c, errors.New("notification_reasons must be actionable, all, or known reasons")
			}
			if mode == "all" {
				c.NotificationReasons = []string{"all"}
			}
		} else {
			if json.Unmarshal(wire.Reasons, &c.NotificationReasons) != nil || len(c.NotificationReasons) == 0 {
				return c, errors.New("notification_reasons must not be empty")
			}
			seen := map[string]bool{}
			for _, reason := range c.NotificationReasons {
				if !knownReason(reason) || reason == "unknown" || seen[reason] {
					return c, errors.New("notification_reasons must contain unique known reasons")
				}
				seen[reason] = true
			}
		}
	}
	c.Repositories = wire.Repositories
	c.RearDetails = wire.RearDetails
	c.Configured = true
	return c, nil
}

func (c Config) matchesReason(reason string) bool {
	if len(c.NotificationReasons) == 0 {
		return actionable(reason)
	}
	for _, selected := range c.NotificationReasons {
		if selected == "all" || selected == reason {
			return true
		}
	}
	return false
}
