package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// editorMenuItem opens the editor menu and returns the row labelled label
// exactly; the menu is closed again.
func editorMenuItem(t *testing.T, m *Model, label string) menuItem {
	t.Helper()
	m.openEditorMenu(10, 5)
	defer func() { m.menu = nil }()
	for _, it := range m.menu.items {
		if it.label == label {
			return it
		}
	}
	t.Fatalf("no %q row in the editor menu:\n%s", label, editorMenuLabels(m))
	return menuItem{}
}

// Stop, Undo and Redo dim when they would do nothing, and say why — dbc
// web's rows (app.js editorMenu), ported.
func TestEditorMenuDimsIdleRows(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	m.editor.SetText("") // loading clears undo

	for label, why := range map[string]string{
		"■ Stop": "nothing is running or connecting",
		"Undo":   "nothing to undo",
		"Redo":   "nothing to redo",
	} {
		if got := editorMenuItem(t, m, label).why; got != why {
			t.Errorf("%s: why = %q, want %q", label, got, why)
		}
	}
	if why := editorMenuItem(t, m, "Paste").why; why != "" {
		t.Errorf("Paste should always be live, why = %q", why)
	}

	// picking the dim Stop says why in the log rather than doing nothing
	m.openEditorMenu(10, 5)
	pickMenu(t, m, "■ Stop")
	if !strings.Contains(logText(m), "nothing is running or connecting") {
		t.Errorf("dim Stop should log why, log:\n%s", logText(m))
	}
}

// After an edit Undo is live; after an undo Redo is, and puts it back.
func TestEditorMenuUndoRedo(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	m.editor.SetText("")
	key(t, m, "a")
	if why := editorMenuItem(t, m, "Undo").why; why != "" {
		t.Fatalf("Undo after typing should be live, why = %q", why)
	}
	m.openEditorMenu(10, 5)
	pickMenu(t, m, "Undo")
	if m.editor.Text() != "" {
		t.Fatalf("after Undo, text = %q", m.editor.Text())
	}
	if why := editorMenuItem(t, m, "Redo").why; why != "" {
		t.Fatalf("Redo after an undo should be live, why = %q", why)
	}
	m.openEditorMenu(10, 5)
	pickMenu(t, m, "Redo")
	if m.editor.Text() != "a" {
		t.Errorf("after Redo, text = %q, want %q", m.editor.Text(), "a")
	}
}

// Paste inserts the local clipboard at the caret, over the selection.
func TestEditorMenuPasteLocal(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	clipReader = func() (string, error) { return "select 1", nil }
	m.editor.SetText("xx")
	m.editor.SelectAll()

	m.openEditorMenu(10, 5)
	pickMenu(t, m, "Paste")
	if got := m.editor.Text(); got != "select 1" {
		t.Errorf("after Paste, text = %q, want the clipboard's", got)
	}
	if m.pasteInto != nil {
		t.Error("a local read should leave no OSC 52 read pending")
	}

	// an empty clipboard is said, not silently skipped
	clipReader = func() (string, error) { return "", nil }
	m.openEditorMenu(10, 5)
	pickMenu(t, m, "Paste")
	if !strings.Contains(logText(m), "the clipboard holds no text") {
		t.Errorf("empty clipboard should be logged, log:\n%s", logText(m))
	}
}

// With no local clipboard (the harness's default reader fails, as over
// SSH) Paste asks the terminal; its answer goes to the editor the menu was
// opened on even when another tab is on screen by then, and a reply nobody
// asked for changes nothing.
func TestEditorMenuPasteViaTerminal(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	m.editor.SetText("")
	asked := m.editor

	m.openEditorMenu(10, 5)
	pickMenu(t, m, "Paste")
	if m.pasteInto != asked {
		t.Fatal("Paste without a local clipboard should wait for the terminal's reply")
	}
	if !strings.Contains(logText(m), "OSC 52") {
		t.Errorf("the fallback should be logged, log:\n%s", logText(m))
	}

	drive(t, m, nil, m.newTab())
	if m.editor == asked {
		t.Fatal("test setup: a new tab should bring its own editor")
	}
	drive(t, m, tea.ClipboardMsg{Content: "select 2", Selection: 'c'})
	if asked.Text() != "select 2" || m.editor.Text() != "" {
		t.Errorf("reply landed wrong: asked %q, current %q", asked.Text(), m.editor.Text())
	}
	if m.pasteInto != nil {
		t.Error("the reply should clear the pending read")
	}

	drive(t, m, tea.ClipboardMsg{Content: "stray", Selection: 'c'})
	if m.editor.Text() != "" || asked.Text() != "select 2" {
		t.Error("an unasked-for reply should change nothing")
	}
}
