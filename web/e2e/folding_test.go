package e2e

import (
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// statementFolding locks down the query editor's folds (editor.js
// registerFolding, over POST /api/v1/folds):
//
//  1. Each multi-line statement folds from its first line of code (its
//     heading comment stays out), and a multi-line (…) inside it folds on
//     its own, up to the line before a ")" that opens its line.
//  2. The right-click menu's "Fold the other statements" folds every
//     statement but the one clicked — their blocks stay open inside; from
//     a line between statements it folds them all — and "Unfold all"
//     opens them.
//  3. Ctrl+Shift+[ folds the innermost range at the caret (the subquery,
//     not its statement) and Ctrl+Shift+] opens it — on a Mac too, where
//     Monaco's own keys are ⌘⌥[ and ].
func statementFolding(t *testing.T, _ *env, p *rod.Page) {
	waitConnected(t, p, "lite")
	eval(t, p, `() => {
	  dbc.editor.setText([
	    "-- names",          // 1
	    "SELECT name,",      // 2  ┐ statement
	    "  id",              // 3  │
	    "FROM cats;",        // 4  ┘
	    "SELECT *",          // 5  ┐ statement
	    "FROM (",            // 6  │ ┐ subquery
	    "  SELECT 1 AS x",   // 7  │ │
	    "  FROM cats",       // 8  │ ┘
	    ") s;",              // 9  ┘
	    "SELECT 1;",         // 10    one line: no fold
	  ].join("\n"));
	  const ed = monaco.editor.getEditors()[0];
	  ed.setPosition({ lineNumber: 1, column: 1 });
	  ed.focus();
	}`)
	// regions reads Monaco's folding model: "start-end" per range, with
	// a "+" on the collapsed ones. It resolves once the model's ranges
	// are the server's (the provider answers asynchronously).
	regions := `() => monaco.editor.getEditors()[0].getContribution("editor.contrib.folding").getFoldingModel()
	  .then((m) => { const r = m.regions, out = [];
	    for (let i = 0; i < r.length; i++) out.push(r.getStartLineNumber(i) + "-" + r.getEndLineNumber(i) + (r.isCollapsed(i) ? "+" : ""));
	    return out.join(" "); })`
	want := func(what, ranges string) {
		t.Helper()
		// waitFor's loop, but a timeout names the folds Monaco has instead
		for deadline := time.Now().Add(waitLimit); ; time.Sleep(100 * time.Millisecond) {
			got := evalStr(t, p, regions)
			if got == ranges {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: folds are %q, want %q", what, got, ranges)
			}
		}
	}

	// 1. the server's ranges, none folded yet
	want("the statements' and the subquery's folds", "2-4 5-9 6-8")

	// 2. the menu's rows
	rightClickLine := func(line int) {
		t.Helper()
		pt, _ := eval(t, p, `(line) => {
		  const ed = monaco.editor.getEditors()[0];
		  const v = ed.getScrolledVisiblePosition({ lineNumber: line, column: 3 });
		  const r = ed.getDomNode().getBoundingClientRect();
		  return [r.left + v.left + 2, r.top + v.top + v.height / 2];
		}`, line).([]any)
		p.Mouse.MustMoveTo(pt[0].(float64), pt[1].(float64))
		if err := p.Mouse.Click(proto.InputMouseButtonRight, 1); err != nil {
			t.Fatalf("right-click: %v", err)
		}
		waitFor(t, p, "the editor menu", `() => !!document.querySelector(".menu")`)
	}
	rightClickLine(2)
	menuPick(t, p, "Fold the other statements")
	want("the second statement folded, the clicked one open", "2-4 5-9+ 6-8")
	// the gutter's arrows show while the pointer is over it: beside the
	// statement marker, on the folded line, and the "⋯" after its text
	p.Mouse.MustMoveTo(evalNum(t, p, `() => monaco.editor.getEditors()[0].getDomNode().getBoundingClientRect().left + 30`),
		evalNum(t, p, `() => monaco.editor.getEditors()[0].getDomNode().getBoundingClientRect().top + 30`))
	shot(t, p, "folding")
	rightClickLine(1) // the heading comment: in no statement's fold
	menuPick(t, p, "Fold the other statements")
	want("both statements folded, the subquery open inside", "2-4+ 5-9+ 6-8")
	rightClickLine(1)
	menuPick(t, p, "Unfold all")
	want("everything open again", "2-4 5-9 6-8")

	// 3. the keys, at a caret in the subquery: Ctrl held by hand, as
	// KeyActions would release it before the bracket lands
	eval(t, p, `() => { const ed = monaco.editor.getEditors()[0]; ed.setPosition({ lineNumber: 7, column: 5 }); ed.focus(); }`)
	chord := func(k input.Key) {
		t.Helper()
		for _, m := range []input.Key{input.ControlLeft, input.ShiftLeft} {
			if err := p.Keyboard.Press(m); err != nil {
				t.Fatal(err)
			}
		}
		p.Keyboard.MustType(k)
		for _, m := range []input.Key{input.ShiftLeft, input.ControlLeft} {
			if err := p.Keyboard.Release(m); err != nil {
				t.Fatal(err)
			}
		}
	}
	chord(input.BracketLeft)
	want("Ctrl+Shift+[ folding the subquery alone", "2-4 5-9 6-8+")
	chord(input.BracketRight)
	want("Ctrl+Shift+] opening it", "2-4 5-9 6-8")
}
