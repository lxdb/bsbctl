package macresources

import (
	"reflect"
	"testing"

	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestSummarySceneKeepsIndependentFrontResourceBarsAndNumericReadings(t *testing.T) {
	t.Parallel()

	config, _ := decodeConfig([]byte(`{}`))
	scene := summaryScene(reading{CPUPercent: 42, MemoryPercent: 68, RXBytesPerSecond: 1024 * 1024, TXBytesPerSecond: 200 * 1024}, config)
	if len(scene.Elements) == 0 || len(scene.Elements) > 64 {
		t.Fatalf("element count = %d", len(scene.Elements))
	}
	front, back := 0, 0
	ids := map[string]bool{}
	for _, element := range scene.Elements {
		if ids[element.ID] {
			t.Fatalf("duplicate element ID %q", element.ID)
		}
		ids[element.ID] = true
		switch element.Display {
		case "front":
			front++
		case "back":
			back++
		default:
			t.Fatalf("element %q display = %q", element.ID, element.Display)
		}
	}
	if front == 0 || back == 0 {
		t.Fatalf("front/back counts = %d/%d", front, back)
	}
	if err := (presentation.Candidate{
		PluginID: PluginID, InstanceID: AppID, Channel: ChannelSummary, Key: observationKey,
		Revision: 1, Generation: 1, AdmissionSequence: 1, Policy: presentation.PolicyRotation, Band: presentation.BandRotation, Impact: protocol.ImpactNormal,
		Scene: scene,
	}).Validate(); err != nil {
		t.Fatalf("scene is not presentation-valid: %v", err)
	}
	for id, want := range map[string]string{
		"front-cpu-label": "CPU", "front-cpu-value": "42%",
		"front-mem-label": "MEM", "front-mem-value": "68%",
		"front-net-label": "NET", "front-net-value": "1.2M",
	} {
		assertText(t, scene, id, want)
	}
	for _, resource := range []string{"cpu", "mem", "net"} {
		assertText(t, scene, "front-"+resource+"-marker", "")
		if got := elementByID(t, scene, "front-"+resource+"-fill"); got.Rectangle == nil || got.Rectangle.Width < 1 {
			t.Fatalf("%s front fill = %#v", resource, got)
		}
	}
	assertText(t, scene, "back-status", "MAC OK")
	assertText(t, scene, "back-reason", "ALL CLEAR")
	assertText(t, scene, "back-cpu-value", "42%")
	assertText(t, scene, "back-mem-value", "68%")
	assertText(t, scene, "back-rx-value", "1.0M/s")
	assertText(t, scene, "back-tx-value", "200K/s")
}

func TestMetricColorsFollowConfiguredSensitivityIndependently(t *testing.T) {
	t.Parallel()
	// The same sample crosses different boundaries when sensitivity changes.
	value := reading{CPUPercent: 60, MemoryPercent: 80, RXBytesPerSecond: 500000, TXBytesPerSecond: 200000}
	for _, test := range []struct {
		name   string
		config string
		colors map[string]string
	}{
		{
			name:   "default thresholds",
			config: `{"network_capacity_bytes_per_second":1000000}`,
			colors: map[string]string{
				"front-cpu": "#35D07FFF", "back-cpu": "#35D07FFF",
				"front-mem": "#F2B84BFF", "back-mem": "#F2B84BFF",
				"front-net": "#F2B84BFF", "back-rx": "#35D07FFF", "back-tx": "#35D07FFF",
			},
		},
		{
			name:   "more sensitive thresholds",
			config: `{"warning_percent":40,"critical_percent":60,"network_capacity_bytes_per_second":1000000}`,
			colors: map[string]string{
				"front-cpu": "#FF786FFF", "back-cpu": "#FF786FFF",
				"front-mem": "#FF786FFF", "back-mem": "#FF786FFF",
				"front-net": "#FF786FFF", "back-rx": "#F2B84BFF", "back-tx": "#35D07FFF",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := decodeConfig([]byte(test.config))
			if err != nil {
				t.Fatal(err)
			}
			scene := summaryScene(value, config)
			for prefix, color := range test.colors {
				assertColor(t, scene, prefix+"-value", color)
				assertColor(t, scene, prefix+"-fill", color)
			}
		})
	}
}

func TestPressureSceneCommunicatesLevelAndSourceWithoutColorOnly(t *testing.T) {
	t.Parallel()
	config, _ := decodeConfig([]byte(`{}`))
	tests := []struct {
		name    string
		value   reading
		state   pressureState
		status  string
		reason  string
		markers map[string]string
		color   string
	}{
		{name: "warning cpu", value: reading{CPUPercent: 80}, state: pressureState{level: pressureWarning, reason: "cpu_pressure"}, status: "MAC WARN", reason: "CPU", markers: map[string]string{"cpu": "!"}, color: colorWarning},
		{name: "critical memory", value: reading{MemoryPercent: 95}, state: pressureState{level: pressureCritical, reason: "memory_pressure"}, status: "MAC CRIT", reason: "MEMORY", markers: map[string]string{"mem": "!!"}, color: colorCritical},
		{name: "critical network", value: reading{RXBytesPerSecond: config.NetworkCapacityBytesPerSecond * .5, TXBytesPerSecond: config.NetworkCapacityBytesPerSecond * .45}, state: pressureState{level: pressureCritical, reason: "network_pressure"}, status: "MAC CRIT", reason: "NETWORK", markers: map[string]string{"net": "!!"}, color: colorCritical},
		{name: "critical multiple", value: reading{CPUPercent: 95, MemoryPercent: 91}, state: pressureState{level: pressureCritical, reason: "multiple_pressure"}, status: "MAC CRIT", reason: "MULTIPLE", markers: map[string]string{"cpu": "!!", "mem": "!!"}, color: colorCritical},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scene := pressureScene(test.value, config, test.state)
			assertText(t, scene, "back-status", test.status)
			assertText(t, scene, "back-reason", test.reason)
			wantSymbol := "!"
			if test.state.level == pressureCritical {
				wantSymbol = "!!"
			}
			assertText(t, scene, "back-symbol", wantSymbol)
			for _, resource := range []string{"cpu", "mem", "net"} {
				assertText(t, scene, "front-"+resource+"-marker", test.markers[resource])
				if test.markers[resource] != "" {
					assertColor(t, scene, "front-"+resource+"-marker", test.color)
				}
			}
		})
	}
}

func TestMultiplePressureMarksHighestResourceWhenHysteresisRetainsStateBelowEntryThreshold(t *testing.T) {
	t.Parallel()
	config, _ := decodeConfig([]byte(`{}`))
	scene := pressureScene(reading{CPUPercent: 70, MemoryPercent: 74}, config, pressureState{level: pressureCritical, reason: "multiple_pressure"})
	assertText(t, scene, "front-cpu-marker", "")
	assertText(t, scene, "front-mem-marker", "!!")
	assertText(t, scene, "front-net-marker", "")
}

func TestFrontPressureMarkerKeepsVisibleGapBetweenLabelAndBar(t *testing.T) {
	t.Parallel()
	config, _ := decodeConfig([]byte(`{}`))
	tests := []struct {
		name     string
		resource string
		scene    presentation.Scene
	}{
		{name: "warning", resource: "cpu", scene: pressureScene(reading{CPUPercent: 80}, config, pressureState{level: pressureWarning, reason: "cpu_pressure"})},
		{name: "critical", resource: "mem", scene: pressureScene(reading{MemoryPercent: 95}, config, pressureState{level: pressureCritical, reason: "memory_pressure"})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			label := elementByID(t, test.scene, "front-"+test.resource+"-label")
			marker := elementByID(t, test.scene, "front-"+test.resource+"-marker")
			bar := elementByID(t, test.scene, "front-"+test.resource+"-border")
			if label.X != 1 || marker.X != 17 || bar.X != 21 {
				t.Fatalf("horizontal layout = label %d, marker %d, bar %d; want 1, 17, 21", label.X, marker.X, bar.X)
			}
		})
	}
}

func TestResourceScenesKeepEveryElementInsideSafeMargins(t *testing.T) {
	t.Parallel()
	config, _ := decodeConfig([]byte(`{}`))
	for _, scene := range []presentation.Scene{
		summaryScene(reading{CPUPercent: 42, MemoryPercent: 68}, config),
		pressureScene(reading{MemoryPercent: 95}, config, pressureState{level: pressureCritical, reason: "memory_pressure"}),
	} {
		for _, element := range scene.Elements {
			if element.ID == "front-background" || element.ID == "back-background" {
				wantWidth, wantHeight := 72, 16
				if element.Display == protocol.DisplayBack {
					wantWidth, wantHeight = 160, 80
				}
				if element.X != 0 || element.Y != 0 || element.Rectangle == nil || element.Rectangle.Width != wantWidth || element.Rectangle.Height != wantHeight || element.Rectangle.Color != "#071522FF" {
					t.Fatalf("background = %#v, want full Trace canvas", element)
				}
				continue
			}
			maxX, maxY := 70, 14
			if element.Display == "back" {
				maxX, maxY = 146, 78
			}
			if element.X < 1 || element.X > maxX || element.Y < 1 || element.Y > maxY {
				t.Fatalf("%s origin = (%d,%d), outside safe %s area", element.ID, element.X, element.Y, element.Display)
			}
			if element.Rectangle != nil && (element.X+element.Rectangle.Width-1 > maxX || element.Y+element.Rectangle.Height-1 > maxY) {
				t.Fatalf("%s rectangle reaches (%d,%d), outside safe %s area", element.ID, element.X+element.Rectangle.Width-1, element.Y+element.Rectangle.Height-1, element.Display)
			}
		}
	}
	for id, geometry := range map[string][4]int{
		"front-cpu-border": {21, 1, 28, 4},
		"front-mem-border": {21, 6, 28, 4},
		"front-net-border": {21, 11, 28, 4},
		"back-status":      {74, 1, 146, 0},
		"back-tx-value":    {146, 62, 0, 0},
	} {
		element := elementByID(t, summaryScene(reading{}, config), id)
		width, height := 0, 0
		if element.Rectangle != nil {
			width, height = element.Rectangle.Width, element.Rectangle.Height
		} else if element.Text != nil {
			width = element.Text.Width
		}
		if got := [4]int{element.X, element.Y, width, height}; got != geometry {
			t.Fatalf("%s geometry = %v, want %v", id, got, geometry)
		}
	}
}

func TestResourceScenesKeepStableTopologyAcrossNormalAndPressureStates(t *testing.T) {
	t.Parallel()
	config, _ := decodeConfig([]byte(`{}`))
	normal := summaryScene(reading{}, config)
	scenes := []presentation.Scene{
		pressureScene(reading{CPUPercent: 80}, config, pressureState{level: pressureWarning, reason: "cpu_pressure"}),
		pressureScene(reading{MemoryPercent: 95}, config, pressureState{level: pressureCritical, reason: "memory_pressure"}),
		pressureScene(reading{CPUPercent: 95, MemoryPercent: 95}, config, pressureState{level: pressureCritical, reason: "multiple_pressure"}),
		summaryScene(reading{CPUPercent: 100, MemoryPercent: 100, RXBytesPerSecond: config.NetworkCapacityBytesPerSecond, TXBytesPerSecond: config.NetworkCapacityBytesPerSecond}, config),
	}
	for index, scene := range scenes {
		if got, want := resourceTopology(scene), resourceTopology(normal); !reflect.DeepEqual(got, want) {
			t.Fatalf("scene %d topology changed:\ngot=%v\nwant=%v", index, got, want)
		}
	}
}

func TestMacLiveStatusScenesKeepStableTopologyAndUseTraceStatusColors(t *testing.T) {
	waiting := resourceStatusScene("WAITING", resourceWaitingColor)
	unavailable := resourceStatusScene("UNAVAILABLE", resourceUnavailableColor)
	if err := waiting.Validate(); err != nil {
		t.Fatalf("waiting status scene is invalid: %v", err)
	}
	if err := unavailable.Validate(); err != nil {
		t.Fatalf("unavailable status scene is invalid: %v", err)
	}
	if got, want := resourceTopology(waiting), resourceTopology(unavailable); !reflect.DeepEqual(got, want) {
		t.Fatalf("status topology changed: waiting=%v unavailable=%v", got, want)
	}
	for _, test := range []struct {
		scene presentation.Scene
		text  string
		color string
	}{
		{scene: waiting, text: "WAITING", color: "#F2B84BFF"},
		{scene: unavailable, text: "UNAVAILABLE", color: "#FF786FFF"},
	} {
		frontStatus := elementByID(t, test.scene, "front-status")
		if frontStatus.Text == nil || frontStatus.Text.Value != test.text || frontStatus.Text.Color != test.color {
			t.Fatalf("front status = %#v, want %q in %q", frontStatus.Text, test.text, test.color)
		}
		backTitle := elementByID(t, test.scene, "back-title")
		if backTitle.Text == nil || backTitle.Text.Color != "#EAF4F2FF" {
			t.Fatalf("back title = %#v, want Trace primary text", backTitle.Text)
		}
		backAccent := elementByID(t, test.scene, "back-accent")
		backBackground := elementByID(t, test.scene, "back-background")
		if backBackground.Rectangle == nil || backBackground.Rectangle.Color != "#071522FF" {
			t.Fatalf("back background = %#v, want Trace canvas", backBackground.Rectangle)
		}
		if backAccent.Rectangle == nil || backAccent.Rectangle.Color != test.color {
			t.Fatalf("back accent = %#v, want %q", backAccent.Rectangle, test.color)
		}
	}
}

func resourceTopology(scene presentation.Scene) []string {
	result := make([]string, 0, len(scene.Elements))
	for _, element := range scene.Elements {
		kind := ""
		switch {
		case element.Text != nil:
			kind = "text"
		case element.Image != nil:
			kind = "image"
		case element.Animation != nil:
			kind = "animation"
		case element.Rectangle != nil:
			kind = "rectangle"
		case element.Countdown != nil:
			kind = "countdown"
		}
		result = append(result, string(element.Display)+":"+element.ID+":"+kind)
	}
	return result
}

func assertText(t *testing.T, raw any, id, want string) {
	t.Helper()
	scene := sceneValue(t, raw)
	element := elementByID(t, scene, id)
	if element.Text == nil || element.Text.Value != want {
		var got string
		if element.Text != nil {
			got = element.Text.Value
		}
		t.Fatalf("%s text = %q, want %q", id, got, want)
	}
}

func assertColor(t *testing.T, raw any, id, want string) {
	t.Helper()
	scene := sceneValue(t, raw)
	element := elementByID(t, scene, id)
	var got string
	if element.Text != nil {
		got = element.Text.Color
	} else if element.Rectangle != nil {
		got = element.Rectangle.Color
	}
	if got != want {
		t.Fatalf("%s color = %q, want %q", id, got, want)
	}
}

func elementByID(t *testing.T, raw any, id string) presentation.Element {
	t.Helper()
	scene := sceneValue(t, raw)
	for _, element := range scene.Elements {
		if element.ID == id {
			return element
		}
	}
	t.Fatalf("missing element %q", id)
	return presentation.Element{}
}

func sceneValue(t *testing.T, raw any) presentation.Scene {
	t.Helper()
	switch scene := raw.(type) {
	case presentation.Scene:
		return scene
	case *presentation.Scene:
		if scene != nil {
			return *scene
		}
	}
	t.Fatalf("unsupported scene value %T", raw)
	return presentation.Scene{}
}
