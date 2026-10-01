package e2e

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// TestWeb walks one browser window through the workbench, a step per
// subtest. The steps share the window and build on each other — a
// connection switched to is the next step's starting point — as a person's
// session does, which is also what makes the suite catch state bugs a
// fresh page per check would hide. So the first step that fails ends the
// run: what follows would only fail for its reason.
//
// Each step must also leave the page without a JavaScript error (see
// probe); one that logs any fails, even if everything on screen was right.
func TestWeb(t *testing.T) {
	e := setup(t)
	p := e.page(t, "lite")

	steps := []struct {
		name string
		fn   func(t *testing.T, e *env, p *rod.Page)
	}{
		{"signed-out page", signedOut},
		{"boot", boot},
		{"run a query", runQuery},
		{"tables sidebar", tablesSidebar},
		{"show columns from the menu", columnsFromMenu},
		{"show columns with c", columnsWithKey},
		{"copy chords on a table", copyChordsOnTable},
		{"copy out of the columns grid", copyColumnsGrid},
		{"switch connections", switchConns},
		{"disconnect and reconnect", disconnect},
		{"connection form fields and DSN", connForm},
		{"postgres schema picker", pgSchemaPicker},
		{"tabs survive a reload", tabsSurviveReload},
		{"sidebar fold keys", sidebarFoldKeys},
	}
	for _, s := range steps {
		ok := t.Run(s.name, func(t *testing.T) {
			// rod's Must* calls panic; recovering here turns one into this
			// step's failure (with the page's state) instead of a crash
			// that would skip every later step's report
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v\n%s", r, pageState(p))
				}
			}()
			s.fn(t, e, p)
			if errs := jsErrors(t, p); len(errs) > 0 {
				t.Fatalf("the page logged JavaScript errors:\n  %s", strings.Join(errs, "\n  "))
			}
		})
		if !ok {
			break
		}
	}
}

// signedOut: a browser without the session cookie gets the sign-in notice,
// never the workbench; the login link is the only way in.
func signedOut(t *testing.T, e *env, _ *rod.Page) {
	res, err := http.Get(e.base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), `id="editor"`) || !strings.Contains(string(body), "not signed in") {
		t.Fatalf("an unsigned request did not get the sign-in notice (%s):\n%s", res.Status, body)
	}
}

// boot: the signed-in page is on the default connection, Monaco has
// replaced the textarea, and the window has a query tab.
func boot(t *testing.T, _ *env, p *rod.Page) {
	waitFor(t, p, "Monaco", `() => !!document.querySelector("#monaco .monaco-editor")`)
	if got := evalStr(t, p, `() => [...document.querySelectorAll("#qtabs .qtab .qt")].map((x) => x.textContent).join(",")`); got != "Query 1" {
		t.Fatalf("query tabs = %q, want Query 1", got)
	}
	if got := evalStr(t, p, `() => [...document.querySelectorAll("#conns .conn-item")].map((b) => b.dataset.conn).join(",")`); !strings.HasPrefix(got, "lite,lite2") {
		t.Fatalf("connections = %q, want lite,lite2 first", got)
	}
}

// runQuery: ▶ Run runs the statement in the editor, and its rows land in
// the grid with the count beside it.
func runQuery(t *testing.T, _ *env, p *rod.Page) {
	before := gridSeq(t, p)
	eval(t, p, `() => dbc.editor.setText("SELECT name, age FROM cats ORDER BY id")`)
	p.MustElement("#run").MustClick()
	if info := waitResult(t, p, before, "name", "age"); info != "3 rows" {
		t.Fatalf("grid info = %q, want 3 rows", info)
	}
	cells := evalStr(t, p, `() => document.querySelector("#grid .gb").textContent`)
	for _, want := range []string{"Tom", "Mia", "Leo"} {
		if !strings.Contains(cells, want) {
			t.Fatalf("grid cells %q lack %s", cells, want)
		}
	}
}

// tablesSidebar: the list holds the connection's table with its row count
// (which lands a moment after the list); SQLite has one schema, so the
// schema picker stays hidden.
func tablesSidebar(t *testing.T, _ *env, p *rod.Page) {
	waitFor(t, p, "cats with its row count", `() => {
	  const r = document.querySelector('#tables li[data-name="cats"] .rows');
	  return !!r && r.textContent.trim() === "(3)";
	}`)
	if hidden := eval(t, p, `() => document.getElementById("table-filter").hidden`); hidden != true {
		t.Fatal("the schema picker is shown for SQLite's single schema")
	}
	if got := evalStr(t, p, `() => document.getElementById("table-count").textContent`); got != "· 1" {
		t.Fatalf("table count = %q, want · 1", got)
	}
}

// catsColumns is what Show columns lists for cats, in order.
var catsColumns = []string{"id", "name", "breed", "age"}

// columnsHeader is the header every Show columns result has.
var columnsHeader = []string{"ordinal_position", "column_name", "data_type", "is_nullable"}

// columnsFromMenu: the table's right-click menu runs Show columns, one row
// per column of the table.
func columnsFromMenu(t *testing.T, _ *env, p *rod.Page) {
	before := gridSeq(t, p)
	reqs(t, p)
	rightClick(t, p, `#tables li[data-name="cats"]`)
	menuPick(t, p, "Show columns")
	if info := waitResult(t, p, before, columnsHeader...); info != "4 rows" {
		t.Fatalf("grid info = %q, want 4 rows (one per column of cats)", info)
	}
	if !hasReq(reqs(t, p), "POST", "/columns") {
		t.Fatal("Show columns did not POST …/columns")
	}
}

// resetGrid puts an unrelated one-row result in the grid, so the next
// check can tell whether Show columns ran.
func resetGrid(t *testing.T, p *rod.Page) {
	t.Helper()
	before := gridSeq(t, p)
	eval(t, p, `() => dbc.editor.setText("SELECT 1 AS x")`)
	p.MustElement("#run").MustClick()
	waitResult(t, p, before, "x")
}

// selectTable clicks a table in the list, which selects and focuses it —
// where the list's keys act.
func selectTable(t *testing.T, p *rod.Page, name string) {
	t.Helper()
	p.MustElement(`#tables li[data-name="` + name + `"]`).MustClick()
	waitFor(t, p, name+" selected and focused", `(n) => {
	  const li = document.activeElement;
	  return !!li && li.matches("#tables li.sel") && li.dataset.name === n;
	}`, name)
}

// columnsWithKey: a plain c on the selected table is Show columns.
func columnsWithKey(t *testing.T, _ *env, p *rod.Page) {
	resetGrid(t, p)
	selectTable(t, p, "cats")
	before := gridSeq(t, p)
	p.Keyboard.MustType(input.KeyC)
	if info := waitResult(t, p, before, columnsHeader...); info != "4 rows" {
		t.Fatalf("grid info = %q, want 4 rows", info)
	}
}

// copyChordsOnTable: ⌘C and Ctrl+C on the selected table are not "c" —
// the list's handler leaves any chord alone (app.js: "plain c only") — so
// neither runs Show columns, and ⌘C still copies the page's text
// selection, as everywhere else.
func copyChordsOnTable(t *testing.T, _ *env, p *rod.Page) {
	resetGrid(t, p)
	selectTable(t, p, "cats")
	// select the row's name as a person would by dragging over it; focus
	// stays on the row, so the chord's keydown reaches the list's handler
	eval(t, p, `() => {
	  const li = document.querySelector('#tables li[data-name="cats"]');
	  const r = document.createRange();
	  r.selectNodeContents(li.firstChild.nodeType === 3 ? li : li.querySelector("span") || li);
	  const s = getSelection(); s.removeAllRanges(); s.addRange(r);
	}`)
	setClipboard(t, p, "sentinel")
	before := gridSeq(t, p)
	reqs(t, p)

	// ⌘C with the copy command a Mac's menu would send; Ctrl+C, which is
	// the copy chord elsewhere (and the page's ⌘ twin)
	chord(t, p, modMeta, "c", "KeyC", 67, "copy")
	chord(t, p, modCtrl, "c", "KeyC", 67)
	time.Sleep(700 * time.Millisecond) // the time a /columns run would need to show up

	if r := reqs(t, p); hasReq(r, "POST", "/columns") {
		t.Fatalf("a copy chord on a table ran Show columns: %v", r)
	}
	if gridSeq(t, p) != before {
		t.Fatal("a copy chord on a table changed the grid")
	}
	if got := clipboard(t, p); !strings.Contains(got, "cats") {
		t.Fatalf("⌘C on the selected table copied %q, want the selected text (cats)", got)
	}
	eval(t, p, `() => getSelection().removeAllRanges()`)
}

// copyColumnsGrid: Show columns' rows copy out of the grid like any
// result's — the point of N-063 — ⌘A then y gives them tab-separated, one
// line per column of the table, in order.
func copyColumnsGrid(t *testing.T, _ *env, p *rod.Page) {
	selectTable(t, p, "cats")
	before := gridSeq(t, p)
	p.Keyboard.MustType(input.KeyC)
	waitResult(t, p, before, columnsHeader...)
	waitFor(t, p, "the grid focused", `() => document.activeElement === document.getElementById("grid")`)

	setClipboard(t, p, "sentinel")
	chord(t, p, modMeta, "a", "KeyA", 65) // the grid's own select-all (no editing command: it is the page's)
	waitFor(t, p, "the whole grid selected", `() => dbc.grid.view().sel`)
	p.Keyboard.MustType(input.KeyY)
	waitFor(t, p, "the copy logged", `() => [...document.querySelectorAll("#log > div")].some((d) => d.textContent.includes("copied"))`)

	lines := strings.Split(strings.TrimRight(clipboard(t, p), "\n"), "\n")
	if len(lines) != len(catsColumns) {
		t.Fatalf("copied %d lines, want %d:\n%s", len(lines), len(catsColumns), strings.Join(lines, "\n"))
	}
	for i, l := range lines {
		f := strings.Split(l, "\t")
		if len(f) < 2 || f[0] != fmt.Sprint(i+1) || f[1] != catsColumns[i] {
			t.Fatalf("line %d = %q, want %d<TAB>%s<TAB>…", i+1, l, i+1, catsColumns[i])
		}
	}
}

// switchConns: a click on another connection moves the tab there and
// redraws the Tables list with its tables; and back again.
func switchConns(t *testing.T, _ *env, p *rod.Page) {
	p.MustElement(`#conns .conn-item[data-conn="lite2"]`).MustClick()
	waitConnected(t, p, "lite2")
	waitFor(t, p, "lite2's tables", `() => [...document.querySelectorAll("#tables li[data-name]")].map((l) => l.dataset.name).join() === "dogs"`)

	p.MustElement(`#conns .conn-item[data-conn="lite"]`).MustClick()
	waitConnected(t, p, "lite")
	waitFor(t, p, "lite's tables", `() => !!document.querySelector('#tables li[data-name="cats"]')`)
}

// disconnect: the connection menu's Disconnect takes the tab off its
// connection, which stays listed; a click connects again.
func disconnect(t *testing.T, _ *env, p *rod.Page) {
	rightClick(t, p, `#conns .conn-item[data-conn="lite"]`)
	menuPick(t, p, "Disconnect")
	waitFor(t, p, "not connected", `() =>
	  document.getElementById("active-conn").textContent === "not connected" &&
	  !document.querySelector("#conns .conn-item.active") &&
	  !document.querySelector("#tables li[data-name]") &&
	  !!document.querySelector('#conns .conn-item[data-conn="lite"]')`)

	p.MustElement(`#conns .conn-item[data-conn="lite"]`).MustClick()
	waitConnected(t, p, "lite")
}

// connForm: the Add a connection form shows the rows its driver and its
// "Enter as" call for, and a connection given as fields tests, saves and
// connects.
func connForm(t *testing.T, e *env, p *rod.Page) {
	p.MustElement("#conn-add").MustClick()
	waitFor(t, p, "the form", `() => !!document.getElementById("cf-name")`)

	// shown reports which of the form's controls a person can see. Hidden
	// is decided per row (label + control), so a hidden ancestor counts.
	shown := func(ids ...string) map[string]bool {
		v, _ := eval(t, p, `(ids) => Object.fromEntries(ids.map((id) => {
		  const e = document.getElementById(id);
		  return [id, !!e && !e.closest("[hidden]") && e.getClientRects().length > 0];
		}))`, ids).(map[string]any)
		out := map[string]bool{}
		for k, b := range v {
			out[k], _ = b.(bool)
		}
		return out
	}
	expect := func(when string, want map[string]bool) {
		t.Helper()
		ids := make([]string, 0, len(want))
		for id := range want {
			ids = append(ids, id)
		}
		got := shown(ids...)
		for id, w := range want {
			if got[id] != w {
				t.Fatalf("%s: #%s shown = %v, want %v", when, id, got[id], w)
			}
		}
	}

	// postgres (the first driver), as fields: the server's fields and TLS
	expect("postgres, fields", map[string]bool{"cf-host": true, "cf-user": true, "cf-password": true,
		"cf-database": true, "cf-file": false, "cf-dsn": false, "cf-tls": true})
	// as a DSN: the one text field instead
	p.MustElement("#cf-mode-d").MustClick()
	expect("postgres, DSN", map[string]bool{"cf-host": false, "cf-user": false, "cf-dsn": true, "cf-tls": true})
	// sqlite, as fields: a file, no host, no TLS
	p.MustElement("#cf-driver").MustSelect("sqlite")
	p.MustElement("#cf-mode-f").MustClick()
	expect("sqlite, fields", map[string]bool{"cf-host": false, "cf-file": true, "cf-options": true,
		"cf-dsn": false, "cf-tls": false})

	p.MustElement("#cf-name").MustInput("lite3")
	p.MustElement("#cf-file").MustInput(filepath.Join(e.home, "lite3.db"))
	p.MustElementR(".modal button", "^Test connection$").MustClick()
	waitFor(t, p, "the test's verdict", `() => {
	  const r = document.querySelector(".connresult");
	  return !!r && !r.hidden && r.textContent.startsWith("✓");
	}`)
	p.MustElementR(".modal button", "^Save$").MustClick()
	waitFor(t, p, "the form closed", `() => !document.querySelector(".modal")`)
	// Save switches the tab to the new connection; it has no tables, so
	// waitConnected (which waits for the list) is not the right wait
	waitFor(t, p, "connected to lite3", `() => dbc.state.active === "lite3" &&
	  !!document.querySelector('#conns .conn-item.active[data-conn="lite3"][data-saved]')`)
	if b, err := os.ReadFile(filepath.Join(e.home, ".config", "dbc", "connections.toml")); err != nil || !strings.Contains(string(b), "lite3") {
		t.Fatalf("lite3 is not in connections.toml (%v):\n%s", err, b)
	}

	p.MustElement(`#conns .conn-item[data-conn="lite"]`).MustClick()
	waitConnected(t, p, "lite")
}

// pgSchemaPicker: on Postgres the Tables list is one schema's, and the
// schema picker types to filter and loads the pick's tables — the part of
// the sidebar SQLite cannot show. Show columns works on a schema-qualified
// table there too. Runs only with DBC_LIVE_PG_DSN.
func pgSchemaPicker(t *testing.T, e *env, p *rod.Page) {
	if e.pgDSN == "" {
		t.Skip("set DBC_LIVE_PG_DSN to check the Postgres schema picker")
	}
	p.MustElement(`#conns .conn-item[data-conn="pg"]`).MustClick()
	waitFor(t, p, "connected to pg with its schema picker", `() => dbc.state.active === "pg" &&
	  !document.querySelector("#conns .conn-item.connecting") &&
	  !document.getElementById("table-filter").hidden`)

	box := p.MustElement("#table-schema")
	box.MustClick()
	waitFor(t, p, "the schema list", `() => !document.getElementById("schema-list").hidden`)
	box.MustInput("e2e_b")
	waitFor(t, p, "the list narrowed to e2e_b", `() => {
	  const rows = [...document.querySelectorAll("#schema-list li")].filter((li) => !li.hidden);
	  return rows.length >= 1 && rows[0].textContent.includes("e2e_b");
	}`)
	p.Keyboard.MustType(input.Enter)
	waitFor(t, p, "e2e_b's tables", `() =>
	  [...document.querySelectorAll("#tables li[data-name]")].map((l) => l.dataset.name).join() === "e2e_b.beta"`)

	selectTable(t, p, "e2e_b.beta")
	before := gridSeq(t, p)
	p.Keyboard.MustType(input.KeyC)
	if info := waitResult(t, p, before, columnsHeader...); info != "2 rows" {
		t.Fatalf("grid info = %q, want 2 rows (id, label)", info)
	}

	p.MustElement(`#conns .conn-item[data-conn="lite"]`).MustClick()
	waitConnected(t, p, "lite")
}

// tabsSurviveReload: a new tab, its text and a rename are saved on the
// server, so a reload of the window brings all three back, on that tab.
func tabsSurviveReload(t *testing.T, _ *env, p *rod.Page) {
	p.MustElement("#qnew").MustClick()
	waitFor(t, p, "Query 2 open", `() => {
	  const on = document.querySelector("#qtabs .qtab.on .qt");
	  return !!on && on.textContent === "Query 2" && !!dbc.state.ws;
	}`)

	// typed, not setText: only an edit fires the debounced tab save
	eval(t, p, `() => dbc.editor.focus()`)
	p.MustInsertText("SELECT 42 AS answer")

	// a double-click renames: two presses, the second with clickCount 2,
	// which is how Chrome tells a double-click from two clicks
	tab := p.MustElement("#qtabs .qtab.on .qt")
	shape := tab.MustShape().Box()
	p.Mouse.MustMoveTo(shape.X+shape.Width/2, shape.Y+shape.Height/2)
	p.Mouse.MustClick(proto.InputMouseButtonLeft)
	if err := p.Mouse.Click(proto.InputMouseButtonLeft, 2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, p, "the rename box", `() => document.activeElement && document.activeElement.matches("input.qrename")`)
	p.MustInsertText("Renamed tab") // replaces the selected old title
	p.Keyboard.MustType(input.Enter)

	// wait for the server to hold both, rather than for a debounce's worth
	// of time: the reload must not race the save
	waitFor(t, p, "the tab saved", `async () => {
	  const r = await (await fetch("/api/v1/tabs")).json();
	  return (r.data || []).some((t) => t.title === "Renamed tab" && t.buffer === "SELECT 42 AS answer");
	}`)

	p.MustReload()
	p.MustWaitLoad()
	waitConnected(t, p, "lite")
	waitFor(t, p, "the tabs back", `() =>
	  [...document.querySelectorAll("#qtabs .qtab .qt")].map((x) => x.textContent).join() === "Query 1,Renamed tab" &&
	  document.querySelector("#qtabs .qtab.on .qt").textContent === "Renamed tab"`)
	waitFor(t, p, "its text back", `() => dbc.editor.text() === "SELECT 42 AS answer"`)
}

// hasReq reports whether a fetch with method went to a path ending in tail.
func hasReq(reqs []string, method, tail string) bool {
	for _, r := range reqs {
		m, u, _ := strings.Cut(r, " ")
		if m == method && strings.HasSuffix(strings.SplitN(u, "?", 2)[0], tail) {
			return true
		}
	}
	return false
}

// sidebarFoldKeys: Ctrl+B and ⌘B fold the sidebar and bring it back, from
// the page and from inside the editor. In the editor the key is Monaco's to
// hand on (editor.js bindKeys): on a Mac it takes Ctrl+B for emacs-style
// cursor-left, so before Ctrl+B was bound there it moved the caret instead
// of folding — found driving Firefox (N-061), and the same in Chrome.
func sidebarFoldKeys(t *testing.T, _ *env, p *rod.Page) {
	folded := func() bool {
		b, _ := eval(t, p, `() => document.querySelector(".app").classList.contains("side-off")`).(bool)
		return b
	}
	for _, where := range []struct{ name, focus string }{
		{"the page", `() => { document.activeElement && document.activeElement.blur(); }`},
		{"the editor", `() => dbc.editor.focus()`},
	} {
		for _, m := range []struct {
			name string
			bit  int
		}{{"Ctrl+B", modCtrl}, {"⌘B", modMeta}} {
			if folded() {
				t.Fatalf("folded before %s in %s", m.name, where.name)
			}
			eval(t, p, where.focus)
			chord(t, p, m.bit, "b", "KeyB", 66)
			if !folded() {
				t.Fatalf("%s in %s did not fold the sidebar", m.name, where.name)
			}
			// a fold moves focus out of the column it hides, so this round's
			// focus is set again before the key that brings it back
			eval(t, p, where.focus)
			chord(t, p, m.bit, "b", "KeyB", 66)
			if folded() {
				t.Fatalf("%s in %s did not bring the sidebar back", m.name, where.name)
			}
		}
	}
}
