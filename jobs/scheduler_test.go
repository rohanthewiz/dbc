package jobs

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/userdata"
)

// clock is a settable Now for a scheduler driven by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

// notices is the Notice texts the engine sent so far.
func (je *jobEngine) notices() []string {
	je.testEngine.mu.Lock()
	defer je.testEngine.mu.Unlock()
	var out []string
	for _, e := range je.evs {
		if n, ok := e.(*Notice); ok {
			out = append(out, n.Text)
		}
	}
	return out
}

// runsOf is the engine's runs of job name, live and finished.
func (je *jobEngine) runsOf(name string) []Run {
	var out []Run
	for _, r := range append(je.Running(), je.Recent()...) {
		if r.Name == name && r.Kind == KindJob {
			out = append(out, r)
		}
	}
	return out
}

// A schedule fires at its time and not before or twice; a fire while the
// last run still goes is skipped and told; a fire the scheduler slept
// through is told as missed and not run; the examples are never scheduled.
func TestSchedulerFires(t *testing.T) {
	je := newJobEngine(t, nil)
	gate := make(chan struct{})
	var gateOnce sync.Once
	je.act("p", func(e *pipeline.Env) error {
		select {
		case <-gate:
		case <-e.Ctx.Done():
		}
		return nil
	})
	dir := t.TempDir()
	if _, _, err := userdata.SaveJob(dir, "n.json", `{"name": "n", "pipelines": [{"id": "s", "pipeline": "p"}],
	  "triggers": {"schedule": ["0 2 * * *"], "tz": "UTC"}}`, ""); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 10, 9, 1, 59, 0, 0, time.UTC)}
	s := je.NewScheduler(SchedOptions{Dir: dir, Now: clk.now})
	s.Step()
	if n := s.NextFires()["n.json"]; !n.Equal(time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("next = %s", n)
	}
	if len(je.runsOf("n")) != 0 {
		t.Fatal("fired early")
	}
	clk.set(time.Date(2026, 10, 9, 2, 0, 5, 0, time.UTC))
	s.Step()
	runs := je.runsOf("n")
	if len(runs) != 1 || runs[0].Trigger != TriggerSchedule || runs[0].By != "0 2 * * *" || runs[0].Source != "n.json" {
		t.Fatalf("runs = %+v", runs)
	}
	s.Step() // the same minute: nothing more
	if len(je.runsOf("n")) != 1 {
		t.Error("fired twice")
	}
	// the next day's fire finds the first run still going: skipped, told
	clk.set(time.Date(2026, 10, 10, 2, 0, 1, 0, time.UTC))
	s.Step()
	if len(je.runsOf("n")) != 1 || !hasNotice(je.notices(), "skipped the 02:00 run of job n") {
		t.Errorf("overlap: runs %d, notices %q", len(je.runsOf("n")), je.notices())
	}
	gateOnce.Do(func() { close(gate) })
	je.wait(t, runs[0].ID)
	// slept through the 11th's 02:00, woke at 05:00: missed, not run
	clk.set(time.Date(2026, 10, 11, 5, 0, 0, 0, time.UTC))
	s.Step()
	if len(je.runsOf("n")) != 1 || !hasNotice(je.notices(), "missed the 2026-10-11 02:00 run of job n") {
		t.Errorf("missed: runs %d, notices %q", len(je.runsOf("n")), je.notices())
	}
	if n := s.NextFires()["n.json"]; !n.Equal(time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("next after the miss = %s", n)
	}
}

func hasNotice(ns []string, part string) bool {
	for _, n := range ns {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

// A file that does not parse is told once, however often the scheduler
// scans; a schedule changed on disk is picked up on the next step; a job
// removed is dropped.
func TestSchedulerRescans(t *testing.T) {
	je := newJobEngine(t, nil)
	dir := t.TempDir()
	if _, _, err := userdata.SaveJob(dir, "bad.json", `{"name": "bad", "pipelines": [], "triggers": {"schedule": ["0 99 * * *"]}}`, ""); err != nil {
		t.Fatal(err)
	}
	rev, _, err := userdata.SaveJob(dir, "ok.json", `{"name": "ok", "pipelines": [{"id": "s", "pipeline": "p"}],
	  "triggers": {"schedule": ["0 2 * * *"], "tz": "UTC"}}`, "")
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	s := je.NewScheduler(SchedOptions{Dir: dir, Now: clk.now})
	s.Step()
	s.Step()
	n := 0
	for _, txt := range je.notices() {
		if strings.Contains(txt, "job bad.json is not scheduled") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("bad file told %d times: %q", n, je.notices())
	}
	if _, _, err = userdata.SaveJob(dir, "ok.json", `{"name": "ok", "pipelines": [{"id": "s", "pipeline": "p"}],
	  "triggers": {"schedule": ["30 14 * * *"], "tz": "UTC"}}`, rev); err != nil {
		t.Fatal(err)
	}
	s.Step()
	if got := s.NextFires()["ok.json"]; !got.Equal(time.Date(2026, 10, 9, 14, 30, 0, 0, time.UTC)) {
		t.Errorf("changed schedule's next = %s", got)
	}
	if _, err = userdata.TrashJob(dir, "ok.json"); err != nil {
		t.Fatal(err)
	}
	s.Step()
	if len(s.NextFires()) != 0 {
		t.Errorf("after the trash = %v", s.NextFires())
	}
}

// catch_up: started after a fire it was not up for, the scheduler runs the
// job once at once — by the latest record — and says it is catching up.
func TestSchedulerCatchesUp(t *testing.T) {
	runs := t.TempDir()
	je := newJobEngine(t, func(o *Options) { o.RunsDir = runs })
	je.act("p", func(*pipeline.Env) error { return nil })
	dir := t.TempDir()
	if _, _, err := userdata.SaveJob(dir, "c.json", `{"name": "c", "pipelines": [{"id": "s", "pipeline": "p"}],
	  "triggers": {"schedule": ["0 2 * * *"], "tz": "UTC", "catch_up": true}}`, ""); err != nil {
		t.Fatal(err)
	}
	last := Run{ID: "20261007-020000-0001", Kind: KindJob, Name: "c", Status: pipeline.Succeeded,
		Started: time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC)}
	bs, _ := json.Marshal(last)
	if err := userdata.SaveRun(runs, KindJob, "c", last.ID, bs); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)}
	s := je.NewScheduler(SchedOptions{Dir: dir, Now: clk.now})
	s.Step()
	got := je.runsOf("c")
	if len(got) != 1 || !strings.Contains(got[0].By, "catching up the 2026-10-09 02:00 run") {
		t.Fatalf("runs = %+v", got)
	}
	je.wait(t, got[0].ID)
	s.Step()
	if len(je.runsOf("c")) != 1 {
		t.Error("caught up twice")
	}
}

// Two schedulers over one jobs_dir and runs_dir — two dbc web processes —
// fire a time once between them: the first to claim it runs it, the other
// says so.
func TestSchedulersShareAFire(t *testing.T) {
	runs := t.TempDir()
	dir := t.TempDir()
	if _, _, err := userdata.SaveJob(dir, "s.json", `{"name": "s", "pipelines": [{"id": "a", "pipeline": "p"}],
	  "triggers": {"schedule": ["0 2 * * *"], "tz": "UTC"}}`, ""); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 10, 9, 1, 59, 0, 0, time.UTC)}
	var engines []*jobEngine
	var scheds []*Scheduler
	for range 2 {
		je := newJobEngine(t, func(o *Options) { o.RunsDir = runs })
		je.act("p", func(*pipeline.Env) error { return nil })
		engines = append(engines, je)
		s := je.NewScheduler(SchedOptions{Dir: dir, Now: clk.now})
		s.Step()
		scheds = append(scheds, s)
	}
	clk.set(time.Date(2026, 10, 9, 2, 0, 1, 0, time.UTC))
	for _, s := range scheds {
		s.Step()
	}
	total := len(engines[0].runsOf("s")) + len(engines[1].runsOf("s"))
	if total != 1 || !hasNotice(engines[1].notices(), "was started by another dbc web") {
		t.Errorf("runs = %d; second's notices %q", total, engines[1].notices())
	}
	for _, je := range engines {
		for _, r := range je.runsOf("s") {
			je.wait(t, r.ID)
		}
	}
}
