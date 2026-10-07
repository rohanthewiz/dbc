package e2e

import (
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// The TUI, driven as a person drives it: the real binary in a terminal,
// keys and mouse through an emulator, every check read off the screen it
// draws. Each step builds on the one before, so they run in order in one
// session (t.Run would let a failed step leave the next to fail on its
// leftovers; a fatal stops the run instead).
func TestTUI(t *testing.T) {
	e := setup(t)
	u := e.start(t)

	step(t, "startup draws the panes and connects to the default", func() {
		u.waitFor("▶ Run", "Connections", "● lite", "Query", "Results", "Log", " cats ", " owners ")
		// row counts are opt-in (N-124): # in Tables turns them on, and
		// they load after the list
		u.ctrl('l')
		u.key(uv.KeyTab) // Connections → Tables
		u.key('#')
		u.waitUntil("cats 3 and owners 2 in Tables", func(string) bool {
			return strings.HasSuffix(strings.TrimRight(strings.Trim(u.lineWith(" cats "), "│ "), " "), "3") &&
				strings.Contains(u.lineWith(" owners "), "2")
		})
		u.click("Type SQL here", 0) // the editor has the keyboard again
	})

	step(t, "Ctrl+R runs the statement and the grid shows it", func() {
		u.typeText("SELECT name, breed FROM cats ORDER BY name;")
		u.ctrl('r')
		u.waitFor("3 rows", "Leo", "Mia", "Tom", "siamese")
	})

	step(t, "completion opens on a dot, narrows as typed, Tab picks", func() {
		u.key(uv.KeyEnter)
		u.key(uv.KeyEnter)
		u.typeText("SELECT name FROM cats c WHERE c.")
		u.waitFor("owner_id", "breed") // the popup, listing cats' columns
		u.typeText("br")
		u.waitGone("owner_id")
		u.key(uv.KeyTab)
		u.waitUntil("c.breed in the editor", func(string) bool {
			return strings.Contains(u.lineWith("WHERE"), "WHERE c.breed")
		})
		u.typeText(" = 'tabby'")
		u.ctrl('r')
		u.waitFor("1 row")
		u.waitGone("Mia")
	})

	step(t, "a double-click on a table previews its rows", func() {
		x, y := u.find(" owners ")
		u.clickAt(x+2, y)
		u.clickAt(x+2, y)
		u.waitFor("Ada", "Bo")
	})

	step(t, "a header click sorts, and again sorts descending", func() {
		// "│ name" is the grid's header cell (the editor has no box rule)
		u.click("│ name", 3)
		u.click("│ name", 3)
		u.waitUntil("Bo above Ada", func(string) bool {
			_, bo := u.find("│ Bo ")
			_, ada := u.find("│ Ada ")
			return bo < ada
		})
	})

	step(t, "a click on another connection switches to it", func() {
		u.click(" lite2 ", 2)
		u.waitFor("● lite2", " dogs ", "Tables · 1")
	})

	step(t, "F1 opens the keys dialog, Esc closes it", func() {
		u.key(uv.KeyF1)
		u.waitFor("Keys · F1 or ?", "Editor", "Query tabs")
		u.key(uv.KeyEscape)
		u.waitGone("Keys · F1 or ?")
	})

	step(t, "Ctrl+B folds the sidebar away and back", func() {
		u.ctrl('b')
		u.waitGone("Connections")
		u.ctrl('b')
		u.waitFor("Connections", "● lite2")
	})

	step(t, "t transposes the grid and back", func() {
		u.click("│ Bo ", 3) // the grid has the keyboard now
		u.key('t')
		u.waitFor("· transposed (t)") // the grid's bottom strip
		u.key('t')
		u.waitGone("· transposed (t)")
		u.waitFor("id │ name") // upright, with every column back in view
	})

	step(t, "Alt+T opens a tab with its own result; Alt+1 / Alt+2 switch", func() {
		u.key('t', uv.ModAlt)
		u.waitUntil("a strip with both tabs", func(string) bool {
			l := u.lineWith("Query 2")
			return strings.Contains(l, "Query 1") && strings.Contains(l, " + ")
		})
		u.typeText("SELECT 7 AS seven;")
		u.ctrl('r')
		u.waitFor("seven │") // the grid's header; the log names the query too
		u.key('1', uv.ModAlt)
		u.waitFor("│ Ada ")
		u.waitGone("seven │")
		u.key('2', uv.ModAlt)
		u.waitFor("seven │")
	})

	step(t, "F2 renames an alias in place", func() {
		u.key(uv.KeyEnter)
		u.typeText("SELECT d.name FROM dogs d")
		u.key(uv.KeyF2)
		u.waitFor("Rename alias d")
		u.typeText("dg")
		u.key(uv.KeyEnter)
		u.waitFor("SELECT dg.name FROM dogs dg")
	})

	step(t, "a in Connections opens the add-connection form", func() {
		u.ctrl('l')
		u.key('a')
		u.waitFor("Add a connection", "Test connection")
		u.key(uv.KeyEscape)
		u.waitGone("Add a connection")
	})

	step(t, "Ctrl+O browses scripts: new from a template, $EDITOR, the check, run, trash, restore", func() {
		u.ctrl('o')
		// an empty scripts dir offers the templates, then the examples
		u.waitFor("Scripts · ~/.config/dbc/scripts", "Query and show", "Examples · read-only", "loop_params.go")
		u.key('n')
		u.waitFor("new script from")
		u.key(uv.KeyDown) // Blank script → Query and show
		u.key(uv.KeyEnter)
		u.waitFor("New script · Query and show")
		// the stand-in editor writes a script that does not compile
		e.nextEdit(t, "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\nfunc Run(s *sdb.S) error {\n\treturn undefinedThing\n}\n")
		u.typeText("nightly") // replaces the offered stem, keeps .go
		u.key(uv.KeyEnter)
		// back from the editor: the check's finding in the log, compiler
		// style, and the browser open on the new script
		u.waitFor("nightly.go:6:9: undefined: undefinedThing", "Scripts · ~/.config/dbc/scripts")

		// e edits it again; this time it compiles, and Enter runs it
		e.nextEdit(t, "// Lists the cats.\npackage main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\n"+
			"func Run(s *sdb.S) error {\n\tr, err := s.Query(\"lite\", \"SELECT name AS cat FROM cats ORDER BY name\")\n"+
			"\tif err != nil {\n\t\treturn err\n\t}\n\ts.Print(\"%d cats\", len(r.Rows))\n\ts.Show(r)\n\treturn nil\n}\n")
		u.key('e')
		u.waitFor("nightly.go saved — checked, no problems", "Lists the cats.")
		u.key(uv.KeyEnter)
		u.waitFor("script nightly.go completed", "3 cats", "│ cat ", "│ Leo ")

		// Del trashes it (nothing asked); the Trash lists it; Enter restores
		u.ctrl('o')
		u.waitFor("Lists the cats.")
		u.key(uv.KeyDelete)
		u.waitFor("moved nightly.go to the trash", "Trash (1)")
		u.key('t')
		u.key('G') // the last row: the trashed script
		u.key(uv.KeyEnter)
		u.waitFor("restored nightly.go")
		u.key(uv.KeyEscape)
		u.waitGone("Scripts · ~/.config/dbc/scripts")
	})

	step(t, "Ctrl+Q quits", func() {
		u.quit()
	})
}

// step runs one named part of a session, logging it so a failure says
// which step got there.
func step(t *testing.T, name string, f func()) {
	t.Helper()
	start := time.Now()
	f()
	t.Logf("ok  %s (%s)", name, time.Since(start).Round(time.Millisecond))
}
