package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// scriptsGood is the script the step fixes its broken one into: two
// results shown (so the results bar's switcher appears) and a line
// printed (so the log is checked too).
const scriptsGood = `// Two results from the cats.
//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	r, err := s.Query("lite", "SELECT name FROM cats ORDER BY id")
	if err != nil {
		return err
	}
	s.Show(r)
	n, err := s.Query("lite", "SELECT count(*) AS n FROM cats")
	if err != nil {
		return err
	}
	s.Show(n)
	s.Print("e2e script done")
	return nil
}
`

// scriptTabs walks the scripts browser and a script tab through the
// whole life of a script (scripts revamp phase 4):
//
//	Ctrl+O on an empty dir ─► the templates and the examples are listed
//	New from "query"       ─► a named file, opened in a script tab (Go)
//	a compile error        ─► a marker, "1 error" in the header, ● unsaved
//	completion             ─► s. lists S's methods; s.Query(" the connections
//	fixed, Ctrl+S          ─► on disk, ● gone
//	▶ Run                  ─► both s.Shows: the switcher, the log line
//	an unsaved edit        ─► kept across a reload (the draft)
//	a save over a change   ─► the conflict: the disk's text, undo gets ours
//	rename, trash, restore ─► the tab follows each, the file moves
//	close with changes     ─► asked first; Discard closes
func scriptTabs(t *testing.T, e *env, p *rod.Page) {
	dir := filepath.Join(e.home, ".config", "dbc", "scripts")
	disk := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "<" + err.Error() + ">"
		}
		return string(b)
	}
	// ctrl presses Ctrl+k, holding Ctrl by hand: KeyActions releases
	// every key at its end, and Monaco takes Ctrl (WinCtrl) on a Mac too
	ctrl := func(k input.Key) {
		t.Helper()
		if err := p.Keyboard.Press(input.ControlLeft); err != nil {
			t.Fatal(err)
		}
		p.Keyboard.MustType(k)
		if err := p.Keyboard.Release(input.ControlLeft); err != nil {
			t.Fatal(err)
		}
	}
	setText := func(s string) {
		t.Helper()
		eval(t, p, `(s) => { dbc.editor.setText(s); monaco.editor.getEditors()[0].focus(); }`, s)
	}

	// ── the browser, on an empty scripts dir ──────────────────────────────
	clickSel(t, p, "#scripts-btn", proto.InputMouseButtonLeft)
	waitFor(t, p, "the scripts browser", `() => !!document.querySelector(".modal.scripts .slist")`)
	got := evalStr(t, p, `() => [document.querySelectorAll(".slist .srow.template").length > 0,
	  document.querySelectorAll(".slist .srow.example").length > 0,
	  !!document.querySelector(".slist .snote")].join("|")`)
	if got != "true|true|true" {
		t.Fatalf("empty dir's browser (templates|examples|note) = %q", got)
	}
	if title := evalStr(t, p, `() => document.querySelector(".modal.scripts .mtitle").textContent`); !strings.Contains(title, ".config/dbc/scripts") {
		t.Fatalf("browser title %q does not name the scripts dir", title)
	}

	// ── new from the query template ───────────────────────────────────────
	eval(t, p, `() => {
	  const row = [...document.querySelectorAll(".slist .srow.template")].find((r) => /query/i.test(r.textContent));
	  row.querySelector(".sact button").click();
	}`)
	waitFor(t, p, "the name prompt", `() => { const i = document.querySelector(".modal input.hfilter"); return !!i && i.value === "query.go"; }`)
	eval(t, p, `() => { document.querySelector(".modal input.hfilter").value = "e2e_report"; document.querySelector(".modal button.primary").click(); }`)
	waitFor(t, p, "the script tab", `() => {
	  const t = document.querySelector("#qtabs .qtab.script.on .qt");
	  return !!t && t.textContent === "e2e_report.go" && document.querySelector(".app").classList.contains("script-mode")
	    && monaco.editor.getEditors()[0].getModel().getLanguageId() === "go";
	}`)
	if d := disk("e2e_report.go"); !strings.Contains(d, `const conn = "lite"`) {
		t.Fatalf("the new script on disk is not the filled template:\n%s", d)
	}

	// ── a compile error is marked as you type ─────────────────────────────
	setText("package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\nfunc Run(s *sdb.S) error {\n\treturn nope\n}\n")
	waitFor(t, p, "the marker on the undefined name", `() => {
	  const m = monaco.editor.getModelMarkers({ owner: "dbc" });
	  return m.length === 1 && m[0].startLineNumber === 6 && m[0].severity === monaco.MarkerSeverity.Error;
	}`)
	waitFor(t, p, "the header and the strip saying so", `() =>
	  document.getElementById("active-conn").textContent === "▷ e2e_report.go ● · 1 error" &&
	  !!document.querySelector("#qtabs .qtab.script.on .qdirty")`)

	// ── completion from the sdb API, and connection names ────────────────
	setText("package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\nfunc Run(db *sdb.S) error {\n\t\n\treturn nil\n}\n")
	eval(t, p, `() => { const ed = monaco.editor.getEditors()[0]; ed.setPosition({ lineNumber: 6, column: 2 }); ed.focus(); }`)
	p.Keyboard.MustType(input.KeyD, input.KeyB, input.Period)
	suggest := `() => [...document.querySelectorAll(".suggest-widget.visible .monaco-list-row")]
	  .map((r) => r.getAttribute("aria-label") || r.textContent).join("|")`
	waitFor(t, p, "S's methods after db. (Run's own name for it)", `() => {
	  const r = (`+suggest+`)();
	  return ["Query", "Copy", "Show", "Print"].every((m) => r.includes(m));
	}`)
	p.Keyboard.MustType(input.Escape)
	eval(t, p, `() => { const ed = monaco.editor.getEditors()[0];
	  ed.executeEdits("e2e", [{ range: new monaco.Range(6, 1, 6, 100), text: "\tdb.Query(\"" }]);
	  ed.setPosition({ lineNumber: 6, column: 12 });
	  ed.trigger("e2e", "editor.action.triggerSuggest", {}); }`)
	waitFor(t, p, "the connections inside Query's first argument", `() => {
	  const r = (`+suggest+`)();
	  return r.includes("lite") && r.includes("lite2");
	}`)
	p.Keyboard.MustType(input.Escape)
	// a variable assigned from a call is that call's first result type:
	// r, err := db.Query(…) makes r. list Result's fields and methods
	eval(t, p, `() => { const ed = monaco.editor.getEditors()[0];
	  ed.executeEdits("e2e", [{ range: new monaco.Range(6, 1, 6, 100), text: "\tr, err := db.Query(\"lite\", \"SELECT 1\")\n\t_ = err\n\tr." }]);
	  ed.setPosition({ lineNumber: 8, column: 4 });
	  ed.trigger("e2e", "editor.action.triggerSuggest", {}); }`)
	waitFor(t, p, "Result's fields after r.", `() => {
	  const r = (`+suggest+`)();
	  return ["Rows", "Columns", "RowCount"].every((m) => r.includes(m)) && !r.includes("Copy");
	}`)
	p.Keyboard.MustType(input.Escape)
	// hover on a method: its signature and doc comment
	eval(t, p, `() => { const ed = monaco.editor.getEditors()[0]; ed.setPosition({ lineNumber: 6, column: 16 }); ed.focus();
	  ed.trigger("e2e", "editor.action.showHover", {}); }`)
	waitFor(t, p, "Query's signature in the hover", `() => [...document.querySelectorAll(".monaco-hover")]
	  .some((h) => h.textContent.includes("func (s *S) Query(conn, query string, args ...any) (*Result, error)"))`)
	p.Keyboard.MustType(input.Escape)
	if got := evalStr(t, p, `() => [
	  dbc.scripts.firstResult("func (s *S) Query(conn, query string, args ...any) (*Result, error)"),
	  dbc.scripts.firstResult("func (s *S) Conns() []string"),
	  dbc.scripts.firstResult("func (s *S) Show(r *Result)"),
	  dbc.scripts.firstResult("func IsCanceled(err error) bool"),
	  dbc.scripts.inString('x := "abc'), dbc.scripts.inString('x := "a" + y'), dbc.scripts.inString("// a \"quote"),
	].join("|")`); got != "Result|||bool|true|false|true" {
		t.Errorf("completion helpers = %q", got)
	}

	// ── fixed, saved with Ctrl+S ──────────────────────────────────────────
	setText(scriptsGood)
	waitFor(t, p, "the markers cleared", `() => monaco.editor.getModelMarkers({ owner: "dbc" }).length === 0`)
	ctrl(input.KeyS)
	waitFor(t, p, "saved: no ● in the header", `() => document.getElementById("active-conn").textContent === "▷ e2e_report.go"`)
	if d := disk("e2e_report.go"); d != scriptsGood {
		t.Fatalf("on disk after Ctrl+S:\n%s", d)
	}

	// ── ▶ Run: both results, the switcher between them, the log ──────────
	before := gridSeq(t, p)
	clickSel(t, p, "#run", proto.InputMouseButtonLeft)
	if info := waitResult(t, p, before, "n"); info != "1 row" {
		t.Fatalf("grid info after the run = %q, want the last s.Show's 1 row", info)
	}
	waitFor(t, p, "Result 1 · 2, on 2", `() => {
	  const b = [...document.querySelectorAll("#rsets button")];
	  return !document.getElementById("rsets").hidden && b.map((x) => x.textContent).join() === "1,2" && b[1].classList.contains("on");
	}`)
	waitFor(t, p, "the script's s.Print in the log", `() => document.getElementById("log").textContent.includes("e2e script done")`)
	before = gridSeq(t, p)
	clickSel(t, p, `#rsets button[data-set="0"]`, proto.InputMouseButtonLeft)
	if info := waitResult(t, p, before, "name"); info != "3 rows" {
		t.Fatalf("grid info on Result 1 = %q, want 3 rows", info)
	}

	// ── an unsaved edit survives a reload (the draft) ────────────────────
	draft := strings.Replace(scriptsGood, "e2e script done", "e2e draft", 1)
	setText(draft)
	waitFor(t, p, "the draft kept", `() => (localStorage.getItem("dbc.script.draft.e2e_report.go") || "").includes("e2e draft")`)
	p.MustReload()
	p.MustWaitLoad()
	waitFor(t, p, "the script tab back with its draft", `() => {
	  const ed = window.monaco && monaco.editor.getEditors()[0];
	  return !!ed && ed.getValue().includes("e2e draft") &&
	    document.getElementById("active-conn").textContent.startsWith("▷ e2e_report.go ●");
	}`)

	// ── a save over a change made elsewhere: the conflict ────────────────
	elsewhere := strings.Replace(scriptsGood, "e2e script done", "changed in vim", 1)
	if err := os.WriteFile(filepath.Join(dir, "e2e_report.go"), []byte(elsewhere), 0o600); err != nil {
		t.Fatal(err)
	}
	eval(t, p, `() => monaco.editor.getEditors()[0].focus()`)
	ctrl(input.KeyS)
	waitFor(t, p, "the file's version in the editor", `() => monaco.editor.getEditors()[0].getValue().includes("changed in vim")`)
	if d := disk("e2e_report.go"); d != elsewhere {
		t.Fatalf("the conflicting save wrote over the file:\n%s", d)
	}
	eval(t, p, `() => monaco.editor.getEditors()[0].trigger("e2e", "undo", null)`)
	waitFor(t, p, "undo brings the draft back", `() => monaco.editor.getEditors()[0].getValue().includes("e2e draft")`)
	ctrl(input.KeyS)
	waitFor(t, p, "the draft saved over it", `() => document.getElementById("active-conn").textContent === "▷ e2e_report.go"`)
	if d := disk("e2e_report.go"); d != draft {
		t.Fatalf("after undo and save, on disk:\n%s", d)
	}

	// ── rename (double-click the tab), trash and restore ─────────────────
	eval(t, p, `() => document.querySelector("#qtabs .qtab.script .qt").dispatchEvent(new MouseEvent("dblclick", { bubbles: true }))`)
	waitFor(t, p, "the rename prompt", `() => { const i = document.querySelector(".modal input.hfilter"); return !!i && i.value === "e2e_report.go"; }`)
	eval(t, p, `() => { document.querySelector(".modal input.hfilter").value = "e2e_renamed.go"; document.querySelector(".modal button.primary").click(); }`)
	waitFor(t, p, "the tab renamed", `() => document.querySelector("#qtabs .qtab.script .qt").textContent === "e2e_renamed.go" &&
	  document.getElementById("active-conn").textContent === "▷ e2e_renamed.go"`)
	if d := disk("e2e_renamed.go"); d != draft {
		t.Fatalf("e2e_renamed.go on disk:\n%s", d)
	}

	ctrl(input.KeyO)
	waitFor(t, p, "the browser listing it", `() => !!document.querySelector('.slist .srow.script[data-name="e2e_renamed.go"]')`)
	eval(t, p, `() => document.querySelector(".modal.scripts input.hfilter").focus()`)
	ctrl(input.Delete)
	waitFor(t, p, "the trash section, and the tab unsaved", `() =>
	  !document.querySelector('.slist .srow.script[data-name="e2e_renamed.go"]') &&
	  [...document.querySelectorAll(".slist li.shead")].some((h) => h.textContent.startsWith("Trash (1)")) &&
	  document.getElementById("active-conn").textContent.startsWith("▷ e2e_renamed.go ●")`)
	if d := disk("e2e_renamed.go"); !strings.HasPrefix(d, "<") {
		t.Fatal("e2e_renamed.go is still in the scripts dir after the trash")
	}
	eval(t, p, `() => [...document.querySelectorAll(".slist li.shead")].find((h) => h.textContent.startsWith("Trash")).click()`)
	waitFor(t, p, "the trashed row", `() => !!document.querySelector('.slist .srow.trash[data-name="e2e_renamed.go"]')`)
	eval(t, p, `() => document.querySelector('.slist .srow.trash[data-name="e2e_renamed.go"] .sact button').click()`)
	waitFor(t, p, "restored: the tab saved again", `() => document.getElementById("active-conn").textContent === "▷ e2e_renamed.go"`)
	if d := disk("e2e_renamed.go"); d != draft {
		t.Fatalf("restored e2e_renamed.go:\n%s", d)
	}

	// ── closing with unsaved changes asks; Discard closes ────────────────
	setText(scriptsGood + "// more\n")
	waitFor(t, p, "unsaved again", `() => !!document.querySelector("#qtabs .qtab.script .qdirty")`)
	eval(t, p, `() => document.querySelector("#qtabs .qtab.script .qx").click()`)
	waitFor(t, p, "the close prompt", `() => !!document.querySelector(".modal") && document.querySelector(".modal .mtitle").textContent.includes("e2e_renamed.go")`)
	eval(t, p, `() => [...document.querySelectorAll(".modal .mfoot button")].find((b) => b.textContent === "Discard changes").click()`)
	waitFor(t, p, "the script tab closed, the query tab back", `() =>
	  !document.querySelector("#qtabs .qtab.script") && !document.querySelector(".app").classList.contains("script-mode") &&
	  localStorage.getItem("dbc.script.draft.e2e_renamed.go") === null`)
	if d := disk("e2e_renamed.go"); d != draft {
		t.Fatalf("Discard changed the file:\n%s", d)
	}
}

// clickSel clicks the middle of the element sel matches, by coordinates:
// an element handle can go stale under a redraw and leave rod waiting on
// it forever (see tabGroups' at, which this generalizes).
func clickSel(t *testing.T, p *rod.Page, sel string, button proto.InputMouseButton) {
	t.Helper()
	waitFor(t, p, sel, `(s) => !!document.querySelector(s)`, sel)
	box := eval(t, p, `(s) => { const r = document.querySelector(s).getBoundingClientRect();
	  return [r.x + r.width / 2, r.y + r.height / 2]; }`, sel).([]any)
	p.Mouse.MustMoveTo(box[0].(float64), box[1].(float64))
	if err := p.Mouse.Click(button, 1); err != nil {
		t.Fatalf("click %s: %v", sel, err)
	}
}
