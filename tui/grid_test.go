package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/model"
)

// pets is a small result with a numeric column, a text column, and a NULL.
func pets() *model.Result {
	return &model.Result{
		Conn: "demo", Query: "SELECT …",
		Columns: []string{"id", "name", "age"},
		Rows: [][]string{
			{"1", "Whiskers", "3"},
			{"2", "luna", "NULL"},
			{"10", "Bella", "12"},
		},
		Raw: [][]any{
			{int64(1), "Whiskers", int64(3)},
			{int64(2), "luna", nil},
			{int64(10), "Bella", int64(12)},
		},
	}
}

// drawGrid draws g into a w×h canvas so its hit-test geometry exists.
func drawGrid(g *grid, w, h int) *Canvas {
	c := NewCanvas(w, h, Style{})
	g.Draw(c.Sub(Rect{0, 0, w, h}), newStyles(themeDefault()), true, "empty")
	return c
}

// cellIndex is the screen column where s starts in a drawn line — not the
// byte index, which box-drawing characters (3 bytes, 1 cell) throw off.
func cellIndex(line, s string) int {
	i := strings.Index(line, s)
	if i < 0 {
		return -1
	}
	return width(line[:i])
}

func names(g *grid) string {
	var out []string
	for row := range g.Rows() {
		v, _, _ := g.value(row, 1)
		out = append(out, v)
	}
	return strings.Join(out, ",")
}

func TestGridSortCyclesAndComparesByType(t *testing.T) {
	g := newGrid()
	g.SetResult(pets(), 0)

	g.Sort(0) // numeric: 10 after 2, not between 1 and 2 as a string would
	if got := names(g); got != "Whiskers,luna,Bella" {
		t.Errorf("id asc = %s", got)
	}
	g.Sort(0)
	if got := names(g); got != "Bella,luna,Whiskers" {
		t.Errorf("id desc = %s", got)
	}
	g.Sort(0)
	if g.sortCol != -1 || names(g) != "Whiskers,luna,Bella" {
		t.Errorf("third click restores result order, got %s", names(g))
	}

	g.Sort(1) // text: case-insensitive
	if got := names(g); got != "Bella,luna,Whiskers" {
		t.Errorf("name asc = %s", got)
	}

	g.Sort(2) // NULLs last in both directions
	if got := names(g); got != "Whiskers,Bella,luna" {
		t.Errorf("age asc = %s", got)
	}
	g.Sort(2)
	if got := names(g); got != "Bella,Whiskers,luna" {
		t.Errorf("age desc = %s", got)
	}
}

// A copied range is a real Result in display order, with Raw carried along,
// so exporting it keeps NULLs and number alignment.
func TestGridSelectedRangeCarriesRaw(t *testing.T) {
	g := newGrid()
	g.SetResult(pets(), 0)
	g.Sort(1) // Bella, luna, Whiskers
	g.moveTo(0, 1, false)
	g.moveTo(1, 2, true)
	r, what := g.Selected(false)
	if what != "2×2 cells" {
		t.Errorf("what = %q", what)
	}
	if strings.Join(r.Columns, ",") != "name,age" {
		t.Errorf("columns = %v", r.Columns)
	}
	if r.Rows[0][0] != "Bella" || r.Rows[1][0] != "luna" || r.Raw[1][1] != nil {
		t.Errorf("rows = %v raw = %v", r.Rows, r.Raw)
	}

	whole, what := g.Selected(true)
	if len(whole.Rows) != 3 || whole.Rows[0][1] != "Bella" || !strings.Contains(what, "3 rows") {
		t.Errorf("whole result should be in display order: %v (%s)", whole.Rows, what)
	}
}

func TestGridDisplayCap(t *testing.T) {
	g := newGrid()
	g.SetResult(pets(), 2)
	if g.Rows() != 2 {
		t.Errorf("rows = %d, want the cap", g.Rows())
	}
	if whole, _ := g.Selected(true); len(whole.Rows) != 3 {
		t.Error("the cap bounds drawing, not what a whole-result copy takes")
	}
}

func TestGridHitTestMatchesTheDrawing(t *testing.T) {
	g := newGrid()
	g.SetResult(pets(), 0)
	c := drawGrid(g, 60, 8)

	x, y := cellIndex(c.Line(0), "name"), 0
	if h := g.hitAt(x, y); h.kind != hitHeader || h.col != 1 {
		t.Errorf("header hit at %d = %+v", x, h)
	}
	x, y = cellIndex(c.Line(3), "Bella"), 3
	if h := g.hitAt(x, y); h.kind != hitCell || h.row != 2 || h.col != 1 {
		t.Errorf("cell hit = %+v", h)
	}
	if h := g.hitAt(0, 2); h.kind != hitRowNum || h.row != 1 {
		t.Errorf("row number hit = %+v", h)
	}
	if h := g.hitAt(55, 5); h.kind != hitNone {
		t.Errorf("empty space should hit nothing, got %+v", h)
	}
}

func TestGridNullsAreDistinct(t *testing.T) {
	g := newGrid()
	g.SetResult(pets(), 0)
	c := drawGrid(g, 60, 8)
	st := newStyles(themeDefault())
	x := cellIndex(c.Line(2), "NULL")
	if c.StyleAt(x, 2).Attr&AttrItalic == 0 || c.StyleAt(x, 2).Fg != st.null.Fg {
		t.Error("a real NULL should be drawn muted and italic")
	}
}

func TestGridKeysMoveAndExtend(t *testing.T) {
	g := newGrid()
	g.SetResult(pets(), 0)
	drawGrid(g, 60, 8)
	g.HandleKey(keyMsg("down"))
	g.HandleKey(keyMsg("shift+right"))
	if !g.sel || g.cur != (cell2{1, 1}) || g.anc != (cell2{1, 0}) {
		t.Errorf("cur %v anc %v sel %v", g.cur, g.anc, g.sel)
	}
	g.HandleKey(keyMsg("esc"))
	if g.sel {
		t.Error("Esc clears the range")
	}
	g.HandleKey(tea.KeyPressMsg{Code: 'G', Text: "G"})
	if g.cur.row != 2 {
		t.Errorf("G goes to the last row, got %d", g.cur.row)
	}
}

// Wide results scroll by whole columns and say where the view is.
func TestGridHorizontalScroll(t *testing.T) {
	r := &model.Result{Columns: []string{"a", "b", "c", "d", "e", "f"}}
	row := []string{strings.Repeat("x", 30), strings.Repeat("y", 30), "c", "d", "e", "f"}
	r.Rows = [][]string{row}
	r.Raw = [][]any{{"", "", "", "", "", ""}}
	g := newGrid()
	g.SetResult(r, 0)
	c := drawGrid(g, 50, 6)
	if !strings.Contains(c.Text(), "▶") {
		t.Error("a clipped result should show there is more to the right")
	}
	g.moveTo(0, 5, false)
	c = drawGrid(g, 50, 6)
	if g.leftCol == 0 || !strings.Contains(c.Text(), "◀") {
		t.Errorf("moving to the last column should scroll: leftCol=%d\n%s", g.leftCol, c.Text())
	}
}

// Hiding works in display columns: the cursor, a copy and the header all see
// the remaining columns, while the sort — kept by result column — is not
// disturbed by the sorted column vanishing.
func TestGridHideShowRemapsColumns(t *testing.T) {
	g := newGrid()
	g.SetResult(pets(), 0)
	g.Sort(1)             // by name: Bella, luna, Whiskers
	g.moveTo(0, 2, false) // age
	if n := g.Hide(1, 1); n != 1 {
		t.Fatalf("hid %d", n)
	}
	if g.Cols() != 2 || g.colName(1) != "age" || g.cur.col != 1 {
		t.Errorf("cols = %v, cursor col %d (%s)", g.cols, g.cur.col, g.colName(g.cur.col))
	}
	if v, _, _ := g.value(0, 0); v != "10" {
		t.Errorf("sort by the hidden name column should hold: first id = %s", v)
	}
	r, what := g.Selected(true)
	if strings.Join(r.Columns, ",") != "id,age" || r.Raw[1][1] != nil {
		t.Errorf("a whole copy takes the visible columns: %v %v", r.Columns, r.Raw)
	}
	if !strings.Contains(what, "1 column hidden") {
		t.Errorf("what = %q", what)
	}
	c := drawGrid(g, 60, 8)
	if strings.Contains(c.Line(0), "name") || !strings.Contains(c.Line(0), "║") {
		t.Errorf("header should drop name and mark the gap: %q", c.Line(0))
	}
	if !strings.Contains(c.Text(), "1 hidden") {
		t.Errorf("the strip should count hidden columns:\n%s", c.Text())
	}

	g.Show(1)
	if g.Cols() != 3 || g.cur.col != 1 {
		t.Errorf("Show puts the cursor on the returned column: cols %v cur %d", g.cols, g.cur.col)
	}
}

// Every column cannot be hidden: nothing would be left to click to undo it.
func TestGridRefusesToHideEverything(t *testing.T) {
	g := newGrid()
	g.SetResult(pets(), 0)
	if g.Hide(0, 2) != 0 || g.Cols() != 3 {
		t.Error("hiding all columns should be refused")
	}
	g.Hide(0, 0)
	g.Hide(0, 0)
	if g.Hide(0, 0) != 0 || g.Cols() != 1 || g.colName(0) != "age" {
		t.Errorf("the last column must stay: %v", g.cols)
	}
	if g.ShowAll() != 2 || g.Cols() != 3 {
		t.Error("ShowAll brings them all back")
	}
}

// A range copy skips a hidden column inside it.
func TestGridRangeCopySkipsHidden(t *testing.T) {
	g := newGrid()
	g.SetResult(pets(), 0)
	g.Hide(1, 1)
	g.moveTo(0, 0, false)
	g.moveTo(1, 1, true)
	r, _ := g.Selected(false)
	if strings.Join(r.Columns, ",") != "id,age" || r.Rows[1][1] != "NULL" {
		t.Errorf("range = %v %v", r.Columns, r.Rows)
	}
}

// Keyboard resizing clamps; fit ignores the auto-size cap.
func TestGridResizeAndFit(t *testing.T) {
	long := strings.Repeat("z", 70)
	r := &model.Result{Columns: []string{"a", "b"}, Rows: [][]string{{"x", long}}, Raw: [][]any{{"x", long}}}
	g := newGrid()
	g.SetResult(r, 0)
	if g.colWidth(1) != maxColWidth {
		t.Fatalf("auto width = %d, want the cap", g.colWidth(1))
	}
	g.Fit(1)
	if g.colWidth(1) != 70 {
		t.Errorf("fit = %d, want the whole value", g.colWidth(1))
	}
	g.Resize(0, -50)
	if g.colWidth(0) != minColWidth {
		t.Errorf("narrowed to %d, want the floor", g.colWidth(0))
	}
	c := drawGrid(g, 120, 6)
	if !strings.Contains(c.Line(1), long) {
		t.Errorf("a fitted column shows its value whole: %q", c.Line(1))
	}
}

// Re-running a query with the same columns keeps hidden columns and hand-set
// widths; a result with different columns starts fresh.
func TestGridLayoutSurvivesARerun(t *testing.T) {
	g := newGrid()
	g.SetResult(pets(), 0)
	g.Hide(0, 0) // a leading hidden column is what a stale cols would mis-map
	g.Resize(0, 6)
	w := g.colWidth(0)
	g.SetResult(pets(), 0)
	if g.Cols() != 2 || g.colWidth(0) != w {
		t.Errorf("same columns: cols %v width %d, want 2 and %d", g.cols, g.colWidth(0), w)
	}
	g.SetResult(&model.Result{Columns: []string{"id", "name"}, Rows: [][]string{{"1", "a"}}, Raw: [][]any{{1, "a"}}}, 0)
	if g.Cols() != 2 || g.HiddenCount() != 0 || g.userW[0] != 0 || g.cur.col != 0 {
		t.Errorf("different columns reset the layout: cols %v cur %v", g.cols, g.cur)
	}
}
