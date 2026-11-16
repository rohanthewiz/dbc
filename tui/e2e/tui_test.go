package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
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

	step(t, "a click on another connection switches to it, with results and a log of its own", func() {
		u.click(" lite2 ", 2)
		u.waitFor("● lite2", " dogs ", "Tables · 1", "Log · lite2")
		// lite2 has run nothing: none of lite's rows, no result tab
		u.waitGone("│ Ada ")
		u.waitGone("preview owners")
		// back to lite, the row under the Connections title (the toolbar's
		// chip says lite2 now, so " lite " is not a safe anchor): its
		// result is back on its result tab, sorted as it was left, and
		// the log is lite's again
		x, y := u.find("Connections")
		u.clickAt(x, y+1)
		u.waitFor("Log · lite ─", "1 preview owners", "│ Ada ", "│ Bo ")
		u.waitUntil("Bo still above Ada", func(string) bool {
			_, bo := u.find("│ Bo ")
			_, ada := u.find("│ Ada ")
			return bo < ada
		})
	})

	step(t, "F1 opens the keys dialog, Esc closes it", func() {
		u.key(uv.KeyF1)
		// what the first screen of the dialog holds (Query tabs is below
		// the fold since the Anywhere group grew ^J and ⌥J)
		u.waitFor("Keys · F1 or ?", "Editor", "Anywhere", "pipelines & jobs — run")
		u.key(uv.KeyEscape)
		u.waitGone("Keys · F1 or ?")
	})

	step(t, "Ctrl+B folds the sidebar away and back", func() {
		u.ctrl('b')
		u.waitGone("Connections")
		u.ctrl('b')
		u.waitFor("Connections", "● lite")
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

	step(t, "Shift+F12 lists the uses; Enter goes to one", func() {
		// the caret is still at the end, on the alias's declaration
		u.raw("\x1b[24;2~") // Shift+F12, as xterm sends it (see raw)
		u.waitFor("Uses of alias dg · 2", "declared")
		u.key(uv.KeyUp) // the use in SELECT
		u.key(uv.KeyEnter)
		u.waitGone("Uses of alias dg")
		// the caret is on dg.name: typing lands there
		u.typeText("x")
		u.waitFor("SELECT xdg.name FROM dogs dg")
		u.ctrl('z')
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

		// e edits it again; this time it compiles, and Enter runs it. It
		// shows two results, so the results title offers a switcher
		e.nextEdit(t, "// Lists the cats.\npackage main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\n"+
			"func Run(s *sdb.S) error {\n\tw, err := s.Query(\"lite\", \"SELECT 1 AS warmup\")\n"+
			"\tif err != nil {\n\t\treturn err\n\t}\n\ts.Show(w)\n"+
			"\tr, err := s.Query(\"lite\", \"SELECT name AS cat FROM cats ORDER BY name\")\n"+
			"\tif err != nil {\n\t\treturn err\n\t}\n\ts.Print(\"%d cats\", len(r.Rows))\n\ts.Show(r)\n\treturn nil\n}\n")
		u.key('e')
		u.waitFor("nightly.go saved — checked, no problems", "Lists the cats.")
		u.key(uv.KeyEnter)
		u.waitFor("script nightly.go completed", "3 cats", "│ cat ", "│ Leo ", "Result 1 · 2")

		// [ in the grid goes back to the first s.Show; a click on the 2
		// on the title comes back to the last
		u.click("│ Leo ", 3) // the grid has the keyboard now
		u.key('[')
		u.waitFor("warmup │")
		u.waitGone("│ Leo ")
		u.click("Result 1 · 2", ansi.StringWidth("Result 1 · ")) // the 2
		u.waitFor("│ Leo ")
		u.waitGone("warmup │")

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

	step(t, "Ctrl+J: pipelines and jobs run, the monitor, the Runs list, a stop, $EDITOR and the check", func() {
		pipelinesAndJobs(t, e, u)
	})

	step(t, "Ctrl+O plugins: an example copied, $EDITOR, the check and the load, a pipeline placing it", func() {
		userPlugins(t, e, u)
	})

	step(t, "a snippet's name then Tab expands it; Tab steps its stops, a stop used twice mirrors; ⌥S lists them", func() {
		// a tab of its own: an empty editor, nothing earlier steps rely on
		u.key('t', uv.ModAlt)
		u.waitFor("Type SQL here")
		u.click("Type SQL here", 0)
		// typed a key at a time: the completion popup opens on "cn", and
		// Tab on the whole name still expands rather than picks
		u.typeText("cnt")
		u.key(uv.KeyTab)
		u.waitFor("SELECT column, count(*) AS n", "FROM table", "GROUP BY column", "Tab next field")
		// words with no completions, so no popup claims the next Tab
		u.typeText("qq")
		u.key(uv.KeyTab)
		u.typeText("xy")
		u.waitFor("SELECT xy, count(*) AS n", "FROM qq", "GROUP BY xy")
		u.key(uv.KeyTab) // past the last stop: the session is over
		u.key(uv.KeyTab) // and Tab is the focus key again
		u.key('s', uv.ModAlt)
		u.waitFor("Snippets", "wrap the selection in a CTE", "Enter insert")
		u.key(uv.KeyEscape)
		u.waitGone("Enter insert")
	})

	step(t, "Alt+Left at a line start steps back over the line break (N-326: it ended dbc)", func() {
		u.key('t', uv.ModAlt)
		u.waitFor("Type SQL here")
		u.click("Type SQL here", 0)
		u.typeText("SELECT 1;")
		u.key(uv.KeyEnter)
		u.typeText("SELECT 2;")
		u.key(uv.KeyHome)
		u.key(uv.KeyLeft, uv.ModAlt) // over the newline and the ;, to before the 1
		u.typeText("x")
		u.waitFor("SELECT x1;", "SELECT 2;")
		select {
		case <-u.done:
			t.Fatal("dbc exited")
		default:
		}
	})

	step(t, "a script printing 10,000 lines finishes in a moment, its last lines in the log (N-278)", func() {
		// one frame per printed line, as before N-278, took 7.2 s here (an
		// M1 Pro); batched, about 40 ms. The bound leaves slow machines
		// two orders of magnitude of room and still catches a frame per line.
		var src strings.Builder
		src.WriteString("// Prints a lot.\npackage main\n\nimport (\n\t\"fmt\"\n\n\t\"github.com/rohanthewiz/dbc/sdb\"\n)\n\n")
		src.WriteString("func Run(s *sdb.S) error {\n\tfor i := 0; i < 10000; i++ {\n\t\ts.Print(fmt.Sprintf(\"printed %d\", i))\n\t}\n\treturn nil\n}\n")
		e.writeScript(t, "chatty.go", src.String())
		u.ctrl('o')
		u.waitFor("chatty.go", "Prints a lot.")
		u.key('/')
		u.typeText("chatty")
		u.key(uv.KeyEnter) // in the filter, Enter is the row's action: run
		began := time.Now()
		u.waitFor("script chatty.go completed", "printed 9999")
		took := time.Since(began)
		t.Logf("10,000 prints landed in %s", took.Round(time.Millisecond))
		if took > 3*time.Second {
			t.Errorf("10,000 prints took %s: a frame per line again?", took.Round(time.Millisecond))
		}
	})

	step(t, "typing into a 3.5 MB buffer keeps up with the keys (N-279)", func() {
		// a tab of its own, the migration pasted in whole (one bracketed
		// paste, as a terminal sends one)
		u.key('t', uv.ModAlt)
		u.waitFor("Type SQL here")
		u.click("Type SQL here", 0)
		var big strings.Builder
		last := 0
		for i := 0; big.Len() < 3_500_000; i++ {
			fmt.Fprintf(&big, "-- step %d\nCREATE TABLE t%d (id integer primary key, name text not null, note text default 'x;y');\n", i, i)
			last = i
		}
		u.raw("\x1b[200~" + big.String() + "\x1b[201~")
		u.waitFor(fmt.Sprintf("CREATE TABLE t%d (", last))
		// 60 letters in one burst, as a held key repeats them: each was
		// ~110 ms before N-279 (the buffer joined, lexed and split twice,
		// on an M1 Pro), so the burst took seconds; now ~2 ms a letter.
		began := time.Now()
		u.raw(strings.Repeat("q", 60))
		u.waitFor(strings.Repeat("q", 60))
		took := time.Since(began)
		t.Logf("60 letters into 3.5 MB landed in %s", took.Round(time.Millisecond))
		if took > 2*time.Second {
			t.Errorf("60 letters into a 3.5 MB buffer took %s: the whole buffer per keystroke again?", took.Round(time.Millisecond))
		}
	})

	step(t, "formatting a deeply nested generated query keeps the editor (N-281)", func() {
		// an ORM's left-nested disjunction, 20,000 deep (~290 KB on one
		// line), pasted into a tab of its own and formatted. Formatting
		// runs on the event loop; before N-281 each level that did not fit
		// measured its whole subtree, and this took ~27 s with the editor
		// frozen. Now ~50 ms.
		u.key('t', uv.ModAlt)
		u.waitFor("Type SQL here")
		u.click("Type SQL here", 0)
		const depth = 20000
		var q strings.Builder
		q.WriteString("SELECT * FROM t WHERE " + strings.Repeat("(", depth) + "a = 0")
		for i := 1; i <= depth; i++ {
			fmt.Fprintf(&q, " OR a = %d)", i)
		}
		u.raw("\x1b[200~" + q.String() + "\x1b[201~")
		// the view follows the caret to the end of the one long line
		u.waitFor(fmt.Sprintf("OR a = %d)", depth-1))
		began := time.Now()
		u.raw("\x1bF") // Alt+Shift+F as a legacy terminal sends it: ESC F
		u.waitFor("formatted the buffer")
		took := time.Since(began)
		t.Logf("formatting a %d-deep OR chain (%d KB) took %s", depth, q.Len()/1024, took.Round(time.Millisecond))
		if took > 3*time.Second {
			t.Errorf("formatting a %d-deep query took %s: a measurement per level of its whole subtree again?", depth, took.Round(time.Millisecond))
		}
		// the clauses on lines of their own: it was laid out, not refused
		u.waitFor("SELECT *", "FROM t", "WHERE")
	})

	step(t, "a 300,000-row result sorts and exports without freezing the screen (N-282)", func() {
		// max_rows is raised in this config (writeConfig). Before N-282 a
		// sort of this many rows of mixed-case text ran in Update, lower-
		// casing both sides of every comparison: about 1.5 s with the
		// screen frozen, and the export's projection and render after it.
		// Now the sort's keys are made once and, past 50,000 rows, the
		// sort runs off the loop, the strip saying "sorting by …"; the
		// export is written by a command.
		u.key('t', uv.ModAlt)
		u.waitFor("Type SQL here")
		u.click("Type SQL here", 0)
		// every label mixed-case, with a long shared prefix: the shape that
		// made each comparison lower-case two strings and read far into
		// them (labels like "A1", "b2" sorted in 0.1 s even then). The
		// numbers are a scramble of i (7919 is prime to 300,000), so each
		// is used once and the sort has work to do.
		u.typeText("WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300000) " +
			"SELECT i AS id, 'Customer ' || ((i * 7919 + 13) % 300000) || ' of Region ' || (i % 37) AS label FROM n;")
		u.ctrl('r')
		u.waitFor("300000 rows")
		began := time.Now()
		u.click("│ label", 3)
		u.waitUntil("the strip answers the click", func(s string) bool {
			return strings.Contains(s, "sorting by label asc…") || strings.Contains(s, "sorted by label asc")
		})
		answered := time.Since(began)
		u.waitFor("sorted by label asc", "│ Customer 0 of Region ")
		t.Logf("a 300,000-row sort: the strip answered in %s, the rows moved in %s",
			answered.Round(time.Millisecond), time.Since(began).Round(time.Millisecond))
		if answered > time.Second {
			t.Errorf("the screen took %s to answer a header click: is the sort back in Update?", answered.Round(time.Millisecond))
		}

		path := filepath.Join(e.home, "big.csv")
		u.ctrl('e')
		u.typeText(path) // typing on the format row starts the path
		u.key(uv.KeyEnter)
		u.waitFor("exported the result (300000 rows)")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		// in the grid's order: sorted by label, so Customer 0 first
		if len(lines) != 300001 || lines[0] != "id,label" || !strings.Contains(lines[1], ",Customer 0 of Region ") {
			t.Errorf("the export has %d lines, starting %q", len(lines), lines[:min(3, len(lines))])
		}
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
