package macresources

import (
	"fmt"
	"math"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	colorText                = "#EAF4F2FF"
	colorTrack               = "#111A20FF"
	colorBorder              = "#2B3940FF"
	colorHealthy             = "#35D07FFF"
	colorWarning             = "#F2B84BFF"
	colorCritical            = "#FF786FFF"
	colorCanvas              = "#071522FF"
	resourceWaitingColor     = colorWarning
	resourceUnavailableColor = colorCritical
)

type reading struct {
	CPUPercent       float64
	MemoryPercent    float64
	RXBytesPerSecond float64
	TXBytesPerSecond float64
}

func resourceStatusScene(status, color string) protocol.Scene {
	return protocol.Scene{Elements: []protocol.Element{
		resourceRectangle("front-background", "front", 0, 0, 72, 16, colorCanvas),
		resourceRectangle("front-accent", "front", 1, 1, 3, 14, color),
		resourceText("front-status", "front", status, "normal", color, 8, 3, "", 0),
		resourceRectangle("back-background", "back", 0, 0, 160, 80, colorCanvas),
		resourceRectangle("back-accent", "back", 1, 1, 4, 78, color),
		resourceText("back-title", "back", "MAC RESOURCES", "normal", colorText, 8, 12, "", 0),
		resourceText("back-status", "back", status, "large", color, 8, 34, "", 0),
		resourceText("back-help", "back", "BACK TO CLOSE", "tiny", colorText, 8, 62, "", 0),
	}}
}

type resourceSignal struct {
	status string
	reason string
	symbol string
	color  string
}

func summaryScene(value reading, config Config) protocol.Scene {
	return buildResourceScene(value, config, resourceSignal{
		status: "MAC OK",
		reason: "ALL CLEAR",
		symbol: "+",
		color:  colorHealthy,
	})
}

func pressureScene(value reading, config Config, state pressureState) protocol.Scene {
	status, symbol, color := "MAC WARN", "!", colorWarning
	if state.level == pressureCritical {
		status, symbol, color = "MAC CRIT", "!!", colorCritical
	}
	return buildResourceScene(value, config, resourceSignal{
		status: status,
		reason: pressureSource(state.reason),
		symbol: symbol,
		color:  color,
	})
}

func buildResourceScene(value reading, config Config, signal resourceSignal) protocol.Scene {
	networkRate := value.RXBytesPerSecond + value.TXBytesPerSecond
	networkPercent := clampPercent(networkRate / config.NetworkCapacityBytesPerSecond * 100)
	elements := []protocol.Element{
		resourceRectangle("front-background", "front", 0, 0, 72, 16, colorCanvas),
		resourceRectangle("back-background", "back", 0, 0, 160, 80, colorCanvas),
	}
	markers := frontResourceMarkers(value, networkPercent, signal, config)
	frontRows := []struct {
		key, label, text string
		percent          float64
		y                int
	}{
		{"cpu", "CPU", fmt.Sprintf("%.0f%%", value.CPUPercent), value.CPUPercent, 1},
		{"mem", "MEM", fmt.Sprintf("%.0f%%", value.MemoryPercent), value.MemoryPercent, 6},
		{"net", "NET", formatRate(networkRate, ""), networkPercent, 11},
	}
	for _, row := range frontRows {
		elements = append(elements, frontMetricElements("front-"+row.key, row.label, row.text, markers[row.key], row.percent, row.y, config, signal.color)...)
	}
	elements = append(elements,
		resourceText("back-status", "back", signal.status, "normal", signal.color, 74, 1, "top_mid", 146),
		resourceText("back-symbol", "back", signal.symbol, "tiny", signal.color, 1, 12, "", 0),
		resourceText("back-reason", "back", signal.reason, "tiny", colorText, 16, 12, "", 0),
	)
	backRows := []struct {
		key, label, text string
		percent          float64
		y                int
	}{
		{"cpu", "CPU", fmt.Sprintf("%.0f%%", value.CPUPercent), value.CPUPercent, 20},
		{"mem", "MEM", fmt.Sprintf("%.0f%%", value.MemoryPercent), value.MemoryPercent, 34},
		{"rx", "RX", formatRate(value.RXBytesPerSecond, "/s"), clampPercent(value.RXBytesPerSecond / config.NetworkCapacityBytesPerSecond * 100), 48},
		{"tx", "TX", formatRate(value.TXBytesPerSecond, "/s"), clampPercent(value.TXBytesPerSecond / config.NetworkCapacityBytesPerSecond * 100), 62},
	}
	for _, row := range backRows {
		elements = append(elements, metricElements("back-"+row.key, "back", row.label, row.text, row.percent, row.y, 1, 29, 92, 146, 5, config)...)
	}
	return protocol.Scene{Elements: elements}
}

func frontResourceMarkers(value reading, networkPercent float64, signal resourceSignal, config Config) map[string]string {
	markers := map[string]string{"cpu": "", "mem": "", "net": ""}
	if signal.symbol != "!" && signal.symbol != "!!" {
		return markers
	}
	switch signal.reason {
	case "CPU":
		markers["cpu"] = signal.symbol
	case "MEMORY":
		markers["mem"] = signal.symbol
	case "NETWORK":
		markers["net"] = signal.symbol
	case "MULTIPLE":
		values := map[string]float64{"cpu": value.CPUPercent, "mem": value.MemoryPercent, "net": networkPercent}
		threshold := config.WarningPercent
		if signal.symbol == "!!" {
			threshold = config.CriticalPercent
		}
		for key, percent := range values {
			if percent >= threshold {
				markers[key] = signal.symbol
			}
		}
		if markers["cpu"] == "" && markers["mem"] == "" && markers["net"] == "" {
			key := "cpu"
			if values["mem"] > values[key] {
				key = "mem"
			}
			if values["net"] > values[key] {
				key = "net"
			}
			markers[key] = signal.symbol
		}
	}
	return markers
}

func frontMetricElements(prefix, label, value, marker string, percent float64, y int, config Config, signalColor string) []protocol.Element {
	elements := metricElements(prefix, "front", label, value, percent, y, 1, 21, 28, 70, 4, config)
	return append(elements, resourceText(prefix+"-marker", "front", marker, "tiny", signalColor, 17, y, "", 0))
}

func pressureSource(reason string) string {
	switch reason {
	case "cpu_pressure":
		return "CPU"
	case "memory_pressure":
		return "MEMORY"
	case "network_pressure":
		return "NETWORK"
	case "multiple_pressure":
		return "MULTIPLE"
	default:
		return "RESOURCE"
	}
}

func metricElements(prefix, display, label, value string, percent float64, y, labelX, barX, barWidth, valueX, barHeight int, config Config) []protocol.Element {
	color := pressureColor(percent, config)
	innerWidth := barWidth - 2
	innerHeight := barHeight - 2
	fillWidth := int(math.Round(float64(innerWidth) * clampPercent(percent) / 100))
	fillColor := color
	if fillWidth == 0 {
		fillWidth = 1
		fillColor = colorTrack
	}
	result := []protocol.Element{
		resourceText(prefix+"-label", display, label, "tiny", colorText, labelX, y, "", 0),
		resourceRectangle(prefix+"-border", display, barX, y, barWidth, barHeight, colorBorder),
		resourceRectangle(prefix+"-track", display, barX+1, y+1, innerWidth, innerHeight, colorTrack),
		resourceRectangle(prefix+"-fill", display, barX+1, y+1, fillWidth, innerHeight, fillColor),
	}
	result = append(result, resourceText(prefix+"-value", display, value, "tiny", color, valueX, y, "top_right", 0))
	return result
}

func resourceText(id, display, value, font, color string, x, y int, align string, width int) protocol.Element {
	return protocol.Element{ID: id, Display: protocol.Display(display), X: x, Y: y, Text: &protocol.TextElement{Value: value, Font: font, Color: color, Align: align, Width: width}}
}

func resourceRectangle(id, display string, x, y, width, height int, color string) protocol.Element {
	return protocol.Element{ID: id, Display: protocol.Display(display), X: x, Y: y, Rectangle: &protocol.RectangleElement{Width: width, Height: height, Color: color}}
}

func pressureColor(percent float64, config Config) string {
	if percent >= config.CriticalPercent {
		return colorCritical
	}
	if percent >= config.WarningPercent {
		return colorWarning
	}
	return colorHealthy
}

func clampPercent(value float64) float64 {
	return min(100, max(0, value))
}

func formatRate(rate float64, suffix string) string {
	for _, unit := range []struct {
		name      string
		threshold float64
	}{{"G", 1024 * 1024 * 1024}, {"M", 1024 * 1024}, {"K", 1024}} {
		if rate >= unit.threshold {
			value := rate / unit.threshold
			if value < 10 {
				return fmt.Sprintf("%.1f%s%s", value, unit.name, suffix)
			}
			return fmt.Sprintf("%.0f%s%s", value, unit.name, suffix)
		}
	}
	return fmt.Sprintf("%.0fB%s", rate, suffix)
}
