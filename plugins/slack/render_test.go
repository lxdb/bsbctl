package slack

import (
	"encoding/json"
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
	scene := detailScene(config{}, workerSnapshot{Fresh: true}, activity{Kind: "channel", Mention: true, Alias: "BUILD"}, 0, fixtureNow)
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
func TestSceneDefaultsPrivacyBoundsAndExplicitStaleness(t *testing.T) {
	_, w, _ := panelFixture(t)
	s := w.snapshot()
	a := s.Items[0]
	a.Preview = "private-body-canary"
	s.Items[0] = a
	for _, phase := range []string{"ready", "unconfigured", "degraded", "auth_required"} {
		s.Phase = phase
		s.Fresh = phase == "ready"
		scenes := []protocol.Scene{summaryScene(w.cfg, s), detailScene(w.cfg, s, a, 0, fixtureNow)}
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
	a.Preview = "FIRST\n" + strings.Repeat("private", 30) + "LAST"
	var pages []string
	for page := range 4 {
		scene := detailScene(w.cfg, s, a, page, fixtureNow)
		assertSceneBounds(t, scene)
		var rear strings.Builder
		for _, e := range scene.Elements {
			if e.Text == nil {
				continue
			}
			if e.Display == protocol.DisplayFront && strings.Contains(e.Text.Value, "private") {
				t.Fatal("private preview on front")
			}
			if e.Display == protocol.DisplayBack {
				rear.WriteString(e.Text.Value)
			}
		}
		pages = append(pages, rear.String())
	}
	if pages[0] == pages[1] || strings.Contains(strings.Join(pages, ""), "LAST") || strings.Contains(strings.Join(pages, ""), "\n") {
		t.Fatal("preview paging/bound broken")
	}
}
func TestNativeTargetRejectsInjectedProviderIdentifiers(t *testing.T) {
	for _, a := range []activity{{ChannelID: "D123&evil=x", MessageTS: "1.000001"}, {ChannelID: "D123", MessageTS: "1.000001&evil=x"}, {ChannelID: "https://evil.invalid", MessageTS: "1.000001"}} {
		if _, err := nativeTarget("T123", a); err == nil {
			t.Fatal("unsafe target accepted")
		}
	}
}
