package jobs

import (
	"context"
	"encoding/json"
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
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/sdb"
	"github.com/rohanthewiz/dbc/userdata"
)

// jobEngine is a test engine whose step pipelines come from a map rather
// than a directory, so a test can hand it Go-func pipelines that block,
// fail or meet each other.
type jobEngine struct {
	*testEngine
	mu    sync.Mutex
	pipes map[string]*pipeline.Spec
}

func newJobEngine(t *testing.T, tweak func(*Options)) *jobEngine {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{MaxRows: 1000, Connections: []config.Connection{
		{Name: "a", Driver: "sqlite", DSN: filepath.Join(dir, "a.db")},
	}}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	if err := db.SeedDemo(mgr, "a"); err != nil {
		t.Fatal(err)
	}
	je := &jobEngine{testEngine: &testEngine{mgr: mgr}, pipes: map[string]*pipeline.Spec{}}
	opt := Options{ProgressEvery: time.Millisecond, FlushEvery: 5 * time.Millisecond,
		Sink: func(e Event) {
			je.testEngine.mu.Lock()
			je.evs = append(je.evs, e)
			je.testEngine.mu.Unlock()
		},
		Find: func(name string) (*pipeline.Spec, error) {
			je.mu.Lock()
			defer je.mu.Unlock()
			if s, ok := je.pipes[strings.TrimSuffix(name, ".json")]; ok {
				return s, nil
			}
			return nil, errors.New("no such pipeline")
		}}
	if tweak != nil {
		tweak(&opt)
	}
	je.Engine = New(cfg, mgr, opt)
	t.Cleanup(func() { je.Close(5 * time.Second) })
	return je
}

// act registers a pipeline of one Go action, fn.
func (je *jobEngine) act(name string, fn func(e *pipeline.Env) error) {
	p := pipeline.New(name)
	p.Fragment("f").Func(fn)
	je.mu.Lock()
	je.pipes[name] = p.Spec()
	je.mu.Unlock()
}

func job(t *testing.T, text string) *Spec {
	t.Helper()
	s, err := ParseJob(text)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// diamond is a → (b, c) → d.
const diamond = `{"name": "d", "root": "a", "pipelines": [
  {"id": "a", "pipeline": "pa"},
  {"id": "b", "pipeline": "pb", "after": ["a"]},
  {"id": "c", "pipeline": "pc", "after": ["a"]},
  {"id": "d", "pipeline": "pd", "after": ["b", "c"]}
], "policy": {"max_parallel": 2 %s}}`

// stepStates is each step's states in the order the events said them.
func stepStates(evs []Event) map[string][]pipeline.Status {
	out := map[string][]pipeline.Status{}
	for _, e := range evs {
		if s, ok := e.(*State); ok && s.Pipeline != "" {
			out[s.Pipeline] = append(out[s.Pipeline], s.Status)
		}
	}
	return out
}

// The diamond fans out — b and c run at once (each waits for the other to
// have started, so max_parallel 2 is the only way through) — and fans in:
// d starts only after both ended.
func TestJobDiamondFansOutAndIn(t *testing.T) {
	je := newJobEngine(t, nil)
	var order []string
	var omu sync.Mutex
	note := func(s string) {
		omu.Lock()
		order = append(order, s)
		omu.Unlock()
	}
	je.act("pa", func(*pipeline.Env) error { note("a"); return nil })
	var both sync.WaitGroup
	both.Add(2)
	meet := func(name string) func(e *pipeline.Env) error {
		return func(e *pipeline.Env) error {
			both.Done()
			done := make(chan struct{})
			go func() { both.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				return errors.New(name + " never saw the other middle step running")
			}
			note(name)
			return nil
		}
	}
	je.act("pb", meet("b"))
	je.act("pc", meet("c"))
	je.act("pd", func(*pipeline.Env) error { note("d"); return nil })

	head, err := je.StartJob(JobRequest{Spec: job(t, strings.Replace(diamond, "%s", "", 1)), Source: "d.json"})
	if err != nil {
		t.Fatal(err)
	}
	if head.Kind != KindJob || len(head.Pipelines) != 4 || head.Pipelines[3].Status != pipeline.Queued ||
		!slices.Equal(head.Pipelines[3].After, []string{"b", "c"}) {
		t.Fatalf("header = %+v", head)
	}
	fin := je.wait(t, head.ID)
	if fin.Status != pipeline.Succeeded || fin.Error != "" {
		t.Fatalf("final = %s %q", fin.Status, fin.Error)
	}
	if order[0] != "a" || order[3] != "d" {
		t.Errorf("order = %v", order)
	}
	for _, p := range fin.Pipelines {
		if p.Status != pipeline.Succeeded || p.Started.IsZero() || p.Ended.IsZero() {
			t.Errorf("%s = %s", p.ID, p.Status)
		}
	}
	// d's running came after both b and c succeeded, in the event order
	var seq []string
	for _, e := range je.events(head.ID) {
		if s, ok := e.(*State); ok && s.Pipeline != "" {
			seq = append(seq, s.Pipeline+":"+string(s.Status))
		}
	}
	di := slices.Index(seq, "d:running")
	if di < 0 || slices.Index(seq, "b:succeeded") > di || slices.Index(seq, "c:succeeded") > di {
		t.Errorf("states = %v", seq)
	}
	var log []string
	for _, l := range fin.Log {
		log = append(log, l.Text)
	}
	if !slices.ContainsFunc(log, func(s string) bool { return strings.HasPrefix(s, "job d succeeded in") }) {
		t.Errorf("log = %q", log)
	}
}

// finish_branches: b fails, c runs to its end, d is skipped, the job fails
// naming b. stop: b fails while c waits, and c ends canceled.
func TestJobFailurePolicies(t *testing.T) {
	for _, policy := range []string{FinishBranches, StopOnFailure} {
		t.Run(policy, func(t *testing.T) {
			je := newJobEngine(t, nil)
			je.act("pa", func(*pipeline.Env) error { return nil })
			je.act("pb", func(*pipeline.Env) error { return errors.New("b broke") })
			cDone := make(chan struct{})
			je.act("pc", func(e *pipeline.Env) error {
				defer close(cDone)
				select {
				case <-time.After(150 * time.Millisecond):
					return nil
				case <-e.Ctx.Done():
					return e.Ctx.Err()
				}
			})
			je.act("pd", func(*pipeline.Env) error { t.Error("d ran"); return nil })
			spec := job(t, strings.Replace(diamond, "%s", `, "on_failure": "`+policy+`"`, 1))
			head, err := je.StartJob(JobRequest{Spec: spec})
			if err != nil {
				t.Fatal(err)
			}
			fin := je.wait(t, head.ID)
			<-cDone
			byID := map[string]pipeline.Status{}
			for _, p := range fin.Pipelines {
				byID[p.ID] = p.Status
			}
			wantC := pipeline.Succeeded
			if policy == StopOnFailure {
				wantC = pipeline.Canceled
			}
			if fin.Status != pipeline.Failed || !strings.HasPrefix(fin.Error, "b: ") || !strings.Contains(fin.Error, "b broke") ||
				byID["b"] != pipeline.Failed || byID["c"] != wantC || byID["d"] != pipeline.Skipped {
				t.Errorf("final = %s %q %v", fin.Status, fin.Error, byID)
			}
			if got := stepStates(je.events(head.ID))["d"]; !slices.Equal(got, []pipeline.Status{pipeline.Skipped}) {
				t.Errorf("d's states = %v", got)
			}
		})
	}
}

// Overlap: under skip a second run of a running job is refused; under
// queue it waits (queued), a third is refused, and the second starts when
// the first ends.
func TestJobOverlap(t *testing.T) {
	je := newJobEngine(t, nil)
	started := make(chan struct{}, 4)
	je.act("slow", func(e *pipeline.Env) error {
		started <- struct{}{}
		<-e.Ctx.Done()
		return e.Ctx.Err()
	})
	one := `{"name": "o", "pipelines": [{"id": "s", "pipeline": "slow"}], "policy": {"overlap": "%s"}}`
	first, err := je.StartJob(JobRequest{Spec: job(t, strings.Replace(one, "%s", "skip", 1))})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err = je.StartJob(JobRequest{Spec: job(t, strings.Replace(one, "%s", "skip", 1))}); !errors.Is(err, ErrBusy) ||
		BusyRun(err) != first.ID || !strings.Contains(err.Error(), "overlap: skip") {
		t.Errorf("skip: %v", err)
	}
	queuedSpec := job(t, strings.Replace(one, "%s", "queue", 1))
	second, err := je.StartJob(JobRequest{Spec: queuedSpec})
	if err != nil || second.Status != pipeline.Queued {
		t.Fatalf("queue: %+v %v", second.Status, err)
	}
	if _, err = je.StartJob(JobRequest{Spec: queuedSpec}); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "already waiting") {
		t.Errorf("a second waiter: %v", err)
	}
	// the pipeline is held by the first run: a run of it alone is refused
	if _, err = je.StartPipeline(Request{Spec: je.pipes["slow"]}); !errors.Is(err, ErrBusy) ||
		!strings.Contains(err.Error(), "step s of job o") {
		t.Errorf("the pipeline beside the job: %v", err)
	}
	_ = je.Cancel(first.ID)
	if fin := je.wait(t, first.ID); fin.Status != pipeline.Canceled || fin.Pipelines[0].Status != pipeline.Canceled {
		t.Errorf("first = %s / %s", fin.Status, fin.Pipelines[0].Status)
	}
	<-started // the queued one is running now
	if r, _ := je.Get(second.ID); r.Status != pipeline.Running {
		t.Errorf("second = %s", r.Status)
	}
	_ = je.Cancel(second.ID)
	je.wait(t, second.ID)
	var runState []pipeline.Status
	for _, e := range je.events(second.ID) {
		if s, ok := e.(*State); ok && s.Pipeline == "" {
			runState = append(runState, s.Status)
		}
	}
	if !slices.Equal(runState, []pipeline.Status{pipeline.Running}) {
		t.Errorf("second's own states = %v", runState)
	}
}

// A timeout fails the job: the step in flight is canceled, the rest
// skipped, and the error says it timed out.
func TestJobTimeout(t *testing.T) {
	je := newJobEngine(t, nil)
	je.act("slow", func(e *pipeline.Env) error { <-e.Ctx.Done(); return e.Ctx.Err() })
	je.act("next", func(*pipeline.Env) error { return nil })
	spec := job(t, `{"name": "t", "pipelines": [{"id": "s", "pipeline": "slow"}, {"id": "n", "pipeline": "next", "after": ["s"]}],
	  "policy": {"timeout": "50ms"}}`)
	head, err := je.StartJob(JobRequest{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	fin := je.wait(t, head.ID)
	if fin.Status != pipeline.Failed || !strings.HasPrefix(fin.Error, "timed out after 50ms") ||
		fin.Pipelines[0].Status != pipeline.Canceled || fin.Pipelines[1].Status != pipeline.Skipped {
		t.Errorf("final = %s %q %s %s", fin.Status, fin.Error, fin.Pipelines[0].Status, fin.Pipelines[1].Status)
	}
}

// A step's params: the job's own params and the run's values substituted
// when it starts, recorded as substituted; a job param left unset with no
// default, or one the job does not declare, is refused up front.
func TestJobParams(t *testing.T) {
	je := newJobEngine(t, nil)
	spec, err := pipeline.Parse(`{"name": "pp", "params": {"x": {"default": ""}}, "fragments": [
	  {"name": "f", "nodes": [{"id": "n", "plugin": "sql.exec", "cfg": {"conn": "a", "sql": "SELECT '${x}'"}}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	je.pipes["pp"] = spec
	js := job(t, `{"name": "j", "params": {"day": {"default": ""}}, "pipelines": [
	  {"id": "s", "pipeline": "pp", "params": {"x": "${day}/${run.job}/${run.trigger}"}}]}`)
	if _, err = je.StartJob(JobRequest{Spec: js}); err == nil || !strings.Contains(err.Error(), "no default") {
		t.Errorf("unset param: %v", err)
	}
	if _, err = je.StartJob(JobRequest{Spec: js, Params: map[string]string{"day": "1", "nope": "2"}}); err == nil ||
		!strings.Contains(err.Error(), "not a parameter of the job") {
		t.Errorf("unknown param: %v", err)
	}
	head, err := je.StartJob(JobRequest{Spec: js, Params: map[string]string{"day": "2026-10-09"}, Trigger: TriggerCLI})
	if err != nil {
		t.Fatal(err)
	}
	fin := je.wait(t, head.ID)
	if fin.Status != pipeline.Succeeded || fin.Pipelines[0].Params["x"] != "2026-10-09/j/cli" || fin.Params["day"] != "2026-10-09" {
		t.Errorf("final = %s %q %v", fin.Status, fin.Error, fin.Pipelines[0].Params)
	}
}

// Records on disk: written at the start (running), kept fresh while the
// run goes, final at the end; a record a dead process left running reads
// interrupted, and Recover rewrites it so.
func TestJobRecords(t *testing.T) {
	runs := t.TempDir()
	je := newJobEngine(t, func(o *Options) { o.RunsDir = runs; o.StaleAfter = 200 * time.Millisecond })
	gate := make(chan struct{})
	je.act("pa", func(*pipeline.Env) error { <-gate; return nil })
	spec := job(t, `{"name": "rec", "pipelines": [{"id": "a", "pipeline": "pa"}]}`)
	head, err := je.StartJob(JobRequest{Spec: spec, Trigger: TriggerSchedule, By: "0 2 * * *"})
	if err != nil {
		t.Fatal(err)
	}
	read := func() Run {
		t.Helper()
		bs, err := os.ReadFile(filepath.Join(runs, "job", "rec", head.ID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var r Run
		if err = json.Unmarshal(bs, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	waitFor := func(cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("timed out")
			}
		}
	}
	waitFor(func() bool {
		_, err := os.Stat(filepath.Join(runs, "job", "rec", head.ID+".json"))
		return err == nil
	})
	waitFor(func() bool { return read().Pipelines[0].Status == pipeline.Running })
	if r := read(); r.Status != pipeline.Running || r.By != "0 2 * * *" {
		t.Errorf("live record = %s %q", r.Status, r.By)
	}
	// a live run is not stale however long it runs: its heartbeat holds
	time.Sleep(300 * time.Millisecond)
	if hs, _ := je.History(userdata.RunFilter{Kind: KindJob}); len(hs) != 1 || hs[0].Status != "running" {
		t.Errorf("history while live = %+v", hs)
	}
	close(gate)
	fin := je.wait(t, head.ID)
	if r := read(); r.Status != pipeline.Succeeded || len(r.Log) == 0 || r.Ended.IsZero() || fin.Status != pipeline.Succeeded {
		t.Errorf("final record = %s", r.Status)
	}

	// a dead process's record: running, written long ago
	dead := Run{ID: "20261001-020000-dead", Kind: KindJob, Name: "rec", Trigger: TriggerSchedule, Status: pipeline.Running,
		Started: time.Now().Add(-time.Hour), Pipelines: []PipelineRun{
			{ID: "a", RunStats: pipeline.RunStats{Status: pipeline.Running, Fragments: []pipeline.FragmentStats{{Name: "f", Status: pipeline.Running}}}},
			{ID: "b", RunStats: pipeline.RunStats{Status: pipeline.Queued}},
		}}
	bs, _ := json.Marshal(dead)
	if err = userdata.SaveRun(runs, KindJob, "rec", dead.ID, bs); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	p := filepath.Join(runs, "job", "rec", dead.ID+".json")
	if err = os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if r, ok := je.Get(dead.ID); !ok || r.Status != pipeline.Interrupted || r.Pipelines[0].Status != pipeline.Interrupted ||
		r.Pipelines[1].Status != pipeline.Skipped || r.Pipelines[0].Fragments[0].Status != pipeline.Interrupted {
		t.Errorf("read back = %+v", r)
	}
	hs, err := je.History(userdata.RunFilter{Kind: KindJob, Name: "rec"})
	if err != nil || len(hs) != 2 || hs[0].ID != head.ID || hs[1].Status != string(pipeline.Interrupted) {
		t.Errorf("history = %+v %v", hs, err)
	}
	if n, err := je.Recover(); n != 1 || err != nil {
		t.Errorf("recover = %d %v", n, err)
	}
	var onDisk Run
	bs, _ = os.ReadFile(p)
	_ = json.Unmarshal(bs, &onDisk)
	if onDisk.Status != pipeline.Interrupted || onDisk.Error == "" {
		t.Errorf("rewritten = %s %q", onDisk.Status, onDisk.Error)
	}
	if last, ok := je.LastRun(KindJob, "rec"); !ok || last.ID != head.ID {
		t.Errorf("last run = %+v", last)
	}
	// a preview is never recorded
	pv, err := je.StartPipeline(Request{Spec: example(t, "cats-report.json"), PreviewRows: 2})
	if err != nil {
		t.Fatal(err)
	}
	je.wait(t, pv.ID)
	if _, err := os.Stat(filepath.Join(runs, "pipeline")); !os.IsNotExist(err) {
		t.Errorf("a preview left a record: %v", err)
	}
}

// The example job checks out against the examples and runs on the demos:
// copy, then clean and breeds side by side, then the report.
func TestExampleJobRuns(t *testing.T) {
	// the two demos, as demoFallback makes them, the bytdb one in a temp
	// dir: each seeds the cats on its first open
	cfg := &config.Config{MaxRows: 1000, Connections: []config.Connection{
		{Name: config.DemoSQLite, Driver: "sqlite", DSN: "file:jobsexample?mode=memory&cache=shared", Demo: true},
		{Name: config.DemoBytdb, Driver: "bytdb", DSN: filepath.Join(t.TempDir(), "demo.bytdb"), Demo: true},
	}}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	e := New(cfg, mgr, Options{})
	t.Cleanup(func() { e.Close(5 * time.Second) })
	spec, file, err := LoadJob("", "nightly")
	if err != nil || file != "nightly.json" {
		t.Fatal(file, err)
	}
	if diags := CheckJob(spec, CheckOptions{Find: PipelineFinder("")}); len(diags) != 0 {
		t.Fatalf("diags = %v", diags)
	}
	head, err := e.StartJob(JobRequest{Spec: spec, Source: file})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fin, err := e.Wait(ctx, head.ID)
	if err != nil || fin.Status != pipeline.Succeeded {
		var log []string
		for _, l := range fin.Log {
			log = append(log, l.Text)
		}
		t.Fatalf("final = %s %q %v\n%s", fin.Status, fin.Error, err, strings.Join(log, "\n"))
	}
	if r, err := mgr.Run(config.DemoBytdb, "SELECT count(*) FROM breed_counts"); err != nil || r.Rows[0][0] == "0" {
		t.Errorf("breed_counts: %v %v", r, err)
	}
}

// s.RunJob from a script: on the engine handed to the session (its run is
// among the engine's, trigger script), and with no engine handed in on a
// private one whose lines are the script's Print lines.
func TestScriptRunsAJob(t *testing.T) {
	runs := t.TempDir()
	jobsDir := t.TempDir()
	je := newJobEngine(t, func(o *Options) { o.RunsDir = runs })
	if _, _, err := userdata.SaveJob(jobsDir, "sj.json", `{"name": "sj", "params": {"x": {"default": "1"}},
	  "pipelines": [{"id": "only", "pipeline": "copy-cats"}]}`, ""); err != nil {
		t.Fatal(err)
	}
	je.cfg.JobsDir = jobsDir
	je.pipes["copy-cats"] = example(t, "cats-report.json")
	src := `package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	run, err := s.RunJob("sj", sdb.Params{"x": "2"})
	if err != nil {
		return err
	}
	s.Print("done %s %s %d", run.Job, run.Status, len(run.Pipelines))
	return nil
}
`
	var lines []string
	var lmu sync.Mutex
	print := func(m string) { lmu.Lock(); lines = append(lines, m); lmu.Unlock() }
	s := sdb.New(je.mgr, nil, print).WithJobs(je.ScriptRunner())
	if err := script.RunSource("sj.go", src, s); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(lines, "done sj succeeded 1") {
		t.Errorf("lines = %q", lines)
	}
	hs, _ := je.History(userdata.RunFilter{Kind: KindJob, Name: "sj"})
	if len(hs) != 1 || hs[0].Trigger != TriggerScript || hs[0].Status != "succeeded" {
		t.Errorf("history = %+v", hs)
	}

	// no engine handed in: the default runner, its own engine, its lines
	// printed with their step, its record in the session's runs dir
	lines = nil
	own := t.TempDir()
	s = sdb.New(je.mgr, nil, print).WithPaths(sdb.Paths{JobsDir: jobsDir, RunsDir: own})
	_, err := s.RunJob("sj", nil)
	// copy-cats here is the real example, on demo connections this test
	// does not have: the job fails, and says where
	if err == nil || !strings.Contains(err.Error(), "job sj failed") {
		t.Fatalf("err = %v", err)
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "[only] ") }) {
		t.Errorf("own engine's lines = %q", lines)
	}
	if hs, _ := userdata.ListRuns(own, userdata.RunFilter{}); len(hs) != 1 || hs[0].Status != "failed" {
		t.Errorf("own engine's record = %+v", hs)
	}
}

// A Signalable engine names its process in each record; another does not.
func TestRecordsNameASignalableProcess(t *testing.T) {
	for _, signalable := range []bool{true, false} {
		je := newJobEngine(t, func(o *Options) { o.Signalable = signalable })
		je.act("pa", func(*pipeline.Env) error { return nil })
		head, err := je.StartJob(JobRequest{Spec: job(t, `{"name": "me", "pipelines": [{"id": "a", "pipeline": "pa"}]}`)})
		if err != nil {
			t.Fatal(err)
		}
		fin := je.wait(t, head.ID)
		if got := fin.PID == os.Getpid() && fin.Host == hostname() && fin.Host != ""; got != signalable {
			t.Errorf("signalable %v: pid %d host %q", signalable, fin.PID, fin.Host)
		}
	}
}
