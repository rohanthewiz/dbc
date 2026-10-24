package e2e

import (
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// formatSQL locks down Format (editor.js registerFormatting, over POST
// /api/v1/format — the TUI's ⌥⇧F):
//
//  1. Shift+Alt+F with nothing selected formats the whole buffer, and
//     unwraps a query pasted from a log line on the way — its label, its
//     quotes, its \n escapes; one undo brings the paste back.
//  2. With a selection, the right-click menu's row is "Format selection"
//     and formats only the selected lines.
func formatSQL(t *testing.T, _ *env, p *rod.Page) {
	waitConnected(t, p, "lite")
	text := `() => monaco.editor.getEditors()[0].getModel().getValue()`
	eval(t, p, `() => {
	  dbc.editor.setText('"SQL query:\\nselect id,name from cats where id=1;\\nselect 2"');
	  const ed = monaco.editor.getEditors()[0];
	  ed.setPosition({ lineNumber: 1, column: 1 });
	  ed.focus();
	}`)
	for _, m := range []input.Key{input.ShiftLeft, input.AltLeft} {
		if err := p.Keyboard.Press(m); err != nil {
			t.Fatal(err)
		}
	}
	p.Keyboard.MustType(input.KeyF)
	for _, m := range []input.Key{input.AltLeft, input.ShiftLeft} {
		if err := p.Keyboard.Release(m); err != nil {
			t.Fatal(err)
		}
	}
	want := "SELECT id, name\nFROM cats\nWHERE id = 1;\n\nSELECT 2"
	waitFor(t, p, "Shift+Alt+F formatting the buffer", `(want) => monaco.editor.getEditors()[0].getModel().getValue() === want`, want)
	eval(t, p, `() => monaco.editor.getEditors()[0].trigger("e2e", "undo", null)`)
	if got := evalStr(t, p, text); got != `"SQL query:\nselect id,name from cats where id=1;\nselect 2"` {
		t.Errorf("one undo should bring the paste back, got %q", got)
	}

	// 2. a selection of the second statement's lines, from the menu
	eval(t, p, `() => {
	  dbc.editor.setText("SELECT 1;\nselect id,name from cats;\nSELECT 3;");
	  const ed = monaco.editor.getEditors()[0];
	  ed.setSelection({ startLineNumber: 2, startColumn: 1, endLineNumber: 3, endColumn: 1 });
	  ed.focus();
	}`)
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
	menuPick(t, p, "Format selection")
	waitFor(t, p, "the selection formatted, the rest as it was",
		`(want) => monaco.editor.getEditors()[0].getModel().getValue() === want`,
		"SELECT 1;\nSELECT id, name\nFROM cats;\nSELECT 3;")
}
