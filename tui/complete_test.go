package tui

import (
	"strings"
	"testing"
)

// popupLabels is the open popup's items, or nil when it is closed.
func popupLabels(m *Model) []string {
	if m.compl == nil {
		return nil
	}
	var out []string
	for _, it := range m.compl.res.Items {
		out = append(out, it.Label)
	}
	return out
}

// Typing "c." after the table's alias opens the popup on its columns (the
// first time through a schema load), and Tab picks one.
func TestCompletionOpensOnDotAndPicks(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	m.editor.SetText("")
	// no FROM yet: c names nothing, so there is nothing to offer after it
	typeText(t, m, "SELECT c.")
	if m.compl != nil {
		t.Fatalf("popup opened before the alias is in scope: %v", popupLabels(m))
	}
	m.editor.SetText("SELECT  FROM cats c")
	m.editor.cur = pos{0, len("SELECT ")}
	typeText(t, m, "c.")
	ls := popupLabels(m)
	if len(ls) != 5 || !strings.Contains(strings.Join(ls, " "), "breed") {
		t.Fatalf("c. → %v", ls)
	}
	typeText(t, m, "br")
	if ls = popupLabels(m); len(ls) != 1 || ls[0] != "breed" {
		t.Fatalf("c.br → %v", ls)
	}
	// the frame shows it, with the column's type
	if f := frame(m).Text(); !strings.Contains(f, "breed") {
		t.Error("the popup is not drawn")
	}
	key(t, m, "tab")
	if got := m.editor.Text(); got != "SELECT c.breed FROM cats c" {
		t.Errorf("after the pick: %q", got)
	}
	if m.compl != nil {
		t.Error("the popup stayed open after the pick")
	}
	if m.focus != focusEditor {
		t.Error("Tab moved the focus instead of picking")
	}
}

// A buffer replaced behind the popup's back (a history pick, a tab switch)
// closes it.
func TestCompletionClosesOnSetText(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	m.editor.SetText("SELECT * FROM ")
	m.editor.cur = pos{0, len("SELECT * FROM ")}
	key(t, m, "ctrl+space")
	if m.compl == nil {
		t.Fatal("no popup")
	}
	m.editor.SetText("SELECT 1")
	if strings.Contains(frame(m).Text(), "▦ cats") || m.compl != nil {
		t.Error("the popup outlived the buffer it was for")
	}
}

// Ctrl+Space asks anywhere; Esc closes; a space closes; Enter on a word
// already typed in full is a new line.
func TestCompletionKeys(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	m.editor.SetText("SELECT * FROM ")
	m.editor.cur = pos{0, len("SELECT * FROM ")}
	key(t, m, "ctrl+space")
	if ls := popupLabels(m); len(ls) == 0 || ls[0] != "cats" {
		t.Fatalf("Ctrl+Space after FROM → %v", ls)
	}
	key(t, m, "esc")
	if m.compl != nil {
		t.Fatal("Esc did not close the popup")
	}
	typeText(t, m, "cats")
	if m.compl == nil {
		t.Fatal("typing a table name did not open the popup")
	}
	key(t, m, "enter")
	if got := m.editor.Text(); got != "SELECT * FROM cats\n" {
		t.Errorf("Enter on a finished word: %q", got)
	}
	typeText(t, m, "WH")
	if ls := popupLabels(m); len(ls) == 0 || ls[0] != "WHERE" {
		t.Fatalf("WH → %v", ls)
	}
	typeText(t, m, " ")
	if m.compl != nil {
		t.Error("a space did not close the popup")
	}
}

// Postgres functions are offered, and a pick lands the caret between the
// parentheses — here on SQLite, whose own functions are offered instead.
func TestCompletionFunctionCaret(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	m.editor.SetText("SELECT  FROM cats")
	m.editor.cur = pos{0, len("SELECT ")}
	typeText(t, m, "coal")
	key(t, m, "tab")
	if got := m.editor.Text(); got != "SELECT coalesce() FROM cats" {
		t.Fatalf("got %q", got)
	}
	if m.editor.cur != (pos{0, len("SELECT coalesce(")}) {
		t.Errorf("caret at %v", m.editor.cur)
	}
}
