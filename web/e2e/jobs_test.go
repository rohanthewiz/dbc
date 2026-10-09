package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-rod/rod"
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
	p.MustReload()
	waitFor(t, p, "the job tab after a reload, its cards ✓", `() => {
	  const t = document.querySelector("#qtabs .qtab.job.on .qt");
	  return !!t && t.textContent === "e2e_nightly.json" && document.querySelectorAll("#jobp .jcard.s-succeeded").length === 4;
	}`)

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
