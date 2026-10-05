package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/clip"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
)

// TRANSPOSED (t): the results grid on its side, as psql's \x shows a wide
// row and as dbc web's ⇄ Transpose does — each record a screen column
// headed by its number, each result column a line led by its name.
//
//	upright                      transposed
//	 # │ id │ name │ breed        column │ 1      │ 2      │ 3   ▲
//	 1 │ 1  │ ann  │ tabby   ⇒    id     │ 1      │ 2      │ 3   █
//	 2 │ 2  │ bob  │ manx         name   │ ann    │ bob    │ cy  │
//	 3 │ 3  │ cy   │ rex          breed  │ tabby  │ manx   │ rex │
//	                              rows 1–3 of 3 · transposed
//	                              └ names ┘└─ every record one width ─┘
//
// ONLY THE PICTURE TURNS. The cursor, the range, the sort and the hidden
// set stay in the grid's data coordinates — row = display row (a record),
// col = display column (a result column) — exactly as dbc web keeps them.
// So hiding, sorting, inspecting and the copies' row/column slicing need no
// second implementation; what swaps axes is drawing, hit-testing, the
// arrow keys and scrolling, and a copy or export turns its piece with
// export.Transpose at the end (copyPiece).
//
// Scrolling is kept in SCREEN terms by the same two fields as upright: top
// is the first line drawn and leftCol the first screen column. Upright
// those are a row and a result column; transposed, a result column and a
// record. Toggling sends both home (their meanings just swapped).
//
// WIDTHS. Every record column has ONE width, recWidth: the widest shown
// column's values (each already capped at maxColWidth), until a key, a drag
// or a fit sets recW. The names column fits the longest shown name (plus
// room for the sort arrow) up to maxColWidth, until a drag or a fit sets
// namesW. One width for every record is what dbc web does too — a
// per-record width would make "the same field" wobble across the screen.
// Both are kept with the rest of the layout across a re-run with the same
// columns (SetResult).
//
// The orientation itself is NOT per result: as in dbc web a new result
// arrives in whichever way the grid is turned.

// Transpose turns the grid on its side, or back upright. Allowed with no
// result: it is then the orientation the next result arrives in.
func (g *grid) Transpose() {
	g.flip = !g.flip
	g.hover, g.hoverH, g.hoverB, g.hoverNB = cell2{-1, -1}, -1, -1, false
	// The view's axes just swapped, so the old offsets mean nothing. The
	// last draw's column geometry is of the other orientation too: cleared,
	// ensureVisible fixes the line axis now, and the cursor's screen column
	// is put at the left edge — the next draw has it in view either way.
	g.colX, g.colW = g.colX[:0], g.colW[:0]
	g.top, g.leftCol = 0, g.cur.col
	if g.flip {
		g.leftCol = g.cur.row
	}
	g.ensureVisible()
}

// recWidth is the content width every record column is drawn at.
func (g *grid) recWidth() int {
	if g.recW > 0 {
		return g.recW
	}
	w := max(minColWidth, len(itoa(max(g.Rows(), 1)))) // the header is the record's number
	for _, rc := range g.cols {
		if rc < len(g.valW) {
			w = max(w, g.valW[rc])
		}
	}
	return w
}

// namesWidth is the content width of the names column (the gutter).
func (g *grid) namesWidth() int {
	if g.namesW > 0 {
		return g.namesW
	}
	return min(g.longestName(), maxColWidth)
}

// longestName is the widest shown column name plus room for the sort
// arrow, and never narrower than the column's own heading.
func (g *grid) longestName() int {
	w := width(export.TransposeNameCol)
	for _, rc := range g.cols {
		w = max(w, width(g.res.Columns[rc])+2)
	}
	return w
}

// fitRecords sizes every record to the widest shown value — past the auto
// cap, as an upright fit is: asking to see the values whole is exactly
// when the cap is in the way.
func (g *grid) fitRecords() {
	if g.res == nil {
		return
	}
	w := len(itoa(max(g.Rows(), 1)))
	for _, rc := range g.cols {
		w = max(w, g.valuesWidth(rc))
	}
	g.recW = clampUserW(w)
}

// FitNames sizes the names column to the longest shown name, uncapped.
func (g *grid) FitNames() {
	if g.res == nil {
		return
	}
	g.namesW = clampUserW(g.longestName())
}

// startFlipResize begins a drag of record rec's right border.
func (g *grid) startFlipResize(rec int) {
	g.resizeCol, g.resizeX = rec-g.leftCol, -1
	if len(g.colX) > 0 && g.resizeCol >= 0 {
		g.resizeX = g.colX[0]
	}
}

// startNamesResize begins a drag of the names column's border.
func (g *grid) startNamesResize() {
	g.resizeNames, g.resizeX = true, g.gutter.X
}

// flipResizeTo continues a transposed border drag.
//
// The names border is the plain case: its left edge is fixed, so the width
// is pointer x minus that edge, less the two padding cells.
//
// A record's border is not. Every record shares one width, so widening the
// dragged record i (its place among those drawn) widens the i records left
// of it too, and they carry its border away from the pointer i times faster
// than it moves. dbc web compensates by scrolling; here the view scrolls by
// whole records, so the width is solved for instead. Record i's right
// border sits at
//
//	x = x0 + i·(W+3) + W + 2      (each record: pad, W, pad, then a rule)
//	⇒ W = (x − x0 − 2 − 3i) / (i+1)
//
// which keeps the border under the pointer (to the cell), sharing the drag
// among the i+1 widths that carry it.
func (g *grid) flipResizeTo(x int) {
	if g.resizeX < 0 {
		return
	}
	if g.resizeNames {
		g.namesW = clampUserW(x - g.resizeX - 2)
		return
	}
	i := g.resizeCol
	if i < 0 {
		return
	}
	g.recW = clampUserW((x - g.resizeX - 2 - 3*i) / (i + 1))
}

// flipHitAt is hitAt for the transposed layout. Its kinds keep their
// meaning for the code that acts on them (gridClick, the right-click):
//
//	the names column's heading border  hitNameBorder  resize / fit the names
//	a record's number (header band)    hitRowNum      select the record whole
//	a record's border (header band)    hitBorder      resize / fit every record
//	a name (the gutter)                hitHeader      sort by that column
//	a value                            hitCell
//
// so a name is the "header" of its line and a record's number is the
// "row number" of its record, as in dbc web.
func (g *grid) flipHitAt(x, y int) gridHit {
	if g.vbar.Contains(x, y) {
		return gridHit{kind: hitVBar}
	}
	if y == g.head.Y && x == g.gutter.X+g.gutter.W-1 {
		return gridHit{kind: hitNameBorder}
	}
	rec := -1
	for i, cx := range g.colX {
		if g.head.Contains(x, y) && x == cx+g.colW[i] {
			return gridHit{kind: hitBorder, col: g.leftCol + i}
		}
		if x >= cx && x < cx+g.colW[i] {
			rec = g.leftCol + i
		}
	}
	switch {
	case g.head.Contains(x, y) && rec >= 0:
		return gridHit{kind: hitRowNum, row: rec}
	case g.gutter.Contains(x, y):
		if c := g.top + (y - g.gutter.Y); c < g.Cols() {
			return gridHit{kind: hitHeader, col: c}
		}
	case g.view.Contains(x, y) && rec >= 0:
		if c := g.top + (y - g.view.Y); c < g.Cols() {
			return gridHit{kind: hitCell, row: rec, col: c}
		}
	}
	return gridHit{}
}

// flipDragTo is dragTo transposed: the pointer's line is a column, its
// screen column a record, and a drag past any edge keeps selecting that
// way — the records run sideways, so past the side edges too.
func (g *grid) flipDragTo(x, y int) {
	col, rec := g.cur.col, g.cur.row
	switch {
	case y < g.view.Y:
		col = g.top - 1
	case y >= g.view.Y+g.view.H:
		col = g.top + g.visRow
	default:
		col = g.top + (y - g.view.Y)
	}
	switch {
	case x < g.view.X:
		rec = g.leftCol - 1
	case len(g.colX) > 0 && x >= g.colX[len(g.colX)-1]+g.colW[len(g.colX)-1]:
		rec = g.leftCol + len(g.colX)
	default:
		for i, cx := range g.colX {
			if x >= cx && x < cx+g.colW[i] {
				rec = g.leftCol + i
			}
		}
	}
	g.moveTo(rec, col, true)
}

// drawFlip paints the transposed grid; see the diagram at the top.
func (g *grid) drawFlip(s Surface, st styles, focused bool) {
	lines, recs := g.Cols(), g.Rows()
	nw, rw := g.namesWidth(), g.recWidth()
	gw := nw + 3 // pad, the name, pad, and the rule
	bottom := 1  // the position strip
	barW := 0
	if lines > s.H()-1-bottom {
		barW = 1
	}
	area := s.Rect()
	g.visRow = max(s.H()-1-bottom, 0)
	g.top = max(0, min(g.top, max(lines-g.visRow, 0)))
	g.leftCol = max(0, min(g.leftCol, max(recs-1, 0)))
	g.head = Rect{area.X + gw, area.Y, area.W - gw - barW, 1}
	g.gutter = Rect{area.X, area.Y + 1, gw, g.visRow}
	g.view = Rect{area.X + gw, area.Y + 1, area.W - gw - barW, g.visRow}

	// records: one width for all, from leftCol until the view is full
	x := g.view.X
	for r := g.leftCol; r < recs && x < g.view.X+g.view.W; r++ {
		g.colX = append(g.colX, x)
		g.colW = append(g.colW, rw+2)
		x += rw + 3 // + separator
	}

	// header band: "column", then each record's number
	hs := s.Sub(Rect{0, 0, s.W() - barW, 1})
	hs.Fill(st.header)
	rule := st.header.WithFg(st.border.Fg)
	hs.Put(1, 0, truncate(export.TransposeNameCol, nw), st.header.WithFg(st.muted.Fg))
	nsep, nst := "│", rule
	if len(g.cols) > 0 && g.cols[0] > 0 {
		nsep, nst = "║", st.header.WithFg(st.accent.Fg) // hidden before the first line
	}
	if g.hoverNB {
		nsep, nst = "┃", st.header.WithFg(st.accent.Fg).Bold()
	}
	s.Put(gw-1, 0, nsep, nst)
	for i, cx := range g.colX {
		r := g.leftCol + i
		hst := st.header
		if r == g.cur.row {
			hst = hst.WithFg(st.accent.Fg) // the cursor's record, as upright its row number
		}
		cs := s.Sub(Rect{cx - area.X, 0, g.colW[i], 1})
		cs.Put(1, 0, truncate(itoa(r+1), rw), hst)
		sep, sst := "│", rule
		if r == g.hoverB {
			sep, sst = "┃", st.header.WithFg(st.accent.Fg).Bold()
		}
		s.Put(cx-area.X+g.colW[i], 0, sep, sst)
	}

	// lines: a name, then that column's value in each record in view
	r0, c0, r1, c1 := g.bounds()
	for y := 0; y < g.visRow; y++ {
		c := g.top + y
		if c >= lines {
			break
		}
		rc := g.cols[c]
		arrow := ""
		if rc == g.sortCol {
			arrow = " ▲"
			if g.sortDesc {
				arrow = " ▼"
			}
		}
		ns := st.header
		if c == g.cur.col {
			ns = ns.WithFg(st.accent.Fg)
		}
		if c == g.hoverH {
			ns = ns.Underline()
		}
		s.Sub(Rect{0, y + 1, gw - 1, 1}).Fill(st.header)
		s.Put(1, y+1, truncate(g.res.Columns[rc], nw-width(arrow))+arrow, ns)
		sep, sst := "│", st.border
		if g.hiddenAfter(c) {
			sep, sst = "║", st.accent // hidden columns sit between this line and the next
		}
		s.Put(gw-1, y+1, sep, sst)
		for i, cx := range g.colX {
			r := g.leftCol + i
			val, _, _ := g.value(r, c)
			null := g.isNull(r, c)
			cst := st.base
			if null {
				cst = st.null
			}
			switch {
			case focused && r == g.cur.row && c == g.cur.col:
				cst = cst.WithBg(st.sel.Bg).Bold()
			case g.sel && r >= r0 && r <= r1 && c >= c0 && c <= c1:
				cst = cst.WithBg(st.selRange.Bg)
			case r == g.cur.row:
				cst = cst.WithBg(st.hover.Bg) // the cursor's record, faintly
			case g.hover.row == r && g.hover.col == c:
				cst = cst.WithBg(st.hover.Bg)
			}
			cs := s.Sub(Rect{cx - area.X, y + 1, g.colW[i], 1})
			cs.Fill(cst)
			text := truncate(flatten(val), rw)
			if g.numeric[rc] && !null {
				cs.PutRight(cs.W()-1, 0, text, cst)
			} else {
				cs.Put(1, 0, text, cst)
			}
			s.Put(cx-area.X+g.colW[i], y+1, "│", st.border)
		}
	}

	if barW > 0 {
		g.vbar = Rect{area.X + area.W - 1, area.Y, 1, s.H() - bottom}
		drawVBar(s.Sub(Rect{s.W() - 1, 0, 1, s.H() - bottom}), st, g.top, g.visRow, lines)
	}

	// bottom strip: which records are in view, and that the grid is turned
	strip := s.Sub(Rect{0, s.H() - 1, s.W(), 1})
	strip.Fill(st.muted)
	lastVis := g.leftCol + len(g.colX) - 1
	if g.colPartial() {
		lastVis--
	}
	pos := " no rows"
	if recs > 0 {
		pos = fmt.Sprintf(" rows %d–%d of %d", g.leftCol+1, max(lastVis+1, g.leftCol+1), recs)
	}
	if g.leftCol > 0 {
		pos = " ◀" + pos
	}
	if lastVis < recs-1 {
		pos += " ▶"
	}
	x = strip.Put(0, 0, pos, st.muted)
	x = strip.Put(x+2, 0, "· transposed (t)", st.accent)
	if n := g.HiddenCount(); n > 0 {
		x = strip.Put(x+2, 0, "· "+itoa(n)+" hidden", st.accent)
	}
	if g.sel {
		strip.Put(x+2, 0, fmt.Sprintf("· %s × %s selected", plural(r1-r0+1, "row"), plural(c1-c0+1, "column")), st.accent)
	}
	if g.sortCol >= 0 {
		dir := "asc"
		if g.sortDesc {
			dir = "desc"
		}
		strip.PutRight(strip.W()-1, 0, "sorted by "+g.res.Columns[g.sortCol]+" "+dir, st.muted)
	}
	g.hbar = strip.Rect()
}

// selFirst is the 1-based display number of the first row a copy of the
// selection (or, with whole, of the result) holds — what a transposed copy
// heads its first record with, so "row 17" in the copy is row 17 in the
// grid.
func (g *grid) selFirst(whole bool) int {
	if whole || g.Rows() == 0 {
		return 1
	}
	r0, _, _, _ := g.bounds()
	return r0 + 1
}

// ---------------------------------------------------------------------------
// The Model's side
// ---------------------------------------------------------------------------

// transposeGrid is t (and the grid menu's row): turn the grid and say so,
// since copies and exports change shape with it.
func (m *Model) transposeGrid() tea.Cmd {
	m.grid.Transpose()
	if m.grid.flip {
		m.log(logInfo, "transposed — each row is a column now; copies and exports come out the same way (t turns it back)")
	} else {
		m.log(logInfo, "upright again — rows across")
	}
	return nil
}

// copyPiece copies a piece of the grid — r, an upright slice in display
// order, whose first row is display row first (1-based) — the way the grid
// shows it: upright as it is, transposed turned on its side. That is dbc
// web's rule (web/grid.go handleCopy): the plain copy (y, Y) carries the
// values in the grid's orientation with no names, and every formatted copy
// is export.Transpose's — names down the left, a record per column, a
// single record as "column | value".
func (m *Model) copyPiece(r *model.Result, what string, first int, f copyFormat) tea.Cmd {
	if r == nil {
		m.log(logWarn, noResult)
		return nil
	}
	if !m.grid.flip {
		return m.copyResult(r, what, f)
	}
	if f == copyText {
		return clipCmd(clip.Content{Text: export.PlainCellsTransposed(r)}, what+", transposed")
	}
	return m.copyResult(export.Transpose(r, first), what, f)
}

// gridView is what an export takes: the whole result in display order,
// hidden columns out — and, transposed, on its side, as the grid shows it.
func (m *Model) gridView() (*model.Result, string) {
	r, what := m.grid.Selected(true)
	if r != nil && m.grid.flip {
		r, what = export.Transpose(r, 1), strings.TrimSuffix(what, ")")+", transposed)"
	}
	return r, what
}
