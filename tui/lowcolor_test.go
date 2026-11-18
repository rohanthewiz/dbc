package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
)

// Below 256 colours a selection is drawn in reverse video, not by its
// background alone, and the grid's cursor cell and the editor's selection
// stay visible; at full colour they keep the background (N-294).
func TestLowColorSelections(t *testing.T) {
	for _, c := range []struct {
		profile colorprofile.Profile
		reverse bool
	}{
		{colorprofile.TrueColor, false},
		{colorprofile.ANSI256, false},
		{colorprofile.ANSI, true},
		{colorprofile.Ascii, true}, // NO_COLOR
	} {
		m := newTestModel(t)
		drive(t, m, tea.ColorProfileMsg{Profile: c.profile})
		key(t, m, "ctrl+r")
		m.focus = focusGrid
		frame(m)
		x, y := m.grid.cursorScreen()
		if got := frame(m).StyleAt(x+1, y).Attr&AttrReverse != 0; got != c.reverse {
			t.Errorf("profile %v: the grid's cursor cell reverse = %v, want %v", c.profile, got, c.reverse)
		}
		m.focus = focusEditor
		m.editor.SelectAll()
		ev := m.lay.editor
		f := frame(m)
		found := false
		for xx := ev.X; xx < ev.X+ev.W; xx++ {
			if f.StyleAt(xx, ev.Y+1).Attr&AttrReverse != 0 {
				found = true
				break
			}
		}
		if found != c.reverse {
			t.Errorf("profile %v: the editor's selection reverse = %v, want %v", c.profile, found, c.reverse)
		}
	}
}

// A warning and an error carry a glyph in the log, drawn but not stored.
func TestLogGlyphs(t *testing.T) {
	m := newTestModel(t)
	m.log(logWarn, "careful now")
	m.log(logErr, "it broke")
	txt := frame(m).Text()
	if !strings.Contains(txt, "⚠ careful now") || !strings.Contains(txt, "✗ it broke") {
		t.Errorf("no glyphs in the log:\n%s", txt)
	}
	if strings.Contains(m.logp.Text(), "⚠") || strings.Contains(m.logp.Text(), "✗") {
		t.Error("the glyphs went into the log's text (a copy would carry them)")
	}
}
