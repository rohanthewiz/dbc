package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// jobResume is a run page's ↻ Resume (N-339), in the Runs dialog:
//
//	a two-step job, its second step stopped ─► the page offers ↻ Resume
//	↻ Resume  ─► the new run's page: the first step's card reused (↻,
//	             dashed), the second running again; "resumes run <id>",
//	             a link to the stopped run
//	the reused card  ─► "reused: run <id> ran it"
//	‹ Runs    ─► the list marks the resume ↻ by its trigger
//
// It stands on boot alone (DBC_E2E_STEPS=resume): its pipelines and job
// are its own, on the lite connection.
func jobResume(t *testing.T, e *env, p *rod.Page) {
	cfg := filepath.Join(e.home, ".config", "dbc")
	for dir, files := range map[string]map[string]string{
		"pipelines": {
			"e2e_rquick.json": `{"name": "e2e_rquick", "fragments": [{"name": "f", "nodes": [
			  {"id": "x", "plugin": "sql.exec", "cfg": {"conn": "lite", "sql": "SELECT 1"}}]}]}`,
			"e2e_rslow.json": `{"name": "e2e_rslow", "fragments": [{"name": "f", "nodes": [{"id": "z", "plugin": "go.action",
			  "cfg": {"code": "func Run(s *sdb.S) error {\n\t<-s.Ctx().Done()\n\treturn s.Ctx().Err()\n}\n"}}]}]}`,
		},
		"jobs": {"e2e_resume.json": `{"name": "e2e_resume", "pipelines": [{"id": "a", "pipeline": "e2e_rquick"},
		  {"id": "b", "pipeline": "e2e_rslow", "after": ["a"]}]}`},
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
	first := evalStr(t, p, `async () => (await dbc.api("POST", "/api/v1/jobs/e2e_resume.json/run", {})).run.id`)
	if first == "" {
		t.Fatal("the job did not start")
	}

	// the run's page in the Runs dialog; b stopped while it runs
	chord(t, p, modAlt, "r", "KeyR", 82)
	waitFor(t, p, "the Runs dialog, listing the run", `(id) => [...document.querySelectorAll(".modal.runs .rvrow")].some((r) => r.dataset.id === id)`, first)
	eval(t, p, `(id) => [...document.querySelectorAll(".modal.runs .rvrow")].find((r) => r.dataset.id === id).click()`, first)
	waitFor(t, p, "b running, a ✓, no Resume while it runs", `() =>
	  !!document.querySelector('.modal.runs .rvdag .dcard.s-running[data-step="b"]') &&
	  !!document.querySelector('.modal.runs .rvdag .dcard.s-succeeded[data-step="a"]') &&
	  !document.querySelector('.modal.runs .rvacts button[data-act="resume"]')`)
	clickSel(t, p, `.modal.runs .rvacts button[data-act="stop"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "stopped: canceled, ↻ Resume offered", `() => {
	  const h = document.querySelector(".modal.runs .rvhead .rvst");
	  return !!h && /canceled/.test(h.textContent) && !!document.querySelector('.modal.runs .rvacts button[data-act="resume"]');
	}`)

	// ↻ Resume: the new run's page, a reused, b running again
	clickSel(t, p, `.modal.runs .rvacts button[data-act="resume"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "the resume's page: a ↻ reused, b running, it names the run it resumes", `(id) => {
	  const a = document.querySelector('.modal.runs .rvdag .dcard.s-reused[data-step="a"]');
	  const line = [...document.querySelectorAll(".modal.runs .rvparams")].find((x) => /^resumes run /.test(x.textContent));
	  return !!a && /↻/.test(a.querySelector(".dst").textContent) && /reused/.test(a.querySelector(".cnum").textContent) &&
	    getComputedStyle(a).borderLeftStyle === "dashed" &&
	    !!document.querySelector('.modal.runs .rvdag .dcard.s-running[data-step="b"]') &&
	    !!line && !!line.querySelector('button[data-act="openrun"][data-id="' + id + '"]') &&
	    document.querySelector(".modal.runs .rvhead .rvmeta").textContent.includes("(resumes " + id + ")");
	}`, first)
	resumed := evalStr(t, p, `() => { const b = document.querySelector('.modal.runs .rvacts button[data-act="copyid"]'); return b ? b.textContent.replace(/^⧉ /, "") : ""; }`)
	if resumed == "" || resumed == first {
		t.Fatalf("the page shows run %q, want a new run (first %s)", resumed, first)
	}
	clickSel(t, p, `.modal.runs .rvdag .dcard[data-step="a"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "a's section: reused, and from which run", `(id) => {
	  const s = document.querySelector(".modal.runs .rvpipe");
	  return !!s && /reused: run /.test(s.textContent) && !!s.querySelector('button[data-act="openrun"][data-id="' + id + '"]');
	}`, first)
	shot(t, p, "run-resumed")
	clickSel(t, p, `.modal.runs .rvacts button[data-act="stop"]`, proto.InputMouseButtonLeft)
	waitFor(t, p, "the resume stopped", `() => /canceled/.test((document.querySelector(".modal.runs .rvhead .rvst") || {}).textContent || "")`)

	// the list: the resume marked ↻ by its trigger
	clickSel(t, p, ".modal.runs .rvback", proto.InputMouseButtonLeft)
	waitFor(t, p, "the list, the resume marked", `(id) => {
	  const r = [...document.querySelectorAll(".modal.runs .rvrow")].find((x) => x.dataset.id === id);
	  return !!r && /↻/.test(r.querySelector(".rvtrig").textContent);
	}`, resumed)
	chord(t, p, 0, "Escape", "Escape", 27)
	waitFor(t, p, "the dialog closed", `() => !document.querySelector(".modal.runs")`)
}
