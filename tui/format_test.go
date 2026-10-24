package tui

import (
	"strings"
	"testing"
)

// ⌥⇧F formats the whole buffer as one undo step, unwrapping a query pasted
// from a log line on the way, and says so in the log.
func TestFormatWholeBuffer(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	pasted := `"SQL query:\nselect a,b from t where x=1;\nselect 2"`
	m.editor.SetText(pasted)
	key(t, m, "alt+shift+f")
	want := "SELECT a, b\nFROM t\nWHERE x = 1;\n\nSELECT 2"
	if got := m.editor.Text(); got != want {
		t.Fatalf("formatted =\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(logText(m), "formatted the buffer") {
		t.Errorf("log: %s", logText(m))
	}
	key(t, m, "ctrl+z")
	if got := m.editor.Text(); got != pasted {
		t.Errorf("one ^Z should restore the paste, got %q", got)
	}
	// already formatted: nothing changes, and the log says why
	m.editor.SetText(want)
	key(t, m, "alt+shift+f")
	if m.editor.Text() != want || !strings.Contains(logText(m), "already formatted") {
		t.Errorf("text %q, log %s", m.editor.Text(), logText(m))
	}
}

// With the statement count unchanged, the caret stays on the statement it
// was in: at its start, after formatting.
func TestFormatKeepsTheCaretsStatement(t *testing.T) {
	m := newTestModel(t)
	m.editor.SetText("select 1;\nselect a,b from t;\nselect 3;")
	m.editor.move(pos{1, 5}, false)
	key(t, m, "alt+shift+f")
	if got, want := m.editor.Caret(), strings.Index(m.editor.Text(), "SELECT a"); got != want {
		t.Errorf("caret at %d, want %d (the second statement's start) in\n%s", got, want, m.editor.Text())
	}
}

// With a selection only it is formatted; the rest of the buffer, and the
// selection's trailing newline, are left as they were.
func TestFormatSelection(t *testing.T) {
	m := newTestModel(t)
	m.editor.SetText("select 1;\nselect a,b from t;\n")
	m.editor.anc, m.editor.cur, m.editor.sel = pos{1, 0}, pos{2, 0}, true
	items := editorMenuLabels(m)
	if !strings.Contains(items, "Format selection") {
		t.Errorf("menu rows: %s", items)
	}
	key(t, m, "alt+shift+f")
	if got, want := m.editor.Text(), "select 1;\nSELECT a, b\nFROM t;\n"; got != want {
		t.Errorf("formatted =\n%q\nwant\n%q", got, want)
	}
}

// editorMenuLabels opens the editor menu and returns its rows' labels, one
// per line; the menu is closed again.
func editorMenuLabels(m *Model) string {
	m.openEditorMenu(10, 5)
	var labels []string
	for _, it := range m.menu.items {
		labels = append(labels, it.label)
	}
	m.menu = nil
	return strings.Join(labels, "\n")
}
