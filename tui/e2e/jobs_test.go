package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

// pipelinesAndJobs drives the Ctrl+J browser, the run monitor and the Runs
// list (Pipelines Phase 5) in the real binary:
//
//	Ctrl+J, / e2e_names, Enter ─► the monitor: the pipeline ✓, its nodes'
//	                               counters; Esc ─► its lines in the log,
//	                               its preview in the grid
//	⌥J ─► the Runs list; Enter opens the run, Backspace comes back
//	Ctrl+J, e2e_job ─► a job of two steps, ✓ each, its lines tagged
//	Ctrl+J, e2e_slow ─► a run that waits: on the status bar, Ctrl+C does
//	                    not quit over it, a click opens it, ^K stops it
//	e ─► $EDITOR (the stand-in) writes a broken spec: the check's finding
//	     in the log as path:line:col; again with the good text: no problems
//
// The examples run on the built-in demos, which a config file replaces
// (writeConfig), so the step writes pipelines and a job of its own on lite.
func pipelinesAndJobs(t *testing.T, e *env, u *term) {
	dir := filepath.Join(e.home, ".config", "dbc")
	write := func(sub, name, text string) {
		t.Helper()
		d := filepath.Join(dir, sub)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// the cats' names copied into a table of their own (made on the way),
	// and shown: one source feeding a sink and a preview
	names := `{
  "name": "e2e_names",
  "desc": "The cats' names, copied and shown",
  "fragments": [
    {
      "name": "copy",
      "nodes": [
        { "id": "src", "plugin": "sql.read", "cfg": { "conn": "lite", "query": "SELECT name FROM cats ORDER BY name" } },
        { "id": "dst", "plugin": "sql.write", "cfg": { "conn": "lite", "table": "e2e_names", "create": "true", "truncate": "true" } },
        { "id": "peek", "plugin": "preview", "cfg": { "rows": "50" } }
      ],
      "edges": [ ["src", "dst"], ["src", "peek"] ]
    }
  ]
}
`
	write("pipelines", "e2e_names.json", names)
	write("pipelines", "e2e_count.json", `{"name": "e2e_count", "fragments": [{"name": "count", "nodes": [
	  {"id": "src", "plugin": "sql.read", "cfg": {"conn": "lite", "query": "SELECT count(*) AS copied FROM e2e_names"}},
	  {"id": "peek", "plugin": "preview", "cfg": {"rows": "5"}}], "edges": [["src", "peek"]]}]}`)
	write("pipelines", "e2e_slow.json", `{"name": "e2e_slow", "fragments": [{"name": "wait", "nodes": [{"id": "z", "plugin": "go.action",
	  "cfg": {"code": "func Run(s *sdb.S) error {\n\t<-s.Ctx().Done()\n\treturn s.Ctx().Err()\n}\n"}}]}]}`)
	write("jobs", "e2e_job.json", `{"name": "e2e_job", "desc": "Copy the names, then count them",
	  "pipelines": [{"id": "a", "pipeline": "e2e_names"}, {"id": "b", "pipeline": "e2e_count", "after": ["a"]}]}`)

	// pick runs the spec the filter narrows to: / types into it, Enter
	// acts on the row it leaves first
	pick := func(name string) {
		u.ctrl('j')
		u.waitFor("Pipelines & jobs", name+".json")
		u.key('/')
		u.typeText(name)
		u.key(uv.KeyEnter)
	}

	// ── a pipeline: the monitor, the log, the preview ─────────────────────
	u.ctrl('j')
	u.waitFor("Pipelines & jobs", "Jobs", "e2e_job.json", "Copy the names, then count them", "e2e_names.json",
		"Examples · read-only", "ƒ Scripts ^O", "◷ Runs ⌥J")
	u.key(uv.KeyEscape)
	pick("e2e_names")
	u.waitFor("Run 2026", "pipeline e2e_names · manual (terminal)", "✓ succeeded", "▾ ✓ copy",
		"src  sql.read", "0 → 3", "dst  sql.write", "3 → 3", "peek  preview", "── log · the run")
	u.key(uv.KeyEscape)
	u.waitGone("Run 2026")
	u.waitFor("[e2e_names] fragment copy", "✓ pipeline e2e_names succeeded", "preview copy/peek", "│ Leo ", "│ Tom ")

	// ── the Runs list, and back from a run to it ─────────────────────────
	// a run another process made — a cron's `dbc pipeline run` — is listed
	// beside the TUI's, from its record in runs_dir
	e.cli(t, "pipeline", "run", "e2e_count")
	u.key('j', uv.ModAlt)
	u.waitFor("Runs", "✓ e2e_names", "pipeline  manual", "3 rows", "✓ e2e_count", "pipeline  cli", "Enter opens · ^K stops")
	u.key(uv.KeyEnter)
	u.waitFor("Run 2026", "⌫ the runs")
	u.key(uv.KeyBackspace)
	u.waitFor("Enter opens · ^K stops")
	u.key(uv.KeyEscape)
	u.waitGone("Enter opens · ^K stops")

	// ── a job: two steps, in order ───────────────────────────────────────
	pick("e2e_job")
	u.waitFor("job e2e_job · manual (terminal)", "✓ succeeded", "✓ a  e2e_names", "✓ b  e2e_count", "after a")
	u.key(uv.KeyEscape)
	u.waitFor("[e2e_job › b] ▶ b: pipeline e2e_count", "[e2e_job] job e2e_job succeeded", "copied │")

	// ── a run that waits: the status bar, Ctrl+C, the click, ^K ──────────
	pick("e2e_slow")
	u.waitFor("● running", "■ Stop ^K")
	u.key(uv.KeyEscape)
	// the monitor gone before the next key: a bare ESC is held a moment by
	// the terminal's decoder (it may begin an Alt chord), and a Ctrl+C sent
	// within it would arrive as Ctrl+Alt+C
	u.waitGone("Run 2026")
	u.waitFor("● e2e_slow ")
	u.ctrl('c')
	u.waitFor("pipeline e2e_slow still running — ⌥J shows the runs")
	u.click("● e2e_slow ", 2)
	u.waitFor("Run 2026", "■ Stop ^K")
	u.ctrl('k')
	u.waitFor("■ canceled", "stopping run")
	u.waitGone("● running")
	u.key(uv.KeyEscape)
	u.waitGone("Run 2026")
	u.waitGone("● e2e_slow ")
	// and the other way: the TUI's runs are records any process lists
	runs := e.cli(t, "runs", "-t", "json")
	for _, want := range []string{`"name": "e2e_names"`, `"by": "terminal"`, `"name": "e2e_job"`, `"status": "canceled"`} {
		if !strings.Contains(runs, want) {
			t.Fatalf("dbc runs lacks %s:\n%s", want, runs)
		}
	}

	// ── $EDITOR, and the check when it exits ─────────────────────────────
	// the stand-in editor copies next-edit.go over the file: first a node
	// whose plugin does not exist, then the good text again
	e.nextEdit(t, `{"name": "e2e_names", "fragments": [{"name": "copy", "nodes": [
  {"id": "src", "plugin": "sql.read", "cfg": {"conn": "lite", "query": "SELECT 1"}},
  {"id": "peek", "plugin": "no.such", "cfg": {}}], "edges": [["src", "peek"]]}]}`)
	u.ctrl('j')
	u.key('/')
	u.typeText("e2e_names")
	u.key(uv.KeyEscape) // back to the list, the row still under the cursor
	u.waitFor("Enter run · p params")
	u.key('e')
	u.waitFor("e2e_names.json saved — the check found 1 error(s)", "e2e_names.json:3:", "copy/peek", "Pipelines & jobs")
	e.nextEdit(t, names)
	u.key('e')
	u.waitFor("e2e_names.json saved — checked, no problems")
	u.key(uv.KeyEscape)
	u.waitGone("Pipelines & jobs")
}
