package tui

import (
	"strings"
	"testing"
	"time"
)

func edit(text string) *editor {
	e := newEditor(false)
	e.SetText(text)
	return e
}

func TestEditorInsertAndNewlineCarryIndent(t *testing.T) {
	e := edit("SELECT *\n    FROM t")
	e.move(pos{1, 10}, false)
	e.Newline()
	e.Insert("WHERE x")
	if got := e.Text(); got != "SELECT *\n    FROM t\n    WHERE x" {
		t.Errorf("text = %q", got)
	}
}

func TestEditorBackspaceJoinsLinesAndDeleteMirrorsIt(t *testing.T) {
	e := edit("ab\ncd")
	e.move(pos{1, 0}, false)
	e.Backspace()
	if e.Text() != "abcd" || e.cur != (pos{0, 2}) {
		t.Errorf("backspace: %q at %v", e.Text(), e.cur)
	}
	e.Delete()
	if e.Text() != "abd" {
		t.Errorf("delete: %q", e.Text())
	}
}

func TestEditorSelectionReplaceAndOffsets(t *testing.T) {
	e := edit("SELECT 1;\nSELECT 2;")
	e.move(pos{1, 7}, false)
	e.move(pos{1, 8}, true)
	sel, a, b := e.Selection()
	if sel != "2" || e.Text()[a:b] != "2" {
		t.Errorf("selection %q [%d:%d]", sel, a, b)
	}
	e.Insert("42")
	if e.Text() != "SELECT 1;\nSELECT 42;" {
		t.Errorf("text = %q", e.Text())
	}
	for off := 0; off <= len(e.Text()); off++ {
		if got := e.offset(e.posAt(off)); got != off {
			t.Errorf("offset(posAt(%d)) = %d", off, got)
		}
	}
}

// Typing a word undoes as one step; a pause or a space starts a new one.
func TestEditorUndoCoalescesTyping(t *testing.T) {
	e := edit("")
	for _, r := range "select" {
		e.Insert(string(r))
	}
	e.Insert(" ")
	for _, r := range "1" {
		e.Insert(string(r))
	}
	if e.Text() != "select 1" {
		t.Fatalf("text = %q", e.Text())
	}
	e.Undo()
	if e.Text() != "select " {
		t.Errorf("first undo = %q", e.Text())
	}
	e.Undo()
	if e.Text() != "select" && e.Text() != "" {
		t.Errorf("second undo = %q", e.Text())
	}
	e.Redo()
	e.Redo()
	if e.Text() != "select 1" {
		t.Errorf("redo = %q", e.Text())
	}

	// a pause past the window splits the run
	e = edit("")
	e.Insert("a")
	e.lastAt = e.lastAt.Add(-2 * time.Second)
	e.Insert("b")
	e.Undo()
	if e.Text() != "a" {
		t.Errorf("after a pause, undo should keep the first run: %q", e.Text())
	}
}

func TestEditorNormalizesInput(t *testing.T) {
	e := edit("a\tb\r\nc")
	if e.Text() != "a    b\nc" {
		t.Errorf("text = %q", e.Text())
	}
	one := newEditor(true)
	one.Insert("x\ny")
	if one.Text() != "x y" {
		t.Errorf("a one-line field takes newlines as spaces: %q", one.Text())
	}
}

func TestEditorWordMotion(t *testing.T) {
	e := edit("SELECT name_1, age FROM cats")
	e.move(pos{0, 0}, false)
	e.move(e.wordRight(e.cur), false)
	e.move(e.wordRight(e.cur), false)
	if e.cur.col != 13 {
		t.Errorf("two words right = %d, want 13 (after name_1)", e.cur.col)
	}
	e.move(e.wordLeft(e.cur), false)
	if e.cur.col != 7 {
		t.Errorf("word left = %d, want 7", e.cur.col)
	}
	e.DeleteWordBack()
	if !strings.HasPrefix(e.Text(), "name_1") {
		t.Errorf("delete word back: %q", e.Text())
	}
}

// A click maps through display columns, so wide runes before the click point
// do not throw the caret off.
func TestEditorClickMapsThroughWideRunes(t *testing.T) {
	e := edit("'猫猫' AS x\nSELECT")
	e.view = Rect{10, 5, 40, 5}
	e.Click(10+5, 5, 1, false) // display col 5 = after "'猫猫" (1+2+2)
	if e.cur != (pos{0, 3}) {
		t.Errorf("caret = %v, want {0 3}", e.cur)
	}
	e.Click(10+30, 6, 1, false) // past the end of line 2
	if e.cur != (pos{1, 6}) {
		t.Errorf("past-the-end click = %v, want {1 6}", e.cur)
	}
	e.Click(10+2, 20, 1, false) // below the last line
	if e.cur.row != 1 {
		t.Errorf("below-the-text click row = %d", e.cur.row)
	}
}

func TestEditorMultiClick(t *testing.T) {
	e := edit("SELECT breed FROM cats")
	e.view = Rect{0, 0, 40, 5}
	e.Click(9, 0, 2, false)
	if s, _, _ := e.Selection(); s != "breed" {
		t.Errorf("double-click = %q", s)
	}
	e.Click(3, 0, 3, false)
	if s, _, _ := e.Selection(); s != "SELECT breed FROM cats" {
		t.Errorf("triple-click = %q", s)
	}
}

func TestEditorDragExtendsSelection(t *testing.T) {
	e := edit("abcdef\nghijkl")
	e.view = Rect{0, 0, 40, 5}
	e.Click(1, 0, 1, false)
	e.Drag(3, 1)
	if s, _, _ := e.Selection(); s != "bcdef\nghi" {
		t.Errorf("drag selection = %q", s)
	}
}

func TestEditorDrawScrollsToCaretAndReportsIt(t *testing.T) {
	e := edit(strings.Repeat("x\n", 30) + "last")
	e.move(pos{30, 4}, false)
	c := NewCanvas(20, 5, Style{})
	st := newStyles(themeDefault())
	x, y, ok := e.Draw(c.Sub(Rect{0, 0, 20, 5}), st, st.base, [2]int{}, true)
	if !ok || y != 4 || c.Line(4)[x-4:x] != "last" {
		t.Errorf("caret at (%d,%d) ok=%v; bottom line %q", x, y, ok, c.Line(4))
	}
}
