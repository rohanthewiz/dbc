// Package tui is dbc's Bubble Tea v2 terminal interface: connections and
// tables sidebar, SQL editor, results grid, log, AI assistant, with the
// mouse as a first-class input.
//
// HOW A FRAME IS MADE. Bubble Tea asks the model for a View, which is one
// styled string. Rather than assembling that string from lipgloss blocks
// joined side by side, every frame is DRAWN into a Canvas — a grid of cells,
// each a grapheme plus a style — and the canvas is serialized once at the end:
//
//	layout(w, h) ──► rects for every pane, button and splitter
//	      │
//	      ├──► draw: each widget paints inside its rect on the Canvas
//	      │          overlays (menus, modals) paint last, on top
//	      │
//	      └──► hit-test: a click is resolved against THE SAME rects
//
// This is cats-todo's rule — one layout description both draws and
// hit-tests, so the two cannot drift — made structural. With string joins
// the rule holds only as long as no line above a clickable one ever wraps;
// with a canvas a widget cannot draw outside its rect at all (Surface clips),
// so a long value in one pane can never shift a click target in another.
// Overlays need no compositor: they are simply drawn after the base.
package tui

import (
	"strconv"
	"strings"

	"github.com/rivo/uniseg"

	"github.com/rohanthewiz/dbc/theme"
)

// Color is a 24-bit RGB color with a "set" flag above it. The zero Color is
// the terminal's own default, so the zero Style is "default on default" —
// the safe reading of a style nobody filled in — and black is still
// representable, as a set color whose RGB happens to be zero.
type Color uint32

const (
	colorDefault Color = 0
	colorSet     Color = 1 << 24
)

// RGB builds a set Color.
func RGB(r, g, b uint8) Color { return colorSet | Color(r)<<16 | Color(g)<<8 | Color(b) }

// hex parses "#rrggbb" into a Color, falling back to the terminal default
// for anything else — a palette value that does not parse should show as
// the terminal's color, not as black.
func hex(s string) Color {
	r, g, b, ok := theme.ParseHex(s)
	if !ok {
		return colorDefault
	}
	return RGB(r, g, b)
}

// Attr is a set of text attributes.
type Attr uint8

const (
	AttrBold Attr = 1 << iota
	AttrDim
	AttrItalic
	AttrUnderline
	AttrReverse
)

// Style is how one cell is painted.
type Style struct {
	Fg, Bg Color
	Attr   Attr
}

// Bold, Italic, Underline and friends return a copy with the attribute set,
// so styles compose as values: st.Bold().Italic().
func (s Style) Bold() Style      { s.Attr |= AttrBold; return s }
func (s Style) Italic() Style    { s.Attr |= AttrItalic; return s }
func (s Style) Underline() Style { s.Attr |= AttrUnderline; return s }
func (s Style) Dim() Style       { s.Attr |= AttrDim; return s }

// WithFg and WithBg return a copy with one color replaced.
func (s Style) WithFg(c Color) Style { s.Fg = c; return s }
func (s Style) WithBg(c Color) Style { s.Bg = c; return s }

// sgr renders the style as one SGR escape. It always starts from a reset
// (0;), which makes every cell's style absolute: the serializer never has to
// reason about which attributes the previous cell left switched on.
func (s Style) sgr() string {
	var b strings.Builder
	b.WriteString("\x1b[0")
	if s.Attr&AttrBold != 0 {
		b.WriteString(";1")
	}
	if s.Attr&AttrDim != 0 {
		b.WriteString(";2")
	}
	if s.Attr&AttrItalic != 0 {
		b.WriteString(";3")
	}
	if s.Attr&AttrUnderline != 0 {
		b.WriteString(";4")
	}
	if s.Attr&AttrReverse != 0 {
		b.WriteString(";7")
	}
	writeColor := func(prefix string, c Color) {
		if c&colorSet == 0 {
			return
		}
		b.WriteString(prefix)
		b.WriteString(strconv.Itoa(int(c >> 16 & 0xff)))
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(int(c >> 8 & 0xff)))
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(int(c & 0xff)))
	}
	writeColor(";38;2;", s.Fg)
	writeColor(";48;2;", s.Bg)
	b.WriteByte('m')
	return b.String()
}

// Rect is a rectangle of cells. The zero Rect is empty and contains nothing,
// which is what a hidden pane's rect is.
type Rect struct{ X, Y, W, H int }

// Contains reports whether the cell (x, y) is inside r.
func (r Rect) Contains(x, y int) bool {
	return x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H
}

// Inset shrinks r by n cells on every side — the inside of a border is
// Inset(1).
func (r Rect) Inset(n int) Rect {
	r.X, r.Y, r.W, r.H = r.X+n, r.Y+n, r.W-2*n, r.H-2*n
	if r.W < 0 {
		r.W = 0
	}
	if r.H < 0 {
		r.H = 0
	}
	return r
}

// Empty reports whether r has no cells.
func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// cell is one screen cell. A wide grapheme (CJK, most emoji) occupies its
// own cell plus w-1 continuation cells, marked by w == 0, which the
// serializer skips — the terminal advances past them by itself.
type cell struct {
	s  string
	w  int8
	st Style
}

// Canvas is one frame being drawn.
type Canvas struct {
	W, H  int
	cells []cell
}

// NewCanvas returns a w×h canvas filled with spaces in the given style.
func NewCanvas(w, h int, fill Style) *Canvas {
	w, h = max(w, 0), max(h, 0)
	c := &Canvas{W: w, H: h, cells: make([]cell, w*h)}
	for i := range c.cells {
		c.cells[i] = cell{s: " ", w: 1, st: fill}
	}
	return c
}

// at returns the cell at (x, y), or nil off-canvas.
func (c *Canvas) at(x, y int) *cell {
	if x < 0 || y < 0 || x >= c.W || y >= c.H {
		return nil
	}
	return &c.cells[y*c.W+x]
}

// Surface is a clipped view of a canvas: coordinates are relative to its
// rect, and nothing drawn through it can land outside. Every widget draws
// through one, which is what guarantees a pane cannot spill into another.
type Surface struct {
	c *Canvas
	r Rect
}

// Sub returns a surface over r (in canvas coordinates), clipped to the canvas.
func (c *Canvas) Sub(r Rect) Surface {
	return Surface{c: c, r: intersect(r, Rect{0, 0, c.W, c.H})}
}

// Sub returns a surface over r, given relative to s and clipped to s.
func (s Surface) Sub(r Rect) Surface {
	abs := Rect{s.r.X + r.X, s.r.Y + r.Y, r.W, r.H}
	return Surface{c: s.c, r: intersect(abs, s.r)}
}

// Rect is the surface's area in canvas coordinates.
func (s Surface) Rect() Rect { return s.r }

// W and H are the surface's size.
func (s Surface) W() int { return s.r.W }
func (s Surface) H() int { return s.r.H }

// intersect clips r to bounds.
func intersect(r, bounds Rect) Rect {
	x0, y0 := max(r.X, bounds.X), max(r.Y, bounds.Y)
	x1, y1 := min(r.X+r.W, bounds.X+bounds.W), min(r.Y+r.H, bounds.Y+bounds.H)
	if x1 <= x0 || y1 <= y0 {
		return Rect{X: x0, Y: y0}
	}
	return Rect{x0, y0, x1 - x0, y1 - y0}
}

// Fill paints every cell of the surface as a space in st.
func (s Surface) Fill(st Style) {
	for y := s.r.Y; y < s.r.Y+s.r.H; y++ {
		for x := s.r.X; x < s.r.X+s.r.W; x++ {
			*s.c.at(x, y) = cell{s: " ", w: 1, st: st}
		}
	}
}

// Put draws text at (x, y) relative to the surface, one grapheme at a time,
// clipped to the surface. It returns the column after the last cell drawn, so
// runs of differently styled text chain: x = s.Put(x, y, a, st1);
// x = s.Put(x, y, b, st2).
//
// Control characters are drawn as a middle dot: a raw tab or newline inside a
// value (a multi-line TEXT column) would otherwise move the terminal's cursor
// and tear the frame apart.
func (s Surface) Put(x, y int, text string, st Style) int {
	if y < 0 || y >= s.r.H {
		return x + uniseg.StringWidth(text)
	}
	state := -1
	for text != "" {
		var g string
		var w int
		g, text, w, state = uniseg.FirstGraphemeClusterInString(text, state)
		if len(g) == 1 && (g[0] < 0x20 || g[0] == 0x7f) {
			g, w = "·", 1
		}
		if w == 0 {
			continue // zero-width joiners and the like ride with the previous cell
		}
		if x >= s.r.W {
			return x + w + uniseg.StringWidth(text)
		}
		if x >= 0 {
			s.set(x, y, g, w, st)
		}
		x += w
	}
	return x
}

// set writes one grapheme of width w at surface-relative (x, y). A wide
// grapheme that would straddle the right edge is replaced by a space: half a
// character is worse than none, and drawing the whole one would spill into
// the next pane.
func (s Surface) set(x, y int, g string, w int, st Style) {
	ax, ay := s.r.X+x, s.r.Y+y
	if x+w > s.r.W {
		*s.c.at(ax, ay) = cell{s: " ", w: 1, st: st}
		return
	}
	// Overwriting half of an existing wide cell would leave the other half
	// orphaned; blank it first.
	s.c.unwide(ax, ay)
	*s.c.at(ax, ay) = cell{s: g, w: int8(w), st: st}
	for i := 1; i < w; i++ {
		s.c.unwide(ax+i, ay)
		*s.c.at(ax+i, ay) = cell{w: 0, st: st}
	}
}

// unwide turns any wide cell overlapping (x, y) back into spaces.
func (c *Canvas) unwide(x, y int) {
	cl := c.at(x, y)
	if cl == nil {
		return
	}
	// find the head of a wide cell this may be a continuation of
	hx := x
	for hx > 0 && c.at(hx, y).w == 0 {
		hx--
	}
	head := c.at(hx, y)
	if head.w <= 1 {
		return
	}
	for i := 0; i < int(head.w); i++ {
		if t := c.at(hx+i, y); t != nil {
			*t = cell{s: " ", w: 1, st: head.st}
		}
	}
}

// Restyle applies f to the style of every cell in the surface without
// touching the text — how a selection or hover highlight is laid over
// content that was drawn normally.
func (s Surface) Restyle(f func(Style) Style) {
	for y := s.r.Y; y < s.r.Y+s.r.H; y++ {
		for x := s.r.X; x < s.r.X+s.r.W; x++ {
			cl := s.c.at(x, y)
			cl.st = f(cl.st)
		}
	}
}

// PutRight draws text right-aligned so it ends at column right (exclusive),
// returning where it started.
func (s Surface) PutRight(right, y int, text string, st Style) int {
	x := right - uniseg.StringWidth(text)
	s.Put(x, y, text, st)
	return x
}

// Box draws a single-line border around the whole surface with an optional
// title in the top edge. The inside is left alone.
func (s Surface) Box(border Style, title string, titleSt Style) {
	w, h := s.r.W, s.r.H
	if w < 2 || h < 2 {
		return
	}
	s.Put(0, 0, "╭", border)
	s.Put(w-1, 0, "╮", border)
	s.Put(0, h-1, "╰", border)
	s.Put(w-1, h-1, "╯", border)
	for x := 1; x < w-1; x++ {
		s.Put(x, 0, "─", border)
		s.Put(x, h-1, "─", border)
	}
	for y := 1; y < h-1; y++ {
		s.Put(0, y, "│", border)
		s.Put(w-1, y, "│", border)
	}
	if title != "" && w > 4 {
		s.Sub(Rect{1, 0, w - 2, 1}).Put(1, 0, " "+title+" ", titleSt)
	}
}

// String serializes the canvas as the styled string Bubble Tea renders. A
// style escape is emitted only when the style changes, and each line ends
// with a reset so a style can never bleed into the terminal's margin.
func (c *Canvas) String() string {
	var b strings.Builder
	b.Grow(c.W * c.H * 4)
	for y := 0; y < c.H; y++ {
		if y > 0 {
			b.WriteByte('\n')
		}
		var cur Style
		first := true
		for x := 0; x < c.W; x++ {
			cl := c.cells[y*c.W+x]
			if cl.w == 0 {
				continue
			}
			if first || cl.st != cur {
				b.WriteString(cl.st.sgr())
				cur, first = cl.st, false
			}
			b.WriteString(cl.s)
		}
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// Line returns row y as plain text, wide cells counted once — for tests,
// which assert on what is on screen without caring how it is colored.
func (c *Canvas) Line(y int) string {
	if y < 0 || y >= c.H {
		return ""
	}
	var b strings.Builder
	for x := 0; x < c.W; x++ {
		if cl := c.cells[y*c.W+x]; cl.w != 0 {
			b.WriteString(cl.s)
		}
	}
	return b.String()
}

// StyleAt returns the style of the cell at (x, y), for tests.
func (c *Canvas) StyleAt(x, y int) Style {
	if cl := c.at(x, y); cl != nil {
		return cl.st
	}
	return Style{}
}

// Text returns the whole canvas as plain lines, for tests and debugging.
func (c *Canvas) Text() string {
	lines := make([]string, c.H)
	for y := range lines {
		lines[y] = c.Line(y)
	}
	return strings.Join(lines, "\n")
}

// width is the display width of s in cells.
func width(s string) int { return uniseg.StringWidth(s) }

// truncate shortens s to at most w cells, ending in "…" when it had to cut.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if width(s) <= w {
		return s
	}
	var b strings.Builder
	used := 0
	state := -1
	for s != "" {
		var g string
		var gw int
		g, s, gw, state = uniseg.FirstGraphemeClusterInString(s, state)
		if used+gw > w-1 {
			break
		}
		b.WriteString(g)
		used += gw
	}
	b.WriteString("…")
	return b.String()
}
