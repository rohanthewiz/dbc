package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/sqlcomplete"
)

// fkey is a function key press, shifted or not — keyMsg spells keys by
// their letters, which "f12" is not.
func fkey(code rune, shift bool) tea.KeyPressMsg {
	k := tea.KeyPressMsg{Code: code}
	if shift {
		k.Mod = tea.ModShift
	}
	return k
}

// symbolModel is a test model whose editor holds text, focused, with the
// caret at the first occurrence of at (plus off bytes).
func symbolModel(t *testing.T, text, at string, off int) *Model {
	t.Helper()
	m := newTestModel(t)
	m.editor.SetText(text)
	m.focus = focusEditor
	i := strings.Index(text, at)
	if i < 0 {
		t.Fatalf("%q not in the text", at)
	}
	m.editor.move(m.editor.posAt(i+off), false)
	return m
}

const symSQL = "SELECT o.id, o.total\nFROM orders o\nWHERE o.total > 10"

func TestF12SelectsTheDeclaration(t *testing.T) {
	m := symbolModel(t, symSQL, "o.total >", 0)
	drive(t, m, fkey(tea.KeyF12, false))
	sel, a, b := m.editor.Selection()
	if sel != "o" || a != strings.Index(symSQL, "o\nWHERE") {
		t.Fatalf("selection = %q at %d..%d, want the alias in FROM", sel, a, b)
	}
	if !strings.Contains(logText(m), "alias o is declared on line 2") {
		t.Errorf("log: %s", logText(m))
	}
}

func TestF12OnNothingSaysWhy(t *testing.T) {
	m := symbolModel(t, symSQL, "SELECT", 2)
	drive(t, m, fkey(tea.KeyF12, false))
	if !strings.Contains(logText(m), "no definition here") {
		t.Errorf("log: %s", logText(m))
	}
}

func TestShiftF12MarksUsesAndStepsThroughThem(t *testing.T) {
	m := symbolModel(t, symSQL, "o.id", 0)
	drive(t, m, fkey(tea.KeyF12, true))
	marks := m.editor.liveMarks()
	if len(marks) != 4 {
		t.Fatalf("marks = %v, want the 4 uses of o", marks)
	}
	if !strings.Contains(logText(m), "alias o: 4 uses") {
		t.Errorf("log: %s", logText(m))
	}
	// the marks are drawn: the "o" of o.id is underlined in the frame
	c := frame(m)
	x, y := findText(t, c, "SELECT o.id")
	if c.StyleAt(x+7, y).Attr&AttrUnderline == 0 {
		t.Error("the use is not drawn marked")
	}

	// again: the caret steps to the next use, then the next, wrapping
	start := m.editor.Caret()
	drive(t, m, fkey(tea.KeyF12, true))
	if got, want := m.editor.Caret(), strings.Index(symSQL, "o.total"); got != want || start == got {
		t.Fatalf("caret = %d, want the next use at %d", got, want)
	}
	for range 3 {
		drive(t, m, fkey(tea.KeyF12, true))
	}
	if got, want := m.editor.Caret(), strings.Index(symSQL, "o.id"); got != want {
		t.Errorf("after wrapping, caret = %d, want %d", got, want)
	}

	// Esc clears them; any edit would have too
	key(t, m, "esc")
	if m.editor.liveMarks() != nil {
		t.Error("Esc left the marks")
	}
	drive(t, m, fkey(tea.KeyF12, true))
	typeText(t, m, "x")
	if m.editor.liveMarks() != nil {
		t.Error("an edit left the marks")
	}
}

func TestF2RenamesAsOneUndoStep(t *testing.T) {
	m := symbolModel(t, symSQL, "o.total >", 0)
	drive(t, m, fkey(tea.KeyF2, false))
	rm, ok := m.modal.(*promptModal)
	if !ok {
		t.Fatalf("modal = %T, want the rename prompt", m.modal)
	}
	if rm.field.Text() != "o" {
		t.Errorf("prompt holds %q, want the current name", rm.field.Text())
	}
	typeText(t, m, "ord") // replaces the selected old name
	key(t, m, "enter")
	if m.modal != nil {
		t.Fatalf("prompt still open: %s", rm.errMsg)
	}
	want := "SELECT ord.id, ord.total\nFROM orders ord\nWHERE ord.total > 10"
	if got := m.editor.Text(); got != want {
		t.Fatalf("text = %q", got)
	}
	if !strings.Contains(logText(m), "renamed alias o to ord — 4 places") {
		t.Errorf("log: %s", logText(m))
	}
	// the caret stayed on the use it was on
	if got := m.editor.Caret(); got != strings.Index(want, "ord.total >") {
		t.Errorf("caret = %d", got)
	}
	m.editor.Undo()
	if m.editor.Text() != symSQL {
		t.Errorf("one undo should bring the old names back, got %q", m.editor.Text())
	}
}

func TestF2QuotesForTheDialect(t *testing.T) {
	// the quoting is the active connection's dialect's (the test model is
	// on SQLite): a name with a space in it is quoted at every use
	m := symbolModel(t, symSQL, "o.id", 0)
	drive(t, m, fkey(tea.KeyF2, false))
	typeText(t, m, "my o")
	key(t, m, "enter")
	if got := m.editor.Text(); !strings.Contains(got, `FROM orders "my o"`) || !strings.Contains(got, `"my o".total >`) {
		t.Errorf("text = %q", got)
	}
}

func TestF2RefusesATakenNameAndATable(t *testing.T) {
	sql := "SELECT o.id, c.name FROM orders o JOIN customers c ON c.id = o.cid"
	m := symbolModel(t, sql, "o.id", 0)
	drive(t, m, fkey(tea.KeyF2, false))
	typeText(t, m, "c")
	key(t, m, "enter")
	rm, ok := m.modal.(*promptModal)
	if !ok || !strings.Contains(rm.errMsg, "already a name") {
		t.Fatalf("a taken name should keep the prompt open with why; modal %T", m.modal)
	}
	key(t, m, "esc")
	if m.editor.Text() != sql {
		t.Errorf("a refused rename changed the text: %q", m.editor.Text())
	}

	m = symbolModel(t, "SELECT orders.id FROM orders", "orders.id", 0)
	drive(t, m, fkey(tea.KeyF2, false))
	if m.modal != nil {
		t.Fatal("a table must not open the prompt")
	}
	if !strings.Contains(logText(m), "is a table") {
		t.Errorf("log: %s", logText(m))
	}
}

func TestCtrlClickGoesToDefinition(t *testing.T) {
	m := symbolModel(t, symSQL, "SELECT", 0)
	c := frame(m)
	x, y := findText(t, c, "WHERE o.total")
	drive(t, m, tea.MouseClickMsg{X: x + 6, Y: y, Button: tea.MouseLeft, Mod: tea.ModCtrl})
	drive(t, m, tea.MouseReleaseMsg{X: x + 6, Y: y, Button: tea.MouseLeft})
	if sel, _, _ := m.editor.Selection(); sel != "o" || m.editor.cur.row != 1 {
		t.Errorf("selection %q on row %d, want the alias on row 2", sel, m.editor.cur.row+1)
	}
}

func TestEditorMenuOffersSymbolActions(t *testing.T) {
	m := symbolModel(t, symSQL, "o.id", 0)
	m.openEditorMenu(10, 5)
	var labels []string
	for _, it := range m.menu.items {
		if it.label == "Rename…" && it.why != "" {
			t.Errorf("rename disabled on an alias: %s", it.why)
		}
		labels = append(labels, it.label)
	}
	for _, want := range []string{"Go to definition", "Show usages", "Rename…"} {
		if !strings.Contains(strings.Join(labels, "|"), want) {
			t.Errorf("menu lacks %q: %v", want, labels)
		}
	}
}

func TestApplyEditsKeepsTheCaret(t *testing.T) {
	e := edit("ab cd ab")
	e.move(pos{0, 4}, false) // inside "cd"
	e.ApplyEdits([]sqlcomplete.Edit{{From: 6, To: 8, Text: "xyz"}, {From: 0, To: 2, Text: "xyz"}})
	if e.Text() != "xyz cd xyz" || e.Caret() != 5 {
		t.Errorf("text %q caret %d", e.Text(), e.Caret())
	}
}
