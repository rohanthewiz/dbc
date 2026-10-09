// Package workspace is one person's working state against the databases,
// and the rules for changing it — with no UI in it.
//
// A Workspace holds the active connection and its catalog, the session
// pinned to it, the run in flight, and — per connection it has been on — a
// result set: result tabs, the last statement, error and plan (results.go). The TUI (package tui) and the browser UI (dbc web) are both clients:
// they turn keys and clicks into calls here, and draw what comes back. The
// rules therefore exist once — ONE RUN AT A TIME, A PINNED SESSION per active
// connection, CANCEL REACHES THE SERVER (see run.go) — rather than as two
// copies that drift.
//
// # Starting work, and where its outcome lands
//
// Methods that start work (Connect, Run, Explain, RunScript) never block.
// They do the bookkeeping that must happen at once — claim the run slot,
// record history, bump a generation — and return a Start holding a Job: the
// blocking half, for the caller to run wherever its UI runs blocking work.
//
//	UI goroutine                         worker goroutine
//	────────────                         ────────────────
//	st, err := w.Run(...)  ── claims the slot, returns st.Job
//	  err != nil → refused (busy, no connection, nothing to run)
//	  log st.Notes
//	  hand st.Job to a worker ─────────► ev := st.Job()
//	                                        runs the statements
//	                                        LANDS the outcome under w.mu:
//	                                          frees the slot, sets last*
//	                                     ◄── returns ev (*RunDone)
//	draw ev (grid, status, log)
//
// The TUI wraps a Job in a tea.Cmd, and the event comes back to Update as a
// message; the web layer runs it on a goroutine and writes the event to the
// workspace's SSE stream. The Job lands its own outcome before returning, so
// the workspace's state is right no matter when — or whether — a UI gets
// round to drawing the event.
//
// Why a Job and not a channel of events: Bubble Tea already is an event loop
// with its own way of running blocking work (tea.Cmd), and the TUI's test
// harness drives it synchronously — press a key, run the command, feed its
// message back — so a workspace that ran its own goroutines would make every
// TUI test a race against a channel. Handing the blocking half back to the
// caller keeps both UIs in charge of their own concurrency.
//
// Events that happen mid-run rather than at the end — a script's s.Show and
// s.Print — cannot be a Job's return value, and go to Options.Sink instead.
//
// # Locking
//
// mu guards the bookkeeping and is only ever held briefly. sessMu guards the
// pinned session and is held for as long as a statement runs on it — which
// may be minutes. The rule that keeps them from deadlocking: never take
// sessMu while holding mu. (Taking mu under sessMu is not needed today and
// is avoided too.)
package workspace

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/sdb"
	"github.com/rohanthewiz/dbc/userdata"
)

// Options configure New beyond the config.
type Options struct {
	// Sink receives the events that happen mid-run, from the run's own
	// goroutine: a script's ScriptShow and ScriptPrint. It must not block
	// for long, and must not call back into the Workspace's start methods
	// synchronously. nil drops them.
	Sink func(Event)

	// WholeCatalog is for a UI with no schema picker (the TUI): a connect
	// that names no schema lists every schema's tables, as long as the
	// database has at most db.AllSchemasLimit of them, instead of opening
	// on the search_path's first schema. Past the limit it opens on that
	// schema all the same, with a note: listing a huge catalog is the cost
	// the per-schema load exists to avoid.
	WholeCatalog bool

	// Jobs runs a script's s.RunJob: the host's jobs engine (dbc web's,
	// the TUI's).
	// Nil leaves it to sdb.DefaultJobRunner, an engine of the script's own
	// for the length of the call.
	Jobs sdb.JobRunner
}

// Workspace is one person's working state. Its methods are safe for
// concurrent use.
type Workspace struct {
	cfg  *config.Config
	mgr  *db.Manager
	hist *userdata.History
	sink func(Event)
	jobs sdb.JobRunner // s.RunJob in a script; see Options.Jobs

	mu sync.Mutex // guards everything below down to sessMu

	active   string         // the active connection
	catalog  *model.Result  // its tables, for a sidebar; nil until connected
	tableIdx *db.TableIndex // the same catalog, indexed for the assistant
	// The levels above the tables. databases is the server's, on a driver
	// with a database picker (db.HasDatabases: Postgres, MySQL); schemas
	// is the active database's, on a driver whose tables are loaded a
	// schema at a time (db.Navigable: Postgres); each is empty otherwise.
	// schema is the one whose tables catalog holds, "" when it holds every
	// schema's.
	databases    []db.DatabaseInfo
	schemas      []db.SchemaInfo
	schema       string
	wholeCatalog bool               // Options.WholeCatalog
	schemaCancel context.CancelFunc // cancels the schema load in flight; nil when none is
	schemaPick   SchemaPick         // what that load is for, while schemaCancel is set (refreshPickLocked)
	// rowCounts are the catalog's tables' row counts, once the Counts job
	// of the connect that loaded it lands; nil until then. countCancel
	// stops that job, which the next connect does: the counts would be for
	// a sidebar about to be replaced.
	rowCounts   map[db.TableRef]db.RowCount
	countCancel context.CancelFunc
	countGen    int // bumped by each Counts job made; see countsJobLocked
	// showCounts is the sidebar's "rows" switch (ShowRowCounts): off, as
	// it starts, no Counts job is ever made, so a connect costs no table
	// scans. It is the workspace's, not the Manager's, so each dbc web tab
	// (and each TUI tab) has its own.
	showCounts bool
	// routines are the stored functions and procedures of the schema the
	// catalog lists (all of them when it lists every schema's), for the
	// sidebar's routines list, once the Routines job of the landing that
	// loaded the catalog has run; nil until then, and while showRoutines
	// is off. routineCancel and routineGen are countCancel and countGen's
	// counterparts: see routinesJobLocked.
	routines      []model.Routine
	routineCancel context.CancelFunc
	routineGen    int
	// showRoutines is the sidebar's routines switch (ShowRoutines): the
	// list is read only while it is on, so a connect costs no pg_proc
	// read for a sidebar showing tables. Per workspace, as showCounts is.
	showRoutines bool

	// sets are the results pane's contents, one result set per connection
	// this workspace has been on — result tabs, the plan, and the last
	// run's statement and error. See results.go: everything "last" is the
	// active connection's.
	sets map[string]*resultSet
	// tabSeq numbers result tabs, resSeq the results put in them; both
	// only ever grow, so an id or seq is never reused within a workspace
	tabSeq, resSeq int
	// runTarget is the result tab the run in flight lands in, picked when
	// it started (targetLocked); nil for a new tab. showTab is the tab a
	// script run's s.Show results are going to, once its first has landed.
	runTarget, showTab *resultTab
	// outKey and outTab are ShowResult's: the key of the outside work
	// (a pipeline run's id) whose results are landing, and the tab they go
	// to once its first has landed — showTab's twin for work that is not
	// the run slot's
	outKey string
	outTab *resultTab
	// runAgain is the tab the run in flight is a rerun of
	// (RerunResultTab): it refills that tab even when it is kept — pinned
	// or shared — which no other run may. runTitle, when set, is the title
	// its result keeps, in place of one derived from the run's tag. Both
	// are cleared by beginRunLocked, so they never outlive their run.
	runAgain *resultTab
	runTitle string

	// planChat caches plan as the assistant is shown it — ChatContext is
	// built every frame by the TUI's context chip, and the plan does not
	// change between explains.
	planChat    string
	planChatFor *explain.Plan

	histWarned bool // a history write failure has been reported once

	// the editor's completion cache: the active connection's schema, and
	// the generation that drops it (see complete.go)
	compl    complState
	complGen int
	// complScoped holds the connections whose whole schema was too big to
	// read (db.ErrCatalogTooBig), for completion or a diagram; both read
	// them scoped from then on, without paying for the failing whole read
	// again
	complScoped map[string]bool

	// the run slot — one run at a time
	busy   bool
	runGen int // bumped per run; a late outcome from an older run is dropped
	runTag string
	runAt  time.Time
	cancel context.CancelFunc
	// progress through a multi-statement run: runStep is the statement
	// executing now (1-based), runSteps how many the run holds. Both 0 for
	// anything that is not a list of statements (a script, an explain).
	runStep, runSteps int

	// connect state. A newer connect supersedes an older one still dialing.
	connGen    int                // bumped per connect; an older one's outcome is dropped
	connCancel context.CancelFunc // cancels the connect in flight; nil when none is
	connName   string             // what it is connecting to, for the log
	// ddlSinceCommit is the connection whose pinned session ran a
	// catalog-changing statement since its last COMMIT, "" for none: the
	// COMMIT that makes it visible to the pool relists the sidebar again
	// (relistAfterRunLocked).
	ddlSinceCommit string

	// sessMu is held while a statement runs on sess — see Locking above.
	sessMu  sync.Mutex
	sess    *db.Session
	sessFor string // the connection sess is pinned to
}

// New builds a workspace over cfg's connections. It does no IO: the first
// connect is the caller's Connect(w.Active()).
//
// The active connection starts as the config's default_connection, or the
// first connection when that names none — so a UI can draw it before the
// connect completes.
func New(cfg *config.Config, mgr *db.Manager, hist *userdata.History, opt Options) *Workspace {
	if hist == nil {
		hist = userdata.LoadHistory("")
	}
	w := &Workspace{cfg: cfg, mgr: mgr, hist: hist, sink: opt.Sink, wholeCatalog: opt.WholeCatalog, jobs: opt.Jobs}
	if _, ok := cfg.ConnByName(cfg.DefaultConnection); ok {
		w.active = cfg.DefaultConnection
	} else if conns := cfg.Conns(); len(conns) > 0 {
		w.active = conns[0].Name
	}
	return w
}

// emit hands a mid-run event to the sink.
func (w *Workspace) emit(e Event) {
	if w.sink != nil {
		w.sink(e)
	}
}

// ---------------------------------------------------------------------------
// Reading the state
// ---------------------------------------------------------------------------

// Config is the configuration the workspace was built with.
func (w *Workspace) Config() *config.Config { return w.cfg }

// Manager is the connection manager the workspace runs on.
func (w *Workspace) Manager() *db.Manager { return w.mgr }

// History is the query history runs are recorded in.
func (w *Workspace) History() *userdata.History { return w.hist }

// Active is the active connection's name.
func (w *Workspace) Active() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active
}

// Catalog is the active connection's tables, as its catalog query returned
// them; nil before the first connect completes, or when the query failed.
func (w *Workspace) Catalog() *model.Result {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.catalog
}

// Databases is the active connection's server's databases, for a database
// picker; nil on a driver without databases (db.HasDatabases), or when the listing
// failed. The slice is shared: read it, never write to it.
func (w *Workspace) Databases() []db.DatabaseInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.databases
}

// Schemas is every schema of the active database, empty ones included,
// with its table count; nil on a driver that is not db.Navigable. The
// slice is shared: read it, never write to it.
func (w *Workspace) Schemas() []db.SchemaInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.schemas
}

// CatalogSchema is the schema whose tables Catalog holds, or "" when it
// holds every schema's (always, on a driver that is not db.Navigable).
func (w *Workspace) CatalogSchema() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.schema
}

// RowCounts is the row count of each of the catalog's tables that has one
// (views never do); nil until the connect's counting lands. The map is
// shared: read it, never write to it.
func (w *Workspace) RowCounts() map[db.TableRef]db.RowCount {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rowCounts
}

// RowCountsShown reports whether the sidebar's row counts are on
// (ShowRowCounts); they start off.
func (w *Workspace) RowCountsShown() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.showCounts
}

// ShowRowCounts turns the sidebar's row counts on or off. They start off:
// every count is an exact count(*), which reads the whole table (see
// db/rowcount.go), so it is the user's to ask for.
//
// Turning them on returns the Job that counts the listed catalog, whose
// event is a *RowCounts as a connect's is; nil when nothing is listed yet
// (the next connect or schema pick counts, now that the switch is on).
// From then on every connect, schema pick and write recounts, as before.
//
// Turning them off drops the counts and cancels a counting in flight; that
// one lands Stale (countGen moves on). It returns nil: there is nothing to
// wait for, and the caller redraws its list without numbers at once.
//
//	off ──ShowRowCounts(true)──► on ─► Counts job ─► *RowCounts
//	on  ──ShowRowCounts(false)─► off: rowCounts nil, in-flight → Stale
func (w *Workspace) ShowRowCounts(on bool) Job {
	w.mu.Lock()
	defer w.mu.Unlock()
	if on == w.showCounts {
		return nil // a repeat of the same tick: the counting already ran or runs
	}
	w.showCounts = on
	if !on {
		if w.countCancel != nil {
			w.countCancel()
			w.countCancel = nil
		}
		w.countGen++ // a counting still on its way lands Stale
		w.rowCounts = nil
		return nil
	}
	if w.active == "" || w.catalog == nil {
		return nil
	}
	return w.countsJobLocked(w.active, w.connGen, db.TableRefs(w.catalog.Rows))
}

// TableIndex is the catalog indexed for name lookups; nil without one.
func (w *Workspace) TableIndex() *db.TableIndex {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tableIdx
}

// LastStmt is the statement the last run on the active connection executed
// ("" before any, and after a script run: what a script ran is not known).
func (w *Workspace) LastStmt() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.activeSetLocked(); s != nil {
		return s.lastStmt
	}
	return ""
}

// LastScript is the file of the script the last run on the active
// connection was, as RunScript was given it — "" before any run, and once
// a statement (or explain) has run there since. A UI whose scripts are not
// in an editor of its own (the TUI's go to $EDITOR) reads the source from
// it to ask the assistant about that script (ScriptChatContext).
func (w *Workspace) LastScript() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.activeSetLocked(); s != nil {
		return s.lastScriptPath
	}
	return ""
}

// LastErr is what the last run on the active connection failed with, ""
// if it worked. A stopped run leaves it alone: stopping is the user's
// choice, not a failure to explain.
func (w *Workspace) LastErr() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.activeSetLocked(); s != nil {
		return s.lastErr
	}
	return ""
}

// LastResult is the result on screen: the active connection's current
// result tab's — a run's, or a script's s.Show. nil when that set has none.
func (w *Workspace) LastResult() *model.Result {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t := w.curLocked(); t != nil {
		return t.res
	}
	return nil
}

// LastResultSeq is LastResult with its seq (see results.go), which moves
// whenever the result on screen does — a run, a show, another result tab
// or connection — and only then. 0 with no result.
func (w *Workspace) LastResultSeq() (*model.Result, int) {
	r, seq, _ := w.LastResultRerun()
	return r, seq
}

// LastResultRerun is LastResultSeq plus rerunOf: the seq of the result the
// one on screen replaced when it is a rerun of its tab's statement, else 0
// (ResultTab.RerunOf). All three are read under one lock, so they always
// describe the same result — a run landing between two calls cannot pair
// one result with another's rerun.
func (w *Workspace) LastResultRerun() (r *model.Result, seq, rerunOf int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t := w.curLocked(); t != nil {
		return t.res, t.seq, t.rerunOf
	}
	return nil, 0, 0
}

// MaxScriptResults caps how many of one script run's s.Show results are
// kept for a UI to switch between. A script that shows a result per loop
// pass could otherwise hold every one of them — each possibly max_rows
// rows — for as long as the tab lives. The newest are kept: the last
// result shown is usually the one the script was building toward.
const MaxScriptResults = 20

// ScriptResults describes the s.Show results held by the result tab on
// screen: n of them kept (0 for a tab holding a run's own result), the one
// the grid is on (at, 0-based), and cut, how many earlier shows were
// dropped to keep to MaxScriptResults — so a UI can number them as the
// script showed them (cut+1 … cut+n).
func (w *Workspace) ScriptResults() (n, at, cut int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.curLocked()
	if t == nil {
		return 0, -1, 0
	}
	at = slices.IndexFunc(t.shows, func(s shown) bool { return s.seq == t.seq })
	return len(t.shows), at, t.cut
}

// ShowScriptResult puts the current result tab's script result i (0-based,
// among the ones kept) back on the grid: it becomes the last result, which
// is what the grid, copies, exports and the assistant all read. Refused
// while a run is in flight — a script still showing results would move the
// grid under the pick — and for an i out of range.
func (w *Workspace) ShowScriptResult(i int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.busy {
		return refuse(Busy, Warn, "busy — %s is still running; pick a result once it is done", w.runTag)
	}
	t := w.curLocked()
	if t == nil || i < 0 || i >= len(t.shows) {
		n := 0
		if t != nil {
			n = len(t.shows)
		}
		return refuse(Invalid, Warn, "no result %d — the script in this result tab showed %d", i+1, n)
	}
	t.res, t.seq, t.rerunOf = t.shows[i].res, t.shows[i].seq, 0
	return nil
}

// Plan is the active connection's last plan: an explain's, or one
// recognized in a result.
func (w *Workspace) Plan() *explain.Plan {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.activeSetLocked(); s != nil {
		return s.plan
	}
	return nil
}

// Busy reports whether a run (statements, an explain or a script) is in
// flight.
func (w *Workspace) Busy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.busy
}

// RunTag names the run in flight ("statement 2/4", "explain", "script x.go");
// "" when idle.
func (w *Workspace) RunTag() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.busy {
		return ""
	}
	return w.runTag
}

// Ticking is the status line for run gen while it is still in flight — its
// tag and elapsed time — and false once it has ended, which is a ticker's
// cue to stop re-arming.
func (w *Workspace) Ticking(gen int) (status string, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.busy || gen != w.runGen {
		return "", false
	}
	return w.runningStatusLocked(), true
}

// RunningStatus is the status line for the run in flight, "" when idle.
func (w *Workspace) RunningStatus() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.busy {
		return ""
	}
	return w.runningStatusLocked()
}

// RunProgress is how far a multi-statement run has got: the statement
// executing now (1-based) and how many there are. steps is 0 when idle, and
// for work that is not a list of statements.
func (w *Workspace) RunProgress() (step, steps int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.busy {
		return 0, 0
	}
	return w.runStep, w.runSteps
}

// Connecting reports whether a connect is in flight, and to what.
func (w *Workspace) Connecting() (name string, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.connName, w.connCancel != nil
}

// Session describes the pinned session: the connection it is pinned to
// ("" when none is) and whether it holds state a close would lose — an open
// transaction, a SET, a temp table. A UI shows the latter as a "transaction
// open" badge.
//
// It takes sessMu, so it waits for a statement in flight to finish; call it
// from a UI only where that is acceptable (not every frame).
func (w *Workspace) Session() (conn string, stateful bool) {
	w.sessMu.Lock()
	defer w.sessMu.Unlock()
	if w.sess == nil {
		return "", false
	}
	return w.sessFor, w.sess.Stateful()
}

// ---------------------------------------------------------------------------
// Shutting down
// ---------------------------------------------------------------------------

// Stop cancels the run, the connect and the row counting in flight, if any,
// without a word — the first step of quitting. The canceled work still lands
// its outcome.
func (w *Workspace) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		w.cancel()
	}
	if w.connCancel != nil {
		w.connCancel()
	}
	if w.countCancel != nil {
		w.countCancel()
	}
	if w.schemaCancel != nil {
		w.schemaCancel()
	}
}

// Close stops what is in flight and releases the pinned session, rolling
// back whatever it left open. It waits for a statement still running to
// return from its cancel.
func (w *Workspace) Close() {
	w.Stop()
	w.dropSession()
}
