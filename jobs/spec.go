package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/pipeline"
)

// Spec is a job as data: what jobs/<name>.json holds — a DAG of pipelines
// with one root, its triggers, and what a failure does.
//
//	{
//	  "name": "nightly",
//	  "desc": "Load, then sync the dimensions in parallel, then report",
//	  "root": "extract",
//	  "params": { "day": { "default": "", "doc": "the day to load; empty for today" } },
//	  "pipelines": [
//	    { "id": "extract",   "pipeline": "orders-nightly" },
//	    { "id": "customers", "pipeline": "customers-sync", "after": ["extract"] },
//	    { "id": "products",  "pipeline": "products-sync",  "after": ["extract"] },
//	    { "id": "report",    "pipeline": "daily-report",   "after": ["customers", "products"],
//	                         "params": { "day": "${run.date}" } }
//	  ],
//	  "triggers": { "schedule": ["0 2 * * *"], "webhook": true },
//	  "policy": { "on_failure": "finish_branches", "max_parallel": 2, "overlap": "skip", "timeout": "2h" }
//	}
//
// A step's params are the pipeline's: each value may name the job's own
// params (${day}) and the run's values (${run.date}; pipeline.RunVars),
// substituted when the step starts.
type Spec struct {
	Name      string                    `json:"name"`
	Desc      string                    `json:"desc,omitempty"`
	Root      string                    `json:"root,omitempty"` // the step with no after; may be left out
	Params    map[string]pipeline.Param `json:"params,omitempty"`
	Pipelines []Step                    `json:"pipelines"`
	Triggers  Triggers                  `json:"triggers"`
	Policy    Policy                    `json:"policy"`
}

// Step is one pipeline node of the DAG.
type Step struct {
	ID       string            `json:"id"`
	Pipeline string            `json:"pipeline"` // a name in pipelines_dir (.json optional) or an example
	After    []string          `json:"after,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
}

// Triggers say what may start the job besides a person: by hand (either
// UI, `dbc job run`, a script's s.RunJob) is always allowed.
type Triggers struct {
	// Schedule is cron expressions (cron.go), fired by a running dbc web.
	Schedule []string `json:"schedule,omitempty"`
	// TZ is the IANA zone the schedule's wall clock is read in; "" is
	// the machine's local zone.
	TZ string `json:"tz,omitempty"`
	// CatchUp: when dbc web starts after a scheduled time it was not up
	// for, run once for the latest missed time. Off by default — a
	// missed night is not replayed unless asked for.
	CatchUp bool `json:"catch_up,omitempty"`
	// Webhook opens POST /api/v1/jobs/<name>/run to a caller holding the
	// launch secret as a Bearer token. Off by default: it is a door from
	// outside, so it is opened per job.
	Webhook bool `json:"webhook,omitempty"`
}

// Policy is what the run does with failure, parallelism, overlap and time.
type Policy struct {
	// OnFailure: "finish_branches" (default) — a failed pipeline's
	// downstream is skipped, the branches that do not depend on it run on,
	// and the job ends failed; "stop" — the run is canceled at once.
	OnFailure string `json:"on_failure,omitempty"`
	// MaxParallel is how many pipelines run at once; 0 means 2. Fan-out
	// contends for connections: each running fragment holds a reader and a
	// writer, so a small pool makes a wide fan-out wait, not fail.
	MaxParallel int `json:"max_parallel,omitempty"`
	// Overlap: a start while a run of the job is going — "skip" (default)
	// refuses it, "queue" runs it when that one ends (one waits at most).
	Overlap string `json:"overlap,omitempty"`
	// Timeout cancels the run after this long ("2h", "45m"); "" is none.
	Timeout string `json:"timeout,omitempty"`
}

// The policy's values.
const (
	FinishBranches = "finish_branches"
	StopOnFailure  = "stop"
	OverlapSkip    = "skip"
	OverlapQueue   = "queue"
)

// DefaultMaxParallel is max_parallel left at 0.
const DefaultMaxParallel = 2

// Parallel is the policy's max_parallel, defaulted.
func (p Policy) Parallel() int {
	if p.MaxParallel > 0 {
		return p.MaxParallel
	}
	return DefaultMaxParallel
}

// TimeoutDur is the policy's timeout; 0 for none (or one that does not
// parse, which Check reports).
func (p Policy) TimeoutDur() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(p.Timeout))
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// ParseJob reads a job from its JSON. Unknown keys are an error, as in a
// pipeline; Check's rules are not applied, so an editor can show what is
// wrong with a half-built job.
func ParseJob(text string) (*Spec, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, serr.Wrap(err, "op", "parse job")
	}
	return &s, nil
}

// JSON is the job as the file holds it, two-space indented, with arrays
// of scalars (a step's after, the schedule) on one line, as a pipeline's.
func (s *Spec) JSON() (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return "", serr.Wrap(err, "op", "encode job")
	}
	return pipeline.CompactArrays(buf.String()), nil
}

// Step is the step with id, or nil.
func (s *Spec) Step(id string) *Step {
	for i := range s.Pipelines {
		if s.Pipelines[i].ID == id {
			return &s.Pipelines[i]
		}
	}
	return nil
}

// RootID is the root step: Root when set, else the one step with no after.
func (s *Spec) RootID() string {
	if s.Root != "" {
		return s.Root
	}
	var roots []string
	for _, st := range s.Pipelines {
		if len(st.After) == 0 {
			roots = append(roots, st.ID)
		}
	}
	if len(roots) == 1 {
		return roots[0]
	}
	return ""
}

// Downstream is the steps that list id in their after, in spec order.
func (s *Spec) Downstream(id string) []string {
	var out []string
	for _, st := range s.Pipelines {
		if slices.Contains(st.After, id) {
			out = append(out, st.ID)
		}
	}
	return out
}

// Schedules parses the job's cron lines in its zone. A line or zone that
// does not parse is an error (Check reports each).
func (s *Spec) Schedules() ([]*Schedule, error) {
	loc, err := s.Location()
	if err != nil {
		return nil, err
	}
	var out []*Schedule
	for _, expr := range s.Triggers.Schedule {
		sc, err := ParseCron(expr, loc)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, nil
}

// Location is the zone the schedule is read in.
func (s *Spec) Location() (*time.Location, error) {
	if strings.TrimSpace(s.Triggers.TZ) == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(strings.TrimSpace(s.Triggers.TZ))
	if err != nil {
		return nil, serr.Wrap(err, "tz", s.Triggers.TZ)
	}
	return loc, nil
}

// NextFire is the earliest next fire after t across the job's schedules;
// zero when it has none (or none that fires).
func (s *Spec) NextFire(t time.Time) time.Time {
	scheds, err := s.Schedules()
	if err != nil {
		return time.Time{}
	}
	var best time.Time
	for _, sc := range scheds {
		if n := sc.Next(t); !n.IsZero() && (best.IsZero() || n.Before(best)) {
			best = n
		}
	}
	return best
}

// Clone copies the spec deeply enough that editing the copy leaves the
// original alone.
func (s *Spec) Clone() *Spec {
	c := *s
	c.Params = maps.Clone(s.Params)
	c.Pipelines = make([]Step, len(s.Pipelines))
	for i, st := range s.Pipelines {
		st.After = slices.Clone(st.After)
		st.Params = maps.Clone(st.Params)
		c.Pipelines[i] = st
	}
	c.Triggers.Schedule = slices.Clone(s.Triggers.Schedule)
	return &c
}

// CheckOptions shape a job Check.
type CheckOptions struct {
	// Find returns a pipeline's spec by the name a step gives; nil skips
	// every check that needs the pipeline (its existence, its params, its
	// own Check).
	Find func(name string) (*pipeline.Spec, error)
	// Conns is handed to each pipeline's Check (see pipeline.CheckOptions).
	Conns []string
}

// CheckJob finds what is wrong with a job without running it. Diags use
// pipeline.Diag, with where "name", "root", "params.<p>", "<step>",
// "<step>.after", "<step>.params.<p>", "triggers.schedule[i]",
// "triggers.tz" or "policy.<field>" — <step> being the step's id, or
// "pipelines[i]" when the id is invalid:
//
//	names      the job, its params and steps are valid names; steps unique
//	graph      every after names another step; no cycles (Kahn); exactly
//	           one step has no after, and it is the root; every step is
//	           reachable from it
//	pipelines  every step's pipeline exists and passes its own Check; the
//	           params a step sets are the pipeline's, every param the
//	           pipeline has no default for is set, and each value's ${…}
//	           is a job param or a run value
//	triggers   every cron line parses and fires; the zone loads
//	policy     on_failure, overlap, max_parallel ≥ 0, timeout a duration
func CheckJob(s *Spec, opt CheckOptions) []pipeline.Diag {
	c := &jobChecker{}
	if !pipeline.ValidName(s.Name) {
		c.errorf("name", "not a job name: letters, digits, . _ - (got %q)", s.Name)
	}
	for _, name := range slices.Sorted(maps.Keys(s.Params)) {
		if !pipeline.ValidName(name) {
			c.errorf("params."+name, "not a parameter name: letters, digits, . _ -")
		}
	}
	if len(s.Pipelines) == 0 {
		c.errorf("pipelines", "a job needs at least one pipeline")
	}
	// wheres[i] is how every finding about the i-th step names it: its id,
	// or "pipelines[i]" when the id is invalid ("", "a b"), as
	// pipeline.Check names a node. An index always reaches the step's card
	// and its place in the text (Locate), whatever the id holds.
	ids := map[string]bool{}
	wheres := make([]string, len(s.Pipelines))
	for i, st := range s.Pipelines {
		wheres[i] = st.ID
		switch {
		case !pipeline.ValidName(st.ID):
			wheres[i] = fmt.Sprintf("pipelines[%d]", i)
			c.errorf(wheres[i], "not a step id: letters, digits, . _ - (got %q)", st.ID)
		case ids[st.ID]:
			c.errorf(st.ID, "two steps are called %q", st.ID)
		}
		ids[st.ID] = true
	}
	c.graph(s, ids, wheres)
	for i, st := range s.Pipelines {
		c.step(s, st, wheres[i], opt)
	}
	c.triggers(s)
	c.policy(s.Policy)
	return c.out
}

type jobChecker struct{ out []pipeline.Diag }

func (c *jobChecker) errorf(where, format string, args ...any) {
	c.out = append(c.out, pipeline.Diag{Where: where, Severity: pipeline.SevError, Msg: fmt.Sprintf(format, args...)})
}

func (c *jobChecker) warnf(where, format string, args ...any) {
	c.out = append(c.out, pipeline.Diag{Where: where, Severity: pipeline.SevWarning, Msg: fmt.Sprintf(format, args...)})
}

// graph checks the DAG: dangling and self edges, the root, cycles, reach.
//
// Kahn's algorithm: repeatedly take a step none of whose afters is left;
// what can never be taken sits on a cycle (or behind one).
func (c *jobChecker) graph(s *Spec, ids map[string]bool, wheres []string) {
	for i, st := range s.Pipelines {
		seen := map[string]bool{}
		for _, a := range st.After {
			switch {
			case a == st.ID:
				c.errorf(wheres[i]+".after", "a step cannot wait for itself")
			case !ids[a]:
				c.errorf(wheres[i]+".after", "no step called %q", a)
			case seen[a]:
				c.errorf(wheres[i]+".after", "%q is listed twice", a)
			}
			seen[a] = true
		}
	}
	var roots []string
	for _, st := range s.Pipelines {
		if len(st.After) == 0 {
			roots = append(roots, st.ID)
		}
	}
	switch {
	case len(s.Pipelines) == 0:
	case len(roots) == 0:
		c.errorf("root", "every step waits for another: a job needs one step with no after, its root")
	case len(roots) > 1:
		c.errorf("root", "a job has one root, but %s have no after (make the others wait for one)", strings.Join(roots, ", "))
	case s.Root != "" && s.Root != roots[0]:
		c.errorf("root", "root is %q, but the step with no after is %q", s.Root, roots[0])
	}
	if s.Root != "" && !ids[s.Root] {
		c.errorf("root", "no step called %q", s.Root)
	}

	waiting := map[string]int{}
	for _, st := range s.Pipelines {
		for _, a := range st.After {
			if ids[a] && a != st.ID {
				waiting[st.ID]++
			}
		}
	}
	var ready, order []string
	for _, st := range s.Pipelines {
		if waiting[st.ID] == 0 {
			ready = append(ready, st.ID)
		}
	}
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, d := range s.Downstream(id) {
			if d == id {
				continue
			}
			if waiting[d]--; waiting[d] == 0 {
				ready = append(ready, d)
			}
		}
	}
	if len(order) < len(ids) {
		var stuck []string
		for _, st := range s.Pipelines {
			if !slices.Contains(order, st.ID) && !slices.Contains(stuck, st.ID) {
				stuck = append(stuck, st.ID)
			}
		}
		c.errorf("pipelines", "the steps wait for each other in a cycle: %s", strings.Join(stuck, ", "))
		return
	}
	// with one root and no cycle, every step reaches back to the root; a
	// second root was reported above, and its subtree is unreachable
	if root := s.RootID(); root != "" {
		reach := map[string]bool{root: true}
		for _, id := range order {
			if reach[id] {
				for _, d := range s.Downstream(id) {
					reach[d] = true
				}
			}
		}
		for i, st := range s.Pipelines {
			if !reach[st.ID] && len(st.After) > 0 {
				c.errorf(wheres[i], "not reachable from the root %s", root)
			}
		}
	}
}

// step checks one step against its pipeline; where names the step.
func (c *jobChecker) step(s *Spec, st Step, where string, opt CheckOptions) {
	if strings.TrimSpace(st.Pipeline) == "" {
		c.errorf(where, "no pipeline named")
		return
	}
	for _, k := range slices.Sorted(maps.Keys(st.Params)) {
		for _, ref := range pipeline.Refs(st.Params[k]) {
			if _, isParam := s.Params[ref]; isParam {
				continue
			}
			if name, ok := strings.CutPrefix(ref, "run."); ok {
				if _, ok := pipeline.RunVars[name]; ok {
					continue
				}
			}
			c.errorf(where+".params."+k, "${%s} is not a parameter of the job or a run value", ref)
		}
	}
	if opt.Find == nil {
		return
	}
	spec, err := opt.Find(st.Pipeline)
	if err != nil {
		c.errorf(where, "pipeline %s: %s", st.Pipeline, errText(err))
		return
	}
	for _, k := range slices.Sorted(maps.Keys(st.Params)) {
		if _, ok := spec.Params[k]; !ok {
			c.errorf(where+".params."+k, "%s has no parameter %q", st.Pipeline, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(spec.Params)) {
		if _, set := st.Params[k]; !set && spec.Params[k].Default == "" {
			c.errorf(where+".params", "%s needs %q (it has no default)", st.Pipeline, k)
		}
	}
	for _, d := range pipeline.Check(spec, pipeline.CheckOptions{Conns: opt.Conns}) {
		if d.Severity == pipeline.SevError {
			c.errorf(where, "pipeline %s: %s", st.Pipeline, d)
		}
	}
}

func (c *jobChecker) triggers(s *Spec) {
	loc, err := s.Location()
	if err != nil {
		c.errorf("triggers.tz", "unknown time zone %q (an IANA name, such as Europe/Paris)", s.Triggers.TZ)
		loc = time.Local
	}
	for i, expr := range s.Triggers.Schedule {
		where := fmt.Sprintf("triggers.schedule[%d]", i)
		sc, err := ParseCron(expr, loc)
		if err != nil {
			c.errorf(where, "%s", errText(err))
			continue
		}
		if sc.Next(time.Now()).IsZero() {
			c.errorf(where, "%q never fires", expr)
		}
	}
	if s.Triggers.CatchUp && len(s.Triggers.Schedule) == 0 {
		c.warnf("triggers.catch_up", "catch_up has no schedule to catch up on")
	}
}

func (c *jobChecker) policy(p Policy) {
	if p.OnFailure != "" && p.OnFailure != FinishBranches && p.OnFailure != StopOnFailure {
		c.errorf("policy.on_failure", `on_failure is "finish_branches" or "stop", not %q`, p.OnFailure)
	}
	if p.Overlap != "" && p.Overlap != OverlapSkip && p.Overlap != OverlapQueue {
		c.errorf("policy.overlap", `overlap is "skip" or "queue", not %q`, p.Overlap)
	}
	if p.MaxParallel < 0 {
		c.errorf("policy.max_parallel", "max_parallel must be at least 1 (0 means %d)", DefaultMaxParallel)
	}
	if t := strings.TrimSpace(p.Timeout); t != "" {
		if d, err := time.ParseDuration(t); err != nil || d <= 0 {
			c.errorf("policy.timeout", "timeout is a positive duration such as 2h or 45m, not %q", p.Timeout)
		}
	}
}
