package ui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// Every cell must be painted from the palette. tview primitives fall back to
// tview.Styles as they are constructed, so a pane built before applyTheme runs
// — or one whose background was never set — shows up here as stock black.
func TestThemedBackgroundCoverage(t *testing.T) {
	a, screen := newTestAppScreen(t)

	setBuffer(t, a, "SELECT id, name, breed FROM cats", 0)
	press(a, tcell.KeyCtrlR)
	waitFor(t, a, "the query to finish", func() bool { return a.lastRes != nil })
	drain(t, a)

	// The surfaces every screen is built from — each must actually show up.
	surfaces := map[tcell.Color]string{
		colBg:     "colBg",
		colPanel:  "colPanel",
		colPanel2: "colPanel2",
		colSel:    "colSel",
	}
	// Fills that mark focus or trouble. Allowed anywhere, required nowhere.
	fills := map[tcell.Color]bool{colAccent: true, colErr: true}

	seen := map[tcell.Color]bool{}
	check := func(what string) {
		t.Helper()
		cells, w, h := screen.GetContents()
		for y := range h {
			for x := range w {
				c := cells[y*w+x]
				fg, bg, _ := c.Style.Decompose()
				if _, ok := surfaces[bg]; !ok && !fills[bg] {
					t.Fatalf("%s: cell (%d,%d) drawn on %v, which is off-palette", what, x, y, bg)
				}
				if fg == bg && len(c.Runes) > 0 && c.Runes[0] != ' ' {
					t.Fatalf("%s: %q at (%d,%d) is invisible — fg and bg are both %v",
						what, string(c.Runes), x, y, bg)
				}
				seen[bg] = true
			}
		}
	}
	check("main")

	// Modals draw over the main page, so they are their own surfaces to miss.
	press(a, tcell.KeyCtrlE)
	drain(t, a)
	check("export modal")
	press(a, tcell.KeyEnter) // opens the format dropdown, which lists on its own styles
	drain(t, a)
	check("export modal, dropdown open")
	press(a, tcell.KeyEsc)
	drain(t, a)

	press(a, tcell.KeyCtrlO)
	drain(t, a)
	check("scripts modal")

	for c, name := range surfaces {
		if !seen[c] {
			t.Errorf("%s never reached the screen", name)
		}
	}
}

// Focus is the one cue for where the keys go, so the focused pane's border
// must be the only accented one.
func TestFocusMovesTheAccentedBorder(t *testing.T) {
	a := newTestApp(t)

	borders := func() (editor, table, conns tcell.Color) {
		return onUI(t, a, func() tcell.Color { return a.editor.GetBorderColor() }),
			onUI(t, a, func() tcell.Color { return a.table.GetBorderColor() }),
			onUI(t, a, func() tcell.Color { return a.connList.GetBorderColor() })
	}

	ed, tb, cn := borders()
	if ed != colAccent || tb != colLine || cn != colLine {
		t.Fatalf("at startup the editor should hold the accent: editor=%v table=%v conns=%v", ed, tb, cn)
	}

	press(a, tcell.KeyTab)
	drain(t, a)
	ed, tb, cn = borders()
	if tb != colAccent || ed != colLine || cn != colLine {
		t.Fatalf("after Tab the table should hold the accent: editor=%v table=%v conns=%v", ed, tb, cn)
	}
}
