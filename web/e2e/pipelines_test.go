package e2e

import (
	"os"
	"path/filepath"
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
	clickAt(t, p, `.plane[data-frag="load"] .pcard[data-id="src"] .cid`, proto.InputMouseButtonLeft)
	waitFor(t, p, "src in the inspector", `() => /sql\.read/.test(document.querySelector(".pinsp .ihead").textContent)`)
	eval(t, p, `() => {
	  const q = [...document.querySelectorAll(".pinsp .ifield")].find((f) => f.querySelector(".iname").textContent.startsWith("query"));
	  const ta = q.querySelector("textarea");
	  ta.value = "SELECT id, name FROM cats ORDER BY id";
	  ta.dispatchEvent(new Event("input", { bubbles: true }));
	}`)
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

	// ── ⇪ Go: the same pipeline as a script, in a script tab ──────────────
	eval(t, p, `() => document.querySelector('.pbar button[data-act="export"]').click()`)
	waitFor(t, p, "the script's name prompt", `() => { const i = document.querySelector(".modal input.hfilter"); return !!i && i.value === "e2e_pipe.go"; }`)
	eval(t, p, `() => document.querySelector(".modal button.primary").click()`)
	waitFor(t, p, "the exported script in a script tab", `() => {
	  const t = document.querySelector("#qtabs .qtab.script.on .qt");
	  return !!t && t.textContent === "e2e_pipe.go" && monaco.editor.getEditors()[0].getValue().includes("sdb.NewPipeline(");
	}`)
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
