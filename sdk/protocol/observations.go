package protocol

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Disposition describes the observation lifecycle and whether user action is available.
type Disposition string

const (
	DispositionSnapshot   Disposition = "snapshot"
	DispositionNotable    Disposition = "notable"
	DispositionActionable Disposition = "actionable"
	DispositionResolved   Disposition = "resolved"
)

// Impact carries domain severity without selecting global attention policy.
type Impact string

const (
	ImpactLow      Impact = "low"
	ImpactNormal   Impact = "normal"
	ImpactNotable  Impact = "notable"
	ImpactCritical Impact = "critical"
)

var (
	reasonCodePattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	themePattern             = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	stockNamePattern         = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,54}\.(image|anim|snd)$`)
	relativeAssetPathPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	colorPattern             = regexp.MustCompile(`^#[A-Fa-f0-9]{8}$`)
)

// Observation is plugin-owned domain state submitted to the core attention arbiter.
type Observation struct {
	Instance    InstanceRef            `json:"instance"`
	Channel     string                 `json:"channel"`
	Key         string                 `json:"key"`
	Revision    uint64                 `json:"revision"`
	Disposition Disposition            `json:"disposition"`
	Impact      Impact                 `json:"impact"`
	ReasonCode  string                 `json:"reason_code"`
	ObservedAt  time.Time              `json:"observed_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
	ValidUntil  time.Time              `json:"valid_until,omitzero"`
	Scene       *Scene                 `json:"scene,omitempty"`
	BusyTimer   *BusyTimerPresentation `json:"busy_timer,omitempty"`
	Audio       *AudioCue              `json:"audio,omitempty"`
}

// Validate checks observation identity, timestamps, lifecycle, and presentation unions.
func (observation Observation) Validate(now time.Time) error {
	var errs []error
	if err := observation.Instance.Validate(); err != nil {
		errs = append(errs, err)
	}
	for name, value := range map[string]string{"channel": observation.Channel, "key": observation.Key} {
		if err := validateIdentifier(name, value); err != nil {
			errs = append(errs, err)
		}
	}
	if observation.Revision == 0 {
		errs = append(errs, errors.New("revision must be greater than zero"))
	}
	switch observation.Disposition {
	case DispositionSnapshot, DispositionNotable, DispositionActionable, DispositionResolved:
	default:
		errs = append(errs, fmt.Errorf("unsupported disposition %q", observation.Disposition))
	}
	switch observation.Impact {
	case ImpactLow, ImpactNormal, ImpactNotable, ImpactCritical:
	default:
		errs = append(errs, fmt.Errorf("unsupported impact %q", observation.Impact))
	}
	if !reasonCodePattern.MatchString(observation.ReasonCode) {
		errs = append(errs, errors.New("reason_code must be a safe lowercase identifier of at most 64 bytes"))
	}
	if err := validateRequiredTimestamp("observed_at", observation.ObservedAt); err != nil {
		errs = append(errs, err)
	}
	if err := validateRequiredTimestamp("updated_at", observation.UpdatedAt); err != nil {
		errs = append(errs, err)
	}
	if observation.Disposition == DispositionResolved {
		if observation.Scene != nil || observation.BusyTimer != nil || observation.Audio != nil {
			errs = append(errs, errors.New("resolved observation must not contain a presentation"))
		}
		return errors.Join(errs...)
	}
	if err := validateRequiredTimestamp("valid_until", observation.ValidUntil); err != nil {
		errs = append(errs, err)
	} else if !observation.ValidUntil.After(now) {
		errs = append(errs, errors.New("valid_until must be in the future"))
	}
	if (observation.Scene == nil) == (observation.BusyTimer == nil) {
		errs = append(errs, errors.New("unresolved observation must contain exactly one of scene or busy_timer"))
	}
	if observation.Scene != nil {
		if err := observation.Scene.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if observation.BusyTimer != nil {
		if !themePattern.MatchString(observation.BusyTimer.Theme) {
			errs = append(errs, errors.New("busy_timer theme must be a safe lowercase firmware directory name of at most 64 bytes"))
		}
		if observation.ValidUntil.Sub(now) > MaxBusyTimerDuration {
			errs = append(errs, errors.New("busy_timer duration must not exceed 24 hours"))
		}
	}
	if observation.Audio != nil {
		if err := observation.Audio.Validate(now, observation.ValidUntil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
