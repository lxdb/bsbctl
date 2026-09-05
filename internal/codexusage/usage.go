// Package codexusage normalizes and renders Codex account rate-limit windows
// independently of the provider used to obtain them.
package codexusage

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	CanvasColor    = "#071522FF"
	SurfaceColor   = "#171A21FF"
	BorderColor    = "#2B3940FF"
	TextColor      = "#EAF4F2FF"
	SecondaryColor = "#9AAFB2FF"
	SignatureColor = "#2AC7B5FF"
	WarningColor   = "#F2B84BFF"
	DangerColor    = "#FF786FFF"
)

type RawWindow struct {
	UsedPercent int
	Duration    time.Duration
	ResetsAt    time.Time
}

type Window struct {
	UsedPercent      int
	RemainingPercent int
	Duration         time.Duration
	ResetsAt         time.Time
}

type Snapshot struct {
	Windows   []Window
	UpdatedAt time.Time
}

type PresentationConfig struct {
	Label                    string
	Badge                    string
	ShowBadge                bool
	WarningRemainingPercent  int
	CriticalRemainingPercent int
}

type Signal uint8

const (
	SignalNone Signal = iota
	SignalLow
	SignalCritical
)

func NormalizeWindows(raw []RawWindow, now time.Time) (Snapshot, error) {
	windows := make([]Window, 0, len(raw))
	for _, source := range raw {
		if source.Duration <= 0 {
			continue
		}
		used := min(100, max(0, source.UsedPercent))
		reset := source.ResetsAt
		if reset.IsZero() || reset.Unix() <= 0 {
			reset = time.Time{}
		} else {
			reset = reset.UTC()
		}
		windows = append(windows, Window{
			UsedPercent: used, RemainingPercent: 100 - used,
			Duration: source.Duration, ResetsAt: reset,
		})
	}
	if len(windows) == 0 {
		return Snapshot{}, errors.New("usage_windows_unavailable")
	}
	slices.SortStableFunc(windows, func(left, right Window) int { return cmp.Compare(left.Duration, right.Duration) })
	return Snapshot{Windows: windows, UpdatedAt: now.UTC()}, nil
}

func SignalFor(snapshot Snapshot, config PresentationConfig) Signal {
	if len(snapshot.Windows) == 0 {
		return SignalNone
	}
	lowest := 101
	for _, window := range snapshot.Windows {
		lowest = min(lowest, window.RemainingPercent)
	}
	if lowest <= config.CriticalRemainingPercent {
		return SignalCritical
	}
	if lowest <= config.WarningRemainingPercent {
		return SignalLow
	}
	return SignalNone
}

func MostConstrained(snapshot Snapshot) Window {
	result := snapshot.Windows[0]
	for _, window := range snapshot.Windows[1:] {
		if window.RemainingPercent < result.RemainingPercent ||
			(window.RemainingPercent == result.RemainingPercent && window.Duration < result.Duration) {
			result = window
		}
	}
	return result
}

func SummaryKey(duration time.Duration) string {
	switch FrontWindowLabel(duration) {
	case "5H":
		return "quota-5h"
	case "1W":
		return "quota-1w"
	case "1M":
		return "quota-1m"
	default:
		return fmt.Sprintf("quota-%ds", int64(duration/time.Second))
	}
}

func FrontWindowLabel(duration time.Duration) string {
	switch {
	case duration == 5*time.Hour:
		return "5H"
	case duration == 7*24*time.Hour:
		return "1W"
	case duration >= 28*24*time.Hour && duration <= 31*24*time.Hour:
		return "1M"
	default:
		return "??"
	}
}

func BackWindowLabel(duration time.Duration) string {
	switch FrontWindowLabel(duration) {
	case "5H":
		return "5 HOURS"
	case "1W":
		return "WEEKLY"
	case "1M":
		return "MONTHLY"
	default:
		if duration%time.Hour == 0 {
			return fmt.Sprintf("%d HOURS", int(duration/time.Hour))
		}
		return "CUSTOM WINDOW"
	}
}

func Scene(snapshot Snapshot, focus Window, config PresentationConfig, signal Signal, assetPath string) protocol.Scene {
	elements := []protocol.Element{
		rectangle("front-background", "front", 0, 0, 72, 16, CanvasColor),
		rectangle("back-background", "back", 0, 0, 160, 80, CanvasColor),
		{ID: "front-codex-mark", Display: protocol.DisplayFront, X: 1, Y: 1, Image: &protocol.ImageElement{Asset: protocol.AssetRef{PackagePath: assetPath}}},
	}
	label := FrontWindowLabel(focus.Duration)
	state, stateColor := "LEFT", SecondaryColor
	switch signal {
	case SignalLow:
		state, stateColor = "LOW", WarningColor
	case SignalCritical:
		state, stateColor = "CRIT", DangerColor
	}
	if config.ShowBadge {
		label = config.Badge + " " + label
	}
	color := quotaColor(focus.RemainingPercent, config)
	elements = append(elements,
		text("front-window-label", "front", label, "normal", TextColor, 18, 1, ""),
		text("front-window-value", "front", formatFrontRemaining(focus.RemainingPercent, config), "normal", color, 70, 1, "top_right"),
		text("front-window-state", "front", state, "tiny", stateColor, 18, 10, ""),
		rectangle("front-window-track", "front", 35, 11, 36, 4, BorderColor),
	)
	fillWidth := int(math.Round(36 * float64(focus.RemainingPercent) / 100))
	fillColor := color
	if fillWidth == 0 {
		fillWidth = 1
		fillColor = BorderColor
	}
	elements = append(elements, rectangle("front-window-fill", "front", 35, 11, fillWidth, 4, fillColor))

	title := "CODEX QUOTA: " + strings.ToUpper(config.Label)
	elements = append(elements, text("back-title", "back", title, "normal", TextColor, 74, 2, "top_mid"))
	if len(snapshot.Windows) == 1 {
		window := snapshot.Windows[0]
		color := quotaColor(window.RemainingPercent, config)
		elements = append(elements,
			text("back-window-0-label", "back", BackWindowLabel(window.Duration), "normal", TextColor, 8, 18, ""),
			text("back-window-0-value", "back", fmt.Sprintf("%d%% LEFT", window.RemainingPercent), "large", color, 142, 15, "top_right"),
		)
		elements = append(elements, backBar("back-window-0", window.RemainingPercent, color, 8, 38, 136)...)
		elements = append(elements, resetElements("back-window-0", snapshot.UpdatedAt, window.ResetsAt, "normal", 8, 54, 142)...)
		return protocol.Scene{Elements: elements}
	}
	for index, window := range snapshot.Windows {
		if index >= 2 {
			break
		}
		prefix := fmt.Sprintf("back-window-%d", index)
		y := 15 + index*30
		color := quotaColor(window.RemainingPercent, config)
		elements = append(elements,
			text(prefix+"-label", "back", BackWindowLabel(window.Duration), "normal", TextColor, 8, y, ""),
			text(prefix+"-value", "back", fmt.Sprintf("%d%% LEFT", window.RemainingPercent), "normal", color, 142, y, "top_right"),
		)
		elements = append(elements, backBar(prefix, window.RemainingPercent, color, 8, y+11, 136)...)
		elements = append(elements, resetElements(prefix, snapshot.UpdatedAt, window.ResetsAt, "small", 8, y+19, 142)...)
	}
	return protocol.Scene{Elements: elements}
}

func backBar(prefix string, remaining int, color string, x, y, width int) []protocol.Element {
	result := []protocol.Element{
		rectangle(prefix+"-border", "back", x, y, width, 7, BorderColor),
		rectangle(prefix+"-track", "back", x+1, y+1, width-2, 5, SurfaceColor),
	}
	fillWidth := int(math.Round(float64(width-2) * float64(remaining) / 100))
	fillColor := color
	if fillWidth == 0 {
		fillWidth = 1
		fillColor = SurfaceColor
	}
	return append(result, rectangle(prefix+"-fill", "back", x+1, y+1, fillWidth, 5, fillColor))
}

func quotaColor(remaining int, config PresentationConfig) string {
	if remaining <= config.CriticalRemainingPercent {
		return DangerColor
	}
	if remaining <= config.WarningRemainingPercent {
		return WarningColor
	}
	return SignatureColor
}

func formatFrontRemaining(remaining int, config PresentationConfig) string {
	remaining = min(100, max(0, remaining))
	if remaining <= config.CriticalRemainingPercent {
		return fmt.Sprintf("%d%%!", remaining)
	}
	return fmt.Sprintf("%d%%", remaining)
}

func resetElements(prefix string, now, reset time.Time, font string, x, y, countdownX int) []protocol.Element {
	label := "RESET UNKNOWN"
	timestamp := int64(1)
	countdownColor := CanvasColor
	if !reset.IsZero() && reset.Unix() > 0 {
		timestamp = reset.Unix()
		remaining := reset.Sub(now)
		if remaining > 0 && remaining < 60*time.Hour {
			label = "RESET IN"
			countdownColor = SecondaryColor
		} else if remaining >= 60*time.Hour {
			days := remaining / (24 * time.Hour)
			hours := remaining % (24 * time.Hour) / time.Hour
			label = fmt.Sprintf("RESET IN %dD", days)
			if hours > 0 {
				label += fmt.Sprintf(" %dH", hours)
			}
		} else {
			label = "RESETTING"
		}
	}
	return []protocol.Element{
		text(prefix+"-reset", "back", label, font, SecondaryColor, x, y, ""),
		countdown(prefix+"-reset-countdown", "back", timestamp, countdownColor, countdownX, y-2, "top_right"),
	}
}

func rectangle(id, display string, x, y, width, height int, color string) protocol.Element {
	return protocol.Element{ID: id, Display: protocol.Display(display), X: x, Y: y, Rectangle: &protocol.RectangleElement{Width: width, Height: height, Color: color}}
}

func text(id, display, value, font, color string, x, y int, align string) protocol.Element {
	return protocol.Element{ID: id, Display: protocol.Display(display), X: x, Y: y, Text: &protocol.TextElement{Value: value, Font: font, Color: color, Align: align}}
}

func countdown(id, display string, timestamp int64, color string, x, y int, align string) protocol.Element {
	return protocol.Element{
		ID: id, Display: protocol.Display(display), X: x, Y: y,
		Countdown: &protocol.CountdownElement{EndsAtUnixSeconds: timestamp, Color: color, ShowHours: protocol.CountdownShowHoursWhenNonZero, Align: align},
	}
}
