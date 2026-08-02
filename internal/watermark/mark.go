// Package watermark holds the mark engine's geometry: one definition, mirrored
// by every renderer.
//
// The display compositor, the loupe, the frame-grab path and the admin preview
// must all produce the same mark from the same inputs, or forensic attribution
// falls apart the moment two of them disagree. web/src/lib/video/mark.ts is the
// browser side of this; both assert testdata/mark_vectors.json so they cannot
// drift apart silently.
//
// The determinism contract is geometry: tile pitch, rotation, drift and opacity
// modulation. It is deliberately not glyph rasterization, which will never match
// between a browser and gofont, and does not need to: forensics locates the mark
// by geometry and reads it by eye. The rasterizer arrives here when the frame
// grab stamp (files.go) is re-based onto this package.
package watermark

import "math"

const (
	// TileBaseWidth and TileBaseHeight are the tile box in CSS pixels at scale
	// 1. Fixed, never measured from text: text metrics vary by platform and font
	// fallback, so a measured pitch would be irreproducible across languages.
	TileBaseWidth  = 360
	TileBaseHeight = 168

	// RotationDegrees tilts the tiling so a crop along either axis cannot land
	// between rows.
	RotationDegrees = -30

	// DriftPeriodMs is long enough not to distract during a review, short enough
	// that temporal averaging over a shot never sees a constant layer.
	DriftPeriodMs = 45_000

	// Room config bounds, mirrored from the admin UI.
	minScale = 0.25
	maxScale = 3
	// Backing-store scale ceiling, matching the compositor's min(dpr, 2).
	maxDPR = 2
)

// RotationRadians is RotationDegrees in radians. A var, not a const: Go folds
// constant expressions in arbitrary precision, which lands one ulp away from
// the float64 arithmetic the browser does. Rounding each step here keeps the
// two bit-identical, which is what lets the shared vectors compare it exactly.
var RotationRadians = float64(RotationDegrees) * math.Pi / 180

// Spec is the mark's content and appearance. Mirrors MarkSpec in mark.ts.
type Spec struct {
	// Token is server-signed and opaque; it is rendered verbatim.
	Token string
	// Lines are name, participant short-ID, room, UTC clock and the
	// server-expanded watermarkText template. There is no email:
	// models.Participant has none.
	Lines []string
	// Seed is the session ID; it drives drift deterministically.
	Seed string
	// Opacity and Scale come from room config.
	Opacity float64
	Scale   float64
}

// Offset is a drift offset in tile pitches.
type Offset struct {
	DX float64
	DY float64
}

// SeedHash is 32-bit FNV-1a over the seed's UTF-8 bytes. Go strings are already
// UTF-8; the JS side encodes rather than walking UTF-16 code units so the two
// agree on non-ASCII seeds.
func SeedHash(seed string) uint32 {
	var hash uint32 = 0x811c9dc5
	for i := 0; i < len(seed); i++ {
		hash ^= uint32(seed[i])
		hash *= 0x01000193
	}
	return hash
}

// TileSize is the tile box in device pixels: integer, and a function of scale
// and dpr only.
func TileSize(spec Spec, dpr float64) (width, height int) {
	s := clamp(spec.Scale, minScale, maxScale) * clamp(dpr, 0.5, maxDPR)
	return int(math.Round(TileBaseWidth * s)), int(math.Round(TileBaseHeight * s))
}

// DriftOffset is the drift at server time t, in tile pitches: |DX|, |DY| reach
// 1, so the mark travels a full tile and never sits at a fixed pixel.
// Deterministic from the seed, which is what makes a leaked still forensically
// locatable — the same session at the same server millisecond produces the same
// offset here and in the browser.
func DriftOffset(seed string, serverTMs int64) Offset {
	hash := SeedHash(seed)
	phaseX := float64(hash&0xffff) / 0x10000 * 2 * math.Pi
	phaseY := float64((hash>>16)&0xffff) / 0x10000 * 2 * math.Pi
	// Reduce against the full path length before the sine. Server time is epoch
	// milliseconds, and sin() of ~1e8 radians is where two runtimes' argument
	// reduction stops agreeing; fmod is exact, so both sides feed sin the same
	// small angle. 3:2 Lissajous, so the path only closes after three periods
	// and consecutive periods do not retrace the same pixels.
	turns := modPositive(float64(serverTMs), DriftPeriodMs*3) / DriftPeriodMs * 2 * math.Pi
	return Offset{
		DX: math.Sin(turns + phaseX),
		DY: math.Sin(turns*2/3 + phaseY),
	}
}

// OpacityScale is the opacity multiplier at server time t, in [0.8, 1]. Slight
// modulation on a different period from the drift, so the mark's contribution is
// never constant frame to frame.
func OpacityScale(seed string, serverTMs int64) float64 {
	hash := SeedHash(seed)
	phase := float64((hash>>8)&0xffff) / 0x10000 * 2 * math.Pi
	const period = DriftPeriodMs * 0.5
	turns := modPositive(float64(serverTMs), period) / period * 2 * math.Pi
	return 0.9 + 0.1*math.Sin(turns+phase)
}

// modPositive is a non-negative remainder, so a negative timestamp still lands
// on the path.
func modPositive(value, modulus float64) float64 {
	return math.Mod(math.Mod(value, modulus)+modulus, modulus)
}

func clamp(v, lo, hi float64) float64 {
	if !(v > lo) { // also catches NaN
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
