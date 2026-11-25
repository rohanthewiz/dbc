package jobs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/userdata"
)

// JobRequest is what StartJob runs.
type JobRequest struct {
	Spec    *Spec
	Params  map[string]string // overrides of the job's params
	Trigger string            // TriggerManual when ""
	By      string            // see Run.By
	Origin  string            // see Run.Origin
	Source  string            // the job's file name, see Run.Source
	// ID is the run's id when the caller picked it (NewRunID): `dbc job
	// run --wait=false` prints the id of a run its detached child starts.
	// "" makes one.
	ID string
	// Resume, when set, makes the run a resume of an earlier run of the
	// job (N-339, resume.go): that run's id, or "last" for the job's
	// newest. The steps it finished, unchanged since, are reused rather
	// than run; its params are the base Params override, and its run
	// values are the ones the steps see.
	Resume string
	// Rerun is steps a resume runs again though it could reuse them —
	// their output did not last — and with each every step after it.
	// Refused without Resume, and when it names a step the job lacks.
	Rerun []string
	// Show, when set, takes the rows the run's preview sinks show, in
	// place of the Sink's Preview events: s.RunJob's script shows them as
	// its own s.Show results (runForScript, N-186). Its run has no origin
	// for a host to land them by, and the tab on screen is the script's,
	// which refuses rows while the script runs.
	Show func(*model.Result)
}

// StartJob starts a run of a job and returns its header at once; the run
// goes on in the background, step by step, and reports through the Sink.
//
// Refused, with nothing started: a job CheckJob finds errors in (its
// pipelines included, each loaded through Options.Find now — the run keeps
// those copies, so editing a pipeline mid-run reaches no step), a param the
// job does not declare or one with no default left unset, a second run
// from the same origin, and — under overlap "skip", the default — a run of
// a job that has one going. Under "queue" the new run is accepted queued
// and starts when the one before it ends; one waits at most, a third is
// refused.
//
// A resume (req.Resume) is refused too when its earlier run is not one of
// the job's, has not ended, or leaves nothing to run (prepareJob).
func (e *Engine) StartJob(req JobRequest) (Run, error) {
	jp, err := e.prepareJob(&req)
	if err != nil {
		return Run{}, err
	}
	spec, params, pipes, hashes, plan, earlier := jp.spec, jp.params, jp.pipes, jp.hashes, jp.plan, jp.earlier

	id := req.ID
	if id == "" {
		id = newID(time.Now())
	} else if !userdata.ValidRunID(id) {
		return Run{}, serr.New("not a run id", "id", id)
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	lr := newLive(id, cancel)
	lr.show = req.Show
	lr.rec = Run{
		ID: lr.id, Kind: KindJob, Name: spec.Name, Source: req.Source, Trigger: req.Trigger, By: req.By,
		Params: params, Origin: req.Origin, Started: now, Status: pipeline.Running,
	}
	e.stampProc(&lr.rec)
	if earlier != nil {
		lr.rec.Resumes, lr.rec.RunValues = earlier.ID, jp.vals
	}
	for _, st := range spec.Pipelines {
		rs := queued(pipes[st.ID], now, false)
		rs.Status, rs.Started = pipeline.Queued, time.Time{}
		lr.rec.Pipelines = append(lr.rec.Pipelines, PipelineRun{ID: st.ID, After: slices.Clone(st.After),
			Params: maps.Clone(st.Params), SpecHash: hashes[st.ID], RunStats: rs})
		if u := plan[st.ID]; u.from != nil {
			markReused(&lr.rec.Pipelines[len(lr.rec.Pipelines)-1], u)
		}
	}

	e.mu.Lock()
	if err := e.admitLocked(req.Origin); err != nil {
		e.mu.Unlock()
		cancel()
		return Run{}, err
	}
	var waitFor <-chan struct{}
	// The overlap rule across processes (held.go). With no run of the job
	// here, this run takes the job's lock; held by another process, it is
	// refused (skip) or queued (queue). A queued run, whoever it waits
	// for, also takes the job's queue lock, as the one run waiting.
	// elsewhere names the lock another process holds, refused once e.mu
	// is let go: saying who holds it reads the records.
	var elsewhere, pid string
	var warn error
	if prev := e.jobLast[spec.Name]; prev != nil {
		ph := prev.header()
		switch {
		case spec.Policy.Overlap != OverlapQueue:
			e.mu.Unlock()
			cancel()
			return Run{}, &busyError{msg: fmt.Sprintf("job %s is already running (since %s; overlap: skip)",
				spec.Name, ph.Started.Format("15:04:05")), run: ph.ID}
		case ph.Status == pipeline.Queued:
			e.mu.Unlock()
			cancel()
			return Run{}, &busyError{msg: fmt.Sprintf("a run of job %s is already waiting for the one before it (overlap: queue holds one)",
				spec.Name), run: ph.ID}
		}
		waitFor = prev.done
		lr.rec.Status = pipeline.Queued
	} else {
		var busy bool
		if busy, pid, warn = e.holdFileLocked(lockJob, spec.Name, lr.id); busy {
			if spec.Policy.Overlap != OverlapQueue {
				elsewhere = lockJob
			} else {
				lr.rec.Status = pipeline.Queued // executeJob polls for the lock
			}
		}
	}
	if elsewhere == "" && lr.rec.Status == pipeline.Queued {
		busy, qpid, qwarn := e.holdFileLocked(lockQueue, spec.Name, lr.id)
		if busy {
			elsewhere, pid = lockQueue, qpid
		}
		if warn == nil {
			warn = qwarn // the same directory: one warning says it
		}
	}
	if elsewhere != "" {
		e.dropRunFilesLocked(lr.id) // nothing taken yet, but never leave one behind
		e.mu.Unlock()
		cancel()
		return Run{}, e.jobElsewhere(spec.Name, elsewhere, pid)
	}
	queuedRun := lr.rec.Status == pipeline.Queued
	e.live[lr.id] = lr
	e.jobLast[spec.Name] = lr
	e.wg.Add(1)
	e.mu.Unlock()
	e.warnLock(lr.id, spec.Name, warn)

	head := lr.header()
	e.emit(lr, &RunStarted{Run: head})
	go e.executeJob(ctx, lr, spec, pipes, plan, queuedRun, waitFor)
	return head, nil
}

// jobPrep is what a job run is started from, settled before anything is
// taken (prepareJob).
type jobPrep struct {
	spec    *Spec                     // the run's own copy
	params  map[string]string         // the job params, settled
	pipes   map[string]*pipeline.Spec // each step's pipeline, loaded now
	hashes  map[string]string         // each step's pipeline's SpecHash
	earlier *Run                      // a resume's earlier run; nil for a fresh run
	plan    map[string]reuse          // a resume's plan; nil for a fresh run
	vals    map[string]string         // a resume's run values; nil for a fresh run
}

// prepareJob is StartJob's pre-flight, which takes and changes nothing:
// the job checked, its params settled, its pipelines loaded and hashed,
// and for a resume the earlier run found and the plan made. It fills in
// req's defaults (Trigger) and, for a resume, notes it in By.
func (e *Engine) prepareJob(req *JobRequest) (*jobPrep, error) {
	if req.Spec == nil {
		return nil, serr.New("no job to run")
	}
	if diags := CheckJob(req.Spec, CheckOptions{Find: e.opt.Find}); pipeline.HasError(diags) {
		return nil, serr.New("the job does not check out: "+errorDiags(diags), "job", req.Spec.Name)
	}
	jp := &jobPrep{spec: req.Spec.Clone(), pipes: map[string]*pipeline.Spec{}, hashes: map[string]string{}}
	spec := jp.spec
	// a resume's earlier run, found and checked before anything is taken
	switch {
	case req.Resume != "":
		r, err := e.earlierRun(spec.Name, req.Resume)
		if err != nil {
			return nil, err
		}
		jp.earlier = &r
	case len(req.Rerun) > 0:
		return nil, serr.New("rerun names steps a resume runs again: it needs a run to resume", "job", spec.Name)
	}
	for _, id := range req.Rerun {
		if spec.Step(id) == nil {
			return nil, serr.New(fmt.Sprintf("job %s has no step %s to rerun", spec.Name, id), "job", spec.Name)
		}
	}
	given := req.Params
	if jp.earlier != nil {
		given = resumeParams(spec, jp.earlier, req.Params)
	}
	var err error
	if jp.params, err = jobParams(spec, given); err != nil {
		return nil, err
	}
	for _, st := range spec.Pipelines {
		ps, err := e.opt.Find(st.Pipeline)
		if err != nil {
			return nil, serr.Wrap(err, "job", spec.Name, "step", st.ID)
		}
		jp.pipes[st.ID] = ps.Clone()
		jp.hashes[st.ID] = SpecHash(jp.pipes[st.ID])
	}
	if req.Trigger == "" {
		req.Trigger = TriggerManual
	}
	if jp.earlier == nil {
		return jp, nil
	}
	// a resume's plan: which steps it reuses, and the run values its
	// steps see (the earlier run's, the trigger its own). One that would
	// run nothing is refused: there is nothing to resume.
	jp.vals = earlierValues(jp.earlier)
	jp.vals["trigger"], jp.vals["job"] = req.Trigger, spec.Name
	jp.plan = resumePlan(spec, jp.pipes, jp.hashes, jp.earlier, jp.params, jp.vals, req.Rerun)
	if !slices.ContainsFunc(spec.Pipelines, func(st Step) bool { return jp.plan[st.ID].from == nil }) {
		return nil, serr.New(fmt.Sprintf("nothing to resume: every step of run %s succeeded and is unchanged "+
			"(--rerun STEP runs one again)", jp.earlier.ID), "job", spec.Name, "run", jp.earlier.ID)
	}
	by := "resumes " + jp.earlier.ID
	if req.By != "" {
		by = req.By + ", " + by
	}
	req.By = by
	return jp, nil
}

// CheckStart reports what StartJob would refuse req for before it takes
// anything: a job that does not check out, a param, a resume's earlier
// run or empty plan. Not what depends on the runs going (overlap, a busy
// origin). `dbc job run --wait=false` asks it before it starts its
// detached child, which would refuse with no one to tell.
func (e *Engine) CheckStart(req JobRequest) error {
	_, err := e.prepareJob(&req)
	return err
}

// jobParams settles a run's job params: the defaults, then the overrides,
// each of which must be declared; one with no default must be given.
func jobParams(spec *Spec, given map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for name, p := range spec.Params {
		out[name] = p.Default
	}
	for name, v := range given {
		if _, ok := spec.Params[name]; !ok {
			return nil, serr.New("not a parameter of the job", "job", spec.Name, "param", name)
		}
		out[name] = v
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Params)) {
		if _, set := given[name]; !set && spec.Params[name].Default == "" {
			return nil, serr.New("parameter has no default and was not given", "job", spec.Name, "param", name)
		}
	}
	return out, nil
}

// errTimedOut is the run context's cause when the policy's timeout ends it.
var errTimedOut = errors.New("timed out")

// executeJob is a job run's goroutine: the readiness walk over the DAG.
//
//	waiting[s] = how many of s's afters have not succeeded yet
//	ready      = [root]
//	loop:
//	    start ready steps while fewer than max_parallel run   (each its own goroutine)
//	    nothing running? done
//	    a step ends:
//	        succeeded → each downstream d: waiting[d]--; at 0, d is ready
//	        otherwise → every step downstream of it, transitively, is skipped;
//	                    on_failure "stop" also cancels the run, so the steps
//	                    still running end canceled
//	steps never started (a cancel, a timeout, a stop) → skipped
//
// A fan-in step starts only when every after has succeeded, so a failure
// anywhere upstream skips it; with finish_branches the branches that do not
// depend on the failure run to the end, and the job ends failed.
//
// A resume (plan non-nil, resume.go) starts the walk past the steps it
// reuses: they count as succeeded, so waiting[s] counts only the afters
// that run, and ready is every step that runs whose afters are all
// reused. A run started afresh reuses nothing, and ready is the root.
//
// A queued run (overlap: queue) first waits its turn (waitTurn): for the
// run before it here, waitFor, and for the job's lock another process may
// hold.
func (e *Engine) executeJob(ctx context.Context, lr *liveRun, spec *Spec, pipes map[string]*pipeline.Spec,
	plan map[string]reuse, queuedRun bool, waitFor <-chan struct{}) {
	defer e.wg.Done()
	stop := e.background(lr)
	if queuedRun {
		if !e.waitTurn(ctx, lr, spec.Name, waitFor) {
			e.skipRest(lr)
			stop()
			e.finish(lr, pipeline.Canceled, "canceled while it waited for the run before it")
			return
		}
		e.emit(lr, &State{Run: lr.id, Status: pipeline.Running})
	}

	// the walk's own context: a timeout or on_failure "stop" ends it with
	// a cause; a Cancel (or Close) ends its parent, ctx
	runCtx, stopRun := context.WithCancelCause(ctx)
	defer stopRun(nil)
	if d := spec.Policy.TimeoutDur(); d > 0 {
		var cancelT context.CancelFunc
		runCtx, cancelT = context.WithTimeoutCause(runCtx, d, errTimedOut)
		defer cancelT()
	}
	// the walk below waits on every step it started; one whose node does
	// not heed the stop would hold the whole job, timeout and all, so the
	// watchdog watches the walk's context — Cancel, Close, the timeout and
	// on_failure "stop" alike — and abandons the job if it outlives it
	// (N-268)
	go e.watchdog(runCtx, lr)
	root := spec.RootID()
	vars := e.runVars(lr, spec.Name)
	params := lr.header().Params
	reused := func(id string) bool { return plan[id].from != nil }
	index := map[string]int{}
	waiting := map[string]int{}
	var ready []string
	for i, st := range spec.Pipelines {
		index[st.ID] = i
		for _, a := range st.After {
			if !reused(a) {
				waiting[st.ID]++
			}
		}
	}
	for _, st := range spec.Pipelines {
		if !reused(st.ID) && waiting[st.ID] == 0 {
			ready = append(ready, st.ID)
		}
	}
	if plan == nil {
		e.line(lr, "", "info", fmt.Sprintf("job %s: %d pipelines from %s, %d at a time (%s)",
			spec.Name, len(spec.Pipelines), root, spec.Policy.Parallel(), lr.header().Trigger))
	} else {
		e.resumeLines(lr, spec, plan, ready)
	}

	type result struct {
		id     string
		status pipeline.Status
		msg    string
	}
	results := make(chan result)
	running := 0
	var firstErr string
	for {
		for len(ready) > 0 && running < spec.Policy.Parallel() && runCtx.Err() == nil {
			id := ready[0]
			ready = ready[1:]
			running++
			go func() {
				status, msg := e.runStep(runCtx, lr, index[id], spec, pipes[id], params, vars)
				results <- result{id, status, msg}
			}()
		}
		if running == 0 {
			break
		}
		r := <-results
		running--
		if r.status == pipeline.Succeeded {
			for _, d := range spec.Downstream(r.id) {
				if waiting[d]--; waiting[d] == 0 {
					ready = append(ready, d)
				}
			}
			continue
		}
		if firstErr == "" {
			firstErr = r.id + ": " + cmpOr(r.msg, string(r.status))
		}
		e.skipDownstream(lr, spec, r.id)
		if spec.Policy.OnFailure == StopOnFailure && runCtx.Err() == nil {
			e.line(lr, "", "err", fmt.Sprintf("job %s: stopping, %s did not succeed (on_failure: stop)", spec.Name, r.id))
			stopRun(fmt.Errorf("%s failed (on_failure: stop)", r.id))
		}
	}
	e.skipRest(lr)
	stop()

	status, msg := pipeline.Succeeded, ""
	switch {
	case ctx.Err() != nil:
		status, msg = pipeline.Canceled, "canceled"
	case errors.Is(context.Cause(runCtx), errTimedOut):
		status, msg = pipeline.Failed, fmt.Sprintf("timed out after %s (policy timeout)", spec.Policy.TimeoutDur())
		if firstErr != "" {
			msg += "; " + firstErr
		}
	case firstErr != "":
		status, msg = pipeline.Failed, firstErr
	}
	level, word := "info", string(status)
	if status == pipeline.Failed {
		level = "err"
	}
	h := lr.header()
	summary := fmt.Sprintf("job %s %s in %s: %s", spec.Name, word, time.Since(h.Started).Round(time.Millisecond), stepSummary(h))
	if msg != "" && status != pipeline.Succeeded {
		summary += " — " + msg
	}
	e.line(lr, "", level, summary)
	e.finish(lr, status, msg)
}

// waitTurn is a queued job run's wait (overlap: queue). First for the run
// before it in this process, when there is one (waitFor: closed once that
// run's finish has let go of the job's lock). Then for the job's lock
// (held.go), which another process may hold, or may have taken in the
// moment between: polled every heldPoll, since a flock that blocks would
// keep its goroutine past a cancel.
//
//	waitFor ──► try job-<name> ──taken──► drop queue-<name>, status running ──► true
//	               │  ▲
//	          held └──┘ every heldPoll        ctx done at any wait ──► false
//
// The lock taken, the run gives up its place in line and is running, both
// under e.mu, so a StartJob that looks sees it queued (holding the queue
// lock) or running (not), never between. Without a runs dir the lock is
// always free: only waitFor is waited for, as before the locks.
func (e *Engine) waitTurn(ctx context.Context, lr *liveRun, job string, waitFor <-chan struct{}) bool {
	if waitFor != nil {
		e.line(lr, "", "info", fmt.Sprintf("job %s: waiting for the run before it to end (overlap: queue)", job))
		select {
		case <-waitFor:
		case <-ctx.Done():
			return false
		}
	}
	told := false // that it waits for another process: said once
	for {
		e.mu.Lock()
		busy, pid, warn := e.holdFileLocked(lockJob, job, lr.id)
		if !busy {
			e.dropFileLocked(lockQueue, job, lr.id)
			lr.mu.Lock()
			lr.rec.Status, lr.rec.Started = pipeline.Running, time.Now()
			lr.mu.Unlock()
		}
		e.mu.Unlock()
		if !busy {
			e.warnLock(lr.id, job, warn)
			return true
		}
		if !told {
			h := e.elsewhere(lockJob, job, pid)
			e.line(lr, "", "info", fmt.Sprintf("job %s: waiting for the run before it to end, %s", job, h.said("overlap: queue")))
			told = true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(heldPoll):
		}
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// stepSummary is each step's outcome in a line: "copy ✓ 12 rows, clean ✗,
// report ↷" — and in a resume "copy ↻ reused, clean ✓ 7 rows".
func stepSummary(r Run) string {
	var parts []string
	for _, p := range r.Pipelines {
		s := p.ID + " " + glyph(p.Status)
		switch p.Status {
		case pipeline.Succeeded:
			s += fmt.Sprintf(" %d rows", p.Rows())
		case pipeline.Reused:
			s += " reused"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// resumeLines opens a resume's log: what it resumes, which steps it
// reuses and where it starts, one line for the run; then, in each step's
// own lines, a reused step's "↻ reused from run …", and why a step the
// earlier run finished runs again (an edit, a param, --rerun), which is
// the one a user would not guess.
//
//	job nightly: resuming run 20261124-020000-7f3a: reusing copy, clean; running report (2 at a time, cli)
//	[copy] ↻ copy: reused from run 20261124-020000-7f3a
//	[clean] clean runs again: pipeline clean-and-load was edited since
func (e *Engine) resumeLines(lr *liveRun, spec *Spec, plan map[string]reuse, ready []string) {
	h := lr.header()
	var use, run []string
	for _, st := range spec.Pipelines {
		if plan[st.ID].from != nil {
			use = append(use, st.ID)
		} else {
			run = append(run, st.ID)
		}
	}
	text := fmt.Sprintf("job %s: resuming run %s: ", spec.Name, h.Resumes)
	if len(use) > 0 {
		text += "reusing " + strings.Join(use, ", ") + "; "
	}
	text += fmt.Sprintf("running %s, from %s (%d at a time, %s)", strings.Join(run, ", "),
		strings.Join(ready, ", "), spec.Policy.Parallel(), h.Trigger)
	e.line(lr, "", "info", text)
	for _, st := range spec.Pipelines {
		switch u := plan[st.ID]; {
		case u.from != nil:
			e.line(lr, st.ID, "info", fmt.Sprintf("↻ %s: reused from run %s", st.ID, u.fromID))
		case u.why != "":
			e.line(lr, st.ID, "info", fmt.Sprintf("%s runs again: %s", st.ID, u.why))
		}
	}
}

// glyph is a state's mark in a line or a tree.
func glyph(s pipeline.Status) string {
	switch s {
	case pipeline.Succeeded:
		return "✓"
	case pipeline.Failed:
		return "✗"
	case pipeline.Canceled:
		return "■"
	case pipeline.Skipped:
		return "↷"
	case pipeline.Running:
		return "●"
	case pipeline.Interrupted:
		return "⚠"
	case pipeline.Reused:
		return "↻"
	}
	return "○"
}

// runStep runs one step of a job: it takes the pipeline (one real run of a
// pipeline at a time, engine-wide), substitutes the step's params, and
// runs it as runOne does, with the state changes around it. It returns the
// step's final status and, when it did not succeed, why.
func (e *Engine) runStep(ctx context.Context, lr *liveRun, idx int, spec *Spec, ps *pipeline.Spec,
	params, vars map[string]string) (pipeline.Status, string) {
	st := spec.Pipelines[idx]
	fail := func(err error) (pipeline.Status, string) {
		msg := errText(err)
		lr.mu.Lock()
		p := &lr.rec.Pipelines[idx]
		p.Status, p.Error, p.Started, p.Ended = pipeline.Failed, msg, time.Now(), time.Now()
		for i := range p.Fragments {
			p.Fragments[i].Status = pipeline.Skipped
		}
		lr.mu.Unlock()
		e.line(lr, st.ID, "err", fmt.Sprintf("pipeline %s failed: %s", ps.Name, msg))
		e.emit(lr, &State{Run: lr.id, Pipeline: st.ID, Status: pipeline.Failed, Error: msg})
		return pipeline.Failed, msg
	}
	if ctx.Err() != nil {
		return pipeline.Skipped, ""
	}
	h := holder{run: lr.id, step: st.ID, job: spec.Name}
	e.mu.Lock()
	err := e.holdLocked(ps.Name, h)
	var busy bool
	var pid string
	var warn error
	if err == nil {
		// the pipeline across processes too (held.go): a step is a real
		// run of it, as a `dbc pipeline run` from a shell is
		if busy, pid, warn = e.holdFileLocked(lockPipeline, ps.Name, lr.id); busy {
			delete(e.pipes, ps.Name)
		}
	}
	e.mu.Unlock()
	if err != nil {
		return fail(err)
	}
	if busy {
		return fail(e.pipelineElsewhere(ps.Name, pid))
	}
	e.warnLock(lr.id, ps.Name, warn)
	defer e.release(ps.Name, h)

	// the step's params: each value with the job's params and the run's
	// values substituted (${min_age}, ${run.date})
	stepParams, err := substStepParams(st, params, vars)
	if err != nil {
		return fail(err)
	}

	lr.mu.Lock()
	p := &lr.rec.Pipelines[idx]
	p.Status, p.Started, p.Params = pipeline.Running, time.Now(), stepParams
	lr.mu.Unlock()
	e.emit(lr, &State{Run: lr.id, Pipeline: st.ID, Status: pipeline.Running})
	e.line(lr, st.ID, "info", fmt.Sprintf("▶ %s: pipeline %s", st.ID, ps.Name))
	e.checkpoint(lr)

	stats, err := e.runOne(ctx, lr, idx, ps, pipeline.Options{Params: stepParams, Run: vars}, lr.header().Origin)
	status := pipeline.Failed
	if stats != nil && stats.Status != "" && stats.Status != pipeline.Running {
		status = stats.Status
	}
	msg := ""
	if err != nil {
		msg = errText(err)
	}
	e.emit(lr, &State{Run: lr.id, Pipeline: st.ID, Status: status, Error: msg})
	e.checkpoint(lr)
	return status, msg
}

// skipDownstream marks every step downstream of id, transitively, skipped
// — those not started yet, which is all of them: a downstream step starts
// only after id succeeded.
func (e *Engine) skipDownstream(lr *liveRun, spec *Spec, id string) {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(from string) {
		for _, d := range spec.Downstream(from) {
			if seen[d] {
				continue
			}
			seen[d] = true
			e.skip(lr, d, "after "+id+" did not succeed")
			walk(d)
		}
	}
	walk(id)
}

// skipRest marks every step still queued skipped: what a cancel, a
// timeout or a stop never reached.
func (e *Engine) skipRest(lr *liveRun) {
	h := lr.header()
	for _, p := range h.Pipelines {
		if p.Status == pipeline.Queued {
			e.skip(lr, p.ID, "")
		}
	}
}

// skip marks one queued step skipped, its fragments with it.
func (e *Engine) skip(lr *liveRun, id, why string) {
	lr.mu.Lock()
	i := slices.IndexFunc(lr.rec.Pipelines, func(p PipelineRun) bool { return p.ID == id })
	if i < 0 || lr.rec.Pipelines[i].Status != pipeline.Queued {
		lr.mu.Unlock()
		return
	}
	p := &lr.rec.Pipelines[i]
	p.Status = pipeline.Skipped
	for j := range p.Fragments {
		p.Fragments[j].Status = pipeline.Skipped
	}
	lr.mu.Unlock()
	if why != "" {
		e.line(lr, id, "info", fmt.Sprintf("↷ %s: skipped, %s", id, why))
	}
	e.emit(lr, &State{Run: lr.id, Pipeline: id, Status: pipeline.Skipped})
}
