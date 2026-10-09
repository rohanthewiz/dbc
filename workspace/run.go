package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/sdb"
	"github.com/rohanthewiz/dbc/sqlsplit"
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
	return w.runLocked(stmts, tag, nil)
}

// runLocked is Run with mu held. again is the result tab the run is a
// rerun of (RerunResultTab), or nil for any other run: with it, the run
// lands in that tab whatever the tab on screen is, kept or not, and its
// result keeps the tab's title.
func (w *Workspace) runLocked(stmts []string, tag string, again *resultTab) (Start, error) {
	if w.active == "" {
		return Start{}, refuse(NoConnection, Warn, "no active connection — pick one in the sidebar")
	}
	if w.busy {
		return Start{}, refuse(Busy, Warn, "busy — %s is still running (Ctrl+K stops it)", w.runTag)
	}
	// the result tab this run will land in is picked now, not when it lands
	// (results.go) — and a run with nowhere to land is refused before it
	// costs anything. A rerun needs no pick: it lands where its statement's
	// result already is, so it never needs room either.
	target := again
	if again == nil {
		var err error
		if target, err = w.targetLocked(w.active); err != nil {
			return Start{}, err
		}
	}
	ctx, err := w.beginRunLocked(tag)
	if err != nil {
		return Start{}, err
	}
	w.runTarget = target
	// the title is read now, under mu, for the job to use without it: the
	// tab's title is the workspace's to change, and the job runs unlocked
	keepTitle := ""
	if again != nil {
		w.runAgain, w.runTitle, keepTitle = again, again.title, again.title
	}
	titleOf := func(stmt string) string {
		if keepTitle != "" {
			return keepTitle
		}
		return resultTitle(tag, stmt)
	}
	conn, gen := w.active, w.runGen
	w.runStep, w.runSteps = 0, len(stmts)
	// read now, under mu: no more results than this can be on screen at
	// once, so the job keeps no more (see rows below)
	limit := w.cfg.ResultTabLimit()
	job := func() Event {
		var res *model.Result
		var err error
		var notes []Note // the server's notices, in the order raised
		var eff runEffects
		// rows are the results that get a tab each (results.go): every
		// statement's that returned rows, the last limit of them — the
		// earlier ones would only be dropped at landing by the cap, and
		// holding a run-all's every result set until then is memory for
		// nothing. over counts the ones let go here.
		var rows []landing
		over := 0
		// a write gets no tab, so its count is logged instead — in a run
		// of several statements only; a single one's is in the done note
		wl := writeLog{on: len(stmts) > 1}
		for i, stmt := range stmts {
			w.stepTo(gen, i+1)
			var notices []db.Notice
			res, notices, err = w.runOnSession(ctx, conn, stmt)
			notes = append(notes, noticeNotes(notices)...)
			// A failed statement may still have written (a stop
			// mid-INSERT on a driver without transactional DDL, a
			// multi-row write cut short), so it counts as well as
			// the ones that succeeded.
			eff.see(stmt)
			if err != nil {
				if len(stmts) > 1 {
					err = serr.Wrap(err, "statement", fmt.Sprintf("%d/%d", i+1, len(stmts)))
				}
				res = nil
				break
			}
			if res != nil && res.IsExec {
				notes = append(notes, wl.see(i+1, len(stmts), stmt, res)...)
			}
			if res != nil && !res.IsExec {
				rows = append(rows, landing{title: titleOf(stmt), stmt: stmt, res: res, n: i + 1})
				if len(rows) > limit {
					rows = slices.Delete(rows, 0, 1)
					over++
				}
			}
		}
		// the folded writes' line closes the per-statement lines, ahead
		// of the run's outcome (a failure's error included)
		notes = append(notes, wl.fold()...)
		// The notices go in first: landRun appends the closing "completed"
		// or failure note after them, so the log reads in the order things
		// happened, the RAISE lines before the run's outcome.
		ev := &RunDone{Tag: tag, Conn: conn, Stmts: stmts, Result: res, Err: err, Notes: notes}
		w.landRun(ev, gen, eff, rows, over)
		return ev
	}
	return Start{
		Tag: tag, Gen: gen, Job: job,
		Notes: []Note{notef(Info, "running %s on %s — %s", tag, conn, Preview(strings.Join(stmts, "; ")))},
	}, nil
}

// writeLines is how many writes of one run get a log line each; the rest
// are folded into one line (writeLog.fold), so a script of 500 INSERTs
// adds six lines to the log, not 500.
const writeLines = 5

// writeLog writes the log lines that give each write of a run of several
// statements its count. Since a write gets no result tab (results.go), and
// the done note names the result on screen, a write's "n affected" had no
// other place: after `SELECT …; UPDATE …` the UPDATE's count was lost.
//
// "Write" here is a statement that may change rows or the catalog
// (db.ChangesRows): a SET or a BEGIN gets no line, and nor does the
// COMMIT or ROLLBACK that ends a transaction — "0 affected" after either
// is noise. Statements that return rows get none either: each has its tab.
// A failed statement gets none: the error says what happened to it.
//
//	statement 2/500: 3 affected — UPDATE t SET …
//	statement 3/500: done — CREATE TABLE u (…)      (DDL: no count worth giving)
//	…                                                 (writeLines lines in all)
//	… 495 more writes, to statement 500: 495 affected in all
//
// The fold line totals the affected counts; a DDL among them adds nothing
// to it. It comes once the run is over, so a stopped or failed run still
// gets it, for the writes that went through.
type writeLog struct {
	on       bool  // the run has several statements
	logged   int   // writes given a line of their own
	folded   int   // writes past writeLines
	affected int64 // the folded writes' affected rows, summed
	last     int   // the last folded write's statement number
}

// see logs (or folds) statement n of total, which ran with result res.
func (l *writeLog) see(n, total int, stmt string, res *model.Result) []Note {
	if !l.on || !db.ChangesRows(stmt) || txnEndVerbs[sqlsplit.FirstKeyword(stmt)] {
		return nil
	}
	if l.logged >= writeLines {
		l.folded++
		l.affected += max(res.Affected, 0)
		l.last = n
		return nil
	}
	l.logged++
	what := fmt.Sprintf("%d affected", res.Affected)
	if sqlsplit.ChangesCatalog(stmt) {
		// a DDL's count means nothing: 0 on most drivers, and on SQLite
		// the last INSERT's, which sqlite3_changes still holds. A CREATE
		// TABLE … AS SELECT's rows go unsaid with it.
		what = "done"
	}
	return []Note{notef(Info, "statement %d/%d: %s — %s", n, total, what, Preview(stmt))}
}

// fold is the line for the writes past writeLines, or none.
func (l *writeLog) fold() []Note {
	if l.folded == 0 {
		return nil
	}
	return []Note{notef(Info, "… %s, to statement %d: %d affected in all",
		plural(l.folded, "more write", "more writes"), l.last, l.affected)}
}

// txnEndVerbs end a transaction: commitVerbs and the ways to roll back.
var txnEndVerbs = map[string]bool{"commit": true, "end": true, "rollback": true, "abort": true}

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
	if w.busy {
		return Start{}, refuse(Busy, Warn, "busy — %s is still running (Ctrl+K stops it)", w.runTag)
	}
	// its shows land as a run's result would: in the tab on screen now,
	// unless pinned (results.go); refused when they could land nowhere
	target, err := w.targetLocked(w.active)
	if err != nil {
		return Start{}, err
	}
	ctx, err := w.beginRunLocked(tag)
	if err != nil {
		return Start{}, err
	}
	gen, conn := w.runGen, w.active
	w.runTarget = target
	s := sdb.New(w.mgr,
		func(r *model.Result) {
			if r != nil {
				w.mu.Lock()
				// a straggling show from a run already written off (Stop,
				// then a new run) must not land in the new run's tab
				if gen == w.runGen {
					w.showLocked(conn, w.runTarget, tag, r)
				}
				w.mu.Unlock()
			}
			w.emit(&ScriptShow{Result: r, Conn: conn})
		},
		func(msg string) { w.emit(&ScriptPrint{Text: msg, Conn: conn}) },
	).WithContext(ctx).WithPaths(sdb.Paths{ScriptsDir: w.cfg.ScriptsDir, PipelinesDir: w.cfg.PipelinesDir})
	job := func() Event {
		ev := &RunDone{Tag: tag, Conn: conn, Script: true, ScriptPath: path, Err: script.Run(path, s)}
		// which rows a script wrote is not known here, so it is taken to
		// have written: a script is more often a data chore than a
		// report. Whether it changed the catalog is known — S notes each
		// catalog-changing statement it runs — and only conn's matters:
		// the sidebar lists no other connection's tables. Its statements
		// ran on the pool, each committed as it went, so none is left
		// waiting on a COMMIT (runEffects.open).
		// Its results have landed already, show by show.
		w.landRun(ev, gen, runEffects{wrote: true, catalog: s.CatalogChanged(conn)}, nil, 0)
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
	// Run and RunScript set the target after this; an explain lands no
	// result, so it keeps none, and no run's shows carry over
	w.runTarget, w.showTab = nil, nil
	w.runAgain, w.runTitle = nil, ""
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

// runEffects is what a run's statements may have done to what the sidebar
// shows, gathered from the statements it reached — the failing one
// included, since a failed statement may still have done its work in part.
type runEffects struct {
	// wrote: a statement may have changed rows (db.ChangesRows), or it
	// was a script — for the sidebar's counts (recountLocked).
	wrote bool
	// catalog: a statement may have changed the catalog
	// (sqlsplit.ChangesCatalog), or a script ran one on the run's
	// connection (sdb.S.CatalogChanged) — for the sidebar's list.
	catalog bool
	// open: such a statement ran after the run's last COMMIT, so it may be
	// inside a transaction still open on the session; committed: the run
	// ran a COMMIT (or Postgres's END). See relistAfterRunLocked.
	open, committed bool
}

// see adds one statement the run reached to e. The order matters for open:
// a CREATE then a COMMIT leaves nothing open, a COMMIT then a CREATE may.
func (e *runEffects) see(stmt string) {
	e.wrote = e.wrote || db.ChangesRows(stmt)
	switch {
	case sqlsplit.ChangesCatalog(stmt):
		e.catalog, e.open = true, true
	case commitVerbs[sqlsplit.FirstKeyword(stmt)]:
		e.committed, e.open = true, false
	}
}

// commitVerbs end a transaction by committing it. ROLLBACK is not among
// them: it puts the catalog back as it was before the transaction, which
// is what the pool — and so the relist that ran when the DDL did — saw.
var commitVerbs = map[string]bool{"commit": true, "end": true}

// landRun installs a run's outcome. eff is what its statements may have
// done (runEffects), for the sidebar: a list re-read when it changed the
// catalog (relistAfterRunLocked), otherwise a recount when it changed rows
// (recountLocked). rows are the results that get a tab each, oldest first,
// after over older ones were let go (Run).
//
// A failed or stopped run still lands the rows of the statements before
// the one that failed: they ran, and their results are as real as a
// successful run's — run-all with a typo in the third statement should not
// throw away the first two's.
func (w *Workspace) landRun(ev *RunDone, gen int, eff runEffects, rows []landing, over int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if gen != w.runGen {
		ev.Stale = true // a straggler from a run that was already written off
		return
	}
	// the relist is decided first: its landing counts the list it reads,
	// so a recount alongside it would only count the old list, to be
	// canceled when the relist lands (cancelCatalogWorkLocked)
	ev.Relist = w.relistAfterRunLocked(ev.Conn, eff)
	if eff.wrote {
		ev.Wrote = true
		if ev.Relist == nil {
			ev.Counts = w.recountLocked(ev.Conn)
		}
	}
	w.dropCompletionsAfterRunLocked(ev)
	ev.Elapsed = w.endRunLocked()
	// everything below lands in the run's own connection's result set,
	// which is not the active one if the tab switched meanwhile
	set := w.setLocked(ev.Conn)
	if len(ev.Stmts) > 0 {
		set.lastStmt, set.lastScript, set.lastScriptPath = ev.Stmts[len(ev.Stmts)-1], "", ""
	}
	if ev.Script {
		// A script's run replaces the last statement rather than leaving
		// it: the error and results below are the script's, and with the
		// old statement still "last", ChatContext would hand them to the
		// assistant as that statement's ("why did this SELECT fail?" with
		// the script's error). The tag is "script <name>" (RunScript).
		set.lastStmt, set.lastScript = "", strings.TrimPrefix(ev.Tag, "script ")
		set.lastScriptPath = ev.ScriptPath
	}
	if ev.Err != nil {
		w.failedLocked(set, ev.Err, ev.Tag, ev.Elapsed, &ev.Notes, &ev.Status)
		if len(rows) > 0 {
			w.landRowsLocked(ev, set, gen, rows, over)
			ev.Notes = append(ev.Notes, notef(Info, "%s from the statements before it landed in the result tabs",
				plural(len(rows), "result", "results")))
		}
		return
	}
	set.lastErr = ""
	if ev.Script {
		ev.Notes = append(ev.Notes, notef(Ok, "%s completed in %s", ev.Tag, ev.Elapsed.Round(time.Millisecond)))
		return
	}
	if len(rows) == 0 && ev.Result != nil {
		// no statement returned rows: the last one's result (its "n
		// affected") lands alone, as a single statement's always did
		last := ev.Stmts[len(ev.Stmts)-1]
		title := w.runTitle // a rerun's: the tab keeps its name
		if title == "" {
			title = resultTitle(ev.Tag, last)
		}
		rows = []landing{{title: title, stmt: last, res: ev.Result, n: len(ev.Stmts)}}
	}
	shown := 0
	if len(rows) > 0 {
		shown = w.landRowsLocked(ev, set, gen, rows, over)
	}
	ev.Notes = append(ev.Notes, doneNote(ev, shown))
}

// landRowsLocked puts a run's rows in their tabs (placeRunLocked), makes
// ev.Result the one now on screen — the last — and notes how many did not
// fit under result_tabs. It returns which statement's result is on screen
// (landing.n), for doneNote. The caller holds mu.
func (w *Workspace) landRowsLocked(ev *RunDone, set *resultSet, gen int, rows []landing, over int) int {
	_, dropped := w.placeRunLocked(ev.Conn, w.runTarget, gen, len(ev.Stmts) > 1, rows)
	last := rows[len(rows)-1]
	ev.Result, ev.Tabs = last.res, len(rows)-dropped
	if cut := over + dropped; cut > 0 {
		ev.Notes = append(ev.Notes, notef(Warn, "%d of the run's %d results did not fit in the result tabs — showing the last %d (result_tabs = %d)",
			cut, len(rows)+over, ev.Tabs, w.cfg.ResultTabLimit()))
	}
	// a result that is itself a plan — the output of an EXPLAIN the user
	// ran — becomes the plan too, so a UI can show it as one. Only the
	// result on screen is looked at: the plan pane shows one plan, and
	// it should be the one beside the grid.
	cc, _ := w.cfg.ConnByName(last.res.Conn)
	if p, ok := explain.Detect(last.res, cc.Driver); ok {
		ev.Plan, set.plan = p, p
	}
	return last.n
}

// plural is "1 result" or "3 results".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
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
// A run of several statements says which one's result is on screen (shown,
// its 1-based number) — the last statement's no longer, now that a write
// gets no tab of its own (results.go) — and, with several, how many tabs
// it filled.
//
//	statement 3/3 completed on Pilot in 1.234s — 42 rows
//	3 statements completed on Pilot in 2.5s — showing the last result: 1 affected
//	3 statements completed on Pilot in 2.5s — showing statement 1's result: 42 rows
//	3 statements completed on Pilot in 2.5s — 2 result tabs, showing statement 3's: 42 rows
func doneNote(ev *RunDone, shown int) Note {
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
	case len(ev.Stmts) > 1 && ev.Tabs > 1:
		return notef(Ok, "%s completed on %s in %s — %d result tabs, showing statement %d's: %s", what, ev.Conn, took, ev.Tabs, shown, summary)
	case len(ev.Stmts) > 1 && shown != len(ev.Stmts):
		return notef(Ok, "%s completed on %s in %s — showing statement %d's result: %s", what, ev.Conn, took, shown, summary)
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

// relistAfterRunLocked makes the Job that lists the sidebar's catalog
// again after a run on conn that may have changed it, or nil. It keeps
// ddlSinceCommit up to date as it goes. The caller holds mu.
//
// DDL inside a transaction is the case to mind. The relist reads through
// the pool, not the pinned session, so on an engine with transactional DDL
// (Postgres, SQLite) the table a session's BEGIN … CREATE TABLE made is not
// there to list until the session's COMMIT — a run with no DDL in it. So
// a run whose DDL may be left open (runEffects.open) marks conn, and the
// COMMIT run that follows relists again:
//
//	BEGIN                    ·        nothing to relist
//	CREATE TABLE t (…)       relist   (lists without t)   mark conn
//	INSERT INTO t …          ·        a recount only
//	COMMIT                   relist   (lists t)           clear the mark
//
// Whether a transaction is open is not tracked — that would take a lexer
// that understands every engine's implicit commits — so an autocommitted
// CREATE marks conn as well, and costs one needless relist at the next
// COMMIT run on conn. MySQL's DDL commits implicitly, so the first relist
// already lists it there. A ROLLBACK leaves the mark: one stray relist at
// a later COMMIT, for not tracking ROLLBACK TO SAVEPOINT, which leaves the
// transaction (and maybe its DDL) open.
func (w *Workspace) relistAfterRunLocked(conn string, eff runEffects) Job {
	pending := w.ddlSinceCommit == conn
	switch {
	case eff.open:
		w.ddlSinceCommit = conn
	case eff.committed && pending:
		w.ddlSinceCommit = ""
	}
	if !eff.catalog && !(eff.committed && pending) {
		return nil
	}
	return w.relistLocked(conn)
}

// relistLocked starts the sidebar's re-read of conn's catalog: a Refresh
// (same pick, same session, the Manager's row counts dropped) that lands
// in quieter words (kindRelist, see landConnect). It returns nil — no
// relist — when the sidebar is not showing conn's tables (the run's
// connection was left meanwhile, or its catalog never loaded: the driver
// has no catalog query, or the connect's read failed and the user has a
// Refresh for that), or while a connect is in flight, whose read is about
// to replace the list anyway and must not be canceled by this one. The
// caller holds mu.
//
// Only this workspace's sidebar is relisted. Another on the same Manager
// (dbc web's other tabs) keeps its list until its own Refresh — as it
// would for DDL any other client ran.
func (w *Workspace) relistLocked(conn string) Job {
	if conn != w.active || w.catalog == nil || w.connCancel != nil {
		return nil
	}
	w.mgr.ForgetRowCounts(conn)
	return w.connectLocked(conn, w.refreshPickLocked(), kindRelist).Job
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
// remembers a real failure in set (the run's connection's) for the
// assistant to explain. A stop is the
// user's choice, not a failure: it is not remembered as the last error.
func (w *Workspace) failedLocked(set *resultSet, err error, tag string, elapsed time.Duration, notes *[]Note, status *string) {
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
	set.lastErr = serr.StringFromErr(err)
	*notes = append(*notes, Note{Err, set.lastErr})
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
