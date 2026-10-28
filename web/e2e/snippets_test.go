package e2e

import (
	"testing"

	"github.com/go-rod/rod"
)

// CDP's Shift bit, beside harness_test.go's Alt, Ctrl and Meta.
const modShift = 8

// liveSnippets locks down snippets in Monaco (editor.js "live snippets",
// snippets.js, over web/snippets.go):
//
//  1. A starter's name then Tab expands it at the line's indent, with the
//     first stop's default selected; typing fills the stop, Tab moves on,
//     and a stop used twice mirrors — Monaco's snippet controller doing
//     the stepping on the server's expansion.
//  2. Tab expands the name even with the suggestion widget open over it.
//  3. Alt+S with text selected: the dialog says a pick wraps it, the
//     filter finds countof, Enter wraps the selection.
//  4. The dialog's form makes a snippet (Alt+N, Ctrl+S), whose name then
//     expands; Alt+D deletes it behind a confirmation.
func liveSnippets(t *testing.T, _ *env, p *rod.Page) {
	waitConnected(t, p, "lite")
	waitFor(t, p, "the snippets list", `() => dbc.snippets && dbc.snippets.has("cnt")`)
	selected := `() => { const ed = monaco.editor.getEditors()[0]; return ed.getModel().getValueInRange(ed.getSelection()); }`
	tab := func(mod int) { chord(t, p, mod, "Tab", "Tab", 9) }
	setCaretEnd := func(text string) {
		eval(t, p, `(text) => {
		  dbc.editor.setText(text);
		  const ed = monaco.editor.getEditors()[0], m = ed.getModel();
		  ed.setPosition(m.getPositionAt(text.length));
		  ed.focus();
		}`, text)
	}

	// 1. expand, fill, step, mirror
	setCaretEnd("  cnt")
	waitFor(t, p, "Tab to be the snippet's", `() => monaco.editor.getEditors()[0]._contextKeyService.getContextKeyValue("dbcSnippetWord") === true`)
	tab(0)
	waitFor(t, p, "cnt expanded at the indent", `(want) => monaco.editor.getEditors()[0].getModel().getValue() === want`,
		"  SELECT column, count(*) AS n\n  FROM table\n  GROUP BY column\n  ORDER BY n DESC")
	if got := evalStr(t, p, selected); got != "table" {
		t.Fatalf("the first stop (VAR1) should be selected, got %q", got)
	}
	p.MustInsertText("cats")
	tab(0)
	if got := evalStr(t, p, selected); got != "column" {
		t.Fatalf("Tab should select the second stop, got %q", got)
	}
	p.MustInsertText("breed")
	waitFor(t, p, "the mirror filled", `(want) => monaco.editor.getEditors()[0].getModel().getValue() === want`,
		"  SELECT breed, count(*) AS n\n  FROM cats\n  GROUP BY breed\n  ORDER BY n DESC")
	tab(modShift)
	if got := evalStr(t, p, selected); got != "cats" {
		t.Fatalf("Shift+Tab should go back to the first stop, got %q", got)
	}

	// 2. the suggestion widget open over the name: Tab still expands
	setCaretEnd("")
	p.MustInsertText("se")
	p.MustInsertText("l")
	eval(t, p, `() => monaco.editor.getEditors()[0].trigger("e2e", "editor.action.triggerSuggest", {})`)
	waitFor(t, p, "the suggestions", `() => !!document.querySelector(".suggest-widget.visible")`)
	tab(0)
	waitFor(t, p, "sel expanded over the suggestions", `(want) => monaco.editor.getEditors()[0].getModel().getValue() === want`,
		"SELECT *\nFROM table\nWHERE true\nLIMIT 100")

	// 3. Alt+S with a selection wraps it
	eval(t, p, `() => {
	  dbc.editor.setText("SELECT id FROM cats\nSELECT 2");
	  const ed = monaco.editor.getEditors()[0];
	  ed.setSelection({ startLineNumber: 1, startColumn: 1, endLineNumber: 1, endColumn: 20 });
	  ed.focus();
	}`)
	chord(t, p, modAlt, "s", "KeyS", 83)
	waitFor(t, p, "the dialog, wrapping", `() => /wraps the selection/.test(document.querySelector(".modal .mtitle")?.textContent || "")`)
	p.MustElement(".modal .hfilter").MustInput("countof")
	waitFor(t, p, "countof first", `() => document.querySelector(".snlist li.cur .hconn")?.textContent === "countof"`)
	chord(t, p, 0, "Enter", "Enter", 13)
	waitFor(t, p, "the selection wrapped", `(want) => monaco.editor.getEditors()[0].getModel().getValue() === want`,
		"SELECT count(*)\nFROM (\n    SELECT id FROM cats\n) AS t\nSELECT 2")

	// 4. make one in the form, use it, delete it
	chord(t, p, modAlt, "s", "KeyS", 83)
	waitFor(t, p, "the dialog", `() => !!document.querySelector(".snlist")`)
	chord(t, p, modAlt, "n", "KeyN", 78)
	waitFor(t, p, "the form", `() => !!document.querySelector(".snform")`)
	eval(t, p, `() => {
	  const [name, desc] = document.querySelectorAll(".snform input");
	  name.value = "e2e_pick"; desc.value = "made by the e2e";
	  document.querySelector(".snform textarea").value = "SELECT $VAR1:x$ -- $VAR1";
	}`)
	chord(t, p, modCtrl, "s", "KeyS", 83)
	waitFor(t, p, "a broken body refused in the form", `() => /not closed/.test(document.querySelector(".snerr")?.textContent || "")`)
	eval(t, p, `() => { document.querySelector(".snform textarea").value = "SELECT $VAR1:x$ -- $VAR1$"; }`)
	chord(t, p, modCtrl, "s", "KeyS", 83)
	waitFor(t, p, "back in the dialog on it", `() => document.querySelector(".snlist li.cur .hconn")?.textContent === "e2e_pick"`)
	chord(t, p, 0, "Escape", "Escape", 27)
	setCaretEnd("e2e_pick")
	waitFor(t, p, "Tab to know the new name", `() => monaco.editor.getEditors()[0]._contextKeyService.getContextKeyValue("dbcSnippetWord") === true`)
	tab(0)
	waitFor(t, p, "e2e_pick expanded", `() => monaco.editor.getEditors()[0].getModel().getValue() === "SELECT x -- x"`)
	p.MustInsertText("yy")
	waitFor(t, p, "its mirror", `() => monaco.editor.getEditors()[0].getModel().getValue() === "SELECT yy -- yy"`)

	chord(t, p, modAlt, "s", "KeyS", 83)
	p.MustElement(".modal .hfilter").MustInput("e2e_pick")
	waitFor(t, p, "e2e_pick under the cursor", `() => document.querySelector(".snlist li.cur .hconn")?.textContent === "e2e_pick"`)
	chord(t, p, modAlt, "d", "KeyD", 68)
	p.MustElementR(".modal .mfoot button", "^Delete e2e_pick$").MustClick()
	waitFor(t, p, "e2e_pick gone", `() => !!document.querySelector(".snlist") && !dbc.snippets.has("e2e_pick")`)
	chord(t, p, 0, "Escape", "Escape", 27)
}
