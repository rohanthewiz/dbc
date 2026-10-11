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

// jobRuns: a job started from the page (the jobs API; the jobs tab is still
// to come) is said in the log on screen when it starts and when it ends,
// and a pipeline tab of the same name is not taken for it.
func jobRuns(t *testing.T, e *env, p *rod.Page) {
	cfg := filepath.Join(e.home, ".config", "dbc")
	for dir, files := range map[string]map[string]string{
		"pipelines": {"e2e_step.json": `{"name": "e2e_step", "fragments": [{"name": "f", "nodes": [
		  {"id": "x", "plugin": "sql.exec", "cfg": {"conn": "lite", "sql": "SELECT 1"}}]}]}`},
		"jobs": {"e2e_job.json": `{"name": "e2e_job", "pipelines": [{"id": "one", "pipeline": "e2e_step"},
		  {"id": "two", "pipeline": "e2e_step", "after": ["one"]}]}`},
	} {
		if err := os.MkdirAll(filepath.Join(cfg, dir), 0o700); err != nil {
			t.Fatal(err)
		}
		for name, text := range files {
			if err := os.WriteFile(filepath.Join(cfg, dir, name), []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	eval(t, p, `() => { dbc.api("POST", "/api/v1/jobs/e2e_job.json/run", {}); }`)
	waitFor(t, p, "the job's start and end in the log", `() => {
	  const t = document.getElementById("log").textContent;
	  return /▶ job e2e_job started \(manual\)/.test(t) && /job e2e_job succeeded in/.test(t);
	}`)
	// a pipeline tab may be open from the step before; none is the job's
	if named, _ := eval(t, p, `() => [...document.querySelectorAll('#qtabs .qtab.pipeline .qt')].some((q) => q.textContent === "e2e_job.json")`).(bool); named {
		t.Error("the job's run opened or named a pipeline tab")
	}
}

// jobTabs is Phase 4 of ai_docs/plans/pipelines.md, end to end — the jobs
// tab and the Runs view:
//
//	Ctrl+O                 ─► the Jobs section: none yet, the nightly example
//	⧉ Copy the example     ─► a named job file, a job tab: the DAG of its four
//	                          steps as the server laid it out — copy left,
//	                          clean and breeds stacked, report right
//	the inspector          ─► the job's cron line and its next fires
//	a palette drag         ─► a pipeline dropped on report: a fifth step, after it
//	Delete                 ─► the step gone again
//	a wire, by hand        ─► report now also waits for copy
//	Ctrl+S                 ─► on disk, in the shape Spec.JSON writes
//	▶ Run                  ─► the tab turns to its runs, on the new run's page:
//	                          it reaches succeeded, every card ✓, a critical path
//	a step's card          ─► its fragments on the run's time axis
//	a fragment's row       ─► its nodes' counters (rows in and out)
//	‹ Runs                 ─► the job's list: the run, ✓
//	Alt+R                  ─► every run, in a dialog; Enter opens one
//	⊞ Design               ─► the cards show the run's states
func jobTabs(t *testing.T, e *env, p *rod.Page) {
	dir := filepath.Join(e.home, ".config", "dbc", "jobs")
	disk := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "<" + err.Error() + ">"
		}
		return string(b)
	}

	// The example's pipelines run on the built-in demos, which a config
	// file replaces (writeConfig): pipelines of the same names on lite,
	// in pipelines_dir, are found first (jobs.PipelineFinder) — so the
	// copied example runs here as written. cats-report ends in a preview
	// sink, whose rows a real run lands in the starting tab's grid.
	pdir := filepath.Join(e.home, ".config", "dbc", "pipelines")
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		t.Fatal(err)
	}
	read := func(id, sql string) string {
		return `{"id": "` + id + `", "plugin": "sql.read", "cfg": {"conn": "lite", "query": "` + sql + `"}}`
	}
	write := func(id, table string) string {
		return `{"id": "` + id + `", "plugin": "sql.write", "cfg": {"conn": "lite", "table": "` + table + `", "create": "true", "truncate": "true"}}`
	}
	for name, text := range map[string]string{
		"copy-cats.json": `{"name": "copy-cats", "fragments": [{"name": "copy", "nodes": [` + read("src", "SELECT id, name, breed, age FROM cats") +
			`, ` + write("dst", "e2e_cats_copy") + `], "edges": [["src", "dst"]]}]}`,
		"clean-and-load.json": `{"name": "clean-and-load", "params": {"min_age": {"default": "0"}}, "fragments": [{"name": "load", "nodes": [` +
			read("src", "SELECT id, name, age FROM e2e_cats_copy WHERE age >= ${min_age}") + `, ` + write("dst", "e2e_cats_clean") +
			`], "edges": [["src", "dst"]]}]}`,
		"breed-counts.json": `{"name": "breed-counts", "fragments": [{"name": "count", "nodes": [` +
			read("src", "SELECT breed, count(*) AS n FROM e2e_cats_copy GROUP BY breed") + `, ` + write("dst", "e2e_breed_counts") +
			`], "edges": [["src", "dst"]]}]}`,
		// a step that runs until it is stopped, for a live run page
		"e2e_slow.json": `{"name": "e2e_slow", "fragments": [{"name": "f", "nodes": [{"id": "z", "plugin": "go.action",
		  "cfg": {"code": "func Run(s *sdb.S) error {\n\t<-s.Ctx().Done()\n\treturn s.Ctx().Err()\n}\n"}}]}]}`,
		"cats-report.json": `{"name": "cats-report", "fragments": [{"name": "report", "nodes": [` +
			read("src", "SELECT breed, n FROM e2e_breed_counts ORDER BY breed") + `, {"id": "peek", "plugin": "preview", "cfg": {"rows": "50"}}` +
			`], "edges": [["src", "peek"]]}]}`,
	} {
		if err := os.WriteFile(filepath.Join(pdir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// ── the browser's Jobs section, and the example copied ───────────────
	clickSel(t, p, "#scripts-btn", proto.InputMouseButtonLeft)
	waitFor(t, p, "the browser's job example", `() =>
	  [...document.querySelectorAll(".slist .srow.jexample")].some((r) => r.dataset.name === "nightly.json")`)
	eval(t, p, `() => [...document.querySelectorAll(".slist .srow.jexample")].find((r) => r.dataset.name === "nightly.json")
	  .querySelector(".sact button").click()`)
	waitFor(t, p, "the name prompt", `() => { const i = document.querySelector(".modal input.hfilter"); return !!i && i.value === "nightly.json"; }`)
	eval(t, p, `() => { document.querySelector(".modal input.hfilter").value = "e2e_nightly"; document.querySelector(".modal button.primary").click(); }`)
	waitFor(t, p, "the job tab and its DAG", `() => {
	  const t = document.querySelector("#qtabs .qtab.job.on .qt");
	  return !!t && t.textContent === "e2e_nightly.json" && document.querySelector(".app").classList.contains("job-mode") &&
	    !document.getElementById("jobp").hidden && document.getElementById("pipe").hidden &&
	    document.querySelectorAll("#jobp .jcard").length === 4 && document.querySelectorAll("#jobp .dedges .edge").length === 4;
	}`)
	if d := disk("e2e_nightly.json"); !strings.Contains(d, `"name": "e2e_nightly"`) || !strings.Contains(d, `"pipeline": "clean-and-load"`) {
		t.Fatalf("the copied job on disk:\n%s", d)
	}
	// the server's layout: one column per rank
	pos := func(id string) (float64, float64) {
		xy := eval(t, p, `(id) => { const c = document.querySelector('#jobp .jcard[data-step="' + id + '"]');
		  return [parseFloat(c.style.left), parseFloat(c.style.top)]; }`, id).([]any)
		return xy[0].(float64), xy[1].(float64)
	}
	cx, _ := pos("copy")
	lx, ly := pos("clean")
	bx, by := pos("breeds")
	rx, _ := pos("report")
	if !(cx < lx && lx == bx && ly != by && bx < rx) {
		t.Fatalf("not laid out by rank: copy %v, clean %v,%v, breeds %v,%v, report %v", cx, lx, ly, bx, by, rx)
	}
	if root := evalStr(t, p, `() => document.querySelector('#jobp .jcard[data-step="copy"] .croot') ? "yes" : ""`); root == "" {
		t.Error("the root has no ◉")
	}
	// the job's inspector: its cron line, with when it fires next
	waitFor(t, p, "the cron line's next fires", `() => {
	  const c = document.querySelector("#jobp .pinsp input.jcron"), f = document.querySelector('#jobp .pinsp [data-fires="0"]');
	  return !!c && c.value === "0 2 * * *" && !!f && /^next: /.test(f.textContent) && (f.textContent.match(/02:00/g) || []).length === 5;
	}`)
	shot(t, p, "job-tab")
	jobDottedIDs(t, p)
	jobUnknownKeys(t, p)

	// ── a palette drag onto a card, then Delete ──────────────────────────
	dragTo(t, p, `#jobp .pitem[data-pipe="cats-report"]`, `#jobp .jcard[data-step="report"]`, 0, 0)
	waitFor(t, p, "a fifth step after report, laid out right of it", `() => {
	  const c = document.querySelector('#jobp .jcard.sel[data-step="cats-report"]'), r = document.querySelector('#jobp .jcard[data-step="report"]');
	  return !!c && !!r && parseFloat(c.style.left) > parseFloat(r.style.left) && document.querySelectorAll("#jobp .dedges .edge").length === 5;
	}`)
	eval(t, p, `() => document.querySelector("#jobp .pcanvas").focus()`)
	chord(t, p, 0, "Delete", "Delete", 46)
	// (back to the file's very text: the tab is not even unsaved now)
	waitFor(t, p, "the step deleted", `() => document.querySelectorAll("#jobp .jcard").length === 4 &&
	  !document.querySelector("#qtabs .qtab.job.on .qdirty")`)

	// ── a wire, by hand: report also waits for copy ──────────────────────
	dragTo(t, p, `#jobp .jcard[data-step="copy"] .pport.out`, `#jobp .jcard[data-step="report"]`, 0, 0)
	waitFor(t, p, "five edges, a clean check", `() => document.querySelectorAll("#jobp .dedges .edge").length === 5 &&
	  !!document.querySelector("#qtabs .qtab.job.on .qdirty") && !document.querySelector("#jobp .pbar .perr")`)

	// ── Ctrl+S ───────────────────────────────────────────────────────────
	eval(t, p, `() => document.querySelector("#jobp .pcanvas").focus()`)
	chord(t, p, modCtrl, "s", "KeyS", 83)
	waitFor(t, p, "saved", `() => !document.querySelector("#qtabs .qtab.job.on .qdirty")`)
	if d := disk("e2e_nightly.json"); !strings.Contains(d, `"after": ["clean", "breeds", "copy"]`) || strings.Contains(d, "cats-report2") {
		t.Fatalf("saved job:\n%s", d)
	}

	// ── ▶ Run: the tab's runs face, the run's page, to succeeded ─────────
	clickSel(t, p, "#run", proto.InputMouseButtonLeft)
	waitFor(t, p, "the run page", `() => !document.querySelector("#jobp .jruns").hidden && !!document.querySelector("#jobp .rvhead")`)
	waitFor(t, p, "the run succeeded, every step ✓", `() => {
	  const h = document.querySelector("#jobp .rvhead .rvst");
	  return !!h && /succeeded/.test(h.textContent) && document.querySelectorAll("#jobp .rvdag .dcard.s-succeeded").length === 4;
	}`)
	if n := evalNum(t, p, `() => document.querySelectorAll("#jobp .rvdag .dcard.crit").length`); n < 2 {
		t.Errorf("critical path: %v steps", n)
	}
	shot(t, p, "job-run-page")

	// ── a step, then a fragment: the counters ────────────────────────────
	clickSel(t, p, `#jobp .rvdag .dcard[data-step="clean"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "clean's fragments on the time axis", `() =>
	  document.querySelectorAll("#jobp .rvpipe .trow").length >= 1 && !!document.querySelector("#jobp .rvpipe .tbar.s-succeeded")`)
	clickSel(t, p, "#jobp .rvpipe .trow", proto.InputMouseButtonLeft)
	waitFor(t, p, "the fragment's nodes, with rows in and out", `() => {
	  const rows = [...document.querySelectorAll("#jobp .rvfrag .ntable tbody tr")];
	  return rows.length >= 2 && rows.some((r) => +r.children[3].textContent.replace(/,/g, "") > 0);
	}`)
	waitFor(t, p, "the log, narrowed to the fragment", `() => /fragment /.test(document.querySelector("#jobp .rvlog").textContent) &&
	  /log · clean\//.test(document.querySelector("#jobp .rvlogs .rvsub").textContent)`)
	shot(t, p, "job-run-fragment")

	// ── back to the job's list ───────────────────────────────────────────
	clickSel(t, p, "#jobp .rvback", proto.InputMouseButtonLeft)
	waitFor(t, p, "the job's runs, listed", `() => {
	  const r = document.querySelectorAll("#jobp .rvrow");
	  return r.length === 1 && /✓/.test(r[0].querySelector(".rvst").textContent) && r[0].querySelector(".rvname").textContent === "e2e_nightly";
	}`)

	// ── Alt+R: every run, in a dialog ────────────────────────────────────
	chord(t, p, modAlt, "r", "KeyR", 82)
	waitFor(t, p, "the Runs dialog, listing the run", `() => !!document.querySelector(".modal.runs") &&
	  [...document.querySelectorAll(".modal.runs .rvrow .rvname")].some((n) => n.textContent === "e2e_nightly")`)
	shot(t, p, "runs-dialog")
	eval(t, p, `() => { const rows = [...document.querySelectorAll(".modal.runs .rvrow")];
	  rows.find((r) => r.querySelector(".rvname").textContent === "e2e_nightly").click(); }`)
	waitFor(t, p, "its page in the dialog", `() => document.querySelectorAll(".modal.runs .rvdag .dcard").length === 4`)
	chord(t, p, 0, "Escape", "Escape", 27)
	waitFor(t, p, "the dialog closed", `() => !document.querySelector(".modal.runs")`)

	// ── ⊞ Design: the cards carry the run's states ───────────────────────
	clickSel(t, p, `#jobp .pbar button[data-act="design"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "the canvas, every card ✓", `() => !document.querySelector("#jobp .pbody").hidden &&
	  document.querySelectorAll("#jobp .jcard.s-succeeded").length === 4`)
	shot(t, p, "job-tab-after-run")

	// ── a reload: the job tab comes back, its cards on the last run ──────
	// and a closed window's unsaved draft of the job — an orphan, its owner
	// never beat — is put back by it, moved under this window's key, as a
	// script's is (drafts.js, N-177)
	saved := disk("e2e_nightly.json")
	orphan := strings.Replace(saved, `"name": "e2e_nightly",`, `"name": "e2e_nightly",`+"\n"+`  "desc": "a closed window's draft",`, 1)
	if orphan == saved {
		t.Fatalf("no name line to put a desc after:\n%s", saved)
	}
	eval(t, p, `(text) => localStorage.setItem("dbc.job.draft.e2egone:e2e_nightly.json", JSON.stringify({ base: "", text, at: Date.now() }))`, orphan)
	p.MustReload()
	waitFor(t, p, "the job tab after a reload, its cards ✓, the orphan adopted", `() => {
	  const t = document.querySelector("#qtabs .qtab.job.on .qt"), me = sessionStorage.getItem("dbc.draftOwner");
	  return !!t && t.textContent === "e2e_nightly.json" && document.querySelectorAll("#jobp .jcard.s-succeeded").length === 4 &&
	    !!document.querySelector("#qtabs .qtab.job.on .qdirty") &&
	    localStorage.getItem("dbc.job.draft.e2egone:e2e_nightly.json") === null &&
	    /a closed window's draft/.test(localStorage.getItem("dbc.job.draft." + me + ":e2e_nightly.json") || "");
	}`)
	// the saved text back through the JSON view: nothing unsaved, no draft
	eval(t, p, `() => document.querySelector('#jobp .pbar button[data-act="json"]').click()`)
	waitFor(t, p, "the job's JSON view", `() => document.querySelector(".app").classList.contains("pipe-json")`)
	eval(t, p, `(text) => monaco.editor.getEditors()[0].setValue(text)`, saved)
	eval(t, p, `() => document.querySelector('#jobp .pbar button[data-act="json"]').click()`)
	waitFor(t, p, "the job as saved, its draft dropped", `() => { const me = sessionStorage.getItem("dbc.draftOwner");
	  return !document.querySelector(".app").classList.contains("pipe-json") && !document.querySelector("#qtabs .qtab.job.on .qdirty") &&
	    localStorage.getItem("dbc.job.draft." + me + ":e2e_nightly.json") === null; }`)

	// ── + New ▾ → Job, one step dropped in, a live run stopped ───────────
	clickSel(t, p, "#scripts-btn", proto.InputMouseButtonLeft)
	waitFor(t, p, "the browser, the job listed", `() => [...document.querySelectorAll(".slist .srow.job")].some((r) => r.dataset.name === "e2e_nightly.json")`)
	eval(t, p, `() => document.querySelector(".modal .mfoot button.primary").click()`)
	waitFor(t, p, "the New menu", `() => !!document.querySelector(".menu")`)
	menuPick(t, p, "Job — pipelines in a DAG, run on a schedule or by hand")
	waitFor(t, p, "the name prompt", `() => { const i = document.querySelector(".modal input.hfilter"); return !!i && i.value === "job.json"; }`)
	eval(t, p, `() => { document.querySelector(".modal input.hfilter").value = "e2e_slow_job"; document.querySelector(".modal button.primary").click(); }`)
	waitFor(t, p, "an empty job tab, saying what to drag", `() => {
	  const t = document.querySelector("#qtabs .qtab.job.on .qt");
	  return !!t && t.textContent === "e2e_slow_job.json" && !!document.querySelector("#jobp .jhint");
	}`)
	dragTo(t, p, `#jobp .pitem[data-pipe="e2e_slow"]`, `#jobp .pcanvas`, 0, 0)
	waitFor(t, p, "the root step, and a clean check", `() =>
	  !!document.querySelector('#jobp .jcard[data-step="e2e_slow"] .croot') && !document.querySelector("#jobp .pbar .perr")`)
	eval(t, p, `() => document.querySelector("#jobp .pcanvas").focus()`)
	chord(t, p, modCtrl, "Enter", "Enter", 13)
	waitFor(t, p, "the live run page: running, the card ●, the tab busy", `() => {
	  const h = document.querySelector("#jobp .rvhead .rvst");
	  return !!h && /running/.test(h.textContent) && !!document.querySelector("#jobp .rvdag .dcard.s-running") &&
	    !!document.querySelector("#qtabs .qtab.job.on .qbusy");
	}`)
	if d := disk("e2e_slow_job.json"); !strings.Contains(d, `"pipeline": "e2e_slow"`) {
		t.Fatalf("Ctrl+Enter did not save first:\n%s", d)
	}
	chord(t, p, modAlt, "r", "KeyR", 82)
	waitFor(t, p, "the dialog: one running, its row live", `() => /1 running/.test(document.querySelector(".modal.runs .rvbar").textContent) &&
	  [...document.querySelectorAll(".modal.runs .rvrow")].some((r) => /●/.test(r.querySelector(".rvst").textContent))`)
	shot(t, p, "runs-dialog-live")
	chord(t, p, 0, "Escape", "Escape", 27)
	clickSel(t, p, `#jobp .rvacts button[data-act="stop"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "stopped: canceled, the tab no longer busy", `() => {
	  const h = document.querySelector("#jobp .rvhead .rvst");
	  return !!h && /canceled/.test(h.textContent) && !document.querySelector("#qtabs .qtab.job.on .qbusy");
	}`)
}

// dottedIDsJob has a step whose id holds a dot, beside the step its id
// starts with, and a step whose id is invalid (N-198):
//
//	copy.x.after   no step called "nope"    → copy.x, not copy
//	pipelines[2]   not a step id; no zz     → "a b", by its place
const dottedIDsJob = `{
  "name": "e2e_nightly",
  "pipelines": [
    { "id": "copy", "pipeline": "copy-cats" },
    { "id": "copy.x", "pipeline": "breed-counts", "after": ["copy", "nope"] },
    { "id": "a b", "pipeline": "zz", "after": ["copy"] }
  ]
}
`

// jobDottedIDs puts dottedIDsJob in the open job tab through its JSON view
// and checks that each finding reaches its own card, the inspector's list
// (labelled by what follows the step) and its line in the JSON view; then
// puts the tab's text back.
func jobDottedIDs(t *testing.T, p *rod.Page) {
	toggleJSON := func() {
		eval(t, p, `() => document.querySelector('#jobp .pbar button[data-act="json"]').click()`)
	}
	toggleJSON()
	waitFor(t, p, "the job's JSON view", `() => document.querySelector(".app").classList.contains("pipe-json")`)
	eval(t, p, `(text) => { const ed = monaco.editor.getEditors()[0]; window.__keptJob = ed.getValue(); ed.setValue(text); }`, dottedIDsJob)
	toggleJSON()
	waitFor(t, p, "copy.x's ⚠ and a b's, none on copy", `() => {
	  const card = (id) => document.querySelector('#jobp .jcard[data-step="' + id + '"]');
	  const n = (id) => ((card(id) || {}).querySelector && card(id).querySelector(".cd") || {}).textContent || "";
	  return !document.querySelector(".app").classList.contains("pipe-json") && !!card("copy") && !!card("copy.x") && !!card("a b") &&
	    n("copy") === "" && !card("copy").classList.contains("bad") &&
	    n("copy.x") === "⚠1" && card("copy.x").classList.contains("bad") && n("a b") === "⚠2";
	}`)
	clickAt(t, p, `#jobp .jcard[data-step="copy.x"] .cid`, proto.InputMouseButtonLeft)
	waitFor(t, p, "copy.x's finding in the inspector, labelled after", `() => {
	  const ds = [...document.querySelectorAll("#jobp .pinsp [data-diags] .idiag")].map((d) => d.textContent);
	  return ds.length === 1 && /^✗ after: no step called "nope"/.test(ds[0]);
	}`)
	clickAt(t, p, `#jobp .jcard[data-step="a b"] .cid`, proto.InputMouseButtonLeft)
	waitFor(t, p, "a b's two findings in the inspector", `() => {
	  const ds = [...document.querySelectorAll("#jobp .pinsp [data-diags] .idiag")].map((d) => d.textContent);
	  return ds.length === 2 && ds.some((d) => /^✗ not a step id/.test(d)) && ds.some((d) => /^✗ pipeline zz/.test(d));
	}`)
	// the JSON view's marks, by the where each message starts with: the
	// line each step is written on (5, 6), the after's on copy.x's own
	got := evalStr(t, p, `() => monaco.editor.getModelMarkers({ resource: monaco.editor.getEditors()[0].getModel().uri, owner: "dbc" })
	  .map((m) => m.message.split(":")[0] + "@" + m.startLineNumber).sort().join(" ")`)
	if want := "copy.x.after@5 pipelines[2]@6 pipelines[2]@6"; got != want {
		t.Errorf("the job JSON view's marks = %q, want %q", got, want)
	}
	shot(t, p, "job-dotted-ids")

	// the tab as it was: the same text is no unsaved change
	toggleJSON()
	waitFor(t, p, "the job's JSON view", `() => document.querySelector(".app").classList.contains("pipe-json")`)
	eval(t, p, `() => monaco.editor.getEditors()[0].setValue(window.__keptJob)`)
	toggleJSON()
	waitFor(t, p, "the job back, nothing unsaved", `() =>
	  !document.querySelector(".app").classList.contains("pipe-json") &&
	  document.querySelectorAll("#jobp .jcard").length === 4 &&
	  !document.querySelector("#qtabs .qtab.job.on .qdirty")`)
}

// jobUnknownKeys (N-180): a key the job has no field for survives a canvas
// edit (jobText) at every level it rewrites — the job, a param, a step,
// the triggers, the policy — and the check goes on naming it.
func jobUnknownKeys(t *testing.T, p *rod.Page) {
	toggle := func() { eval(t, p, `() => document.querySelector('#jobp .pbar button[data-act="json"]').click()`) }
	toggle()
	waitFor(t, p, "the job's JSON view", `() => document.querySelector(".app").classList.contains("pipe-json")`)
	eval(t, p, `() => {
	  const ed = monaco.editor.getEditors()[0];
	  window.__keptJobUK = ed.getValue();
	  const s = JSON.parse(window.__keptJobUK);
	  s.colour = "red";
	  s.params = Object.assign(s.params || {}, { day: { default: "", hint: "x" } });
	  s.pipelines[0].retries = 3;
	  s.triggers = Object.assign(s.triggers || {}, { shedule: ["0 3 * * *"] });
	  s.policy = Object.assign(s.policy || {}, { on_fail: "stop" });
	  ed.setValue(JSON.stringify(s, null, 2));
	}`)
	toggle()
	waitFor(t, p, "the canvas, the check naming the unknown key", `() => !document.querySelector(".app").classList.contains("pipe-json") &&
	  monaco.editor.getModelMarkers({ owner: "dbc" }).some((m) => /unknown field "/.test(m.message))`)
	// a canvas edit: the job's description, in its own form
	eval(t, p, `() => document.querySelector("#jobp .pcanvas").focus()`)
	p.Keyboard.MustType(input.Escape)
	waitFor(t, p, "the job's own form", `() => /job/.test(document.querySelector("#jobp .pinsp .ihead").textContent)`)
	eval(t, p, `() => {
	  const ta = document.querySelector("#jobp .pinsp .ifield textarea");
	  ta.value = "unknown keys kept";
	  ta.dispatchEvent(new Event("input", { bubbles: true }));
	}`)
	toggle()
	waitFor(t, p, "the job's JSON view", `() => document.querySelector(".app").classList.contains("pipe-json")`)
	got := evalStr(t, p, `() => { const s = JSON.parse(monaco.editor.getEditors()[0].getValue());
	  return [s.desc, s.colour, s.params.day.hint, s.pipelines[0].retries, s.triggers.shedule.join(), s.policy.on_fail].join("|"); }`)
	if want := "unknown keys kept|red|x|3|0 3 * * *|stop"; got != want {
		t.Errorf("after a canvas edit = %q, want %q", got, want)
	}

	// the tab as it was
	eval(t, p, `() => monaco.editor.getEditors()[0].setValue(window.__keptJobUK)`)
	toggle()
	waitFor(t, p, "the job back, nothing unsaved", `() =>
	  !document.querySelector(".app").classList.contains("pipe-json") &&
	  document.querySelectorAll("#jobp .jcard").length === 4 &&
	  !document.querySelector("#qtabs .qtab.job.on .qdirty")`)
}
