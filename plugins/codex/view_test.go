package codex

import (
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestCardSceneUsesBothDisplaysAndKeepsStateTextVisible(t *testing.T) {
	t.Parallel()
	card := Card{StateWord: "WAIT CMD", ContextLine: "Safe title", DetailLine: "Command approval"}
	scene := cardScene(card)
	assertCardScene(t, scene)
	assertCardText(t, scene, "front-state", "WAIT CMD")
	assertCardText(t, scene, "back-state", "WAIT CMD")
	assertCardText(t, scene, "back-context", "Safe title")
	assertCardText(t, scene, "back-detail", "Command approval")
	mark := cardElement(t, scene, "front-codex-mark")
	if mark.Image == nil || mark.Image.Asset.PackagePath != "assets/codex-mark.png" || mark.Image.Asset.StockName != "" || mark.Display != protocol.DisplayFront || mark.X != 1 || mark.Y != 1 {
		t.Fatalf("front Codex mark = %#v", mark)
	}
	state := cardElement(t, scene, "front-state")
	if state.X != 44 || state.Y != 8 || state.Text.Align != "center" {
		t.Fatalf("front state = %#v, want centered at 44,8", state)
	}
	assertCardRectangles(t, scene, "front-background", "back-background", "back-surface")
	assertCodexSafeMargins(t, scene)
}

func TestCardSceneRendersSeparateSessionAndProjectIdentityWithoutDeduplication(t *testing.T) {
	t.Parallel()
	scene := cardScene(Card{
		StateWord: "RUN", SessionLine: "Same", ProjectLine: "Same", DetailLine: "Active",
	})
	assertCardText(t, scene, "back-session", "Same")
	assertCardText(t, scene, "back-workdir", "Same")
}

func TestCardSceneMakesWorkdirTheReadableFrontMarqueeAndStateSecondary(t *testing.T) {
	t.Parallel()
	workdir := strings.TrimSpace(strings.Repeat("Long workdir ", 7))
	scene := cardScene(Card{StateWord: "RUN", SessionLine: "Session", ProjectLine: workdir, DetailLine: "Active"})
	front := cardElement(t, scene, "front-workdir")
	if front.Text.Value != workdir || front.Text.Font != "normal" || front.Text.Width == 0 || front.Text.Marquee == nil {
		t.Fatalf("front workdir marquee = %#v", front)
	}
	state := cardElement(t, scene, "front-state")
	if state.Text.Font != "tiny" || state.Y < 8 {
		t.Fatalf("secondary front state = %#v", state)
	}
	if front.X != 18 || front.Y != 0 || front.Text.Width != 53 || state.X != 18 || state.Y != 10 {
		t.Fatalf("safe centered front layout = workdir %#v, state %#v", front, state)
	}
	assertCodexSafeMargins(t, scene)
}

func TestCardSceneMakesBackWorkdirPrimaryAndSessionSecondary(t *testing.T) {
	t.Parallel()
	session := strings.Repeat("S", 80)
	project := strings.Repeat("P", 80)
	scene := cardScene(Card{StateWord: "RUN", SessionLine: session, ProjectLine: project, DetailLine: "Active"})
	assertCardText(t, scene, "back-session-label", "SESSION")
	assertCardText(t, scene, "back-workdir-label", "WORKDIR")
	for id, want := range map[string]string{"back-session": session, "back-workdir": project} {
		element := cardElement(t, scene, id)
		if element.Text.Value != want || element.Text.Width == 0 || element.Text.Marquee == nil {
			t.Fatalf("%s marquee = %#v", id, element)
		}
	}
	if got := cardElement(t, scene, "back-workdir").Text.Font; got != "normal" {
		t.Fatalf("back workdir font = %q, want normal", got)
	}
	if got := cardElement(t, scene, "back-session").Text.Font; got != "small" {
		t.Fatalf("back session font = %q, want small", got)
	}
}

func TestTypedQuestionSceneKeepsDescriptionElementTopologyStable(t *testing.T) {
	t.Parallel()
	card := Card{SessionLine: "Session", ProjectLine: "Project"}
	described := typedQuestionScene(card, "QUESTION 1/1", "Choose", "OPTION 1/2", requestOption{
		Label:       "Described",
		Description: "Details",
	})
	descriptionless := typedQuestionScene(card, "QUESTION 1/1", "Choose", "OPTION 2/2", requestOption{
		Label: "Descriptionless",
	})

	if len(descriptionless.Elements) != len(described.Elements) {
		t.Fatalf("descriptionless/described element counts = %d/%d, want identical topology", len(descriptionless.Elements), len(described.Elements))
	}
	for index := range described.Elements {
		got, want := descriptionless.Elements[index], described.Elements[index]
		if got.ID != want.ID || cardElementKind(got) != cardElementKind(want) {
			t.Fatalf("element %d descriptionless topology differs: %#v / %#v", index, got, want)
		}
	}

	got := cardElement(t, descriptionless, "back-option-description")
	want := cardElement(t, described, "back-option-description")
	if got.Text.Value != "" || got.X != want.X || got.Y != want.Y {
		t.Fatalf("descriptionless description element = %#v, want empty text at described position %#v", got, want)
	}
	assertCardRectangles(t, described, "front-background", "back-background")
	assertCodexSafeMargins(t, described)
	for id, geometry := range map[string][3]int{
		"front-option-label":      {18, 1, 53},
		"back-question":           {8, 18, 132},
		"back-option-label":       {8, 42, 132},
		"back-option-description": {8, 61, 0},
	} {
		element := cardElement(t, described, id)
		width := 0
		if element.Text != nil {
			width = element.Text.Width
		}
		if element.X != geometry[0] || element.Y != geometry[1] || width != geometry[2] {
			t.Fatalf("%s geometry = (%d,%d,%d), want %v", id, element.X, element.Y, width, geometry)
		}
	}
}

func TestTypedQuestionSceneShowsTheSelectedOptionOnTheFront(t *testing.T) {
	t.Parallel()
	scene := typedQuestionScene(
		Card{ProjectLine: "~/Documents/bsbctl"},
		"QUESTION 1/1",
		"Which previews should be refreshed?",
		"OPTION 1/2",
		requestOption{Label: "Codex and Calendar", Description: "Update both feature tours"},
	)
	option := cardElement(t, scene, "front-option-label")
	if option.Text.Value != "Codex and Calendar" || option.Text.Font != "normal" || option.Text.Width != 53 || option.Text.Marquee == nil {
		t.Fatalf("front selected option = %#v", option)
	}
	if got := cardElement(t, scene, "front-option-position"); got.Text.Value != "OPTION 1/2" || got.Text.Font != "tiny" {
		t.Fatalf("front option position = %#v", got)
	}
}

func TestCardSceneMapsStatusToSemanticAccents(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"CODEX ON":   signature,
		"RUN":        signature,
		"COMPACT":    information,
		"COMPACTED":  success,
		"PLAN 1/3":   information,
		"PLAN READY": information,
		"WAIT FILE":  warning,
		"ASK":        warning,
		"DONE":       success,
		"STOP":       counterpoint,
		"FAIL":       danger,
		"CODEX OFF":  danger,
	}
	for state, want := range tests {
		state, want := state, want
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			scene := cardScene(Card{StateWord: state, ContextLine: "Codex", DetailLine: "State"})
			if got := cardElement(t, scene, "front-state").Text.Color; got != want {
				t.Fatalf("state color = %q, want %q", got, want)
			}
		})
	}
}

func assertCardRectangles(t *testing.T, scene presentation.Scene, want ...string) {
	t.Helper()
	got := make([]string, 0, len(want))
	for _, element := range scene.Elements {
		if element.Rectangle != nil {
			got = append(got, element.ID)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rectangle elements = %v, want %v", got, want)
	}
}

func assertCodexSafeMargins(t *testing.T, scene presentation.Scene) {
	t.Helper()
	for _, element := range scene.Elements {
		if element.ID == "front-background" || element.ID == "back-background" {
			continue
		}
		maxX, maxY := 70, 14
		if element.Display == protocol.DisplayBack {
			maxX, maxY = 146, 78
		}
		minY := 1
		if element.ID == "front-workdir" && element.Display == protocol.DisplayFront {
			minY = 0
		}
		if element.X < 1 || element.X > maxX || element.Y < minY || element.Y > maxY {
			t.Fatalf("%s origin = (%d,%d), outside safe %s area", element.ID, element.X, element.Y, element.Display)
		}
		if element.Rectangle != nil && (element.X+element.Rectangle.Width-1 > maxX || element.Y+element.Rectangle.Height-1 > maxY) {
			t.Fatalf("%s rectangle reaches (%d,%d), outside safe %s area", element.ID, element.X+element.Rectangle.Width-1, element.Y+element.Rectangle.Height-1, element.Display)
		}
		if element.Text != nil && element.Text.Width > 0 && element.X+element.Text.Width-1 > maxX {
			t.Fatalf("%s text viewport reaches x=%d, outside safe %s area", element.ID, element.X+element.Text.Width-1, element.Display)
		}
		if element.Image != nil && element.ID == "front-codex-mark" && (element.X+13 > maxX || element.Y+13 > maxY) {
			t.Fatalf("%s image reaches (%d,%d), outside safe front area", element.ID, element.X+13, element.Y+13)
		}
	}
}

func assertCardScene(t *testing.T, scene presentation.Scene) {
	t.Helper()
	front, back := 0, 0
	ids := make(map[string]bool)
	for _, element := range scene.Elements {
		if ids[element.ID] {
			t.Fatalf("duplicate element %q", element.ID)
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
		t.Fatalf("front/back elements = %d/%d", front, back)
	}
	if err := (presentation.Candidate{
		PluginID: PluginID, InstanceID: "codex", Channel: ChannelAttention, Key: "request.test",
		Revision: 1, Generation: 1, AdmissionSequence: 1, Policy: presentation.PolicyAttention, Band: presentation.BandActionable, Impact: protocol.ImpactNotable,
		CreatedAt: time.Now(), UpdatedAt: time.Now(), Scene: scene,
	}).Validate(); err != nil {
		t.Fatalf("scene validation: %v", err)
	}
}

func assertCardText(t *testing.T, scene presentation.Scene, id, want string) {
	t.Helper()
	if got := cardElement(t, scene, id).Text.Value; got != want {
		t.Fatalf("%s text = %q, want %q", id, got, want)
	}
}

func cardElement(t *testing.T, value any, id string) presentation.Element {
	t.Helper()
	var scene presentation.Scene
	switch value := value.(type) {
	case presentation.Scene:
		scene = value
	case *protocol.Scene:
		if value != nil {
			scene = *value
		}
	default:
		t.Fatalf("unsupported scene type %T", value)
	}
	for _, element := range scene.Elements {
		if element.ID == id {
			return element
		}
	}
	t.Fatalf("missing element %q", id)
	return presentation.Element{}
}

func cardElementKind(element presentation.Element) string {
	switch {
	case element.Text != nil:
		return "text"
	case element.Image != nil:
		return "image"
	case element.Animation != nil:
		return "animation"
	case element.Rectangle != nil:
		return "rectangle"
	case element.Countdown != nil:
		return "countdown"
	default:
		return ""
	}
}
