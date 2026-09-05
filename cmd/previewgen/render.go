//go:build preview

package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"image/png"
	"math"
)

const (
	frontWidth  = 72
	frontHeight = 16
	ledCellSize = 10
)

var (
	frontScreenBounds = image.Rect(24, 61, 24+frontWidth*ledCellSize, 61+frontHeight*ledCellSize)
	previewPalette    = transparentPlan9Palette()
)

func transparentPlan9Palette() color.Palette {
	result := make(color.Palette, 0, len(palette.Plan9))
	result = append(result, color.RGBA{})
	result = append(result, palette.Plan9[:16]...)
	return append(result, palette.Plan9[17:]...)
}

//go:embed assets/busybar-device.png
var deviceFramePNG []byte

type timedFrame struct {
	Image *image.RGBA
	Delay int
}

func encodeGIF(source []timedFrame) ([]byte, error) {
	coalesced, err := coalesceFrames(source)
	if err != nil {
		return nil, err
	}
	animation := &gif.GIF{
		LoopCount: 0,
		Config:    image.Config{ColorModel: previewPalette, Width: 768, Height: 248},
	}
	var previous *image.RGBA
	for _, current := range coalesced {
		rendered, err := renderDeviceFrame(current.Image)
		if err != nil {
			return nil, err
		}
		bounds := rendered.Bounds()
		if previous != nil {
			bounds = changedBounds(previous, rendered)
			if bounds.Empty() {
				animation.Delay[len(animation.Delay)-1] += current.Delay
				continue
			}
		}
		animation.Image = append(animation.Image, palettedFrame(rendered, bounds))
		animation.Delay = append(animation.Delay, current.Delay)
		animation.Disposal = append(animation.Disposal, gif.DisposalNone)
		previous = rendered
	}
	var output bytes.Buffer
	if err := gif.EncodeAll(&output, animation); err != nil {
		return nil, fmt.Errorf("encode GIF: %w", err)
	}
	return output.Bytes(), nil
}

func coalesceFrames(source []timedFrame) ([]timedFrame, error) {
	if len(source) == 0 {
		return nil, errors.New("preview contains no frames")
	}
	result := make([]timedFrame, 0, len(source))
	for _, current := range source {
		if current.Image == nil || current.Image.Bounds() != image.Rect(0, 0, frontWidth, frontHeight) || current.Delay <= 0 {
			return nil, errors.New("preview contains an invalid frame")
		}
		last := len(result) - 1
		if last >= 0 && bytes.Equal(result[last].Image.Pix, current.Image.Pix) {
			result[last].Delay += current.Delay
			continue
		}
		result = append(result, current)
	}
	return result, nil
}

func changedBounds(previous, current *image.RGBA) image.Rectangle {
	result := image.Rectangle{}
	for y := current.Bounds().Min.Y; y < current.Bounds().Max.Y; y++ {
		for x := current.Bounds().Min.X; x < current.Bounds().Max.X; x++ {
			if current.RGBAAt(x, y) == previous.RGBAAt(x, y) {
				continue
			}
			point := image.Rect(x, y, x+1, y+1)
			if result.Empty() {
				result = point
			} else {
				result = result.Union(point)
			}
		}
	}
	return result
}

func palettedFrame(source *image.RGBA, bounds image.Rectangle) *image.Paletted {
	result := image.NewPaletted(bounds, previewPalette)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			result.Set(x, y, source.At(x, y))
		}
	}
	return result
}

func encodePNG(source *image.RGBA) ([]byte, error) {
	rendered, err := renderDeviceFrame(source)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := png.Encode(&output, rendered); err != nil {
		return nil, fmt.Errorf("encode PNG: %w", err)
	}
	return output.Bytes(), nil
}

func renderDeviceFrame(source *image.RGBA) (*image.RGBA, error) {
	if source == nil || source.Bounds() != image.Rect(0, 0, frontWidth, frontHeight) {
		return nil, errors.New("preview contains an invalid frame")
	}
	device, err := png.Decode(bytes.NewReader(deviceFramePNG))
	if err != nil {
		return nil, errors.New("decode BUSY Bar preview frame")
	}
	if device.Bounds() != image.Rect(0, 0, 768, 248) {
		return nil, errors.New("BUSY Bar preview frame has unexpected dimensions")
	}
	result := image.NewRGBA(device.Bounds())
	draw.Draw(result, result.Bounds(), device, device.Bounds().Min, draw.Over)
	for y := range frontScreenBounds.Dy() {
		for x := range frontScreenBounds.Dx() {
			sourceColor := source.RGBAAt(x/ledCellSize, y/ledCellSize)
			alpha, brightness := ledMask(x%ledCellSize, y%ledCellSize)
			intensity := (float64(sourceColor.R) + float64(sourceColor.G) + float64(sourceColor.B)) / (3 * 255)
			if alpha == 0 || intensity < 0.04 {
				continue
			}
			targetX, targetY := frontScreenBounds.Min.X+x, frontScreenBounds.Min.Y+y
			foreground := color.RGBA{
				R: uint8(float64(sourceColor.R)*brightness + 0.5),
				G: uint8(float64(sourceColor.G)*brightness + 0.5),
				B: uint8(float64(sourceColor.B)*brightness + 0.5),
				A: uint8(alpha*255 + 0.5),
			}
			result.SetRGBA(targetX, targetY, over(foreground, result.RGBAAt(targetX, targetY)))
		}
	}
	return result, nil
}

func ledMask(x, y int) (alpha, brightness float64) {
	u := (float64(x) + 0.5) / ledCellSize
	v := (float64(y) + 0.5) / ledCellSize
	halfSize := 0.85 * 0.5
	radius := math.Sqrt(0.5) * halfSize
	qx := math.Abs(u-0.5) - (halfSize - radius)
	qy := math.Abs(v-0.5) - (halfSize - radius)
	distance := math.Hypot(max(qx, 0), max(qy, 0)) + min(max(qx, qy), 0) - radius
	delta := float64(frontHeight) / float64(frontScreenBounds.Dy()) * 1.5
	alpha = 1 - smoothstep(-delta, delta, distance)
	vignette := smoothstep(0.7, 0.3, math.Hypot(u-0.5, v-0.5))
	brightness = 1 - 0.15*(1-vignette)
	if alpha < 0.001 {
		alpha = 0
	}
	return alpha, brightness
}

func smoothstep(edge0, edge1, value float64) float64 {
	value = min(1, max(0, (value-edge0)/(edge1-edge0)))
	return value * value * (3 - 2*value)
}

func over(source, destination color.RGBA) color.RGBA {
	alpha := float64(source.A) / 255
	return color.RGBA{
		R: uint8(float64(source.R)*alpha + float64(destination.R)*(1-alpha) + 0.5),
		G: uint8(float64(source.G)*alpha + float64(destination.G)*(1-alpha) + 0.5),
		B: uint8(float64(source.B)*alpha + float64(destination.B)*(1-alpha) + 0.5),
		A: 255,
	}
}
