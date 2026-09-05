package codexquota

import (
	"github.com/lxdb/bsbctl/internal/codexusage"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	canvas                = codexusage.CanvasColor
	border                = codexusage.BorderColor
	signature             = codexusage.SignatureColor
	warning               = codexusage.WarningColor
	danger                = codexusage.DangerColor
	textColor             = codexusage.TextColor
	quotaWaitingColor     = warning
	quotaUnavailableColor = danger
)

func quotaScene(snapshot Snapshot, focus Window, config Config, signal signalDisposition) protocol.Scene {
	return codexusage.Scene(snapshot, focus, quotaPresentationConfig(config), signal, codexMarkSource)
}

func quotaStatusScene(status, color string) protocol.Scene {
	return protocol.Scene{Elements: []protocol.Element{
		{ID: "front-background", Display: protocol.DisplayFront, X: 0, Y: 0, Rectangle: &protocol.RectangleElement{Width: 72, Height: 16, Color: canvas}},
		{ID: "front-accent", Display: protocol.DisplayFront, X: 1, Y: 1, Rectangle: &protocol.RectangleElement{Width: 3, Height: 14, Color: color}},
		{ID: "front-status", Display: protocol.DisplayFront, X: 8, Y: 3, Text: &protocol.TextElement{Value: status, Font: "normal", Color: color}},
		{ID: "back-background", Display: protocol.DisplayBack, X: 0, Y: 0, Rectangle: &protocol.RectangleElement{Width: 160, Height: 80, Color: canvas}},
		{ID: "back-accent", Display: protocol.DisplayBack, X: 1, Y: 1, Rectangle: &protocol.RectangleElement{Width: 4, Height: 78, Color: color}},
		{ID: "back-title", Display: protocol.DisplayBack, X: 8, Y: 12, Text: &protocol.TextElement{Value: "CODEX QUOTA", Font: "normal", Color: textColor}},
		{ID: "back-status", Display: protocol.DisplayBack, X: 8, Y: 34, Text: &protocol.TextElement{Value: status, Font: "large", Color: color}},
		{ID: "back-help", Display: protocol.DisplayBack, X: 8, Y: 62, Text: &protocol.TextElement{Value: "BACK TO CLOSE", Font: "tiny", Color: textColor}},
	}}
}
