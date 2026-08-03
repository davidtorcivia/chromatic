package handlers

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/f64"
	"golang.org/x/image/math/fixed"
)

// Standalone tiled stamp for grabbed frames (watermarking plan, Phase 0).
//
// Deliberately independent of the Phase 1 mark engine: closing the frame-grab
// hole ships ahead of the compositor work and must not wait on it. When
// internal/watermark lands, this composite gets re-based onto it so the grab
// path becomes one of the shared renderers. A still has nothing temporal to
// reproduce, so there is no drift and no seed here.

const (
	// Rotation of the tiled text. Diagonal so a crop along either axis cannot
	// land between rows.
	stampRotationRad = -30 * math.Pi / 180
	// Font size as a fraction of image height, before the room's scale factor.
	// Legible on a 720p preview without dominating a 4K frame.
	stampFontHeightFraction = 0.018
	stampMinFontPx          = 9.0
	stampLineSpacing        = 1.35
	// Tile text column, in ems. Fixed so every pass shares one grid; lines
	// longer than this are ellipsized rather than widening the tile.
	stampTextWidthEms = 26.0
	// Re-encode quality; matches the client's grab quality so the round trip
	// does not add a visible generation loss.
	stampJPEGQuality = 92
)

// frameStampSpec is the content and appearance of one stamp pass.
type frameStampSpec struct {
	Lines   []string
	Opacity float64 // room watermark opacity (0-1)
	Scale   float64 // room watermark scale multiplier
	// Phase offsets this layer's tile grid by a fraction of a tile. A served
	// frame carries two passes — the capture stamp burned in at upload and the
	// download stamp composited on top — and both use the same rotation and
	// pitch. At the same phase they land on each other and neither is legible,
	// which destroys exactly the attribution the stamp exists for. Half a tile
	// interleaves them.
	Phase float64
}

// stampJPEG decodes a JPEG, composites the tiled stamp over it and re-encodes.
// Returns the encoded bytes.
func stampJPEG(src []byte, spec frameStampSpec) ([]byte, error) {
	img, err := jpeg.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}

	stamped, err := stampImage(img, spec)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, stamped, &jpeg.Options{Quality: stampJPEGQuality}); err != nil {
		return nil, fmt.Errorf("encode frame: %w", err)
	}
	return out.Bytes(), nil
}

// stampImage composites the tiled stamp over img and returns a new RGBA image.
// The source is left untouched.
func stampImage(img image.Image, spec frameStampSpec) (image.Image, error) {
	lines := nonEmptyLines(spec.Lines)
	bounds := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), img, bounds.Min, draw.Src)
	if len(lines) == 0 {
		return dst, nil
	}

	opacity := clampFloat(spec.Opacity, 0.05, 1.0)
	scale := clampFloat(spec.Scale, 0.25, 3.0)

	fontPx := float64(bounds.Dy()) * stampFontHeightFraction * scale
	if fontPx < stampMinFontPx {
		fontPx = stampMinFontPx
	}

	tile, err := renderStampTile(lines, fontPx, opacity)
	if err != nil {
		return nil, err
	}

	tileW := float64(tile.Bounds().Dx())
	tileH := float64(tile.Bounds().Dy())
	sin, cos := math.Sin(stampRotationRad), math.Cos(stampRotationRad)
	cx, cy := float64(dst.Bounds().Dx())/2, float64(dst.Bounds().Dy())/2
	// Tiles are laid out in an unrotated pattern space centred on the image and
	// each is rotated into place, rather than rotating one image-sized layer:
	// the layer would need a diagonal-sized buffer (~77 MB for a 4K frame) for
	// the same result.
	radius := math.Hypot(cx, cy)
	cols := int(math.Ceil(radius/tileW)) + 1
	rows := int(math.Ceil(radius/tileH)) + 1

	for row := -rows - 1; row <= rows+1; row++ {
		for col := -cols - 1; col <= cols+1; col++ {
			ox := (float64(col) + spec.Phase) * tileW
			oy := (float64(row) + spec.Phase) * tileH
			m := f64.Aff3{
				cos, -sin, cos*ox - sin*oy + cx,
				sin, cos, sin*ox + cos*oy + cy,
			}
			draw.ApproxBiLinear.Transform(dst, m, tile, tile.Bounds(), draw.Over, nil)
		}
	}

	return dst, nil
}

// renderStampTile rasterizes one text block. Each line is drawn twice, a dark
// copy offset down-right under a light copy, so the mark modulates luminance in
// both directions: no single-direction levels or gamma move removes both.
func renderStampTile(lines []string, fontPx, opacity float64) (*image.RGBA, error) {
	// The font is parsed per call rather than cached: sfnt.Font is not safe for
	// concurrent use, and two viewers downloading the same grab at once would
	// share it. Parsing is table indexing, cheap next to the JPEG round trip.
	parsed, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return nil, fmt.Errorf("parse stamp font: %w", err)
	}
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{
		Size:    fontPx,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, fmt.Errorf("stamp font face: %w", err)
	}
	defer face.Close()

	lineHeight := fontPx * stampLineSpacing

	// The tile box is fixed from the font size, never from the measured text.
	// Two passes land on one frame (capture at upload, download at serve); if
	// each sized its own grid to its own longest line the two grids would drift
	// against each other and collide unpredictably. A fixed box plus a phase
	// offset makes them interleave everywhere.
	padX := fontPx * 2.5
	padY := fontPx * 1.75
	textWidth := fontPx * stampTextWidthEms
	tileW := int(math.Ceil(textWidth + padX*2))
	tileH := int(math.Ceil(lineHeight*float64(len(lines)) + padY*2))
	tile := image.NewRGBA(image.Rect(0, 0, tileW, tileH))

	alpha := uint8(math.Round(clampFloat(opacity, 0, 1) * 255))
	shadowAlpha := uint8(math.Round(float64(alpha) * 0.8))
	shadowOffset := math.Max(1, math.Round(fontPx/12))

	light := image.NewUniform(color.NRGBA{R: 255, G: 255, B: 255, A: alpha})
	dark := image.NewUniform(color.NRGBA{R: 0, G: 0, B: 0, A: shadowAlpha})

	drawer := font.Drawer{Dst: tile, Face: face}
	for i, line := range lines {
		line = ellipsize(face, line, textWidth)
		baseline := padY + lineHeight*float64(i) + fontPx
		for _, pass := range []struct {
			src    image.Image
			dx, dy float64
		}{
			{dark, shadowOffset, shadowOffset},
			{light, 0, 0},
		} {
			drawer.Src = pass.src
			drawer.Dot = fixed.Point26_6{
				X: fixed.Int26_6(math.Round((padX + pass.dx) * 64)),
				Y: fixed.Int26_6(math.Round((baseline + pass.dy) * 64)),
			}
			drawer.DrawString(line)
		}
	}

	return tile, nil
}

// ellipsize trims a line to fit the fixed tile box. Measuring is fine here —
// it only decides where to cut the text, never the tile pitch.
func ellipsize(face font.Face, text string, maxWidth float64) string {
	width := func(s string) float64 { return float64(font.MeasureString(face, s)) / 64 }
	if width(text) <= maxWidth {
		return text
	}
	runes := []rune(text)
	for len(runes) > 0 {
		runes = runes[:len(runes)-1]
		if width(string(runes)+"…") <= maxWidth {
			return string(runes) + "…"
		}
	}
	return ""
}

func nonEmptyLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func clampFloat(v, lo, hi float64) float64 {
	if !(v > lo) { // also catches NaN
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
