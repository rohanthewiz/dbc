package e2e

import (
	"strings"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// queryDesigner locks down a design tab (designer.js over web/design.go):
//
//  1. Alt+D opens one; a click on a palette table adds it, and the next is
//     joined by its foreign key — the edge says 1:N.
//  2. Ticking and the column menu build sum(o.total) beside sum(p.amount):
//     the guide warns that orders repeat once per payment, and its
//     "Summarize p" fix rewrites the join; the cards get the engine's plan.
//  3. Ctrl+Enter on the canvas runs the design's SQL: the totals are the
//     true ones (Ana's orders 120, not 50·2 + 70).
//  4. A column dragged onto another card's joins them by hand; the join's
//     label opens its inspector, a kind changes the SQL, Ctrl+Z undoes it.
//  5. The story tells the query; a reload brings the design back.
//  6. Closing a design kept only in its tab asks; ⤓ Save as… names it, a
//     named one closes without asking, and Alt+Shift+D reopens it.
//  7. A query tab's statement opens as a design (the editor menu's
//     "◇ Open as a design"), its LIKE told to ignore case; a CTE is
//     refused in the status bar.
func queryDesigner(t *testing.T, e *env, p *rod.Page) {
	e.dbc(t, "lite", `DROP TABLE IF EXISTS qd_notes; DROP TABLE IF EXISTS payments; DROP TABLE IF EXISTS orders; DROP TABLE IF EXISTS customers;
CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT NOT NULL, country TEXT);
CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INTEGER NOT NULL REFERENCES customers(id), total NUMERIC NOT NULL);
CREATE TABLE payments (id INTEGER PRIMARY KEY, order_id INTEGER NOT NULL REFERENCES orders(id), amount NUMERIC NOT NULL);
CREATE TABLE qd_notes (id INTEGER PRIMARY KEY, order_ref INTEGER, body TEXT);
INSERT INTO customers VALUES (1, 'Ana', 'NZ'), (2, 'Bo', 'AU');
INSERT INTO orders VALUES (10, 1, 50), (11, 1, 70), (12, 2, 20);
INSERT INTO payments VALUES (100, 10, 20), (101, 10, 30), (102, 11, 70)`)
	// the step before may leave a pipeline or job tab on screen: start from
	// a query tab of our own on lite (closed again at the end), so the
	// design tab opens on lite beside it
	chord(t, p, modAlt, "t", "KeyT", 84)
	waitFor(t, p, "a new query tab", `() => !!dbc.state.ws && !dbc.state.tab.pipeline && !dbc.state.tab.job && !dbc.state.tab.script`)
	clickAt(t, p, `#conns .conn-item[data-conn="lite"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
	mine := evalStr(t, p, `() => dbc.state.tab.key`)

	// 1. a design tab, two tables, a foreign-key join
	eval(t, p, `() => dbc.editor.focus()`)
	chord(t, p, modAlt, "d", "KeyD", 68)
	waitFor(t, p, "a design tab", `() => !!dbc.state.tab.design && !document.getElementById("qd").hidden &&
	  document.querySelector("#qtabs .qtab.on .qt").textContent === "Design 1"`)
	design := evalStr(t, p, `() => dbc.state.tab.key`)
	waitFor(t, p, "the palette", `() => !!document.querySelector('#qd .qditem[data-label="customers"]')`)
	clickAt(t, p, `#qd .qditem[data-label="customers"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "customers on the canvas", `() => !!document.querySelector('#qd .qdcard[data-alias="c"] .qdc[data-col="name"]')`)
	clickAt(t, p, `#qd .qditem[data-label="orders"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "orders joined, one to many", `() => [...document.querySelectorAll("#qd .qdjl")].some((l) => l.textContent === "⋈ 1:N")`)
	clickAt(t, p, `#qd .qditem[data-label="payments"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "payments joined", `() => document.querySelectorAll("#qd .qdcard").length === 3 && document.querySelectorAll("#qd .qdjl").length === 2`)

	// 2. the columns, the fan-out, the fix
	clickAt(t, p, `#qd .qdcard[data-alias="c"] .qdc[data-col="name"] input`, proto.InputMouseButtonLeft)
	waitFor(t, p, "c.name shown", `() => dbc.editor.text().startsWith("SELECT c.name\n")`)
	clickAt(t, p, `#qd .qdcard[data-alias="o"] .qdc[data-col="total"] .qdcn`, proto.InputMouseButtonLeft)
	waitFor(t, p, "the column menu", `() => !!document.querySelector(".menu")`)
	menuPick(t, p, "Show sum(total)")
	clickAt(t, p, `#qd .qdcard[data-alias="p"] .qdc[data-col="amount"] .qdcn`, proto.InputMouseButtonLeft)
	waitFor(t, p, "the column menu", `() => !!document.querySelector(".menu")`)
	menuPick(t, p, "Show sum(amount)")
	waitFor(t, p, "the fan-out warning", `() => !!document.querySelector("#qd .qdhealth .qds-warn")`)
	clickAt(t, p, "#qd .qdhealth", proto.InputMouseButtonLeft)
	waitFor(t, p, "the finding", `() => /sum\(o\.total\) counts each o row more than once/.test(
	  document.querySelector('#qd .qdl[data-rule="fanout-aggregate"]')?.textContent || "")`)
	shot(t, p, "designer-1-fanout")
	fix, err := p.ElementR(`#qd .qdl[data-rule="fanout-aggregate"] .qdsm`, "^Summarize p per o$")
	if err != nil {
		t.Fatalf("no Summarize fix: %v", err)
	}
	fix.MustClick()
	waitFor(t, p, "the summarized SQL", `() => dbc.editor.text().includes("LEFT JOIN (\n    SELECT p.order_id, sum(p.amount) AS sum_amount")`)
	waitFor(t, p, "no warning left", `() => !document.querySelector("#qd .qdhealth .qds-warn") && !document.querySelector("#qd .qdhealth .qds-error")`)
	waitFor(t, p, "the plan on the cards", `() => document.querySelectorAll("#qd .qdcard .qdpb").length >= 2`)
	shot(t, p, "designer-2-fixed")

	// 3. run it from the canvas
	before := gridSeq(t, p)
	eval(t, p, `() => document.querySelector("#qd .qdcanvas").focus()`)
	chord(t, p, modCtrl, "Enter", "Enter", 13)
	waitResult(t, p, before, "name", "sum_total", "sum_amount")
	rows := evalStr(t, p, `() => [...document.querySelectorAll("#grid .gb .gr")]
	  .sort((a, b) => parseFloat(a.style.top) - parseFloat(b.style.top))
	  .map((r) => [...r.querySelectorAll(".gc")].map((c) => c.textContent).join("|")).join(" ")`)
	if rows != "Ana|120|120 Bo|20|NULL" {
		t.Fatalf("the designed query returned %q", rows)
	}

	// 4. a join by hand: qd_notes.order_ref dragged onto orders.id
	clickAt(t, p, "#qd .qdtabs button", proto.InputMouseButtonLeft) // Query
	eval(t, p, `() => { const f = document.querySelector("#qd .pfilter"); f.value = "qd_"; f.dispatchEvent(new Event("input")); }`)
	clickAt(t, p, `#qd .qditem[data-label="qd_notes"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "qd_notes unjoined", `() => !!document.querySelector('#qd .qdcard[data-alias="qn"]') &&
	  document.querySelector('#qd .qdcard[data-alias="qn"] .qdrole')?.textContent.includes("not joined")`)
	dragTo(t, p, `#qd .qdcard[data-alias="qn"] .qdc[data-col="order_ref"] .qdcn`, `#qd .qdcard[data-alias="o"] .qdc[data-col="id"]`, 0, 0)
	waitFor(t, p, "joined by hand", `() => dbc.editor.text().includes("JOIN qd_notes qn ON qn.order_ref = o.id")`)
	clickAt(t, p, `#qd .qdjl:last-of-type`, proto.InputMouseButtonLeft)
	waitFor(t, p, "the join inspector", `() => !!document.querySelector("#qd .qdinsp .qdkinds")`)
	shot(t, p, "designer-3-join")
	left, err := p.ElementR("#qd .qdkind .qdkn", "^left join$")
	if err != nil {
		t.Fatal(err)
	}
	left.MustClick()
	waitFor(t, p, "a left join", `() => dbc.editor.text().includes("LEFT JOIN qd_notes qn ON qn.order_ref = o.id")`)
	eval(t, p, `() => document.querySelector("#qd .qdcanvas").focus()`)
	chord(t, p, modCtrl, "z", "KeyZ", 90)
	waitFor(t, p, "undone", `() => dbc.editor.text().includes("\nJOIN qd_notes qn ON qn.order_ref = o.id")`)

	// 5. the story, and the design surviving a reload
	story, err := p.ElementR("#qd .qdtabs button", "^Story$")
	if err != nil {
		t.Fatal(err)
	}
	story.MustClick()
	waitFor(t, p, "the story", `() => /Summarize payments \(p\) per o first/.test(document.querySelector("#qd .qdstory")?.textContent || "")`)
	shot(t, p, "designer-4-story")
	waitFor(t, p, "the design saved", `async () => {
	  const r = await (await fetch("/api/v1/tabs")).json();
	  return (r.data || []).some((t) => t.title === "Design 1" && (t.design || "").includes('"qd_notes"'));
	}`)
	p.MustReload()
	p.MustWaitLoad()
	waitConnected(t, p, "lite")
	waitFor(t, p, "the design back", `() => !!dbc.state.tab.design && document.querySelectorAll("#qd .qdcard").length === 4 &&
	  dbc.editor.text().includes("JOIN qd_notes qn")`)

	gone := func(what, key string) {
		t.Helper()
		waitFor(t, p, what+" off the strip", `(k) => !document.querySelector('#qtabs .qtab[data-key="' + k + '"]')`, key)
	}
	// button clicks the button labelled label by script, looked up at the
	// moment of the click: the designer's bar is redrawn on every
	// analysis, and rod's MustClick on a handle it detached waits forever
	button := func(sel, label string) {
		t.Helper()
		waitFor(t, p, "the button "+label, `(sel, label) => { const b = [...document.querySelectorAll(sel)].find((x) => x.textContent === label);
		  if (!b) return false; b.click(); return true; }`, sel, label)
	}

	// 6. kept only in its tab, closing it asks first; saved under a name
	// (⤓ Save as…) the tab takes the name, the design is listed, and the
	// tab closes without a word — then Alt+Shift+D brings it back
	chord(t, p, modAlt, "w", "KeyW", 87)
	waitFor(t, p, "the close question", `() => /kept only in this tab/.test(document.querySelector(".modal")?.textContent || "")`)
	button(".modal button", "Keep it open")
	waitFor(t, p, "the question gone, the tab kept", `(k) => !document.querySelector(".modal") && dbc.state.tab.key === k`, design)
	button("#qd .qdbar button", "⤓ Save as…")
	waitFor(t, p, "the name prompt", `() => document.querySelector(".modal input.gname")?.value === "Design 1"`)
	eval(t, p, `() => { const i = document.querySelector(".modal input.gname"); i.value = "Totals by customer"; i.dispatchEvent(new Event("input")); }`)
	button(".modal button", "Save")
	waitFor(t, p, "the tab named", `() => document.querySelector("#qtabs .qtab.on .qt").textContent === "Totals by customer" &&
	  !!document.querySelector("#qd .qdbar .qdsaved")`)
	waitFor(t, p, "the design listed", `async () => {
	  const r = await (await fetch("/api/v1/designs")).json();
	  return (r.data.designs || []).some((d) => d.name === "Totals by customer" && d.tables.includes("qd_notes"));
	}`)
	shot(t, p, "designer-5-saved")
	chord(t, p, modAlt, "w", "KeyW", 87)
	gone("the named design tab", design)
	if eval(t, p, `() => dbc.modal.isOpen()`).(bool) {
		t.Fatal("closing a saved design asked a question")
	}
	chord(t, p, modAlt|modShift, "D", "KeyD", 68)
	waitFor(t, p, "the saved designs", `() => [...document.querySelectorAll(".modal .hlist li .hconn")].some((x) => x.textContent === "Totals by customer")`)
	shot(t, p, "designer-6-open")
	chord(t, p, 0, "Enter", "Enter", 13)
	waitFor(t, p, "the design reopened", `() => !!dbc.state.tab.design && dbc.state.tab.designName === "Totals by customer" &&
	  document.querySelectorAll("#qd .qdcard").length === 4 && dbc.editor.text().includes("JOIN qd_notes qn")`)
	chord(t, p, modAlt, "w", "KeyW", 87)
	waitFor(t, p, "the reopened tab closed", `() => !dbc.state.tab.design && !dbc.modal.isOpen()`)

	// 7. a hand-written query opened as a design: the statement under the
	// caret, on the canvas, judged; "ignore case" on its LIKE; a statement
	// the designer cannot say is refused in words
	clickAt(t, p, `#qtabs .qtab[data-key="`+mine+`"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "our query tab on screen", `(k) => dbc.state.tab.key === k && !!dbc.state.ws`, mine)
	openAsDesign := func(sql string) {
		t.Helper()
		eval(t, p, `(sql) => { dbc.editor.setText(sql); const ed = monaco.editor.getEditors()[0];
		  ed.setPosition({ lineNumber: 1, column: 1 }); ed.focus(); }`, sql)
		pt, _ := eval(t, p, `() => { const ed = monaco.editor.getEditors()[0];
		  const v = ed.getScrolledVisiblePosition({ lineNumber: 1, column: 3 });
		  const r = ed.getDomNode().getBoundingClientRect();
		  return [r.left + v.left + 2, r.top + v.top + v.height / 2]; }`).([]any)
		p.Mouse.MustMoveTo(pt[0].(float64), pt[1].(float64))
		if err := p.Mouse.Click(proto.InputMouseButtonRight, 1); err != nil {
			t.Fatalf("right-click: %v", err)
		}
		waitFor(t, p, "the editor menu", `() => !!document.querySelector(".menu")`)
		menuPick(t, p, "◇ Open as a design")
	}
	openAsDesign("SELECT c.name, sum(o.total) AS spent\nFROM customers c JOIN orders o ON o.customer_id = c.id\n" +
		"JOIN payments p ON p.order_id = o.id\nWHERE c.name LIKE '%n%'\nGROUP BY c.name")
	waitFor(t, p, "the query on the canvas", `() => !!dbc.state.tab.design && document.querySelectorAll("#qd .qdcard").length === 3 &&
	  !!document.querySelector("#qd .qdhealth .qds-warn")`)
	imported := evalStr(t, p, `() => dbc.state.tab.key`)
	clickAt(t, p, "#qd .qdtabs button", proto.InputMouseButtonLeft) // Query
	clickAt(t, p, `#qd .qdfil input[data-k="f0icase"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "ignoring case", `() => dbc.editor.text().includes("lower(c.name) LIKE lower('%n%')")`)
	shot(t, p, "designer-7-imported")
	chord(t, p, modAlt, "w", "KeyW", 87)
	waitFor(t, p, "the close question", `() => /kept only in this tab/.test(document.querySelector(".modal")?.textContent || "")`)
	button(".modal button", "Close without saving")
	gone("the imported design tab", imported)
	clickAt(t, p, `#qtabs .qtab[data-key="`+mine+`"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "our query tab on screen", `(k) => dbc.state.tab.key === k`, mine)
	openAsDesign("WITH x AS (SELECT 1 AS n) SELECT n FROM x")
	waitFor(t, p, "the refusal", `() => /not opened as a design — a WITH clause/.test(document.getElementById("status").textContent) &&
	  !dbc.state.tab.design`)

	// leave the window as the next steps expect it: the query tab opened
	// for this step, closed. The strip redraws once the neighbour that
	// comes on screen has loaded, so it is waited for.
	chord(t, p, modAlt, "w", "KeyW", 87)
	gone("our query tab", mine)
	if strip := evalStr(t, p, `() => [...document.querySelectorAll("#qtabs .qtab .qt")].map((x) => x.textContent).join(",")`); strings.Contains(strip, "Design 1") || strings.Contains(strip, "Totals by customer") {
		t.Fatalf("a design tab is still on the strip: %s", strip)
	}
}
