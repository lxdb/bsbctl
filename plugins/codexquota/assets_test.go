package codexquota

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/png"
	"os"
	"testing"
)

func TestStaticCodexMarkIsTheOnlyPackagedAsset(t *testing.T) {
	t.Parallel()
	declarations := AssetDeclarations()
	if len(declarations) != 1 {
		t.Fatalf("asset declarations = %d, want 1", len(declarations))
	}
	declaration := declarations[0]
	if declaration.Source != "assets/codex-mark.png" ||
		declaration.MediaType != "image/png" {
		t.Fatalf("asset declaration = %#v", declaration)
	}
	file, err := os.Open(declaration.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	configuration, err := png.DecodeConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Width != 14 || configuration.Height != 14 {
		t.Fatalf("static mark dimensions = %dx%d, want 14x14", configuration.Width, configuration.Height)
	}
	content, err := os.ReadFile(declaration.Source)
	if err != nil {
		t.Fatal(err)
	}
	mark, err := png.Decode(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	visible := image.Rectangle{}
	for y := mark.Bounds().Min.Y; y < mark.Bounds().Max.Y; y++ {
		for x := mark.Bounds().Min.X; x < mark.Bounds().Max.X; x++ {
			_, _, _, alpha := mark.At(x, y).RGBA()
			if alpha != 0 {
				visible = visible.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	if visible != mark.Bounds() {
		t.Fatalf("static mark visible bounds = %v, want full %v device footprint", visible, mark.Bounds())
	}
	digest := sha256.Sum256(content)
	if declaration.SHA256 != hex.EncodeToString(digest[:]) || declaration.Size != int64(len(content)) {
		t.Fatalf("asset integrity = sha256:%s size:%d, declaration=%#v", hex.EncodeToString(digest[:]), len(content), declaration)
	}
	entries, err := os.ReadDir("assets")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "codex-mark.png" || !entries[0].Type().IsRegular() {
		t.Fatalf("packaged assets = %#v, want only codex-mark.png", entries)
	}
}
