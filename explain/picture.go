package explain

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
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

// The plan as a picture: the graph view, drawn in Go, for the formats that
// travel as a file rather than a page — a JPEG or PNG for a chat or a
// ticket, and the PDF (pdf.go) that wraps the same raster for a document or
// a printer.
//
//	┌──────────────────────────────────────────────────────────┐
//	│ [dbc] Plan · postgres · analyzed · execution 21.6 ms …   │  header: headline,
//	│ engine postgres · connection prod · analyzed · by time   │  facts, notes, the
//	│ ┌ SELECT …  (the statement, up to 14 lines) ───────────┐ │  statement
//	│ └──────────────────────────────────────────────────────┘ │
//	│                   ┌──────────┐                           │
//	│                   │Hash Join │   the tidy tree, laid out │  graph: the web
//	│                   └────▲─────┘   exactly as plan.js lays │  view's cards and
//	│             36.7k rows │ 12 rows  it out (same card size,│  edges
//	│          ┌──────────┐    ┌──────────┐  gaps and folding) │
//	│          │Seq Scan  │    │Hash      │                    │
//	│          └──────────┘    └──────────┘                    │
//	│ INSIGHTS                                                 │  findings: title,
//	│ ▌● Sequential scan on orders reads 200k rows for 12 …    │  detail, fix, SQL
//	└──────────────────────────────────────────────────────────┘
//
// WHY DRAWN IN GO, NOT CAPTURED FROM THE BROWSER. The picture has to come
// out of `dbc explain -t jpeg` on a CI box and out of the TUI, where there
// is no browser to screenshot. Drawing it here gives one renderer for all
// three homes (CLI, TUI, web), testable with `go test`, with no headless
// Chrome and no cgo — golang.org/x/image supplies an anti-aliasing
// rasterizer and the Go fonts, both pure Go.
//
// WHY THE SAME LAYOUT AS plan.js. Someone who has been looking at the Plan
// tab and sends a JPEG should send the picture they were looking at, so the
// card size, gaps, heat ramp, edge weights and fold-at-depth-5 rule are
// the view's own numbers (see the constants below and computeLayout in
// plan.js). What a still image cannot do — hover, pan, the detail panel —
// is replaced by putting the findings under the graph.
//
// UNITS. Everything is laid out in CSS pixels, the view's own unit, and
// drawn at Scale device pixels per CSS pixel (2 by default, a "retina"
// picture that stays sharp when a viewer zooms in). Text is measured with
// unhinted faces at 1× — unhinted advances scale linearly — so the layout
// does not depend on the scale it is later drawn at.

// PictureOptions choose how the picture looks. The zero value is the plan
// view's defaults: the plan's best metric, the dark dbc palette, 2×.
type PictureOptions struct {
	// Metric sizes the bars and heat-colors the cards; "" is the plan's
	// best (Plan.Metric), and one the plan does not have falls back to it.
	Metric Metric
	// Palette colors the picture; the zero value is theme.Default(). dbc web
	// passes the light or dark palette the view is wearing, so what is sent
	// is what was on screen.
	Palette theme.Palette
	// Compare is the before/after line for a plan re-explained after a
	// change ("vs last: 3.4× faster"), shown in the header when set.
	Compare string
	// Scale is device pixels per CSS pixel; 0 means 2. A plan too big to
	// draw at it is drawn smaller (see maxPixels).
	Scale float64
}

// Layout constants, in CSS pixels. The card and gap sizes are plan.js's
// (CW, CH, HG, VG) so the picture and the Plan tab lay out the same.
const (
	picCardW  = 250.0
	picCardH  = 108.0
	picHGap   = 30.0
	picVGap   = 64.0
	picMargin = 28.0
	picMinW   = 960.0 // the header and findings need room even for a one-step plan
	picTextW  = 880.0 // findings wrap at a readable measure, not the tree's width

	// maxPixels bounds the raster: an RGBA picture costs 4 bytes a pixel,
	// so 36M pixels is ~144 MB at worst. A plan big enough to hit it (well
	// past the 120 steps at which the tree folds) is drawn at a smaller
	// scale rather than refused — a picture that needs zooming beats none.
	maxPixels = 36_000_000
	// maxSide keeps both sides inside what every encoder and viewer takes
	// (JPEG's hard limit is 65,535).
	maxSide = 20_000.0
	// picFoldAt is plan.js's rule: a plan of more than this many steps
	// starts folded below depth 5, so the first view is a map, not a wall.
	picFoldAt    = 120
	picFoldDepth = 5
	// stmtMaxLines caps the statement in the header; a 400-line generated
	// query would otherwise push the plan off the first screen.
	stmtMaxLines = 14
)

// JPEG renders the picture as a JPEG. Quality 90 keeps text edges clean;
// the flat fills compress well enough that higher buys nothing visible.
func (p *Plan) JPEG(opt PictureOptions) ([]byte, error) {
	img, err := p.Picture(opt)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err = jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		return nil, serr.Wrap(err, "op", "encode plan jpeg")
	}
	return b.Bytes(), nil
}

// PNG renders the picture as a PNG: lossless, and for a picture of flat
// fills and text usually smaller than the JPEG too.
func (p *Plan) PNG(opt PictureOptions) ([]byte, error) {
	img, err := p.Picture(opt)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err = png.Encode(&b, img); err != nil {
		return nil, serr.Wrap(err, "op", "encode plan png")
	}
	return b.Bytes(), nil
}

// Picture draws the plan. See the top of this file for what is on it.
func (p *Plan) Picture(opt PictureOptions) (*image.RGBA, error) {
	img, _, err := p.picture(opt)
	return img, err
}

// picture is Picture plus the scale it was drawn at, which the PDF needs to
// size its page in points rather than pixels.
func (p *Plan) picture(opt PictureOptions) (*image.RGBA, float64, error) {
	fs, err := picFonts()
	if err != nil {
		return nil, 0, err
	}
	if opt.Palette == (theme.Palette{}) {
		opt.Palette = theme.Default()
	}
	m := opt.Metric
	valid := false
	for _, x := range p.Metrics() {
		valid = valid || x == m
	}
	if !valid {
		m = p.Metric
		if m == "" {
			m = p.Metrics()[0]
		}
	}

	// Pass 1 — lay out: measure everything at 1× and record what to draw
	// as ops, so the image can be allocated at its final size before a
	// single pixel is painted.
	lay := &picLayout{plan: p, metric: m, pal: newPicPalette(opt.Palette), meas: newPicFaces(fs, 1)}
	defer lay.meas.close()
	w, h := lay.build(opt.Compare)

	// Pass 2 — choose the scale and paint.
	k := opt.Scale
	if k <= 0 {
		k = 2
	}
	k = math.Min(k, math.Sqrt(maxPixels/(w*h)))
	k = math.Min(k, math.Min(maxSide/w, maxSide/h))
	img := image.NewRGBA(image.Rect(0, 0, int(math.Ceil(w*k)), int(math.Ceil(h*k))))
	pt := &painter{img: img, k: k, faces: newPicFaces(fs, k)}
	defer pt.faces.close()
	draw.Draw(img, img.Bounds(), image.NewUniform(lay.pal.bg), image.Point{}, draw.Src)
	for _, op := range lay.ops {
		op(pt)
	}
	return img, k, nil
}

// ---------------------------------------------------------------------------
// Fonts and text
// ---------------------------------------------------------------------------

// The Go fonts: a humanist sans for prose, its bold for names, and the
// monospace for SQL. They are embedded in golang.org/x/image, so the
// picture looks the same on every machine and needs no system font.
const (
	fRegular = iota
	fBold
	fMono
)

var picFonts = sync.OnceValues(func() ([3]*opentype.Font, error) {
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
	tHead    = txt{fBold, 16}
	tBadge   = txt{fBold, 13}
	tMeta    = txt{fRegular, 12.5}
	tMono    = txt{fMono, 12}
	tOp      = txt{fBold, 13.5}
	tCard    = txt{fRegular, 12}
	tCardB   = txt{fBold, 12}
	tSum     = txt{fMono, 11.5}
	tKind    = txt{fMono, 9.5}
	tEdge    = txt{fRegular, 11}
	tSection = txt{fBold, 12}
	tInsT    = txt{fBold, 13}
	tInsD    = txt{fRegular, 12.5}
)

// lineH is the line height for a style — plan.css's 1.45.
func (t txt) lineH() float64 { return t.size * 1.45 }

// picFaces caches one font.Face per style at one scale. Faces are not safe
// for concurrent use, so every render makes its own.
type picFaces struct {
	fonts [3]*opentype.Font
	k     float64
	faces map[txt]font.Face
}

func newPicFaces(fs [3]*opentype.Font, k float64) *picFaces {
	return &picFaces{fonts: fs, k: k, faces: map[txt]font.Face{}}
}

func (f *picFaces) face(t txt) font.Face {
	if fc, ok := f.faces[t]; ok {
		return fc
	}
	// At DPI 72 a point is a pixel, so Size is the device-pixel size.
	// Unhinted: hinting snaps advances to whole pixels, which would make
	// a line measured at 1× a different width drawn at 2×.
	fc, err := opentype.NewFace(f.fonts[t.font], &opentype.FaceOptions{Size: t.size * f.k, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		// NewFace fails only on a bad size, and every size here is a
		// positive constant times a positive scale
		panic(err)
	}
	f.faces[t] = fc
	return fc
}

func (f *picFaces) close() {
	for _, fc := range f.faces {
		_ = fc.Close()
	}
}

// width is s's advance in CSS pixels.
func (f *picFaces) width(t txt, s string) float64 {
	return fix2f(font.MeasureString(f.face(t), s)) / f.k
}

// fit shortens s with an ellipsis until it is at most w CSS pixels wide.
func (f *picFaces) fit(t txt, s string, w float64) string {
	if f.width(t, s) <= w {
		return s
	}
	r := []rune(s)
	// binary search the longest prefix that fits with its "…": measuring
	// is the cost here, and a long SQL fragment would otherwise be
	// measured once per rune dropped
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

// wrap breaks s into lines of at most w CSS pixels, at spaces where it
// can and inside a word where it must (a long identifier, a URL).
// Newlines in s are kept as line breaks.
func (f *picFaces) wrap(t txt, s string, w float64) []string {
	var out []string
	for para := range strings.SplitSeq(s, "\n") {
		line := ""
		for word := range strings.FieldsSeq(para) {
			try := word
			if line != "" {
				try = line + " " + word
			}
			if f.width(t, try) <= w {
				line = try
				continue
			}
			if line != "" {
				out = append(out, line)
			}
			// a word wider than the whole measure is cut into pieces that fit
			for f.width(t, word) > w {
				r := []rune(word)
				n := len(r) - 1
				for n > 1 && f.width(t, string(r[:n])) > w {
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

// picPalette is the theme's hex strings, parsed once.
type picPalette struct {
	bg, panel, panel2, line, fg, muted, accent, warn, err color.RGBA
}

func newPicPalette(p theme.Palette) picPalette {
	c := func(hex string) color.RGBA {
		r, g, b, _ := theme.ParseHex(hex)
		return color.RGBA{r, g, b, 0xff}
	}
	return picPalette{bg: c(p.Bg), panel: c(p.Panel), panel2: c(p.Panel2), line: c(p.Line), fg: c(p.Fg),
		muted: c(p.Muted), accent: c(p.Accent), warn: c(p.Warn), err: c(p.Err)}
}

// mixRGB is a at weight t over b: 0 is b, 1 is a (CSS color-mix order).
func mixRGB(a, b color.RGBA, t float64) color.RGBA {
	t = math.Max(0, math.Min(1, t))
	l := func(x, y uint8) uint8 { return uint8(math.Round(float64(x)*t + float64(y)*(1-t))) }
	return color.RGBA{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B), 0xff}
}

// heat is plan.js's ramp for a step's share of the metric: accent through
// warn to err, reaching warn at 40% and err at 80%.
func (c picPalette) heat(s float64) color.RGBA {
	if s <= 0.4 {
		return mixRGB(c.warn, c.accent, s/0.4)
	}
	return mixRGB(c.err, c.warn, (s-0.4)/0.4)
}

func (c picPalette) sev(s Severity) color.RGBA {
	switch s {
	case SevCrit:
		return c.err
	case SevWarn:
		return c.warn
	}
	return c.accent
}

// ---------------------------------------------------------------------------
// The painter: shapes and text at scale k, in CSS-pixel coordinates
// ---------------------------------------------------------------------------

type painter struct {
	img   *image.RGBA
	k     float64
	z     vector.Rasterizer
	faces *picFaces
}

// shape rasterizes one path, anti-aliased, over the picture. The
// rasterizer is sized to the shape's own bounding box rather than the
// whole picture: Reset clears every cell it covers, so a picture-sized
// rasterizer would make each of a big plan's hundreds of shapes cost a
// pass over millions of pixels.
//
// path receives an emitter taking CSS-pixel points; minX…maxY is the
// shape's CSS-pixel bounding box.
func (pt *painter) shape(minX, minY, maxX, maxY float64, col color.Color, path func(move, line func(x, y float64), cube func(x1, y1, x2, y2, x, y float64))) {
	x0, y0 := int(math.Floor(minX*pt.k)), int(math.Floor(minY*pt.k))
	x1, y1 := int(math.Ceil(maxX*pt.k))+1, int(math.Ceil(maxY*pt.k))+1
	r := image.Rect(x0, y0, x1, y1)
	// The rasterizer does not clip to the destination; every shape is
	// inside the margins by construction, and one that is not is skipped
	// rather than allowed to write out of bounds.
	if r.Empty() || !r.In(pt.img.Bounds()) {
		return
	}
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

// roundRect fills a rectangle with corners of radius rad.
func (pt *painter) roundRect(x, y, w, h, rad float64, col color.Color) {
	rad = math.Min(rad, math.Min(w, h)/2)
	c := rad * kappa
	pt.shape(x, y, x+w, y+h, col, func(move, line func(x, y float64), cube func(x1, y1, x2, y2, x, y float64)) {
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

// box is a bordered rounded rectangle: the border color filled, then the
// fill inset by the border's width — cheaper and crisper than stroking.
func (pt *painter) box(x, y, w, h, rad, border float64, stroke, fill color.Color) {
	pt.roundRect(x, y, w, h, rad, stroke)
	pt.roundRect(x+border, y+border, w-2*border, h-2*border, math.Max(rad-border, 0), fill)
}

func (pt *painter) circle(cx, cy, rad float64, col color.Color) {
	pt.roundRect(cx-rad, cy-rad, 2*rad, 2*rad, rad, col)
}

// curve strokes the S-shaped cubic plan.js draws between a parent's bottom
// and a child's top: vertical tangents at both ends, bending at mid-height.
//
// vector only fills, so the stroke is built as its outline: the curve is
// flattened to points, each offset half the width either side along the
// normal, and the left side forward plus the right side back is one closed
// polygon. The curve is gentle (it never turns more than 90°), so the
// offset sides never cross and no join handling is needed.
func (pt *painter) curve(x1, y1, x2, y2, width float64, col color.Color) {
	my := (y1 + y2) / 2
	bez := func(t float64) (float64, float64) {
		u := 1 - t
		// control points: (x1,y1) (x1,my) (x2,my) (x2,y2)
		x := u*u*u*x1 + 3*u*u*t*x1 + 3*u*t*t*x2 + t*t*t*x2
		y := u*u*u*y1 + 3*u*u*t*my + 3*u*t*t*my + t*t*t*y2
		return x, y
	}
	const steps = 32
	var left, right [][2]float64
	for i := 0; i <= steps; i++ {
		t := float64(i) / steps
		x, y := bez(t)
		// the tangent, from a small step either side (clamped at the ends)
		ax, ay := bez(math.Max(t-0.01, 0))
		bx, by := bez(math.Min(t+0.01, 1))
		dx, dy := bx-ax, by-ay
		l := math.Hypot(dx, dy)
		if l == 0 {
			dx, dy, l = 0, 1, 1
		}
		nx, ny := -dy/l*width/2, dx/l*width/2
		left = append(left, [2]float64{x + nx, y + ny})
		right = append(right, [2]float64{x - nx, y - ny})
	}
	minX, maxX := math.Min(x1, x2)-width, math.Max(x1, x2)+width
	pt.shape(minX, y1-width, maxX, y2+width, col, func(move, line func(x, y float64), _ func(x1, y1, x2, y2, x, y float64)) {
		move(left[0][0], left[0][1])
		for _, q := range left[1:] {
			line(q[0], q[1])
		}
		for i := len(right) - 1; i >= 0; i-- {
			line(right[i][0], right[i][1])
		}
	})
}

// text draws s with its top at y (the line box's top, as CSS places it).
func (pt *painter) text(t txt, col color.Color, x, y float64, s string) {
	fc := pt.faces.face(t)
	m := fc.Metrics()
	// center the glyphs in the line box the way CSS does: half the
	// leading above, then the ascent to the baseline
	lead := (t.lineH()*pt.k - fix2f(m.Ascent+m.Descent)) / 2
	d := font.Drawer{Dst: pt.img, Src: image.NewUniform(col), Face: fc,
		Dot: fixed.Point26_6{X: fixed.Int26_6(x * pt.k * 64), Y: fixed.Int26_6((y*pt.k + lead + fix2f(m.Ascent)) * 64)}}
	d.DrawString(s)
}

// ---------------------------------------------------------------------------
// Layout: what goes where, as a list of paint ops
// ---------------------------------------------------------------------------

type picLayout struct {
	plan   *Plan
	metric Metric
	pal    picPalette
	meas   *picFaces
	ops    []func(*painter)

	collapsed map[int]bool
	pos       map[int][2]float64
	treeW     float64
	treeH     float64
}

func (l *picLayout) op(f func(*painter)) { l.ops = append(l.ops, f) }

// build lays out the whole picture and returns its CSS-pixel size.
func (l *picLayout) build(compare string) (w, h float64) {
	p := l.plan
	l.collapsed = map[int]bool{}
	if len(p.nodes) > picFoldAt {
		for _, n := range p.nodes {
			if n.Depth == picFoldDepth && len(n.Children) > 0 {
				l.collapsed[n.ID] = true
			}
		}
	}
	l.layoutTree()

	w = math.Max(l.treeW+2*picMargin, picMinW)
	inner := w - 2*picMargin
	y := picMargin
	y = l.header(y, inner, compare)
	y += 22

	// the tree, centered when the header is the wider of the two
	tx := picMargin + (inner-l.treeW)/2
	l.tree(tx, y)
	y += l.treeH + 24

	y = l.insights(y, math.Min(inner, picTextW))
	return w, y + picMargin
}

// header lays out the badge and headline, the facts line, the plan's notes
// and the statement, from y down; it returns the y below them.
func (l *picLayout) header(y, w float64, compare string) float64 {
	p, c, f := l.plan, l.pal, l.meas
	x := picMargin

	// [dbc] badge, then the headline in what is left of the line
	bw := f.width(tBadge, "dbc") + 16
	head := f.fit(tHead, p.Headline(), w-bw-12)
	top := y
	l.op(func(pt *painter) {
		pt.roundRect(x, top+2, bw, tBadge.lineH()+4, 6, c.accent)
		pt.text(tBadge, c.bg, x+8, top+4, "dbc")
		pt.text(tHead, c.fg, x+bw+12, top, head)
	})
	y += tHead.lineH() + 6

	facts := []string{"engine " + p.Engine}
	if p.Conn != "" {
		facts = append(facts, "connection "+p.Conn)
	}
	switch {
	case p.Analyzed:
		facts = append(facts, "analyzed — measured per step")
	case p.Measured:
		facts = append(facts, "estimated plan · measured run")
	default:
		facts = append(facts, "estimated — not executed")
	}
	if l.metric != MetricShape {
		facts = append(facts, "sized and colored by "+l.metric.Label())
	} else {
		facts = append(facts, "no numbers from the engine — sized by the kind of step")
	}
	if compare != "" {
		facts = append(facts, "vs last: "+compare)
	}
	for _, line := range f.wrap(tMeta, strings.Join(facts, "   ·   "), w) {
		ly := y
		l.op(func(pt *painter) { pt.text(tMeta, c.muted, x, ly, line) })
		y += tMeta.lineH()
	}

	// the notes are facts a reader must have to read the plan right
	// ("ran inside a transaction that was rolled back"), so they are on
	// the picture as the page shows them: a warn-barred callout each
	for _, n := range p.Notes {
		lines := f.wrap(tMeta, n, w-24)
		nh := float64(len(lines))*tMeta.lineH() + 8
		y += 6
		ny := y
		l.op(func(pt *painter) {
			pt.roundRect(x, ny, w, nh, 4, c.panel2)
			pt.roundRect(x, ny, 3, nh, 1.5, c.warn)
			for i, ln := range lines {
				pt.text(tMeta, c.fg, x+12, ny+4+float64(i)*tMeta.lineH(), ln)
			}
		})
		y += nh
	}

	if p.Statement != "" {
		var lines []string
		for raw := range strings.SplitSeq(strings.TrimSpace(p.Statement), "\n") {
			lines = append(lines, f.wrap(tMono, strings.TrimRight(raw, " \t"), w-24)...)
		}
		more := 0
		if len(lines) > stmtMaxLines {
			more = len(lines) - stmtMaxLines + 1
			lines = lines[:stmtMaxLines-1]
		}
		n := len(lines)
		if more > 0 {
			n++
		}
		sh := float64(n)*tMono.lineH() + 16
		y += 10
		sy := y
		l.op(func(pt *painter) {
			pt.box(x, sy, w, sh, 8, 1, c.line, c.panel)
			for i, ln := range lines {
				pt.text(tMono, c.fg, x+12, sy+8+float64(i)*tMono.lineH(), ln)
			}
			if more > 0 {
				pt.text(tMono, c.muted, x+12, sy+8+float64(len(lines))*tMono.lineH(),
					fmt.Sprintf("… %d more lines", more))
			}
		})
		y += sh
	}
	return y
}

// layoutTree is plan.js's computeLayout: each subtree gets the width its
// visible leaves need, and a parent is centered over its first and last
// child. Subtrees own disjoint horizontal spans, so cards never overlap
// and every edge stays short.
func (l *picLayout) layoutTree() {
	span := map[int]float64{}
	kids := func(n *Node) []*Node {
		if l.collapsed[n.ID] {
			return nil
		}
		return n.Children
	}
	var measure func(n *Node) float64
	measure = func(n *Node) float64 {
		s := 0.0
		for i, c := range kids(n) {
			if i > 0 {
				s += picHGap
			}
			s += measure(c)
		}
		span[n.ID] = math.Max(picCardW, s)
		return span[n.ID]
	}
	measure(l.plan.Root)

	l.pos = map[int][2]float64{}
	maxY := 0.0
	var place func(n *Node, x0 float64, depth int)
	place = func(n *Node, x0 float64, depth int) {
		ks := kids(n)
		y := float64(depth) * (picCardH + picVGap)
		maxY = math.Max(maxY, y)
		if len(ks) == 0 {
			l.pos[n.ID] = [2]float64{x0 + (span[n.ID]-picCardW)/2, y}
			return
		}
		total := 0.0
		for i, c := range ks {
			if i > 0 {
				total += picHGap
			}
			total += span[c.ID]
		}
		cx := x0 + (span[n.ID]-total)/2
		for _, c := range ks {
			place(c, cx, depth+1)
			cx += span[c.ID] + picHGap
		}
		a, b := l.pos[ks[0].ID], l.pos[ks[len(ks)-1].ID]
		l.pos[n.ID] = [2]float64{(a[0] + b[0]) / 2, y}
	}
	place(l.plan.Root, 0, 0)
	l.treeW = span[l.plan.Root.ID]
	// room below the deepest row for a folded step's "+N steps" pill
	l.treeH = maxY + picCardH + 34
}

// tree queues the edges, then the cards over them, offset to (ox, oy).
func (l *picLayout) tree(ox, oy float64) {
	p, c, f := l.plan, l.pal, l.meas
	sev := worstSeverities(p)
	at := func(n *Node) (float64, float64) { q := l.pos[n.ID]; return ox + q[0], oy + q[1] }

	var edges, cards []func(*painter)
	var walk func(n *Node)
	walk = func(n *Node) {
		x, y := at(n)
		cards = append(cards, l.card(n, x, y, sev[n.ID]))
		if l.collapsed[n.ID] {
			label := fmt.Sprintf("+%d steps", countBelow(n))
			pw := f.width(tEdge, label) + 18
			px, py := x+picCardW/2-pw/2, y+picCardH+14
			cards = append(cards, func(pt *painter) {
				pt.box(px, py, pw, tEdge.lineH()+4, 10, 1, c.muted, c.panel)
				pt.text(tEdge, c.muted, px+9, py+2, label)
			})
			return
		}
		for _, ch := range n.Children {
			cx, cy := at(ch)
			x1, y1, x2, y2 := x+picCardW/2, y+picCardH, cx+picCardW/2, cy
			// edge weight: log of the rows flowing up, so a 200,000-row
			// stream is visibly fatter than a 5-row one without swamping
			// the drawing — plan.js's formula
			width := 1.5
			if ch.RowsOut > 0 {
				width = math.Min(1.5+math.Log10(ch.RowsOut+1)*1.4, 10)
			}
			var parts []string
			if ch.Relationship != "" && (len(n.Children) > 1 || (ch.Relationship != "Outer" && ch.Relationship != "Inner")) {
				parts = append(parts, ch.Relationship)
			}
			if ch.RowsOut > 0 {
				parts = append(parts, FmtRows(ch.RowsOut)+" rows")
			}
			label := strings.Join(parts, " · ")
			lw := f.width(tEdge, label)
			edges = append(edges, func(pt *painter) {
				pt.curve(x1, y1, x2, y2, width, c.line)
				if label != "" {
					// a bg-colored plate behind the label, the picture's
					// version of the SVG text's halo stroke
					lx, ly := (x1+x2)/2+6, (y1+y2)/2-tEdge.lineH()/2
					pt.roundRect(lx-3, ly, lw+6, tEdge.lineH(), 3, c.bg)
					pt.text(tEdge, c.muted, lx, ly, label)
				}
			})
			walk(ch)
		}
	}
	walk(p.Root)
	l.ops = append(l.ops, edges...)
	l.ops = append(l.ops, cards...)
}

// card is one step's card, plan.js's cardHTML drawn: heat border and tint,
// the operation, what it works on, its most telling detail, rows and its
// share of the metric, and the share bar. The kind glyphs the page draws
// (▤ ⋈ ◈ …) are not in the Go fonts, so the card names its kind in a small
// tag instead — which also says what the glyph meant.
func (l *picLayout) card(n *Node, x, y float64, sev Severity) func(*painter) {
	p, c, f, m := l.plan, l.pal, l.meas, l.metric
	s := p.Share(n, m)
	h := c.heat(s)
	hot := s >= 0.02
	border, fill := c.line, c.panel
	if hot {
		border = h
		fill = mixRGB(h, c.panel, (8+22*math.Min(s/0.6, 1))/100)
	}

	const pad = 10.0
	inner := picCardW - 2*pad
	kind := ""
	if n.Kind != KindOther {
		kind = strings.ToUpper(string(n.Kind))
	}
	kw := f.width(tKind, kind)
	right := kw
	if sev != "" {
		right += 14
	}
	op := f.fit(tOp, n.Op, inner-right-8)
	target := f.fit(tCard, n.Target(), inner)
	sum := f.fit(tSum, n.Summary(), inner)

	rows := ""
	switch {
	case n.NeverExecuted:
		rows = "never ran"
	case n.HasActual:
		rows = fmtRowCount(n.RowsOut) + " rows"
	case n.HasEst:
		rows = "~" + fmtRowCount(n.RowsOut) + " rows"
	}
	factor := ""
	if p.Analyzed && n.Misestimate != 0 {
		// the card's compact form of FmtFactor, as plan.js writes it
		switch {
		case n.Misestimate >= 2:
			factor = "×" + FmtRows(math.Round(n.Misestimate)) + "↑"
		case n.Misestimate <= 0.5:
			factor = "×" + FmtRows(math.Round(1/n.Misestimate)) + "↓"
		}
	}
	// a step with none of the metric (a label step, a synthetic root)
	// shows nothing rather than "0 · 0%", which reads like a measurement
	val := ""
	if m != MetricShape && n.Self(m) > 0 {
		val = fmt.Sprintf("%s · %.0f%%", FmtMetric(m, n.Self(m)), s*100)
	}
	valW := f.width(tCardB, val)
	rowsW := f.width(tCard, rows)
	factorW := f.width(tCardB, factor)

	return func(pt *painter) {
		pt.box(x, y, picCardW, picCardH, 10, 1.5, border, fill)
		ty := y + 7
		pt.text(tOp, c.fg, x+pad, ty, op)
		kx := x + picCardW - pad - kw
		pt.text(tKind, c.muted, kx, ty+3, kind)
		if sev != "" {
			pt.circle(kx-9, ty+tOp.lineH()/2, 4.5, c.sev(sev))
		}
		ty += tOp.lineH() + 2
		pt.text(tCard, c.fg, x+pad, ty, target)
		ty += tCard.lineH()
		pt.text(tSum, c.muted, x+pad, ty, sum)
		ty += tSum.lineH() + 3
		pt.text(tCard, c.fg, x+pad, ty, rows)
		if factor != "" {
			pt.text(tCardB, c.warn, x+pad+rowsW+8, ty, factor)
		}
		if val != "" && x+pad+rowsW+factorW+16 < x+picCardW-pad-valW {
			pt.text(tCardB, c.fg, x+picCardW-pad-valW, ty, val)
		}
		bx, by, bw := x+pad, y+picCardH-11, inner
		pt.roundRect(bx, by, bw, 4, 2, c.panel2)
		if s > 0 {
			pt.roundRect(bx, by, math.Max(bw*s, 4), 4, 2, h)
		}
		if n.NeverExecuted {
			// the page's opacity .5: the card faded half into the page
			pt.roundRect(x, y, picCardW, picCardH, 10, color.RGBA{c.bg.R / 2, c.bg.G / 2, c.bg.B / 2, 0x80})
		}
	}
}

// insights lays out the findings under the graph — the picture's stand-in
// for the page's side panel. It returns the y below them.
func (l *picLayout) insights(y, w float64) float64 {
	p, c, f := l.plan, l.pal, l.meas
	x := picMargin
	head := "INSIGHTS"
	if len(p.Insights) > 0 {
		head = fmt.Sprintf("INSIGHTS (%d)", len(p.Insights))
	}
	hy := y
	l.op(func(pt *painter) {
		pt.roundRect(x, hy-12, w, 1, 0, c.line)
		pt.text(tSection, c.muted, x, hy, head)
	})
	y += tSection.lineH() + 8
	if len(p.Insights) == 0 {
		ey := y
		l.op(func(pt *painter) { pt.text(tInsD, c.muted, x, ey, "No findings — nothing in this plan stands out.") })
		return y + tInsD.lineH()
	}

	const pad = 12.0
	tw := w - 2*pad - 4
	for _, in := range p.Insights {
		title := f.wrap(tInsT, in.Title, tw-14)
		var detail, fix, sql []string
		if in.Detail != "" {
			detail = f.wrap(tInsD, in.Detail, tw)
		}
		if in.Fix != "" {
			fix = f.wrap(tInsD, "→ "+in.Fix, tw)
		}
		if in.SQL != "" {
			sql = f.wrap(tMono, in.SQL, tw-16)
		}
		bh := 10 + float64(len(title))*tInsT.lineH() + float64(len(detail)+len(fix))*tInsD.lineH() + 10
		if len(detail) > 0 {
			bh += 3
		}
		if len(fix) > 0 {
			bh += 4
		}
		if len(sql) > 0 {
			bh += 6 + float64(len(sql))*tMono.lineH() + 12
		}
		by, sc := y, c.sev(in.Severity)
		l.op(func(pt *painter) {
			pt.box(x, by, w, bh, 8, 1, c.line, c.panel)
			pt.roundRect(x, by, 4, bh, 2, sc)
			tx, ty := x+pad+4, by+10
			pt.circle(tx+4, ty+tInsT.lineH()/2, 4, sc)
			for _, ln := range title {
				pt.text(tInsT, c.fg, tx+14, ty, ln)
				ty += tInsT.lineH()
			}
			if len(detail) > 0 {
				ty += 3
			}
			for _, ln := range detail {
				pt.text(tInsD, c.muted, tx, ty, ln)
				ty += tInsD.lineH()
			}
			if len(fix) > 0 {
				ty += 4
			}
			for _, ln := range fix {
				pt.text(tInsD, c.fg, tx, ty, ln)
				ty += tInsD.lineH()
			}
			if len(sql) > 0 {
				ty += 6
				sh := float64(len(sql))*tMono.lineH() + 12
				pt.roundRect(tx, ty, tw, sh, 6, c.panel2)
				for i, ln := range sql {
					pt.text(tMono, c.fg, tx+8, ty+6+float64(i)*tMono.lineH(), ln)
				}
			}
		})
		y += bh + 8
	}
	return y
}

// countBelow is how many steps sit under n, for a folded step's pill.
func countBelow(n *Node) int {
	c := 0
	for _, k := range n.Children {
		c += 1 + countBelow(k)
	}
	return c
}
