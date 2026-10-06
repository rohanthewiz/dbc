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
	"github.com/rohanthewiz/dbc/userdata"
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
	err := w.hist.AddEntry(userdata.Entry{At: time.Now(), Conn: conn, SQL: stmt, DB: w.dbKey(conn)})
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

// dbKey is the database connection conn lands on, as a history entry
// records it (userdata.Entry.DB): db.ConsoleTarget's host and database,
// keyed the way the consoles directory is, so a database's history and its
// consoles are scoped alike. "" for no connection, or a name the config no
// longer knows.
func (w *Workspace) dbKey(conn string) string {
	if conn == "" {
		return ""
	}
	cc, ok := w.cfg.ConnByName(conn)
	if !ok {
		return ""
	}
	t := db.ConsoleTarget(cc)
	return userdata.ConsoleDBOf(t.Host, t.Database).Key()
}

// HistoryKey is the active connection's database as history records it —
// what the history pickers (the TUI's Ctrl+P, dbc web's) scope to when they
// open on "this database".
func (w *Workspace) HistoryKey() string { return w.dbKey(w.Active()) }

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

// ShowColumns lists a table's information_schema.columns rows in the grid,
// where they can be read, sorted and copied like any result. qname is the
// name the sidebar shows for the table (see db.TableIndex.Lookup).
//
// It goes through RunStmts, like a table preview: the statement is recorded
// in the history, because it is what the user would have typed and is the
// easiest way back to it (or to an edited SELECT * of it). It is not put in
// the editor, whose text is the user's.
//
// Only a table in the active connection's catalog is accepted. The name is
// inlined into SQL, so a name from anywhere else is refused, not escaped
// and tried.
func (w *Workspace) ShowColumns(qname string) (Start, error) {
	cc, ok := w.cfg.ConnByName(w.Active())
	if !ok {
		return Start{}, refuse(NoConnection, Warn, "no active connection — pick one in the sidebar")
	}
	t, ok := w.TableIndex().Lookup(qname)
	if !ok {
		return Start{}, refuse(Invalid, Warn, "no table %q on %s", qname, cc.Name)
	}
	q, err := db.InfoColumnsQuery(cc.Driver, t)
	if err != nil {
		return Start{}, refuse(Invalid, Err, "%s", serr.StringFromErr(err))
	}
	return w.RunStmts([]string{q}, "columns "+qname)
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
	w.runStep, w.runSteps = 0, len(stmts)
	job := func() Event {
		var res *model.Result
		var err error
		var notes []Note // the server's notices, in the order raised
		wrote := false
		for i, stmt := range stmts {
			w.stepTo(gen, i+1)
			var notices []db.Notice
			res, notices, err = w.runOnSession(ctx, conn, stmt)
			notes = append(notes, noticeNotes(notices)...)
			// A failed statement may still have written (a stop
			// mid-INSERT on a driver without transactional DDL, a
			// multi-row write cut short), so it counts as well as
			// the ones that succeeded.
			wrote = wrote || db.ChangesRows(stmt)
			if err != nil {
				if len(stmts) > 1 {
					err = serr.Wrap(err, "statement", fmt.Sprintf("%d/%d", i+1, len(stmts)))
				}
				res = nil
				break
			}
		}
		// The notices go in first: landRun appends the closing "completed"
		// or failure note after them, so the log reads in the order things
		// happened, the RAISE lines before the run's outcome.
		ev := &RunDone{Tag: tag, Conn: conn, Stmts: stmts, Result: res, Err: err, Notes: notes}
		w.landRun(ev, gen, wrote)
		return ev
	}
	return Start{
		Tag: tag, Gen: gen, Job: job,
		Notes: []Note{notef(Info, "running %s on %s — %s", tag, conn, Preview(strings.Join(stmts, "; ")))},
	}, nil
}

// noticeNotes turns server notices (RAISE NOTICE and its kin) into log
// lines. A WARNING is shown as one; the chattier severities (NOTICE, INFO,
// LOG, DEBUG) as Info, since they are what the user asked the server to
// say, not trouble.
func noticeNotes(ns []db.Notice) []Note {
	var out []Note
	for _, n := range ns {
		l := Info
		if n.Severity == "WARNING" {
			l = Warn
		}
		out = append(out, Note{Level: l, Text: n.String()})
	}
	return out
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
		// what a script ran is not known here, so it is taken to have
		// written: a script is more often a data chore than a report
		w.landRun(ev, gen, true)
		return ev
	}
	return Start{Tag: tag, Gen: gen, Job: job, Notes: []Note{notef(Info, "running %s", tag)}}, nil
}

// stepTo records that run gen has reached statement step, for the status
// line and a UI's progress. A straggler (gen no longer current) writes
// nothing: the slot belongs to a newer run.
func (w *Workspace) stepTo(gen, step int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if gen == w.runGen {
		w.runStep = step
	}
}

// beginRunLocked claims the run slot, or refuses when a run is already in
// flight.
func (w *Workspace) beginRunLocked(tag string) (context.Context, error) {
	if w.busy {
		return nil, refuse(Busy, Warn, "busy — %s is still running (Ctrl+K stops it)", w.runTag)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.busy, w.cancel, w.runTag, w.runAt = true, cancel, tag, time.Now()
	w.runStep, w.runSteps = 0, 0 // Run sets them for a list of statements
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

// runningStatusLocked is the status line of the run in flight: its tag and
// elapsed time and, for several statements, which one is executing — "all
// 4 statements · 2/4 1.3s" — so a long script shows it is moving.
func (w *Workspace) runningStatusLocked() string {
	took := time.Since(w.runAt).Round(100 * time.Millisecond)
	if w.runSteps > 1 && w.runStep > 0 {
		return fmt.Sprintf("%s · %d/%d %s", w.runTag, w.runStep, w.runSteps, took)
	}
	return fmt.Sprintf("%s %s", w.runTag, took)
}

// landRun installs a run's outcome. wrote is whether the run may have
// changed rows (db.ChangesRows of any statement it reached, or a script),
// for the sidebar's counts: see recountLocked.
func (w *Workspace) landRun(ev *RunDone, gen int, wrote bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if gen != w.runGen {
		ev.Stale = true // a straggler from a run that was already written off
		return
	}
	if wrote {
		ev.Wrote = true
		ev.Counts = w.recountLocked(ev.Conn)
	}
	w.dropCompletionsAfterRunLocked(ev)
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
	ev.Notes = append(ev.Notes, doneNote(ev))
}

// doneNote is the log line that closes a successful run, the pair of
// Run's "running … on …" note. The status bar also gets a summary, but it
// sits away from the log and reads the same before and after a rerun that
// returns the same count, so on its own a finished run looked like one
// still going: the log's last line was still "running …".
//
// The time is the whole run's (Elapsed, the slot held), not the last
// statement's Result.Duration: it is what the user waited. The rows are
// the count fetched; the "showing N" cap is a UI's (max_display_rows), and
// each UI logs that itself.
//
//	statement 3/3 completed on Pilot in 1.234s — 42 rows
//	3 statements completed on Pilot in 2.5s — showing the last result: 1 affected
func doneNote(ev *RunDone) Note {
	took := ev.Elapsed.Round(time.Millisecond)
	what := ev.Tag
	if len(ev.Stmts) > 1 {
		what = fmt.Sprintf("%d statements", len(ev.Stmts))
	}
	summary := ""
	if r := ev.Result; r != nil {
		summary = fmt.Sprintf("%d rows", len(r.Rows))
		if r.IsExec {
			summary = fmt.Sprintf("%d affected", r.Affected)
		}
	}
	switch {
	case summary == "": // a driver handed back no result: say it finished all the same
		return notef(Ok, "%s completed on %s in %s", what, ev.Conn, took)
	case len(ev.Stmts) > 1:
		return notef(Ok, "%s completed on %s in %s — showing the last result: %s", what, ev.Conn, took, summary)
	default:
		return notef(Ok, "%s completed on %s in %s — %s", what, ev.Conn, took, summary)
	}
}

// recountLocked makes the Job that refreshes the sidebar's row counts after
// a run on conn that may have changed rows, or nil when the sidebar is not
// showing conn's tables (the run's connection was left meanwhile, or its
// catalog never loaded). The caller holds mu.
//
// The Manager's cached counts for conn are dropped first: they are shared,
// cached for minutes, and would otherwise hand the same pre-write numbers
// straight back. A counting already in flight for this sidebar is canceled
// — it may have read the rows before the write — and lands Stale.
//
// The drop happens whether or not this sidebar shows counts, or shows
// conn's tables at all: another workspace on the same Manager may show
// them (dbc web's other tabs, see Recount), and must not be served the
// pre-write numbers from the cache. Only the recount itself is gated, by
// countsJobLocked, on this sidebar's switch (ShowRowCounts).
//
// The whole listed catalog is recounted, not just the tables the
// statements name: a write reaches tables its text does not (a cascade, a
// trigger, a function), and a sidebar lists at most one schema's tables on
// a big server (db.Navigable), so this costs what a connect's counting does.
//
// A write inside an open transaction is not visible to the pool the counts
// read through, so its recount shows the old numbers; the COMMIT that ends
// it is itself a run that recounts (db.ChangesRows).
func (w *Workspace) recountLocked(conn string) Job {
	w.mgr.ForgetRowCounts(conn)
	if conn != w.active || w.catalog == nil {
		return nil
	}
	return w.refreshCountsLocked()
}

// Recount refreshes the sidebar's row counts after another workspace's run
// on conn may have changed rows, or returns nil when this sidebar is not
// showing conn's tables. A UI that hosts several workspaces on one Manager
// (dbc web's query tabs and windows) calls it on the others when a RunDone
// reports Wrote, so every sidebar on the connection shows the write, not
// only the one that made it — whether or not the writer's own sidebar
// counts. It returns nil, too, when this sidebar's counts are off.
//
// Unlike recountLocked it does not drop the Manager's cached counts: the
// writer's landRun already did, before its RunDone was delivered, so what
// is cached now was counted after the write. Dropping again would only
// make each workspace count afresh instead of sharing the writer's
// counting (db.Manager.RowCounts waits for one in flight).
func (w *Workspace) Recount(conn string) Job {
	w.mu.Lock()
	defer w.mu.Unlock()
	if conn != w.active || w.catalog == nil {
		return nil
	}
	return w.refreshCountsLocked()
}

// refreshCountsLocked cancels a counting in flight for this sidebar (it
// may have read the rows before the write) and makes the Job that counts
// the listed catalog again.
func (w *Workspace) refreshCountsLocked() Job {
	if w.countCancel != nil {
		w.countCancel()
		w.countCancel = nil
	}
	return w.countsJobLocked(w.active, w.connGen, db.TableRefs(w.catalog.Rows))
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
