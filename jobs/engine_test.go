package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/scripts"
)

// testEngine is an engine over two SQLite files ("a" seeded with the demo
// cats, "b" empty) and the events it sent.
type testEngine struct {
	*Engine
	mgr *db.Manager
	mu  sync.Mutex
	evs []Event
}

func newTestEngine(t *testing.T) *testEngine {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{MaxRows: 1000, Connections: []config.Connection{
		{Name: "a", Driver: "sqlite", DSN: filepath.Join(dir, "a.db")},
		{Name: "b", Driver: "sqlite", DSN: filepath.Join(dir, "b.db")},
	}}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	if err := db.SeedDemo(mgr, "a"); err != nil {
		t.Fatal(err)
	}
	te := &testEngine{mgr: mgr}
	te.Engine = New(cfg, mgr, Options{ProgressEvery: time.Millisecond, Sink: func(e Event) {
		te.mu.Lock()
		te.evs = append(te.evs, e)
		te.mu.Unlock()
	}})
	t.Cleanup(func() { te.Close(5 * time.Second) })
	return te
}

// events is the events of run id, in order.
func (te *testEngine) events(id string) []Event {
	te.mu.Lock()
	defer te.mu.Unlock()
	var out []Event
	for _, e := range te.evs {
		if e.RunID() == id {
			out = append(out, e)
		}
	}
	return out
}

func (te *testEngine) wait(t *testing.T, id string) Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := te.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (te *testEngine) rows(t *testing.T, conn, q string) [][]string {
	t.Helper()
	r, err := te.mgr.Run(conn, q)
	if err != nil {
		t.Fatal(err)
	}
	return r.Rows
}

// example is a built-in pipeline with its demo connection renamed to "a".
func example(t *testing.T, name string) *pipeline.Spec {
	t.Helper()
	ex, ok := scripts.PipelineByName(name)
	if !ok {
		t.Fatalf("no example %s", name)
	}
	spec, err := pipeline.Parse(strings.ReplaceAll(ex.Text, "demo-sqlite", "a"))
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// A run of the clean-and-load example: started at once, every fragment's
// states in order (queued in the header, running, succeeded), the log
// lines with the runner's summary, a final record that keeps the log, and
// what it wrote in the database. Its go.transform needs package script's
// plugins, which the engine registers by importing it.
func TestEngineRunsAPipeline(t *testing.T) {
	te := newTestEngine(t)
	head, err := te.StartPipeline(Request{Spec: example(t, "clean-and-load.json"), Params: map[string]string{"min_age": "3"}, Origin: "tab1"})
	if err != nil {
		t.Fatal(err)
	}
	if head.Status != pipeline.Running || head.Trigger != TriggerManual || head.Origin != "tab1" ||
		len(head.Pipelines) != 1 || len(head.Pipelines[0].Fragments) != 2 ||
		head.Pipelines[0].Fragments[1].Status != pipeline.Queued || len(head.Pipelines[0].Fragments[0].Nodes) != 5 {
		t.Fatalf("header = %+v", head)
	}
	fin := te.wait(t, head.ID)
	if fin.Status != pipeline.Succeeded || fin.Error != "" || fin.Ended.IsZero() {
		t.Fatalf("final = %+v", fin)
	}
	frags := fin.Pipelines[0].Fragments
	if frags[0].Status != pipeline.Succeeded || frags[0].Rows != 5 || frags[1].Status != pipeline.Succeeded {
		t.Errorf("fragments = %+v", frags)
	}
	if got := te.rows(t, "a", "SELECT count(*) FROM cats_clean"); got[0][0] != "5" {
		t.Errorf("cats_clean: %v", got)
	}
	var log []string
	for _, l := range fin.Log {
		log = append(log, l.Text)
	}
	if !slices.ContainsFunc(log, func(s string) bool { return strings.HasPrefix(s, "pipeline clean-and-load: 2 fragments") }) ||
		!slices.ContainsFunc(log, func(s string) bool { return strings.HasPrefix(s, `DDL a: CREATE TABLE IF NOT EXISTS "cats_clean"`) }) {
		t.Errorf("log = %q", log)
	}

	evs := te.events(head.ID)
	if _, ok := evs[0].(*RunStarted); !ok {
		t.Errorf("first event %T", evs[0])
	}
	done, ok := evs[len(evs)-1].(*RunDone)
	if !ok || done.Run.Status != pipeline.Succeeded || done.Run.Log != nil {
		t.Errorf("last event %T %+v", evs[len(evs)-1], evs[len(evs)-1])
	}
	// per fragment: running before succeeded, and nothing after the end
	states := map[string][]pipeline.Status{}
	for _, e := range evs {
		if p, ok := e.(*Progress); ok {
			s := states[p.Fragment.Name]
			if len(s) == 0 || s[len(s)-1] != p.Fragment.Status {
				states[p.Fragment.Name] = append(s, p.Fragment.Status)
			}
		}
	}
	for _, f := range []string{"clean", "stamp"} {
		if got := states[f]; !slices.Equal(got, []pipeline.Status{pipeline.Running, pipeline.Succeeded}) {
			t.Errorf("%s states = %v", f, got)
		}
	}
	if got := te.Recent(); len(got) != 1 || got[0].ID != head.ID || got[0].Log != nil {
		t.Errorf("recent = %+v", got)
	}
	if got := te.Running(); len(got) != 0 {
		t.Errorf("running after the end = %+v", got)
	}
}

// A relative path in a file node is in the config's files_dir, not the
// process's working directory: the engine is what dbc web, its scheduler,
// the TUI and `dbc pipeline run` all run a pipeline through, so this is
// what makes one that works from a shell write the same file when
// scheduled. A script.run node's session gets the directory too.
func TestEngineFilesDir(t *testing.T) {
	te := newTestEngine(t)
	files := t.TempDir()
	te.cfg.FilesDir = files
	spec, err := pipeline.Parse(`{"name": "files", "fragments": [{"name": "out", "nodes": [
	  {"id": "src", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT id, name FROM cats ORDER BY id LIMIT 3"}},
	  {"id": "dst", "plugin": "csv.write", "cfg": {"path": "engine-exports/cats.csv"}}
	], "edges": [["src", "dst"]]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	head, err := te.StartPipeline(Request{Spec: spec, Trigger: TriggerSchedule})
	if err != nil {
		t.Fatal(err)
	}
	fin := te.wait(t, head.ID)
	if fin.Status != pipeline.Succeeded {
		t.Fatalf("final = %+v", fin)
	}
	want := filepath.Join(files, "engine-exports", "cats.csv")
	if got := fin.Pipelines[0].Fragments[0].Vars["path"]; got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
	if bs, err := os.ReadFile(want); err != nil || strings.Count(string(bs), "\n") != 4 {
		t.Errorf("file: %q %v", bs, err)
	}
	if _, err := os.Stat("engine-exports"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the working directory got engine-exports: %v", err)
	}
	if got := te.paths().FilesDir; got != files {
		t.Errorf("a node's session FilesDir = %q", got)
	}
}

// A preview writes nothing: each preview sink's rows come as a Preview
// event carrying the run's origin, and the action fragment is skipped.
func TestEnginePreview(t *testing.T) {
	te := newTestEngine(t)
	head, err := te.StartPipeline(Request{Spec: example(t, "clean-and-load.json"), PreviewRows: 3, Origin: "tab9"})
	if err != nil {
		t.Fatal(err)
	}
	if head.Trigger != TriggerPreview || head.Preview != 3 {
		t.Errorf("header = %+v", head)
	}
	fin := te.wait(t, head.ID)
	if fin.Status != pipeline.Succeeded || fin.Pipelines[0].Fragments[1].Status != pipeline.Skipped {
		t.Fatalf("final = %+v", fin.Pipelines[0].Fragments)
	}
	var shown []*Preview
	for _, e := range te.events(head.ID) {
		if p, ok := e.(*Preview); ok {
			shown = append(shown, p)
		}
	}
	// dst and peek both become preview sinks
	if len(shown) != 2 || shown[0].Origin != "tab9" || shown[0].Result.RowCount() != 3 {
		t.Fatalf("previews = %+v", shown)
	}
	if r, err := te.mgr.Run("a", "SELECT count(*) FROM sqlite_master WHERE name = 'cats_clean'"); err != nil || r.Rows[0][0] != "0" {
		t.Errorf("a preview created the table: %v %v", r, err)
	}
}

// blocking is a pipeline whose source waits for the run to be stopped:
// a run that is running for as long as the test needs.
func blocking(name string, started chan<- struct{}) *pipeline.Spec {
	p := pipeline.New(name)
	f := p.Fragment("wait")
	f.Func(func(e *pipeline.Env) (*pipeline.Batch, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-e.Ctx.Done()
		return nil, e.Ctx.Err()
	})
	f.Then("sql.write", pipeline.Config{"conn": "b", "table": "never", "create": "true"})
	return p.Spec()
}

// One run per origin and one real run per pipeline: a second is refused
// as ErrBusy, a preview of a running pipeline from another origin is not.
// Cancel stops the run, which ends canceled with its sink rolled back.
func TestEngineBusyAndCancel(t *testing.T) {
	te := newTestEngine(t)
	started := make(chan struct{}, 1)
	head, err := te.StartPipeline(Request{Spec: blocking("slow", started), Origin: "tab1"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err = te.StartPipeline(Request{Spec: example(t, "cats-report.json"), Origin: "tab1"}); !errors.Is(err, ErrBusy) {
		t.Errorf("same origin: %v", err)
	}
	if _, err = te.StartPipeline(Request{Spec: blocking("slow", nil), Origin: "tab2"}); !errors.Is(err, ErrBusy) ||
		!strings.Contains(err.Error(), "slow is already running") {
		t.Errorf("same pipeline: %v", err)
	}
	pv, err := te.StartPipeline(Request{Spec: blocking("slow", nil), Origin: "tab2", PreviewRows: 5})
	if err != nil {
		t.Errorf("a preview beside a run: %v", err)
	}
	if got := te.Running(); len(got) != 2 {
		t.Errorf("running = %d", len(got))
	}
	if err = te.Cancel(head.ID); err != nil {
		t.Fatal(err)
	}
	fin := te.wait(t, head.ID)
	if fin.Status != pipeline.Canceled || !strings.Contains(fin.Error, "canceled") {
		t.Errorf("final = %s %q", fin.Status, fin.Error)
	}
	if fin.Log[len(fin.Log)-1].Level != "info" || !strings.HasPrefix(fin.Log[len(fin.Log)-1].Text, "pipeline slow stopped") {
		t.Errorf("last line = %+v", fin.Log[len(fin.Log)-1])
	}
	if r, _ := te.mgr.Run("b", "SELECT count(*) FROM sqlite_master WHERE name = 'never'"); r.Rows[0][0] != "0" {
		t.Errorf("the canceled sink created its table")
	}
	_ = te.Cancel(pv.ID)
	te.wait(t, pv.ID)
	if err = te.Cancel("nope"); err == nil {
		t.Error("cancel of an unknown run")
	}
	// a finished run's cancel lost the race: not an error
	if err = te.Cancel(head.ID); err != nil {
		t.Errorf("cancel after the end: %v", err)
	}
}

// A fragment that fails stops the pipeline: the record marks the ones
// after it skipped (nodes still listed), and the failure is an error line.
func TestEngineFailureSkipsTheRest(t *testing.T) {
	te := newTestEngine(t)
	spec, err := pipeline.Parse(`{"name": "bad", "fragments": [
	  {"name": "one", "nodes": [{"id": "x", "plugin": "sql.exec", "cfg": {"conn": "a", "sql": "SELECT * FROM no_such_table"}}]},
	  {"name": "two", "nodes": [{"id": "y", "plugin": "sql.exec", "cfg": {"conn": "a", "sql": "SELECT 1"}}]}
	]}`)
	if err != nil {
		t.Fatal(err)
	}
	head, err := te.StartPipeline(Request{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	fin := te.wait(t, head.ID)
	frags := fin.Pipelines[0].Fragments
	if fin.Status != pipeline.Failed || len(frags) != 2 || frags[0].Status != pipeline.Failed ||
		frags[1].Status != pipeline.Skipped || len(frags[1].Nodes) != 1 {
		t.Fatalf("final = %s %+v", fin.Status, frags)
	}
	last := fin.Log[len(fin.Log)-1]
	if last.Level != "err" || !strings.Contains(last.Text, "pipeline bad failed") || !strings.Contains(last.Text, "no_such_table") {
		t.Errorf("last line = %+v", last)
	}
	// the context serr carried says where: the fragment and the node
	if !strings.Contains(last.Text, "fragment one") || !strings.Contains(last.Text, "node x") ||
		!strings.Contains(fin.Error, "node x") || strings.Contains(fin.Error, "location") {
		t.Errorf("no context in %q / %q", last.Text, fin.Error)
	}
}

// A spec Check refuses is refused before anything runs; Close cancels what
// runs and then refuses new starts.
func TestEngineRefusesAndCloses(t *testing.T) {
	te := newTestEngine(t)
	spec, _ := pipeline.Parse(`{"name": "x", "fragments": [{"name": "f", "nodes": [{"id": "n", "plugin": "no.such"}]}]}`)
	if _, err := te.StartPipeline(Request{Spec: spec}); err == nil || !strings.Contains(err.Error(), "no plugin") {
		t.Errorf("bad spec: %v", err)
	}
	started := make(chan struct{}, 1)
	head, err := te.StartPipeline(Request{Spec: blocking("slow", started)})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if late := te.Close(5 * time.Second); late {
		t.Error("close gave up")
	}
	if r, _ := te.Get(head.ID); r.Status != pipeline.Canceled {
		t.Errorf("after close: %s", r.Status)
	}
	if _, err = te.StartPipeline(Request{Spec: example(t, "cats-report.json")}); !errors.Is(err, ErrClosed) {
		t.Errorf("start after close: %v", err)
	}
}
