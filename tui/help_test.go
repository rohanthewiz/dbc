package tui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/userdata"
)

// F1 opens the keys dialog from anywhere — the editor included — and Esc
// closes it; ? opens it only where nothing takes text, so typing a ? into
// the editor still types it.
func TestHelpDialog(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusEditor
	drive(t, m, tea.KeyPressMsg{Code: tea.KeyF1})
	if _, ok := m.modal.(*helpModal); !ok {
		t.Fatalf("F1 opened %T", m.modal)
	}
	txt := frame(m).Text()
	for _, want := range []string{"Keys · F1 or ?", "Editor", "run the statement under the caret"} {
		if !strings.Contains(txt, want) {
			t.Errorf("dialog lacks %q:\n%s", want, txt)
		}
	}
	key(t, m, "esc")
	if m.modal != nil {
		t.Fatalf("Esc left %T open", m.modal)
	}

	m.editor.SetText("")
	typeText(t, m, "?")
	if m.modal != nil || m.editor.Text() != "?" {
		t.Fatalf("? in the editor: modal %T, text %q", m.modal, m.editor.Text())
	}

	m.focus = focusGrid
	typeText(t, m, "?")
	if _, ok := m.modal.(*helpModal); !ok {
		t.Fatalf("? in the grid opened %T", m.modal)
	}
	// it scrolls without running off the end
	for range 200 {
		key(t, m, "down")
	}
	frame(m)
	h := m.modal.(*helpModal)
	if h.top == 0 || h.top > len(h.lines) {
		t.Errorf("scrolled to %d of %d lines", h.top, len(h.lines))
	}
}

// ^B folds the sidebar away and back; a click on the › tab brings it back
// too, and the keyboard leaves a pane that folded away.
func TestSidebarFold(t *testing.T) {
	m := newTestModel(t)
	frame(m)
	if m.lay.conns.Empty() {
		t.Fatal("the sidebar is not showing at 120 columns")
	}
	editorX := m.lay.editor.X
	m.focus = focusTables
	key(t, m, "ctrl+b")
	frame(m)
	if !m.lay.conns.Empty() || m.lay.editor.X != 0 {
		t.Fatalf("folded: conns %v, editor at x=%d", m.lay.conns, m.lay.editor.X)
	}
	if m.focus != focusEditor {
		t.Errorf("focus stayed on %v, a pane that is not drawn", m.focus)
	}
	if got := frame(m).Line(m.lay.foldTab.Y); !strings.HasPrefix(got, "›") {
		t.Errorf("no › tab on the edge: %q", got)
	}

	click(t, m, m.lay.foldTab.X, m.lay.foldTab.Y)
	frame(m)
	if m.lay.conns.Empty() || m.lay.editor.X != editorX {
		t.Fatalf("› did not bring the sidebar back: conns %v, editor at x=%d", m.lay.conns, m.lay.editor.X)
	}

	// the ‹ on the Connections box folds it, and ^L brings it back
	click(t, m, m.lay.foldTab.X+1, m.lay.foldTab.Y)
	frame(m)
	if !m.lay.conns.Empty() {
		t.Fatal("‹ did not fold the sidebar")
	}
	key(t, m, "ctrl+l")
	frame(m)
	if m.lay.conns.Empty() || m.focus != focusConns {
		t.Fatalf("^L: conns %v, focus %v", m.lay.conns, m.focus)
	}
}

// The pane sizes and the fold survive a restart: saved at quit, restored by
// the next model, and clamped to the window on the first frame.
func TestLayoutPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui-layout.json")
	m := newTestModel(t)
	m.layoutFile = path
	m.sideW, m.logH, m.edFrac = 30, 9, 0.5
	key(t, m, "ctrl+b")
	m.saveLayout()

	m2 := newTestModel(t)
	m2.restoreLayout(userdata.LoadLayout(path))
	if m2.sideW != 30 || m2.logH != 9 || m2.edFrac != 0.5 || !m2.sideHidden {
		t.Fatalf("restored side %d, log %d, editor %.2f, hidden %v", m2.sideW, m2.logH, m2.edFrac, m2.sideHidden)
	}
	frame(m2)
	if !m2.lay.conns.Empty() {
		t.Error("the fold was saved but the sidebar shows")
	}
}
