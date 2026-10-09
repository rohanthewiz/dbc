// Package jobs runs pipelines outside any workspace: one engine per
// process, which several runs share at once, with a record of each run
// (its fragments, their node counters, its log) and a stream of events a
// host turns into its own messages.
//
// WHY NOT A WORKSPACE'S RUN SLOT. A query tab runs one thing at a time and
// is busy while it does; a pipeline can run for an hour, and a load that
// ties up the tab it was started from — or that dies with it — is the
// wrong shape. So a run belongs to the engine, which the host owns for its
// whole life (dbc web's Server), and the tab that started it is only an
// Origin: where its preview results are shown, and what its Stop stops.
//
//	host ──StartPipeline(Request)──► Engine ──go──► pipeline.Run on its own sdb.S
//	  ▲                                │                 │
//	  │                                │ Progress (per batch, coalesced)
//	  │                                │ Line     (the run's log, s.Print)
//	  └───────────── Sink(Event) ◄─────┤ Preview  (a preview sink's rows)
//	                                   │ RunStarted / RunDone
//	host ──Cancel(id)──► the run's context ──► the runner stops, every sink rolls back
//
// This is the engine's first half (Phase 2 of ai_docs/plans/pipelines.md):
// single pipelines, run by hand or as a preview, their records kept in
// memory. Jobs — DAGs of pipelines, schedules, run records on disk — are
// Phase 3, and build on the Run record and events here.
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

// Kinds of run. Phase 3 adds "job".
const KindPipeline = "pipeline"

// Triggers: what started a run.
const (
	TriggerManual  = "manual"  // a person: the pipeline tab's Run
	TriggerPreview = "preview" // the Preview button: nothing is written
)

// Run is one run's record: what was run, by what, and how it went, down to
// each fragment's node counters. Live, it is filled in as the run goes;
// finished, it is the outcome. Hosts get copies (Engine.Get, events), never
// the engine's own.
type Run struct {
	ID   string `json:"id"` // 20261009-020000-7f3a: sorts by start, unique enough per machine
	Kind string `json:"kind"`
	Name string `json:"name"` // the pipeline's
	// Source is where the spec came from, as the host names it — dbc web's
	// pipeline file name ("orders.json") — so a host can tie the run to
	// what edits that file; "" when it was not given.
	Source   string            `json:"source,omitempty"`
	Trigger  string            `json:"trigger"`
	Params   map[string]string `json:"params,omitempty"`
	Fragment string            `json:"fragment,omitempty"` // only this fragment ran
	Preview  int               `json:"preview,omitempty"`  // rows per preview; 0 for a real run
	// Origin is the host's own tag for where the run was asked from — in
	// dbc web the query tab — so its preview results land there and its
	// Stop finds it. The engine only compares it.
	Origin  string          `json:"origin,omitempty"`
	Started time.Time       `json:"started"`
	Ended   time.Time       `json:"ended"`
	Status  pipeline.Status `json:"status"`
	Error   string          `json:"error,omitempty"`
	// Pipelines is the run's pipelines: one for a pipeline run; a job's,
	// in its order, once jobs exist.
	Pipelines []PipelineRun `json:"pipelines"`
	// Log is the run's log, oldest first, the newest MaxLogLines kept.
	// Left out of the events (a host already has the lines one by one).
	Log []Line `json:"log,omitempty"`
}

// PipelineRun is one pipeline's part of a run. ID is its node id in a job,
// and the pipeline's name for a bare pipeline run.
type PipelineRun struct {
	ID string `json:"id"`
	pipeline.RunStats
}

// Line is one line of a run's log.
type Line struct {
	At    time.Time `json:"at"`
	Level string    `json:"level"` // "info", "err"
	Text  string    `json:"text"`
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
	Origin      string // see Run.Origin
	Source      string // see Run.Source
}

// Events. Every event names its run; the engine sends one run's events in
// the order they happened (one lock per run orders them), from the run's
// goroutine or its progress ticker — never while holding the engine's own
// lock, so a Sink may call back into the engine (Get, Running).
type Event interface{ RunID() string }

// RunStarted: a run was accepted and is starting. Run is its header, its
// fragments queued.
type RunStarted struct{ Run Run }

// Progress: a fragment's counters, or its state, moved. A state change
// (running, succeeded, …) is sent at once; batch counters at most every
// Options.ProgressEvery per run, the latest only.
type Progress struct {
	Run      string                 `json:"run"`
	Pipeline string                 `json:"pipeline"` // the PipelineRun's ID
	Fragment pipeline.FragmentStats `json:"fragment"`
}

// Logged: a line of the run's log — the runner's own, a node's Logf, a Go
// node's s.Print, the DDL log.
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

// RunDone: the run ended; Run is its final record, without the log.
type RunDone struct{ Run Run }

func (e *RunStarted) RunID() string { return e.Run.ID }
func (e *Progress) RunID() string   { return e.Run }
func (e *Logged) RunID() string     { return e.Run }
func (e *Preview) RunID() string    { return e.Run }
func (e *RunDone) RunID() string    { return e.Run.ID }

// Options configure an Engine.
type Options struct {
	// Sink receives every event; nil drops them.
	Sink func(Event)
	// Keep is how many finished runs stay in memory for Get and Runs;
	// 0 means 50. (Phase 3 keeps records on disk as well.)
	Keep int
	// ProgressEvery is the coalescing period for batch counters; 0 means
	// 250ms, the hosts' tick.
	ProgressEvery time.Duration
}

// ErrBusy is a start refused because what it would run is running: the
// same pipeline, or another run from the same origin. ErrClosed is a start
// after Close. Hosts map ErrBusy to "conflict".
var (
	ErrBusy   = errors.New("already running")
	ErrClosed = errors.New("the engine is shutting down")
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

// Engine runs pipelines, several at once, outside any workspace.
type Engine struct {
	cfg *config.Config
	mgr *db.Manager
	opt Options

	mu     sync.Mutex
	live   map[string]*liveRun
	done   []*Run // finished, oldest first, at most opt.Keep
	closed bool
	wg     sync.WaitGroup
}

// liveRun is a run in flight.
type liveRun struct {
	cancel context.CancelFunc
	done   chan struct{} // closed once the run has ended and its record is final

	// mu guards rec, which the run's goroutine fills and Get copies
	mu  sync.Mutex
	rec Run

	// emitMu orders the run's events, and guards the progress coalescing:
	// pending is the latest snapshot per fragment not sent yet, sent the
	// state last sent per fragment
	emitMu  sync.Mutex
	pending map[string]pipeline.FragmentStats
	sent    map[string]pipeline.Status
}

// New makes an engine over the host's config and connections. It starts
// nothing.
func New(cfg *config.Config, mgr *db.Manager, opt Options) *Engine {
	if opt.Keep <= 0 {
		opt.Keep = 50
	}
	if opt.ProgressEvery <= 0 {
		opt.ProgressEvery = 250 * time.Millisecond
	}
	return &Engine{cfg: cfg, mgr: mgr, opt: opt, live: map[string]*liveRun{}}
}

// StartPipeline starts a run of req.Spec and returns its header at once;
// the run goes on in the background and reports through the Sink.
//
// Refused, with nothing started: a spec Check finds errors in (the error
// lists them), a second real run of a pipeline already running — two loads
// into the same table — and a second run from an origin that has one
// going (a tab's grid and Stop belong to one run at a time). Previews of a
// running pipeline are allowed: they write nothing.
func (e *Engine) StartPipeline(req Request) (Run, error) {
	if req.Spec == nil {
		return Run{}, serr.New("no pipeline to run")
	}
	if diags := pipeline.Check(req.Spec, pipeline.CheckOptions{}); pipeline.HasError(diags) {
		var msgs []string
		for _, d := range diags {
			if d.Severity == pipeline.SevError {
				msgs = append(msgs, d.String())
			}
		}
		return Run{}, serr.New("the pipeline does not check out: "+strings.Join(msgs, "; "), "pipeline", req.Spec.Name)
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
	lr := &liveRun{cancel: cancel, done: make(chan struct{}),
		pending: map[string]pipeline.FragmentStats{}, sent: map[string]pipeline.Status{}}
	lr.rec = Run{
		ID: newID(now), Kind: KindPipeline, Name: spec.Name, Source: req.Source, Trigger: req.Trigger,
		Params: maps.Clone(req.Params), Fragment: req.Fragment, Preview: req.PreviewRows,
		Origin: req.Origin, Started: now, Status: pipeline.Running,
		Pipelines: []PipelineRun{{ID: spec.Name, RunStats: queued(spec, now, req.PreviewRows > 0)}},
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		cancel()
		return Run{}, ErrClosed
	}
	for _, other := range e.live {
		o := other.header()
		switch {
		case req.Origin != "" && o.Origin == req.Origin:
			e.mu.Unlock()
			cancel()
			return Run{}, &busyError{msg: fmt.Sprintf("this tab is still running %s %s — stop it first", what(o), o.Name), run: o.ID}
		case req.PreviewRows == 0 && o.Preview == 0 && o.Name == spec.Name:
			e.mu.Unlock()
			cancel()
			return Run{}, &busyError{msg: fmt.Sprintf("%s is already running (since %s) — one run of a pipeline at a time",
				o.Name, o.Started.Format("15:04:05")), run: o.ID}
		}
	}
	e.live[lr.rec.ID] = lr
	e.wg.Add(1)
	e.mu.Unlock()

	head := lr.header()
	e.emit(lr, &RunStarted{Run: head})
	go e.execute(ctx, lr, spec, req)
	return head, nil
}

// what names a run's kind for a message.
func what(r Run) string {
	if r.Preview > 0 {
		return "a preview of"
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

// execute is a run's goroutine: its own session over the shared
// connections, the runner, then the record made final.
func (e *Engine) execute(ctx context.Context, lr *liveRun, spec *pipeline.Spec, req Request) {
	defer e.wg.Done()
	id, pid := lr.rec.ID, lr.rec.Pipelines[0].ID
	line := func(level, text string) {
		l := Line{At: time.Now(), Level: level, Text: text}
		lr.mu.Lock()
		lr.rec.Log = append(lr.rec.Log, l)
		if over := len(lr.rec.Log) - MaxLogLines; over > 0 {
			lr.rec.Log = slices.Delete(lr.rec.Log, 0, over)
		}
		lr.mu.Unlock()
		e.emit(lr, &Logged{Run: id, Pipeline: pid, Line: l})
	}
	// The session a script gets: Query, Print and Show for Go nodes, the
	// DDL log on (as script.Run turns it on for every script), and Release
	// at the end, so a node that failed mid-load never leaves a Writer's
	// transaction open on a pooled connection.
	s := sdb.New(e.mgr,
		func(r *model.Result) {
			if r != nil {
				e.emit(lr, &Preview{Run: id, Origin: req.Origin, Pipeline: pid, Result: r})
			}
		},
		func(msg string) { line("info", msg) },
	).WithContext(ctx).WithPaths(sdb.Paths{ScriptsDir: e.cfg.ScriptsDir, PipelinesDir: e.cfg.PipelinesDir})
	s.LogDDL()

	stopTick := e.tick(lr, id, pid)
	st, err := pipeline.Run(ctx, s, spec, pipeline.Options{
		Params: req.Params, Fragment: req.Fragment, PreviewRows: req.PreviewRows,
		Log:      func(text string) { line("info", text) },
		Progress: func(f pipeline.FragmentStats) { e.progress(lr, id, pid, f) },
	})
	s.Release()
	stopTick()
	if err != nil {
		// the runner logs a success's summary itself; a failure's is ours
		level, verb := "err", "failed"
		if st != nil && st.Status == pipeline.Canceled {
			level, verb = "info", "stopped"
		}
		kind := "pipeline"
		if req.PreviewRows > 0 {
			kind = "preview of"
		}
		line(level, fmt.Sprintf("%s %s %s: %s", kind, spec.Name, verb, errText(err)))
	}

	lr.mu.Lock()
	p := &lr.rec.Pipelines[0]
	if st != nil {
		final := *st
		final.Fragments = mergeFragments(p.Fragments, st.Fragments)
		p.RunStats = final
	}
	lr.rec.Ended = time.Now()
	lr.rec.Status = p.Status
	if lr.rec.Status == "" || lr.rec.Status == pipeline.Running {
		lr.rec.Status = pipeline.Failed
	}
	if err != nil {
		lr.rec.Error = errText(err)
	}
	final := lr.rec.clone()
	lr.mu.Unlock()

	e.mu.Lock()
	delete(e.live, id)
	e.done = append(e.done, &final)
	if over := len(e.done) - e.opt.Keep; over > 0 {
		e.done = slices.Delete(e.done, 0, over)
	}
	e.mu.Unlock()
	lr.cancel()

	// RunDone before done is closed, so whoever Waits sees the event has
	// gone out; a Sink calling Wait from RunDone does not block, as the run
	// has left e.live by now
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

// progress takes one fragment snapshot from the runner: into the record,
// and out as an event — at once when the fragment's state changed, else
// left for the ticker, which sends the latest.
func (e *Engine) progress(lr *liveRun, id, pid string, f pipeline.FragmentStats) {
	lr.mu.Lock()
	frags := lr.rec.Pipelines[0].Fragments
	if i := slices.IndexFunc(frags, func(x pipeline.FragmentStats) bool { return x.Name == f.Name }); i >= 0 {
		frags[i] = f
	}
	lr.mu.Unlock()

	lr.emitMu.Lock()
	defer lr.emitMu.Unlock()
	if lr.sent[f.Name] != f.Status {
		// a newer snapshot than anything pending for it: that goes stale
		delete(lr.pending, f.Name)
		lr.sent[f.Name] = f.Status
		e.send(&Progress{Run: id, Pipeline: pid, Fragment: f})
		return
	}
	lr.pending[f.Name] = f
}

// tick sends a run's pending progress every ProgressEvery until stopped.
// The returned func stops it and waits for it to be gone, so nothing it
// sends can follow the run's RunDone.
func (e *Engine) tick(lr *liveRun, id, pid string) func() {
	stop, gone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(gone)
		t := time.NewTicker(e.opt.ProgressEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				lr.emitMu.Lock()
				names := slices.Sorted(maps.Keys(lr.pending))
				for _, n := range names {
					e.send(&Progress{Run: id, Pipeline: pid, Fragment: lr.pending[n]})
				}
				clear(lr.pending)
				lr.emitMu.Unlock()
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

// Cancel stops a running run: its context is canceled, the fragment in
// flight rolls every sink back, and RunDone follows with status canceled.
// A run already finished is not an error (it lost the race); an unknown id
// is.
func (e *Engine) Cancel(id string) error {
	e.mu.Lock()
	lr, live := e.live[id]
	known := live || slices.ContainsFunc(e.done, func(r *Run) bool { return r.ID == id })
	e.mu.Unlock()
	if !known {
		return serr.New("no such run", "run", id)
	}
	if live {
		lr.cancel()
	}
	return nil
}

// Get is a run's record — live or among the finished ones kept — with its
// log.
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
	return Run{}, false
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

// Recent is the finished runs kept, newest first, without their logs.
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
// record. For tests and for a host that runs one pipeline and exits.
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

// Close cancels every live run and waits up to grace for them to roll back
// and end; it reports whether some were still going when it gave up. No run
// starts after it.
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
