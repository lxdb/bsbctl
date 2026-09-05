package protocol

import (
	"errors"
	"fmt"
	"math"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// AssetRef names either one authenticated package asset or one firmware stock asset.
type AssetRef struct {
	PackagePath string `json:"package_path,omitempty"`
	StockName   string `json:"stock_name,omitempty"`
}

// ValidatePackagePath validates one canonical package-relative asset source.
func ValidatePackagePath(value string) error {
	if len(value) > 255 || path.Clean(value) != value || strings.HasPrefix(value, "/") ||
		strings.Contains(value, "\\") || !relativeAssetPathPattern.MatchString(value) {
		return errors.New("package path must be a canonical package-relative path")
	}
	return nil
}

// Validate requires exactly one canonical authenticated package path or typed stock basename.
func (asset AssetRef) Validate() error {
	if (asset.PackagePath == "") == (asset.StockName == "") {
		return errors.New("asset must contain exactly one of package_path or stock_name")
	}
	if asset.PackagePath != "" {
		return ValidatePackagePath(asset.PackagePath)
	}
	if !stockNamePattern.MatchString(asset.StockName) {
		return errors.New("stock_name must be a safe firmware basename with a supported extension")
	}
	return nil
}

// Display selects the BUSY Bar front or back canvas.
type Display string

const (
	DisplayFront Display = "front"
	DisplayBack  Display = "back"
)

// Element places exactly one supported presentation payload on one display.
type Element struct {
	ID        string            `json:"id"`
	Display   Display           `json:"display"`
	X         int               `json:"x,omitzero"`
	Y         int               `json:"y,omitzero"`
	Text      *TextElement      `json:"text,omitempty"`
	Image     *ImageElement     `json:"image,omitempty"`
	Animation *AnimationElement `json:"animation,omitempty"`
	Rectangle *RectangleElement `json:"rectangle,omitempty"`
	Countdown *CountdownElement `json:"countdown,omitempty"`
}

// TextElement renders bounded text with optional alignment and marquee behavior.
type TextElement struct {
	Value   string   `json:"value"`
	Font    string   `json:"font,omitempty"`
	Color   string   `json:"color,omitempty"`
	Align   string   `json:"align,omitempty"`
	Width   int      `json:"width,omitzero"`
	Marquee *Marquee `json:"marquee,omitempty"`
}

// Marquee defines firmware-native horizontal scrolling in pixels and milliseconds.
type Marquee struct {
	PixelsPerMinute         uint32 `json:"pixels_per_minute"`
	StartDelayMilliseconds  uint32 `json:"start_delay_milliseconds,omitzero"`
	RepeatDelayMilliseconds uint32 `json:"repeat_delay_milliseconds,omitzero"`
}

// ImageElement renders one image asset.
type ImageElement struct {
	Asset AssetRef `json:"asset"`
}

// AnimationElement renders one animation asset.
type AnimationElement struct {
	Asset AssetRef `json:"asset"`
	Loop  bool     `json:"loop,omitzero"`
}

// RectangleElement renders a positive-size solid rectangle.
type RectangleElement struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Color  string `json:"color"`
}

// CountdownShowHours controls when a countdown renders its hour field.
type CountdownShowHours string

const (
	CountdownShowHoursWhenNonZero CountdownShowHours = "when_non_zero"
	CountdownShowHoursAlways      CountdownShowHours = "always"
)

// CountdownElement renders a future Unix-second deadline.
type CountdownElement struct {
	EndsAtUnixSeconds int64              `json:"ends_at_unix_seconds"`
	ShowHours         CountdownShowHours `json:"show_hours"`
	Color             string             `json:"color"`
	Align             string             `json:"align,omitempty"`
}

// Scene is a complete bounded presentation for both device displays.
type Scene struct {
	Elements []Element `json:"elements"`
}

// BusyTimerPresentation requests a firmware-native BUSY timer theme.
type BusyTimerPresentation struct {
	Theme string `json:"theme"`
}

// AudioCue requests one expiring best-effort firmware stock sound.
type AudioCue struct {
	ID        string    `json:"id"`
	Asset     AssetRef  `json:"asset"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Validate checks the stock sound and its bounded future expiry.
func (audio AudioCue) Validate(now, validUntil time.Time) error {
	var errs []error
	if err := validateIdentifier("audio id", audio.ID); err != nil {
		errs = append(errs, err)
	}
	if err := audio.Asset.Validate(); err != nil {
		errs = append(errs, err)
	} else if audio.Asset.PackagePath != "" {
		errs = append(errs, errors.New("audio assets must use stock_name in protocol 1.0"))
	} else if audio.Asset.StockName != "" && !strings.HasSuffix(audio.Asset.StockName, ".snd") {
		errs = append(errs, errors.New("audio stock_name must end in .snd"))
	}
	if err := validateRequiredTimestamp("audio expires_at", audio.ExpiresAt); err != nil {
		errs = append(errs, err)
	} else {
		if !now.IsZero() && !audio.ExpiresAt.After(now) {
			errs = append(errs, errors.New("audio expires_at must be in the future"))
		}
		if !validUntil.IsZero() && audio.ExpiresAt.After(validUntil) {
			errs = append(errs, errors.New("audio expires_at must not exceed valid_until"))
		}
	}
	return errors.Join(errs...)
}

// Validate checks scene cardinality, element identities, and element bounds.
func (scene Scene) Validate() error {
	if len(scene.Elements) == 0 || len(scene.Elements) > MaxSceneElements {
		return fmt.Errorf("scene must contain between 1 and %d elements", MaxSceneElements)
	}
	var errs []error
	ids := make(map[string]struct{}, len(scene.Elements))
	for index, element := range scene.Elements {
		if err := element.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("scene element %d: %w", index, err))
		}
		if _, exists := ids[element.ID]; exists {
			errs = append(errs, fmt.Errorf("scene element id %q is duplicated", element.ID))
		}
		ids[element.ID] = struct{}{}
	}
	return errors.Join(errs...)
}

// Validate checks coordinates and requires exactly one supported element payload.
func (element Element) Validate() error {
	var errs []error
	if err := validateIdentifier("element id", element.ID); err != nil {
		errs = append(errs, err)
	}
	if element.Display != DisplayFront && element.Display != DisplayBack {
		errs = append(errs, fmt.Errorf("unsupported element display %q", element.Display))
	} else {
		width, height := 72, 16
		if element.Display == DisplayBack {
			width, height = 160, 80
		}
		if element.X < 0 || element.X >= width || element.Y < 0 || element.Y >= height {
			errs = append(errs, fmt.Errorf("element coordinates must fit the %dx%d %s display", width, height, element.Display))
		}
	}
	variants := countTrue(element.Text != nil, element.Image != nil, element.Animation != nil, element.Rectangle != nil, element.Countdown != nil)
	if variants != 1 {
		errs = append(errs, errors.New("element must contain exactly one of text, image, animation, rectangle, or countdown"))
	}
	if element.Text != nil {
		if len(element.Text.Value) > MaxTextBytes || !utf8.ValidString(element.Text.Value) {
			errs = append(errs, fmt.Errorf("text must be valid UTF-8 no larger than %d bytes", MaxTextBytes))
		}
		if element.Text.Marquee != nil && element.Text.Marquee.PixelsPerMinute == 0 {
			errs = append(errs, errors.New("marquee pixels_per_minute must be greater than zero"))
		}
		if !validFont(element.Text.Font) {
			errs = append(errs, fmt.Errorf("unsupported text font %q", element.Text.Font))
		}
		if element.Text.Color != "" && !colorPattern.MatchString(element.Text.Color) {
			errs = append(errs, errors.New("text color must use #RRGGBBAA"))
		}
		if !validAlign(element.Text.Align) {
			errs = append(errs, fmt.Errorf("unsupported text align %q", element.Text.Align))
		}
		if element.Text.Width < 0 || uint64(element.Text.Width) > math.MaxUint32 {
			errs = append(errs, errors.New("text width must be omitted or fit an unsigned 32-bit integer"))
		}
	}
	if element.Image != nil {
		if err := element.Image.Asset.Validate(); err != nil {
			errs = append(errs, err)
		} else if element.Image.Asset.StockName != "" && !strings.HasSuffix(element.Image.Asset.StockName, ".image") {
			errs = append(errs, errors.New("image stock_name must end in .image"))
		}
	}
	if element.Animation != nil {
		if err := element.Animation.Asset.Validate(); err != nil {
			errs = append(errs, err)
		} else if element.Animation.Asset.StockName != "" && !strings.HasSuffix(element.Animation.Asset.StockName, ".anim") {
			errs = append(errs, errors.New("animation stock_name must end in .anim"))
		}
	}
	if element.Rectangle != nil {
		if element.Rectangle.Width <= 0 || element.Rectangle.Width > math.MaxInt32 || element.Rectangle.Height <= 0 || element.Rectangle.Height > math.MaxInt32 {
			errs = append(errs, errors.New("rectangle width and height must be positive signed 32-bit integers"))
		}
		if !colorPattern.MatchString(element.Rectangle.Color) {
			errs = append(errs, errors.New("rectangle color must use #RRGGBBAA"))
		}
	}
	if element.Countdown != nil {
		if element.Countdown.EndsAtUnixSeconds <= 0 {
			errs = append(errs, errors.New("countdown ends_at_unix_seconds must be greater than zero"))
		}
		if element.Countdown.ShowHours != CountdownShowHoursWhenNonZero && element.Countdown.ShowHours != CountdownShowHoursAlways {
			errs = append(errs, fmt.Errorf("unsupported countdown show_hours %q", element.Countdown.ShowHours))
		}
		if !colorPattern.MatchString(element.Countdown.Color) {
			errs = append(errs, errors.New("countdown color must use #RRGGBBAA"))
		}
		if !validAlign(element.Countdown.Align) {
			errs = append(errs, fmt.Errorf("unsupported countdown align %q", element.Countdown.Align))
		}
	}
	return errors.Join(errs...)
}

func validFont(value string) bool {
	switch value {
	case "tiny", "small", "normal", "condensed", "bold", "large", "extra_large", "global":
		return true
	default:
		return false
	}
}

func validAlign(value string) bool {
	switch value {
	case "", "top_left", "top_mid", "top_right", "mid_left", "center", "mid_right", "bottom_left", "bottom_mid", "bottom_right":
		return true
	default:
		return false
	}
}
