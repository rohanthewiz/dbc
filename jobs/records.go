package jobs

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/userdata"
)

// Run records on disk: one JSON file per run (userdata/runs.go), written by
// the engine running it and readable by any process.
//
//	start ──► record (status running, every step queued)
//	  │  every FlushEvery: rewritten whole — the latest counters, and a
//	  │  heartbeat: its file's age says whether a writer is still alive
//	end ───► final record; older ones of the same name beyond RunsKeep pruned
//
//	process dies mid-run ──► the record stays "running" and stops ageing
//	                         well: past StaleAfter, any reader takes it for
//	                         interrupted (settle), and Recover rewrites it so
//
// Previews are never recorded.

// Summary is one run as a listing shows it: the record's header, read off
// disk or taken from a live run.
type Summary = userdata.RunHead

// recording reports whether r is written to disk.
func (e *Engine) recording(r *Run) bool { return e.opt.RunsDir != "" && r.Preview == 0 }

// save writes r's record. A failure is told once per run (a Notice), and
// the run goes on: a record is a report, not part of the work.
func (e *Engine) save(lr *liveRun, r *Run) {
	bs, err := json.MarshalIndent(r, "", "  ")
	if err == nil {
		err = userdata.SaveRun(e.opt.RunsDir, r.Kind, r.Name, r.ID, bs)
	}
	if err == nil {
		return
	}
	lr.mu.Lock()
	told := lr.saveFailed
	lr.saveFailed = true
	lr.mu.Unlock()
	if !told {
		e.send(&Notice{Run: r.ID, Job: r.Name, Level: "warn",
			Text: fmt.Sprintf("could not write the record of run %s: %s", r.ID, errText(err))})
	}
}

// checkpoint writes a live run's record now, between flushes: a job step
// started or ended, so a crash right after still leaves the record saying
// which step was running.
func (e *Engine) checkpoint(lr *liveRun) {
	lr.mu.Lock()
	snap := lr.rec.clone()
	lr.mu.Unlock()
	if e.recording(&snap) {
		e.save(lr, &snap)
	}
}

// prune keeps the newest RunsKeep records of kind/name. A record another
// process is still writing is never taken from under it.
func (e *Engine) prune(kind, name string) {
	_, _ = userdata.PruneRuns(e.opt.RunsDir, kind, name, e.opt.RunsKeep, func(h userdata.RunHead) bool {
		return e.liveElsewhere(h)
	})
}

// liveElsewhere reports whether a record that says it is running (or
// queued) is still being written by someone: this engine, or a process
// whose heartbeat is fresh.
func (e *Engine) liveElsewhere(h userdata.RunHead) bool {
	if h.Status != string(pipeline.Running) && h.Status != string(pipeline.Queued) {
		return false
	}
	if e.isLive(h.ID) {
		return true
	}
	// fresh by its heartbeat, unless its lock says its process is gone
	return time.Since(h.Mod) < e.opt.StaleAfter && !e.lockSaysDead(h.Kind, h.Name, h.Status)
}

func (e *Engine) isLive(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.live[id]
	return ok
}

// stale reports whether a record last written at mod, saying running or
// queued, has lost its writer.
func (e *Engine) stale(r *Run, mod time.Time) bool {
	if (r.Status != pipeline.Running && r.Status != pipeline.Queued) || e.isLive(r.ID) {
		return false
	}
	// the heartbeat's rule, or at once when the run's lock is free: its
	// process is gone (lockSaysDead)
	return time.Since(mod) >= e.opt.StaleAfter || e.lockSaysDead(r.Kind, r.Name, string(r.Status))
}

// interrupt rewrites a dead run's record as interrupted: the run, and
// whatever of it was running when its writer stopped. What never started
// is skipped. Ended is the record's last write — when it was last known
// alive.
func interrupt(r *Run, mod time.Time) {
	r.Status = pipeline.Interrupted
	if r.Ended.IsZero() {
		r.Ended = mod
	}
	if r.Error == "" {
		r.Error = "the process running it stopped before the run ended (dbc web or dbc job run was quit, killed or crashed)"
	}
	for i := range r.Pipelines {
		p := &r.Pipelines[i]
		switch p.Status {
		case pipeline.Running:
			p.Status = pipeline.Interrupted
		case pipeline.Queued:
			p.Status = pipeline.Skipped
		}
		for j := range p.Fragments {
			switch p.Fragments[j].Status {
			case pipeline.Running:
				p.Fragments[j].Status = pipeline.Interrupted
			case pipeline.Queued:
				p.Fragments[j].Status = pipeline.Skipped
			}
		}
	}
}

// fromDisk reads run id's record back, settled: a dead "running" one reads
// interrupted.
func (e *Engine) fromDisk(id string) (Run, bool) {
	if e.opt.RunsDir == "" {
		return Run{}, false
	}
	bs, mod, err := userdata.ReadRun(e.opt.RunsDir, id)
	if err != nil {
		return Run{}, false
	}
	var r Run
	if json.Unmarshal(bs, &r) != nil {
		return Run{}, false
	}
	if e.stale(&r, mod) {
		interrupt(&r, mod)
	}
	return r, true
}

// Records is Get for many runs at once, in the order of ids, leaving out
// the ones not found: the live and recent ones from memory, the others
// read from disk through one index of the runs directory
// (userdata.RunIndex) rather than a glob per id, which lists every
// kind and name directory each time (N-285; `dbc runs --sql` reads every
// record it loads).
func (e *Engine) Records(ids []string) []Run {
	out := make([]Run, 0, len(ids))
	var index map[string]string // made on the first record not in memory
	for _, id := range ids {
		if r, ok := e.inMemory(id); ok {
			out = append(out, r)
			continue
		}
		if e.opt.RunsDir == "" {
			continue
		}
		if index == nil {
			var err error
			if index, err = userdata.RunIndex(e.opt.RunsDir); err != nil {
				index = map[string]string{}
			}
		}
		path, ok := index[id]
		if !ok {
			continue
		}
		bs, mod, err := userdata.ReadRunAt(path)
		if err != nil {
			continue
		}
		var r Run
		if json.Unmarshal(bs, &r) != nil {
			continue
		}
		if e.stale(&r, mod) {
			interrupt(&r, mod)
		}
		out = append(out, r)
	}
	return out
}

// Recover rewrites as interrupted every record a dead process left
// running or queued — so a listing that does not settle records (a plain
// `cat`, jq) reads the truth too. A host calls it once when it starts.
// It returns how many it rewrote.
//
// It reads each record's status alone (userdata.RunStatuses: the front of
// the file), not its head: every headless run calls this, and a head is
// parsed from the whole record — every record a name keeps, with its log
// lines — where only a running or queued one is read whole (N-285).
func (e *Engine) Recover() (int, error) {
	if e.opt.RunsDir == "" {
		return 0, nil
	}
	sts, err := userdata.RunStatuses(e.opt.RunsDir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, h := range sts {
		if h.Status != string(pipeline.Running) && h.Status != string(pipeline.Queued) {
			continue
		}
		bs, mod, err := userdata.ReadRunAt(h.Path)
		if err != nil {
			continue
		}
		var r Run
		if json.Unmarshal(bs, &r) != nil || !e.stale(&r, mod) {
			continue
		}
		interrupt(&r, mod)
		out, err := json.MarshalIndent(&r, "", "  ")
		if err == nil && userdata.SaveRun(e.opt.RunsDir, r.Kind, r.Name, r.ID, out) == nil {
			n++
		}
	}
	return n, nil
}

// History lists runs newest first: the live ones and the records on disk
// (settled, so a dead one reads interrupted), through f. Without a runs
// directory it is the live runs and the finished ones kept in memory.
func (e *Engine) History(f userdata.RunFilter) ([]Summary, error) {
	var out []Summary
	seen := map[string]bool{}
	add := func(s Summary) {
		if seen[s.ID] || !matches(s, f) {
			return
		}
		seen[s.ID] = true
		out = append(out, s)
	}
	for _, r := range e.Running() {
		if r.Preview == 0 {
			add(summaryOf(&r, time.Now()))
		}
	}
	if e.opt.RunsDir != "" {
		heads, err := userdata.ListRuns(e.opt.RunsDir, userdata.RunFilter{Kind: f.Kind, Name: f.Name, Since: f.Since})
		if err != nil {
			return nil, err
		}
		for _, h := range heads {
			if e.liveStatus(h) {
				h.Status = string(pipeline.Interrupted)
			}
			add(h)
		}
	} else {
		for _, r := range e.Recent() {
			if r.Preview == 0 {
				add(summaryOf(&r, r.Ended))
			}
		}
	}
	slices.SortFunc(out, func(a, b Summary) int {
		if c := b.Started.Compare(a.Started); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// liveStatus reports whether head says running or queued but its writer is
// gone: it is shown interrupted.
func (e *Engine) liveStatus(h Summary) bool {
	return (h.Status == string(pipeline.Running) || h.Status == string(pipeline.Queued)) && !e.liveElsewhere(h)
}

func matches(s Summary, f userdata.RunFilter) bool {
	return (f.Kind == "" || s.Kind == f.Kind) && (f.Name == "" || s.Name == f.Name) &&
		(f.Status == "" || s.Status == f.Status) && (f.Since.IsZero() || !s.Started.Before(f.Since))
}

// summaryOf is a run's header as a listing row.
func summaryOf(r *Run, mod time.Time) Summary {
	s := Summary{ID: r.ID, Kind: r.Kind, Name: r.Name, Trigger: r.Trigger, By: r.By, Status: string(r.Status),
		Started: r.Started, Ended: r.Ended, Error: r.Error, Resumes: r.Resumes, Mod: mod}
	for _, p := range r.Pipelines {
		s.Rows += p.Rows()
	}
	return s
}

// LastRun is the newest record of a job or pipeline on disk, for the
// scheduler's catch-up and a listing's "last run"; ok false when none.
func (e *Engine) LastRun(kind, name string) (Summary, bool) {
	hs, err := e.History(userdata.RunFilter{Kind: kind, Name: name, Limit: 1})
	if err != nil || len(hs) == 0 {
		return Summary{}, false
	}
	return hs[0], true
}
