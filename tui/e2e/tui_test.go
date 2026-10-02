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
		u.waitFor("▶ Run", "Connections", "● lite", "Query", "Results", "Log")
		// the row counts load after the list
		u.waitUntil("cats 3 and owners 2 in Tables", func(string) bool {
			return strings.HasSuffix(strings.TrimRight(strings.Trim(u.lineWith(" cats "), "│ "), " "), "3") &&
				strings.Contains(u.lineWith(" owners "), "2")
		})
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
