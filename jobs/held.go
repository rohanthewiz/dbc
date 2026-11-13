package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/userdata"
)

// HELD: THE OVERLAP RULES ACROSS PROCESSES (N-269). One real run of a
// pipeline at a time, and a job's overlap policy, are kept in memory
// (Engine.pipes, Engine.jobLast): per engine, so per process. Runs come
// from several. Cron's `dbc job run nightly` every five minutes beside a
// seven-minute run, or beside dbc web firing the same job: each process
// saw nothing running, and both loaded one table. So every hold is also an
// OS file lock, one per name, which every engine on the machine takes:
//
//	runs_dir/.held/
//	    pipeline-orders.lock   a real run of pipeline orders, alone or as a job's step
//	    job-nightly.lock       a run of job nightly, walking its steps
//	    queue-nightly.lock     a run of job nightly waiting for the one before it (overlap: queue)
//
// The lock is btypedb.AcquireLock's, as userdata.lockFile and config's
// saved connections use it: flock on Unix, a share mode on Windows. The OS
// drops it when its holder exits, however it exits (kill -9 included), so
// unlike the records' heartbeat it needs no staleness rule: held means
// alive. And a flock belongs to the open file, not the process, so two
// engines in one process (a script.run node's private engine beside the
// host's) exclude each other as two processes would.
//
// WHY THE IN-MEMORY CHECK STAYS, AND COMES FIRST. This engine's own run of
// a name would make a second flock fail, and read as "another process";
// the engine's own state says better what is running, and a run queued
// behind one here waits on that run's done rather than on a poll. The
// file lock is taken only once the in-memory check passed, under e.mu, so
// of two runs in this process at most one ever tries for it.
//
//	StartPipeline / runStep:  holdLocked(name) ─► holdFileLocked(pipeline-name)
//	StartJob, none here:      holdFileLocked(job-name)
//	                            held elsewhere, skip  ─► refused
//	                            held elsewhere, queue ─► holdFileLocked(queue-name), run queued
//	StartJob, one here:       skip ─► refused;  queue ─► holdFileLocked(queue-name), run queued
//	executeJob, queued:       wait for the one here (done), then poll job-name;
//	                          once taken, drop queue-name, status running
//	finish:                   drop every lock the run holds
//
// The queue lock carries "one waits at most" across processes too: a cron
// that fires every five minutes beside a twenty-minute run would
// otherwise pile up a waiting process per fire.
//
// The files are never removed. Unlinking a lock file another process has
// open, and is about to flock, lets a third create a fresh file at the
// path and lock that, and two then hold "the" lock. One small file per name
// ever run is the price.
//
// Previews take no locks (they write nothing), nor does an engine with no
// runs dir (a test's): there is nothing to share it with. The scheduler's
// .fires claims stay as they were. They decide which of two dbc webs fires
// 02:00, which a lock cannot: under overlap "queue" the second would wait
// for the first and then run the job again.
const heldDir = ".held"

// Lock names: a prefix, then the pipeline's or job's name. A name holds
// only letters, digits, . _ - (pipeline.ValidName), and the prefixes differ
// in their first letter, so no two kinds' names can meet: "job-x.y" is
// never "queue-…" or "pipeline-…", however x and y are spelled.
const (
	lockPipeline = "pipeline-"
	lockJob      = "job-"
	lockQueue    = "queue-"
)

// heldPoll is how often a queued run tries for its job's lock while
// another process holds it. A flock that fails costs an open and a
// syscall; a second is no delay to a job that waited for another run. A
// var so the tests need not wait that long.
var heldPoll = time.Second

// heldLock is one file lock the engine holds, and the run it holds it for.
type heldLock struct {
	run string
	c   io.Closer
}

// holdFileLocked takes lock prefix+name for run. Under e.mu.
//
//	taken, or no lock applies   busy false
//	another holder has it       busy true; pid is the holder's as its lock
//	                            file records it ("" when unreadable)
//	no lock could be made       busy false; warn says why
//
// A lock that cannot be made (a read-only runs dir, say) does not refuse
// the run. The in-process rule still holds, which is all dbc had before,
// and refusing every run over a lock file would be the worse failure. The
// caller tells warn once it has let go of e.mu (a Sink may call back in).
func (e *Engine) holdFileLocked(prefix, name, run string) (busy bool, pid string, warn error) {
	if e.opt.RunsDir == "" || !pipeline.ValidName(name) {
		// an invalid name never gets here (Check refuses it first); were it
		// to, it must not become a path outside .held
		return false, "", nil
	}
	key := prefix + name
	if h, ok := e.held[key]; ok {
		// already this engine's. The in-memory checks before every call
		// mean it is never another run's; if it were, that run is here.
		return h.run != run, strconv.Itoa(os.Getpid()), nil
	}
	dir := filepath.Join(e.opt.RunsDir, heldDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, "", serr.Wrap(err, "op", "make the run locks' directory", "dir", dir)
	}
	c, err := btypedb.AcquireLock(filepath.Join(dir, key))
	switch {
	case err == nil:
		e.held[key] = heldLock{run: run, c: c}
		return false, "", nil
	case errors.Is(err, btypedb.ErrLocked):
		return true, serr.SErrFromErr(err).FieldsMap()["holder_pid"], nil
	}
	return false, "", serr.Wrap(err, "op", "take the run lock", "lock", key)
}

// dropFileLocked lets go of lock prefix+name if run holds it. Under e.mu.
func (e *Engine) dropFileLocked(prefix, name, run string) {
	key := prefix + name
	if h, ok := e.held[key]; ok && h.run == run {
		_ = h.c.Close()
		delete(e.held, key)
	}
}

// dropRunFilesLocked lets go of every lock run holds: its end (finish),
// or its abandoning. Under e.mu.
func (e *Engine) dropRunFilesLocked(run string) {
	for key, h := range e.held {
		if h.run == run {
			_ = h.c.Close()
			delete(e.held, key)
		}
	}
}

// warnLock tells a lock that could not be made (holdFileLocked's warn):
// the run goes on, guarded within this process only.
func (e *Engine) warnLock(run, name string, warn error) {
	if warn == nil {
		return
	}
	e.send(&Notice{Run: run, Job: name, Level: "warn", Text: "could not take the run lock of " + name +
		", so another process could run it at once (only this one's runs are kept apart): " + errText(warn)})
}

// pipelineElsewhere is the refusal of a real run of pipeline name whose
// lock another engine holds (pid as its lock file says), in the words
// holdLocked uses for one held here.
func (e *Engine) pipelineElsewhere(name, pid string) error {
	h := e.elsewhere(lockPipeline, name, pid)
	return &busyError{msg: fmt.Sprintf("%s is already running %s%s — one run of a pipeline at a time",
		name, h.step, h.said()), run: h.run}
}

// jobElsewhere is the refusal of a run of job name over lock prefix (the
// job's, under overlap "skip"; its queue's, under "queue") another engine
// holds, in the words StartJob uses for one held here.
func (e *Engine) jobElsewhere(name, prefix, pid string) error {
	h := e.elsewhere(prefix, name, pid)
	if prefix == lockQueue {
		return &busyError{msg: fmt.Sprintf("a run of job %s is already waiting for the one before it, %s",
			name, h.said("overlap: queue holds one")), run: h.run}
	}
	return &busyError{msg: fmt.Sprintf("job %s is already running %s", name, h.said("overlap: skip")), run: h.run}
}

// heldBy is what is known of a lock's other holder, for a message.
type heldBy struct {
	where   string   // "in another process", or "elsewhere in this process"
	details []string // "pid 4242", "run 20261113-020000-7f3a", as known
	step    string   // a pipeline's: "as step load of job nightly ", or ""
	run     string   // the holder's run id when a record says it (BusyRun)
}

// said is where the holder is, its details and the message's own notes in
// one parenthesis: "in another process (pid 4242, run …; overlap: skip)".
func (h heldBy) said(notes ...string) string {
	var parts []string
	if len(h.details) > 0 {
		parts = append(parts, strings.Join(h.details, ", "))
	}
	parts = append(parts, notes...)
	if len(parts) == 0 {
		return h.where
	}
	return h.where + " (" + strings.Join(parts, "; ") + ")"
}

// elsewhere finds what a refusal can say of a lock another holder has:
// its PID, from the lock file; and when a record says which run holds it,
// that run, and in a pipeline's case the job step running it. Not under
// e.mu: it reads the records.
func (e *Engine) elsewhere(prefix, name, pid string) heldBy {
	h := heldBy{where: "in another process"}
	if pid == strconv.Itoa(os.Getpid()) {
		h.where = "elsewhere in this process" // another engine here: a script.run node's
	}
	if pid != "" {
		h.details = append(h.details, "pid "+pid)
	}
	if r, ok := e.holderRecord(prefix, name); ok {
		h.run = r.ID
		h.details = append(h.details, "run "+r.ID)
		if r.Kind == KindJob && prefix == lockPipeline {
			for _, p := range r.Pipelines {
				if p.Status == pipeline.Running && p.Pipeline == name {
					h.step = "as step " + p.ID + " of job " + r.Name + " "
					break
				}
			}
		}
	}
	return h
}

// holderRecord finds the run another engine holds lock prefix+name for,
// by its record: the newest whose writer is alive and which says it is
// running that job or pipeline (queued, for a queue lock). A pipeline is
// held by a run of it alone or by a job whose step is running it, which
// only the job's whole record says. ok is false when no record says so:
// the holder took its lock and has not written its record yet, or keeps
// none (no runs dir there).
func (e *Engine) holderRecord(prefix, name string) (Run, bool) {
	heads, err := userdata.ListRuns(e.opt.RunsDir, userdata.RunFilter{})
	if err != nil {
		return Run{}, false
	}
	want := pipeline.Running
	if prefix == lockQueue {
		want = pipeline.Queued
	}
	for _, h := range heads {
		if h.Status != string(want) || e.isLive(h.ID) || !e.liveElsewhere(h) {
			continue
		}
		switch {
		case prefix != lockPipeline && h.Kind == KindJob && h.Name == name,
			prefix == lockPipeline && h.Kind == KindPipeline && h.Name == name:
			r, _ := e.fromDisk(h.ID)
			r.ID, r.Kind, r.Name = h.ID, h.Kind, h.Name // whether or not the read worked
			return r, true
		case prefix == lockPipeline && h.Kind == KindJob:
			bs, _, err := userdata.ReadRun(e.opt.RunsDir, h.ID)
			var r Run
			if err != nil || json.Unmarshal(bs, &r) != nil {
				continue
			}
			for _, p := range r.Pipelines {
				if p.Status == pipeline.Running && p.Pipeline == name {
					return r, true
				}
			}
		}
	}
	return Run{}, false
}
