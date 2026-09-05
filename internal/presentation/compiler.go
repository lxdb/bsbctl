package presentation

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/lxdb/bsbctl/sdk/protocol"
	busylib "github.com/lxdb/busylib-go"
)

// ErrInvalidPresentation identifies a deterministic presentation that cannot
// be represented by the supported BUSY Bar API.
var ErrInvalidPresentation = errors.New("invalid BUSY Bar presentation")

// AssetCompilerResolver resolves authenticated package IDs and validated stock
// basenames without exposing physical paths to plugins.
type AssetCompilerResolver interface {
	ResolveScene(string, Scene) (ResolvedScene, error)
	ResolveAudioCue(string, AudioCue) (ResolvedAudioCue, error)
}

// ValidateObservation compiles every device-bound part of an observation
// without I/O. A successful return means the observation is representable by
// the supported busylib contract at publication time.
func ValidateObservation(pluginID string, observation protocol.Observation, resolver AssetCompilerResolver) error {
	if observation.Disposition == protocol.DispositionResolved {
		return nil
	}
	if observation.Scene != nil {
		resolved := ResolveScene(*observation.Scene)
		if resolver != nil {
			var err error
			resolved, err = resolver.ResolveScene(pluginID, *observation.Scene)
			if err != nil {
				return err
			}
		}
		if _, err := CompileScene("bsbctl", 100, resolved); err != nil {
			return err
		}
	}
	if observation.BusyTimer != nil {
		if err := CompileBusyTimer(observation.BusyTimer.Theme); err != nil {
			return err
		}
	}
	if observation.Audio != nil {
		resolved := ResolveAudioCue(*observation.Audio)
		if resolver != nil {
			var err error
			resolved, err = resolver.ResolveAudioCue(pluginID, *observation.Audio)
			if err != nil {
				return err
			}
		}
		if _, err := CompileAudio("bsbctl", resolved); err != nil {
			return err
		}
	}
	return nil
}

// CompileBusyTimer validates the firmware theme field through the authoritative
// busylib boundary without reading or mutating device state.
func CompileBusyTimer(theme string) error {
	if err := (busylib.BusyBarSettings{Theme: theme}).Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPresentation, err)
	}
	return nil
}

// CompileScene performs the pure public-DSL to busylib translation used at
// publication and again at the device boundary. It never performs device I/O.
func CompileScene(applicationName string, priority int, scene ResolvedScene) (busylib.DisplayElements, error) {
	elements := make([]busylib.DisplayElement, 0, len(scene.Elements))
	for _, element := range scene.Elements {
		base := busylib.BaseDisplayElement{
			ID: element.ID, X: new(element.X), Y: new(element.Y),
			Display: busylib.DisplayTarget(element.Display),
		}
		switch {
		case element.Text != nil:
			text := element.Text
			font := busylib.Font(text.Font)
			if font == "" {
				font = busylib.FontNormal
			}
			base.Align = busylib.DisplayAlign(text.Align)
			var scrollRate, startDelay, repeatDelay int
			if text.Marquee != nil {
				scrollRate = int(text.Marquee.PixelsPerMinute)
				startDelay = int(text.Marquee.StartDelayMilliseconds)
				repeatDelay = int(text.Marquee.RepeatDelayMilliseconds)
			}
			elements = append(elements, busylib.TextElement{
				BaseDisplayElement: base, Text: text.Value, Font: font, Color: text.Color, Width: text.Width,
				ScrollRate: scrollRate, ScrollStartDelay: startDelay, ScrollRepeatDelay: repeatDelay,
			})
		case element.Image != nil:
			path, stockPath := resolvedAssetPaths(element.Path, element.Image.Asset.StockName != "")
			elements = append(elements, busylib.ImageElement{BaseDisplayElement: base, Path: path, StockPath: stockPath})
		case element.Animation != nil:
			path, stockPath := resolvedAssetPaths(element.Path, element.Animation.Asset.StockName != "")
			elements = append(elements, busylib.AnimationElement{
				BaseDisplayElement: base, Path: path, StockPath: stockPath, Loop: element.Animation.Loop,
			})
		case element.Countdown != nil:
			countdown := element.Countdown
			base.Align = busylib.DisplayAlign(countdown.Align)
			elements = append(elements, busylib.CountdownElement{
				BaseDisplayElement: base, Timestamp: strconv.FormatInt(countdown.EndsAtUnixSeconds, 10), Color: countdown.Color,
				Direction: busylib.CountdownTimeLeft, ShowHours: busylib.CountdownShowHours(countdown.ShowHours),
			})
		case element.Rectangle != nil:
			rectangleValue := element.Rectangle
			borderWidth := 0
			rectangle := busylib.RectangleElement{
				BaseDisplayElement: base, Width: rectangleValue.Width, Height: rectangleValue.Height,
				Fill: busylib.RectangleFillNone, BorderWidth: &borderWidth,
			}
			if rectangleValue.Color != "" {
				rectangle.Fill = busylib.RectangleFillSolid
				rectangle.FillColors = []string{rectangleValue.Color}
			}
			elements = append(elements, rectangle)
		default:
			return busylib.DisplayElements{}, fmt.Errorf("%w: unsupported scene element %q", ErrInvalidPresentation, element.ID)
		}
	}
	request := busylib.DisplayElements{ApplicationName: applicationName, Priority: priority, Elements: elements}
	if err := request.Validate(); err != nil {
		return busylib.DisplayElements{}, fmt.Errorf("%w: %v", ErrInvalidPresentation, err)
	}
	if warnings := request.Warnings(); len(warnings) > 0 {
		return busylib.DisplayElements{}, fmt.Errorf("%w: %s: %s", ErrInvalidPresentation, warnings[0].Field, warnings[0].Message)
	}
	return request, nil
}

// CompileAudio performs the pure resolved-cue to busylib translation.
func CompileAudio(applicationName string, cue ResolvedAudioCue) (busylib.PlayAudio, error) {
	request := busylib.PlayAudio{ApplicationName: applicationName}
	if cue.Asset.StockName != "" {
		request.StockPath = cue.Path
	} else {
		request.Path = cue.Path
	}
	if cue.Path == "" {
		return busylib.PlayAudio{}, fmt.Errorf("%w: audio asset has not been resolved", ErrInvalidPresentation)
	}
	if err := request.Validate(); err != nil {
		return busylib.PlayAudio{}, fmt.Errorf("%w: %v", ErrInvalidPresentation, err)
	}
	return request, nil
}

func resolvedAssetPaths(path string, stock bool) (string, string) {
	if stock {
		return "", path
	}
	return path, ""
}
