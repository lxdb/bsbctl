// Package presentation defines BUSY Bar-specific plugin output and selection policy.
package presentation

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/lxdb/bsbctl/internal/identifier"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

var busyTimerThemePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Policy determines when a candidate may participate in display arbitration.
type Policy = string

const (
	PolicyAttention    = "attention"
	PolicyInteractive  = "interactive"
	PolicyWhenRelevant = "when_relevant"
	PolicyRotation     = "rotation"
)

// Band is the daemon-derived semantic priority of one eligible candidate.
// Plugins choose neither the band nor its ordering.
type Band string

const (
	BandCriticalActionable Band = "critical_actionable"
	BandInteractive        Band = "interactive"
	BandActionable         Band = "actionable"
	BandRelevant           Band = "relevant"
	BandRotation           Band = "rotation"
)

// BandFor returns the only semantic band valid for an eligible policy and
// impact pair. Attention eligibility is established before candidate creation.
func BandFor(policy Policy, impact protocol.Impact) Band {
	switch policy {
	case PolicyAttention:
		if impact == protocol.ImpactCritical {
			return BandCriticalActionable
		}
		return BandActionable
	case PolicyInteractive:
		return BandInteractive
	case PolicyWhenRelevant:
		return BandRelevant
	case PolicyRotation:
		return BandRotation
	default:
		return ""
	}
}

// CompareBand compares daemon-owned semantic priority.
func CompareBand(left, right Band) int {
	return comparePriority(bandPriority(left), bandPriority(right))
}

// CompareImpact compares the exact four provider impact levels.
func CompareImpact(left, right protocol.Impact) int {
	return comparePriority(impactPriority(left), impactPriority(right))
}

func comparePriority(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func bandPriority(band Band) int {
	switch band {
	case BandCriticalActionable:
		return 5
	case BandInteractive:
		return 4
	case BandActionable:
		return 3
	case BandRelevant:
		return 2
	case BandRotation:
		return 1
	default:
		return 0
	}
}

func impactPriority(impact protocol.Impact) int {
	switch impact {
	case protocol.ImpactCritical:
		return 4
	case protocol.ImpactNotable:
		return 3
	case protocol.ImpactNormal:
		return 2
	case protocol.ImpactLow:
		return 1
	default:
		return 0
	}
}

// Element is the unresolved public plugin element.
type Element = protocol.Element

// Scene is an unresolved public plugin scene.
type Scene = protocol.Scene

type AudioCue = protocol.AudioCue

// ResolvedElement is a host-owned element whose logical asset may have been
// translated to a device path. Path never crosses the plugin wire boundary.
type ResolvedElement struct {
	Element
	Path string `json:"path,omitempty"`
}

// ResolvedScene is the host-owned form consumed by the device gateway.
type ResolvedScene struct {
	Elements []ResolvedElement `json:"elements"`
}

// ResolveScene copies a public scene into the host-owned resolved form.
func ResolveScene(scene Scene) ResolvedScene {
	result := ResolvedScene{Elements: make([]ResolvedElement, len(scene.Elements))}
	for index, element := range scene.Elements {
		result.Elements[index].Element = element
	}
	return result
}

type ResolvedAudioCue struct {
	AudioCue
	Path string `json:"path,omitempty"`
}

func ResolveAudioCue(cue AudioCue) ResolvedAudioCue {
	return ResolvedAudioCue{AudioCue: cue}
}

// Candidate is one plugin channel's current display offer.
type Candidate struct {
	PluginID          string                          `json:"plugin_id"`
	InstanceID        string                          `json:"instance_id"`
	Channel           string                          `json:"channel"`
	Key               string                          `json:"key"`
	Revision          uint64                          `json:"revision"`
	Generation        uint64                          `json:"generation"`
	Policy            Policy                          `json:"policy"`
	Band              Band                            `json:"band"`
	Impact            protocol.Impact                 `json:"impact"`
	AdmissionSequence uint64                          `json:"admission_sequence"`
	Reason            string                          `json:"reason,omitempty"`
	CreatedAt         time.Time                       `json:"created_at"`
	UpdatedAt         time.Time                       `json:"updated_at"`
	ExpiresAt         time.Time                       `json:"expires_at,omitempty"`
	HoldMS            int                             `json:"hold_ms,omitempty"`
	CooldownMS        int                             `json:"cooldown_ms,omitempty"`
	RequiresAck       bool                            `json:"requires_ack,omitempty"`
	Acknowledged      bool                            `json:"acknowledged,omitempty"`
	DevicePriority    int                             `json:"device_priority,omitempty"`
	Scene             Scene                           `json:"scene,omitempty"`
	BusyTimer         *protocol.BusyTimerPresentation `json:"busy_timer,omitempty"`
	AudioCue          *protocol.AudioCue              `json:"audio_cue,omitempty"`
}

// Identity is the comparable internal key for one plugin observation.
type Identity struct {
	PluginID   string
	InstanceID string
	Channel    string
	Key        string
}

func (i Identity) String() string {
	value, _ := identifier.Encode(i.PluginID, i.InstanceID, i.Channel, i.Key)
	return value
}

func (c Candidate) Identity() Identity {
	return Identity{PluginID: c.PluginID, InstanceID: c.InstanceID, Channel: c.Channel, Key: c.Key}
}

// ID returns the stable host-side identity used for replacement and history.
func (c Candidate) ID() string {
	return c.Identity().String()
}

// Hold returns the configured or policy-default minimum readable hold.
func (c Candidate) Hold() time.Duration {
	if c.HoldMS > 0 {
		return time.Duration(c.HoldMS) * time.Millisecond
	}
	switch c.Policy {
	case PolicyAttention, PolicyRotation:
		return 8 * time.Second
	case PolicyWhenRelevant:
		return 6 * time.Second
	default:
		return 0
	}
}

// Cooldown returns the configured or policy-default repeat cooldown.
func (c Candidate) Cooldown() time.Duration {
	if c.CooldownMS > 0 {
		return time.Duration(c.CooldownMS) * time.Millisecond
	}
	switch c.Policy {
	case PolicyAttention:
		return 30 * time.Second
	case PolicyWhenRelevant:
		return 60 * time.Second
	default:
		return 0
	}
}

// EffectiveDevicePriority returns a validated policy default when no override exists.
func (c Candidate) EffectiveDevicePriority() int {
	if c.DevicePriority >= 1 && c.DevicePriority <= 100 {
		return c.DevicePriority
	}
	switch c.Policy {
	case PolicyAttention, PolicyInteractive:
		return 100
	case PolicyWhenRelevant:
		return 60
	case PolicyRotation:
		return 20
	default:
		return 0
	}
}

// Eligible reports whether the candidate can participate at now.
func (c Candidate) Eligible(now time.Time) bool {
	if !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt) {
		return false
	}
	if c.Policy == PolicyAttention && c.RequiresAck && c.Acknowledged {
		return false
	}
	return true
}

// Validate rejects malformed or unsafe candidate data before it reaches arbitration.
func (c Candidate) Validate() error {
	var errs []error
	for name, value := range map[string]string{
		"plugin_id": c.PluginID, "instance_id": c.InstanceID, "channel": c.Channel, "key": c.Key,
	} {
		if err := identifier.Validate(name, value); err != nil {
			errs = append(errs, err)
		}
	}
	validPolicy := true
	switch c.Policy {
	case PolicyAttention, PolicyInteractive, PolicyWhenRelevant, PolicyRotation:
	default:
		validPolicy = false
		errs = append(errs, fmt.Errorf("unsupported policy %q", c.Policy))
	}
	validBand := true
	switch c.Band {
	case BandCriticalActionable, BandInteractive, BandActionable, BandRelevant, BandRotation:
	default:
		validBand = false
		errs = append(errs, fmt.Errorf("unsupported presentation band %q", c.Band))
	}
	validImpact := true
	switch c.Impact {
	case protocol.ImpactLow, protocol.ImpactNormal, protocol.ImpactNotable, protocol.ImpactCritical:
	default:
		validImpact = false
		errs = append(errs, fmt.Errorf("unsupported impact %q", c.Impact))
	}
	if validPolicy && validBand && validImpact {
		if expected := BandFor(c.Policy, c.Impact); c.Band != expected {
			errs = append(errs, fmt.Errorf("presentation band %q does not match policy %q and impact %q; want %q", c.Band, c.Policy, c.Impact, expected))
		}
	}
	if c.Revision == 0 {
		errs = append(errs, errors.New("revision must be greater than zero"))
	}
	if c.Generation == 0 {
		errs = append(errs, errors.New("generation must be greater than zero"))
	}
	if c.AdmissionSequence == 0 {
		errs = append(errs, errors.New("admission_sequence must be greater than zero"))
	}
	if c.DevicePriority < 0 || c.DevicePriority > 100 {
		errs = append(errs, errors.New("device_priority must be between 1 and 100 when set"))
	}
	if c.HoldMS < 0 || c.CooldownMS < 0 {
		errs = append(errs, errors.New("hold_ms and cooldown_ms must not be negative"))
	}
	hasScene := len(c.Scene.Elements) != 0
	hasBusyTimer := c.BusyTimer != nil
	if hasScene == hasBusyTimer {
		errs = append(errs, errors.New("candidate must contain exactly one of scene or busy_timer"))
	}
	if hasBusyTimer && !busyTimerThemePattern.MatchString(c.BusyTimer.Theme) {
		errs = append(errs, errors.New("busy_timer theme must be a safe lowercase identifier of at most 64 bytes"))
	}
	if hasScene {
		if err := c.Scene.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if c.AudioCue != nil {
		if err := c.AudioCue.Validate(c.UpdatedAt, c.ExpiresAt); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
