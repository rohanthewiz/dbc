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

	// leave the window as the next steps expect it: the design tab, and
	// the query tab opened for it, closed. The strip redraws once the
	// neighbour that comes on screen has loaded, so it is waited for.
	chord(t, p, modAlt, "w", "KeyW", 87)
	gone := func(what, key string) {
		t.Helper()
		waitFor(t, p, what+" off the strip", `(k) => !document.querySelector('#qtabs .qtab[data-key="' + k + '"]')`, key)
	}
	gone("the design tab", design)
	clickAt(t, p, `#qtabs .qtab[data-key="`+mine+`"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "our query tab on screen", `(k) => dbc.state.tab.key === k`, mine)
	chord(t, p, modAlt, "w", "KeyW", 87)
	gone("our query tab", mine)
	if strip := evalStr(t, p, `() => [...document.querySelectorAll("#qtabs .qtab .qt")].map((x) => x.textContent).join(",")`); strings.Contains(strip, "Design 1") {
		t.Fatalf("the design tab is still on the strip: %s", strip)
	}
}
