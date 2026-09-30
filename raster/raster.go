// Package raster is the drawing kit dbc's pictures are painted with: the Go
// fonts, text measured and fitted in CSS pixels, and anti-aliased filled
// shapes and strokes — all pure Go, on golang.org/x/image's vector
// rasterizer.
//
//	explain (a plan's picture) ──┐
//	                             ├──► raster: Fonts · Faces · Painter
//	erd (a schema's diagram) ────┘            Style · Pt · Mix · RGB
//
// Each picture keeps its own layout, text styles and palette; what they
// share is only how a shape or a string gets onto the pixels. That part
// was written once for the plan and copied for the diagram (about 150
// lines); it lives here so a fix to either (the rasterizer's bounding box,
// a stroke's joints) reaches both, and a third picture starts from it.
//
// UNITS. Every coordinate a caller passes is in CSS pixels. A Painter draws
// them at K device pixels per CSS pixel, and a Faces measures text at its
// own K; layouts measure with a Faces at 1× (unhinted advances scale
// linearly), so a layout does not depend on the scale it is drawn at.
//
// WHY NOT A GENERAL 2D LIBRARY. gg, canvas and friends bring cgo, a
// different font stack, or a much larger API than rounded rectangles,
// polylines and one-line text; x/image is already a dependency and pure Go,
// so the shell, the TUI and dbc web render with no C toolchain.
package raster

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/theme"
)

// ---------------------------------------------------------------------------
// Fonts and text
// ---------------------------------------------------------------------------

// The Go fonts: a humanist sans for prose, its bold for names, and the
// monospace for SQL and types. They are embedded in golang.org/x/image, so
// a picture looks the same on every machine and needs no system font.
const (
	Regular = iota
	Bold
	Mono
)

// Fonts are the three Go fonts, parsed once per process. A parsed font is
// read-only and safe to share; the faces made from it (Faces) are not.
var Fonts = sync.OnceValues(func() ([3]*opentype.Font, error) {
	var out [3]*opentype.Font
	for i, ttf := range [][]byte{goregular.TTF, gobold.TTF, gomono.TTF} {
		f, err := opentype.Parse(ttf)
		if err != nil {
			return out, serr.Wrap(err, "op", "parse the go fonts")
		}
		out[i] = f
	}
	return out, nil
})

// Style is a text style: which font (Regular, Bold, Mono), at what
// CSS-pixel size.
type Style struct {
	Font int
	Size float64
}

// LineH is the style's line height: CSS's 1.45, the web UI's own, so a
// picture sets text the way the page it mirrors does.
func (s Style) LineH() float64 { return s.Size * 1.45 }

// Faces caches one font.Face per style at one scale. A face is not safe for
// concurrent use, so every render makes its own and closes it.
type Faces struct {
	fonts [3]*opentype.Font
	k     float64
	m     map[Style]font.Face
}

// NewFaces makes faces from fs (see Fonts) at k device pixels per CSS pixel.
func NewFaces(fs [3]*opentype.Font, k float64) *Faces {
	return &Faces{fonts: fs, k: k, m: map[Style]font.Face{}}
}

// Face is the style's face at the Faces' scale, made on first use.
func (f *Faces) Face(s Style) font.Face {
	if fc, ok := f.m[s]; ok {
		return fc
	}
	// At DPI 72 a point is a pixel, so Size is the device-pixel size.
	// Unhinted: hinting snaps advances to whole pixels, which would make a
	// line measured at 1× a different width drawn at 2×.
	fc, err := opentype.NewFace(f.fonts[s.Font], &opentype.FaceOptions{Size: s.Size * f.k, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		// NewFace fails only on a bad size, and every size is a positive
		// constant times a positive scale
		panic(err)
	}
	f.m[s] = fc
	return fc
}

// Close releases every face made.
func (f *Faces) Close() {
	for _, fc := range f.m {
		_ = fc.Close()
	}
}

// Width is s's advance in CSS pixels.
func (f *Faces) Width(st Style, s string) float64 {
	return fix2f(font.MeasureString(f.Face(st), s)) / f.k
}

// Fit shortens s with an ellipsis until it is at most w CSS pixels wide.
// The longest prefix that fits with its "…" is found by binary search:
// measuring is the cost, and a long SQL fragment or type name would
// otherwise be measured once per rune dropped.
func (f *Faces) Fit(st Style, s string, w float64) string {
	if f.Width(st, s) <= w {
		return s
	}
	r := []rune(s)
	lo, hi := 0, len(r)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if f.Width(st, string(r[:mid])+"…") <= w {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return strings.TrimRight(string(r[:lo]), " ") + "…"
}

// Wrap breaks s into lines of at most w CSS pixels, at spaces where it can
// and inside a word where it must (a long identifier, a URL). Newlines in s
// are kept as line breaks.
func (f *Faces) Wrap(st Style, s string, w float64) []string {
	var out []string
	for para := range strings.SplitSeq(s, "\n") {
		line := ""
		for word := range strings.FieldsSeq(para) {
			try := word
			if line != "" {
				try = line + " " + word
			}
			if f.Width(st, try) <= w {
				line = try
				continue
			}
			if line != "" {
				out = append(out, line)
			}
			// a word wider than the whole measure is cut into pieces that fit
			for f.Width(st, word) > w {
				r := []rune(word)
				n := len(r) - 1
				for n > 1 && f.Width(st, string(r[:n])) > w {
					n--
				}
				out = append(out, string(r[:n]))
				word = string(r[n:])
			}
			line = word
		}
		out = append(out, line)
	}
	return out
}

func fix2f(v fixed.Int26_6) float64 { return float64(v) / 64 }

// ---------------------------------------------------------------------------
// Colors
// ---------------------------------------------------------------------------

// RGB parses a theme's #rrggbb into an opaque color.
func RGB(hex string) color.RGBA {
	r, g, b, _ := theme.ParseHex(hex)
	return color.RGBA{r, g, b, 0xff}
}

// Mix is a at weight t over b: 0 is b, 1 is a (CSS color-mix order).
func Mix(a, b color.RGBA, t float64) color.RGBA {
	t = math.Max(0, math.Min(1, t))
	l := func(x, y uint8) uint8 { return uint8(math.Round(float64(x)*t + float64(y)*(1-t))) }
	return color.RGBA{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B), 0xff}
}

// ---------------------------------------------------------------------------
// The painter: shapes and text at scale K, in CSS-pixel coordinates
// ---------------------------------------------------------------------------

// Painter draws onto Img at K device pixels per CSS pixel. Faces must be at
// the same K. A Painter is not safe for concurrent use: its rasterizer is
// reused from shape to shape.
type Painter struct {
	Img   *image.RGBA
	K     float64
	Faces *Faces
	z     vector.Rasterizer
}

// PathFn receives the emitters of one filled path: move starts a subpath,
// line and cube extend it. Coordinates are CSS pixels.
type PathFn func(move, line func(x, y float64), cube func(x1, y1, x2, y2, x, y float64))

// Fill rasterizes one path, anti-aliased, over the picture. The rasterizer
// is sized to the path's own bounding box (minX…maxY, CSS px), not the
// picture: Reset clears every cell it covers, so a picture-sized one would
// make each of a big picture's thousands of shapes cost a pass over
// millions of pixels.
//
// The rasterizer does not clip to the destination. Everything is inside
// the margins by construction; a shape that is not is clipped to the
// picture rather than allowed to write out of bounds.
//
// Several subpaths in one call are accumulated together, and vector clamps
// the accumulated coverage, so overlapping subpaths of the SAME winding
// fill once, not twice — which Polyline relies on.
func (p *Painter) Fill(minX, minY, maxX, maxY float64, col color.Color, path PathFn) {
	x0, y0 := int(math.Floor(minX*p.K)), int(math.Floor(minY*p.K))
	x1, y1 := int(math.Ceil(maxX*p.K))+1, int(math.Ceil(maxY*p.K))+1
	r := image.Rect(x0, y0, x1, y1).Intersect(p.Img.Bounds())
	if r.Empty() {
		return
	}
	x0, y0 = r.Min.X, r.Min.Y
	p.z.Reset(r.Dx(), r.Dy())
	p.z.DrawOp = draw.Over
	tx := func(x float64) float32 { return float32(x*p.K - float64(x0)) }
	ty := func(y float64) float32 { return float32(y*p.K - float64(y0)) }
	path(
		func(x, y float64) { p.z.MoveTo(tx(x), ty(y)) },
		func(x, y float64) { p.z.LineTo(tx(x), ty(y)) },
		func(ax, ay, bx, by, x, y float64) { p.z.CubeTo(tx(ax), ty(ay), tx(bx), ty(by), tx(x), ty(y)) },
	)
	p.z.ClosePath()
	p.z.Draw(p.Img, r, image.NewUniform(col), image.Point{})
}

// Kappa places a cubic's control points so it traces a quarter circle.
const Kappa = 0.5522847498

// RoundRect fills a rectangle with corners of radius rad (0 for square).
func (p *Painter) RoundRect(x, y, w, h, rad float64, col color.Color) {
	rad = math.Max(0, math.Min(rad, math.Min(w, h)/2))
	c := rad * Kappa
	p.Fill(x, y, x+w, y+h, col, func(move, line func(x, y float64), cube func(x1, y1, x2, y2, x, y float64)) {
		move(x+rad, y)
		line(x+w-rad, y)
		cube(x+w-rad+c, y, x+w, y+rad-c, x+w, y+rad)
		line(x+w, y+h-rad)
		cube(x+w, y+h-rad+c, x+w-rad+c, y+h, x+w-rad, y+h)
		line(x+rad, y+h)
		cube(x+rad-c, y+h, x, y+h-rad+c, x, y+h-rad)
		line(x, y+rad)
		cube(x, y+rad-c, x+rad-c, y, x+rad, y)
	})
}

// Box is a bordered rounded rectangle: the border color filled, then the
// fill inset by the border's width — cheaper and crisper than stroking.
func (p *Painter) Box(x, y, w, h, rad, border float64, stroke, fill color.Color) {
	p.RoundRect(x, y, w, h, rad, stroke)
	p.RoundRect(x+border, y+border, w-2*border, h-2*border, math.Max(rad-border, 0), fill)
}

// Circle fills a circle.
func (p *Painter) Circle(cx, cy, rad float64, col color.Color) {
	p.RoundRect(cx-rad, cy-rad, 2*rad, 2*rad, rad, col)
}

// Pt is a CSS-pixel point.
type Pt struct{ X, Y float64 }

// Polyline strokes the line through pts at the given width.
//
// vector only fills, so the stroke is built from one quad per segment (the
// segment offset half the width to either side), all in one path. Every
// quad is wound the same way — its first side is always the segment's LEFT
// offset — so where neighbouring quads overlap at a joint the coverage
// clamps rather than cancels. That makes it safe for a tight U-turn (a
// self-referencing key's loop), where a single offset outline would fold
// over itself and punch a hole. The joints' outer corners leave a sliver on
// a sharp turn; curves flattened by CubicPts are fine enough that no turn
// is sharp.
//
// The line is rasterized in chunks of a few segments, each with its own
// small bounding box. One path for the whole line would size the
// rasterizer to the line's bounding box, and a line from a hub table to a
// child forty rows down spans millions of pixels that would all be cleared
// and accumulated for a 1.6 px stroke — that made a 400-table diagram take
// seconds. Chunks share their joint segment's end, so the only cost is a
// joint's anti-aliased fringe being composited twice, which does not show.
func (p *Painter) Polyline(pts []Pt, width float64, col color.Color) {
	const chunk = 8
	for lo := 0; lo < len(pts)-1; lo += chunk {
		hi := min(lo+chunk, len(pts)-1)
		p.polySpan(pts, lo, hi, width, col)
	}
}

// polySpan strokes segments lo+1…hi of pts (point lo to point hi) as one
// path. The segment indexes are the whole line's, so it knows which joints
// are interior and which are the line's true ends.
func (p *Painter) polySpan(pts []Pt, lo, hi int, width float64, col color.Color) {
	minX, minY, maxX, maxY := pts[lo].X, pts[lo].Y, pts[lo].X, pts[lo].Y
	for _, q := range pts[lo+1 : hi+1] {
		minX, maxX = math.Min(minX, q.X), math.Max(maxX, q.X)
		minY, maxY = math.Min(minY, q.Y), math.Max(maxY, q.Y)
	}
	hw := width / 2
	p.Fill(minX-width, minY-width, maxX+width, maxY+width, col, func(move, line func(x, y float64), _ func(x1, y1, x2, y2, x, y float64)) {
		for i := lo + 1; i <= hi; i++ {
			a, b := pts[i-1], pts[i]
			dx, dy := b.X-a.X, b.Y-a.Y
			l := math.Hypot(dx, dy)
			if l == 0 {
				continue
			}
			// Each segment is extended by half the width at an interior
			// joint, so consecutive quads overlap there instead of
			// meeting edge to edge (which leaves a hairline gap on a
			// bend). The polyline's two real ends are not extended: they
			// sit on a box edge or a marker, and would poke past it.
			ex, ey := dx/l*hw, dy/l*hw
			nx, ny := -dy/l*hw, dx/l*hw
			ax, ay, bx, by := a.X, a.Y, b.X, b.Y
			if i > 1 {
				ax, ay = a.X-ex, a.Y-ey
			}
			if i < len(pts)-1 {
				bx, by = b.X+ex, b.Y+ey
			}
			move(ax+nx, ay+ny)
			line(bx+nx, by+ny)
			line(bx-nx, by-ny)
			line(ax-nx, ay-ny)
			line(ax+nx, ay+ny)
		}
	})
}

// CubicPts flattens the cubic Bézier a→b with control points c1, c2 into
// points, a included, b included. 40 steps keeps every turn between two
// segments under a few degrees even on a self-loop's half circle.
func CubicPts(a, c1, c2, b Pt) []Pt {
	const steps = 40
	out := make([]Pt, 0, steps+1)
	for i := 0; i <= steps; i++ {
		t := float64(i) / steps
		u := 1 - t
		out = append(out, Pt{
			u*u*u*a.X + 3*u*u*t*c1.X + 3*u*t*t*c2.X + t*t*t*b.X,
			u*u*u*a.Y + 3*u*u*t*c1.Y + 3*u*t*t*c2.Y + t*t*t*b.Y,
		})
	}
	return out
}

// Text draws s with its line box's top at y, the glyphs centred in the line
// box the way CSS centres them: half the leading above, then the ascent to
// the baseline.
func (p *Painter) Text(st Style, col color.Color, x, y float64, s string) {
	fc := p.Faces.Face(st)
	m := fc.Metrics()
	lead := (st.LineH()*p.K - fix2f(m.Ascent+m.Descent)) / 2
	d := font.Drawer{Dst: p.Img, Src: image.NewUniform(col), Face: fc,
		Dot: fixed.Point26_6{X: fixed.Int26_6(x * p.K * 64), Y: fixed.Int26_6((y*p.K + lead + fix2f(m.Ascent)) * 64)}}
	d.DrawString(s)
}
