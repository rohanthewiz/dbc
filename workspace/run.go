package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/sdb"
)

// Running statements and scripts. The rules are the former tview UI's,
// carried over unchanged because they were each learned the hard way:
//
//   - ONE RUN AT A TIME. A second run while one is in flight is refused in
//     words, not queued: a queued DELETE behind a slow SELECT is a
//     surprise.
//   - A PINNED SESSION per active connection — see session.go.
//   - CANCEL REACHES THE SERVER. Every run has a context; Cancel cancels it,
//     and the drivers turn that into a server-side cancel.
//
// A run's life, in the run slot:
//
//	beginRunLocked ── busy, runGen++, cancel ──► Job runs ──► landRun
//	                                                            │ gen == runGen? no ─► Stale
//	                                                            ▼ yes
//	                                             endRunLocked: busy = false, elapsed
//	                                             last* updated, notes written

// RunEditor is Ctrl+R (all false) or Ctrl+Shift+R (all true) on what the
// editor holds.
func (w *Workspace) RunEditor(ed Editor, all bool) (Start, error) {
	stmts, tag := Pick(ed)
	if all {
		stmts, tag = PickAll(ed.Text)
	}
	return w.RunStmts(stmts, tag)
}

// RunStmts records statements in the history and runs them. It is how
// anything the user asked to run by hand gets run: Ctrl+R, run all, a
// table preview.
func (w *Workspace) RunStmts(stmts []string, tag string) (Start, error) {
	if len(stmts) == 0 {
		return Start{}, refuse(Nothing, Warn, "nothing to run — type a query first")
	}
	// recorded before the run, even one about to be refused: the query
	// worth recalling is very often the one that just failed
	var notes []Note
	for _, s := range stmts {
		if n, ok := w.record(s); ok {
			notes = append(notes, n)
		}
	}
	st, err := w.Run(stmts, tag)
	st.Notes = append(notes, st.Notes...)
	return st, err
}

// record adds a statement to the history. A write failure is reported
// once, as a Note; the entry is kept in memory regardless.
func (w *Workspace) record(stmt string) (Note, bool) {
	w.mu.Lock()
	conn := w.active
	w.mu.Unlock()
	err := w.hist.Add(conn, stmt, time.Now())
	if err == nil {
		return Note{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.histWarned {
		return Note{}, false
	}
	w.histWarned = true
	return notef(Warn, "query history is not being saved: %s", serr.StringFromErr(err)), true
}

// ListTables is Ctrl+T: the active driver's catalog query, through the same
// path a run takes, so it lands in the grid and exports like any result. It
// is the app's query, not the user's, so it is not recorded.
func (w *Workspace) ListTables() (Start, error) {
	cc, ok := w.cfg.ConnByName(w.Active())
	if !ok {
		return Start{}, refuse(NoConnection, Warn, "no active connection — pick one in the sidebar")
	}
	q, err := db.TablesQuery(cc.Driver)
	if err != nil {
		return Start{}, refuse(Invalid, Err, "%s", serr.StringFromErr(err))
	}
	return w.Run([]string{q}, "list tables")
}

// Run executes statements in order on the pinned session, stopping at the
// first failure, and publishes the last result. The Job's event is a
// *RunDone.
func (w *Workspace) Run(stmts []string, tag string) (Start, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active == "" {
		return Start{}, refuse(NoConnection, Warn, "no active connection — pick one in the sidebar")
	}
	ctx, err := w.beginRunLocked(tag)
	if err != nil {
		return Start{}, err
	}
	conn, gen := w.active, w.runGen
	job := func() Event {
		var res *model.Result
		var err error
		for i, stmt := range stmts {
			res, err = w.runOnSession(ctx, conn, stmt)
			if err != nil {
				if len(stmts) > 1 {
					err = serr.Wrap(err, "statement", fmt.Sprintf("%d/%d", i+1, len(stmts)))
				}
				res = nil
				break
			}
		}
		ev := &RunDone{Tag: tag, Conn: conn, Stmts: stmts, Result: res, Err: err}
		w.landRun(ev, gen)
		return ev
	}
	return Start{
		Tag: tag, Gen: gen, Job: job,
		Notes: []Note{notef(Info, "running %s on %s — %s", tag, conn, Preview(strings.Join(stmts, "; ")))},
	}, nil
}

// RunScript runs a Go script. Its s.Show and s.Print fire from the script's
// goroutine mid-run and go to the sink as ScriptShow and ScriptPrint; the
// Job's event is a *RunDone with Script set.
func (w *Workspace) RunScript(path string) (Start, error) {
	tag := "script " + filepath.Base(path)
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx, err := w.beginRunLocked(tag)
	if err != nil {
		return Start{}, err
	}
	gen, conn := w.runGen, w.active
	s := sdb.New(w.mgr,
		func(r *model.Result) {
			if r != nil {
				w.mu.Lock()
				w.lastRes = r
				w.mu.Unlock()
			}
			w.emit(&ScriptShow{Result: r})
		},
		func(msg string) { w.emit(&ScriptPrint{Text: msg}) },
	).WithContext(ctx)
	job := func() Event {
		ev := &RunDone{Tag: tag, Conn: conn, Script: true, Err: script.Run(path, s)}
		w.landRun(ev, gen)
		return ev
	}
	return Start{Tag: tag, Gen: gen, Job: job, Notes: []Note{notef(Info, "running %s", tag)}}, nil
}

// beginRunLocked claims the run slot, or refuses when a run is already in
// flight.
func (w *Workspace) beginRunLocked(tag string) (context.Context, error) {
	if w.busy {
		return nil, refuse(Busy, Warn, "busy — %s is still running (Ctrl+K stops it)", w.runTag)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.busy, w.cancel, w.runTag, w.runAt = true, cancel, tag, time.Now()
	w.runGen++
	return ctx, nil
}

// endRunLocked releases the slot and returns how long the run took.
func (w *Workspace) endRunLocked() time.Duration {
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
	w.busy = false
	return time.Since(w.runAt)
}

func (w *Workspace) runningStatusLocked() string {
	return fmt.Sprintf("%s %s", w.runTag, time.Since(w.runAt).Round(100*time.Millisecond))
}

// landRun installs a run's outcome.
func (w *Workspace) landRun(ev *RunDone, gen int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if gen != w.runGen {
		ev.Stale = true // a straggler from a run that was already written off
		return
	}
	ev.Elapsed = w.endRunLocked()
	if len(ev.Stmts) > 0 {
		w.lastStmt = ev.Stmts[len(ev.Stmts)-1]
	}
	if ev.Err != nil {
		w.failedLocked(ev.Err, ev.Tag, ev.Elapsed, &ev.Notes, &ev.Status)
		return
	}
	w.lastErr = ""
	if ev.Script {
		ev.Notes = append(ev.Notes, notef(Ok, "%s completed in %s", ev.Tag, ev.Elapsed.Round(time.Millisecond)))
		return
	}
	if ev.Result != nil {
		w.lastRes = ev.Result
		// a result that is itself a plan — the output of an EXPLAIN the
		// user ran — becomes the plan too, so a UI can show it as one
		cc, _ := w.cfg.ConnByName(ev.Result.Conn)
		if p, ok := explain.Detect(ev.Result, cc.Driver); ok {
			ev.Plan, w.plan = p, p
		}
	}
	if len(ev.Stmts) > 1 {
		ev.Notes = append(ev.Notes, notef(Ok, "%d statements completed — showing the last result", len(ev.Stmts)))
	}
}

// failedLocked writes a failed or stopped run's notes and status, and
// remembers a real failure for the assistant to explain. A stop is the
// user's choice, not a failure: it is not remembered as the last error.
func (w *Workspace) failedLocked(err error, tag string, elapsed time.Duration, notes *[]Note, status *string) {
	took := elapsed.Round(time.Millisecond)
	if errors.Is(err, db.ErrCanceled) {
		*notes = append(*notes, notef(Warn, "%s stopped after %s", tag, took))
		*status = fmt.Sprintf("stopped after %s", took)
		// Stopping a statement can cost the connection (the driver may
		// close it to abort the statement). When the session held state,
		// that is worth more than "stopped" alone.
		if errors.Is(err, db.ErrSessionLost) {
			*notes = append(*notes, Note{Warn, "the session was lost with it — its open transaction, SET values and temp tables are gone"})
		}
		return
	}
	w.lastErr = serr.StringFromErr(err)
	*notes = append(*notes, Note{Err, w.lastErr})
	*status = fmt.Sprintf("error after %s — ✦ ask the assistant why", took)
}

// Cancel is Ctrl+K and the Stop button: it stops the run in flight or,
// with none, the connect in flight. The Note says what it did; status is
// for a status bar ("" leaves it). The stopped work still lands, as stopped.
func (w *Workspace) Cancel() (n Note, status string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.busy && w.cancel != nil {
		w.cancel()
		return notef(Warn, "stopping %s…", w.runTag), "stopping " + w.runTag + "…"
	}
	// no run, but a connect may be dialing — Cancel stops that too
	if n, ok := w.cancelConnectLocked(); ok {
		return n, ""
	}
	return Note{Warn, "nothing is running"}, ""
}
