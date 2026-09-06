package slack

import (
	"image"
	"image/png"
	"os"
	"testing"
)

func TestSlackMarkKeepsOnePixelMargin(t *testing.T) {
	file, err := os.Open("assets/slack-mark.png")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	mark, err := png.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	if mark.Bounds() != image.Rect(0, 0, 16, 16) {
		t.Fatalf("Slack mark bounds = %v, want 16x16", mark.Bounds())
	}
	for offset := 0; offset < 16; offset++ {
		for _, point := range []image.Point{{X: offset}, {X: offset, Y: 15}, {Y: offset}, {X: 15, Y: offset}} {
			if _, _, _, alpha := mark.At(point.X, point.Y).RGBA(); alpha != 0 {
				t.Fatalf("Slack mark margin pixel %v alpha = %d, want 0", point, alpha)
			}
		}
	}
	visible := false
	for y := 1; y < 15 && !visible; y++ {
		for x := 1; x < 15; x++ {
			if _, _, _, alpha := mark.At(x, y).RGBA(); alpha != 0 {
				visible = true
				break
			}
		}
	}
	if !visible {
		t.Fatal("Slack mark has no visible pixels inside its margin")
	}
}
