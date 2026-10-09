package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// pipelineTabs builds a pipeline on the canvas by hand, as Phase 2 of
// ai_docs/plans/pipelines.md promises — "built, previewed and run without
// writing JSON or Go":
//
//	Ctrl+O              ─► the Pipelines section: none yet, the examples
//	+ New ▾ → Pipeline  ─► a named file, a pipeline tab: the canvas over
//	                       the editor, a lane with src ─► peek
//	the inspector       ─► src's query typed in (its plugin's fields)
//	a palette drag      ─► rows.filter dropped on the lane, its rule set
//	two wires by hand   ─► src's output to filter, filter's to a second
//	                       dropped preview (src ─► filter replaced nothing:
//	                       filter had no input)
//	◎ Preview           ─► both branches' rows in the grid, "Result 1 · 2",
//	                       the filtered one with fewer rows; nothing written
//	Ctrl+S              ─► on disk, ● gone, in the shape Spec.JSON writes
//	▶ Run               ─► the engine runs it: the lane ✓, the filter's card
//	                       counts 3 → 2, the summary in the log
//	{ } JSON            ─► the same text in the editor; back to the canvas
func pipelineTabs(t *testing.T, e *env, p *rod.Page) {
	dir := filepath.Join(e.home, ".config", "dbc", "pipelines")
	disk := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "<" + err.Error() + ">"
		}
		return string(b)
	}

	// ── the browser's Pipelines section ───────────────────────────────────
	clickSel(t, p, "#scripts-btn", proto.InputMouseButtonLeft)
	waitFor(t, p, "the browser's pipeline examples", `() =>
	  document.querySelectorAll(".slist .srow.pexample").length >= 3 &&
	  [...document.querySelectorAll(".slist .snote")].some((n) => /copy an example/.test(n.textContent))`)

	// ── + New ▾ → Pipeline ────────────────────────────────────────────────
	eval(t, p, `() => document.querySelector(".modal .mfoot button.primary").click()`)
	waitFor(t, p, "the New menu", `() => !!document.querySelector(".menu")`)
	menuPick(t, p, "Pipeline — a source into a preview, on the canvas")
	waitFor(t, p, "the name prompt", `() => { const i = document.querySelector(".modal input.hfilter"); return !!i && i.value === "pipeline.json"; }`)
	eval(t, p, `() => { document.querySelector(".modal input.hfilter").value = "e2e_pipe"; document.querySelector(".modal button.primary").click(); }`)
	waitFor(t, p, "the pipeline tab, its canvas and lane", `() => {
	  const t = document.querySelector("#qtabs .qtab.pipeline.on .qt");
	  return !!t && t.textContent === "e2e_pipe.json" && document.querySelector(".app").classList.contains("pipe-mode") &&
	    !document.getElementById("pipe").hidden &&
	    document.querySelectorAll('.plane[data-frag="load"] .pcard').length === 2 &&
	    document.querySelectorAll('.plane[data-frag="load"] .ledges .edge').length === 1;
	}`)
	if d := disk("e2e_pipe.json"); !strings.Contains(d, `"name": "e2e_pipe"`) || !strings.Contains(d, `"conn": "lite"`) {
		t.Fatalf("the new pipeline on disk:\n%s", d)
	}

	// ── src's query, through the inspector ────────────────────────────────
	defineCodeEd(t, p)
	clickAt(t, p, `.plane[data-frag="load"] .pcard[data-id="src"] .cid`, proto.InputMouseButtonLeft)
	waitFor(t, p, "src in the inspector", `() => /sql\.read/.test(document.querySelector(".pinsp .ihead").textContent)`)
	// the query is a sql field: a small Monaco editor of its own (editor.js
	// mini), coloured as the node's connection's dialect (lite: SQLite,
	// generic sql)
	waitFor(t, p, "the query's editor", `() => {
	  const ed = codeEd("query");
	  return !!ed && ed.getModel().getLanguageId() === "sql";
	}`)
	eval(t, p, `() => codeEd("query").setValue("SELECT id, name FROM cats ORDER BY id")`)
	waitFor(t, p, "the card's summary and the unsaved mark", `() =>
	  /FROM cats/.test(document.querySelector('.pcard[data-id="src"] .csum').textContent) &&
	  !!document.querySelector("#qtabs .qtab.pipeline.on .qdirty")`)

	// ── a palette drag: rows.filter onto the lane ─────────────────────────
	dragTo(t, p, `.pitem[data-plugin="rows.filter"]`, `.plane[data-frag="load"] .lbody`, 0, 70)
	waitFor(t, p, "the filter node, selected", `() =>
	  !!document.querySelector('.pcard.sel[data-id="filter"]') && /rows\.filter/.test(document.querySelector(".pinsp .ihead").textContent)`)
	eval(t, p, `() => {
	  const ta = document.querySelector(".pinsp .ifield textarea");
	  ta.value = "id >= 2";
	  ta.dispatchEvent(new Event("input", { bubbles: true }));
	}`)
	// and a preview after it, dropped further right
	dragTo(t, p, `.pitem[data-plugin="preview"]`, `.plane[data-frag="load"] .lbody`, 220, 70)
	waitFor(t, p, "the second preview node", `() => !!document.querySelector('.pcard[data-id="preview"]')`)
	// dropped near a card, a new one takes the next free place: none covers another
	if n := evalNum(t, p, `() => {
	  const cs = [...document.querySelectorAll('.plane[data-frag="load"] .pcard')].map((c) => c.getBoundingClientRect());
	  let n = 0;
	  for (let i = 0; i < cs.length; i++) for (let j = i + 1; j < cs.length; j++) {
	    const a = cs[i], b = cs[j];
	    if (a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom) n++;
	  }
	  return n;
	}`); n != 0 {
		t.Fatalf("%v cards overlap", n)
	}

	shot(t, p, "pipeline-drop")

	// ── two wires, by hand ────────────────────────────────────────────────
	dragTo(t, p, `.pcard[data-id="src"] .pport.out`, `.pcard[data-id="filter"]`, 0, 0)
	dragTo(t, p, `.pcard[data-id="filter"] .pport.out`, `.pcard[data-id="preview"]`, 0, 0)
	waitFor(t, p, "three wires, and a clean check", `() =>
	  document.querySelectorAll('.plane[data-frag="load"] .ledges .edge').length === 3 &&
	  !document.querySelector(".pcard.bad, .pcard.warn") && !document.querySelector(".pbar .perr")`)

	// ── ◎ Preview: both branches into the grid ────────────────────────────
	seq := gridSeq(t, p)
	eval(t, p, `() => document.querySelector('.pbar button[data-act="preview"]').click()`)
	waitResult(t, p, seq, "id", "name")
	waitFor(t, p, "the switcher over the two previews", `() => {
	  const b = document.querySelectorAll("#rsets button");
	  return !document.getElementById("rsets").hidden && b.length === 2;
	}`)
	// the second (current) is the filtered branch's: 2 of the 3 cats
	waitFor(t, p, "the filtered rows", `() => /^2 rows/.test(document.getElementById("grid-info").textContent)`)
	shot(t, p, "pipeline-preview")

	// ── Ctrl+S ────────────────────────────────────────────────────────────
	eval(t, p, `() => document.querySelector("#pipe .pcanvas").focus()`)
	chord(t, p, modCtrl, "s", "KeyS", 83)
	waitFor(t, p, "saved: no unsaved mark", `() => !document.querySelector("#qtabs .qtab.pipeline.on .qdirty")`)
	d := disk("e2e_pipe.json")
	for _, want := range []string{`"plugin": "rows.filter"`, `"rules": "id >= 2"`, `["src", "filter"]`, `["filter", "preview"]`,
		`"query": "SELECT id, name FROM cats ORDER BY id"`} {
		if !strings.Contains(d, want) {
			t.Fatalf("saved file lacks %s:\n%s", want, d)
		}
	}

	// ── ▶ Run: the engine, the counters, the summary ──────────────────────
	clickSel(t, p, "#run", proto.InputMouseButtonLeft)
	waitFor(t, p, "the run's end on the canvas", `() =>
	  /✓/.test(document.querySelector('.plane[data-frag="load"] .lstate').textContent) &&
	  /3 → 2/.test(document.querySelector('.pcard[data-id="filter"] .cnum').textContent)`)
	shot(t, p, "pipeline-run")
	waitFor(t, p, "the summary in the pipeline's log", `() =>
	  [...document.querySelectorAll("#log > div")].some((l) => /pipeline e2e_pipe: 1 fragment, \d+ rows in .* \(succeeded\)/.test(l.textContent))`)

	// ── { } JSON and back ─────────────────────────────────────────────────
	eval(t, p, `() => document.querySelector('.pbar button[data-act="json"]').click()`)
	waitFor(t, p, "the JSON view", `() => document.querySelector(".app").classList.contains("pipe-json") &&
	  monaco.editor.getEditors()[0].getModel().getLanguageId() === "dbcjson" &&
	  monaco.editor.getEditors()[0].getValue().includes('"rows.filter"')`)
	shot(t, p, "pipeline-json")
	eval(t, p, `() => document.querySelector('.pbar button[data-act="json"]').click()`)
	waitFor(t, p, "the canvas back", `() => !document.querySelector(".app").classList.contains("pipe-json") &&
	  document.querySelectorAll(".pcard").length === 4`)

	// ── an unsaved edit survives a reload; the tab comes back on its canvas
	eval(t, p, `() => document.querySelector("#pipe .pcanvas").focus()`)
	p.Keyboard.MustType(input.Escape) // nothing selected: the pipeline's own form
	waitFor(t, p, "the pipeline's own form", `() => /pipeline/.test(document.querySelector(".pinsp .ihead").textContent)`)
	eval(t, p, `() => {
	  const ta = document.querySelector(".pinsp .ifield textarea");
	  ta.value = "two cats of three";
	  ta.dispatchEvent(new Event("input", { bubbles: true }));
	}`)
	waitFor(t, p, "unsaved", `() => !!document.querySelector("#qtabs .qtab.pipeline.on .qdirty")`)
	time.Sleep(400 * time.Millisecond) // the draft's write is debounced
	p.MustReload()
	waitFor(t, p, "the pipeline tab back, unsaved, its draft on the canvas", `() =>
	  !!document.querySelector("#qtabs .qtab.pipeline.on .qdirty") && document.querySelectorAll(".pcard").length === 4 &&
	  /two cats of three/.test(document.querySelector(".pbar .pmeta").textContent)`)

	// ── the JSON view edits the same pipeline: a waiting source, then ■ Stop
	eval(t, p, `() => document.querySelector('.pbar button[data-act="json"]').click()`)
	waitFor(t, p, "the JSON view", `() => document.querySelector(".app").classList.contains("pipe-json")`)
	eval(t, p, `(text) => monaco.editor.getEditors()[0].setValue(text)`, waitingPipeline)
	eval(t, p, `() => document.querySelector('.pbar button[data-act="json"]').click()`)
	waitFor(t, p, "the canvas drawing the edited JSON", `() => !document.querySelector(".app").classList.contains("pipe-json") &&
	  !!document.querySelector('.plane[data-frag="w"] .pcard[data-id="src"]')`)
	clickSel(t, p, "#run", proto.InputMouseButtonLeft)
	waitFor(t, p, "the tab busy, the lane running", `() =>
	  !!document.querySelector("#qtabs .qtab.pipeline.on .qbusy") && !document.getElementById("stop").disabled &&
	  /●/.test(document.querySelector('.plane[data-frag="w"] .lstate').textContent)`)
	clickSel(t, p, "#stop", proto.InputMouseButtonLeft)
	waitFor(t, p, "stopped", `() => !document.querySelector("#qtabs .qtab.pipeline.on .qbusy") &&
	  /■/.test(document.querySelector('.plane[data-frag="w"] .lstate').textContent) &&
	  /\(canceled\)/.test(document.getElementById("status").textContent)`)
	if d := disk("e2e_pipe.json"); !strings.Contains(d, `"go.source"`) {
		t.Fatalf("Run did not save the JSON view's edit first:\n%s", d)
	}

	pipelineCodeField(t, p, disk)

	// ── ⇪ Go: the same pipeline as a script, in a script tab ──────────────
	eval(t, p, `() => document.querySelector('.pbar button[data-act="export"]').click()`)
	waitFor(t, p, "the script's name prompt", `() => { const i = document.querySelector(".modal input.hfilter"); return !!i && i.value === "e2e_pipe.go"; }`)
	eval(t, p, `() => document.querySelector(".modal button.primary").click()`)
	waitFor(t, p, "the exported script in a script tab", `() => {
	  const t = document.querySelector("#qtabs .qtab.script.on .qt");
	  return !!t && t.textContent === "e2e_pipe.go" && monaco.editor.getEditors()[0].getValue().includes("sdb.NewPipeline(");
	}`)
}

// pipelineCodeField drives a go field's editor in the inspector (N-175),
// on the waiting pipeline's go.source, which is on the canvas by now:
//
//	select the card     ─► the code in a Monaco editor of its own, as Go
//	e. typed            ─► sdb.Env's fields and methods offered: e is
//	                       Next's parameter (scripts.js varType)
//	an undefined name   ─► the check's mark on that line of the field, with
//	                       its note — the same editor, kept under the caret
//	Ctrl+X, nothing     ─► not the workbench's explain (the mini's
//	selected               dbcScript), Ctrl+S ─► the pipeline saved, as
//	                       from the canvas (the chords are page-wide)
//	another card        ─► the editor disposed with the selection
func pipelineCodeField(t *testing.T, p *rod.Page, disk func(string) string) {
	defineCodeEd(t, p)
	// the varType forms the providers read, a parameter among them
	if got := evalStr(t, p, `() => [
	  dbc.scripts.varType({}, "func Apply(b *sdb.Batch) (*sdb.Batch, error) {", "b", "s"),
	  dbc.scripts.varType({}, "func Apply(e *sdb.Env, b *sdb.Batch) (*sdb.Batch, error) {", "e", "s"),
	  dbc.scripts.varType({}, "func Apply(e *sdb.Env, b *sdb.Batch) (*sdb.Batch, error) {", "b", "s"),
	  dbc.scripts.varType({}, "o := sdb.CopyOpts{}", "o", "s"),
	  dbc.scripts.varType({}, "func f(xb *sdb.Batch)", "b", "s"),
	].join("|")`); got != "Batch|Env|Batch|CopyOpts|" {
		t.Errorf("varType = %q", got)
	}

	base := evalNum(t, p, `() => monaco.editor.getEditors().length`)
	clickAt(t, p, `.plane[data-frag="w"] .pcard[data-id="src"] .cid`, proto.InputMouseButtonLeft)
	waitFor(t, p, "the go field's editor", `() => {
	  const ed = codeEd("code");
	  return !!ed && ed.getModel().getLanguageId() === "go" && /func Next\(e \*sdb\.Env\)/.test(ed.getValue()) &&
	    !document.querySelector('.pinsp .ifield textarea.code');
	}`)
	if n := evalNum(t, p, `() => monaco.editor.getEditors().length`); n != base+1 {
		t.Fatalf("%v editors with src selected, want %v", n, base+1)
	}

	// completion on a parameter: a new first line in Next's body, e. typed
	// as keys (the "." is the provider's trigger)
	eval(t, p, `() => {
	  const ed = codeEd("code");
	  ed.executeEdits("e2e", [{ range: new monaco.Range(2, 1, 2, 1), text: "\t\n" }]);
	  ed.setPosition({ lineNumber: 2, column: 2 });
	  ed.focus();
	}`)
	p.Keyboard.MustType(input.KeyE, input.Period)
	// the widget draws only the rows in view: two of Env's fields that sort
	// near the top
	waitFor(t, p, "Env's members offered", `() => {
	  const rows = [...document.querySelectorAll(".pinsp .suggest-widget.visible .monaco-list-row")]
	    .map((r) => r.getAttribute("aria-label") || r.textContent);
	  return rows.some((r) => /^Ctx/.test(r)) && rows.some((r) => /^Params/.test(r));
	}`)
	p.Keyboard.MustType(input.Escape)

	// a compile error: the mark lands on the field's own line 2, and the
	// editor is the same one (a check redraws only the list under a caret)
	eval(t, p, `() => {
	  const ed = codeEd("code");
	  ed.__e2e = "kept";
	  ed.executeEdits("e2e", [{ range: ed.getModel().getFullModelRange(),
	    text: "func Next(e *sdb.Env) (*sdb.Batch, error) {\n\treturn nil, nope\n}\n" }]);
	}`)
	waitFor(t, p, "the check's mark on line 2", `() => {
	  const ed = codeEd("code");
	  if (!ed || ed.__e2e !== "kept" || !ed.hasTextFocus()) return false;
	  const ms = monaco.editor.getModelMarkers({ resource: ed.getModel().uri, owner: "dbc" });
	  return ms.length === 1 && ms[0].startLineNumber === 2 && /undefined: nope/.test(ms[0].message) &&
	    [...ed.getContainerDomNode().querySelectorAll(".diag-note")].some((n) => /undefined:\snope/.test(n.textContent)) &&
	    /undefined: nope/.test(document.querySelector(".pinsp .idiags").textContent);
	}`)
	shot(t, p, "pipeline-code-field")

	// Ctrl+X with nothing selected is not the SQL tab's explain here
	eval(t, p, `() => { window.__explained = 0; window.__explain = dbc.cmd.explain; dbc.cmd.explain = () => { window.__explained++; }; }`)
	chord(t, p, modCtrl, "x", "KeyX", 88)
	chord(t, p, modMeta, "x", "KeyX", 88)
	n := evalNum(t, p, `() => { dbc.cmd.explain = window.__explain; return window.__explained; }`)
	if n != 0 {
		t.Errorf("Ctrl+X in a code field explained %v times", n)
	}

	// back to the waiting source, then Ctrl+S from inside the editor
	eval(t, p, `(text) => { const ed = codeEd("code"); ed.executeEdits("e2e", [{ range: ed.getModel().getFullModelRange(), text }]); }`,
		"func Next(e *sdb.Env) (*sdb.Batch, error) {\n\t<-e.Ctx.Done()\n\treturn nil, e.Ctx.Err()\n}\n")
	waitFor(t, p, "unsaved, the mark gone", `() => !!document.querySelector("#qtabs .qtab.pipeline.on .qdirty") &&
	  monaco.editor.getModelMarkers({ resource: codeEd("code").getModel().uri, owner: "dbc" }).length === 0`)
	chord(t, p, modCtrl, "s", "KeyS", 83)
	waitFor(t, p, "saved from the code field", `() => !document.querySelector("#qtabs .qtab.pipeline.on .qdirty")`)
	if d := disk("e2e_pipe.json"); !strings.Contains(d, `\t<-e.Ctx.Done()`) || strings.Contains(d, "nope") {
		t.Fatalf("saved file after the code field's edits:\n%s", d)
	}

	// another card: the editor goes with the selection
	clickAt(t, p, `.plane[data-frag="w"] .pcard[data-id="show"] .cid`, proto.InputMouseButtonLeft)
	waitFor(t, p, "show in the inspector, src's editor disposed", `() =>
	  /preview/.test(document.querySelector(".pinsp .ihead").textContent) && !codeEd("code") &&
	  monaco.editor.getEditors().length === `+strconv.Itoa(int(base)))
}

// defineCodeEd puts codeEd(name) on the page: the Monaco editor of the
// inspector's code field name, or null. Again after a reload.
func defineCodeEd(t *testing.T, p *rod.Page) {
	t.Helper()
	eval(t, p, `() => { window.codeEd = (name) => monaco.editor.getEditors().find((ed) =>
	  !!ed.getContainerDomNode().closest('.pinsp .ifield[data-field="' + name + '"]')) || null; }`)
}

// waitingPipeline is typed into the JSON view: a Go source that waits until
// the run is stopped, into a preview — a run that runs until ■ Stop.
const waitingPipeline = `{
  "name": "e2e_pipe",
  "fragments": [
    {
      "name": "w",
      "nodes": [
        {"id": "src", "plugin": "go.source", "cfg": {"code": "func Next(e *sdb.Env) (*sdb.Batch, error) {\n\t<-e.Ctx.Done()\n\treturn nil, e.Ctx.Err()\n}\n"}},
        {"id": "show", "plugin": "preview", "cfg": {}}
      ],
      "edges": [["src", "show"]]
    }
  ]
}
`

// shot writes the page as name.png into DBC_E2E_SHOTS, when it is set:
// what a canvas looks like is not something a predicate can say.
func shot(t *testing.T, p *rod.Page, name string) {
	t.Helper()
	dir := os.Getenv("DBC_E2E_SHOTS")
	if dir == "" {
		return
	}
	b, err := p.Screenshot(false, nil)
	if err != nil {
		t.Logf("screenshot %s: %v", name, err)
		return
	}
	if err = os.WriteFile(filepath.Join(dir, name+".png"), b, 0o644); err != nil {
		t.Logf("screenshot %s: %v", name, err)
	}
}

// dragTo drags the element from matches to dx, dy from the top left of the
// one to matches (its center when both are 0), moving in steps as a hand
// would, with a pause after: what the canvas's pointer handlers see from a
// real mouse.
func dragTo(t *testing.T, p *rod.Page, from, to string, dx, dy float64) {
	t.Helper()
	waitFor(t, p, from+" and "+to+" drawn", `(a, b) => !!document.querySelector(a) && !!document.querySelector(b)`, from, to)
	pts := eval(t, p, `(a, b, dx, dy) => {
	  document.querySelector(a).scrollIntoView({ block: "nearest" }); // a palette entry below the fold
	  const r = document.querySelector(a).getBoundingClientRect(), s = document.querySelector(b).getBoundingClientRect();
	  const tx = dx || dy ? s.left + dx + 90 : s.left + s.width / 2, ty = dx || dy ? s.top + dy : s.top + s.height / 2;
	  return [r.left + r.width / 2, r.top + r.height / 2, tx, ty];
	}`, from, to, dx, dy).([]any)
	x, y, x2, y2 := pts[0].(float64), pts[1].(float64), pts[2].(float64), pts[3].(float64)
	p.Mouse.MustMoveTo(x, y)
	p.Mouse.MustDown(proto.InputMouseButtonLeft)
	if err := p.Mouse.MoveLinear(proto.Point{X: x2, Y: y2}, 10); err != nil {
		t.Fatal(err)
	}
	p.Mouse.MustUp(proto.InputMouseButtonLeft)
	waitFor(t, p, "the drop settled", `() => !document.querySelector(".pghost")`)
}
