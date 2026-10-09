package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/cats"
	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/jobs"
	"github.com/rohanthewiz/dbc/pipeline"
)

// The jobs side of the TUI (jobs.go, pipes.go, runs.go): the Ctrl+J
// browser, runs on the session's engine, their lines in the log, the run
// monitor and the Runs list.
//
// The engine runs on goroutines of its own and reaches the model through
// the pump, which calls m.send from its goroutine — so these tests catch
// its messages in a locked queue (jobQueue) and drive them into the model
// themselves, in order, once the run they wait for has ended.

// jobQueue collects the pump's messages; everything else m.send carries
// goes on to the harness as before.
type jobQueue struct {
	mu   sync.Mutex
	msgs []tea.Msg
}

// newJobsModel is a test model with pipelines and jobs directories of its
// own (runs stay in memory: NoPersist), and its pump's messages queued.
func newJobsModel(t *testing.T) (*Model, *jobQueue, string, string) {
	t.Helper()
	for _, k := range []string{cats.EnvMarker, cats.EnvPaneID, cats.EnvControlSocket, cats.EnvHookSocket} {
		t.Setenv(k, "")
	}
	// the harness runs the 1s refresh's command (a sleep) in line, and
	// drops its message: no need to wait the second
	prevTick := jobTickEvery
	jobTickEvery = time.Millisecond
	t.Cleanup(func() { jobTickEvery = prevTick })
	pdir, jdir := t.TempDir(), t.TempDir()
	m := newTestModelCfg(t, func(c *config.Config) { c.PipelinesDir, c.JobsDir = pdir, jdir })
	q := &jobQueue{}
	prev := m.send
	m.send = func(msg tea.Msg) {
		if _, ok := msg.(jobMsg); ok {
			q.mu.Lock()
			q.msgs = append(q.msgs, msg)
			q.mu.Unlock()
			return
		}
		prev(msg)
	}
	return m, q, pdir, jdir
}

// until drives the queued engine events into the model, in order, until
// one satisfies done — or fails the test after a while.
func (q *jobQueue) until(t *testing.T, m *Model, what string, done func(jobs.Event) bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		q.mu.Lock()
		msgs := q.msgs
		q.msgs = nil
		q.mu.Unlock()
		hit := false
		for _, msg := range msgs {
			drive(t, m, msg)
			if done(msg.(jobMsg).ev) {
				hit = true
			}
		}
		if hit {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited for %s; log:\n%s", what, logText(m))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ended is a done func for until: run id's RunDone.
func ended(id string) func(jobs.Event) bool {
	return func(ev jobs.Event) bool {
		d, ok := ev.(*jobs.RunDone)
		return ok && (id == "" || d.Run.ID == id)
	}
}

// writeSpec writes a spec file into dir.
func writeSpec(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// catsPipe is a pipeline on the test model's demo: the cats' names into a
// preview. extra is put among its keys (params).
func catsPipe(name, extra, query string) string {
	return `{"name": "` + name + `", ` + extra + `"fragments": [{"name": "names", "nodes": [` +
		`{"id": "src", "plugin": "sql.read", "cfg": {"conn": "demo-sqlite", "query": "` + query + `"}},` +
		`{"id": "peek", "plugin": "preview", "cfg": {"rows": "50"}}], "edges": [["src", "peek"]]}]}`
}

// slowPipe is a pipeline whose only node waits until it is stopped.
const slowPipe = `{"name": "slow", "fragments": [{"name": "wait", "nodes": [{"id": "z", "plugin": "go.action",
  "cfg": {"code": "func Run(s *sdb.S) error {\n\t<-s.Ctx().Done()\n\treturn s.Ctx().Err()\n}\n"}}]}]}`

// pipes is the open browser, or the test fails.
func pipes(t *testing.T, m *Model) *pipesModal {
	t.Helper()
	pm, ok := m.modal.(*pipesModal)
	if !ok {
		t.Fatalf("modal = %T, want the pipelines & jobs browser; log:\n%s", m.modal, logText(m))
	}
	return pm
}

// pipeCursorTo puts the browser's cursor on the row labelled name in the
// section of kind k (mine or an example).
func pipeCursorTo(t *testing.T, pm *pipesModal, kind pipeRowKind, k specKind, name string) {
	t.Helper()
	for i, it := range pm.lst.items {
		if r, ok := it.data.(pipeRow); ok && r.kind == kind && r.spec == k && r.name == name {
			pm.lst.cur = i
			return
		}
	}
	t.Fatalf("no row %s in the browser", name)
}

// monitor is the open run monitor, or the test fails.
func monitor(t *testing.T, m *Model) *runMonitor {
	t.Helper()
	mon, ok := m.modal.(*runMonitor)
	if !ok {
		t.Fatalf("modal = %T, want the run monitor; log:\n%s", m.modal, logText(m))
	}
	return mon
}

// Ctrl+J lists the user's pipelines and jobs and the examples; Enter runs a
// pipeline on the session's engine: the monitor opens on it, its lines go
// to the log under its name, its preview lands in the grid of the tab it
// was started from, and the monitor's tree ends ✓ with its counters.
func TestPipesBrowserRunsAPipeline(t *testing.T) {
	m, q, pdir, jdir := newJobsModel(t)
	writeSpec(t, pdir, "names.json", catsPipe("names", `"desc": "The cats' names", `, "SELECT name FROM cats ORDER BY name"))
	writeSpec(t, jdir, "solo.json", `{"name": "solo", "pipelines": [{"id": "only", "pipeline": "names"}], "triggers": {"schedule": ["0 2 * * *"]}}`)

	key(t, m, "ctrl+j")
	pm := pipes(t, m)
	c := frame(m)
	for _, s := range []string{"Pipelines & jobs", "Jobs", "solo.json", "◷ 0 2 * * *", "names.json", "The cats' names",
		"Examples · read-only", "copy-cats.json", "nightly.json"} {
		findText(t, c, s)
	}
	pipeCursorTo(t, pm, prMine, specPipeline, "names.json")
	key(t, m, "enter")
	mon := monitor(t, m)
	if !strings.Contains(frame(m).Text(), "● names") {
		t.Errorf("the status bar does not show the run:\n%s", frame(m).Line(39))
	}
	q.until(t, m, "the run's end", ended(mon.id))

	// the log names the run's lines; the preview is in the grid
	log := logText(m)
	for _, s := range []string{"pipeline names started — run " + mon.id, "[names] "} {
		if !strings.Contains(log, s) {
			t.Errorf("log lacks %q:\n%s", s, log)
		}
	}
	if r := m.ws.LastResult(); r == nil || len(r.Rows) != 8 || !strings.HasPrefix(m.grid.colName(0), "name") {
		t.Fatalf("the preview did not land in the grid: %+v", r)
	}
	// the monitor shows the finished run: its tree, the fragment's
	// counters, the node rows
	c = frame(m)
	for _, s := range []string{"Run " + mon.id, "✓ succeeded", "✓ names", "8 rows", "src  sql.read", "0 → 8", "peek  preview", "── log · the run"} {
		findText(t, c, s)
	}
	if m.jobIndicator() != "" {
		t.Errorf("a finished run is still on the status bar: %q", m.jobIndicator())
	}
	// Esc leaves; the run is in the Runs list (⌥J), and Enter there opens
	// it again, Backspace goes back
	key(t, m, "esc")
	key(t, m, "alt+j")
	rm, ok := m.modal.(*runsModal)
	if !ok {
		t.Fatalf("modal = %T, want the Runs list", m.modal)
	}
	findText(t, frame(m), "names")
	if h, ok := rm.current(); !ok || h.ID != mon.id || h.Status != "succeeded" {
		t.Fatalf("the Runs list's first run = %+v", h)
	}
	key(t, m, "enter")
	monitor(t, m)
	key(t, m, "backspace")
	if _, ok := m.modal.(*runsModal); !ok {
		t.Fatalf("Backspace: modal = %T, want the Runs list", m.modal)
	}
}

// A job runs its steps in order; its lines are tagged with the job and the
// step; the monitor's tree has a row per step, with its pipeline.
func TestPipesBrowserRunsAJob(t *testing.T) {
	m, q, pdir, jdir := newJobsModel(t)
	writeSpec(t, pdir, "first.json", catsPipe("first", "", "SELECT name FROM cats"))
	writeSpec(t, pdir, "second.json", catsPipe("second", "", "SELECT breed FROM cats"))
	writeSpec(t, jdir, "two.json", `{"name": "two", "pipelines": [{"id": "a", "pipeline": "first"}, {"id": "b", "pipeline": "second", "after": ["a"]}]}`)

	key(t, m, "ctrl+j")
	pipeCursorTo(t, pipes(t, m), prMine, specJob, "two.json")
	key(t, m, "enter")
	mon := monitor(t, m)
	q.until(t, m, "the job's end", ended(mon.id))
	if mon.run.Status != pipeline.Succeeded || len(mon.run.Pipelines) != 2 {
		t.Fatalf("the job's record: %+v", mon.run)
	}
	log := logText(m)
	for _, s := range []string{"[two] job two: 2 pipelines from a", "[two › a] ▶ a: pipeline first", "[two › b] ▶ b: pipeline second", "[two] job two succeeded"} {
		if !strings.Contains(log, s) {
			t.Errorf("log lacks %q:\n%s", s, log)
		}
	}
	c := frame(m)
	findText(t, c, "✓ a  first")
	findText(t, c, "✓ b  second")
	findText(t, c, "after a")

	// a step row narrows the log to the step's lines; ← folds it
	for i, row := range mon.rows {
		if row.depth == 1 && mon.run.Pipelines[row.pi].ID == "b" {
			mon.cur = i
		}
	}
	c = frame(m)
	findText(t, c, "── log · b")
	if strings.Contains(c.Text(), "▶ a: pipeline first") {
		t.Errorf("the log under step b shows step a's lines:\n%s", c.Text())
	}
	n := len(mon.rows)
	key(t, m, "left")
	if len(mon.rows) >= n {
		t.Errorf("← did not fold the step: %d rows, was %d", len(mon.rows), n)
	}
}

// A param with no default is asked for before the run, and the run gets
// the value typed; an empty answer is refused in the prompt. p asks for
// every param, offering each default.
func TestPipesBrowserAsksForParams(t *testing.T) {
	m, q, pdir, _ := newJobsModel(t)
	writeSpec(t, pdir, "aged.json", catsPipe("aged", `"params": {"min": {"default": "", "doc": "the youngest age"}, "max": {"default": "9"}}, `,
		"SELECT name FROM cats WHERE age >= ${min} AND age <= ${max}"))

	key(t, m, "ctrl+j")
	pipeCursorTo(t, pipes(t, m), prMine, specPipeline, "aged.json")
	key(t, m, "enter")
	p := prompt(t, m)
	if p.head != "Run aged · min" || !strings.Contains(p.hint, "the youngest age") {
		t.Fatalf("prompt = %q (%q)", p.head, p.hint)
	}
	key(t, m, "enter") // empty: refused in place
	if prompt(t, m).errMsg == "" {
		t.Fatal("an empty value for a param with no default was taken")
	}
	typeText(t, m, "5")
	key(t, m, "enter")
	mon := monitor(t, m)
	q.until(t, m, "the run's end", ended(mon.id))
	if got := mon.run.Params["min"]; got != "5" {
		t.Errorf("the run's min = %q, want 5 (params %v)", got, mon.run.Params)
	}
	if r := m.ws.LastResult(); r == nil || len(r.Rows) != 3 {
		t.Errorf("cats aged 5 to 9: %+v", r)
	}

	// p asks for both, in name order, each offered with its default
	key(t, m, "esc")
	key(t, m, "ctrl+j")
	pipeCursorTo(t, pipes(t, m), prMine, specPipeline, "aged.json")
	key(t, m, "p")
	if p := prompt(t, m); p.head != "Run aged · max (1 of 2)" || p.field.Text() != "9" {
		t.Fatalf("first prompt = %q %q", p.head, p.field.Text())
	}
	key(t, m, "esc") // back out: the browser again, nothing run
	pipes(t, m)
}

// The status bar shows a run going; Ctrl+C does not quit over it; Ctrl+K in
// the monitor stops it, and the run ends canceled.
func TestRunMonitorStopsARun(t *testing.T) {
	m, q, pdir, _ := newJobsModel(t)
	writeSpec(t, pdir, "slow.json", slowPipe)

	key(t, m, "ctrl+j")
	pipeCursorTo(t, pipes(t, m), prMine, specPipeline, "slow.json")
	key(t, m, "enter")
	mon := monitor(t, m)
	q.until(t, m, "the fragment running", func(ev jobs.Event) bool {
		p, ok := ev.(*jobs.Progress)
		return ok && p.Fragment.Status == pipeline.Running
	})
	key(t, m, "esc")
	c := frame(m)
	x, y := findText(t, c, "● slow")
	if y != c.H-1 {
		t.Errorf("the run's indicator is not on the status bar (row %d)", y)
	}

	// Ctrl+C: a run is going — said, not quit
	key(t, m, "ctrl+c")
	if m.quit || !strings.Contains(logText(m), "pipeline slow still running") {
		t.Fatalf("Ctrl+C with a run going: quit=%v, log:\n%s", m.quit, logText(m))
	}

	// a click on the indicator opens the run; ^K stops it
	click(t, m, x+1, y)
	if monitor(t, m).id != mon.id {
		t.Fatal("the indicator opened another run")
	}
	findText(t, frame(m), "■ Stop ^K")
	key(t, m, "ctrl+k")
	q.until(t, m, "the run's end", ended(mon.id))
	mon = monitor(t, m)
	if mon.run.Status != pipeline.Canceled {
		t.Errorf("stopped run: %s", mon.run.Status)
	}
	if !strings.Contains(mon.note, "stopping run") || m.jobIndicator() != "" {
		t.Errorf("note %q, indicator %q", mon.note, m.jobIndicator())
	}
	if strings.Contains(frame(m).Text(), "■ Stop ^K") {
		t.Error("an ended run still offers Stop")
	}
}

// n makes a spec from the starter — its own name set to the file's —
// edits it in $EDITOR, and checks it on return: a finding is logged as
// path:line:col, the form an editor jumps to. d copies an example under a
// name of its own; Del trashes, and Enter on the trashed row restores.
func TestPipesBrowserMakesEditsAndChecks(t *testing.T) {
	m, _, pdir, _ := newJobsModel(t)
	var edit func(path string)
	prev := execEditor
	execEditor = func(c *exec.Cmd, fn tea.ExecCallback) tea.Cmd {
		path := c.Args[len(c.Args)-1]
		return func() tea.Msg {
			if edit != nil {
				edit(path)
			}
			return fn(nil)
		}
	}
	t.Cleanup(func() { execEditor = prev })
	t.Setenv("EDITOR", "true") // found on PATH; the stub stands in for it

	key(t, m, "ctrl+j")
	key(t, m, "n")
	key(t, m, "enter") // "Pipeline — …"
	p := prompt(t, m)
	if p.head != "New pipeline" || p.field.Text() != "pipeline.json" {
		t.Fatalf("prompt %q %q", p.head, p.field.Text())
	}
	// the editor writes a node whose plugin does not exist
	edit = func(path string) {
		b, _ := os.ReadFile(path)
		_ = os.WriteFile(path, []byte(strings.Replace(string(b), `"plugin": "preview"`, `"plugin": "no.such"`, 1)), 0o600)
	}
	typeText(t, m, "orders")
	key(t, m, "enter")
	pipes(t, m) // back in the browser, on the new spec
	text, _, err := m.specKit(specPipeline).read(pdir, "orders.json")
	if err != nil || !strings.Contains(text, `"name": "orders"`) || !strings.Contains(text, `"conn": "demo-sqlite"`) {
		t.Fatalf("the new pipeline (%v):\n%s", err, text)
	}
	line, _ := pipeline.Locate(text, "load/peek")
	want := filepath.Join(pdir, "orders.json") + ":" + itoa(line) + ":"
	if log := logText(m); !strings.Contains(log, "orders.json saved — the check found 1 error(s)") ||
		!strings.Contains(log, config.TildePath(want)) || !strings.Contains(log, "load/peek") {
		t.Fatalf("the check's finding is not placed (want %s):\n%s", want, log)
	}

	// d on an example: a copy of yours, named after its file
	edit = nil
	pipeCursorTo(t, pipes(t, m), prExample, specJob, "nightly.json")
	key(t, m, "d")
	if p := prompt(t, m); p.head != "Copy the example nightly.json" || p.field.Text() != "nightly.json" {
		t.Fatalf("prompt %q %q", p.head, p.field.Text())
	}
	typeText(t, m, "mine")
	key(t, m, "enter")
	jtext, _, err := m.specKit(specJob).read(m.cfg.JobsDir, "mine.json")
	if err != nil || !strings.Contains(jtext, `"name": "mine"`) {
		t.Fatalf("the copy (%v):\n%s", err, jtext)
	}
	if !strings.Contains(logText(m), "mine.json saved") && !strings.Contains(logText(m), "mine.json unchanged") {
		t.Errorf("the copy was not checked:\n%s", logText(m))
	}

	// Del trashes it; t shows the Trash; Enter restores
	pipeCursorTo(t, pipes(t, m), prMine, specJob, "mine.json")
	key(t, m, "delete")
	pm := pipes(t, m)
	if len(pm.jobs) != 0 || len(pm.trash) != 1 {
		t.Fatalf("after Del: %d jobs, %d trashed", len(pm.jobs), len(pm.trash))
	}
	key(t, m, "t")
	key(t, m, "G")
	key(t, m, "enter")
	if pm := pipes(t, m); len(pm.jobs) != 1 || !strings.Contains(logText(m), "restored the job mine.json") {
		t.Fatalf("restore: %d jobs; log:\n%s", len(pm.jobs), logText(m))
	}
}

// The pump hands events to the program in the order the engine sent them,
// and push never waits on the program — the engine sends RunStarted from
// inside the Update that started the run.
func TestJobPumpKeepsOrderWithoutBlocking(t *testing.T) {
	got := make(chan tea.Msg, 1000)
	release := make(chan struct{})
	p := newJobPump(func(msg tea.Msg) {
		<-release // a program busy in Update: Send waits
		got <- msg
	})
	defer p.close()
	done := make(chan struct{})
	go func() {
		for i := range 500 {
			p.push(&jobs.Notice{Text: itoa(i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("push waited on the program")
	}
	close(release)
	for i := range 500 {
		select {
		case msg := <-got:
			if n := msg.(jobMsg).ev.(*jobs.Notice).Text; n != itoa(i) {
				t.Fatalf("event %d arrived as %s", i, n)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d never arrived", i)
		}
	}
}

// Quitting with a run going stops it: the engine cancels it, its fragment
// rolls back, and its record ends canceled — not left running for the next
// process to call interrupted.
func TestQuitStopsARun(t *testing.T) {
	m, q, pdir, _ := newJobsModel(t)
	writeSpec(t, pdir, "slow.json", slowPipe)
	key(t, m, "ctrl+j")
	pipeCursorTo(t, pipes(t, m), prMine, specPipeline, "slow.json")
	key(t, m, "enter")
	id := monitor(t, m).id
	q.until(t, m, "the fragment running", func(ev jobs.Event) bool {
		p, ok := ev.(*jobs.Progress)
		return ok && p.Fragment.Status == pipeline.Running
	})
	m.shutdown()
	r, ok := m.jobs.Get(id)
	if !ok || r.Status != pipeline.Canceled || r.Pipelines[0].Fragments[0].Status != pipeline.Canceled {
		t.Fatalf("after quitting, the run: %+v", r)
	}
}
