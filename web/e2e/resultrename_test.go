package e2e

import (
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// renameResultTab locks down renaming a result tab (app.js
// renameResultTab, op rename → Workspace.RenameResultTab):
//
//  1. A double-click on the tab opens its name in place; Enter keeps it,
//     and the strip shows it from then on.
//  2. ↻ reruns the tab's query and keeps the name — the same question.
//  3. F2 in the results opens it again, and the edit survives the strip
//     being redrawn under it; Esc leaves the name as it was.
//  4. An emptied name gives the tab its query's title back.
//  5. Another query run into the tab clears a name: it named those rows.
func renameResultTab(t *testing.T, _ *env, p *rod.Page) {
	waitConnected(t, p, "lite")
	const q = "SELECT name FROM cats ORDER BY id"
	run := func(sql, col string) {
		t.Helper()
		eval(t, p, `(sql) => dbc.editor.setText(sql)`, sql)
		before := gridSeq(t, p)
		p.MustElement("#run").MustClick()
		waitResult(t, p, before, col)
	}
	// the tab on screen's title, once it reads want
	onScreen := func(what, want string) {
		t.Helper()
		waitFor(t, p, what+" ("+want+")", `(want) => {
		  const b = document.querySelector("#rstrip button.rt.on");
		  return !!b && b.querySelector(".tt").textContent === want; }`, want)
	}
	// type replaces the edit box's text: it opens with the name selected
	typeName := func(name string, end input.Key) {
		t.Helper()
		waitFor(t, p, "the name's edit box focused", `() => document.activeElement && document.activeElement.matches("#rstrip .rrename")`)
		if name != "" {
			p.MustInsertText(name)
		} else {
			p.Keyboard.MustType(input.Backspace)
		}
		p.Keyboard.MustType(end)
	}
	run(q, "name")
	onScreen("the query's tab", q)

	// 1. double-click, a name, Enter
	clickAt(t, p, "#rstrip button.rt.on .tt", proto.InputMouseButtonLeft)
	if err := p.Mouse.Click(proto.InputMouseButtonLeft, 2); err != nil { // a double-click is two presses
		t.Fatal(err)
	}
	typeName("cat names", input.Enter)
	onScreen("the tab renamed", "cat names")
	if ok := evalStr(t, p, `() => (document.querySelector("#rstrip button.rt.on").getAttribute("aria-description") || "").startsWith("SELECT name") ? "y" : ""`); ok != "y" {
		t.Error("a renamed tab's description should still show its query")
	}

	// 1b. its hover card (ui.js dbc.hovercard, app.js resultTabCard): the
	// name as the heading, the query highlighted under it. The pointer
	// leaves first: the double-click's presses muted the tab's card until
	// it does. A press puts the card away, and it stays away while the
	// pointer rests there.
	p.Mouse.MustMoveTo(0, 0)
	moveAt(t, p, "#rstrip button.rt.on .tt", "hover")
	waitFor(t, p, "the tab's hover card", `() => { const c = document.querySelector(".hcard");
	  return !!c && c.querySelector(".hc-title").textContent === "cat names" &&
	    c.querySelector(".hc-sql").textContent.startsWith("SELECT name") &&
	    !!c.querySelector(".hc-sql .hl-k"); }`)
	if err := p.Mouse.Click(proto.InputMouseButtonLeft, 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, p, "the card put away by the press", `() => !document.querySelector(".hcard")`)
	time.Sleep(700 * time.Millisecond) // past SHOW_MS: a card coming back would be up by now
	if evalStr(t, p, `() => document.querySelector(".hcard") ? "up" : ""`) != "" {
		t.Error("a press should keep the tab's card away until the pointer leaves")
	}

	// 2. ↻ keeps it
	before := gridSeq(t, p)
	clickAt(t, p, "#rstrip button.rt.on [data-rerun]", proto.InputMouseButtonLeft)
	waitResult(t, p, before, "name")
	onScreen("the name kept by a rerun", "cat names")

	// 3. F2 from the grid; the strip redrawn mid-edit (a run landing, a
	// share elsewhere) keeps the box, its text and its focus; then Esc
	eval(t, p, `() => dbc.grid.focus()`)
	p.Keyboard.MustType(input.F2)
	waitFor(t, p, "the name's edit box focused", `() => document.activeElement && document.activeElement.matches("#rstrip .rrename")`)
	p.MustInsertText("half typ")
	eval(t, p, `() => dbc.cmd.syncResultTabs()`)
	waitFor(t, p, "the edit surviving a redraw", `() => {
	  const i = document.querySelector("#rstrip .rrename");
	  return !!i && i === document.activeElement && i.value === "half typ"; }`)
	shot(t, p, "result-tab-rename")
	typeName("not kept", input.Escape)
	onScreen("Esc leaving the name", "cat names")

	// 4. emptied: the query's title again
	eval(t, p, `() => dbc.grid.focus()`)
	p.Keyboard.MustType(input.F2)
	typeName("", input.Enter)
	onScreen("an emptied name giving the title back", q)

	// 5. named, then another query into the tab
	eval(t, p, `() => dbc.grid.focus()`)
	p.Keyboard.MustType(input.F2)
	typeName("soon gone", input.Enter)
	onScreen("named again", "soon gone")
	run("SELECT 1 AS one", "one")
	onScreen("another query clearing the name", "SELECT 1 AS one")
}
