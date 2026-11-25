package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/userdata"
)

// Resuming a job run (N-339): a new run of the job that takes over the
// steps an earlier run of it finished, rather than running them again,
// and runs the rest.
//
// WHY A STEP CAN BE TAKEN OVER. Steps hand each other nothing in memory:
// each pipeline reads and writes databases and files, and every step runs
// on its own session. A step that succeeded has left its output where the
// steps after it read it, so a resume can start from the steps it did not
// get to.
//
//	earlier run  a ✓ ─► b ✓ ─► d ✗ ─► e ↷
//	                └─► c ✓
//	resume       a ↻ ─► b ↻ ─► d ● ─► e ○      (↻ reused: not run, marked so)
//	                └─► c ↻
//
// A resume is its own run, with its own id and record (Resumes names the
// earlier one), so the earlier record keeps how that run went.
//
// WHEN A STEP IS REUSED (resumePlan). Only when nothing it was run with has
// changed since, and everything it waits for is reused too:
//
//	the earlier run has a step of that id that succeeded (or was itself
//	reused: a resume of a resume) ─┐
//	the same pipeline, by name ────┤
//	the same pipeline spec, by hash (SpecHash, kept in each step's record)
//	the same params, as substituted ─┤
//	not named in --rerun ────────────┤
//	every after reused ──────────────┴─► reused
//
// The last rule makes the reused steps a set closed upstream: a step that
// runs again feeds new rows to every step after it, which run again too.
// A record written before the hash was kept cannot say its pipeline is
// unchanged, so its steps run again.
//
// RUN VALUES. A step's names may hold ${run.date} or ${run.id} — a table
// orders_${run.date}, a batch column stamped '${run.id}' that a later step
// selects by. So a resume's steps see the earlier run's run values (and,
// for a resume of a resume, the first run's): id, date, time, started.
// Only trigger is the resume's own. The values are kept in the record
// (RunValues), for the next resume of it. A step whose params hold
// ${run.trigger} therefore differs from the earlier run's whenever the
// trigger does (a 2am schedule resumed from a shell), and runs again,
// as the params rule says; its log line says which param.
//
// WHAT IS NOT CHECKED. A reused step's output is trusted to still be there:
// a file under a per-run directory someone cleaned up, staging an outside
// job dropped. The user, who named the run, names such steps in Rerun
// (`--rerun STEP`), and each runs again with everything after it.

// resumeVars are the run values a resume carries over; trigger and job are
// the resume's own (the job's name is the same).
var resumeVars = []string{"id", "date", "time", "started"}

// resumable reports whether a run that ended with status may be resumed:
// any end (a succeeded run with an edited pipeline re-runs that step and
// what follows it), but not a run still going.
func resumable(s pipeline.Status) bool {
	return s != pipeline.Running && s != pipeline.Queued
}

// earlierRun finds and checks the run a resume of job takes over: an id,
// or "last" for the newest run of the job. It must be a job run of that
// name, ended.
func (e *Engine) earlierRun(job, id string) (Run, error) {
	if id == "last" {
		hs, err := e.History(userdata.RunFilter{Kind: KindJob, Name: job, Limit: 1})
		if err != nil {
			return Run{}, serr.Wrap(err, "op", "find the last run", "job", job)
		}
		if len(hs) == 0 {
			return Run{}, serr.New("job "+job+" has no run to resume", "job", job)
		}
		id = hs[0].ID
	} else if !userdata.ValidRunID(id) {
		return Run{}, serr.New(fmt.Sprintf("%q is not a run id (20261009-020000-7f3a, or last)", id))
	}
	r, ok := e.Get(id)
	if !ok {
		return Run{}, serr.New("no run "+id+" to resume", "run", id)
	}
	if r.Kind != KindJob || r.Name != job {
		return Run{}, serr.New(fmt.Sprintf("run %s is a run of %s %s, not of job %s", id, r.Kind, r.Name, job), "run", id)
	}
	if !resumable(r.Status) {
		return Run{}, serr.New(fmt.Sprintf("run %s is still %s: a run can be resumed once it has ended", id, r.Status), "run", id)
	}
	return r, nil
}

// earlierValues is the run values an earlier run's steps saw: kept in the
// record when it was itself a resume, otherwise what its id and start
// make (runVars' rule).
func earlierValues(r *Run) map[string]string {
	out := map[string]string{}
	if len(r.RunValues) > 0 {
		for _, k := range resumeVars {
			out[k] = r.RunValues[k]
		}
		return out
	}
	t := r.Started.Local()
	out["id"], out["date"], out["time"], out["started"] =
		r.ID, t.Format("2006-01-02"), t.Format("15:04:05"), t.Format(time.RFC3339)
	return out
}

// resumeParams is a resume's job params: the earlier run's, for the names
// the job still declares, under the overrides given. jobParams then adds
// the defaults of params declared since, and refuses an override the job
// does not declare.
func resumeParams(spec *Spec, earlier *Run, given map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range earlier.Params {
		if _, ok := spec.Params[k]; ok {
			out[k] = v
		}
	}
	maps.Copy(out, given)
	return out
}

// SpecHash is a pipeline spec's fingerprint, kept in each job step's
// record so a resume can tell whether the pipeline was edited since: the
// first 16 hex digits of the SHA-256 of its JSON. encoding/json sorts map
// keys, so the same spec always hashes the same; a Go-func node's Fn is
// not in the JSON (json:"-") and so not in the hash. 64 bits tell an edit
// from none; nothing here needs a collision to be hard to make.
func SpecHash(ps *pipeline.Spec) string {
	bs, err := json.Marshal(ps)
	if err != nil {
		return "" // an unmarshalable spec cannot be told unchanged: it runs again
	}
	sum := sha256.Sum256(bs)
	return hex.EncodeToString(sum[:8])
}

// substStepParams is a step's params with the job's params and the run's
// values substituted (${min_age}, ${run.date}): what runStep runs the
// step's pipeline with, and what a resume compares with the earlier run's.
func substStepParams(st Step, params, vars map[string]string) (map[string]string, error) {
	out := map[string]string{}
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
			return nil, serr.Wrap(err, "param", k)
		}
		out[k] = sv
	}
	return out, nil
}

// reuse is resumePlan's verdict on one step.
type reuse struct {
	from   *PipelineRun // the earlier run's step, when reused
	fromID string       // the run where the step last ran (ReusedFrom)
	// why says why a step the earlier run finished runs again; "" for a
	// reused step, and for one the earlier run did not finish (that it
	// runs needs no reason)
	why string
}

// resumePlan decides, step by step, which steps of spec a resume of
// earlier reuses (see the top). pipes and hashes are the steps' pipelines
// as this run loads them; params and vars the run's job params and run
// values; rerun the steps named to run again. Steps are taken in
// readiness order, so each step's afters are decided before it.
func resumePlan(spec *Spec, pipes map[string]*pipeline.Spec, hashes map[string]string, earlier *Run,
	params, vars map[string]string, rerun []string) map[string]reuse {
	before := map[string]*PipelineRun{}
	for i := range earlier.Pipelines {
		before[earlier.Pipelines[i].ID] = &earlier.Pipelines[i]
	}
	out := map[string]reuse{}
	for _, id := range readinessOrder(spec) {
		st := spec.Step(id)
		was := before[id]
		finished := was != nil && (was.Status == pipeline.Succeeded || was.Status == pipeline.Reused)
		if !finished {
			out[id] = reuse{}
			continue
		}
		why := ""
		switch {
		case slices.Contains(rerun, id):
			why = "named in --rerun"
		case slices.ContainsFunc(st.After, func(a string) bool { return out[a].from == nil }):
			// the first after that runs again names the cause
			i := slices.IndexFunc(st.After, func(a string) bool { return out[a].from == nil })
			why = "after " + st.After[i] + ", which runs"
		case was.Pipeline != pipes[id].Name:
			why = fmt.Sprintf("its pipeline is %s now, was %s", pipes[id].Name, was.Pipeline)
		case was.SpecHash == "":
			why = "run " + earlier.ID + "'s record has no hash of its pipeline (written before resumes)"
		case was.SpecHash != hashes[id]:
			why = "pipeline " + pipes[id].Name + " was edited since"
		default:
			now, err := substStepParams(*st, params, vars)
			if err != nil {
				why = "its params do not substitute: " + errText(err)
			} else if d := paramsDiff(was.Params, now); d != "" {
				why = "its params differ: " + d
			}
		}
		if why != "" {
			out[id] = reuse{why: why}
			continue
		}
		from := earlier.ID
		if was.Status == pipeline.Reused && was.ReusedFrom != "" {
			from = was.ReusedFrom // where it last ran, down a chain of resumes
		}
		out[id] = reuse{from: was, fromID: from}
	}
	return out
}

// paramsDiff names the first param that differs between a step's params
// then and now, "k: old → new", or "" when they are the same.
func paramsDiff(was, now map[string]string) string {
	keys := slices.Sorted(maps.Keys(was))
	for k := range now {
		if _, ok := was[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range slices.Compact(keys) {
		ov, oo := was[k]
		nv, no := now[k]
		if oo != no || ov != nv {
			return fmt.Sprintf("%s: %s → %s", k, quoteOr(ov, oo), quoteOr(nv, no))
		}
	}
	return ""
}

// quoteOr is a param value in a message, or "unset".
func quoteOr(v string, set bool) string {
	if !set {
		return "unset"
	}
	return fmt.Sprintf("%q", v)
}

// readinessOrder is the job's steps in an order where every step comes
// after its afters (Kahn's walk from the root, ties in spec order). A
// checked job is a DAG with one root, so every step is in it; a step a
// cycle would leave out is appended at the end, so none is lost.
func readinessOrder(spec *Spec) []string {
	waiting := map[string]int{}
	for _, st := range spec.Pipelines {
		waiting[st.ID] = len(st.After)
	}
	var out, ready []string
	for _, st := range spec.Pipelines {
		if len(st.After) == 0 {
			ready = append(ready, st.ID)
		}
	}
	seen := map[string]bool{}
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, id)
		seen[id] = true
		for _, d := range spec.Downstream(id) {
			if waiting[d]--; waiting[d] == 0 {
				ready = append(ready, d)
			}
		}
	}
	for _, st := range spec.Pipelines {
		if !seen[st.ID] {
			out = append(out, st.ID)
		}
	}
	return out
}

// markReused makes a queued step of a starting resume's record a reused
// one: not run, its fragments listed with nothing counted (its rows are
// the earlier run's, and a listing's totals would count them twice), its
// params the ones it ran with.
func markReused(p *PipelineRun, u reuse) {
	p.Status = pipeline.Reused
	p.ReusedFrom = u.fromID
	p.Params = maps.Clone(u.from.Params)
	for i := range p.Fragments {
		p.Fragments[i].Status = pipeline.Reused
	}
}
