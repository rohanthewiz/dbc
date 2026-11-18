package e2e

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
)

// completionDocs: N-328's docs on demand, in Monaco. An answer carries the
// docs of its first 20 items (web/complete.go's docsAhead); a row past
// those gets its doc when it is focused, through the provider's
// resolveCompletionItem and a docOf request. 30 tables named zz00…zz29
// make a list long enough: zz25's DDL in the details pane can only have
// come from that request.
//
// Stands on boot alone (DBC_E2E_STEPS=docs); the tables are dropped, and
// the connection refreshed, at the end.
func completionDocs(t *testing.T, e *env, p *rod.Page) {
	const n = 30
	var create, drop []string
	for i := range n {
		create = append(create, fmt.Sprintf("CREATE TABLE zz%02d (id INTEGER PRIMARY KEY, note TEXT)", i))
		drop = append(drop, fmt.Sprintf("DROP TABLE zz%02d", i))
	}
	refresh := func(what string, listed bool) {
		t.Helper()
		rightClick(t, p, `#conns .conn-item[data-conn="lite"]`)
		menuPick(t, p, "Refresh")
		waitFor(t, p, what, `(listed) => !document.querySelector("#conns .conn-item.connecting") &&
		  document.getElementById("status").textContent.includes("refreshed") &&
		  !!document.querySelector('#tables li[data-name="zz29"]') === listed`, listed)
	}
	for _, sql := range create {
		e.dbc(t, "lite", sql)
	}
	refresh("the zz tables listed", true)
	defer func() {
		for _, sql := range drop {
			e.dbc(t, "lite", sql)
		}
		refresh("the zz tables gone", false)
	}()

	eval(t, p, `() => {
	  dbc.editor.setText("SELECT * FROM zz");
	  const ed = monaco.editor.getEditors()[0];
	  ed.setPosition({ lineNumber: 1, column: 17 });
	  ed.focus();
	  ed.trigger("e2e", "editor.action.triggerSuggest", {});
	}`)
	rows := `() => [...document.querySelectorAll(".suggest-widget.visible .monaco-list-row")].map((r) => r.getAttribute("aria-label") || r.textContent).join("|")`
	waitFor(t, p, "the zz tables suggested", `() => (`+rows+`)().includes("zz00")`)
	// the details pane, which shows the focused row's documentation: an
	// overlay of its own beside the list, not inside .suggest-widget
	eval(t, p, `() => {
	  const d = document.querySelector(".suggest-details");
	  if (!d || !d.offsetParent) monaco.editor.getEditors()[0].trigger("e2e", "toggleSuggestionDetails", {});
	}`)
	details := `() => { const d = document.querySelector(".suggest-details"); return d && d.offsetParent ? d.textContent : ""; }`
	// the DDL names the table schema-qualified: CREATE TABLE main.zz00 (
	waitFor(t, p, "zz00's doc in the details pane, carried by the answer", `() => (`+details+`)().includes(".zz00 (")`)

	// down to zz25, past the first 20
	for range 25 {
		eval(t, p, `() => monaco.editor.getEditors()[0].trigger("e2e", "selectNextSuggestion", {})`)
	}
	waitFor(t, p, "zz25 focused and its doc asked for and shown", `() => (`+details+`)().includes(".zz25 (")`)
	if d := evalStr(t, p, details); strings.Contains(d, "zz24") {
		t.Fatalf("the details pane shows another table's doc too: %q", d)
	}
	p.Keyboard.MustType(input.Escape)
	eval(t, p, `() => dbc.editor.setText("")`)
}
