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
		{"editor completion", completion},
		{"go to and rename an alias and a column", aliasRename},
		{"tables sidebar", tablesSidebar},
		{"show columns from the menu", columnsFromMenu},
		{"show columns with c", columnsWithKey},
		{"find a table by typing", findTable},
		{"copy chords on a table", copyChordsOnTable},
		{"copy out of the columns grid", copyColumnsGrid},
		{"transpose the grid", transposeGrid},
		{"switch connections", switchConns},
		{"result tabs and logs per connection", resultTabsPerConn},
		{"history scoped to the database", historyScope},
		{"disconnect and reconnect", disconnect},
		{"refresh a connection", refreshConn},
		{"connection form fields and DSN", connForm},
		{"postgres schema picker", pgSchemaPicker},
		{"tabs survive a reload", tabsSurviveReload},
		{"sidebar fold keys", sidebarFoldKeys},
		{"other tabs' connections", otherTabsConns},
		{"consoles per database", consolesPerDatabase},
		{"code blocks highlighted", codeHighlight},
		{"tab groups", tabGroups},
		{"fold the recent conversations", foldRecentChats},
		{"script tabs", scriptTabs},
	}
	// DBC_E2E_STEPS narrows a run to the steps whose names contain one of
	// its comma-separated words (plus the sign-in and boot every step
	// stands on), for iterating on one step without the whole suite
	only := strings.Split(os.Getenv("DBC_E2E_STEPS"), ",")
	wanted := func(name string) bool {
		if only[0] == "" || name == "signed-out page" || name == "boot" {
			return true
		}
		for _, w := range only {
			if w != "" && strings.Contains(name, w) {
				return true
			}
		}
		return false
	}
	for _, s := range steps {
		if !wanted(s.name) {
			continue
		}
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

// completion: typing "c." after the alias is in the statement opens
// Monaco's suggest widget on the table's columns, from the schema
// (POST …/complete, package sqlcomplete); typing on narrows it, and Tab
// picks. A suggestion that never came, or came word-based, fails here.
func completion(t *testing.T, _ *env, p *rod.Page) {
	eval(t, p, `() => {
	  dbc.editor.setText("SELECT  FROM cats c");
	  const ed = monaco.editor.getEditors()[0];
	  ed.setPosition({ lineNumber: 1, column: 8 });
	  ed.focus();
	}`)
	p.Keyboard.MustType(input.KeyC, input.Period)
	rows := `() => [...document.querySelectorAll(".suggest-widget.visible .monaco-list-row")]
	  .map((r) => r.getAttribute("aria-label") || r.textContent).join("|")`
	waitFor(t, p, "the suggest widget on cats' columns", `() => {
	  const r = (`+rows+`)();
	  return ["breed", "age", "name"].every((c) => r.includes(c));
	}`)
	p.Keyboard.MustType(input.KeyB, input.KeyR)
	waitFor(t, p, "the list narrowed to breed", `() => {
	  const r = (`+rows+`)();
	  return r.includes("breed") && !r.includes("age");
	}`)
	p.Keyboard.MustType(input.Tab)
	waitFor(t, p, "the pick in the editor", `() => dbc.editor.text() === "SELECT c.breed FROM cats c"`)
}

// aliasRename: F12 on a qualifier goes to where its alias is declared, and
// F2 renames the alias — only the outer one, not the subquery's own alias
// of the same name (POST …/symbol and …/rename, sqlcomplete's resolver).
// The renamed statement still runs. Shift+F12 on a CTE's column opens
// Monaco's references list with every use, and F2 renames the column at
// its declaration and its uses; that statement runs with the new column
// name. F2 on a catalog table's column says why no rename box opens.
func aliasRename(t *testing.T, _ *env, p *rod.Page) {
	eval(t, p, `() => {
	  dbc.editor.setText("SELECT c.name FROM cats c WHERE c.age > (SELECT avg(c.age) FROM cats c)");
	  const ed = monaco.editor.getEditors()[0];
	  ed.setPosition({ lineNumber: 1, column: 8 }); // on the first "c"
	  ed.focus();
	}`)
	p.Keyboard.MustType(input.F12)
	// the outer alias is the "c" after "cats ", at column 25
	waitFor(t, p, "the caret on the alias's declaration",
		`() => monaco.editor.getEditors()[0].getPosition().column === 25`)

	p.Keyboard.MustType(input.F2)
	// focused, not just shown: the box is drawn a moment before it takes
	// the keyboard, and typing sooner lands in the editor
	waitFor(t, p, "the rename box, holding the alias and focused", `() => {
	  const i = document.querySelector(".rename-box input");
	  return !!i && i.value === "c" && document.activeElement === i;
	}`)
	p.MustInsertText("kitty")
	p.Keyboard.MustType(input.Enter)
	waitFor(t, p, "the outer alias renamed, the subquery's left alone", `() => dbc.editor.text() ===
	  "SELECT kitty.name FROM cats kitty WHERE kitty.age > (SELECT avg(c.age) FROM cats c)"`)

	before := gridSeq(t, p)
	p.MustElement("#run").MustClick()
	waitResult(t, p, before, "name")

	// a CTE's column: "n" is declared by AS n and used twice as t.n
	const cte = "WITH t AS (SELECT count(*) AS n FROM cats) SELECT t.n FROM t ORDER BY t.n"
	eval(t, p, `(sql) => {
	  dbc.editor.setText(sql);
	  const ed = monaco.editor.getEditors()[0];
	  ed.setPosition({ lineNumber: 1, column: 53 }); // on the first t.n's "n"
	  ed.focus();
	}`, cte)
	// Shift held across the key: rod's KeyActions would release it early
	if err := p.Keyboard.Press(input.ShiftLeft); err != nil {
		t.Fatal(err)
	}
	p.Keyboard.MustType(input.F12)
	if err := p.Keyboard.Release(input.ShiftLeft); err != nil {
		t.Fatal(err)
	}
	// one row per use, the file's own row left out as there is one file;
	// the second row (the caret's) comes selected
	waitFor(t, p, "the references list, with the declaration and both uses", `() => {
	  const rows = [...document.querySelectorAll(".reference-zone-widget .ref-tree .monaco-list-row")];
	  return rows.length === 3 && rows[0].textContent.includes("AS n FROM") && rows[1].classList.contains("selected");
	}`)
	p.Keyboard.MustType(input.Escape)
	waitFor(t, p, "the references list closed", `() => !document.querySelector(".reference-zone-widget .ref-tree .monaco-list-row")`)

	eval(t, p, `() => {
	  const ed = monaco.editor.getEditors()[0];
	  ed.setPosition({ lineNumber: 1, column: 53 });
	  ed.focus();
	}`)
	p.Keyboard.MustType(input.F2)
	waitFor(t, p, "the rename box, holding the column and focused", `() => {
	  const i = document.querySelector(".rename-box input");
	  return !!i && i.value === "n" && document.activeElement === i;
	}`)
	p.MustInsertText("total")
	p.Keyboard.MustType(input.Enter)
	waitFor(t, p, "the column renamed at its declaration and both uses", `() => dbc.editor.text() ===
	  "WITH t AS (SELECT count(*) AS total FROM cats) SELECT t.total FROM t ORDER BY t.total"`)
	before = gridSeq(t, p)
	p.MustElement("#run").MustClick()
	waitResult(t, p, before, "total")

	eval(t, p, `() => {
	  dbc.editor.setText("SELECT c.name FROM cats c");
	  const ed = monaco.editor.getEditors()[0];
	  ed.setPosition({ lineNumber: 1, column: 11 }); // on "name"
	  ed.focus();
	}`)
	p.Keyboard.MustType(input.F2)
	waitFor(t, p, "the reason there is nothing to rename", `() => [...document.querySelectorAll(".monaco-editor-overlaymessage")]
	  .some((m) => m.textContent.includes("Rename works on a table alias, a CTE name, or a column the query names itself"))`)
	p.Keyboard.MustType(input.Escape)
}

// tablesSidebar: the list holds the connection's table, without a row
// count until the heading's "rows" box is ticked (counts are opt-in, an
// exact count(*) each); ticked, the count lands a moment later, and
// unticked it goes. SQLite has one schema, so the schema picker stays
// hidden.
func tablesSidebar(t *testing.T, _ *env, p *rod.Page) {
	waitFor(t, p, "cats in the list", `() => !!document.querySelector('#tables li[data-name="cats"]')`)
	if ticked := eval(t, p, `() => document.getElementById("row-counts").checked`); ticked != false {
		t.Fatal("the rows box starts ticked")
	}
	if has := eval(t, p, `() => !!document.querySelector('#tables li[data-name="cats"] .rows')`); has != false {
		t.Fatal("cats has a row count before the rows box was ticked")
	}
	p.MustElement("#row-counts").MustClick()
	waitFor(t, p, "cats with its row count", `() => {
	  const r = document.querySelector('#tables li[data-name="cats"] .rows');
	  return !!r && r.textContent.trim() === "(3)";
	}`)
	waitFor(t, p, "the counting mark cleared", `() => !document.getElementById("row-counts-box").classList.contains("counting")`)
	p.MustElement("#row-counts").MustClick()
	waitFor(t, p, "the row count gone", `() => !document.querySelector('#tables li[data-name="cats"] .rows')`)
	if hidden := eval(t, p, `() => document.getElementById("table-filter").hidden`); hidden != true {
		t.Fatal("the schema picker is shown for SQLite's single schema")
	}
	if got := evalStr(t, p, `() => document.getElementById("table-count").textContent`); got != "· 1" {
		t.Fatalf("table count = %q, want · 1", got)
	}
}

// findTable: the table box narrows the list as you type and Enter
// previews the match; typing on a row starts a find there, and Tab goes
// on from the box to the selected row. lite has one table, cats, so a
// miss empties the list and a hit brings it back selected.
func findTable(t *testing.T, _ *env, p *rod.Page) {
	resetGrid(t, p)
	selectTable(t, p, "cats")
	// a letter that is not one of the row's keys (c, e) starts a find
	p.Keyboard.MustType(input.KeyZ)
	waitFor(t, p, "the find box with z, nothing matching", `() =>
	  document.activeElement === document.getElementById("table-find") &&
	  document.getElementById("table-find").value === "z" &&
	  !document.querySelector("#tables li[data-name]") &&
	  document.querySelector("#tables li.none").textContent.startsWith("no table matches") &&
	  document.getElementById("find-filter").classList.contains("on")`)
	// Esc drops the find: every table back, the first selected
	p.Keyboard.MustType(input.Escape)
	waitFor(t, p, "the find dropped", `() =>
	  document.activeElement === document.getElementById("table-find") &&
	  document.getElementById("table-find").value === "" &&
	  !!document.querySelector('#tables li.sel[data-name="cats"]') &&
	  !document.getElementById("find-filter").classList.contains("on")`)
	// "ats" is inside cats, not at its start: still a match
	p.Keyboard.MustType(input.KeyA, input.KeyT, input.KeyS)
	waitFor(t, p, "cats found by ats", `() =>
	  document.getElementById("table-find").value === "ats" &&
	  [...document.querySelectorAll("#tables li[data-name]")].map((l) => l.dataset.name).join() === "cats" &&
	  !!document.querySelector('#tables li.sel[data-name="cats"]')`)
	before := gridSeq(t, p)
	p.Keyboard.MustType(input.Enter)
	if info := waitResult(t, p, before, catsColumns...); info != "3 rows" {
		t.Fatalf("grid info = %q, want 3 rows (the preview of cats)", info)
	}
	// Tab goes on to the selected row, the list's one Tab stop
	eval(t, p, `() => document.getElementById("table-find").focus()`)
	p.Keyboard.MustType(input.Tab)
	waitFor(t, p, "the keyboard on cats' row", `() => {
	  const li = document.activeElement;
	  return !!li && li.matches('#tables li.sel[data-name="cats"]') && li.tabIndex === 0;
	}`)
	// and / goes back to the box, the find kept
	p.Keyboard.MustType(input.Slash)
	waitFor(t, p, "back in the find box", `() =>
	  document.activeElement === document.getElementById("table-find") &&
	  document.getElementById("table-find").value === "ats"`)
	// two Escs: the find dropped, then the box left
	p.Keyboard.MustType(input.Escape, input.Escape)
	waitFor(t, p, "out of the find box, every table shown", `() =>
	  document.activeElement !== document.getElementById("table-find") &&
	  document.getElementById("table-find").value === "" &&
	  !!document.querySelector('#tables li[data-name="cats"]')`)
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
	clickAt(t, p, `#tables li[data-name="`+name+`"]`, proto.InputMouseButtonLeft)
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

// transposeGrid: t turns the grid on its side — a column per record, a
// line per result column with its name in the gutter — and back. The
// arrows follow the screen (right is the next record, down the next
// column), a record's number selects it whole, and copies come out the
// way the grid shows them: y as values on their side, the Teams table with
// the names as row headers.
func transposeGrid(t *testing.T, _ *env, p *rod.Page) {
	before := gridSeq(t, p)
	eval(t, p, `() => dbc.editor.setText("SELECT id, name, age FROM cats ORDER BY id")`)
	p.MustElement("#run").MustClick()
	waitResult(t, p, before, "id", "name", "age")
	p.MustElement("#grid").MustFocus()

	p.Keyboard.MustType(input.KeyT)
	waitFor(t, p, "the grid transposed", `() => dbc.grid.view().flip &&
	  document.getElementById("flip-btn").getAttribute("aria-pressed") === "true" &&
	  document.getElementById("grid-info").textContent.includes("transposed") &&
	  document.querySelectorAll("#grid .gh .hc.rec").length === 3`)
	if names := evalStr(t, p, `() => [...document.querySelectorAll("#grid .gb .rn.fn")].map((d) => d.textContent).join(",")`); names != "id,name,age" {
		t.Fatalf("gutter names = %q, want id,name,age", names)
	}

	// right: the next record (Mia); down: the next column (name)
	p.Keyboard.MustType(input.ArrowRight, input.ArrowDown)
	waitFor(t, p, "the cursor on Mia's name", `() => { const c = dbc.grid.view().cur; return c.row === 1 && c.col === 1; }`)
	setClipboard(t, p, "sentinel")
	p.Keyboard.MustType(input.KeyY)
	waitFor(t, p, "Mia copied", `() => navigator.clipboard.readText().then((s) => s === "Mia")`)

	// record 3's number selects Leo whole; y copies him as a column
	p.MustElement(`#grid .gh .hc.rec[data-r="2"]`).MustClick()
	waitFor(t, p, "record 3 selected", `() => { const v = dbc.grid.view(); return v.sel && v.bounds.join() === "2,0,2,2"; }`)
	p.Keyboard.MustType(input.KeyY)
	waitFor(t, p, "Leo copied on his side", `() => navigator.clipboard.readText().then((s) => s === "3\nLeo\n1")`)

	// the Teams table: the selection (one record) as "column | value", the
	// names as row headers
	p.MustElement("#copy-btn").MustClick()
	p.MustElementR(".menu .mitem", "Table for Teams").MustClick()
	waitFor(t, p, "the HTML copy logged", `() => [...document.querySelectorAll("#log > div")].some((d) => d.textContent.includes("1×3 cells as a table, transposed"))`)
	html := evalStr(t, p, `async () => {
	  for (const it of await navigator.clipboard.read()) {
	    if (it.types.includes("text/html")) return await (await it.getType("text/html")).text();
	  }
	  return "";
	}`)
	for _, want := range []string{`>value</th>`, `<th scope="row"`, `>name</th>`, `>Leo</td>`} {
		if !strings.Contains(html, want) {
			t.Fatalf("the Teams copy lacks %s:\n%s", want, html)
		}
	}

	// N-112: a record's border widens every record, and stays under the
	// pointer — three narrow records cannot scroll, so the drag is shared
	// by the two widths that carry record 2's border; the release is no
	// click (record 3 stays selected)
	rec0 := evalNum(t, p, `() => dbc.grid.view().recW`)
	edgeJS := `() => document.querySelector('#grid .gh [data-r="1"]').getBoundingClientRect().right`
	edge0 := evalNum(t, p, edgeJS)
	x, y := handleCenter(t, p, `#grid .gh [data-rzr="1"]`)
	drag(t, p, x, y, x+40)
	if got := evalNum(t, p, `() => dbc.grid.view().recW`); got < rec0+19 || got > rec0+21 {
		t.Fatalf("record width %v → %v after a 40px drag of record 2's border, want +20", rec0, got)
	}
	if edge := evalNum(t, p, edgeJS); edge < edge0+40-2 || edge > edge0+40+2 {
		t.Fatalf("record 2's border %v → %v, want it to follow the pointer's 40px", edge0, edge)
	}
	if b := evalStr(t, p, `() => dbc.grid.view().bounds.join()`); b != "2,0,2,2" {
		t.Fatalf("the drag's release acted as a click: bounds %s", b)
	}
	// the names gutter: dragged wider, then a double-click fits it back
	// to the longest name
	fn0 := evalNum(t, p, `() => dbc.grid.view().fnW`)
	x, y = handleCenter(t, p, `#grid .gh [data-rzn]`)
	drag(t, p, x, y, x+30)
	if got := evalNum(t, p, `() => dbc.grid.view().fnW`); got < fn0+29 || got > fn0+31 {
		t.Fatalf("names gutter %v → %v after a 30px drag, want +30", fn0, got)
	}
	x, y = handleCenter(t, p, `#grid .gh [data-rzn]`)
	p.Mouse.MustMoveTo(x, y)
	p.Mouse.MustClick(proto.InputMouseButtonLeft) // a double-click is two presses (grid.js counts them)
	if err := p.Mouse.Click(proto.InputMouseButtonLeft, 2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, p, "the names fitted", `(w) => dbc.grid.view().fnW < w`, fn0+30)

	// a result wider than the pane, scrolled: the drag scrolls to keep the
	// dragged record's left edge put, so every record takes the whole 40px
	// and the border still follows the pointer. (The grid stays transposed
	// over a rerun, so the header holds numbers: wait on the info line.)
	before = gridSeq(t, p)
	eval(t, p, `() => dbc.editor.setText("WITH RECURSIVE n(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM n WHERE v < 200) SELECT v, 'x' AS t FROM n")`)
	p.MustElement("#run").MustClick()
	waitFor(t, p, "200 records, transposed", `(after) => dbc.grid.view().seq > after && dbc.grid.view().flip &&
	  document.getElementById("grid-info").textContent.startsWith("200 rows")`, before)
	eval(t, p, `() => { const g = document.getElementById("grid"); g.scrollLeft = 30 * dbc.grid.view().recW; }`)
	waitFor(t, p, "record 34 drawn", `() => !!document.querySelector('#grid .gh [data-r="33"]')`)
	rec0 = evalNum(t, p, `() => dbc.grid.view().recW`)
	edgeJS = `() => document.querySelector('#grid .gh [data-r="33"]').getBoundingClientRect().right`
	edge0 = evalNum(t, p, edgeJS)
	x, y = handleCenter(t, p, `#grid .gh [data-rzr="33"]`)
	drag(t, p, x, y, x+40)
	if got := evalNum(t, p, `() => dbc.grid.view().recW`); got != rec0+40 {
		t.Fatalf("scrolled: record width %v → %v, want +40", rec0, got)
	}
	if edge := evalNum(t, p, edgeJS); edge < edge0+40-2 || edge > edge0+40+2 {
		t.Fatalf("scrolled: record 34's border %v → %v, want it to follow the pointer's 40px", edge0, edge)
	}

	// and back upright, as the later steps expect
	p.MustElement("#grid").MustFocus()
	p.Keyboard.MustType(input.KeyT)
	waitFor(t, p, "the grid upright", `() => !dbc.grid.view().flip && !document.querySelector("#grid .gh .hc.rec")`)
}

// handleCenter is the screen center of the element sel matches.
func handleCenter(t *testing.T, p *rod.Page, sel string) (float64, float64) {
	t.Helper()
	box := eval(t, p, `(s) => { const r = document.querySelector(s).getBoundingClientRect(); return [r.x + r.width / 2, r.y + r.height / 2]; }`, sel).([]any)
	return box[0].(float64), box[1].(float64)
}

// drag presses at (x, y), moves to x2 in steps, as a hand would, and
// releases.
func drag(t *testing.T, p *rod.Page, x, y, x2 float64) {
	t.Helper()
	p.Mouse.MustMoveTo(x, y)
	p.Mouse.MustDown(proto.InputMouseButtonLeft)
	if err := p.Mouse.MoveLinear(proto.Point{X: x2, Y: y}, 8); err != nil {
		t.Fatal(err)
	}
	p.Mouse.MustUp(proto.InputMouseButtonLeft)
	time.Sleep(450 * time.Millisecond) // past the double-click window, so the next press is a new one
}

func evalNum(t *testing.T, p *rod.Page, js string, args ...any) float64 {
	t.Helper()
	n, _ := eval(t, p, js, args...).(float64)
	return n
}

// switchConns: a click on another connection moves the tab there and
// redraws the Tables list with its tables; and back again.
func switchConns(t *testing.T, _ *env, p *rod.Page) {
	clickAt(t, p, `#conns .conn-item[data-conn="lite2"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite2")
	waitFor(t, p, "lite2's tables", `() => [...document.querySelectorAll("#tables li[data-name]")].map((l) => l.dataset.name).join() === "dogs"`)

	clickAt(t, p, `#conns .conn-item[data-conn="lite"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
	waitFor(t, p, "lite's tables", `() => !!document.querySelector('#tables li[data-name="cats"]')`)
}

// resultTabsPerConn: each connection keeps its own result tabs, grid view
// and log. A result sorted on lite comes back sorted after a visit to
// lite2, which shows none of lite's results or log lines; a pinned tab
// makes the next run open a second tab; clicking the first shows it again
// with its view; the log's header names the connection, Copy copies it and
// Clear empties it; a tab closes from its menu.
func resultTabsPerConn(t *testing.T, _ *env, p *rod.Page) {
	eval(t, p, `() => { if (dbc.grid.view().flip) dbc.grid.transpose(); }`)
	before := gridSeq(t, p)
	eval(t, p, `() => dbc.editor.setText("SELECT name, age FROM cats ORDER BY id")`)
	p.MustElement("#run").MustClick()
	waitResult(t, p, before, "name", "age")
	waitFor(t, p, "one result tab", `() => document.querySelectorAll("#rstrip .rt").length === 1 &&
	  !!document.querySelector("#rstrip .rt.on") && !document.getElementById("rstrip").hidden`)
	waitFor(t, p, "the log names lite", `() => document.getElementById("log-conn").textContent === "· lite" &&
	  document.getElementById("log").textContent.includes("completed on lite")`)
	clickAt(t, p, `#grid .gh .hc[data-c="1"]`, proto.InputMouseButtonLeft) // sort by age
	waitFor(t, p, "sorted by age", `() => dbc.grid.view().sort === 1`)
	liteSeq := gridSeq(t, p)

	clickAt(t, p, `#conns .conn-item[data-conn="lite2"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite2")
	waitFor(t, p, "lite2's empty results and its own log", `() => !dbc.grid.hasResult() &&
	  document.getElementById("rstrip").hidden &&
	  document.getElementById("log-conn").textContent === "· lite2" &&
	  !document.getElementById("log").textContent.includes("completed on lite ")`)
	before = gridSeq(t, p)
	eval(t, p, `() => dbc.editor.setText("SELECT name FROM dogs")`)
	p.MustElement("#run").MustClick()
	waitResult(t, p, before, "name")

	clickAt(t, p, `#conns .conn-item[data-conn="lite"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
	waitFor(t, p, "lite's result back, still sorted", `(seq) => dbc.grid.view().seq === seq &&
	  dbc.grid.view().sort === 1 && document.querySelectorAll("#rstrip .rt").length === 1 &&
	  document.getElementById("log-conn").textContent === "· lite" &&
	  document.getElementById("log").textContent.includes("completed on lite") &&
	  !document.getElementById("log").textContent.includes("on lite2")`, liteSeq)

	// P pins (dispatched on the focused grid: a shifted printable key)
	eval(t, p, `() => { dbc.grid.focus();
	  document.activeElement.dispatchEvent(new KeyboardEvent("keydown", { key: "P", bubbles: true, cancelable: true })); }`)
	waitFor(t, p, "the tab pinned", `() => !!document.querySelector("#rstrip .rt.on .pin")`)
	before = gridSeq(t, p)
	eval(t, p, `() => dbc.editor.setText("SELECT 1 AS one")`)
	p.MustElement("#run").MustClick()
	waitResult(t, p, before, "one")
	waitFor(t, p, "a second tab, on screen", `() => document.querySelectorAll("#rstrip .rt").length === 2 &&
	  document.querySelector("#rstrip .rt:nth-child(2)").classList.contains("on")`)

	clickAt(t, p, `#rstrip .rt:nth-child(1) .tt`, proto.InputMouseButtonLeft)
	waitFor(t, p, "the first tab back, with its sort", `(seq) => dbc.grid.view().seq === seq && dbc.grid.view().sort === 1 &&
	  document.querySelector("#rstrip .rt:nth-child(1)").classList.contains("on")`, liteSeq)

	setClipboard(t, p, "sentinel")
	p.MustElement("#log-copy").MustClick()
	waitFor(t, p, "the log copied", `() => [...document.querySelectorAll("#log > div")].some((d) => d.textContent.includes("copied the log"))`)
	if got := clipboard(t, p); !strings.Contains(got, "completed on lite") || strings.Contains(got, "on lite2") {
		t.Fatalf("the copied log = %q", got)
	}
	p.MustElement("#log-clear").MustClick()
	waitFor(t, p, "the log cleared", `() => document.querySelectorAll("#log > div").length === 0`)

	rightClick(t, p, `#rstrip .rt:nth-child(1)`)
	// lite has no ai_rows: sharing is offered, off, with the reason (the
	// share itself, which needs an ai_rows connection, is web/resulttabs_test.go's)
	if got := evalStr(t, p, `() => { const b = [...document.querySelectorAll(".menu .mitem")]
	  .find((m) => m.textContent.includes("Share with the assistant"));
	  return b ? b.className + "|" + b.title : ""; }`); !strings.Contains(got, "off|") || !strings.Contains(got, "ai_rows = true on lite") {
		t.Fatalf("the share row = %q", got)
	}
	menuPick(t, p, "Close this result tab")
	waitFor(t, p, "one tab left, on screen, its result in the grid", `() => document.querySelectorAll("#rstrip .rt").length === 1 &&
	  !!document.querySelector("#rstrip .rt.on") && !document.querySelector("#rstrip .pin") &&
	  [...document.querySelectorAll("#grid .gh .hc")].some((h) => h.textContent.includes("one"))`)
}

// historyScope: the history opens on the tab's database when it has
// statements there (N-101), and the scope button shows every database's.
// lite has the earlier steps' queries; one run on lite2 makes it a
// database with history of its own.
func historyScope(t *testing.T, _ *env, p *rod.Page) {
	clickAt(t, p, `#conns .conn-item[data-conn="lite2"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite2")
	before := gridSeq(t, p)
	eval(t, p, `() => dbc.editor.setText("SELECT 'only on lite2' AS here")`)
	p.MustElement("#run").MustClick()
	waitResult(t, p, before, "here")

	p.MustElement("#history-btn").MustClick()
	rows := `() => [...document.querySelectorAll(".history li")].map((l) => l.textContent).join("\n")`
	waitFor(t, p, "lite2's statement alone", `() => {
	  const r = (`+rows+`)();
	  return r.includes("only on lite2") && !r.includes("FROM cats");
	}`)
	p.MustElement(".mfoot button.linkish").MustClick()
	waitFor(t, p, "every database's statements", `() => {
	  const r = (`+rows+`)();
	  return r.includes("only on lite2") && r.includes("FROM cats");
	}`)
	eval(t, p, `() => dbc.modal.close()`)

	clickAt(t, p, `#conns .conn-item[data-conn="lite"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
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

	clickAt(t, p, `#conns .conn-item[data-conn="lite"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
}

// refreshConn: the connection menu's Refresh re-reads the tab's
// connection in place — a table another client created (dbc headless, a
// separate process) is listed, and one it dropped goes — without leaving
// it. On a row the tab is not on, Refresh is shown but off, and says why.
func refreshConn(t *testing.T, e *env, p *rod.Page) {
	listed := `() => !!document.querySelector('#tables li[data-name="e2e_refreshed"]')`
	refreshed := func(what string, want bool) {
		t.Helper()
		waitFor(t, p, what, `(want) => {
		  const a = document.querySelector('#conns .conn-item.active[data-conn="lite"]');
		  return !!a && !document.querySelector("#conns .conn-item.connecting") &&
		    document.getElementById("status").textContent.includes("refreshed") &&
		    !!document.querySelector('#tables li[data-name="e2e_refreshed"]') === want;
		}`, want)
	}

	e.dbc(t, "lite", "CREATE TABLE e2e_refreshed (id INTEGER)")
	if eval(t, p, listed) != false {
		t.Fatal("the new table was listed before any refresh — the step proves nothing")
	}
	rightClick(t, p, `#conns .conn-item[data-conn="lite2"]`)
	waitFor(t, p, "Refresh shown off on lite2", `() => [...document.querySelectorAll(".menu .mitem")]
	  .some((b) => b.textContent === "Refresh" && b.classList.contains("off"))`)
	p.Keyboard.MustType(input.Escape)
	waitFor(t, p, "the menu closed", `() => !document.querySelector(".menu")`)

	rightClick(t, p, `#conns .conn-item[data-conn="lite"]`)
	menuPick(t, p, "Refresh")
	refreshed("the new table listed", true)

	e.dbc(t, "lite", "DROP TABLE e2e_refreshed")
	// the status still reads "refreshed" from the last one: clear it so the
	// wait below sees this refresh land, not that one
	eval(t, p, `() => { document.getElementById("status").textContent = ""; }`)
	rightClick(t, p, `#conns .conn-item[data-conn="lite"]`)
	menuPick(t, p, "Refresh")
	refreshed("the dropped table gone", false)
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

	clickAt(t, p, `#conns .conn-item[data-conn="lite"]`, proto.InputMouseButtonLeft)
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
	clickAt(t, p, `#conns .conn-item[data-conn="pg"]`, proto.InputMouseButtonLeft)
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

	// Tab completes to the one match left, as Enter would, and moves on
	// to the table box — rather than leaving the box back on the schema
	// picked before (e2e_b). The keyboard gets there before e2e_a's
	// tables do; they land with the first selected, for Enter to preview
	box.MustClick()
	waitFor(t, p, "the schema list again", `() => !document.getElementById("schema-list").hidden`)
	box.MustInput("e2e_a")
	waitFor(t, p, "the list narrowed to e2e_a", `() =>
	  [...document.querySelectorAll("#schema-list li[data-i]")].map((li) => li.textContent).join().startsWith("e2e_a")`)
	p.Keyboard.MustType(input.Tab)
	waitFor(t, p, "e2e_a's tables after Tab, the keyboard in the table box", `() =>
	  document.activeElement === document.getElementById("table-find") &&
	  document.getElementById("table-schema").value === "e2e_a" &&
	  [...document.querySelectorAll("#tables li[data-name]")].map((l) => l.dataset.name).join() === "e2e_a.alpha" &&
	  !!document.querySelector('#tables li.sel[data-name="e2e_a.alpha"]')`)
	p.Keyboard.MustType(input.KeyA, input.KeyL)
	waitFor(t, p, "alpha found by al", `() =>
	  document.getElementById("table-find").value === "al" &&
	  !!document.querySelector('#tables li.sel[data-name="e2e_a.alpha"]')`)
	before = gridSeq(t, p)
	p.Keyboard.MustType(input.Enter)
	if info := waitResult(t, p, before, "id"); info != "0 rows" {
		t.Fatalf("grid info = %q, want 0 rows (the preview of e2e_a.alpha)", info)
	}

	// Tab takes a schema the arrows moved to, too, with nothing typed;
	// before, it moved focus on and the box went back to the pick before
	box.MustClick()
	waitFor(t, p, "the schema list on e2e_a", `() => {
	  const hi = document.querySelector("#schema-list li.hi");
	  return !document.getElementById("schema-list").hidden && !!hi && hi.textContent.startsWith("e2e_a");
	}`)
	p.Keyboard.MustType(input.ArrowDown)
	waitFor(t, p, "e2e_b highlighted", `() => {
	  const hi = document.querySelector("#schema-list li.hi");
	  return !!hi && hi.textContent.startsWith("e2e_b");
	}`)
	p.Keyboard.MustType(input.Tab)
	waitFor(t, p, "e2e_b's tables after an arrow and Tab", `() =>
	  document.activeElement === document.getElementById("table-find") &&
	  document.getElementById("table-find").value === "" &&
	  document.getElementById("table-schema").value === "e2e_b" &&
	  !!document.querySelector('#tables li.sel[data-name="e2e_b.beta"]')`)

	// A database picked with Tab moves on too, once its connect lands
	// (until then the boxes below are the old database's): to the table
	// box on the postgres database, whose one schema means no schema box,
	// and to the schema box back on dbc
	dbBox := p.MustElement("#table-db")
	tabToDatabase := func(name string) {
		t.Helper()
		dbBox.MustClick()
		waitFor(t, p, "the database list", `() => !document.getElementById("db-list").hidden`)
		dbBox.MustInput(name)
		waitFor(t, p, "the list narrowed to "+name, `(n) => {
		  const first = document.querySelector("#db-list li[data-i]");
		  return !!first && first.textContent.startsWith(n);
		}`, name)
		p.Keyboard.MustType(input.Tab)
	}
	tabToDatabase("postgres")
	waitFor(t, p, "on pg/postgres, the keyboard in the table box", `() =>
	  dbc.state.active === "pg/postgres" && !document.querySelector("#conns .conn-item.connecting") &&
	  document.getElementById("table-filter").hidden &&
	  document.activeElement === document.getElementById("table-find")`)
	tabToDatabase("dbc")
	waitFor(t, p, "back on pg, the keyboard in the schema box", `() =>
	  dbc.state.active === "pg" && !document.querySelector("#conns .conn-item.connecting") &&
	  document.activeElement === document.getElementById("table-schema") &&
	  !document.getElementById("schema-list").hidden`)

	clickAt(t, p, `#conns .conn-item[data-conn="lite"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
}

// tabsSurviveReload: a new tab, its text and a rename are saved on the
// server, so a reload of the window brings all three back, on that tab.
func tabsSurviveReload(t *testing.T, _ *env, p *rod.Page) {
	clickAt(t, p, "#qnew", proto.InputMouseButtonLeft)
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
	// and its console file, which is what a reload reads the text from.
	// Checked before the reload on purpose: the page's goodbye also saves
	// the shown console, which hid N-110 — the rename's tab save dropped
	// the pending console save, so the file stayed empty until then
	waitFor(t, p, "the console file saved", `async () => {
	  const c = dbc.state.tab.cdb, name = dbc.state.tab.console;
	  if (!c || !name) return false;
	  const r = await (await fetch("/api/v1/consoles/" + encodeURIComponent(c.host) + "/" +
	    encodeURIComponent(c.database) + "/" + encodeURIComponent(name))).json();
	  return (r.data || {}).text === "SELECT 42 AS answer";
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

	// The saved fold ends as the sidebar does: open. Each toggle writes
	// the layout, and two writes sent back to back used to race to the
	// server, so the "1" of a fold could land after the "" of the reveal —
	// and the next load (tabGroups reloads) drew the sidebar folded, its
	// connection rows zero-sized and unclickable (N-120). Here the fold's
	// write is held back 300 ms, so without core.js's putLayout queue the
	// reveal's write would land first every time.
	eval(t, p, `() => {
	  const f = window.fetch;
	  let held = false;
	  window.fetch = async (u, o) => {
	    if (!held && o && o.method === "PUT" && String(u).includes("/api/v1/layout")) {
	      held = true;
	      await new Promise((r) => setTimeout(r, 300));
	      window.fetch = f; // only the one write is held
	    }
	    return f(u, o);
	  };
	  dbc.cmd.toggleSidebar();
	  dbc.cmd.toggleSidebar();
	}`)
	if folded() {
		t.Fatal("a fold and a reveal left the sidebar folded")
	}
	time.Sleep(500 * time.Millisecond) // past the held write
	waitFor(t, p, "the layout saved open", `async () =>
	  ((await (await fetch("/api/v1/layout")).json()).data || {}).sideHidden === ""`)
}

// otherTabsConns: a connection another query tab is on — in this window or
// another browser window — has the dimmed bar (.inuse) and a tooltip naming
// that tab (web/inuse.go, app.js markInUse); the tab on screen marks its own
// with the bright bar alone, and a tab's Disconnect takes its mark away.
//
// It starts where tabsSurviveReload left the window: two query tabs, both
// on lite, "Renamed tab" on screen and Query 1 reattached in the background.
func otherTabsConns(t *testing.T, e *env, p *rod.Page) {
	// marked waits until exactly the rows conns (in list order) are .inuse
	marked := func(p *rod.Page, conns string) {
		t.Helper()
		waitFor(t, p, "in-use rows "+conns, `(w) =>
		  [...document.querySelectorAll("#conns .conn-item.inuse")].map((b) => b.dataset.conn).join() === w`, conns)
	}
	tooltip := func(p *rod.Page, conn string) string {
		t.Helper()
		return evalStr(t, p, `(c) => document.querySelector('#conns .conn-item[data-conn="' + c + '"]').title`, conn)
	}

	// Renamed tab moves to lite2: lite is now Query 1's alone
	clickAt(t, p, `#conns .conn-item[data-conn="lite2"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite2")
	marked(p, "lite")
	if got := tooltip(p, "lite"); !strings.HasPrefix(got, "also open in Query 1 — ") {
		t.Fatalf("lite's tooltip = %q, want it to name Query 1", got)
	}

	// on Query 1 the marks swap: its own bar on lite, Renamed tab's on lite2
	clickAt(t, p, tabSelector(t, p, "Query 1"), proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
	marked(p, "lite2")
	if got := tooltip(p, "lite2"); !strings.HasPrefix(got, "also open in Renamed tab — ") {
		t.Fatalf("lite2's tooltip = %q, want it to name Renamed tab", got)
	}
	if got := tooltip(p, "lite"); strings.Contains(got, "also open") {
		t.Fatalf("lite's tooltip = %q: the tab on screen named itself", got)
	}

	// Query 1 disconnects: lite has no bar of either kind
	rightClick(t, p, `#conns .conn-item[data-conn="lite"]`)
	menuPick(t, p, "Disconnect")
	waitFor(t, p, "not connected", `() => !document.querySelector("#conns .conn-item.active")`)
	marked(p, "lite2")

	// another browser window boots on lite (its saved tabs are this
	// window's, so it gets a fresh one): this window marks lite for it,
	// without a title — that tab is the other window's to name
	q := e.page(t, "lite")
	defer q.MustClose()
	marked(p, "lite,lite2")
	if got := tooltip(p, "lite"); !strings.HasPrefix(got, "also open in a tab in another browser window — ") {
		t.Fatalf("lite's tooltip = %q, want another browser window", got)
	}
	// and that window sees both of this one's tabs' connections — Query 1
	// is off lite, Renamed tab is on lite2
	marked(q, "lite2")

	// back on lite, as the step found it
	p.MustActivate()
	clickAt(t, p, `#conns .conn-item[data-conn="lite"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
}

// consolesPerDatabase: a tab's text is a console of the database it is on —
// a file under consoles/<host>/<database>/ — and follows the tab's
// connection there and back; Alt+N adds a console of the database, Alt+C
// cycles through them, and a console changed on disk (the TUI saving it)
// is loaded rather than written over.
func consolesPerDatabase(t *testing.T, e *env, p *rod.Page) {
	setText := func(s string) {
		t.Helper()
		eval(t, p, `() => { dbc.editor.setText(""); dbc.editor.focus(); }`)
		p.MustInsertText(s)
	}
	// onDisk waits for a console of an embedded database to hold text, and
	// returns its file
	onDisk := func(text string) string {
		t.Helper()
		deadline := time.Now().Add(waitLimit)
		for {
			files, _ := filepath.Glob(filepath.Join(e.home, ".config", "dbc", "consoles", "local", "*", "*.sql"))
			for _, f := range files {
				if b, err := os.ReadFile(f); err == nil && string(b) == text {
					return f
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("no console file holds %q (have %v)\n%s", text, files, pageState(p))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	editorIs := func(what, text string) {
		t.Helper()
		waitFor(t, p, what, `(s) => dbc.editor.text() === s`, text)
	}

	waitConnected(t, p, "lite")
	setText("SELECT 'on lite'")
	liteFile := onDisk("SELECT 'on lite'")
	if db := filepath.Base(filepath.Dir(liteFile)); !strings.HasPrefix(db, "lite.db-") {
		t.Fatalf("lite's console is in %s, want local/lite.db-<hash>/", liteFile)
	}

	// another database: another console; back again: this one's text
	clickAt(t, p, `#conns .conn-item[data-conn="lite2"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite2")
	waitFor(t, p, "lite2's console", `() => dbc.editor.text() !== "SELECT 'on lite'"`)
	setText("SELECT 'on lite2'")
	if f := onDisk("SELECT 'on lite2'"); filepath.Dir(f) == filepath.Dir(liteFile) {
		t.Fatalf("lite2's console %s is in lite's directory", f)
	}
	clickAt(t, p, `#conns .conn-item[data-conn="lite"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
	editorIs("lite's console back", "SELECT 'on lite'")
	// and as it was left: the caret after what was typed, the typing still
	// undoable — its document was kept, not reopened at the top afresh
	waitFor(t, p, "lite's caret and undo kept", `(n) => dbc.editor.caret() === n &&
	  monaco.editor.getEditors()[0].getModel().canUndo()`, len("SELECT 'on lite'"))

	// Alt+N: a fresh console of lite, named on the tab
	before := evalStr(t, p, `() => document.querySelector("#qtabs .qtab.on .qcon").textContent`)
	chord(t, p, modAlt, "n", "KeyN", 78)
	waitFor(t, p, "a new console", `(b) => dbc.editor.text() === "" &&
	  document.querySelector("#qtabs .qtab.on .qcon").textContent !== b`, before)
	setText("SELECT 'another'")
	if f := onDisk("SELECT 'another'"); filepath.Dir(f) != filepath.Dir(liteFile) {
		t.Fatalf("the new console %s is not lite's", f)
	}
	// Alt+C from the newest wraps round to the first, lite's "console"
	chord(t, p, modAlt, "c", "KeyC", 67)
	editorIs("Alt+C back to the first console", "SELECT 'on lite'")

	// the TUI saves the console meanwhile: the next save here meets the
	// newer file, which the editor then shows (Ctrl+Z would bring this
	// tab's text back) — the file is not overwritten
	if err := os.WriteFile(liteFile, []byte("SELECT 'from the tui'"), 0o600); err != nil {
		t.Fatal(err)
	}
	eval(t, p, `() => dbc.editor.focus()`)
	p.MustInsertText(" -- edited here")
	editorIs("the file's text loaded", "SELECT 'from the tui'")
	waitFor(t, p, "the conflict logged", `() => document.getElementById("log").textContent.includes("was changed elsewhere")`)
	if b, _ := os.ReadFile(liteFile); string(b) != "SELECT 'from the tui'" {
		t.Fatalf("the page overwrote the TUI's save: %q", b)
	}
}

// codeHighlight: an answer's code block is drawn by hl.js in the editor's
// colors. The block is built with chat.js's own path (dbc.hl inside a
// .cblock) in the live page, so the check covers the script being loaded
// and the stylesheet's rules, not just the lexer.
func codeHighlight(t *testing.T, _ *env, p *rod.Page) {
	got := evalStr(t, p, `() => {
	  const pre = dbc.el("pre", null, dbc.hl("go", "func f() { return \"s\" } // c"));
	  const box = dbc.el("div", "cblock", pre);
	  document.body.append(box);
	  const css = (sel) => getComputedStyle(box.querySelector(sel));
	  const v = (n) => getComputedStyle(document.documentElement).getPropertyValue("--" + n).trim();
	  const rgb = (hex) => { const d = document.createElement("i"); d.style.color = hex; document.body.append(d);
	    const c = getComputedStyle(d).color; d.remove(); return c; };
	  const out = [
	    [...box.querySelectorAll(".hl-k")].map((x) => x.textContent).join(","),
	    css(".hl-k").color === rgb(v("accent")),
	    css(".hl-s").color === rgb(v("warn")),
	    css(".hl-c").fontStyle,
	    pre.textContent,
	  ];
	  // a shell $variable is the accent (hl-v), not the error color bind
	  // parameters (hl-p) get
	  pre.replaceChildren(dbc.hl("sh", "echo $HOME"));
	  out.push(box.querySelector(".hl-v").textContent, css(".hl-v").color === rgb(v("accent")), !box.querySelector(".hl-p"));
	  box.remove();
	  return out.join("|");
	}`)
	if want := `func,return|true|true|italic|func f() { return "s" } // c|$HOME|true|true`; got != want {
		t.Fatalf("highlighted block = %q, want %q", got, want)
	}
}

// tabGroups: tabs grouped by hand (ad-hoc) and by connection, gathered
// behind their chip, folded by a click on it, and kept across a reload
// (tabgroups.js, web/groups.go). It starts where consolesPerDatabase left
// the window — Query 1 on lite, on screen; Renamed tab on lite2 — and
// leaves no group behind.
func tabGroups(t *testing.T, _ *env, p *rod.Page) {
	// strip is the strip as drawn: a chip as [text], a tab as its title
	strip := func(want string) {
		t.Helper()
		got := ""
		for deadline := time.Now().Add(waitLimit); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			got = evalStr(t, p, `() => [...document.querySelectorAll("#qtabs > *")]
			  .map((x) => x.classList.contains("qchip") ? "[" + x.textContent + "]" : x.querySelector(".qt") ? x.querySelector(".qt").textContent : "")
			  .filter(Boolean).join(",")`)
			if got == want {
				return
			}
		}
		dump := evalStr(t, p, `async () => [...document.querySelectorAll("#qtabs .qtab")].map((b) => b.dataset.key + ":" +
		  b.querySelector(".qt").textContent + ":" + b.className).join(" | ") + "\n" +
		  JSON.stringify((await (await fetch("/api/v1/layout")).json()).data) + "\n" +
		  JSON.stringify((await (await fetch("/api/v1/tabs")).json()).data.map((t) => t.id + ":" + t.title + ":" + t.conn))`)
		t.Fatalf("the strip is %s, want %s\n%s\n%s", got, want, dump, pageState(p))
	}
	// name answers the group-name dialog: the field's seed is selected, so
	// typing replaces it
	name := func(n string) {
		t.Helper()
		waitFor(t, p, "the name dialog", `() => document.activeElement && document.activeElement.matches("input.gname")`)
		eval(t, p, `() => document.activeElement.select()`)
		p.MustInsertText(n)
		p.Keyboard.MustType(input.Enter)
	}
	// at and tabSel are clickAt and tabSelector (harness_test.go): renderTabs
	// rebuilds the strip on many events, so every click here goes by
	// coordinates looked up at the moment of the click
	at := func(sel string, button proto.InputMouseButton) {
		t.Helper()
		clickAt(t, p, sel, button)
	}
	tabSel := func(title string) string {
		t.Helper()
		return tabSelector(t, p, title)
	}

	strip("Query 1,Renamed tab")

	// A tab brought on screen and left straight away keeps its own
	// connection (N-120). activate shows a tab before its workspace
	// answers, and until then the page's active connection is still the
	// previous tab's; leaving saves the tab, and that save used to write
	// the previous tab's connection into it — Renamed tab came back on
	// lite after a reload. In the full suite the gap was wide enough only
	// on a loaded run, so the workspace read is held back here to open it
	// every time, and the saves the page sends are checked directly.
	eval(t, p, `() => {
	  window.__tabPuts = [];
	  window.__realFetch = window.fetch;
	  window.fetch = async (u, o) => {
	    const url = String(u).split("?")[0], m = (o && o.method) || "GET";
	    if (m === "GET" && /\/api\/v1\/ws\/[^/]+$/.test(url)) await new Promise((r) => setTimeout(r, 700));
	    if (m === "PUT" && url.includes("/api/v1/tabs/")) window.__tabPuts.push(JSON.parse(o.body));
	    return window.__realFetch(u, o);
	  };
	}`)
	at(tabSel("Renamed tab"), proto.InputMouseButtonLeft)
	waitFor(t, p, "Renamed tab on screen", `() => dbc.state.tab && dbc.state.tab.title === "Renamed tab"`)
	at(tabSel("Query 1"), proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
	eval(t, p, `() => { window.fetch = window.__realFetch; }`)
	if got := evalStr(t, p, `() => window.__tabPuts.filter((b) => b.title === "Renamed tab").map((b) => b.conn).join()`); got == "" || strings.Trim(strings.ReplaceAll(got, "lite2", ""), ",") != "" {
		t.Fatalf("Renamed tab was saved on %q, want lite2 (its own connection) every time", got)
	}

	// a hand-made group; a name the dialog refuses says why and stays open
	at(tabSel("Query 1"), proto.InputMouseButtonRight)
	menuPick(t, p, "Add to group…")
	menuPick(t, p, "New ad-hoc group…")
	name("toolong123")
	waitFor(t, p, "the refusal", `() => document.querySelector(".gprompt .gwhy").textContent.includes("at most 8")`)
	name("wip")
	strip("[wip],Query 1,Renamed tab")

	// a tab opened from a grouped tab joins its group
	at("#qnew", proto.InputMouseButtonLeft)
	strip("[wip],Query 1,Query 2,Renamed tab")

	// a connection group takes every tab on its connection — including one
	// that switches to it later, which moves in beside it
	at(tabSel("Renamed tab"), proto.InputMouseButtonRight)
	menuPick(t, p, "Add to group…")
	menuPick(t, p, "New connection group: lite2")
	name("lite2")
	strip("[wip],Query 1,Query 2,[lite2],Renamed tab")

	// the + says where a new tab lands: in the colour and name of the
	// group Alt+T would put it in — wip, the tab on screen's
	plusIn := func(group string) {
		t.Helper()
		waitFor(t, p, "+ marked for "+group, `(g) => { const b = document.querySelector("#qnew");
		  return b.classList.contains("grp") && b.title.includes("in group " + g); }`, group)
	}
	plusIn("wip")
	// a right-click on + opens the tab in another group, on its connection
	at("#qnew", proto.InputMouseButtonRight)
	p.MustElementR(".menu .mitem .ml", `^lite2 `).MustClick()
	strip("[wip],Query 1,Query 2,[lite2],Renamed tab,Query 3")
	waitConnected(t, p, "lite2")
	plusIn("lite2")
	// and a group's own menu does the same — into the leftmost group, from
	// a tab in the other one
	at(`#qtabs .qchip[data-group="wip"]`, proto.InputMouseButtonRight)
	menuPick(t, p, "New tab in wip")
	strip("[wip],Query 1,Query 2,Query 4,[lite2],Renamed tab,Query 3")
	waitConnected(t, p, "lite")
	plusIn("wip")
	// back to where the rest starts: the two gone, Query 2 on screen
	at(tabSel("Query 3")+" [data-close]", proto.InputMouseButtonLeft)
	at(tabSel("Query 4")+" [data-close]", proto.InputMouseButtonLeft)
	strip("[wip],Query 1,Query 2,[lite2],Renamed tab")
	at(tabSel("Query 2"), proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
	at(tabSel("Query 2"), proto.InputMouseButtonRight)
	menuPick(t, p, "Remove from group wip")
	waitConnected(t, p, "lite")
	clickAt(t, p, `#conns .conn-item[data-conn="lite2"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite2")
	strip("[wip],Query 1,[lite2],Query 2,Renamed tab")

	// a click on the chip folds its group to the chip and a count
	at(`#qtabs .qchip[data-group="wip"]`, proto.InputMouseButtonLeft)
	strip("[wip +1],[lite2],Query 2,Renamed tab")

	// all of it survives a reload once the server holds it
	waitFor(t, p, "the groups saved", `async () => {
	  const l = (await (await fetch("/api/v1/layout")).json()).data || {};
	  const gs = JSON.parse(l.groups || "[]");
	  return gs.length === 2 && gs[0].name === "wip" && gs[0].collapsed && gs[1].conn === "lite2";
	}`)
	// The reload also claims the fresh "Query 1" the other browser window of
	// otherTabsConns saved before it closed: it is free now, and lands last
	// (the order key never named it) — ungrouped, on lite.
	p.MustReload()
	p.MustWaitLoad()
	waitConnected(t, p, "lite2")
	strip("[wip +1],[lite2],Query 2,Renamed tab,Query 1")

	// a connection group whose last tab connects elsewhere stays on the
	// strip as an idle chip at its end — it used to vanish while still
	// stored — and a click on it opens a tab on its connection. (wip's
	// folded Query 1 is not drawn, so tabSel finds the last one.)
	at(tabSel("Query 1"), proto.InputMouseButtonRight)
	menuPick(t, p, "Add to group…")
	menuPick(t, p, "New connection group: lite")
	name("lite")
	strip("[wip +1],[lite2],Query 2,Renamed tab,[lite],Query 1")
	at(tabSel("Query 1"), proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
	at(`#conns .conn-item[data-conn="lite2"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite2")
	strip("[wip +1],[lite2],Query 2,Renamed tab,Query 1,[lite]")
	at(`#qtabs .qchip.idle[data-group="lite"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
	strip("[wip +1],[lite2],Query 2,Renamed tab,Query 1,[lite],Query 3")
	// closing Query 3 idles lite again (Query 1 comes on screen); its
	// chip's menu ungroups it, and Query 1 goes back to lite
	at(tabSel("Query 3")+" [data-close]", proto.InputMouseButtonLeft)
	strip("[wip +1],[lite2],Query 2,Renamed tab,Query 1,[lite]")
	at(`#qtabs .qchip.idle[data-group="lite"]`, proto.InputMouseButtonRight)
	menuPick(t, p, "Ungroup")
	strip("[wip +1],[lite2],Query 2,Renamed tab,Query 1")
	waitConnected(t, p, "lite2")
	at(`#conns .conn-item[data-conn="lite"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
	strip("[wip +1],[lite2],Query 2,Renamed tab,Query 1")

	// the chip's menu: ungroup lite2; a second click unfolds wip; its
	// menu ungroups it too
	at(`#qtabs .qchip[data-group="lite2"]`, proto.InputMouseButtonRight)
	menuPick(t, p, "Ungroup")
	strip("[wip +1],Query 2,Renamed tab,Query 1")
	at(`#qtabs .qchip[data-group="wip"]`, proto.InputMouseButtonLeft)
	strip("[wip],Query 1,Query 2,Renamed tab,Query 1")
	at(`#qtabs .qchip[data-group="wip"]`, proto.InputMouseButtonRight)
	menuPick(t, p, "Ungroup")
	strip("Query 1,Query 2,Renamed tab,Query 1")
	waitFor(t, p, "no groups saved", `async () =>
	  ((await (await fetch("/api/v1/layout")).json()).data || {}).groups === "[]"`)
}

// foldRecentChats: the header of the assistant's Recent conversations list
// folds it to the header (with the count) and back, and the fold is a
// layout value — a reload draws the list folded.
//
// The pane is unhidden by hand rather than opened with Ctrl+I: opening
// starts the configured agent, and this run has none (nor should it reach
// a real one on the PATH). The empty pane is drawn at boot regardless —
// fetchAll lists the saved conversations seedChats wrote — so unhiding it
// shows exactly what an opened pane would.
func foldRecentChats(t *testing.T, _ *env, p *rod.Page) {
	unhide := `() => { document.getElementById("chat").hidden = false; }`
	rows := func() float64 { return evalNum(t, p, `() => document.querySelectorAll("#chat-trans .crecent").length`) }
	saved := func(want string) {
		t.Helper()
		waitFor(t, p, fmt.Sprintf("chatRecentFolded saved as %q", want), `async (want) => {
		  const r = await (await fetch("/api/v1/layout")).json();
		  return ((r.data || {}).chatRecentFolded || "") === want;
		}`, want)
	}

	eval(t, p, unhide)
	waitFor(t, p, "the recent list", `() => document.querySelectorAll("#chat-trans .crecent").length === 5`)
	if got := evalStr(t, p, `() => document.querySelector("#chat-trans .cfold").textContent`); got != "▾ recent conversations" {
		t.Fatalf("header = %q, want the unfolded one", got)
	}

	p.MustElement("#chat-trans .cfold").MustClick()
	if n := rows(); n != 0 {
		t.Fatalf("%v rows left after folding", n)
	}
	got := evalStr(t, p, `() => { const h = document.querySelector("#chat-trans .cfold");
	  return [h.textContent, h.getAttribute("aria-expanded"), document.activeElement === h,
	    !!document.querySelector("#chat-trans > .cempty > .linkish")].join("|"); }`)
	if got != "▸ recent conversations · 7|false|true|false" {
		t.Fatalf("folded header (text|expanded|focused|all-link) = %q", got)
	}
	saved("1")

	p.MustReload()
	p.MustWaitLoad()
	eval(t, p, unhide)
	waitFor(t, p, "the folded header after a reload", `() => {
	  const h = document.querySelector("#chat-trans .cfold");
	  return !!h && h.getAttribute("aria-expanded") === "false";
	}`)
	if n := rows(); n != 0 {
		t.Fatalf("%v rows drawn after a reload, want the list still folded", n)
	}

	// back out, with the keyboard this time: the header is a button
	eval(t, p, `() => document.querySelector("#chat-trans .cfold").focus()`)
	p.Keyboard.MustType(input.Enter)
	waitFor(t, p, "the list unfolded", `() => document.querySelectorAll("#chat-trans .crecent").length === 5`)
	saved("")
	eval(t, p, `() => { document.getElementById("chat").hidden = true; }`)
}
