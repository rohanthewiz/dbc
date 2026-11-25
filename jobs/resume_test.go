package jobs

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/userdata"
)

// counter counts the runs of each pipeline a test registered with it.
type counter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *counter) note(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[name]++
}

func (c *counter) get(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[name]
}

// actParam registers a pipeline of one Go action that takes param x, and
// whose action hands x to fn.
func (je *jobEngine) actParam(name string, fn func(x string) error) {
	p := pipeline.New(name).Param("x", "-")
	p.Fragment("f").Func(func(e *pipeline.Env) error { return fn(e.Params["x"]) })
	je.mu.Lock()
	je.pipes[name] = p.Spec()
	je.mu.Unlock()
}

func statuses(r Run) map[string]pipeline.Status {
	out := map[string]pipeline.Status{}
	for _, p := range r.Pipelines {
		out[p.ID] = p.Status
	}
	return out
}

func step(r Run, id string) PipelineRun {
	for _, p := range r.Pipelines {
		if p.ID == id {
			return p
		}
	}
	return PipelineRun{}
}

// A failed diamond, resumed: the steps the first run finished (a, c) are
// reused and not run again, the failed one (b) and the one it skipped (d)
// run. The resume's record names the run it resumes, says each reused
// step's origin, counts no rows for it, and its log and summary say so.
func TestResumeRunsFromTheFailure(t *testing.T) {
	je := newJobEngine(t, func(o *Options) { o.RunsDir = t.TempDir() })
	var ran counter
	var bFails atomic.Bool
	bFails.Store(true)
	for _, name := range []string{"pa", "pb", "pc", "pd"} {
		je.act(name, func(*pipeline.Env) error {
			ran.note(name)
			if name == "pb" && bFails.Load() {
				return errorString("b broke")
			}
			return nil
		})
	}
	spec := job(t, strings.Replace(diamond, "%s", "", 1))
	first, err := je.StartJob(JobRequest{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	fin1 := je.wait(t, first.ID)
	if fin1.Status != pipeline.Failed || statuses(fin1)["d"] != pipeline.Skipped {
		t.Fatalf("first run = %s %v", fin1.Status, statuses(fin1))
	}
	if h := step(fin1, "a").SpecHash; len(h) != 16 {
		t.Errorf("a's spec hash = %q", h)
	}

	bFails.Store(false)
	head, err := je.StartJob(JobRequest{Spec: spec, Resume: first.ID, Trigger: TriggerCLI})
	if err != nil {
		t.Fatal(err)
	}
	if head.Resumes != first.ID || statuses(head)["a"] != pipeline.Reused || statuses(head)["b"] != pipeline.Queued {
		t.Errorf("resume's header = %q %v", head.Resumes, statuses(head))
	}
	fin := je.wait(t, head.ID)
	want := map[string]pipeline.Status{"a": pipeline.Reused, "b": pipeline.Succeeded, "c": pipeline.Reused, "d": pipeline.Succeeded}
	for id, st := range want {
		if statuses(fin)[id] != st {
			t.Errorf("resume: %s = %s, want %s", id, statuses(fin)[id], st)
		}
	}
	if fin.Status != pipeline.Succeeded || fin.Resumes != first.ID || fin.By != "resumes "+first.ID {
		t.Errorf("resume = %s %q %q %q", fin.Status, fin.Error, fin.Resumes, fin.By)
	}
	for name, n := range map[string]int{"pa": 1, "pb": 2, "pc": 1, "pd": 1} {
		if ran.get(name) != n {
			t.Errorf("%s ran %d times, want %d", name, ran.get(name), n)
		}
	}
	a := step(fin, "a")
	if a.ReusedFrom != first.ID || a.Rows() != 0 || !a.Started.IsZero() || len(a.Fragments) != 1 ||
		a.Fragments[0].Status != pipeline.Reused {
		t.Errorf("reused a = %+v", a)
	}
	if got := fin.RunValues["id"]; got != first.ID {
		t.Errorf("resume's run.id = %q, want the first run's %s", got, first.ID)
	}
	if s := stepSummary(fin); !strings.Contains(s, "a ↻ reused") || !strings.Contains(s, "b ✓ 0 rows") {
		t.Errorf("summary = %q", s)
	}
	if tr := fin.Tree(); !strings.Contains(tr, "resumes "+first.ID) || !strings.Contains(tr, "↻ a") ||
		!strings.Contains(tr, "reused from "+first.ID) {
		t.Errorf("tree:\n%s", tr)
	}
	var said []string
	for _, ev := range je.events(head.ID) {
		if l, ok := ev.(*Logged); ok {
			said = append(said, "["+l.Pipeline+"] "+l.Line.Text)
		}
	}
	all := strings.Join(said, "\n")
	for _, w := range []string{
		"[] job d: resuming run " + first.ID + ": reusing a, c; running b, d, from b (2 at a time, cli)",
		"[a] ↻ a: reused from run " + first.ID,
	} {
		if !strings.Contains(all, w) {
			t.Errorf("log lacks %q:\n%s", w, all)
		}
	}
	// on disk too, for a resume from another process, and in a listing's
	// head (read off the record) with the rows the resume committed only
	if r, ok := je.fromDisk(head.ID); !ok || r.Resumes != first.ID || step(r, "c").ReusedFrom != first.ID {
		t.Errorf("record on disk = %v %+v", ok, r)
	}
	hs, err := userdata.ListRuns(je.opt.RunsDir, userdata.RunFilter{Kind: KindJob, Name: "d"})
	if err != nil || len(hs) != 2 {
		t.Fatalf("listing = %v %v", hs, err)
	}
	for _, h := range hs {
		if want := map[string]string{first.ID: "", head.ID: first.ID}[h.ID]; h.Resumes != want {
			t.Errorf("listed %s resumes %q, want %q", h.ID, h.Resumes, want)
		}
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

// Run values: a resume's steps see the first run's ${run.id} (and date,
// time, started), its own trigger — down a chain of resumes too, where a
// step reused twice still names the run it ran in. A step whose params
// hold ${run.trigger} differs whenever the trigger does, so it runs
// again (a is reused only because its params leave the trigger out).
func TestResumeCarriesRunValues(t *testing.T) {
	je := newJobEngine(t, nil)
	var fail sync.Map // step pipeline → fails while set
	fail.Store("pb", true)
	seen := map[string]string{}
	var smu sync.Mutex
	for _, name := range []string{"pa", "pb", "pc"} {
		je.actParam(name, func(x string) error {
			smu.Lock()
			seen[name] = x
			smu.Unlock()
			if _, ok := fail.Load(name); ok {
				return errorString(name + " broke")
			}
			return nil
		})
	}
	spec := job(t, `{"name": "chain", "pipelines": [
	  {"id": "a", "pipeline": "pa", "params": {"x": "${run.id}|${run.date}"}},
	  {"id": "b", "pipeline": "pb", "after": ["a"], "params": {"x": "${run.id}|${run.date}"}},
	  {"id": "c", "pipeline": "pc", "after": ["b"], "params": {"x": "${run.id}|${run.trigger}"}}]}`)
	r1, err := je.StartJob(JobRequest{Spec: spec, Trigger: TriggerSchedule})
	if err != nil {
		t.Fatal(err)
	}
	f1 := je.wait(t, r1.ID)
	day := f1.Started.Local().Format("2006-01-02")
	if f1.Status != pipeline.Failed || seen["pa"] != r1.ID+"|"+day {
		t.Fatalf("run 1 = %s, a saw %q", f1.Status, seen["pa"])
	}

	// resume 1: b now runs and passes, c fails
	fail.Delete("pb")
	fail.Store("pc", true)
	r2, err := je.StartJob(JobRequest{Spec: spec, Resume: r1.ID, Trigger: TriggerCLI})
	if err != nil {
		t.Fatal(err)
	}
	f2 := je.wait(t, r2.ID)
	if f2.Status != pipeline.Failed || statuses(f2)["a"] != pipeline.Reused || statuses(f2)["b"] != pipeline.Succeeded ||
		statuses(f2)["c"] != pipeline.Failed {
		t.Fatalf("run 2 = %s %v", f2.Status, statuses(f2))
	}
	if seen["pb"] != r1.ID+"|"+day || seen["pc"] != r1.ID+"|cli" {
		t.Errorf("b saw %q, c %q: want run 1's id and date, run 2's trigger", seen["pb"], seen["pc"])
	}

	// resume 2, of resume 1: a and b reused, c runs with run 1's id
	fail.Delete("pc")
	r3, err := je.StartJob(JobRequest{Spec: spec, Resume: "last", Trigger: TriggerManual})
	if err != nil {
		t.Fatal(err)
	}
	f3 := je.wait(t, r3.ID)
	if f3.Status != pipeline.Succeeded || f3.Resumes != r2.ID {
		t.Fatalf("run 3 = %s %q resumes %q", f3.Status, f3.Error, f3.Resumes)
	}
	if seen["pc"] != r1.ID+"|manual" || f3.RunValues["id"] != r1.ID || f3.RunValues["trigger"] != TriggerManual {
		t.Errorf("c saw %q; run 3's values %v", seen["pc"], f3.RunValues)
	}
	if a, b := step(f3, "a"), step(f3, "b"); a.ReusedFrom != r1.ID || b.ReusedFrom != r2.ID {
		t.Errorf("reused from: a %q (want %s), b %q (want %s)", a.ReusedFrom, r1.ID, b.ReusedFrom, r2.ID)
	}
	if got := step(f3, "a").Params["x"]; got != r1.ID+"|"+day {
		t.Errorf("reused a's params = %q: the ones it ran with", got)
	}
}

// resumePlan's rules, one step at a time: what makes a step the earlier
// run finished run again, and that everything after it runs again too.
func TestResumePlan(t *testing.T) {
	spec := job(t, `{"name": "p", "params": {"n": {"default": "1"}}, "pipelines": [
	  {"id": "a", "pipeline": "pa"},
	  {"id": "b", "pipeline": "pb", "after": ["a"], "params": {"x": "${n}"}},
	  {"id": "c", "pipeline": "pc", "after": ["a"]},
	  {"id": "d", "pipeline": "pd", "after": ["b", "c"]}]}`)
	pipes := map[string]*pipeline.Spec{}
	hashes := map[string]string{}
	for _, id := range []string{"a", "b", "c", "d"} {
		pipes[id] = pipeline.New("p" + id).Spec()
		hashes[id] = SpecHash(pipes[id])
	}
	earlierOf := func(edit func(*Run)) *Run {
		r := &Run{ID: "20261125-020000-aaaa", Kind: KindJob, Name: "p", Status: pipeline.Failed}
		for _, id := range []string{"a", "b", "c", "d"} {
			p := PipelineRun{ID: id, SpecHash: hashes[id], RunStats: pipeline.RunStats{Pipeline: "p" + id, Status: pipeline.Succeeded}}
			if id == "b" {
				p.Params = map[string]string{"x": "1"}
			}
			r.Pipelines = append(r.Pipelines, p)
		}
		r.Pipelines[3].Status = pipeline.Failed
		if edit != nil {
			edit(r)
		}
		return r
	}
	params := map[string]string{"n": "1"}
	vars := map[string]string{"id": "x"}
	got := func(plan map[string]reuse) string {
		var out []string
		for _, id := range []string{"a", "b", "c", "d"} {
			u := plan[id]
			switch {
			case u.from != nil:
				out = append(out, id+" reused")
			case u.why != "":
				out = append(out, id+" runs: "+u.why)
			default:
				out = append(out, id+" runs")
			}
		}
		return strings.Join(out, "; ")
	}
	for _, tc := range []struct {
		name   string
		edit   func(*Run)
		params map[string]string
		rerun  []string
		hashes map[string]string
		want   string
	}{
		{name: "the failed step only", want: "a reused; b reused; c reused; d runs"},
		{name: "rerun", rerun: []string{"c"}, want: "a reused; b reused; c runs: named in --rerun; d runs"},
		{name: "rerun the root", rerun: []string{"a"},
			want: "a runs: named in --rerun; b runs: after a, which runs; c runs: after a, which runs; d runs"},
		{name: "a param", params: map[string]string{"n": "2"},
			want: `a reused; b runs: its params differ: x: "1" → "2"; c reused; d runs`},
		{name: "an edit", hashes: map[string]string{"c": "0000000000000000"},
			want: "a reused; b reused; c runs: pipeline pc was edited since; d runs"},
		{name: "no hash", edit: func(r *Run) { r.Pipelines[1].SpecHash = "" },
			want: "a reused; b runs: run 20261125-020000-aaaa's record has no hash of its pipeline (written before resumes); c reused; d runs"},
		{name: "another pipeline", edit: func(r *Run) { r.Pipelines[2].Pipeline = "old" },
			want: "a reused; b reused; c runs: its pipeline is pc now, was old; d runs"},
		{name: "a new step", edit: func(r *Run) { r.Pipelines = r.Pipelines[:2] }, // c, d were not in the job
			want: "a reused; b reused; c runs; d runs"},
		{name: "skipped earlier", edit: func(r *Run) { r.Pipelines[1].Status = pipeline.Skipped },
			want: "a reused; b runs; c reused; d runs"},
		{name: "reused earlier", edit: func(r *Run) {
			r.Pipelines[0].Status, r.Pipelines[0].ReusedFrom = pipeline.Reused, "20261124-020000-bbbb"
		}, want: "a reused; b reused; c reused; d runs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := hashes
			if tc.hashes != nil {
				hs = map[string]string{}
				for k, v := range hashes {
					hs[k] = v
				}
				for k, v := range tc.hashes {
					hs[k] = v
				}
			}
			ps := params
			if tc.params != nil {
				ps = tc.params
			}
			earlier := earlierOf(tc.edit)
			plan := resumePlan(spec, pipes, hs, earlier, ps, vars, tc.rerun)
			if g := got(plan); g != tc.want {
				t.Errorf("plan =\n  %s\nwant\n  %s", g, tc.want)
			}
			if tc.name == "reused earlier" && plan["a"].fromID != "20261124-020000-bbbb" {
				t.Errorf("a reused from %q: where it last ran", plan["a"].fromID)
			}
			if tc.name == "the failed step only" && plan["b"].fromID != earlier.ID {
				t.Errorf("b reused from %q", plan["b"].fromID)
			}
		})
	}
}

// What StartJob refuses of a resume, with nothing started: no such run,
// another job's run, a run still going, one with nothing left to run,
// --rerun without a resume or of a step the job lacks, last with no run;
// and a param override still has to be one the job declares.
func TestResumeRefusals(t *testing.T) {
	je := newJobEngine(t, nil)
	gate := make(chan struct{})
	je.act("pa", func(*pipeline.Env) error { return nil })
	je.act("slow", func(*pipeline.Env) error { <-gate; return nil })
	spec := job(t, `{"name": "one", "params": {"n": {"default": "1"}}, "pipelines": [{"id": "a", "pipeline": "pa"}]}`)
	other := job(t, `{"name": "two", "pipelines": [{"id": "s", "pipeline": "slow"}]}`)

	try := func(req JobRequest, want string) {
		t.Helper()
		if _, err := je.StartJob(req); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: err = %v, want %q", req, err, want)
		}
	}
	try(JobRequest{Spec: spec, Resume: "last"}, "job one has no run to resume")
	try(JobRequest{Spec: spec, Resume: "nope"}, `"nope" is not a run id`)
	try(JobRequest{Spec: spec, Resume: "20261125-020000-aaaa"}, "no run 20261125-020000-aaaa to resume")
	try(JobRequest{Spec: spec, Rerun: []string{"a"}}, "it needs a run to resume")

	running, err := je.StartJob(JobRequest{Spec: other})
	if err != nil {
		t.Fatal(err)
	}
	try(JobRequest{Spec: other, Resume: running.ID}, "is still running")
	try(JobRequest{Spec: spec, Resume: running.ID}, "is a run of job two, not of job one")
	close(gate)
	je.wait(t, running.ID)

	done, err := je.StartJob(JobRequest{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	je.wait(t, done.ID)
	try(JobRequest{Spec: spec, Resume: done.ID}, "nothing to resume: every step of run "+done.ID+" succeeded and is unchanged")
	try(JobRequest{Spec: spec, Resume: done.ID, Rerun: []string{"zz"}}, "job one has no step zz to rerun")
	try(JobRequest{Spec: spec, Resume: done.ID, Params: map[string]string{"m": "2"}}, "not a parameter of the job")

	// a succeeded run, its step named in --rerun: it runs again
	again, err := je.StartJob(JobRequest{Spec: spec, Resume: "last", Rerun: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if fin := je.wait(t, again.ID); fin.Status != pipeline.Succeeded || statuses(fin)["a"] != pipeline.Succeeded ||
		fin.Resumes != done.ID {
		t.Errorf("rerun of a = %s %v %q", fin.Status, statuses(fin), fin.Resumes)
	}
}

// A param override on a resume: the earlier run's params are the base, so
// a step whose substituted params it changes runs again, with everything
// after it; one it leaves alone is reused.
func TestResumeParams(t *testing.T) {
	je := newJobEngine(t, nil)
	var ran counter
	var cFails atomic.Bool
	cFails.Store(true)
	for _, name := range []string{"pa", "pb", "pc"} {
		je.actParam(name, func(string) error {
			ran.note(name)
			if name == "pc" && cFails.Load() {
				return errorString("c broke")
			}
			return nil
		})
	}
	spec := job(t, `{"name": "pj", "params": {"n": {"default": "1"}, "m": {"default": "1"}}, "pipelines": [
	  {"id": "a", "pipeline": "pa", "params": {"x": "${m}"}},
	  {"id": "b", "pipeline": "pb", "after": ["a"], "params": {"x": "${n}"}},
	  {"id": "c", "pipeline": "pc", "after": ["b"]}]}`)
	first, err := je.StartJob(JobRequest{Spec: spec, Params: map[string]string{"n": "5"}})
	if err != nil {
		t.Fatal(err)
	}
	je.wait(t, first.ID)
	cFails.Store(false)

	// no override: the earlier run's n=5 is kept, so b is reused
	r, err := je.StartJob(JobRequest{Spec: spec, Resume: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	if fin := je.wait(t, r.ID); fin.Params["n"] != "5" || statuses(fin)["b"] != pipeline.Reused {
		t.Errorf("no override: params %v, %v", fin.Params, statuses(fin))
	}
	// n=6: b runs again (and c after it); a, whose x is ${m}, is reused
	r, err = je.StartJob(JobRequest{Spec: spec, Resume: first.ID, Params: map[string]string{"n": "6"}})
	if err != nil {
		t.Fatal(err)
	}
	fin := je.wait(t, r.ID)
	if statuses(fin)["a"] != pipeline.Reused || statuses(fin)["b"] != pipeline.Succeeded || step(fin, "b").Params["x"] != "6" {
		t.Errorf("n=6: %v, b's params %v", statuses(fin), step(fin, "b").Params)
	}
	if ran.get("pa") != 1 || ran.get("pb") != 2 || ran.get("pc") != 3 {
		t.Errorf("ran a %d b %d c %d", ran.get("pa"), ran.get("pb"), ran.get("pc"))
	}
}
