package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/workspace"
)

// grid is the results table.
//
// It is VIRTUALIZED: only the rows and columns that fit are drawn each frame,
// so a 50,000-row result costs the same to draw as a 50-row one. (The former
// tview table built a widget per cell up front, which is why max_display_rows
// exists; the cap is still honored, so the setting means what it always has.)
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
//
// A "col" is likewise a DISPLAY column — an index into cols, the result's
// columns minus the hidden ones — for the same reason rows go through
// order: the cursor, a range, a click and a copy all mean what is on
// screen, and hiding a column is one rebuild of cols rather than a skip
// test in every walk. Per-column facts that belong to the data (widths,
// numeric, the sort column, hidden) stay indexed by RESULT column, so they
// survive a column being hidden and shown again.
//
//	result cols   0    1    2    3    4        hidden = {1, 3}
//	cols        [ 0,        2,        4 ]      display col 1 → result col 2
type grid struct {
	res   *model.Result
	order []int // display row → result row
	shown int   // display cap (max_display_rows); rows past it are not drawn

	sortCol  int // result column, -1 = result order
	sortDesc bool

	cols   []int  // display column → result column (the visible ones, in order)
	hidden []bool // per result column

	widths  []int  // per result column, auto-sized, in cells, content only (no padding)
	userW   []int  // per result column, a width set by hand (drag, keys); 0 = auto
	numeric []bool // per result column: right-align these

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
	hoverB int   // column whose right border is under the mouse, -1 when none

	// A border drag in progress: the display column being resized and its
	// left edge at the press. The edge is fixed for the whole drag — only
	// the column's own width changes, and leftCol does not move — so the
	// new width is simply pointer x minus that edge.
	resizeCol, resizeX int
}

// cell2 is a grid position: display row and column index.
type cell2 struct{ row, col int }

// maxColWidth caps a column's width so one long TEXT value cannot push every
// other column off screen; the full value is a double-click away, and a
// double-click on the header border (Fit) widens the column past the cap.
const maxColWidth = 40

// minColWidth is the narrowest a column can be dragged: enough for two
// characters and the truncation ellipsis, so a column never vanishes by
// resizing — hiding is the way to make one go away, and it says so.
const minColWidth = 3

// maxUserWidth bounds a width set by hand. It is far past maxColWidth on
// purpose — widening a column to read a long value in place is the point of
// dragging — but finite, so a runaway drag cannot build absurd layouts.
const maxUserWidth = 400

// widthSample bounds how many rows are measured to size the columns. Sizing
// by the first few hundred is indistinguishable in practice and keeps a huge
// result from costing a full scan on arrival.
const widthSample = 500

func newGrid() *grid {
	return &grid{sortCol: -1, hover: cell2{-1, -1}, hoverH: -1, hoverB: -1}
}

// SetResult installs a new result and resets the view onto it.
//
// Hidden columns and hand-set widths are KEPT when the new result has
// exactly the same columns (names and order) as the one it replaces. The
// common loop is edit-the-WHERE-and-re-run, and losing the layout on every
// run would make hiding and resizing not worth doing. Any other result —
// a different query, a table preview — starts fresh.
func (g *grid) SetResult(r *model.Result, displayCap int) {
	keep := r != nil && g.res != nil && slices.Equal(g.res.Columns, r.Columns) &&
		len(g.hidden) == len(r.Columns) && len(g.userW) == len(r.Columns)
	g.res = r
	g.sortCol, g.sortDesc = -1, false
	g.cur, g.anc, g.sel = cell2{}, cell2{}, false
	g.top, g.leftCol = 0, 0
	if r == nil {
		g.order, g.widths, g.numeric, g.cols, g.hidden, g.userW = nil, nil, nil, nil, nil, nil
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
	for c := range r.Columns {
		g.widths[c] = min(g.contentWidth(c), maxColWidth)
	}
	if !keep {
		g.hidden = make([]bool, len(r.Columns))
		g.userW = make([]int, len(r.Columns))
	}
	// cols still maps the OLD result; cleared, rebuildCols has no stale
	// result column to carry the (reset) cursor onto
	g.cols = g.cols[:0]
	g.rebuildCols()
}

// contentWidth measures result column c: its header (plus room for the sort
// arrow) and the first widthSample values, uncapped. Auto-sizing caps it;
// a double-click on the border (fit) does not.
func (g *grid) contentWidth(c int) int {
	w := width(g.res.Columns[c]) + 2 // room for the sort arrow
	for i := 0; i < min(len(g.res.Rows), widthSample); i++ {
		if c < len(g.res.Rows[i]) {
			w = max(w, width(flatten(g.res.Rows[i][c])))
		}
	}
	return max(minColWidth, w)
}

// colWidth is result column rc's content width as drawn: the hand-set
// width when there is one, else the auto size.
func (g *grid) colWidth(rc int) int {
	if rc < len(g.userW) && g.userW[rc] > 0 {
		return g.userW[rc]
	}
	return g.widths[rc]
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

// Cols is how many columns are shown — the result's, less the hidden ones.
func (g *grid) Cols() int {
	if g.res == nil {
		return 0
	}
	return len(g.cols)
}

// resultCol maps a display column to its result column, or -1.
func (g *grid) resultCol(col int) int {
	if col < 0 || col >= len(g.cols) {
		return -1
	}
	return g.cols[col]
}

// colName is the header of a display column.
func (g *grid) colName(col int) string {
	if rc := g.resultCol(col); rc >= 0 {
		return g.res.Columns[rc]
	}
	return ""
}

// value returns the display string and raw value at a display position.
func (g *grid) value(row, col int) (string, any, bool) {
	rc := g.resultCol(col)
	if g.res == nil || row < 0 || row >= len(g.order) || rc < 0 {
		return "", nil, false
	}
	ri := g.order[row]
	var raw any
	if ri < len(g.res.Raw) && rc < len(g.res.Raw[ri]) {
		raw = g.res.Raw[ri][rc]
	}
	return g.res.Rows[ri][rc], raw, true
}

// isNull reports whether the value at a display position is a real SQL NULL.
func (g *grid) isNull(row, col int) bool {
	rc := g.resultCol(col)
	if g.res == nil || row >= len(g.order) || rc < 0 {
		return false
	}
	ri := g.order[row]
	return ri < len(g.res.Raw) && rc < len(g.res.Raw[ri]) && g.res.Raw[ri][rc] == nil
}

// ---------------------------------------------------------------------------
// Sorting
// ---------------------------------------------------------------------------

// Sort cycles a (display) column through ascending → descending → result
// order, the three states a header click steps through. The sort is kept by
// result column, so hiding the sorted column leaves the rows as they are.
func (g *grid) Sort(col int) {
	if g.res == nil || g.resultCol(col) < 0 {
		return
	}
	col = g.resultCol(col)
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

// applySort rebuilds order from the sort state. The comparison — NULLs
// last in both directions, numbers as numbers, text case-insensitively — is
// workspace.SortRows, shared with dbc web so a sorted copy puts the same
// rows in the same order from either UI.
func (g *grid) applySort() {
	workspace.SortRows(g.order, g.res, g.sortCol, g.sortDesc, g.numeric)
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
//
// The rectangle is in display columns, so a hidden column inside it is left
// out: a copy takes what the user sees. That is what makes hiding useful
// for sharing — hide the noisy columns, then copy the table for Teams.
func (g *grid) sub(r0, c0, r1, c1 int) *model.Result {
	r1 = min(r1, len(g.order)-1)
	var rows []int
	if r0 <= r1 {
		rows = g.order[r0 : r1+1]
	}
	return workspace.Project(g.res, rows, g.cols[c0:c1+1])
}

// Selected returns what a copy should take: the range selection, or with
// whole set the entire result in display order. what names it for the log.
func (g *grid) Selected(whole bool) (r *model.Result, what string) {
	if g.res == nil || g.Cols() == 0 {
		return nil, ""
	}
	if whole || g.Rows() == 0 {
		what = fmt.Sprintf("the result (%d rows)", len(g.order))
		if n := g.HiddenCount(); n > 0 {
			// said out loud, so a shared table missing a column is never a
			// surprise to the person who hid it last week
			what = fmt.Sprintf("the result (%d rows, %s hidden)", len(g.order), plural(n, "column"))
		}
		return g.sub(0, 0, max(len(g.order)-1, 0), g.Cols()-1), what
	}
	r0, c0, r1, c1 := g.bounds()
	rows, cols := r1-r0+1, c1-c0+1
	switch {
	case rows == 1 && cols == 1:
		return g.sub(r0, c0, r1, c1), fmt.Sprintf("%s of row %d", g.colName(c0), r0+1)
	default:
		return g.sub(r0, c0, r1, c1), fmt.Sprintf("%d×%d cells", rows, cols)
	}
}

// RowResult returns the cursor's whole row.
func (g *grid) RowResult() (*model.Result, string) {
	if g.res == nil || g.Rows() == 0 {
		return nil, ""
	}
	return g.sub(g.cur.row, 0, g.cur.row, g.Cols()-1), fmt.Sprintf("row %d", g.cur.row+1)
}

// ---------------------------------------------------------------------------
// Hiding and resizing columns
// ---------------------------------------------------------------------------

// rebuildCols recomputes cols from hidden, keeping the cursor, the range's
// anchor and the view's left edge on the same RESULT columns they were on —
// display indices shift when a column before them comes or goes. One that
// was itself hidden moves to the next visible column to its right (or the
// last one), the way deleting a spreadsheet column moves the cursor.
func (g *grid) rebuildCols() {
	curRC, ancRC, leftRC := g.resultCol(g.cur.col), g.resultCol(g.anc.col), g.resultCol(g.leftCol)
	g.cols = g.cols[:0]
	for c, h := range g.hidden {
		if !h {
			g.cols = append(g.cols, c)
		}
	}
	g.cur.col, g.anc.col = g.displayOf(curRC), g.displayOf(ancRC)
	// the cursor must not end up left of the view; its right side is put
	// right by the next ensureVisible, which needs a fresh draw's geometry
	g.leftCol = min(g.displayOf(leftRC), g.cur.col)
	g.hover, g.hoverH, g.hoverB = cell2{-1, -1}, -1, -1
}

// displayOf is the display column showing result column rc, or the first
// visible one after it, or the last visible one.
func (g *grid) displayOf(rc int) int {
	if rc < 0 {
		return 0
	}
	for i, c := range g.cols {
		if c >= rc {
			return i
		}
	}
	return max(len(g.cols)-1, 0)
}

// HiddenCount is how many of the result's columns are hidden.
func (g *grid) HiddenCount() int {
	n := 0
	for _, h := range g.hidden {
		if h {
			n++
		}
	}
	return n
}

// HiddenCols lists the hidden result columns, in result order.
func (g *grid) HiddenCols() []int {
	var out []int
	for c, h := range g.hidden {
		if h {
			out = append(out, c)
		}
	}
	return out
}

// Hide hides display columns c0…c1 (inclusive) and returns how many it hid.
// It refuses — returning 0 — to hide every visible column: an empty grid
// with a result behind it reads as a failed query, and there would be no
// header left to right-click to get them back.
func (g *grid) Hide(c0, c1 int) int {
	c0, c1 = max(c0, 0), min(c1, g.Cols()-1)
	if g.res == nil || c0 > c1 || c1-c0+1 >= g.Cols() {
		return 0
	}
	for c := c0; c <= c1; c++ {
		g.hidden[g.cols[c]] = true
	}
	g.sel = false // the range spanned what just vanished; a shrunken one would mislead
	g.rebuildCols()
	return c1 - c0 + 1
}

// Show makes result column rc visible again and puts the cursor on it, so
// the user sees where it came back.
func (g *grid) Show(rc int) {
	if rc < 0 || rc >= len(g.hidden) || !g.hidden[rc] {
		return
	}
	g.hidden[rc] = false
	g.rebuildCols()
	g.cur.col, g.sel = g.displayOf(rc), false
	g.ensureVisible()
}

// ShowAll makes every column visible and returns how many were hidden.
func (g *grid) ShowAll() int {
	n := g.HiddenCount()
	if n == 0 {
		return 0
	}
	clear(g.hidden)
	g.rebuildCols()
	return n
}

// Resize adds delta cells to display column col's width.
func (g *grid) Resize(col, delta int) {
	if rc := g.resultCol(col); rc >= 0 {
		g.setWidth(rc, g.colWidth(rc)+delta)
	}
}

// Fit sizes display column col to its content — the double-click on a
// header border. Unlike auto-sizing it is not capped at maxColWidth: asking
// to see a column whole is exactly when the cap is in the way.
func (g *grid) Fit(col int) {
	if rc := g.resultCol(col); rc >= 0 {
		g.setWidth(rc, g.contentWidth(rc))
	}
}

func (g *grid) setWidth(rc, w int) {
	g.userW[rc] = max(minColWidth, min(w, maxUserWidth))
}

// startResize begins a border drag on display column col.
func (g *grid) startResize(col int) {
	g.resizeCol, g.resizeX = col, -1
	if i := col - g.leftCol; i >= 0 && i < len(g.colX) {
		g.resizeX = g.colX[i]
	}
}

// resizeTo continues a border drag: the border follows the pointer. A
// column is drawn as its content width plus a cell of padding on each side,
// with its border in the cell after that, so a border at x means a content
// width of x − left edge − 2.
//
//	resizeX                 x
//	│ pad │ content … │ pad │ border
func (g *grid) resizeTo(x int) {
	rc := g.resultCol(g.resizeCol)
	if rc < 0 || g.resizeX < 0 {
		return
	}
	g.setWidth(rc, x-g.resizeX-2)
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
	hitBorder // a header's right border: drag to resize, double-click to fit
)

// hitAt resolves a canvas cell against the geometry of the last draw.
func (g *grid) hitAt(x, y int) gridHit {
	if g.res == nil {
		return gridHit{}
	}
	if g.vbar.Contains(x, y) {
		return gridHit{kind: hitVBar}
	}
	// A border is the separator cell just right of a column, which belongs
	// to no column's span — so it is tested first, and a header click a
	// cell to its left still sorts.
	if g.head.Contains(x, y) {
		for i, cx := range g.colX {
			if x == cx+g.colW[i] {
				return gridHit{kind: hitBorder, col: g.leftCol + i}
			}
		}
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
	for c := g.leftCol; c < len(g.cols) && x < g.view.X+g.view.W; c++ {
		w := g.colWidth(g.cols[c]) + 2
		g.colX = append(g.colX, x)
		g.colW = append(g.colW, w)
		x += w + 1 // + separator
	}

	// header band
	//
	// A border takes the accent while it is hovered (or dragged: motion
	// during a drag does not update hover, so the highlight stays on the
	// border being moved). Where hidden columns sit, the border is drawn
	// doubled — ║ — so a gap in the columns is visible where it is, not
	// only as a count in the strip.
	hs := s.Sub(Rect{0, 0, s.W() - barW, 1})
	hs.Fill(st.header)
	rule := st.header.WithFg(st.border.Fg)
	for i, cx := range g.colX {
		c := g.leftCol + i
		rc := g.cols[c]
		name := g.res.Columns[rc]
		arrow := ""
		if rc == g.sortCol {
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
		label := truncate(name, g.colWidth(rc)-width(arrow)) + arrow
		if g.numeric[rc] {
			cs.PutRight(cs.W()-1, 0, label, hst)
		} else {
			cs.Put(1, 0, label, hst)
		}
		sep, sst := "│", rule
		if g.hiddenAfter(c) {
			sep, sst = "║", st.header.WithFg(st.accent.Fg)
		}
		if c == g.hoverB {
			sep, sst = "┃", st.header.WithFg(st.accent.Fg).Bold()
		}
		s.Put(cx-area.X+g.colW[i], 0, sep, sst)
	}
	s.Put(0, 0, strings.Repeat(" ", gw-1), st.header)
	if len(g.cols) > 0 && g.cols[0] > 0 {
		s.Put(gw-1, 0, "║", st.header.WithFg(st.accent.Fg)) // hidden before the first
	} else {
		s.Put(gw-1, 0, "│", rule)
	}

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
			text := truncate(flatten(val), g.colWidth(g.cols[c]))
			if g.numeric[g.cols[c]] && !g.isNull(row, c) {
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
	if n := g.HiddenCount(); n > 0 {
		x = strip.Put(x+2, 0, "· "+itoa(n)+" hidden", st.accent)
	}
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

// hiddenAfter reports whether hidden columns sit between display column c
// and the next visible one (or the end).
func (g *grid) hiddenAfter(c int) bool {
	if c+1 < len(g.cols) {
		return g.cols[c+1]-g.cols[c] > 1
	}
	return g.cols[c] < len(g.hidden)-1
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
