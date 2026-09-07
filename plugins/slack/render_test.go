package slack

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func assertSceneBounds(t *testing.T, scene protocol.Scene) {
	t.Helper()
	if err := scene.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, e := range scene.Elements {
		width, height := 72, 16
		if e.Display == protocol.DisplayBack {
			width, height = 160, 80
		}
		if e.Text != nil {
			textWidth := len(e.Text.Value) * 4
			if e.Text.Width > 0 {
				textWidth = e.Text.Width
			}
			fontHeight := map[string]int{"tiny": 5, "small": 7, "normal": 9}[e.Text.Font]
			if fontHeight == 0 || e.X+textWidth > width || e.Y+fontHeight > height {
				t.Fatalf("text escapes canvas: %+v", e)
			}
		}
		if e.Rectangle != nil && (e.X+e.Rectangle.Width > width || e.Y+e.Rectangle.Height > height) {
			t.Fatal("rectangle escapes canvas")
		}
	}
}

func TestAttentionSceneUsesNotificationHierarchyAndSlackIdentity(t *testing.T) {
	scene := detailScene(config{}, workerSnapshot{Fresh: true}, activity{Kind: "channel", Mention: true, Alias: "BUILD"}, panelOpen, fixtureNow)
	elements := make(map[string]protocol.Element, len(scene.Elements))
	for _, element := range scene.Elements {
		elements[element.ID] = element
	}
	headline := elements["front-label"]
	if headline.X != 18 || headline.Y != 0 || headline.Text == nil || headline.Text.Value != "Mentioned" || headline.Text.Font != "normal" || headline.Text.Width != 54 || headline.Text.Marquee == nil || headline.Text.Color != slackWarning {
		t.Fatalf("front headline = %+v", headline)
	}
	context := elements["front-context"]
	if context.X != 18 || context.Y != 9 || context.Text == nil || context.Text.Font != "tiny" || context.Text.Width != 54 || context.Text.Marquee == nil || context.Text.Color != slackSecondary {
		t.Fatalf("front context = %+v", context)
	}
	icon := elements["front-icon"]
	if icon.X != 0 || icon.Y != 0 || icon.Image == nil || icon.Image.Asset.PackagePath != "assets/slack-mark.png" {
		t.Fatalf("front icon = %+v", icon)
	}
	background := elements["front-background"]
	if background.Rectangle == nil || background.Rectangle.Width != 72 || background.Rectangle.Height != 16 || background.Rectangle.Color != slackCanvas {
		t.Fatalf("front background = %+v", background)
	}
	back := elements["back-line-0"]
	if back.X != 4 || back.Y != 4 || back.Text == nil || back.Text.Font != "small" || back.Text.Width != 152 {
		t.Fatalf("back headline = %+v", back)
	}
}

func TestListSceneShowsDetailsWithoutAdvertisingAnExternalAction(t *testing.T) {
	scene := listScene(config{}, workerSnapshot{Fresh: true}, activity{Kind: "dm"}, fixtureNow)
	assertSceneBounds(t, scene)
	var rear []string
	for _, element := range scene.Elements {
		if element.Text == nil {
			continue
		}
		if strings.Contains(element.Text.Value, "OPEN IN SLACK") || strings.Contains(element.Text.Value, "TURN ACTION") {
			t.Errorf("list advertises a detail action: %q", element.Text.Value)
		}
		if element.Display == protocol.DisplayBack {
			rear = append(rear, element.Text.Value)
		}
	}
	text := strings.Join(rear, "\n")
	if strings.Count(text, "PLAY: DETAILS") != 1 || !strings.Contains(text, "TURN SELECT / BACK CLOSE") {
		t.Fatalf("list guidance = %q", text)
	}
}

func TestSummaryUsesFullWorkspaceLabelWithMarquee(t *testing.T) {
	scene := summaryScene(config{label: "Engineering Workspace"}, workerSnapshot{Phase: "ready", Fresh: true})
	for _, element := range scene.Elements {
		if element.ID != "front-context" {
			continue
		}
		if element.Text == nil || element.Text.Value != "Engineering Workspace" || element.Text.Marquee == nil {
			t.Fatalf("workspace context = %+v", element)
		}
		return
	}
	t.Fatal("workspace context is missing")
}

func TestConnectedCoverageGapDoesNotMasqueradeAsReconnect(t *testing.T) {
	s := workerSnapshot{Phase: "ready", Fresh: true, Gap: true, Items: []activity{{Kind: "channel"}}}
	scene := summaryScene(config{label: "Slack"}, s)
	raw, err := json.Marshal(scene)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "1 channels") || !strings.Contains(text, "Earlier activity may be incomp") || strings.Contains(text, "reconnecting") {
		t.Fatalf("connected gap scene = %s", raw)
	}
}
func TestSceneDefaultsPrivacyBoundsAndExplicitStaleness(t *testing.T) {
	_, w, _ := panelFixture(t)
	s := w.snapshot()
	a := s.Items[0]
	a.Preview = "private-body-canary"
	s.Items[0] = a
	for _, phase := range []string{"ready", "unconfigured", "degraded", "auth_required"} {
		s.Phase = phase
		s.Fresh = phase == "ready"
		scenes := []protocol.Scene{summaryScene(w.cfg, s), detailScene(w.cfg, s, a, panelOpen, fixtureNow)}
		for _, scene := range scenes {
			assertSceneBounds(t, scene)
			raw, _ := json.Marshal(scene)
			if strings.Contains(string(raw), "private-body-canary") {
				t.Fatal("default private preview")
			}
			if !s.Fresh && phase != "unconfigured" && !strings.Contains(string(raw), "Slack activity may be incomplete") && !strings.Contains(string(raw), "Slack access expired") {
				t.Fatal("stale scene has no visible status")
			}
		}
	}
}
func TestOptionalPreviewIsBoundedPagedAndRearOnly(t *testing.T) {
	_, w, _ := panelFixture(t)
	w.cfg.rearDetails = true
	s := w.snapshot()
	a := s.Items[0]
	a.Preview = strings.Repeat("A", 48) + strings.Repeat("B", 48) + strings.Repeat("C", 48) + "FINAL\nPAGE123456" + "CLIPPED"
	for page, body := range []string{strings.Repeat("A", 48), strings.Repeat("B", 48), strings.Repeat("C", 48), "FINAL PAGE123456"} {
		scene := readerScene(s, &panelSession{target: a, page: page}, fixtureNow)
		assertSceneBounds(t, scene)
		var rows []string
		position, controls := "", ""
		for _, e := range scene.Elements {
			if e.Text == nil {
				continue
			}
			if e.Display == protocol.DisplayFront && (strings.Contains(e.Text.Value, "AAAA") || strings.Contains(e.Text.Value, "BBBB") || strings.Contains(e.Text.Value, "CCCC") || strings.Contains(e.Text.Value, "FINAL")) {
				t.Fatalf("message body appeared on front: %q", e.Text.Value)
			}
			if e.Display == protocol.DisplayBack {
				switch e.ID {
				case "back-line-1", "back-line-2":
					rows = append(rows, e.Text.Value)
				case "back-line-3":
					position = e.Text.Value
				case "back-line-4":
					controls = e.Text.Value
				}
			}
		}
		if got := strings.Join(rows, ""); got != body {
			t.Errorf("page %d body = %q, want %q", page+1, got, body)
		}
		if position != fmt.Sprintf("PAGE %d/4 / TURN SCROLL", page+1) || controls != "BACK ACTIONS" {
			t.Errorf("page %d controls = %q / %q", page+1, position, controls)
		}
	}
}
func TestNativeTargetRejectsInjectedProviderIdentifiers(t *testing.T) {
	for _, a := range []activity{{ChannelID: "D123&evil=x", MessageTS: "1.000001"}, {ChannelID: "D123", MessageTS: "1.000001&evil=x"}, {ChannelID: "https://evil.invalid", MessageTS: "1.000001"}} {
		if _, err := nativeTarget("T123", a); err == nil {
			t.Fatal("unsafe target accepted")
		}
	}
}
