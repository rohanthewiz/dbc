package tui

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
)

// grid is the results table.
//
// It is VIRTUALIZED: only the rows and columns that fit are drawn each frame,
// so a 50,000-row result costs the same to draw as a 50-row one. (The tview
// table built a widget per cell up front, which is why max_display_rows
// exists; the cap is still honored so the two UIs agree on what is shown.)
//
// COORDINATES. A "row" here is a DISPLAY row — an index into order, which is
// the sorted view of the result's rows. Sorting permutes order and nothing
// else, so the cursor, the selection, a copy, and a click all work in what
// the user sees, and the result itself is never reordered.
//
//	┌ gutter ┬─ col leftCol ─┬─ col leftCol+1 ─┬ …   ▲ ← vertical scrollbar
//	│   #    │ header        │ header          │     █
//	│   1    │ value         │ value           │     │
//	└────────┴───────────────┴─────────────────┴──── ▼
//
// Horizontal scrolling is by whole columns (leftCol), which keeps every
// visible column's left edge aligned to a cell boundary and makes the
// column under a click a simple walk of the drawn widths.
type grid struct {
	res   *model.Result
	order []int // display row → result row
	shown int   // display cap (max_display_rows); rows past it are not drawn

	sortCol  int // -1 = result order
	sortDesc bool

	widths  []int  // per column, in cells, content only (no padding)
	numeric []bool // right-align these

	cur, anc cell2 // cursor, and the other corner of a range selection
	sel      bool

	top, leftCol int

	// Geometry of the last draw, in canvas coordinates, for hit-testing.
	view   Rect  // the data rows
	head   Rect  // the header band
	gutter Rect  // row numbers
	colX   []int // canvas x where each visible column starts; index i is leftCol+i
	colW   []int // drawn width of each visible column (content + padding)
	vbar   Rect  // vertical scrollbar
	hbar   Rect  // horizontal position strip (bottom edge)
	visRow int   // how many data rows fit
	hover  cell2 // cell under the mouse, row -1 when none
	hoverH int   // header column under the mouse, -1 when none
}

// cell2 is a grid position: display row and column index.
type cell2 struct{ row, col int }

// maxColWidth caps a column's width so one long TEXT value cannot push every
// other column off screen; the full value is a double-click away.
const maxColWidth = 40

// widthSample bounds how many rows are measured to size the columns. Sizing
// by the first few hundred is indistinguishable in practice and keeps a huge
// result from costing a full scan on arrival.
const widthSample = 500

func newGrid() *grid {
	return &grid{sortCol: -1, hover: cell2{-1, -1}, hoverH: -1}
}

// SetResult installs a new result and resets the view onto it.
func (g *grid) SetResult(r *model.Result, displayCap int) {
	g.res = r
	g.sortCol, g.sortDesc = -1, false
	g.cur, g.anc, g.sel = cell2{}, cell2{}, false
	g.top, g.leftCol = 0, 0
	if r == nil {
		g.order, g.widths, g.numeric = nil, nil, nil
		return
	}
	n := len(r.Rows)
	g.shown = n
	if displayCap > 0 && displayCap < n {
		g.shown = displayCap
	}
	g.order = make([]int, n)
	for i := range g.order {
		g.order[i] = i
	}
	g.numeric = export.NumericColumns(r)
	g.widths = make([]int, len(r.Columns))
	for c, name := range r.Columns {
		w := width(name) + 2 // room for the sort arrow
		for i := 0; i < min(n, widthSample); i++ {
			if c < len(r.Rows[i]) {
				w = max(w, width(flatten(r.Rows[i][c])))
			}
		}
		g.widths[c] = max(3, min(w, maxColWidth))
	}
}

// flatten puts a value on one line for a cell: a newline would otherwise be
// drawn as the control-character dot and hide that the value continues.
func flatten(s string) string {
	if !strings.ContainsAny(s, "\n\r\t") {
		return s
	}
	return strings.NewReplacer("\r\n", "↵", "\n", "↵", "\r", "↵", "\t", " ").Replace(s)
}

// Rows is how many display rows exist (the display cap applied).
func (g *grid) Rows() int {
	if g.res == nil {
		return 0
	}
	return g.shown
}

// Cols is how many columns the result has.
func (g *grid) Cols() int {
	if g.res == nil {
		return 0
	}
	return len(g.res.Columns)
}

// value returns the display string and raw value at a display position.
func (g *grid) value(row, col int) (string, any, bool) {
	if g.res == nil || row < 0 || row >= len(g.order) || col < 0 || col >= len(g.res.Columns) {
		return "", nil, false
	}
	ri := g.order[row]
	var raw any
	if ri < len(g.res.Raw) && col < len(g.res.Raw[ri]) {
		raw = g.res.Raw[ri][col]
	}
	return g.res.Rows[ri][col], raw, true
}

// isNull reports whether the value at a display position is a real SQL NULL.
func (g *grid) isNull(row, col int) bool {
	if g.res == nil || row >= len(g.order) {
		return false
	}
	ri := g.order[row]
	return ri < len(g.res.Raw) && col < len(g.res.Raw[ri]) && g.res.Raw[ri][col] == nil
}

// ---------------------------------------------------------------------------
// Sorting
// ---------------------------------------------------------------------------

// Sort cycles a column through ascending → descending → result order, the
// three states a header click steps through.
func (g *grid) Sort(col int) {
	if g.res == nil || col < 0 || col >= len(g.res.Columns) {
		return
	}
	switch {
	case g.sortCol != col:
		g.sortCol, g.sortDesc = col, false
	case !g.sortDesc:
		g.sortDesc = true
	default:
		g.sortCol = -1
	}
	g.applySort()
}

// applySort rebuilds order from the sort state. NULLs sort last in both
// directions — "biggest first" should not start with a screen of nothing —
// and a numeric column compares numbers, so 10 sorts after 9.
func (g *grid) applySort() {
	for i := range g.order {
		g.order[i] = i
	}
	if g.sortCol < 0 {
		return
	}
	c, desc, num := g.sortCol, g.sortDesc, g.numeric[g.sortCol]
	raw := func(ri int) any {
		if ri < len(g.res.Raw) && c < len(g.res.Raw[ri]) {
			return g.res.Raw[ri][c]
		}
		return nil
	}
	slices.SortStableFunc(g.order, func(a, b int) int {
		ra, rb := raw(a), raw(b)
		switch {
		case ra == nil && rb == nil:
			return 0
		case ra == nil:
			return 1
		case rb == nil:
			return -1
		}
		var r int
		if num {
			r = cmp.Compare(toFloat(ra), toFloat(rb))
		} else {
			r = cmp.Compare(strings.ToLower(g.res.Rows[a][c]), strings.ToLower(g.res.Rows[b][c]))
		}
		if desc {
			r = -r
		}
		return r
	})
}

// toFloat converts a Go number to float64 for comparison. Only called on
// columns NumericColumns vouched for.
func toFloat(v any) float64 {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	}
	return 0
}

// ---------------------------------------------------------------------------
// Selection and copying
// ---------------------------------------------------------------------------

// bounds returns the selection rectangle (inclusive), or the cursor cell.
func (g *grid) bounds() (r0, c0, r1, c1 int) {
	if !g.sel {
		return g.cur.row, g.cur.col, g.cur.row, g.cur.col
	}
	return min(g.cur.row, g.anc.row), min(g.cur.col, g.anc.col),
		max(g.cur.row, g.anc.row), max(g.cur.col, g.anc.col)
}

// inSel reports whether a display cell is inside the range selection.
func (g *grid) inSel(row, col int) bool {
	if !g.sel {
		return false
	}
	r0, c0, r1, c1 := g.bounds()
	return row >= r0 && row <= r1 && col >= c0 && col <= c1
}

// sub builds a Result holding a rectangle of the grid, in display order, with
// Raw carried along — so a copied selection exports exactly like a whole
// result: NULLs stay NULLs, numbers stay right-aligned in HTML.
func (g *grid) sub(r0, c0, r1, c1 int) *model.Result {
	src := g.res
	out := &model.Result{Conn: src.Conn, Query: src.Query, Duration: src.Duration}
	out.Columns = append([]string(nil), src.Columns[c0:c1+1]...)
	for row := r0; row <= r1 && row < len(g.order); row++ {
		ri := g.order[row]
		out.Rows = append(out.Rows, append([]string(nil), src.Rows[ri][c0:c1+1]...))
		if ri < len(src.Raw) {
			out.Raw = append(out.Raw, append([]any(nil), src.Raw[ri][c0:c1+1]...))
		}
	}
	return out
}

// Selected returns what a copy should take: the range selection, or with
// whole set the entire result in display order. what names it for the log.
func (g *grid) Selected(whole bool) (r *model.Result, what string) {
	if g.res == nil || len(g.res.Columns) == 0 {
		return nil, ""
	}
	if whole || g.Rows() == 0 {
		return g.sub(0, 0, max(len(g.order)-1, 0), len(g.res.Columns)-1),
			fmt.Sprintf("the result (%d rows)", len(g.order))
	}
	r0, c0, r1, c1 := g.bounds()
	rows, cols := r1-r0+1, c1-c0+1
	switch {
	case rows == 1 && cols == 1:
		return g.sub(r0, c0, r1, c1), fmt.Sprintf("%s of row %d", g.res.Columns[c0], r0+1)
	default:
		return g.sub(r0, c0, r1, c1), fmt.Sprintf("%d×%d cells", rows, cols)
	}
}

// RowResult returns the cursor's whole row.
func (g *grid) RowResult() (*model.Result, string) {
	if g.res == nil || g.Rows() == 0 {
		return nil, ""
	}
	return g.sub(g.cur.row, 0, g.cur.row, len(g.res.Columns)-1), fmt.Sprintf("row %d", g.cur.row+1)
}

// ---------------------------------------------------------------------------
// Movement
// ---------------------------------------------------------------------------

// moveTo puts the cursor at (row, col), extending the selection with extend.
func (g *grid) moveTo(row, col int, extend bool) {
	if g.Rows() == 0 {
		return
	}
	row = max(0, min(row, g.Rows()-1))
	col = max(0, min(col, g.Cols()-1))
	if extend {
		if !g.sel {
			g.anc, g.sel = g.cur, true
		}
	} else {
		g.sel = false
	}
	g.cur = cell2{row, col}
	g.ensureVisible()
}

// ensureVisible scrolls so the cursor cell is on screen.
func (g *grid) ensureVisible() {
	if g.visRow > 0 {
		if g.cur.row < g.top {
			g.top = g.cur.row
		} else if g.cur.row >= g.top+g.visRow {
			g.top = g.cur.row - g.visRow + 1
		}
	}
	if g.cur.col < g.leftCol {
		g.leftCol = g.cur.col
	} else if n := len(g.colX); n > 0 && g.cur.col >= g.leftCol+n {
		// the column is past the last one drawn; step until it fits — a
		// few steps at most, since columns are capped in width
		g.leftCol = g.cur.col - max(n-1, 0)
	} else if n > 0 && g.cur.col == g.leftCol+n-1 && g.colPartial() {
		g.leftCol++
	}
}

// colPartial reports whether the last drawn column was clipped by the edge.
func (g *grid) colPartial() bool {
	n := len(g.colX)
	if n == 0 {
		return false
	}
	return g.colX[n-1]+g.colW[n-1] > g.view.X+g.view.W
}

// HandleKey moves the cursor and selection. It reports whether it used the key.
func (g *grid) HandleKey(k tea.KeyPressMsg) bool {
	if g.Rows() == 0 {
		return false
	}
	s := k.String()
	ext := strings.Contains(s, "shift")
	page := max(g.visRow-1, 1)
	switch strings.TrimPrefix(s, "shift+") {
	case "up", "k":
		g.moveTo(g.cur.row-1, g.cur.col, ext)
	case "down", "j":
		g.moveTo(g.cur.row+1, g.cur.col, ext)
	case "left", "h":
		g.moveTo(g.cur.row, g.cur.col-1, ext)
	case "right", "l":
		g.moveTo(g.cur.row, g.cur.col+1, ext)
	case "pgup":
		g.moveTo(g.cur.row-page, g.cur.col, ext)
	case "pgdown":
		g.moveTo(g.cur.row+page, g.cur.col, ext)
	case "home":
		g.moveTo(g.cur.row, 0, ext)
	case "end":
		g.moveTo(g.cur.row, g.Cols()-1, ext)
	case "ctrl+home", "g":
		g.moveTo(0, g.cur.col, ext)
	case "ctrl+end", "G":
		g.moveTo(g.Rows()-1, g.cur.col, ext)
	case "esc":
		if !g.sel {
			return false
		}
		g.sel = false
	default:
		return false
	}
	return true
}

// Scroll moves the view without moving the cursor — the wheel.
func (g *grid) Scroll(rows, cols int) {
	maxTop := max(g.Rows()-g.visRow, 0)
	g.top = max(0, min(g.top+rows, maxTop))
	g.leftCol = max(0, min(g.leftCol+cols, max(g.Cols()-1, 0)))
}

// ---------------------------------------------------------------------------
// Hit-testing
// ---------------------------------------------------------------------------

// gridHit is what a screen cell is, in grid terms.
type gridHit struct {
	kind hitKind
	row  int // display row, for hitCell/hitRowNum
	col  int // column, for hitCell/hitHeader
}

type hitKind int

const (
	hitNone hitKind = iota
	hitCell
	hitHeader
	hitRowNum
	hitVBar
)

// hitAt resolves a canvas cell against the geometry of the last draw.
func (g *grid) hitAt(x, y int) gridHit {
	if g.res == nil {
		return gridHit{}
	}
	if g.vbar.Contains(x, y) {
		return gridHit{kind: hitVBar}
	}
	col := -1
	for i, cx := range g.colX {
		if x >= cx && x < cx+g.colW[i] {
			col = g.leftCol + i
			break
		}
	}
	switch {
	case g.head.Contains(x, y) && col >= 0:
		return gridHit{kind: hitHeader, col: col}
	case g.gutter.Contains(x, y):
		row := g.top + (y - g.gutter.Y)
		if row < g.Rows() {
			return gridHit{kind: hitRowNum, row: row}
		}
	case g.view.Contains(x, y) && col >= 0:
		row := g.top + (y - g.view.Y)
		if row < g.Rows() {
			return gridHit{kind: hitCell, row: row, col: col}
		}
	}
	return gridHit{}
}

// dragTo extends the selection to the cell nearest (x, y), scrolling when the
// pointer leaves the data area — a drag past the bottom edge keeps selecting
// downward, as in a spreadsheet.
func (g *grid) dragTo(x, y int) {
	if g.Rows() == 0 {
		return
	}
	row, col := g.cur.row, g.cur.col
	switch {
	case y < g.view.Y:
		row = g.top - 1
	case y >= g.view.Y+g.view.H:
		row = g.top + g.visRow
	default:
		row = g.top + (y - g.view.Y)
	}
	switch {
	case x < g.view.X:
		col = g.leftCol - 1
	case len(g.colX) > 0 && x >= g.colX[len(g.colX)-1]+g.colW[len(g.colX)-1]:
		col = g.leftCol + len(g.colX)
	default:
		for i, cx := range g.colX {
			if x >= cx && x < cx+g.colW[i] {
				col = g.leftCol + i
			}
		}
	}
	g.moveTo(row, col, true)
}

// vbarJump scrolls so the thumb centers on y — a click or drag on the
// scrollbar track.
func (g *grid) vbarJump(y int) {
	if g.vbar.H <= 0 || g.Rows() <= g.visRow {
		return
	}
	frac := float64(y-g.vbar.Y) / float64(max(g.vbar.H-1, 1))
	frac = max(0, min(frac, 1))
	g.top = int(frac * float64(g.Rows()-g.visRow))
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

// Draw paints the grid into s (the inside of the Results pane).
func (g *grid) Draw(s Surface, st styles, focused bool, empty string) {
	s.Fill(st.base)
	g.colX, g.colW = g.colX[:0], g.colW[:0]
	g.view, g.head, g.gutter, g.vbar, g.hbar = Rect{}, Rect{}, Rect{}, Rect{}, Rect{}
	if g.res == nil || len(g.res.Columns) == 0 {
		msg := empty
		s.Put(max((s.W()-width(msg))/2, 1), s.H()/2, msg, st.muted)
		return
	}
	rows := g.Rows()
	gw := len(itoa(max(rows, 1))) + 2 // row numbers, a space, and a rule
	bottom := 1                       // the column-position strip
	barW := 0
	if rows > s.H()-1-bottom {
		barW = 1
	}
	area := s.Rect()
	g.visRow = max(s.H()-1-bottom, 0)
	g.top = max(0, min(g.top, max(rows-g.visRow, 0)))
	g.head = Rect{area.X + gw, area.Y, area.W - gw - barW, 1}
	g.gutter = Rect{area.X, area.Y + 1, gw, g.visRow}
	g.view = Rect{area.X + gw, area.Y + 1, area.W - gw - barW, g.visRow}

	// columns: walk from leftCol until the view is full
	x := g.view.X
	for c := g.leftCol; c < len(g.res.Columns) && x < g.view.X+g.view.W; c++ {
		w := g.widths[c] + 2
		g.colX = append(g.colX, x)
		g.colW = append(g.colW, w)
		x += w + 1 // + separator
	}

	// header band
	hs := s.Sub(Rect{0, 0, s.W() - barW, 1})
	hs.Fill(st.header)
	for i, cx := range g.colX {
		c := g.leftCol + i
		name := g.res.Columns[c]
		arrow := ""
		if c == g.sortCol {
			arrow = " ▲"
			if g.sortDesc {
				arrow = " ▼"
			}
		}
		hst := st.header
		if c == g.hoverH {
			hst = hst.Underline()
		}
		cs := s.Sub(Rect{cx - area.X, 0, g.colW[i], 1})
		label := truncate(name, g.widths[c]-width(arrow)) + arrow
		if g.numeric[c] {
			cs.PutRight(cs.W()-1, 0, label, hst)
		} else {
			cs.Put(1, 0, label, hst)
		}
		s.Put(cx-area.X+g.colW[i], 0, "│", st.header.WithFg(st.border.Fg))
	}
	s.Put(0, 0, strings.Repeat(" ", gw-1), st.header)
	s.Put(gw-1, 0, "│", st.header.WithFg(st.border.Fg))

	// rows
	r0, c0, r1, c1 := g.bounds()
	for y := 0; y < g.visRow; y++ {
		row := g.top + y
		if row >= rows {
			break
		}
		ns := st.muted
		if row == g.cur.row {
			ns = st.accent
		}
		s.PutRight(gw-2, y+1, itoa(row+1), ns)
		s.Put(gw-1, y+1, "│", st.border)
		for i, cx := range g.colX {
			c := g.leftCol + i
			val, _, _ := g.value(row, c)
			cst := st.base
			if g.isNull(row, c) {
				cst = st.null
			}
			switch {
			case focused && row == g.cur.row && c == g.cur.col:
				cst = cst.WithBg(st.sel.Bg).Bold()
			case g.sel && row >= r0 && row <= r1 && c >= c0 && c <= c1:
				cst = cst.WithBg(st.selRange.Bg)
			case row == g.cur.row:
				cst = cst.WithBg(st.hover.Bg) // the cursor's row, faintly
			case g.hover.row == row && g.hover.col == c:
				cst = cst.WithBg(st.hover.Bg)
			}
			cs := s.Sub(Rect{cx - area.X, y + 1, g.colW[i], 1})
			cs.Fill(cst)
			text := truncate(flatten(val), g.widths[c])
			if g.numeric[c] && !g.isNull(row, c) {
				cs.PutRight(cs.W()-1, 0, text, cst)
			} else {
				cs.Put(1, 0, text, cst)
			}
			s.Put(cx-area.X+g.colW[i], y+1, "│", st.border)
		}
	}

	// vertical scrollbar: a track with a thumb sized by the visible fraction
	if barW > 0 {
		g.vbar = Rect{area.X + area.W - 1, area.Y, 1, s.H() - bottom}
		drawVBar(s.Sub(Rect{s.W() - 1, 0, 1, s.H() - bottom}), st, g.top, g.visRow, rows)
	}

	// bottom strip: where the view sits horizontally, and the selection size
	strip := s.Sub(Rect{0, s.H() - 1, s.W(), 1})
	strip.Fill(st.muted)
	lastVis := g.leftCol + len(g.colX) - 1
	if g.colPartial() {
		lastVis--
	}
	pos := fmt.Sprintf(" cols %d–%d of %d", g.leftCol+1, max(lastVis+1, g.leftCol+1), g.Cols())
	if g.leftCol > 0 {
		pos = " ◀" + pos
	}
	if lastVis < g.Cols()-1 {
		pos += " ▶"
	}
	x = strip.Put(0, 0, pos, st.muted)
	if g.sel {
		strip.Put(x+2, 0, fmt.Sprintf("· %d×%d selected", r1-r0+1, c1-c0+1), st.accent)
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

// drawVBar draws a one-column scrollbar for a view of vis items out of total,
// scrolled to top.
func drawVBar(s Surface, st styles, top, vis, total int) {
	h := s.H()
	if h <= 0 || total <= 0 {
		return
	}
	for y := 0; y < h; y++ {
		s.Put(0, y, "│", st.scrollTrack)
	}
	thumb := max(1, h*vis/total)
	pos := 0
	if total > vis {
		pos = (h - thumb) * top / (total - vis)
	}
	for y := pos; y < pos+thumb && y < h; y++ {
		s.Put(0, y, "┃", st.scrollThumb)
	}
}

// cursorScreen is the canvas position of the cursor cell as last drawn — what
// a context menu opened from the keyboard anchors to, so it appears where the
// user is looking rather than at a corner.
func (g *grid) cursorScreen() (int, int) {
	i := g.cur.col - g.leftCol
	if i < 0 || i >= len(g.colX) || g.cur.row < g.top || g.cur.row >= g.top+g.visRow {
		return g.view.X, g.view.Y
	}
	return g.colX[i] + 1, g.view.Y + (g.cur.row - g.top)
}
