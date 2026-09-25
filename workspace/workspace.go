// Package workspace is one person's working state against the databases,
// and the rules for changing it — with no UI in it.
//
// A Workspace holds the active connection and its catalog, the session
// pinned to it, the run in flight, and the last statement, error, result and
// plan. The TUI (package tui) and the browser UI (dbc web) are both clients:
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
	"sync"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/userdata"
)

// Options configure New beyond the config.
type Options struct {
	// Sink receives the events that happen mid-run, from the run's own
	// goroutine: a script's ScriptShow and ScriptPrint. It must not block
	// for long, and must not call back into the Workspace's start methods
	// synchronously. nil drops them.
	Sink func(Event)
}

// Workspace is one person's working state. Its methods are safe for
// concurrent use.
type Workspace struct {
	cfg  *config.Config
	mgr  *db.Manager
	hist *userdata.History
	sink func(Event)

	mu sync.Mutex // guards everything below down to sessMu

	active   string         // the active connection
	catalog  *model.Result  // its tables, for a sidebar; nil until connected
	tableIdx *db.TableIndex // the same catalog, indexed for the assistant

	lastStmt string        // the statement the last run executed
	lastErr  string        // what it failed with, "" if it worked
	lastRes  *model.Result // the last result published (a run's or a script's s.Show)
	plan     *explain.Plan // the last plan: an explain's, or one detected in a result

	// planChat caches plan as the assistant is shown it — ChatContext is
	// built every frame by the TUI's context chip, and the plan does not
	// change between explains.
	planChat    string
	planChatFor *explain.Plan

	histWarned bool // a history write failure has been reported once

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
	w := &Workspace{cfg: cfg, mgr: mgr, hist: hist, sink: opt.Sink}
	if _, ok := cfg.ConnByName(cfg.DefaultConnection); ok {
		w.active = cfg.DefaultConnection
	} else if len(cfg.Connections) > 0 {
		w.active = cfg.Connections[0].Name
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

// TableIndex is the catalog indexed for name lookups; nil without one.
func (w *Workspace) TableIndex() *db.TableIndex {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tableIdx
}

// LastStmt is the statement the last run executed ("" before any).
func (w *Workspace) LastStmt() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastStmt
}

// LastErr is what the last run failed with, "" if it worked. A stopped run
// leaves it alone: stopping is the user's choice, not a failure to explain.
func (w *Workspace) LastErr() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastErr
}

// LastResult is the last result published — by a run, or by a script's
// s.Show. nil before any.
func (w *Workspace) LastResult() *model.Result {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastRes
}

// Plan is the last plan: an explain's, or one recognized in a result.
func (w *Workspace) Plan() *explain.Plan {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.plan
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

// Stop cancels the run and the connect in flight, if any, without a word —
// the first step of quitting. The canceled work still lands its outcome.
func (w *Workspace) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		w.cancel()
	}
	if w.connCancel != nil {
		w.connCancel()
	}
}

// Close stops what is in flight and releases the pinned session, rolling
// back whatever it left open. It waits for a statement still running to
// return from its cancel.
func (w *Workspace) Close() {
	w.Stop()
	w.dropSession()
}
