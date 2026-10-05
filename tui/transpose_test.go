package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/model"
)

// flipped is pets() drawn on its side.
func flipped(t *testing.T, w, h int) (*grid, *Canvas) {
	t.Helper()
	g := newGrid()
	g.SetResult(pets(), 0)
	g.Transpose()
	return g, drawGrid(g, w, h)
}

// Transposed, each record is a column headed by its number and each result
// column a line led by its name.
func TestTransposeDrawsOnItsSide(t *testing.T) {
	_, c := flipped(t, 60, 8)
	head := c.Line(0)
	if !strings.HasPrefix(strings.TrimSpace(head), "column") {
		t.Errorf("the names column heads the band: %q", head)
	}
	for _, n := range []string{"1", "2", "3"} {
		if cellIndex(head, " "+n+" ") < 0 {
			t.Errorf("record %s has no header: %q", n, head)
		}
	}
	for i, want := range [][]string{{"id", "1", "2", "10"}, {"name", "Whiskers", "luna", "Bella"}, {"age", "3", "NULL", "12"}} {
		line := c.Line(i + 1)
		at := -1
		for _, v := range want {
			j := cellIndex(line, v)
			if j <= at {
				t.Errorf("line %d: %q missing or out of order in %q", i+1, v, line)
			}
			at = j
		}
	}
	if !strings.Contains(c.Line(7), "transposed") || !strings.Contains(c.Line(7), "rows 1–3 of 3") {
		t.Errorf("strip = %q", c.Line(7))
	}
}

// Every record shares one width, so a field lines up across records.
func TestTransposeRecordsShareOneWidth(t *testing.T) {
	g, c := flipped(t, 80, 8)
	if len(g.colW) != 3 || g.colW[0] != g.colW[1] || g.colW[1] != g.colW[2] {
		t.Fatalf("record widths = %v", g.colW)
	}
	if g.recWidth() != len("Whiskers") {
		t.Errorf("auto record width = %d, want the widest value", g.recWidth())
	}
	// "luna" sits at the start of record 2's span
	if x := cellIndex(c.Line(2), "luna"); x != g.colX[1]+1 {
		t.Errorf("luna at %d, record 2 starts at %d", x, g.colX[1])
	}
}

// Hit-testing follows the picture: a value is a cell of (record, column),
// a name sorts, a record's number selects it.
func TestTransposeHitTestMatchesTheDrawing(t *testing.T) {
	g, c := flipped(t, 60, 8)
	if h := g.hitAt(cellIndex(c.Line(2), "Bella"), 2); h.kind != hitCell || h.row != 2 || h.col != 1 {
		t.Errorf("Bella hit = %+v, want record 2, column 1", h)
	}
	if h := g.hitAt(cellIndex(c.Line(3), "age"), 3); h.kind != hitHeader || h.col != 2 {
		t.Errorf("name hit = %+v, want the age line's header", h)
	}
	if h := g.hitAt(g.colX[1]+1, 0); h.kind != hitRowNum || h.row != 1 {
		t.Errorf("record header hit = %+v, want record 1", h)
	}
	if h := g.hitAt(g.colX[0]+g.colW[0], 0); h.kind != hitBorder || h.col != 0 {
		t.Errorf("record border hit = %+v", h)
	}
	if h := g.hitAt(g.gutter.X+g.gutter.W-1, 0); h.kind != hitNameBorder {
		t.Errorf("names border hit = %+v", h)
	}
}

// The arrows follow the screen: down is the next column, right the next row.
func TestTransposeKeysFollowTheScreen(t *testing.T) {
	g, _ := flipped(t, 60, 8)
	g.HandleKey(keyMsg("down"))
	if g.cur != (cell2{0, 1}) {
		t.Errorf("down → %+v, want the next column", g.cur)
	}
	g.HandleKey(keyMsg("right"))
	if g.cur != (cell2{1, 1}) {
		t.Errorf("right → %+v, want the next row", g.cur)
	}
	g.HandleKey(keyMsg("end"))
	if g.cur != (cell2{2, 1}) {
		t.Errorf("end → %+v, want the last record", g.cur)
	}
	g.HandleKey(keyMsg("shift+up"))
	if r0, c0, r1, c1 := g.bounds(); !g.sel || r0 != 2 || r1 != 2 || c0 != 0 || c1 != 1 {
		t.Errorf("shift+up range = %d,%d–%d,%d", r0, c0, r1, c1)
	}
}

// Sorting and hiding work as upright: the records reorder, a hidden column
// is a line gone, and the view's top line stays on its column.
func TestTransposeSortAndHide(t *testing.T) {
	g, _ := flipped(t, 60, 8)
	g.Sort(1) // by name
	if got := names(g); got != "Bella,luna,Whiskers" {
		t.Errorf("sorted records = %s", got)
	}
	g.Hide(0, 0)
	c := drawGrid(g, 60, 8)
	if strings.HasPrefix(strings.TrimSpace(c.Line(1)), "id") || !strings.HasPrefix(strings.TrimSpace(c.Line(1)), "name") {
		t.Errorf("first line after hiding id = %q", c.Line(1))
	}
	if !strings.Contains(c.Line(0), "║") {
		t.Errorf("a hidden column before the first line is marked: %q", c.Line(0))
	}
}

// Dragging a record's border keeps it under the pointer, though widening
// it widens every record to its left too.
func TestTransposeRecordBorderTracksThePointer(t *testing.T) {
	g, _ := flipped(t, 120, 8)
	g.startResize(1)
	x := g.colX[1] + g.colW[1] + 8 // eight cells right of record 2's border
	g.resizeTo(x)
	drawGrid(g, 120, 8)
	if border := g.colX[1] + g.colW[1]; border != x {
		t.Errorf("border at %d, pointer at %d (record width %d)", border, x, g.recW)
	}
	// Record 2's border moves two cells per cell of width (record 1 widens
	// too), so it can land only on every other cell: within one of the
	// pointer, never past it.
	g.startResize(1)
	x = g.colX[1] + g.colW[1] - 3
	g.resizeTo(x)
	drawGrid(g, 120, 8)
	if border := g.colX[1] + g.colW[1]; border > x || x-border > 1 {
		t.Errorf("border at %d, pointer at %d", border, x)
	}
	// the names border is the plain case
	g.startNamesResize()
	g.resizeTo(g.gutter.X + 20)
	drawGrid(g, 120, 8)
	if g.gutter.X+g.gutter.W-1 != 20 {
		t.Errorf("names border at %d, want 20", g.gutter.X+g.gutter.W-1)
	}
	// = (fit) sizes the records to the widest value, < > step them all
	g.Fit(0)
	if g.recW != len("Whiskers") {
		t.Errorf("fit = %d", g.recW)
	}
	g.Resize(0, 2)
	if g.recWidth() != len("Whiskers")+2 {
		t.Errorf("> = %d", g.recWidth())
	}
}

// The orientation belongs to the grid, not to one result: a new result
// arrives on its side, and the hand-set widths go with the old columns.
func TestTransposeSurvivesANewResult(t *testing.T) {
	g, _ := flipped(t, 60, 8)
	g.recW = 20
	g.SetResult(pets(), 0)
	if !g.flip || g.recW != 20 {
		t.Errorf("same columns: flip %v, recW %d", g.flip, g.recW)
	}
	g.SetResult(&model.Result{Columns: []string{"x"}, Rows: [][]string{{"1"}}, Raw: [][]any{{int64(1)}}}, 0)
	if !g.flip || g.recW != 0 {
		t.Errorf("other columns: flip %v, recW %d (want it kept on its side, widths fresh)", g.flip, g.recW)
	}
}

// t turns the grid; the copies and the export come out the way it shows.
func TestTransposeCopiesAndExportFollowTheGrid(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	m.focus = focusGrid
	key(t, m, "t")
	if !m.grid.flip {
		t.Fatal("t did not transpose")
	}
	if c := frame(m).Text(); !strings.Contains(c, "transposed (t)") {
		t.Errorf("the strip does not say transposed:\n%s", c)
	}

	// y on one cell copies the value, as upright
	key(t, m, "y")
	if got := lastClip(t).Text; got != m.grid.res.Rows[m.grid.order[0]][0] {
		t.Errorf("y = %q", got)
	}

	// Y: the row's values, one line per column (the grid's orientation)
	key(t, m, "Y")
	if got := lastClip(t).Text; strings.Count(got, "\n") != 4 || strings.Contains(got, "\t") {
		t.Errorf("Y = %q, want five lines of one value each", got)
	}
	if !strings.Contains(logText(m), "row 1, transposed") {
		t.Errorf("log does not say transposed:\n%s", logText(m))
	}

	// the whole result as a Teams table: names down the left
	drive(t, m, nil, m.copyGrid(copyHTML, true))
	html := lastClip(t).HTML
	if !strings.Contains(html, "row 1") || !strings.Contains(html, ">breed<") {
		t.Errorf("HTML copy is not transposed: %s", html)
	}

	// a single record copies as "column | value"
	key(t, m, "right") // record 2 — and a selection of just it
	drive(t, m, nil, m.copyRow(copyMarkdown))
	md := lastClip(t).Text
	if !strings.Contains(md, "column") || !strings.Contains(md, "value") || strings.Contains(md, "row 2") {
		t.Errorf("one record as Markdown = %q", md)
	}

	// the export dialog writes the transposed result
	key(t, m, "ctrl+e")
	path := filepath.Join(t.TempDir(), "out.csv")
	typeText(t, m, path)
	key(t, m, "enter")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("export: %v\nlog: %s", err, logText(m))
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if !strings.HasPrefix(lines[0], "column,row 1,row 2") || !strings.HasPrefix(lines[1], "id,") || len(lines) != 6 {
		t.Errorf("transposed export = %q", b)
	}

	// t again: upright, and the copies with it
	m.focus = focusGrid
	key(t, m, "t")
	drive(t, m, nil, m.copyGrid(copyCSV, true))
	if got := lastClip(t).Text; !strings.HasPrefix(got, "id,name,breed,age,adopted") {
		t.Errorf("upright CSV = %q", got)
	}
}

// The grid menu offers the turn, and says the copies are transposed.
func TestTransposeInTheGridMenu(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	m.focus = focusGrid
	m.openGridMenu(10, 10)
	if !menuHas(m, "Transpose (each row a column)") {
		t.Fatal("no Transpose row in the grid menu")
	}
	key(t, m, "esc")
	key(t, m, "t")
	m.openGridMenu(10, 10)
	if !menuHas(m, "Turn upright (rows across)") || !menuHas(m, "copy whole result, transposed, as") {
		t.Error("the transposed grid menu should offer Turn upright and say the copies are transposed")
	}
}

func menuHas(m *Model, label string) bool {
	if m.menu == nil {
		return false
	}
	for _, it := range m.menu.items {
		if strings.TrimSpace(it.label) == label {
			return true
		}
	}
	return false
}
