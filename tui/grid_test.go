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
