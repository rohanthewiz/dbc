package e2e

import (
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// newConsoleFromConn: a connection's right-click menu has "New query
// console", which opens a NEW query tab on that connection showing a NEW,
// empty console of its database — and leaves the tab it was picked from on
// the connection it was on.
//
// "New" is the point: a plain new tab (Alt+T) takes the first console of
// the database that no tab here shows, text and all (app.js followConsole).
// After "consoles per database", lite2 has such a console ("console", with
// "SELECT 'on lite2'" in it), so a tab that reused it would show that text
// and fail the empty-editor check below. Run alone (DBC_E2E_STEPS=new
// console) lite2 may have no console file yet, and the check still holds,
// only more weakly.
func newConsoleFromConn(t *testing.T, e *env, p *rod.Page) {
	waitConnected(t, p, "lite")
	from := evalStr(t, p, `() => dbc.state.tab.key`)

	// offered on the active row too: a second console beside the first
	rightClick(t, p, `#conns .conn-item[data-conn="lite"]`)
	waitFor(t, p, "New query console on the active row", `() => [...document.querySelectorAll(".menu .mitem .ml")]
	  .some((b) => b.textContent === "New query console")`)
	p.Keyboard.MustType(input.Escape)
	waitFor(t, p, "the menu closed", `() => !document.querySelector(".menu")`)

	rightClick(t, p, `#conns .conn-item[data-conn="lite2"]`)
	menuPick(t, p, "New query console")
	waitFor(t, p, "a new tab on lite2", `(k) => dbc.state.tab.key !== k && dbc.state.tab.conn === "lite2"`, from)
	waitConnected(t, p, "lite2")
	// its console: shown in the editor, empty, and not one of lite2's
	// console files (cdb.names lists those; a new console has none until
	// something is typed in it)
	waitFor(t, p, "a new console of lite2", `() => {
	  const t = dbc.state.tab, k = dbc.editor.docKey();
	  return !!t.console && k.startsWith("c:") && k.endsWith("/" + t.console) &&
	    dbc.editor.text() === "" && !t.cdb.names.includes(t.console);
	}`)

	// the tab it was picked from is still on lite. Closing the new one
	// brings on its neighbour, which later in the full suite is another
	// tab (earlier steps leave several), so the first is clicked back.
	opened := evalStr(t, p, `() => dbc.state.tab.key`)
	eval(t, p, `() => dbc.cmd.closeTab()`)
	waitFor(t, p, "the new tab closed", `(k) => !document.querySelector('#qtabs .qtab[data-key="' + k + '"]')`, opened)
	clickAt(t, p, `#qtabs .qtab[data-key="`+from+`"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "back on the first tab", `(k) => dbc.state.tab.key === k`, from)
	waitConnected(t, p, "lite")
}
