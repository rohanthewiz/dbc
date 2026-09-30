package raster

import (
	"image"
	"image/color"
	"strings"
	"testing"
)

func testFaces(t *testing.T, k float64) *Faces {
	t.Helper()
	fs, err := Fonts()
	if err != nil {
		t.Fatal(err)
	}
	f := NewFaces(fs, k)
	t.Cleanup(f.Close)
	return f
}

// A width measured at 1× is the same width at 2× (unhinted faces), which is
// what lets a layout be measured once and drawn at any scale.
func TestWidthIsScaleFree(t *testing.T) {
	st := Style{Font: Regular, Size: 12}
	a, b := testFaces(t, 1).Width(st, "customer_id"), testFaces(t, 2).Width(st, "customer_id")
	if a <= 0 || a-b > 0.01 || b-a > 0.01 {
		t.Errorf("1× %.3f, 2× %.3f", a, b)
	}
}

func TestFitAndWrap(t *testing.T) {
	f := testFaces(t, 1)
	st := Style{Font: Mono, Size: 11}
	if got := f.Fit(st, "short", 500); got != "short" {
		t.Errorf("Fit changed a string that fits: %q", got)
	}
	got := f.Fit(st, strings.Repeat("x", 200), 80)
	if !strings.HasSuffix(got, "…") || f.Width(st, got) > 80 {
		t.Errorf("Fit = %q (%.1f px)", got, f.Width(st, got))
	}
	lines := f.Wrap(st, "one two three four five six\n"+strings.Repeat("y", 60), 60)
	for _, l := range lines {
		if f.Width(st, l) > 60 {
			t.Errorf("wrapped line %q is %.1f px", l, f.Width(st, l))
		}
	}
	if len(lines) < 4 {
		t.Errorf("Wrap = %q", lines)
	}
}

// A polyline's U-turn (a self-reference's loop) is filled solid: every
// segment's quad is wound the same way, so overlaps clamp, not cancel. And
// a shape partly off the picture is clipped, not dropped or out of bounds.
func TestPolylineAndClip(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 60, 60))
	p := &Painter{Img: img, K: 1, Faces: testFaces(t, 1)}
	white := color.RGBA{255, 255, 255, 255}
	pts := CubicPts(Pt{X: 10, Y: 10}, Pt{X: 50, Y: 10}, Pt{X: 50, Y: 40}, Pt{X: 10, Y: 40})
	p.Polyline(pts, 3, white)
	// the curve's rightmost point: t=0.5 is at x = 10·¼ + 50·¾ = 40
	if c := img.RGBAAt(40, 25); c.R < 200 {
		t.Errorf("the loop's turn is not filled: %v", c)
	}
	p.RoundRect(50, 50, 30, 30, 4, white) // runs off the bottom right
	if c := img.RGBAAt(55, 55); c.R < 200 {
		t.Errorf("a clipped shape was not drawn: %v", c)
	}
}

func TestMixAndRGB(t *testing.T) {
	a, b := RGB("#ffffff"), RGB("#000000")
	if a != (color.RGBA{255, 255, 255, 255}) || Mix(a, b, 0) != b || Mix(a, b, 1) != a {
		t.Errorf("RGB/Mix ends: %v %v", a, Mix(a, b, 0))
	}
	if m := Mix(a, b, 0.5); m.R != 128 {
		t.Errorf("Mix half = %v", m)
	}
}
