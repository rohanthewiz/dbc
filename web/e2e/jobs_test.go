package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-rod/rod"
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
