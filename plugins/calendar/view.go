package calendar

import "github.com/lxdb/bsbctl/sdk/protocol"

const (
	calendarBlack     = "#000000FF"
	calendarWhite     = "#FFFFFFFF"
	calendarSecondary = "#A8ADB3FF"
	calendarAccent    = "#4C8BF5FF"
	marqueeRate       = 1000
	marqueeStartDelay = 1000
	marqueeRepeat     = 2500
)

func calendarScene(card calendarCard) protocol.Scene {
	animation, icon := calendarReminderAnimation, calendarReminderIcon
	if card.Channel == ChannelActive {
		animation, icon = calendarActiveAnimation, calendarActiveIcon
	}
	timestamp := card.CountdownAt.Unix()
	elements := []protocol.Element{
		calendarRectangle("front-background", "front", 0, 0, 72, 16, calendarBlack),
		calendarStock("front-calendar", "animation", "front", animation, 0, 0, true),
		calendarMarquee("front-title", "front", card.Title, "normal", calendarWhite, 18, 0, 54),
		calendarStock("front-timer-icon", "image", "front", icon, 18, 10, false),
		calendarCountdown("front-countdown", "front", timestamp, calendarWhite, 25, 9, ""),
		calendarRectangle("back-background", "back", 0, 0, 160, 80, calendarBlack),
		calendarStock("back-calendar", "animation", "back", animation, 8, 8, true),
		calendarText("back-phase", "back", card.State, "tiny", calendarSecondary, 30, 8, ""),
		calendarMarquee("back-title", "back", card.Title, "normal", calendarWhite, 30, 19, 122),
		calendarStock("back-timer-icon", "image", "back", icon, 30, 40, false),
		calendarCountdown("back-countdown", "back", timestamp, calendarWhite, 39, 38, ""),
		calendarText("back-action", "back", "START / OPTIONS", "small", calendarAccent, 8, 66, ""),
	}
	return protocol.Scene{Elements: elements}
}

func calendarRectangle(id, display string, x, y, width, height int, color string) protocol.Element {
	return protocol.Element{ID: id, Display: protocol.Display(display), X: x, Y: y, Rectangle: &protocol.RectangleElement{Width: width, Height: height, Color: color}}
}

func calendarText(id, display, value, font, color string, x, y int, align string) protocol.Element {
	return protocol.Element{ID: id, Display: protocol.Display(display), X: x, Y: y, Text: &protocol.TextElement{Value: value, Font: font, Color: color, Align: align}}
}

func calendarMarquee(id, display, value, font, color string, x, y, width int) protocol.Element {
	element := calendarText(id, display, value, font, color, x, y, "")
	element.Text.Width = width
	element.Text.Marquee = &protocol.Marquee{PixelsPerMinute: marqueeRate, StartDelayMilliseconds: marqueeStartDelay, RepeatDelayMilliseconds: marqueeRepeat}
	return element
}

func calendarStock(id, kind, display, assetID string, x, y int, loop bool) protocol.Element {
	result := protocol.Element{ID: id, Display: protocol.Display(display), X: x, Y: y}
	asset := protocol.AssetRef{StockName: assetID}
	if kind == "animation" {
		result.Animation = &protocol.AnimationElement{Asset: asset, Loop: loop}
	} else {
		result.Image = &protocol.ImageElement{Asset: asset}
	}
	return result
}

func calendarCountdown(id, display string, timestamp int64, color string, x, y int, align string) protocol.Element {
	return protocol.Element{ID: id, Display: protocol.Display(display), X: x, Y: y, Countdown: &protocol.CountdownElement{EndsAtUnixSeconds: timestamp, Color: color, ShowHours: protocol.CountdownShowHoursWhenNonZero, Align: align}}
}
