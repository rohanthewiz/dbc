package e2e

import (
	"strings"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// editorMenu locks down the query editor's right-click menu (app.js
// editorMenu, editor.js bindMenu) — dbc's own menu in place of Monaco's:
//
//  1. A right-click on a statement moves the caret there first, and the Run
//     rows name what they would run, as the server's splitter sees it
//     (POST /api/v1/picks): "Run statement 2/2", "Run all 2 statements".
//     Picking Run runs the clicked statement, not the one the caret was on.
//     Monaco's own menu never opens.
//  2. With one statement in the buffer, Run all is dim and, picked, says
//     why in the log; Cut and Copy are dim with nothing selected.
//  3. A right-click inside a selection keeps it: Copy copies it, and Paste
//     puts the clipboard in at the caret.
//  4. Shift+F10 opens the same menu from the keyboard, and Esc gives the
//     editor its focus back.
func editorMenu(t *testing.T, _ *env, p *rod.Page) {
	waitConnected(t, p, "lite")
	eval(t, p, `() => {
	  dbc.editor.setText("SELECT name FROM cats ORDER BY id;\nSELECT count(*) AS n FROM cats");
	  const ed = monaco.editor.getEditors()[0];
	  ed.setPosition({ lineNumber: 1, column: 1 });
	  ed.focus();
	}`)
	rightClickAt := func(line, col int) {
		t.Helper()
		pt, _ := eval(t, p, `(line, col) => {
		  const ed = monaco.editor.getEditors()[0];
		  const v = ed.getScrolledVisiblePosition({ lineNumber: line, column: col });
		  const r = ed.getDomNode().getBoundingClientRect();
		  return [r.left + v.left + 2, r.top + v.top + v.height / 2];
		}`, line, col).([]any)
		p.Mouse.MustMoveTo(pt[0].(float64), pt[1].(float64))
		if err := p.Mouse.Click(proto.InputMouseButtonRight, 1); err != nil {
			t.Fatalf("right-click: %v", err)
		}
		waitFor(t, p, "the editor menu", `() => !!document.querySelector(".menu")`)
	}
	rows := func() string {
		t.Helper()
		return evalStr(t, p, `() => [...document.querySelectorAll(".menu .mitem")]
		  .map((b) => (b.classList.contains("off") ? "~" : "") + b.querySelector(".ml").textContent).join("|")`)
	}

	// 1. the clicked statement, named, and run
	rightClickAt(2, 10)
	got := rows()
	for _, want := range []string{"▶ Run statement 2/2|▶ Run all 2 statements|", "◈ Explain statement 2/2", "~Cut|~Copy|Paste",
		"History…", "Go to definition", "+ New console"} {
		if !strings.Contains(got, want) {
			t.Fatalf("menu rows %q lack %q", got, want)
		}
	}
	if line := evalStr(t, p, `() => String(monaco.editor.getEditors()[0].getPosition().lineNumber)`); line != "2" {
		t.Fatalf("caret on line %s after the right-click, want 2", line)
	}
	if n := evalStr(t, p, `() => String(document.querySelectorAll(".monaco-editor .context-view .monaco-menu, .context-view.monaco-menu-container").length)`); n != "0" {
		t.Fatalf("Monaco's own menu opened too (%s)", n)
	}
	before := gridSeq(t, p)
	menuPick(t, p, "▶ Run statement 2/2")
	if info := waitResult(t, p, before, "n"); info != "1 row" {
		t.Fatalf("grid info = %q, want the count's 1 row", info)
	}

	// 2. one statement: Run all says why instead of running
	eval(t, p, `() => dbc.editor.setText("SELECT 1 AS one")`)
	rightClickAt(1, 3)
	if got := rows(); !strings.HasPrefix(got, "▶ Run query|~▶ Run all|") {
		t.Fatalf("one-statement menu rows = %q", got)
	}
	menuPick(t, p, "▶ Run all")
	waitFor(t, p, "Run all's reason in the log", `() => [...document.querySelectorAll("#log > div")]
	  .some((d) => d.textContent.includes("the buffer holds one statement"))`)

	// 3. a selection survives the right-click; Copy and Paste
	eval(t, p, `() => {
	  const ed = monaco.editor.getEditors()[0];
	  ed.setSelection(new monaco.Selection(1, 1, 1, 9)); // "SELECT 1"
	  ed.focus();
	}`)
	setClipboard(t, p, "sentinel")
	rightClickAt(1, 4)
	if got := rows(); !strings.Contains(got, "|Cut|Copy|Paste|") || !strings.HasPrefix(got, "▶ Run selection|") {
		t.Fatalf("menu rows on a selection = %q", got)
	}
	menuPick(t, p, "Copy")
	waitFor(t, p, "the selection on the clipboard", `() => navigator.clipboard.readText().then((s) => s === "SELECT 1")`)
	eval(t, p, `(s) => navigator.clipboard.writeText(s)`, " -- pasted")
	eval(t, p, `() => monaco.editor.getEditors()[0].setPosition({ lineNumber: 1, column: 16 })`)
	rightClickAt(1, 16)
	menuPick(t, p, "Paste")
	waitFor(t, p, "the clipboard pasted at the caret", `() => dbc.editor.text() === "SELECT 1 AS one -- pasted"`)

	// 4. Shift+F10, and Esc back to the editor
	eval(t, p, `() => monaco.editor.getEditors()[0].focus()`)
	// Shift held by hand: KeyActions would release it before F10 lands
	if err := p.Keyboard.Press(input.ShiftLeft); err != nil {
		t.Fatal(err)
	}
	p.Keyboard.MustType(input.F10)
	if err := p.Keyboard.Release(input.ShiftLeft); err != nil {
		t.Fatal(err)
	}
	waitFor(t, p, "the menu from Shift+F10", `() => !!document.querySelector(".menu")`)
	p.Keyboard.MustType(input.Escape)
	waitFor(t, p, "the editor focused again", `() => !document.querySelector(".menu") && dbc.editor.hasFocus()`)
}
