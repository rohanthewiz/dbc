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

	"github.com/rohanthewiz/dbc/pipeline"
)

// JobRequest is what StartJob runs.
type JobRequest struct {
	Spec    *Spec
	Params  map[string]string // overrides of the job's params
	Trigger string            // TriggerManual when ""
	By      string            // see Run.By
	Origin  string            // see Run.Origin
	Source  string            // the job's file name, see Run.Source
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
func (e *Engine) StartJob(req JobRequest) (Run, error) {
	if req.Spec == nil {
		return Run{}, serr.New("no job to run")
	}
	if diags := CheckJob(req.Spec, CheckOptions{Find: e.opt.Find}); pipeline.HasError(diags) {
		return Run{}, serr.New("the job does not check out: "+errorDiags(diags), "job", req.Spec.Name)
	}
	spec := req.Spec.Clone()
	params, err := jobParams(spec, req.Params)
	if err != nil {
		return Run{}, err
	}
	pipes := map[string]*pipeline.Spec{}
	for _, st := range spec.Pipelines {
		ps, err := e.opt.Find(st.Pipeline)
		if err != nil {
			return Run{}, serr.Wrap(err, "job", spec.Name, "step", st.ID)
		}
		pipes[st.ID] = ps.Clone()
	}
	if req.Trigger == "" {
		req.Trigger = TriggerManual
	}

	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	lr := newLive(newID(now), cancel)
	lr.rec = Run{
		ID: lr.id, Kind: KindJob, Name: spec.Name, Source: req.Source, Trigger: req.Trigger, By: req.By,
		Params: params, Origin: req.Origin, Started: now, Status: pipeline.Running,
	}
	e.stampProc(&lr.rec)
	for _, st := range spec.Pipelines {
		rs := queued(pipes[st.ID], now, false)
		rs.Status, rs.Started = pipeline.Queued, time.Time{}
		lr.rec.Pipelines = append(lr.rec.Pipelines, PipelineRun{ID: st.ID, After: slices.Clone(st.After),
			Params: maps.Clone(st.Params), RunStats: rs})
	}

	e.mu.Lock()
	if err := e.admitLocked(req.Origin); err != nil {
		e.mu.Unlock()
		cancel()
		return Run{}, err
	}
	var waitFor <-chan struct{}
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
	}
	e.live[lr.id] = lr
	e.jobLast[spec.Name] = lr
	e.wg.Add(1)
	e.mu.Unlock()

	head := lr.header()
	e.emit(lr, &RunStarted{Run: head})
	go e.executeJob(ctx, lr, spec, pipes, waitFor)
	return head, nil
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
func (e *Engine) executeJob(ctx context.Context, lr *liveRun, spec *Spec, pipes map[string]*pipeline.Spec, waitFor <-chan struct{}) {
	defer e.wg.Done()
	stop := e.background(lr)
	if waitFor != nil {
		e.line(lr, "", "info", fmt.Sprintf("job %s: waiting for the run before it to end (overlap: queue)", spec.Name))
		select {
		case <-waitFor:
		case <-ctx.Done():
			e.skipRest(lr)
			stop()
			e.finish(lr, pipeline.Canceled, "canceled while it waited for the run before it")
			return
		}
		lr.mu.Lock()
		lr.rec.Status, lr.rec.Started = pipeline.Running, time.Now()
		lr.mu.Unlock()
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
	root := spec.RootID()
	vars := e.runVars(lr, spec.Name)
	params := lr.header().Params
	e.line(lr, "", "info", fmt.Sprintf("job %s: %d pipelines from %s, %d at a time (%s)",
		spec.Name, len(spec.Pipelines), root, spec.Policy.Parallel(), lr.header().Trigger))

	index := map[string]int{}
	waiting := map[string]int{}
	for i, st := range spec.Pipelines {
		index[st.ID] = i
		waiting[st.ID] = len(st.After)
	}
	type result struct {
		id     string
		status pipeline.Status
		msg    string
	}
	results := make(chan result)
	ready := []string{root}
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

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// stepSummary is each step's outcome in a line: "copy ✓ 12 rows, clean ✗, report ↷".
func stepSummary(r Run) string {
	var parts []string
	for _, p := range r.Pipelines {
		s := p.ID + " " + glyph(p.Status)
		if p.Status == pipeline.Succeeded {
			s += fmt.Sprintf(" %d rows", p.Rows())
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
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
	e.mu.Unlock()
	if err != nil {
		return fail(err)
	}
	defer e.release(ps.Name, h)

	// the step's params: each value with the job's params and the run's
	// values substituted (${min_age}, ${run.date})
	stepParams := map[string]string{}
	for k, v := range st.Params {
		sv, err := pipeline.Subst(v, func(name string) (string, bool) {
			if pv, ok := params[name]; ok {
				return pv, true
			}
			if rn, ok := strings.CutPrefix(name, "run."); ok {
				rv, ok := vars[rn]
				return rv, ok
			}
			return "", false
		})
		if err != nil {
			return fail(serr.Wrap(err, "param", k))
		}
		stepParams[k] = sv
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
