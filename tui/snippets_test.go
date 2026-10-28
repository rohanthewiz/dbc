package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// pasteText puts s in as a paste: one edit, and no completion popup to
// claim the next Tab (typeText would open one on a word's second letter).
func pasteText(t *testing.T, m *Model, s string) {
	t.Helper()
	drive(t, m, tea.PasteMsg{Content: s})
}

// stopText is what the live snippet's current stop holds.
func stopText(t *testing.T, m *Model) string {
	t.Helper()
	sn := m.editor.liveSnip()
	if sn == nil {
		t.Fatal("no live snippet")
	}
	sp := sn.stops[sn.cur].Spans[0]
	return m.editor.Text()[sp.From:sp.To]
}

// A snippet's name then Tab expands it at the line's indent, its stops are
// visited by number with each default selected, and Tab past the last ends
// the session — the keyboard staying in the editor throughout.
func TestSnippetTabExpands(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	m.editor.SetText("  sel")
	m.editor.move(pos{0, 5}, false)
	key(t, m, "tab")
	want := "  SELECT *\n  FROM table\n  WHERE true\n  LIMIT 100"
	if got := m.editor.Text(); got != want {
		t.Fatalf("expanded =\n%q\nwant\n%q", got, want)
	}
	if m.focus != focusEditor {
		t.Fatal("Tab moved the focus instead of expanding")
	}
	// VAR1 first (FROM), its default selected so typing replaces it
	if s, _, _ := m.editor.Selection(); s != "table" {
		t.Fatalf("first stop selected %q, want \"table\"", s)
	}
	pasteText(t, m, "cats")
	key(t, m, "tab")
	if got := stopText(t, m); got != "*" {
		t.Fatalf("second stop = %q, want \"*\"", got)
	}
	pasteText(t, m, "name")
	key(t, m, "shift+tab")
	if got := stopText(t, m); got != "cats" {
		t.Fatalf("⇧Tab went to %q, want back to \"cats\"", got)
	}
	for range 4 { // VAR2, VAR3, VAR4, then past the last
		key(t, m, "tab")
	}
	if m.editor.liveSnip() != nil {
		t.Fatal("Tab past the last stop should end the session")
	}
	if got, want := m.editor.Text(), "  SELECT name\n  FROM cats\n  WHERE true\n  LIMIT 100"; got != want {
		t.Fatalf("filled =\n%q\nwant\n%q", got, want)
	}
	if m.editor.Caret() != len(m.editor.Text()) || m.focus != focusEditor {
		t.Fatalf("caret %d focus %v: want the end, in the editor", m.editor.Caret(), m.focus)
	}
	// with the session over, Tab is the focus key again
	key(t, m, "tab")
	if m.focus == focusEditor {
		t.Fatal("Tab after the session should move the focus")
	}
}

// Typing into a stop used twice fills both uses. ^Z keeps them in step:
// each step back is an edit inside the stop, so the mirrors follow and the
// session lives on, until the step that takes the snippet itself back.
func TestSnippetMirrors(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	m.editor.SetText("") // the demo's sample query
	typeText(t, m, "cnt")
	m.compl = nil
	key(t, m, "tab")
	key(t, m, "tab") // to VAR2, the column, used in SELECT and GROUP BY
	if got := stopText(t, m); got != "column" {
		t.Fatalf("stop = %q", got)
	}
	drive(t, m, tea.KeyPressMsg{Code: 'q', Text: "q"})
	drive(t, m, tea.KeyPressMsg{Code: 'z', Text: "z"})
	want := "SELECT qz, count(*) AS n\nFROM table\nGROUP BY qz\nORDER BY n DESC"
	if got := m.editor.Text(); got != want {
		t.Fatalf("mirrored =\n%s\nwant\n%s", got, want)
	}
	if c := m.editor.Caret(); c != strings.Index(want, "qz")+2 {
		t.Fatalf("caret %d, want after qz in the first use", c)
	}
	// typing over the selected default is one step, the letters after it
	// another (the editor's own grouping)
	for _, want := range []string{"q", "column"} {
		key(t, m, "ctrl+z")
		got := m.editor.Text()
		if !strings.Contains(got, "SELECT "+want+",") || !strings.Contains(got, "GROUP BY "+want+"\n") {
			t.Fatalf("^Z: both uses should read %q, got\n%s", want, got)
		}
		if m.editor.liveSnip() == nil {
			t.Fatalf("^Z back to %q ended the session", want)
		}
	}
	key(t, m, "ctrl+z")
	if got := m.editor.Text(); got != "cnt" || m.editor.liveSnip() != nil {
		t.Fatalf("^Z past the insert: %q, session %v", got, m.editor.liveSnip() != nil)
	}
}

// Esc ends the session; so does the caret leaving the stop. A word that is
// no snippet, or one after a ".", leaves Tab to the focus cycle.
func TestSnippetSessionEndsAndFallThrough(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	m.editor.SetText("cnt")
	m.editor.move(pos{0, 3}, false)
	key(t, m, "tab")
	key(t, m, "esc")
	if m.editor.liveSnip() != nil || !strings.Contains(logText(m), "snippet cnt: done") {
		t.Fatalf("Esc did not end the session; log %s", logText(m))
	}

	m.editor.SetText("cnt")
	m.editor.move(pos{0, 3}, false)
	key(t, m, "tab")
	key(t, m, "ctrl+home")
	if m.editor.liveSnip() != nil {
		t.Fatal("the caret left the stop; the session should be over")
	}

	for _, text := range []string{"nosuch", "o.sel", "sel2"} {
		m.focus = focusEditor
		m.editor.SetText(text)
		m.editor.move(pos{0, len(text)}, false)
		key(t, m, "tab")
		if m.editor.Text() != text || m.focus == focusEditor {
			t.Errorf("%q: Tab expanded (%q) or kept the focus", text, m.editor.Text())
		}
	}
}

// The dialog's pick with text selected wraps it: $SELECTION$ takes the
// lines, re-indented under the snippet's, and Tab past the last stop lands
// on $END$.
func TestSnippetDialogWrapsSelection(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	m.editor.SetText("SELECT a\n  FROM t\nSELECT 2")
	m.editor.anc, m.editor.cur, m.editor.sel = pos{0, 0}, pos{2, 0}, true
	if items := editorMenuLabels(m); !strings.Contains(items, "Surround with snippet…") {
		t.Fatalf("menu rows: %s", items)
	}
	key(t, m, "alt+s")
	sd, ok := m.modal.(*snippetsModal)
	if !ok || !strings.Contains(sd.title(), "wraps the selection") {
		t.Fatalf("modal %T", m.modal)
	}
	typeText(t, m, "cte")
	if sn, _ := sd.current(); sn.Name != "cte" {
		t.Fatalf("filter's first row = %q", sn.Name)
	}
	key(t, m, "enter")
	if m.modal != nil {
		t.Fatal("Enter should close the dialog")
	}
	want := "WITH q AS (\n    SELECT a\n      FROM t\n)\nSELECT *\nFROM q\nSELECT 2"
	if got := m.editor.Text(); got != want {
		t.Fatalf("wrapped =\n%s\nwant\n%s", got, want)
	}
	pasteText(t, m, "base")
	key(t, m, "tab")
	if m.editor.liveSnip() != nil {
		t.Fatal("one stop: Tab should end the session")
	}
	if got := m.editor.Text(); !strings.HasPrefix(got, "WITH base AS") || !strings.Contains(got, "FROM base\n") {
		t.Fatalf("mirror: %s", got)
	}
	if c, want := m.editor.Caret(), strings.Index(m.editor.Text(), "FROM base")+len("FROM base"); c != want {
		t.Fatalf("caret %d, want $END$ at %d", c, want)
	}
}

// New, a refused save that stays in the form, edit, and delete behind its
// confirmation.
func TestSnippetDialogManages(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	key(t, m, "alt+s")
	key(t, m, "ctrl+n")
	f, ok := m.modal.(*snippetForm)
	if !ok {
		t.Fatalf("^N opened %T", m.modal)
	}
	typeText(t, m, "mine")
	key(t, m, "enter") // to the description
	typeText(t, m, "my one")
	key(t, m, "tab") // to the body
	pasteText(t, m, "SELECT $VAR1")
	key(t, m, "ctrl+s")
	if m.modal != f || !strings.Contains(f.errMsg, "not closed") {
		t.Fatalf("broken body: modal %T, err %q", m.modal, f.errMsg)
	}
	pasteText(t, m, "$ FROM t")
	key(t, m, "ctrl+s")
	sd, ok := m.modal.(*snippetsModal)
	if !ok {
		t.Fatalf("after save: %T (form error %q)", m.modal, f.errMsg)
	}
	if sn, _ := sd.current(); sn.Name != "mine" || sn.Desc != "my one" || sn.Body != "SELECT $VAR1$ FROM t" {
		t.Fatalf("cursor on %+v", sn)
	}
	// a name taken refuses in place
	key(t, m, "ctrl+e")
	f = m.modal.(*snippetForm)
	f.fields[sfName].SetText("sel")
	key(t, m, "ctrl+s")
	if !strings.Contains(f.errMsg, "already exists") {
		t.Fatalf("rename onto sel: %q", f.errMsg)
	}
	key(t, m, "esc")
	sd = m.modal.(*snippetsModal)
	key(t, m, "ctrl+d")
	pickMenu(t, m, "Delete mine")
	if _, ok, _ := m.snips.Get("mine"); ok {
		t.Fatal("mine survived its delete")
	}
	for _, it := range sd.lst.items {
		if it.label == "mine" {
			t.Fatal("the dialog still lists mine")
		}
	}
}
