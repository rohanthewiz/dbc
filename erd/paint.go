package erd

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

// The drawing primitives the diagram is painted with: the Go fonts, filled
// rounded rectangles, stroked polylines and text, all anti-aliased by
// golang.org/x/image/vector, in pure Go.
//
// They follow explain/picture.go's painter (same units, same fonts, same
// bounding-box-sized rasterizer) but are this package's own: the plan's
// painter is unexported and draws only the vertical S-curves a tree needs,
// while a diagram's edges leave a box sideways, loop back to the same side
// and carry crow's-foot markers — a general polyline stroke. Sharing would
// mean exporting the plan's painter as an API; the two are small enough
// that each package keeping the one it needs is the cheaper coupling.
//
// UNITS. Layout is in CSS pixels, measured at 1× with unhinted faces
// (unhinted advances scale linearly), and painted at k device pixels per
// CSS pixel, so the layout never depends on the scale it is drawn at.

const (
	fRegular = iota
	fBold
	fMono
)

// fonts are parsed once per process; the parsed fonts are read-only and safe
// to share, while the faces made from them (below) are not.
var fonts = sync.OnceValues(func() ([3]*opentype.Font, error) {
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

// txt is a text style: which font, at what CSS-pixel size.
type txt struct {
	font int
	size float64
}

var (
	tTitle  = txt{fBold, 16}
	tTable  = txt{fBold, 13}
	tCol    = txt{fRegular, 12}
	tColB   = txt{fBold, 12}
	tType   = txt{fMono, 11}
	tBadge  = txt{fMono, 9.5}
	tLegend = txt{fRegular, 11.5}
)

// lineH is a style's line height, CSS's usual 1.45 for this UI.
func (t txt) lineH() float64 { return t.size * 1.45 }

// faces caches one font.Face per style at one scale. A face is not safe for
// concurrent use, so every render makes its own.
type faces struct {
	fonts [3]*opentype.Font
	k     float64
	m     map[txt]font.Face
}

func newFaces(fs [3]*opentype.Font, k float64) *faces {
	return &faces{fonts: fs, k: k, m: map[txt]font.Face{}}
}

func (f *faces) face(t txt) font.Face {
	if fc, ok := f.m[t]; ok {
		return fc
	}
	// DPI 72 makes a point a pixel. Unhinted, so a width measured at 1×
	// is the same width drawn at 2× (hinting snaps advances to pixels).
	fc, err := opentype.NewFace(f.fonts[t.font], &opentype.FaceOptions{Size: t.size * f.k, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		// only a non-positive size fails, and every size is a positive
		// constant times a positive scale
		panic(err)
	}
	f.m[t] = fc
	return fc
}

func (f *faces) close() {
	for _, fc := range f.m {
		_ = fc.Close()
	}
}

// width is s's advance in CSS pixels.
func (f *faces) width(t txt, s string) float64 {
	return float64(font.MeasureString(f.face(t), s)) / 64 / f.k
}

// fit shortens s with an ellipsis until it is at most w CSS pixels wide.
// The longest prefix that fits is found by binary search: measuring is the
// cost, and a long type name would otherwise be measured once per rune.
func (f *faces) fit(t txt, s string, w float64) string {
	if f.width(t, s) <= w {
		return s
	}
	r := []rune(s)
	lo, hi := 0, len(r)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if f.width(t, string(r[:mid])+"…") <= w {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return strings.TrimRight(string(r[:lo]), " ") + "…"
}

// palette is the theme's hex colors, parsed, plus the few mixes the
// diagram uses.
type palette struct {
	bg, panel, panel2, line, fg, muted, accent, warn color.RGBA
	edge                                             color.RGBA // relationship lines
}

func newPalette(p theme.Palette) palette {
	c := func(hex string) color.RGBA {
		r, g, b, _ := theme.ParseHex(hex)
		return color.RGBA{r, g, b, 0xff}
	}
	out := palette{bg: c(p.Bg), panel: c(p.Panel), panel2: c(p.Panel2), line: c(p.Line), fg: c(p.Fg),
		muted: c(p.Muted), accent: c(p.Accent), warn: c(p.Warn)}
	// lines a little quieter than the accent the table names wear, so the
	// boxes read first and the lines second
	out.edge = mix(out.accent, out.muted, 0.7)
	return out
}

// mix is a at weight t over b: 0 is b, 1 is a (CSS color-mix order).
func mix(a, b color.RGBA, t float64) color.RGBA {
	t = math.Max(0, math.Min(1, t))
	l := func(x, y uint8) uint8 { return uint8(math.Round(float64(x)*t + float64(y)*(1-t))) }
	return color.RGBA{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B), 0xff}
}

// painter draws at scale k onto img, taking CSS-pixel coordinates.
type painter struct {
	img   *image.RGBA
	k     float64
	z     vector.Rasterizer
	faces *faces
}

// pathFn receives the emitters of one filled path: move starts a subpath,
// line and cube extend it. Coordinates are CSS pixels.
type pathFn func(move, line func(x, y float64), cube func(x1, y1, x2, y2, x, y float64))

// fill rasterizes one path over the picture. The rasterizer is sized to the
// path's own bounding box (minX…maxY, CSS px), not the picture: Reset clears
// every cell it covers, so a picture-sized one would make each of a big
// diagram's thousands of shapes cost a pass over millions of pixels.
//
// Several subpaths in one call are accumulated together, and vector clamps
// the accumulated coverage, so overlapping subpaths of the SAME winding
// fill once, not twice — which the polyline stroke below relies on.
func (pt *painter) fill(minX, minY, maxX, maxY float64, col color.Color, path pathFn) {
	x0, y0 := int(math.Floor(minX*pt.k)), int(math.Floor(minY*pt.k))
	x1, y1 := int(math.Ceil(maxX*pt.k))+1, int(math.Ceil(maxY*pt.k))+1
	r := image.Rect(x0, y0, x1, y1).Intersect(pt.img.Bounds())
	// the rasterizer does not clip; everything is inside the margins by
	// construction, and a shape that is not is clipped to the picture
	if r.Empty() {
		return
	}
	x0, y0 = r.Min.X, r.Min.Y
	pt.z.Reset(r.Dx(), r.Dy())
	pt.z.DrawOp = draw.Over
	tx := func(x float64) float32 { return float32(x*pt.k - float64(x0)) }
	ty := func(y float64) float32 { return float32(y*pt.k - float64(y0)) }
	path(
		func(x, y float64) { pt.z.MoveTo(tx(x), ty(y)) },
		func(x, y float64) { pt.z.LineTo(tx(x), ty(y)) },
		func(ax, ay, bx, by, x, y float64) { pt.z.CubeTo(tx(ax), ty(ay), tx(bx), ty(by), tx(x), ty(y)) },
	)
	pt.z.ClosePath()
	pt.z.Draw(pt.img, r, image.NewUniform(col), image.Point{})
}

// kappa places a cubic's control points so it traces a quarter circle.
const kappa = 0.5522847498

// roundRect fills a rectangle with corners of radius rad (0 for square).
func (pt *painter) roundRect(x, y, w, h, rad float64, col color.Color) {
	rad = math.Max(0, math.Min(rad, math.Min(w, h)/2))
	c := rad * kappa
	pt.fill(x, y, x+w, y+h, col, func(move, line func(x, y float64), cube func(x1, y1, x2, y2, x, y float64)) {
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

func (pt *painter) circle(cx, cy, rad float64, col color.Color) {
	pt.roundRect(cx-rad, cy-rad, 2*rad, 2*rad, rad, col)
}

// pt2 is a CSS-pixel point.
type pt2 struct{ x, y float64 }

// polyline strokes the line through pts at the given width.
//
// vector only fills, so the stroke is built from one quad per segment (the
// segment offset half the width to either side), all in one path. Every
// quad is wound the same way — its first side is always the segment's LEFT
// offset — so where neighbouring quads overlap at a joint the coverage
// clamps rather than cancels. That is what makes this safe for the tight
// U-turn of a self-referencing key, where a single offset outline would
// fold over itself and punch a hole. The joints' outer corners leave a
// sliver on a sharp turn; the curves here are flattened finely enough
// (see cubicPts) that no turn is sharp.
//
// The line is rasterized in chunks of a few segments, each with its own
// small bounding box. One path for the whole line would size the rasterizer
// to the line's bounding box, and a line from a hub table to a child forty
// rows down spans millions of pixels that would all be cleared and
// accumulated for a 1.6 px stroke — that made a 400-table picture take
// seconds. Chunks share their joint segment's end, so the only cost is a
// joint's anti-aliased fringe being composited twice, which does not show.
func (pt *painter) polyline(pts []pt2, width float64, col color.Color) {
	const chunk = 8
	for lo := 0; lo < len(pts)-1; lo += chunk {
		hi := min(lo+chunk, len(pts)-1)
		pt.polySpan(pts, lo, hi, width, col)
	}
}

// polySpan strokes segments lo+1…hi of pts (point lo to point hi) as one
// path. The segment indexes are the whole line's, so it knows which joints
// are interior and which are the line's true ends.
func (pt *painter) polySpan(pts []pt2, lo, hi int, width float64, col color.Color) {
	minX, minY, maxX, maxY := pts[lo].x, pts[lo].y, pts[lo].x, pts[lo].y
	for _, p := range pts[lo+1 : hi+1] {
		minX, maxX = math.Min(minX, p.x), math.Max(maxX, p.x)
		minY, maxY = math.Min(minY, p.y), math.Max(maxY, p.y)
	}
	hw := width / 2
	pt.fill(minX-width, minY-width, maxX+width, maxY+width, col, func(move, line func(x, y float64), _ func(x1, y1, x2, y2, x, y float64)) {
		for i := lo + 1; i <= hi; i++ {
			a, b := pts[i-1], pts[i]
			dx, dy := b.x-a.x, b.y-a.y
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
			ax, ay, bx, by := a.x, a.y, b.x, b.y
			if i > 1 {
				ax, ay = a.x-ex, a.y-ey
			}
			if i < len(pts)-1 {
				bx, by = b.x+ex, b.y+ey
			}
			move(ax+nx, ay+ny)
			line(bx+nx, by+ny)
			line(bx-nx, by-ny)
			line(ax-nx, ay-ny)
			line(ax+nx, ay+ny)
		}
	})
}

// cubicPts flattens the cubic Bézier a→b with control points c1, c2 into
// points, a included, b included. 40 steps keeps every turn between two
// segments under a few degrees even on a self-loop's half circle.
func cubicPts(a, c1, c2, b pt2) []pt2 {
	const steps = 40
	out := make([]pt2, 0, steps+1)
	for i := 0; i <= steps; i++ {
		t := float64(i) / steps
		u := 1 - t
		out = append(out, pt2{
			u*u*u*a.x + 3*u*u*t*c1.x + 3*u*t*t*c2.x + t*t*t*b.x,
			u*u*u*a.y + 3*u*u*t*c1.y + 3*u*t*t*c2.y + t*t*t*b.y,
		})
	}
	return out
}

// text draws s with its line box's top at y, glyphs centred in the line
// box the way CSS centres them: half the leading above, then the ascent.
func (pt *painter) text(t txt, col color.Color, x, y float64, s string) {
	fc := pt.faces.face(t)
	m := fc.Metrics()
	lead := (t.lineH()*pt.k - float64(m.Ascent+m.Descent)/64) / 2
	d := font.Drawer{Dst: pt.img, Src: image.NewUniform(col), Face: fc,
		Dot: fixed.Point26_6{X: fixed.Int26_6(x * pt.k * 64), Y: fixed.Int26_6((y*pt.k + lead + float64(m.Ascent)/64) * 64)}}
	d.DrawString(s)
}
