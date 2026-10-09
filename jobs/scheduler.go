package jobs

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/dbc/userdata"
)

// The scheduler fires the jobs' cron lines while its host runs — dbc web,
// the one long-lived dbc process. A cron outside dbc calls `dbc job run
// NAME` instead; the TUI does not schedule.
//
// Only the jobs in the jobs directory are scheduled. The built-in examples
// never are: a sample's "0 2 * * *" would otherwise fire on every machine
// that starts dbc web.
//
//	every step (at the earliest next fire, a Reload, or a minute at most):
//	    scan jobs_dir: new or changed schedules get their next fires,
//	        removed jobs are dropped, a file that does not parse is told once
//	    for each job with a fire due:
//	        late by more than MissGrace (the machine slept, the process was
//	        stopped)? a miss — told, not run — unless the job says catch_up
//	        otherwise StartJob(trigger schedule, by the cron line); a run
//	        still going (overlap: skip) is told as a skipped fire
//
// Waking at least once a minute is what keeps it right across a clock
// change or a laptop's sleep: timers count the monotonic clock, which may
// stand still while the machine sleeps, and the wall clock is what a cron
// line means — so each step reads the wall clock afresh rather than
// trusting a long timer.
//
// CATCH-UP. With "catch_up": true, a job whose latest record is older than
// a fire time that has passed — dbc web was not running at 02:00 — runs
// once when the scheduler first sees it, and a fire missed while running
// (asleep) runs late instead of being skipped. Without it, missed times are
// not replayed.

// SchedOptions configure a Scheduler.
type SchedOptions struct {
	// Dir is the jobs directory.
	Dir string
	// MissGrace is how late a fire may be and still run; 0 means a minute.
	MissGrace time.Duration
	// MaxWait is the longest the scheduler sleeps between steps (and so
	// how soon it notices a file changed behind its back); 0 means a
	// minute.
	MaxWait time.Duration
	// Now is the clock; nil is time.Now. Tests set it.
	Now func() time.Time
}

// Scheduler fires schedules; see above. Its methods are safe for
// concurrent use.
type Scheduler struct {
	e    *Engine
	opt  SchedOptions
	wake chan struct{}
	stop chan struct{}
	gone chan struct{}

	mu      sync.Mutex
	entries map[string]*schedEntry // by job file name
	told    map[string]string      // file → the problem last told about it
	started bool                   // the first scan (catch-up's moment) is done
}

// schedEntry is one scheduled job.
type schedEntry struct {
	file   string
	spec   *Spec
	key    string // tz and cron lines: a change recomputes the fires
	scheds []*Schedule
	next   []time.Time // per schedule; zero when it fires no more
}

// NewScheduler makes a scheduler over the engine without starting it:
// Start runs it, Step drives it by hand (tests).
func (e *Engine) NewScheduler(opt SchedOptions) *Scheduler {
	if opt.MissGrace <= 0 {
		opt.MissGrace = time.Minute
	}
	if opt.MaxWait <= 0 {
		opt.MaxWait = time.Minute
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Scheduler{e: e, opt: opt, wake: make(chan struct{}, 1), stop: make(chan struct{}), gone: make(chan struct{}),
		entries: map[string]*schedEntry{}, told: map[string]string{}}
}

// Start runs the scheduler on its own goroutine until Stop.
func (s *Scheduler) Start() {
	go func() {
		defer close(s.gone)
		for {
			s.Step()
			wait := s.untilNext(s.opt.Now())
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-s.wake:
				t.Stop()
			case <-s.stop:
				t.Stop()
				return
			}
		}
	}()
}

// Stop ends a started scheduler and waits for it; runs it started go on
// (they are the engine's).
func (s *Scheduler) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	select {
	case <-s.gone:
	case <-time.After(5 * time.Second):
	}
}

// Reload makes a started scheduler scan the jobs again now: a job was
// saved, renamed or trashed.
func (s *Scheduler) Reload() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// NextFires is each scheduled job's next fire, by job file name.
func (s *Scheduler) NextFires() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]time.Time{}
	for file, ent := range s.entries {
		if n := earliest(ent.next); !n.IsZero() {
			out[file] = n
		}
	}
	return out
}

func earliest(ts []time.Time) time.Time {
	var best time.Time
	for _, t := range ts {
		if !t.IsZero() && (best.IsZero() || t.Before(best)) {
			best = t
		}
	}
	return best
}

// untilNext is how long to sleep: to the earliest fire, at most MaxWait.
func (s *Scheduler) untilNext(now time.Time) time.Duration {
	wait := s.opt.MaxWait
	for _, n := range s.NextFires() {
		if d := n.Sub(now); d < wait {
			wait = max(d, 0)
		}
	}
	return wait
}

// Step is one pass: scan the jobs, fire what is due. Start calls it; a test
// calls it with its own clock.
func (s *Scheduler) Step() {
	now := s.opt.Now()
	s.scan(now)
	for _, f := range s.due(now) {
		s.fire(f)
	}
}

// firing is one fire due now.
type firing struct {
	spec *Spec
	file string
	expr string
	at   time.Time // the time it was due
	late bool      // past MissGrace: a catch-up
}

// scan reads the jobs directory into the entries.
func (s *Scheduler) scan(now time.Time) {
	infos, err := userdata.ListJobs(s.opt.Dir)
	if err != nil {
		s.tell("", "", fmt.Sprintf("the scheduler could not list %s: %s", s.opt.Dir, errText(err)))
		return
	}
	s.mu.Lock()
	first := !s.started
	s.started = true
	s.mu.Unlock()

	seen := map[string]bool{}
	for _, in := range infos {
		if len(in.Schedule) == 0 {
			continue
		}
		seen[in.Name] = true
		text, _, err := userdata.ReadJob(s.opt.Dir, in.Name)
		var spec *Spec
		if err == nil {
			spec, err = ParseJob(text)
		}
		var scheds []*Schedule
		if err == nil {
			scheds, err = spec.Schedules()
		}
		if err != nil {
			s.tell(in.Name, "", fmt.Sprintf("job %s is not scheduled: %s", in.Name, errText(err)))
			s.mu.Lock()
			delete(s.entries, in.Name)
			s.mu.Unlock()
			continue
		}
		s.untell(in.Name)
		key := spec.Triggers.TZ + "\x00" + strings.Join(spec.Triggers.Schedule, "\x00")

		s.mu.Lock()
		ent := s.entries[in.Name]
		if ent != nil && ent.key == key {
			ent.spec = spec // the rest of the job may have changed; the fires stand
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()

		ent = &schedEntry{file: in.Name, spec: spec, key: key, scheds: scheds, next: make([]time.Time, len(scheds))}
		for i, sc := range scheds {
			ent.next[i] = sc.Next(now)
		}
		// catch-up, the first time the scheduler sees the job: a fire
		// between its last run and now was missed while dbc web was down
		if first && spec.Triggers.CatchUp {
			if last, ok := s.e.LastRun(KindJob, spec.Name); ok {
				for i, sc := range scheds {
					if m := lastFireBefore(sc, last.Started, now); !m.IsZero() {
						ent.next[i] = m
					}
				}
			}
		}
		s.mu.Lock()
		s.entries[in.Name] = ent
		s.mu.Unlock()
	}
	s.mu.Lock()
	for file := range s.entries {
		if !seen[file] {
			delete(s.entries, file)
		}
	}
	s.mu.Unlock()
}

// lastFireBefore is the latest fire of sc after from and not after now —
// the one a catch-up runs for — or zero when none fell between.
func lastFireBefore(sc *Schedule, from, now time.Time) time.Time {
	var last time.Time
	for t := sc.Next(from); !t.IsZero() && !t.After(now); t = sc.Next(t) {
		last = t
	}
	return last
}

// due takes the fires due at now off the entries, advancing each fired
// schedule past now. Several lines of one job due at once fire once.
func (s *Scheduler) due(now time.Time) []firing {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []firing
	for _, file := range slices.Sorted(maps.Keys(s.entries)) {
		ent := s.entries[file]
		var f *firing
		for i, n := range ent.next {
			if n.IsZero() || n.After(now) {
				continue
			}
			if f == nil || n.Before(f.at) {
				f = &firing{spec: ent.spec, file: file, expr: ent.scheds[i].String(), at: n}
			}
			ent.next[i] = ent.scheds[i].Next(now)
		}
		if f == nil {
			continue
		}
		if now.Sub(f.at) > s.opt.MissGrace {
			if !ent.spec.Triggers.CatchUp {
				s.e.send(&Notice{Job: ent.spec.Name, Level: "warn", Text: fmt.Sprintf(
					"missed the %s run of job %s (dbc web was not running, or the machine slept); catch_up is off, so it is not run late",
					f.at.Format("2006-01-02 15:04"), ent.spec.Name)})
				continue
			}
			f.late = true
		}
		out = append(out, *f)
	}
	return out
}

// fire starts one scheduled run; a refusal is told, not retried.
func (s *Scheduler) fire(f firing) {
	if !s.claim(f) {
		s.e.send(&Notice{Job: f.spec.Name, Level: "info", Text: fmt.Sprintf(
			"the %s run of job %s was started by another dbc web on this machine", f.at.Format("15:04"), f.spec.Name)})
		return
	}
	by := f.expr
	if f.late {
		by += " (catching up the " + f.at.Format("2006-01-02 15:04") + " run)"
	}
	run, err := s.e.StartJob(JobRequest{Spec: f.spec, Trigger: TriggerSchedule, By: by, Source: f.file})
	switch {
	case errors.Is(err, ErrBusy):
		s.e.send(&Notice{Job: f.spec.Name, Level: "info", Text: fmt.Sprintf("skipped the %s run of job %s: %s",
			f.at.Format("15:04"), f.spec.Name, err.Error())})
	case err != nil:
		s.e.send(&Notice{Job: f.spec.Name, Level: "err", Text: fmt.Sprintf("the %s run of job %s did not start: %s",
			f.at.Format("15:04"), f.spec.Name, errText(err))})
	default:
		s.e.send(&Notice{Run: run.ID, Job: f.spec.Name, Level: "info", Text: fmt.Sprintf("⏱ job %s started on schedule (%s), run %s",
			f.spec.Name, by, run.ID)})
	}
}

// CLAIMS. Two dbc web processes on one machine — dbc.app and one started in
// a terminal, say — read the same jobs_dir, and each would fire 02:00: the
// overlap rule is per engine, so it cannot see the other's run. A fire is
// claimed first by creating a file named for the job and the fire's time
// in runs_dir/.fires with O_EXCL — atomic, so of two processes exactly one
// creates it — and only the claimer runs. No lock is held between fires,
// so a process that dies leaves nothing to clean up but a small file,
// swept after two days.
const claimsDir = ".fires"

// claim reports whether this process may fire f. Without a runs dir (a
// test's engine) there is no one to share with: always.
func (s *Scheduler) claim(f firing) bool {
	if s.e.opt.RunsDir == "" {
		return true
	}
	dir := filepath.Join(s.e.opt.RunsDir, claimsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return true // no claims possible: firing beats not firing
	}
	name := fmt.Sprintf("%s.%d", f.spec.Name, f.at.Unix())
	fh, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return !errors.Is(err, fs.ErrExist)
	}
	_ = fh.Close()
	sweepClaims(dir, s.opt.Now())
	return true
}

// sweepClaims removes claims older than two days: a fire that old cannot
// be claimed again.
func sweepClaims(dir string, now time.Time) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > 48*time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// tell sends a problem with a job file once: the scheduler scans every
// minute, and a broken file must not say so every minute.
func (s *Scheduler) tell(file, run, text string) {
	s.mu.Lock()
	if s.told[file] == text {
		s.mu.Unlock()
		return
	}
	s.told[file] = text
	s.mu.Unlock()
	s.e.send(&Notice{Run: run, Job: strings.TrimSuffix(file, ".json"), Level: "warn", Text: text})
}

// untell forgets a file's problem: fixed, it may break again and be told.
func (s *Scheduler) untell(file string) {
	s.mu.Lock()
	delete(s.told, file)
	s.mu.Unlock()
}
