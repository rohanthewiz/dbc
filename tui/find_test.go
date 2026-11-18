package tui

import (
	"strings"
	"testing"
)

// The grid's / finds a cell by its text, any case, and n / N step through
// the matches in reading order, wrapping; Esc closes the line and leaves
// the cursor on the match (N-292).
func TestGridFind(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	m.focus = focusGrid
	g := m.grid
	key(t, m, "/")
	if m.gfind == nil || !m.gfind.editing {
		t.Fatal("/ should open the grid's find line")
	}
	typeText(t, m, "tabby")
	r, c := g.cur.row, g.cur.col
	if text, _, _ := g.value(r, c); !strings.EqualFold(text, "Tabby") {
		t.Fatalf("cursor on %q (%d,%d), want a Tabby", text, r, c)
	}
	if !strings.Contains(frame(m).Text(), "⌕ tabby") {
		t.Errorf("the find line is not drawn:\n%s", frame(m).Text())
	}
	key(t, m, "enter") // the next Tabby
	if g.cur.row <= r {
		t.Errorf("Enter should move to a later row, at %d (was %d)", g.cur.row, r)
	}
	second := g.cur.row
	key(t, m, "shift+enter")
	if g.cur.row != r {
		t.Errorf("⇧Enter should go back to row %d, at %d", r, g.cur.row)
	}
	key(t, m, "esc")
	if m.gfind != nil || g.cur.row != r {
		t.Errorf("Esc: line %v, cursor row %d (want closed, %d)", m.gfind, g.cur.row, r)
	}

	// off the line, n and N repeat the last find: open it, keep its text
	key(t, m, "/")
	typeText(t, m, "tabby")
	key(t, m, "tab") // leave the line, the find kept
	if m.gfind == nil || m.gfind.editing {
		t.Fatal("Tab should leave the line and keep the find")
	}
	m.focus = focusGrid
	key(t, m, "n")
	if g.cur.row != second {
		t.Errorf("n: row %d, want %d", g.cur.row, second)
	}

	// a miss moves nothing and says so
	key(t, m, "/")
	for range len("tabby") {
		key(t, m, "backspace")
	}
	typeText(t, m, "zzqq")
	if !m.gfind.miss || g.cur.row != second {
		t.Errorf("miss: %v, row %d", m.gfind.miss, g.cur.row)
	}
	if !strings.Contains(frame(m).Text(), "no match") {
		t.Error("a miss should say so on the line")
	}
}

// A run with the line open still runs: Ctrl+R is not the line's.
func TestFindLinePassesChords(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	key(t, m, "ctrl+f")
	if m.efind == nil || !m.efind.editing {
		t.Fatal("Ctrl+F should open the editor's find line")
	}
	key(t, m, "ctrl+r")
	if m.ws.LastResult() == nil {
		t.Error("Ctrl+R should still run with the find line open")
	}
}

// The editor's Ctrl+F selects each match in turn, and ":N" goes to line N.
func TestEditorFind(t *testing.T) {
	m := newTestModel(t)
	m.editor.SetText("select 1 FROM a;\nselect 2 from b;\nselect 3 From c;")
	m.focus = focusEditor
	key(t, m, "ctrl+f")
	typeText(t, m, "from")
	sel, start, _ := m.editor.Selection()
	if sel != "FROM" || start != 9 {
		t.Fatalf("first match = %q at %d", sel, start)
	}
	key(t, m, "enter")
	if sel, _, _ := m.editor.Selection(); sel != "from" {
		t.Errorf("second = %q", sel)
	}
	key(t, m, "enter")
	if sel, _, _ := m.editor.Selection(); sel != "From" {
		t.Errorf("third = %q", sel)
	}
	key(t, m, "enter") // wraps
	if sel, start, _ := m.editor.Selection(); sel != "FROM" || start != 9 || m.efind.note != "wrapped to the top" {
		t.Errorf("wrap: %q at %d, note %q", sel, start, m.efind.note)
	}
	key(t, m, "shift+enter") // back past the top: the last one
	if sel, _, _ := m.editor.Selection(); sel != "From" {
		t.Errorf("back wrap = %q", sel)
	}
	key(t, m, "esc")
	if m.efind != nil {
		t.Error("Esc should close the line")
	}
	// typing replaces the found text, as a selection does
	typeText(t, m, "FROM")
	if !strings.Contains(m.editor.Text(), "select 3 FROM c;") {
		t.Errorf("typing over the match: %q", m.editor.Text())
	}

	key(t, m, "ctrl+f")
	for range 4 {
		key(t, m, "backspace")
	}
	typeText(t, m, ":2")
	key(t, m, "enter")
	if m.editor.cur.row != 1 || m.editor.cur.col != 0 || m.efind != nil {
		t.Errorf(":2 → caret %v, line open %v", m.editor.cur, m.efind != nil)
	}
}

// The log's / marks the line holding the text and scrolls to it.
func TestLogFind(t *testing.T) {
	m := newTestModel(t)
	for i := range 200 {
		m.logf(logInfo, "filler line %d", i)
	}
	m.log(logErr, "the needle is HERE")
	for i := range 200 {
		m.logf(logInfo, "more filler %d", i)
	}
	frame(m)
	m.focus = focusLog
	key(t, m, "/")
	typeText(t, m, "needle")
	lp := m.logp
	if lp.mark < 0 || !strings.Contains(lp.lines[lp.mark].text, "needle") {
		t.Fatalf("mark = %d", lp.mark)
	}
	if lp.follow {
		t.Error("the view should leave the bottom to show the match")
	}
	if !strings.Contains(frame(m).Text(), "the needle is HERE") {
		t.Error("the matched line should be on screen")
	}
	key(t, m, "esc")
	if m.lfind != nil || lp.mark != -1 {
		t.Errorf("Esc: line %v, mark %d", m.lfind != nil, lp.mark)
	}
}

// o sorts the grid by the cursor's column: ascending, descending, off.
func TestGridSortKey(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	m.focus = focusGrid
	g := m.grid
	g.moveTo(0, 1, false) // name
	key(t, m, "o")
	if g.sortCol != g.resultCol(1) || g.sortDesc {
		t.Fatalf("o: sortCol %d desc %v", g.sortCol, g.sortDesc)
	}
	first, _, _ := g.value(0, 1)
	key(t, m, "o")
	if !g.sortDesc {
		t.Error("o again: descending")
	}
	if last, _, _ := g.value(0, 1); strings.ToLower(last) < strings.ToLower(first) {
		t.Errorf("descending starts at %q, ascending at %q", last, first)
	}
	key(t, m, "o")
	if g.sortCol != -1 {
		t.Error("o a third time: the result's order")
	}
}

// A tab switch drops the finds: the grid, log and buffer they were on are
// parked.
func TestTabSwitchDropsFinds(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	key(t, m, "ctrl+f")
	key(t, m, "esc")
	key(t, m, "ctrl+f")
	typeText(t, m, "x")
	key(t, m, "alt+t")
	if m.efind != nil || m.gfind != nil || m.lfind != nil {
		t.Error("a new tab should start with no find open")
	}
}

func TestIndexFold(t *testing.T) {
	cases := []struct {
		s, q       string
		from, want int
	}{
		{"Hello World", "world", 0, 6},
		{"Hello World", "WORLD", 0, -1}, // q must be folded by the caller
		{"abcABC", "abc", 1, 3},
		{"abc", "abcd", 0, -1},
		{"", "a", 0, -1},
		{"Grüße aus Köln", "köln", 0, strings.Index("Grüße aus Köln", "Köln")},
		{"x", "", 0, -1},
	}
	for _, c := range cases {
		if got := indexFold(c.s, c.q, c.from); got != c.want {
			t.Errorf("indexFold(%q, %q, %d) = %d, want %d", c.s, c.q, c.from, got, c.want)
		}
	}
	if got := lastIndexFold("ab AB ab", "ab", 6); got != 3 {
		t.Errorf("lastIndexFold = %d, want 3", got)
	}
	if got := lastIndexFold("ab", "ab", 0); got != -1 {
		t.Errorf("nothing before 0: %d", got)
	}
	if foldLower("abc") != "abc" || foldLower("ÄB") != "äb" {
		t.Error("foldLower")
	}
}
