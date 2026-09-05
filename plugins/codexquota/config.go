package codexquota

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const maxAccounts = 8

var labelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _-]{0,11}$`)

type Config struct {
	CredentialsHome          string
	ConfigurationHome        string
	Label                    string
	Badge                    string
	PollInterval             time.Duration
	WarningRemainingPercent  int
	CriticalRemainingPercent int
	ShowBadge                bool
}

type configJSON struct {
	CredentialsHome          *string `json:"credentials_home,omitempty"`
	ConfigurationHome        *string `json:"configuration_home,omitempty"`
	Label                    *string `json:"label,omitempty"`
	Badge                    *string `json:"badge,omitempty"`
	PollIntervalSeconds      *int    `json:"poll_interval_seconds,omitempty"`
	WarningRemainingPercent  *int    `json:"warning_remaining_percent,omitempty"`
	CriticalRemainingPercent *int    `json:"critical_remaining_percent,omitempty"`
}

type configuredAccount struct {
	protocol.Instance
	Config
}

func defaultConfig(mainHome string) Config {
	return Config{
		CredentialsHome: mainHome, ConfigurationHome: mainHome,
		Label: "MAIN", Badge: "M", PollInterval: 120 * time.Second,
		WarningRemainingPercent: 20, CriticalRemainingPercent: 5,
	}
}

func decodeConfig(raw json.RawMessage, mainHome string) (Config, error) {
	if !filepath.IsAbs(mainHome) {
		return Config{}, errors.New("main Codex home is unavailable")
	}
	value := configJSON{}
	if err := protocol.DecodeStrict(raw, &value); err != nil {
		return Config{}, fmt.Errorf("decode Codex quota configuration: %w", err)
	}
	result := defaultConfig(filepath.Clean(mainHome))
	userHome := homeForExpansion(mainHome)
	var err error
	if value.CredentialsHome != nil {
		result.CredentialsHome, err = resolveHomePath(*value.CredentialsHome, userHome)
		if err != nil {
			return Config{}, fmt.Errorf("credentials_home: %w", err)
		}
	}
	if value.ConfigurationHome != nil {
		result.ConfigurationHome, err = resolveHomePath(*value.ConfigurationHome, userHome)
		if err != nil {
			return Config{}, fmt.Errorf("configuration_home: %w", err)
		}
	}
	if value.Label != nil {
		result.Label = strings.TrimSpace(*value.Label)
	}
	if value.Badge != nil {
		result.Badge = strings.TrimSpace(*value.Badge)
	}
	if value.PollIntervalSeconds != nil {
		result.PollInterval = time.Duration(*value.PollIntervalSeconds) * time.Second
	}
	if value.WarningRemainingPercent != nil {
		result.WarningRemainingPercent = *value.WarningRemainingPercent
	}
	if value.CriticalRemainingPercent != nil {
		result.CriticalRemainingPercent = *value.CriticalRemainingPercent
	}
	if err := result.validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

func decodeInstances(instances []protocol.Instance, mainHome string) ([]configuredAccount, error) {
	result := make([]configuredAccount, 0, len(instances))
	for _, instance := range instances {
		if len(result) == maxAccounts {
			return nil, fmt.Errorf("Codex quota supports at most %d accounts", maxAccounts)
		}
		config, err := decodeConfig(instance.Config, mainHome)
		if err != nil {
			return nil, fmt.Errorf("instance %q: %w", instance.ID, err)
		}
		result = append(result, configuredAccount{Instance: instance, Config: config})
	}
	showBadges := len(result) > 1
	homes := make(map[string]string, len(result))
	badges := make(map[string]string, len(result))
	for index := range result {
		account := &result[index]
		if owner, exists := homes[account.CredentialsHome]; exists {
			return nil, fmt.Errorf("instances %q and %q use the same credentials_home", owner, account.ID)
		}
		homes[account.CredentialsHome] = account.ID
		if showBadges {
			if owner, exists := badges[account.Badge]; exists {
				return nil, fmt.Errorf("instances %q and %q use the same badge", owner, account.ID)
			}
			badges[account.Badge] = account.ID
		}
		account.ShowBadge = showBadges
	}
	return result, nil
}

func (c Config) validate() error {
	var errs []error
	if !filepath.IsAbs(c.CredentialsHome) || !filepath.IsAbs(c.ConfigurationHome) {
		errs = append(errs, errors.New("Codex home paths must resolve to absolute paths"))
	}
	if !labelPattern.MatchString(c.Label) {
		errs = append(errs, errors.New("label must contain 1-12 safe ASCII letters, digits, spaces, underscores, or hyphens"))
	}
	if len(c.Badge) != 1 || !((c.Badge[0] >= 'A' && c.Badge[0] <= 'Z') || (c.Badge[0] >= '0' && c.Badge[0] <= '9')) {
		errs = append(errs, errors.New("badge must be one uppercase ASCII letter or digit"))
	}
	if c.PollInterval < 60*time.Second || c.PollInterval > 900*time.Second {
		errs = append(errs, errors.New("poll_interval_seconds must be between 60 and 900"))
	}
	if c.CriticalRemainingPercent < 0 || c.WarningRemainingPercent > 100 || c.CriticalRemainingPercent >= c.WarningRemainingPercent {
		errs = append(errs, errors.New("remaining thresholds must satisfy 0 <= critical < warning <= 100"))
	}
	return errors.Join(errs...)
}

func resolveHomePath(value, userHome string) (string, error) {
	value = strings.TrimSpace(value)
	switch {
	case value == "~":
		value = userHome
	case strings.HasPrefix(value, "~/"):
		value = filepath.Join(userHome, strings.TrimPrefix(value, "~/"))
	}
	if !filepath.IsAbs(value) {
		return "", errors.New("must be absolute or begin with ~/")
	}
	return filepath.Clean(value), nil
}

func homeForExpansion(mainHome string) string {
	if filepath.Base(mainHome) == ".codex" {
		return filepath.Dir(mainHome)
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		return home
	}
	return filepath.Dir(mainHome)
}
