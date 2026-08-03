package watermark

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The same fixture is asserted by web/src/lib/video/mark.test.ts. If these two
// ever disagree, a leaked frame cannot be located from the browser side, so the
// fixture is regenerated (web/scripts/gen-mark-vectors.ts) only when the
// geometry is deliberately changed.
//
// Hashes and tile sizes compare exactly. Drift and opacity are floats: sin() is
// not bit-identical across runtimes, and the determinism contract is geometry,
// not float bits. 1e-9 of a tile pitch is far below a pixel.
const epsilon = 1e-9

type markVectors struct {
	Constants struct {
		TileBaseWidth   int     `json:"tileBaseWidth"`
		TileBaseHeight  int     `json:"tileBaseHeight"`
		RotationRadians float64 `json:"rotationRadians"`
		DriftPeriodMs   float64 `json:"driftPeriodMs"`
	} `json:"constants"`
	SeedHashes []struct {
		Seed string `json:"seed"`
		Hash uint32 `json:"hash"`
	} `json:"seedHashes"`
	TileSizes []struct {
		Scale  float64 `json:"scale"`
		DPR    float64 `json:"dpr"`
		Width  int     `json:"width"`
		Height int     `json:"height"`
	} `json:"tileSizes"`
	Drift []struct {
		Seed string  `json:"seed"`
		TMs  int64   `json:"tMs"`
		DX   float64 `json:"dx"`
		DY   float64 `json:"dy"`
	} `json:"drift"`
	Opacity []struct {
		Seed  string  `json:"seed"`
		TMs   int64   `json:"tMs"`
		Scale float64 `json:"scale"`
	} `json:"opacity"`
}

func loadVectors(t *testing.T) markVectors {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "mark_vectors.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var v markVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(v.SeedHashes) == 0 || len(v.Drift) == 0 {
		t.Fatalf("%s has no vectors", path)
	}
	return v
}

func TestSharedConstants(t *testing.T) {
	v := loadVectors(t)
	if v.Constants.TileBaseWidth != TileBaseWidth || v.Constants.TileBaseHeight != TileBaseHeight {
		t.Errorf("tile base = %dx%d, want %dx%d",
			TileBaseWidth, TileBaseHeight, v.Constants.TileBaseWidth, v.Constants.TileBaseHeight)
	}
	if v.Constants.RotationRadians != RotationRadians {
		t.Errorf("rotation = %v, want %v", RotationRadians, v.Constants.RotationRadians)
	}
	if v.Constants.DriftPeriodMs != DriftPeriodMs {
		t.Errorf("drift period = %v, want %v", float64(DriftPeriodMs), v.Constants.DriftPeriodMs)
	}
}

func TestSeedHashMatchesVectors(t *testing.T) {
	for _, c := range loadVectors(t).SeedHashes {
		if got := SeedHash(c.Seed); got != c.Hash {
			t.Errorf("SeedHash(%q) = %d, want %d", c.Seed, got, c.Hash)
		}
	}
}

func TestTileSizeMatchesVectors(t *testing.T) {
	for _, c := range loadVectors(t).TileSizes {
		w, h := TileSize(Spec{Scale: c.Scale}, c.DPR)
		if w != c.Width || h != c.Height {
			t.Errorf("TileSize(scale=%v, dpr=%v) = %dx%d, want %dx%d",
				c.Scale, c.DPR, w, h, c.Width, c.Height)
		}
	}
}

func TestDriftOffsetMatchesVectors(t *testing.T) {
	for _, c := range loadVectors(t).Drift {
		got := DriftOffset(c.Seed, c.TMs)
		if math.Abs(got.DX-c.DX) > epsilon || math.Abs(got.DY-c.DY) > epsilon {
			t.Errorf("DriftOffset(%q, %d) = (%v, %v), want (%v, %v)",
				c.Seed, c.TMs, got.DX, got.DY, c.DX, c.DY)
		}
	}
}

func TestOpacityScaleMatchesVectors(t *testing.T) {
	for _, c := range loadVectors(t).Opacity {
		if got := OpacityScale(c.Seed, c.TMs); math.Abs(got-c.Scale) > epsilon {
			t.Errorf("OpacityScale(%q, %d) = %v, want %v", c.Seed, c.TMs, got, c.Scale)
		}
	}
}

func TestDriftStaysWithinOneTilePitch(t *testing.T) {
	for tMs := int64(0); tMs < DriftPeriodMs*3; tMs += 137 {
		o := DriftOffset("session-0001", tMs)
		if math.Abs(o.DX) > 1+epsilon || math.Abs(o.DY) > 1+epsilon {
			t.Fatalf("drift at %d = (%v, %v), outside one tile pitch", tMs, o.DX, o.DY)
		}
	}
}

// Amplitude below one pitch would leave a band the mark never covers, which is
// exactly what temporal averaging looks for.
func TestDriftCoversAFullPitch(t *testing.T) {
	minDX, maxDX := math.Inf(1), math.Inf(-1)
	for tMs := int64(0); tMs < DriftPeriodMs*3; tMs += 97 {
		dx := DriftOffset("session-0001", tMs).DX
		minDX = math.Min(minDX, dx)
		maxDX = math.Max(maxDX, dx)
	}
	if maxDX-minDX < 1.9 {
		t.Errorf("drift range = %v, want at least ~2 tile pitches", maxDX-minDX)
	}
}

func TestDriftRepeatsAfterThreePeriods(t *testing.T) {
	const at = int64(1_000)
	base := DriftOffset("session-0001", at)
	after := DriftOffset("session-0001", at+DriftPeriodMs*3)
	if math.Abs(base.DX-after.DX) > epsilon || math.Abs(base.DY-after.DY) > epsilon {
		t.Errorf("path did not close after three periods: %v vs %v", base, after)
	}
	// The y component is what breaks the single-period repeat.
	oneLater := DriftOffset("session-0001", at+DriftPeriodMs)
	if math.Abs(base.DY-oneLater.DY) < 1e-3 {
		t.Error("drift repeats every period; temporal averaging would separate it")
	}
}

// Real callers pass server epoch milliseconds. Reducing before the sine is what
// keeps Go and the browser agreeing there.
func TestDriftSurvivesEpochTimestamps(t *testing.T) {
	const now = int64(1_762_000_000_000)
	base := DriftOffset("session-0001", now)
	if math.IsNaN(base.DX) || math.IsNaN(base.DY) {
		t.Fatal("drift is NaN at epoch-scale time")
	}
	later := DriftOffset("session-0001", now+DriftPeriodMs*3)
	if math.Abs(base.DX-later.DX) > epsilon {
		t.Errorf("epoch-scale drift did not repeat: %v vs %v", base.DX, later.DX)
	}
}

func TestOpacityStaysSubtle(t *testing.T) {
	for tMs := int64(0); tMs < DriftPeriodMs; tMs += 53 {
		s := OpacityScale("session-0001", tMs)
		if s < 0.8-epsilon || s > 1+epsilon {
			t.Fatalf("opacity scale at %d = %v, outside [0.8, 1]", tMs, s)
		}
	}
}

// Room config allows 0.25-3.0 and the compositor caps dpr at 2. A spec outside
// those must not produce a tile the browser cannot reproduce.
func TestTileSizeClampsToConfigBounds(t *testing.T) {
	cases := []struct {
		name             string
		scale, dpr       float64
		wantScale, wantD float64
	}{
		{"scale above max", 99, 1, 3, 1},
		{"scale below min", 0, 1, 0.25, 1},
		{"scale NaN", math.NaN(), 1, 0.25, 1},
		{"dpr above cap", 1, 8, 1, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotW, gotH := TileSize(Spec{Scale: c.scale}, c.dpr)
			wantW, wantH := TileSize(Spec{Scale: c.wantScale}, c.wantD)
			if gotW != wantW || gotH != wantH {
				t.Errorf("TileSize = %dx%d, want %dx%d", gotW, gotH, wantW, wantH)
			}
		})
	}
}
