package e2e

import (
	"strings"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// backslashSplit is N-238 in the page. Where a string ends is the engine's
// rule, and on SQLite a backslash is plain text, so 'a\' closes at its quote
// and the statement after it is one of its own. The page's editor helpers
// are the server's splitter, and now say which engine they are for (the
// driver, dbc.state.driver) — before, they sent none and the server read
// MySQL's rule, so all three of these saw one run-on statement:
//
//  1. the gutter marker (POST /api/v1/stmt) sits on line 2 alone — with one
//     statement there would be no marker at all;
//  2. the right-click menu (POST /api/v1/picks) offers "Run statement 2/2";
//  3. Run, which the workspace splits by its own connection's engine, runs
//     the second statement alone.
func backslashSplit(t *testing.T, _ *env, p *rod.Page) {
	waitConnected(t, p, "lite")
	eval(t, p, `() => {
	  dbc.editor.setText("SELECT 'a\\' AS v;\nSELECT 2 AS n");
	  const ed = monaco.editor.getEditors()[0];
	  ed.setPosition({ lineNumber: 2, column: 3 });
	  ed.focus();
	}`)

	// 1. the marker, on the second statement's line
	waitFor(t, p, "the statement marker on line 2", `() => {
	  const ed = monaco.editor.getEditors()[0];
	  const marks = ed.getModel().getAllDecorations()
	    .filter((d) => d.options.linesDecorationsClassName === "stmt-mark");
	  return marks.length === 1 && marks[0].range.startLineNumber === 2 && marks[0].range.endLineNumber === 2;
	}`)

	// 2. the menu names the second of two statements
	pt, _ := eval(t, p, `() => {
	  const ed = monaco.editor.getEditors()[0];
	  const v = ed.getScrolledVisiblePosition({ lineNumber: 2, column: 3 });
	  const r = ed.getDomNode().getBoundingClientRect();
	  return [r.left + v.left + 2, r.top + v.top + v.height / 2];
	}`).([]any)
	p.Mouse.MustMoveTo(pt[0].(float64), pt[1].(float64))
	if err := p.Mouse.Click(proto.InputMouseButtonRight, 1); err != nil {
		t.Fatalf("right-click: %v", err)
	}
	waitFor(t, p, "the editor menu", `() => !!document.querySelector(".menu")`)
	rows := evalStr(t, p, `() => [...document.querySelectorAll(".menu .mitem .ml")].map((e) => e.textContent).join("|")`)
	if !strings.Contains(rows, "▶ Run statement 2/2|▶ Run all 2 statements") {
		t.Fatalf("menu rows %q, want the second of two statements", rows)
	}

	// 3. Run runs it, on its own
	before := gridSeq(t, p)
	menuPick(t, p, "▶ Run statement 2/2")
	if info := waitResult(t, p, before, "n"); info != "1 row" {
		t.Fatalf("grid info = %q, want SELECT 2's 1 row", info)
	}
}
