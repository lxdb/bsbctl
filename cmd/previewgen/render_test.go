//go:build preview

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"
)

func TestOfficialDeviceFrameMatchesReviewedFirmwareAsset(t *testing.T) {
	t.Parallel()
	digest := sha256.Sum256(deviceFramePNG)
	if got, want := hex.EncodeToString(digest[:]), "7534fce2f998dd884890a92b1bd31b504884988d13612e06b95219c686788efb"; got != want {
		t.Fatalf("BUSY Bar frame SHA-256 = %s, want %s", got, want)
	}
}

func TestEncodeGIFPreservesExactPixelsAndTiming(t *testing.T) {
	t.Parallel()
	red := solidFrame(color.RGBA{R: 255, A: 255})
	green := solidFrame(color.RGBA{G: 255, A: 255})
	encoded, err := encodeGIF([]timedFrame{
		{Image: red, Delay: 10},
		{Image: red, Delay: 10},
		{Image: green, Delay: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := gif.DecodeAll(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Config.Width != 768 || decoded.Config.Height != 248 {
		t.Fatalf("GIF dimensions = %dx%d, want 768x248", decoded.Config.Width, decoded.Config.Height)
	}
	if len(decoded.Image) != 2 || decoded.Delay[0] != 20 || decoded.Delay[1] != 10 {
		t.Fatalf("GIF frames/delays = %d/%v, want 2/[20 10]", len(decoded.Image), decoded.Delay)
	}
	if decoded.LoopCount != 0 {
		t.Fatalf("GIF loop count = %d, want infinite looping", decoded.LoopCount)
	}
	for index, disposal := range decoded.Disposal {
		if disposal != gif.DisposalNone {
			t.Fatalf("GIF frame %d disposal = %d, want DisposalNone", index, disposal)
		}
	}
	first := compositeGIFFrame(decoded, 0)
	second := compositeGIFFrame(decoded, 1)
	ledCenter := image.Pt(frontScreenBounds.Min.X+5, frontScreenBounds.Min.Y+5)
	if got := color.RGBAModel.Convert(first.At(ledCenter.X, ledCenter.Y)).(color.RGBA); got.R <= got.G {
		t.Fatalf("first LED center = %#v, want red-dominant", got)
	}
	if got := color.RGBAModel.Convert(second.At(ledCenter.X, ledCenter.Y)).(color.RGBA); got.G <= got.R {
		t.Fatalf("second LED center = %#v, want green-dominant", got)
	}
}

func TestEncodeRawGIFPreservesOpaqueFramebuffersAndTiming(t *testing.T) {
	t.Parallel()
	red := solidFrame(color.RGBA{R: 255, A: 255})
	green := solidFrame(color.RGBA{G: 255, A: 255})
	encoded, err := encodeRawGIF([]timedFrame{
		{Image: red, Delay: 25},
		{Image: green, Delay: 25},
	})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := decodeMockGIF(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || frames[0].Delay != 25 || frames[1].Delay != 25 {
		t.Fatalf("raw frames/delays = %d/%d/%d, want 2/25/25", len(frames), frames[0].Delay, frames[1].Delay)
	}
	for frameIndex, frame := range frames {
		for y := range frontHeight {
			for x := range frontWidth {
				if alpha := frame.Image.RGBAAt(x, y).A; alpha != 0xff {
					t.Fatalf("raw frame %d pixel (%d,%d) alpha = %d, want opaque", frameIndex, x, y, alpha)
				}
			}
		}
	}
	if got := frames[0].Image.RGBAAt(0, 0); got.R <= got.G {
		t.Fatalf("first raw pixel = %#v, want red-dominant", got)
	}
	if got := frames[1].Image.RGBAAt(0, 0); got.G <= got.R {
		t.Fatalf("second raw pixel = %#v, want green-dominant", got)
	}
}

func TestDeviceRendererUsesRoundedLEDsInsideTheOfficialFrame(t *testing.T) {
	t.Parallel()
	black := solidFrame(color.RGBA{A: 255})
	white := solidFrame(color.RGBA{A: 255})
	white.SetRGBA(0, 0, color.RGBA{R: 255, G: 255, B: 255, A: 255})

	withoutLED, err := renderDeviceFrame(black)
	if err != nil {
		t.Fatal(err)
	}
	withLED, err := renderDeviceFrame(white)
	if err != nil {
		t.Fatal(err)
	}
	if got := withLED.Bounds(); got != image.Rect(0, 0, 768, 248) {
		t.Fatalf("device frame bounds = %v, want 768x248", got)
	}
	corner := frontScreenBounds.Min
	center := corner.Add(image.Pt(5, 5))
	if withLED.RGBAAt(corner.X, corner.Y) != withoutLED.RGBAAt(corner.X, corner.Y) {
		t.Fatal("LED cell corner was filled; want rounded separation")
	}
	if withLED.RGBAAt(center.X, center.Y) == withoutLED.RGBAAt(center.X, center.Y) {
		t.Fatal("LED cell center did not render")
	}
}

func TestEncodePNGPlacesTheFramebufferInsideTheDeviceFrame(t *testing.T) {
	t.Parallel()
	frame := solidFrame(color.RGBA{B: 255, A: 255})
	frame.SetRGBA(0, 0, color.RGBA{R: 255, A: 255})
	encoded, err := encodePNG(frame)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds() != image.Rect(0, 0, 768, 248) {
		t.Fatalf("PNG bounds = %v, want 768x248", decoded.Bounds())
	}
	ledCenter := image.Pt(frontScreenBounds.Min.X+5, frontScreenBounds.Min.Y+5)
	if got := color.RGBAModel.Convert(decoded.At(ledCenter.X, ledCenter.Y)).(color.RGBA); got.R <= got.B {
		t.Fatalf("rendered LED center = %#v, want red-dominant", got)
	}
}

func TestPreviewArtifactsPreserveTransparencyOutsideTheDeviceFrame(t *testing.T) {
	t.Parallel()
	frame := solidFrame(color.RGBA{A: 255})

	pngData, err := encodePNG(frame)
	if err != nil {
		t.Fatal(err)
	}
	pngImage, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		t.Fatal(err)
	}
	if got := color.RGBAModel.Convert(pngImage.At(0, 0)).(color.RGBA); got.A != 0 {
		t.Fatalf("PNG canvas corner alpha = %d, want transparent", got.A)
	}

	gifData, err := encodeGIF([]timedFrame{{Image: frame, Delay: 10}})
	if err != nil {
		t.Fatal(err)
	}
	gifImage, err := gif.DecodeAll(bytes.NewReader(gifData))
	if err != nil {
		t.Fatal(err)
	}
	if got := color.RGBAModel.Convert(gifImage.Image[0].At(0, 0)).(color.RGBA); got.A != 0 {
		t.Fatalf("GIF canvas corner alpha = %d, want transparent", got.A)
	}
}

func compositeGIFFrame(animation *gif.GIF, through int) *image.RGBA {
	result := image.NewRGBA(image.Rect(0, 0, animation.Config.Width, animation.Config.Height))
	for index := 0; index <= through; index++ {
		frame := animation.Image[index]
		for y := frame.Bounds().Min.Y; y < frame.Bounds().Max.Y; y++ {
			for x := frame.Bounds().Min.X; x < frame.Bounds().Max.X; x++ {
				result.Set(x, y, frame.At(x, y))
			}
		}
	}
	return result
}

func solidFrame(value color.RGBA) *image.RGBA {
	frame := image.NewRGBA(image.Rect(0, 0, 72, 16))
	for y := range 16 {
		for x := range 72 {
			frame.SetRGBA(x, y, value)
		}
	}
	return frame
}
