// Package jobs runs pipelines and jobs outside any workspace: one engine
// per process, which several runs share at once, with a record of each run
// (its pipelines, their fragments' node counters, its log) kept in memory
// and on disk, and a stream of events a host turns into its own messages.
//
// WHY NOT A WORKSPACE'S RUN SLOT. A query tab runs one thing at a time and
// is busy while it does; a pipeline can run for an hour, and a load that
// ties up the tab it was started from — or that dies with it — is the
// wrong shape. So a run belongs to the engine, which the host owns for its
// whole life (dbc web's Server, `dbc job run`), and the tab that started it
// is only an Origin: where its preview results are shown, and what its
// Stop stops.
//
//	host ──StartPipeline / StartJob──► Engine ──go──► pipeline.Run, each on its own sdb.S
//	  ▲                                  │                    │
//	  │                                  │ State    (a run's or a job step's state)
//	  │                                  │ Progress (per batch, coalesced)
//	  │                                  │ Logged   (the run's log, s.Print)
//	  └──────────── Sink(Event) ◄────────┤ Preview  (a preview sink's rows)
//	                                     │ RunStarted / RunDone
//	host ──Cancel(id)──► the run's context ──► the runners stop, every sink rolls back
//
// A run is one of two kinds:
//
//	pipeline   one pipeline (StartPipeline): the pipeline tab's Run and
//	           Preview, `dbc pipeline run` through a host's engine
//	job        a DAG of pipelines (StartJob, job.go): one root, fan-out
//	           and fan-in, a failure policy, run by hand, on a schedule
//	           (scheduler.go), from a webhook or from a script
//
// Either way the Run record has one PipelineRun per pipeline, so a host
// draws both from the same shape. Records go to disk (records.go) when the
// engine has a runs directory — at the start, every few seconds while
// running, and at the end — so another process can list them and a crash
// leaves a partial record, which the next engine marks interrupted.
// Previews are never written: they change nothing and come by the dozen.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/pipeline"
	// the Go-code plugins (go.transform, go.source, go.action, script.run)
	// register in package script's init: a host running pipelines through
	// the engine must have them, whatever else it imports
	_ "github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/sdb"
)

// Kinds of run.
const (
	KindPipeline = "pipeline"
	KindJob      = "job"
)

// Triggers: what started a run.
const (
	TriggerManual   = "manual"   // a person: a tab's Run, the jobs API from the page
	TriggerPreview  = "preview"  // the Preview button: nothing is written
	TriggerSchedule = "schedule" // a cron line, fired by dbc web's scheduler
	TriggerWebhook  = "webhook"  // POST /api/v1/jobs/:name/run with the Bearer secret
	TriggerScript   = "script"   // a script's s.RunJob
	TriggerCLI      = "cli"      // dbc job run, from a shell or cron
)

// Run is one run's record: what was run, by what, and how it went, down to
// each fragment's node counters. Live, it is filled in as the run goes;
// finished, it is the outcome. Hosts get copies (Engine.Get, events), never
// the engine's own.
type Run struct {
	ID   string `json:"id"` // 20261009-020000-7f3a: sorts by start, unique enough per machine
	Kind string `json:"kind"`
	Name string `json:"name"` // the pipeline's, or the job's
	// Source is where the spec came from, as the host names it — the
	// pipeline or job file's name ("orders.json") — so a host can tie the
	// run to what edits that file; "" when it was not given.
	Source  string `json:"source,omitempty"`
	Trigger string `json:"trigger"`
	// By says more about the trigger: the cron line that fired, the
	// script that called s.RunJob, the webhook's caller.
	By       string            `json:"by,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
	Fragment string            `json:"fragment,omitempty"` // only this fragment ran
	Preview  int               `json:"preview,omitempty"`  // rows per preview; 0 for a real run
	// Origin is the host's own tag for where the run was asked from — in
	// dbc web the query tab — so its preview results land there and its
	// Stop finds it. The engine only compares it.
	Origin string `json:"origin,omitempty"`
	// PID and Host name the process running the run, when that process's
	// interrupt stops it and nothing else — a headless `dbc job run` or
	// `dbc pipeline run` (Options.Signalable). Another process can then
	// stop the run though it has no hold on it (Cancel, `dbc run cancel`).
	// 0 and "" for dbc web's and the TUI's runs: their interrupt would
	// stop far more than one run.
	PID     int             `json:"pid,omitempty"`
	Host    string          `json:"host,omitempty"`
	Started time.Time       `json:"started"`
	Ended   time.Time       `json:"ended"`
	Status  pipeline.Status `json:"status"`
	Error   string          `json:"error,omitempty"`
	// Pipelines is the run's pipelines: one for a pipeline run; a job's
	// steps in its spec's order, each queued from the start.
	Pipelines []PipelineRun `json:"pipelines"`
	// Log is the run's log, oldest first, the newest MaxLogLines kept.
	// Left out of the events (a host already has the lines one by one).
	Log []Line `json:"log,omitempty"`
}

// PipelineRun is one pipeline's part of a run. ID is its step id in a job,
// and the pipeline's name for a bare pipeline run; After and Params are a
// job step's (the params as substituted when it started).
type PipelineRun struct {
	ID     string            `json:"id"`
	After  []string          `json:"after,omitempty"`
	Params map[string]string `json:"params,omitempty"`
	pipeline.RunStats
}

// Line is one line of a run's log. Pipeline is the PipelineRun it came
// from — a job's step — or "" for the run's own lines.
type Line struct {
	At       time.Time `json:"at"`
	Level    string    `json:"level"` // "info", "err"
	Pipeline string    `json:"pipeline,omitempty"`
	Text     string    `json:"text"`
}

// MaxLogLines bounds a run's kept log; the host saw every line as it came.
const MaxLogLines = 2000

// Request is what StartPipeline runs.
type Request struct {
	Spec        *pipeline.Spec
	Params      map[string]string
	Fragment    string // only this fragment; "" for all
	PreviewRows int    // > 0: a preview of that many rows (pipeline.Options.PreviewRows)
	Trigger     string // TriggerManual when ""
	By          string // see Run.By
	Origin      string // see Run.Origin
	Source      string // see Run.Source
}

// Events. Every event names its run; the engine sends one run's events in
// the order they happened (one lock per run orders them), from the run's
// goroutines or its progress ticker — never while holding the engine's own
// lock, so a Sink may call back into the engine (Get, Running).
type Event interface{ RunID() string }

// RunStarted: a run was accepted. Run is its header, every pipeline and
// fragment listed queued — and for a job queued behind another run of it
// (overlap: queue), its own status queued too.
type RunStarted struct{ Run Run }

// State: a run, or one of its pipelines, changed state. Pipeline "" is the
// run itself (a queued job run starting); otherwise the PipelineRun's ID —
// a job step starting, ending, or skipped because one before it failed.
type State struct {
	Run      string          `json:"run"`
	Pipeline string          `json:"pipeline,omitempty"`
	Status   pipeline.Status `json:"status"`
	Error    string          `json:"error,omitempty"`
}

// Progress: a fragment's counters, or its state, moved. A state change
// (running, succeeded, …) is sent at once; batch counters at most every
// Options.ProgressEvery per run, the latest only.
type Progress struct {
	Run      string                 `json:"run"`
	Pipeline string                 `json:"pipeline"` // the PipelineRun's ID
	Fragment pipeline.FragmentStats `json:"fragment"`
}

// Logged: a line of the run's log — the runner's own, a node's Logf, a Go
// node's s.Print, the DDL log, a job's step starting.
type Logged struct {
	Run      string `json:"run"`
	Pipeline string `json:"pipeline"`
	Line     Line   `json:"line"`
}

// Preview: a preview sink (or any node calling s.Show) handed over rows.
// Origin is the run's, for the host to land them where they were asked for.
type Preview struct {
	Run      string
	Origin   string
	Pipeline string
	Result   *model.Result
}

// Notice: something the engine has to say that belongs to no run — a
// scheduled start skipped because the last one still runs, a job file that
// no longer parses, a record that could not be written. RunID is "" (or
// the run's, for a record).
type Notice struct {
	Run   string `json:"run,omitempty"`
	Job   string `json:"job,omitempty"`
	Level string `json:"level"` // "info", "warn", "err"
	Text  string `json:"text"`
}

// RunDone: the run ended; Run is its final record, without the log.
type RunDone struct{ Run Run }

func (e *RunStarted) RunID() string { return e.Run.ID }
func (e *State) RunID() string      { return e.Run }
func (e *Progress) RunID() string   { return e.Run }
func (e *Logged) RunID() string     { return e.Run }
func (e *Preview) RunID() string    { return e.Run }
func (e *Notice) RunID() string     { return e.Run }
func (e *RunDone) RunID() string    { return e.Run.ID }

// Options configure an Engine.
type Options struct {
	// Sink receives every event; nil drops them.
	Sink func(Event)
	// Keep is how many finished runs stay in memory for Get and Recent;
	// 0 means 50. (Older ones are read back from RunsDir.)
	Keep int
	// ProgressEvery is the coalescing period for batch counters; 0 means
	// 250ms, the hosts' tick.
	ProgressEvery time.Duration
	// RunsDir, when set, is where each run's record is written
	// (userdata.SaveRun); "" keeps records in memory only. RunsKeep is how
	// many records of each job and pipeline stay there; 0 means
	// config.DefaultRunsKeep.
	RunsDir  string
	RunsKeep int
	// FlushEvery is how often a live run's record is rewritten; 0 means
	// 2s. It is also the heartbeat by which another process tells a live
	// record from a dead one: see StaleAfter.
	FlushEvery time.Duration
	// StaleAfter is how long a "running" record may go unwritten before it
	// is taken for one whose process is gone (Recover, listings); 0 means
	// 30s, fifteen missed flushes.
	StaleAfter time.Duration
	// Find resolves a job step's pipeline by name; nil is
	// PipelineFinder(cfg.PipelinesDir): the user's file, then an example.
	Find func(name string) (*pipeline.Spec, error)
	// Signalable says the process exists to run this engine's runs and an
	// interrupt (Ctrl+C, SIGINT) stops them, rolling back: a headless
	// `dbc job run` or `dbc pipeline run`. Each record then names the
	// process (Run.PID, Run.Host), so another process's Cancel can stop
	// the run by interrupting it (signal.go).
	Signalable bool
}

// ErrBusy is a start refused because what it would run is running: the
// same pipeline, the same job (overlap: skip), or another run from the
// same origin. ErrClosed is a start after Close. Hosts map ErrBusy (and
// ErrElsewhere) to "conflict".
var (
	ErrBusy   = errors.New("already running")
	ErrClosed = errors.New("the engine is shutting down")
	// ErrElsewhere is a Cancel of a run another process is running.
	ErrElsewhere = errors.New("not this process's run")
)

// busyError is a start refused as ErrBusy, in words that say what is
// running, and which run that is (BusyRun).
type busyError struct{ msg, run string }

func (e *busyError) Error() string        { return e.msg }
func (e *busyError) Is(target error) bool { return target == ErrBusy }

// BusyRun is the id of the run an ErrBusy refusal ran into, or "".
func BusyRun(err error) string {
	var b *busyError
	if errors.As(err, &b) {
		return b.run
	}
	return ""
}

// Engine runs pipelines and jobs, several at once, outside any workspace.
type Engine struct {
	cfg *config.Config
	mgr *db.Manager
	opt Options

	mu     sync.Mutex
	live   map[string]*liveRun
	done   []*Run // finished, oldest first, at most opt.Keep
	closed bool
	wg     sync.WaitGroup
	// pipes is the pipelines a real run holds now, by spec name, to the
	// run (and step) holding it: one load of a pipeline at a time,
	// whether it was started alone or as a job's step
	pipes map[string]holder
	// jobLast is each job's latest accepted live run — running, or queued
	// behind the one before (overlap: queue)
	jobLast map[string]*liveRun
}

// holder is who holds a pipeline: a run, and in a job the step.
type holder struct{ run, step, job string }

// liveRun is a run in flight.
type liveRun struct {
	id     string // the record's ID, fixed at the start
	cancel context.CancelFunc
	done   chan struct{} // closed once the run has ended and its record is final

	// mu guards rec, which the run's goroutines fill and Get copies
	mu  sync.Mutex
	rec Run

	// emitMu orders the run's events, and guards the progress coalescing:
	// pending is the latest snapshot per pipeline and fragment not sent
	// yet, sent the state last sent for each
	emitMu  sync.Mutex
	pending map[string]*Progress
	sent    map[string]pipeline.Status
	// saveFailed is set (under mu) after a record write failed and was
	// reported, so a run on a full disk says so once, not every flush
	saveFailed bool
}

func newLive(id string, cancel context.CancelFunc) *liveRun {
	return &liveRun{id: id, cancel: cancel, done: make(chan struct{}),
		pending: map[string]*Progress{}, sent: map[string]pipeline.Status{}}
}

// New makes an engine over the host's config and connections. It starts
// nothing; a host with a runs directory calls Recover once to settle the
// records a crash left behind.
func New(cfg *config.Config, mgr *db.Manager, opt Options) *Engine {
	if opt.Keep <= 0 {
		opt.Keep = 50
	}
	if opt.ProgressEvery <= 0 {
		opt.ProgressEvery = 250 * time.Millisecond
	}
	if opt.RunsKeep <= 0 {
		opt.RunsKeep = config.DefaultRunsKeep
	}
	if opt.FlushEvery <= 0 {
		opt.FlushEvery = 2 * time.Second
	}
	if opt.StaleAfter <= 0 {
		opt.StaleAfter = 30 * time.Second
	}
	if opt.Find == nil {
		opt.Find = PipelineFinder(cfg.PipelinesDir)
	}
	return &Engine{cfg: cfg, mgr: mgr, opt: opt, live: map[string]*liveRun{},
		pipes: map[string]holder{}, jobLast: map[string]*liveRun{}}
}

// StartPipeline starts a run of req.Spec and returns its header at once;
// the run goes on in the background and reports through the Sink.
//
// Refused, with nothing started: a spec Check finds errors in (the error
// lists them), a second real run of a pipeline already running — alone or
// as a job's step: two loads into the same table — and a second run from
// an origin that has one going (a tab's grid and Stop belong to one run at
// a time). Previews of a running pipeline are allowed: they write nothing.
func (e *Engine) StartPipeline(req Request) (Run, error) {
	if req.Spec == nil {
		return Run{}, serr.New("no pipeline to run")
	}
	if diags := pipeline.Check(req.Spec, pipeline.CheckOptions{}); pipeline.HasError(diags) {
		return Run{}, serr.New("the pipeline does not check out: "+errorDiags(diags), "pipeline", req.Spec.Name)
	}
	if req.Trigger == "" {
		req.Trigger = TriggerManual
	}
	if req.PreviewRows > 0 {
		req.Trigger = TriggerPreview
	}
	// the spec is the caller's: the run keeps its own copy, so an edit on
	// the canvas mid-run cannot reach a running fragment
	spec := req.Spec.Clone()
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	lr := newLive(newID(now), cancel)
	lr.rec = Run{
		ID: lr.id, Kind: KindPipeline, Name: spec.Name, Source: req.Source, Trigger: req.Trigger, By: req.By,
		Params: maps.Clone(req.Params), Fragment: req.Fragment, Preview: req.PreviewRows,
		Origin: req.Origin, Started: now, Status: pipeline.Running,
		Pipelines: []PipelineRun{{ID: spec.Name, RunStats: queued(spec, now, req.PreviewRows > 0)}},
	}
	e.stampProc(&lr.rec)

	e.mu.Lock()
	if err := e.admitLocked(req.Origin); err != nil {
		e.mu.Unlock()
		cancel()
		return Run{}, err
	}
	if req.PreviewRows == 0 {
		if err := e.holdLocked(spec.Name, holder{run: lr.id}); err != nil {
			e.mu.Unlock()
			cancel()
			return Run{}, err
		}
	}
	e.live[lr.id] = lr
	e.wg.Add(1)
	e.mu.Unlock()

	head := lr.header()
	e.emit(lr, &RunStarted{Run: head})
	go e.execute(ctx, lr, spec, req)
	return head, nil
}

// admitLocked refuses a start the engine cannot take: after Close, or from
// an origin with a run going. Under e.mu.
func (e *Engine) admitLocked(origin string) error {
	if e.closed {
		return ErrClosed
	}
	if origin == "" {
		return nil
	}
	for _, other := range e.live {
		if o := other.header(); o.Origin == origin {
			return &busyError{msg: fmt.Sprintf("this tab is still running %s %s — stop it first", what(o), o.Name), run: o.ID}
		}
	}
	return nil
}

// holdLocked takes pipeline name for h, or refuses: one real run of a
// pipeline at a time. Under e.mu.
func (e *Engine) holdLocked(name string, h holder) error {
	cur, held := e.pipes[name]
	if !held {
		e.pipes[name] = h
		return nil
	}
	since := ""
	if lr := e.live[cur.run]; lr != nil {
		since = " (since " + lr.header().Started.Format("15:04:05") + ")"
	}
	if cur.job != "" {
		return &busyError{msg: fmt.Sprintf("%s is already running as step %s of job %s%s — one run of a pipeline at a time",
			name, cur.step, cur.job, since), run: cur.run}
	}
	return &busyError{msg: fmt.Sprintf("%s is already running%s — one run of a pipeline at a time", name, since), run: cur.run}
}

// release lets go of pipeline name if h holds it.
func (e *Engine) release(name string, h holder) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pipes[name] == h {
		delete(e.pipes, name)
	}
}

// errorDiags joins a check's errors for a refusal's message.
func errorDiags(diags []pipeline.Diag) string {
	var msgs []string
	for _, d := range diags {
		if d.Severity == pipeline.SevError {
			msgs = append(msgs, d.String())
		}
	}
	return strings.Join(msgs, "; ")
}

// what names a run's kind for a message.
func what(r Run) string {
	switch {
	case r.Preview > 0:
		return "a preview of"
	case r.Kind == KindJob:
		return "the job"
	}
	return "the pipeline"
}

// queued is a pipeline's stats before it runs: every fragment queued with
// its nodes listed, so a host draws the whole pipeline from the first
// event, not only the fragments reached so far.
func queued(spec *pipeline.Spec, now time.Time, preview bool) pipeline.RunStats {
	st := pipeline.RunStats{Pipeline: spec.Name, Status: pipeline.Running, Started: now, Preview: preview}
	for _, f := range spec.Fragments {
		fs := pipeline.FragmentStats{Name: f.Name, Status: pipeline.Queued}
		for _, n := range f.Nodes {
			fs.Nodes = append(fs.Nodes, pipeline.NodeStats{ID: n.ID, Plugin: n.Plugin})
		}
		st.Fragments = append(st.Fragments, fs)
	}
	return st
}

// execute is a pipeline run's goroutine: the pipeline on its own session,
// then the record made final.
func (e *Engine) execute(ctx context.Context, lr *liveRun, spec *pipeline.Spec, req Request) {
	defer e.wg.Done()
	stop := e.background(lr)
	st, err := e.runOne(ctx, lr, 0, spec, pipeline.Options{
		Params: req.Params, Fragment: req.Fragment, PreviewRows: req.PreviewRows,
		Run: e.runVars(lr, ""),
	}, req.Origin)
	stop()
	status := pipeline.Failed
	if st != nil && st.Status != "" && st.Status != pipeline.Running {
		status = st.Status
	}
	msg := ""
	if err != nil {
		msg = errText(err)
	}
	e.finish(lr, status, msg)
}

// runVars is the ${run.…} values of a run's pipelines: its id and trigger,
// its start's date and time (the run's, not each step's, so every step of
// a job sees the same day), and the job's name when it is one.
func (e *Engine) runVars(lr *liveRun, job string) map[string]string {
	h := lr.header()
	t := h.Started.Local()
	return map[string]string{
		"id": h.ID, "trigger": h.Trigger, "job": job,
		"date": t.Format("2006-01-02"), "time": t.Format("15:04:05"), "started": t.Format(time.RFC3339),
	}
}

// runOne runs one pipeline of a run — the run's only one, or a job's step
// at index idx — on its own session over the shared connections, and puts
// its outcome into the record. The session is a script's: Query, Print
// and Show for Go nodes, the DDL log on (as script.Run turns it on for
// every script), and Release at the end, so a node that failed mid-load
// never leaves a Writer's transaction open on a pooled connection.
func (e *Engine) runOne(ctx context.Context, lr *liveRun, idx int, spec *pipeline.Spec, opt pipeline.Options, origin string) (*pipeline.RunStats, error) {
	lr.mu.Lock()
	pid := lr.rec.Pipelines[idx].ID
	preview := lr.rec.Preview > 0
	lr.mu.Unlock()
	line := func(level, text string) { e.line(lr, pid, level, text) }
	s := sdb.New(e.mgr,
		func(r *model.Result) {
			if r != nil {
				e.emit(lr, &Preview{Run: lr.id, Origin: origin, Pipeline: pid, Result: r})
			}
		},
		func(msg string) { line("info", msg) },
	).WithContext(ctx).WithPaths(e.paths())
	s.LogDDL()

	opt.Log = func(text string) { line("info", text) }
	opt.Progress = func(f pipeline.FragmentStats) { e.progress(lr, idx, pid, f) }
	// one files_dir for every run, so a relative path means the same file
	// for a run started from a shell, dbc web, the scheduler or dbc.app —
	// not each process's working directory
	if opt.FilesDir == "" {
		opt.FilesDir = e.cfg.FilesDir
	}
	st, err := pipeline.Run(ctx, s, spec, opt)
	s.Release()
	if err != nil {
		// the runner logs a success's summary itself; a failure's is ours
		level, verb := "err", "failed"
		if st != nil && st.Status == pipeline.Canceled {
			level, verb = "info", "stopped"
		}
		kind := "pipeline"
		if preview {
			kind = "preview of"
		}
		line(level, fmt.Sprintf("%s %s %s: %s", kind, spec.Name, verb, errText(err)))
	}

	lr.mu.Lock()
	p := &lr.rec.Pipelines[idx]
	if st != nil {
		final := *st
		final.Fragments = mergeFragments(p.Fragments, st.Fragments)
		p.RunStats = final
	} else {
		p.Status = pipeline.Failed
	}
	if p.Status == "" || p.Status == pipeline.Running {
		p.Status = pipeline.Failed
	}
	if err != nil && p.Error == "" {
		p.Error = errText(err)
	}
	if p.Ended.IsZero() {
		p.Ended = time.Now()
	}
	lr.mu.Unlock()
	return st, err
}

// paths is where a run's sessions resolve names, and where a script run by
// a script.run node would find its jobs (s.RunJob inside a node).
func (e *Engine) paths() sdb.Paths {
	return sdb.Paths{ScriptsDir: e.cfg.ScriptsDir, PipelinesDir: e.cfg.PipelinesDir,
		JobsDir: e.cfg.JobsDir, RunsDir: e.opt.RunsDir, FilesDir: e.cfg.FilesDir}
}

// line appends a line to the run's log and sends it.
func (e *Engine) line(lr *liveRun, pid, level, text string) {
	l := Line{At: time.Now(), Level: level, Pipeline: pid, Text: text}
	lr.mu.Lock()
	lr.rec.Log = append(lr.rec.Log, l)
	if over := len(lr.rec.Log) - MaxLogLines; over > 0 {
		lr.rec.Log = slices.Delete(lr.rec.Log, 0, over)
	}
	lr.mu.Unlock()
	e.emit(lr, &Logged{Run: lr.id, Pipeline: pid, Line: l})
}

// finish makes a run's record final: its status and error, the record
// written (and the older ones of its name pruned), its place in memory
// moved from live to done, what it held let go, RunDone sent — and only
// then done closed, so whoever Waits sees the event has gone out. A Sink
// calling Wait from RunDone does not block, as the run has left e.live.
func (e *Engine) finish(lr *liveRun, status pipeline.Status, msg string) {
	lr.mu.Lock()
	lr.rec.Ended = time.Now()
	lr.rec.Status = status
	if msg != "" {
		lr.rec.Error = msg
	}
	final := lr.rec.clone()
	lr.mu.Unlock()
	if e.recording(&final) {
		e.save(lr, &final)
		e.prune(final.Kind, final.Name)
	}

	e.mu.Lock()
	delete(e.live, lr.id)
	for name, h := range e.pipes {
		if h.run == lr.id {
			delete(e.pipes, name)
		}
	}
	if e.jobLast[final.Name] == lr {
		delete(e.jobLast, final.Name)
	}
	e.done = append(e.done, &final)
	if over := len(e.done) - e.opt.Keep; over > 0 {
		e.done = slices.Delete(e.done, 0, over)
	}
	e.mu.Unlock()
	lr.cancel()

	out := final.clone()
	out.Log = nil
	e.emit(lr, &RunDone{Run: out})
	close(lr.done)
}

// mergeFragments is the record's fragments after the run: the runner's
// outcome for every fragment it reached, and the queued ones it never
// did — a failure stopped the pipeline before them — marked skipped, with
// their nodes still listed.
func mergeFragments(queued, ran []pipeline.FragmentStats) []pipeline.FragmentStats {
	out := slices.Clone(ran)
	for _, q := range queued[min(len(ran), len(queued)):] {
		q.Status = pipeline.Skipped
		out = append(out, q)
	}
	return out
}

// progress takes one fragment snapshot from a runner: into the record, and
// out as an event — at once when the fragment's state changed, else left
// for the ticker, which sends the latest.
func (e *Engine) progress(lr *liveRun, idx int, pid string, f pipeline.FragmentStats) {
	lr.mu.Lock()
	frags := lr.rec.Pipelines[idx].Fragments
	if i := slices.IndexFunc(frags, func(x pipeline.FragmentStats) bool { return x.Name == f.Name }); i >= 0 {
		frags[i] = f
	}
	lr.mu.Unlock()

	key := pid + "\x00" + f.Name
	ev := &Progress{Run: lr.id, Pipeline: pid, Fragment: f}
	lr.emitMu.Lock()
	defer lr.emitMu.Unlock()
	if lr.sent[key] != f.Status {
		// a newer snapshot than anything pending for it: that goes stale
		delete(lr.pending, key)
		lr.sent[key] = f.Status
		e.send(ev)
		return
	}
	lr.pending[key] = ev
}

// background starts a run's helpers — the progress ticker, and the record
// flusher when the run is recorded — and returns the func that stops them
// and waits for them to be gone, so nothing they send or write can follow
// the run's final record.
func (e *Engine) background(lr *liveRun) func() {
	stop, gone := make(chan struct{}), make(chan struct{})
	head := lr.header()
	rec := e.recording(&head)
	if rec {
		e.save(lr, &head) // at once: a queued or brand-new run is listed by others
	}
	go func() {
		defer close(gone)
		t := time.NewTicker(e.opt.ProgressEvery)
		defer t.Stop()
		var flush <-chan time.Time
		if rec {
			ft := time.NewTicker(e.opt.FlushEvery)
			defer ft.Stop()
			flush = ft.C
		}
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				lr.emitMu.Lock()
				for _, k := range slices.Sorted(maps.Keys(lr.pending)) {
					e.send(lr.pending[k])
				}
				clear(lr.pending)
				lr.emitMu.Unlock()
			case <-flush:
				lr.mu.Lock()
				snap := lr.rec.clone()
				lr.mu.Unlock()
				e.save(lr, &snap)
			}
		}
	}()
	return func() { close(stop); <-gone }
}

// emit sends one of a run's events, in the run's order.
func (e *Engine) emit(lr *liveRun, ev Event) {
	lr.emitMu.Lock()
	defer lr.emitMu.Unlock()
	e.send(ev)
}

func (e *Engine) send(ev Event) {
	if e.opt.Sink != nil {
		e.opt.Sink(ev)
	}
}

// header is a copy of the run's record without its log.
func (lr *liveRun) header() Run {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	r := lr.rec.clone()
	r.Log = nil
	return r
}

// clone copies a record deeply enough that the copy's holder and the run
// never see each other's changes.
func (r *Run) clone() Run {
	c := *r
	c.Params = maps.Clone(r.Params)
	c.Pipelines = make([]PipelineRun, len(r.Pipelines))
	for i, p := range r.Pipelines {
		cp := p
		cp.After = slices.Clone(p.After)
		cp.Params = maps.Clone(p.Params)
		cp.Fragments = make([]pipeline.FragmentStats, len(p.Fragments))
		for j, f := range p.Fragments {
			cf := f
			cf.Nodes = slices.Clone(f.Nodes)
			cf.Vars = maps.Clone(f.Vars)
			cp.Fragments[j] = cf
		}
		c.Pipelines[i] = cp
	}
	c.Log = slices.Clone(r.Log)
	return c
}

// Cancel stops a run: its context is canceled, every fragment in flight
// rolls its sinks back, a job's steps not started yet are skipped, and
// RunDone follows with status canceled. A run already finished is not an
// error (it lost the race); an unknown id is. A run another process is
// running right now is stopped by interrupting that process when its
// record names it on this machine (Run.PID: a cron's `dbc job run`), and
// is refused otherwise (ErrElsewhere) — another dbc web: this engine has
// no hold on it, and saying nothing would read as stopped.
func (e *Engine) Cancel(id string) error {
	e.mu.Lock()
	lr, live := e.live[id]
	known := live || slices.ContainsFunc(e.done, func(r *Run) bool { return r.ID == id })
	e.mu.Unlock()
	if !known {
		r, ok := e.fromDisk(id)
		if !ok {
			return serr.New("no such run", "run", id)
		}
		// fromDisk settles a record: one whose writer died reads
		// interrupted, so running or queued here has a live writer. One
		// that names its process on this machine (a `dbc job run` from a
		// shell or cron) is stopped by interrupting it: its own Ctrl+C
		// cancels the run, which rolls back and ends canceled.
		if r.Status == pipeline.Running || r.Status == pipeline.Queued {
			if signalRun(r) == nil {
				return nil
			}
			return fmt.Errorf("%w: run %s (%s %s) is running in another process — another dbc web, or a "+
				"`dbc job run` elsewhere; stop it there (Ctrl+C)", ErrElsewhere, id, r.Kind, r.Name)
		}
		return nil
	}
	if live {
		lr.cancel()
	}
	return nil
}

// Get is a run's record — live, among the finished ones kept, or read back
// from the runs directory — with its log.
func (e *Engine) Get(id string) (Run, bool) {
	e.mu.Lock()
	lr, live := e.live[id]
	var fin *Run
	if !live {
		for _, r := range e.done {
			if r.ID == id {
				fin = r
			}
		}
	}
	e.mu.Unlock()
	switch {
	case live:
		lr.mu.Lock()
		defer lr.mu.Unlock()
		return lr.rec.clone(), true
	case fin != nil:
		return fin.clone(), true
	}
	return e.fromDisk(id)
}

// Running is the live runs, newest first, without their logs.
func (e *Engine) Running() []Run {
	e.mu.Lock()
	lrs := make([]*liveRun, 0, len(e.live))
	for _, lr := range e.live {
		lrs = append(lrs, lr)
	}
	e.mu.Unlock()
	out := make([]Run, 0, len(lrs))
	for _, lr := range lrs {
		out = append(out, lr.header())
	}
	// by start, not id: ids sort by start only to the second
	slices.SortFunc(out, func(a, b Run) int { return b.Started.Compare(a.Started) })
	return out
}

// Recent is the finished runs kept in memory, newest first, without their
// logs.
func (e *Engine) Recent() []Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Run, 0, len(e.done))
	for _, fin := range slices.Backward(e.done) {
		r := fin.clone()
		r.Log = nil
		out = append(out, r)
	}
	return out
}

// Wait blocks until run id has ended (or ctx is done) and returns its final
// record. For tests, and for a host that runs one thing and exits.
func (e *Engine) Wait(ctx context.Context, id string) (Run, error) {
	e.mu.Lock()
	lr, live := e.live[id]
	e.mu.Unlock()
	if live {
		select {
		case <-lr.done:
		case <-ctx.Done():
			return Run{}, ctx.Err()
		}
	}
	r, ok := e.Get(id)
	if !ok {
		return Run{}, serr.New("no such run", "run", id)
	}
	return r, nil
}

// Close cancels every live run and waits up to grace for them to roll back,
// end and write their records; it reports whether some were still going
// when it gave up. No run starts after it.
func (e *Engine) Close(grace time.Duration) (late bool) {
	e.mu.Lock()
	e.closed = true
	for _, lr := range e.live {
		lr.cancel()
	}
	e.mu.Unlock()
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
		return false
	case <-time.After(grace):
		return true
	}
}

// errText is an error as a run's log and record say it: the core message,
// then the context the layers below attached as serr fields — which
// connection, which fragment and node — since the core alone ("unknown
// connection") does not say which. serr's own location fields are left
// out, and a pair said twice (each wrap may add one) is said once:
//
//	unknown connection — name demo-sqlite · fragment clean · node src · plugin sql.read
func errText(err error) string {
	fields := serr.SErrFromErr(err).UserFields()
	var parts []string
	seen := map[string]bool{}
	for i := 0; i+1 < len(fields); i += 2 {
		k, v := fields[i], fields[i+1]
		// "batch 0" is the batch counter before the first: no news
		if v == "" || seen[k+"\x00"+v] || (k == "batch" && v == "0") {
			continue
		}
		seen[k+"\x00"+v] = true
		parts = append(parts, k+" "+v)
	}
	if len(parts) == 0 {
		return err.Error()
	}
	return err.Error() + " — " + strings.Join(parts, " · ")
}

// newID is a run id: the start to the second, then four random hex digits,
// so ids sort by start and two runs in one second still differ.
func newID(t time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return t.Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}
