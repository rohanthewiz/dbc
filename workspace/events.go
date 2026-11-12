package workspace

import (
	"fmt"
	"strings"
	"time"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/model"
)

// ---------------------------------------------------------------------------
// Notes and refusals: the workspace's words
// ---------------------------------------------------------------------------

// Level is how a Note should be shown — the TUI's log colors, the web log's
// classes.
type Level int

const (
	Info   Level = iota // neutral progress: "running query on pg — SELECT …"
	Ok                  // something finished well
	Warn                // worth a look, not a failure: stopped, busy, lost state
	Err                 // a failure
	Accent              // a pointer to something new on screen
	Muted               // background detail
)

// Note is one line for the user's log. The workspace writes the words, so a
// busy refusal or a stopped run reads the same in the terminal and the
// browser.
type Note struct {
	Level Level
	Text  string
}

func notef(l Level, format string, args ...any) Note {
	return Note{Level: l, Text: fmt.Sprintf(format, args...)}
}

// Reason is why a request was refused — what the web layer maps to a status
// code (Busy → 409, the rest → 400).
type Reason int

const (
	Busy         Reason = iota + 1 // a run is already in flight
	NoConnection                   // no active connection
	Nothing                        // nothing to run or explain
	Invalid                        // the request cannot be carried out as asked
)

// Refusal is a request the workspace declined, with the reason in words for
// the user. It is an error, but not a failure: nothing was attempted, and
// nothing about the workspace changed.
type Refusal struct {
	Reason Reason
	Note   Note
}

func (r *Refusal) Error() string { return r.Note.Text }

func refuse(reason Reason, l Level, format string, args ...any) *Refusal {
	return &Refusal{Reason: reason, Note: notef(l, format, args...)}
}

// ---------------------------------------------------------------------------
// Starting work
// ---------------------------------------------------------------------------

// Job is the blocking half of a request: it does the work, lands the
// outcome in the workspace, and returns an Event describing what landed
// (nil when there is nothing to tell). Run it off the UI goroutine.
type Job func() Event

// Start is what a start method returns.
type Start struct {
	Tag   string // names the run: "query", "statement 2/4", "explain", …
	Gen   int    // the run generation, for a ticker (see Ticking)
	Notes []Note // what to log now — present even when the request was refused
	Job   Job    // the blocking half; nil when refused or when there is nothing to do
}

// ---------------------------------------------------------------------------
// Events: what a Job (or the sink) reports
// ---------------------------------------------------------------------------

// Event is something that happened in the workspace. The concrete types are
// the pointers below.
type Event interface{ event() }

// Connected lands a connect.
//
// A connect proper lands as soon as the connection is open, WITHOUT its
// catalog: Catalog, Databases, Schemas and Schema are then empty, and Load
// is the Job that reads them (its event a *CatalogLoaded). A re-read of the
// connection the workspace is already on — a Refresh, the relist after a
// run's DDL — dials nothing worth waiting for, so it still reads the
// catalog in its one Job and lands it here, with Load nil.
//
//	connect ─► dial ─► *Connected (Load set) ─► Load ─► *CatalogLoaded
//	refresh ─► dial (cached pool) + read ─────────────► *Connected (Load nil)
type Connected struct {
	Name    string
	Catalog *model.Result // a re-read's tables; nil on a connect (see Load), or if the catalog query failed
	// Databases and Schemas are the levels above the tables: the server's
	// databases, on a driver with a database picker (db.HasDatabases:
	// Postgres, MySQL), and this database's schemas with their table
	// counts, on one whose sidebar is loaded a schema at a time
	// (db.Navigable: Postgres). Schema is the one whose tables Catalog
	// holds, "" for every schema's. Each is empty on the other drivers,
	// and Databases or Schemas is nil when its listing failed.
	Databases []db.DatabaseInfo
	Schemas   []db.SchemaInfo
	Schema    string
	Err       error // the connect's failure (db.ErrCanceled when stopped); nil on success
	Changed   bool  // the active connection moved to Name
	// Relisted reports the re-read a run's DDL started (RunDone.Relist),
	// not one the user asked for: the status bar belongs to the run that
	// started it, so a UI leaves it as it is rather than saying "ready".
	Relisted bool
	// Left is the connection Changed moved away from, "" when there was
	// none (the first connect, or one after a Disconnect). A UI that knows
	// no other workspace is on it may close its pool once Release has run
	// — see Workspace.Derived for the one kind worth closing.
	Left string
	// Stale reports a connect superseded by a newer one: nothing landed,
	// and a UI draws nothing.
	Stale  bool
	Notes  []Note
	Status string // for a status bar; "" leaves it
	// Release, when non-nil, closes the session pinned to the connection
	// just left — rolling back what it left open. It is a separate Job
	// because it waits for any statement still running on that session,
	// and the switch itself must not.
	Release Job
	// Load, when non-nil, reads the catalog of the connection just opened
	// — the server's databases, this database's schemas, the tables of the
	// schema the connect's pick names — for the sidebar; its event is a
	// *CatalogLoaded. It is a Job of its own so that "connected" means the
	// connection is open, and is said when it is: on a big production
	// database the three reads take seconds (the schema summary alone can
	// spend its whole budget), and a user who came to run a statement
	// should not wait for a table list to do it. A UI must run it whenever
	// it is set, as it does Counts: until it lands the workspace reports
	// CatalogLoading, and its sidebar has nothing to list. nil on a re-read
	// (above), on a failed connect, and on a driver with no catalog query.
	Load Job
	// Counts, when non-nil, fetches the row counts of the catalog's tables
	// for the sidebar; its event is a *RowCounts. It is separate so the
	// tables list draws at once and the numbers fill in after — a count
	// reads a table's rows, and the list must never wait for that. nil
	// while the sidebar's counts are off (Workspace.ShowRowCounts).
	Counts Job
	// Routines, when non-nil, reads the stored functions and procedures
	// of the schema the catalog lists, for the sidebar's routines list;
	// its event is a *RoutinesLoaded. Separate, as Counts is, so the
	// tables draw at once. nil while the routines list is off
	// (Workspace.ShowRoutines) and on a driver with none (db.HasRoutines).
	Routines Job
}

// RunDone lands a run of statements, or of a script.
type RunDone struct {
	Tag   string
	Conn  string
	Stmts []string // what ran; nil for a script
	// Result is the result the run put on screen: the last of its
	// statements' that returned rows (each got a result tab, see Tabs), or
	// the last statement's when none did. A failed run still lands the
	// rows of the statements before the failure, so Result can be set
	// beside Err. nil when nothing landed, and for a script.
	Result *model.Result
	// Tabs is how many result tabs the run filled, one per statement that
	// returned rows (results.go); 0 for a script and when nothing landed.
	Tabs   int
	Plan   *explain.Plan // Result is itself a query plan (an EXPLAIN the user typed)
	Err    error         // the failure, or db.ErrCanceled when stopped
	Script bool
	// ScriptPath is the script's file, as RunScript was given it, for a
	// script run; "" otherwise. Tag names only its base name, and a UI
	// that asks the assistant about the script needs its source.
	ScriptPath string
	Elapsed    time.Duration
	Stale      bool // a straggler from a run already written off: draw nothing
	Notes      []Note
	// Status is for the status bar when the run failed or stopped. On
	// success it is "": the summary depends on how many rows the UI shows
	// (see ResultStatus).
	Status string
	// Wrote reports that the run may have changed rows (db.ChangesRows of
	// a statement it reached, or a script), failed or not. A UI with other
	// workspaces on the same Manager recounts their sidebars on it
	// (Workspace.Recount); Counts alone would not tell it, being nil
	// whenever this workspace's own counts are off.
	Wrote bool
	// Counts, when non-nil, refreshes the sidebar's row counts: the run
	// Wrote, and the sidebar shows counts (Workspace.ShowRowCounts). Its
	// event is a *RowCounts, as Connected.Counts's is.
	Counts Job
	// Relist, when non-nil, reads the sidebar's catalog again — databases,
	// schemas, tables — after a run that may have changed it: a statement
	// that changes the catalog (sqlsplit.ChangesCatalog), a script that ran
	// one on its connection (sdb.S.CatalogChanged), or the COMMIT that ends
	// a transaction one ran in. It is a Refresh in all but its words
	// (relistLocked), its event a *Connected with Changed false, and it
	// counts the rows of the list it lands — so Counts is nil whenever it
	// is set. A UI must run it whenever it is set, as it does Counts: until
	// it lands, the workspace is mid-connect (Connecting), which refuses a
	// Refresh and a schema pick.
	Relist Job
}

// ExplainDone lands an explain.
type ExplainDone struct {
	Tag     string
	Conn    string
	Stmt    string
	Plan    *explain.Plan
	Err     error
	Elapsed time.Duration
	Stale   bool
	Notes   []Note
	Status  string // as RunDone's: set on failure only
}

// ScriptShow is a script's s.Show, mid-run: a result to put in the grid —
// when Conn, the connection the script run started on (whose result set it
// landed in), is still the active one.
type ScriptShow struct {
	Result *model.Result
	Conn   string
}

// ScriptPrint is a script's s.Print, mid-run: a line for the log of Conn,
// the connection the script run started on.
type ScriptPrint struct {
	Text string
	Conn string
}

// SessionReleased reports that switching connections closed the session
// pinned to the previous one.
type SessionReleased struct {
	Conn     string
	Stateful bool   // it held state, now gone
	Notes    []Note // a warning when it did
}

// RowCounts lands the row counts of the active connection's tables. The
// counts are also kept in the workspace (see Workspace.RowCounts), so a UI
// that redraws its tables list later finds them there.
type RowCounts struct {
	Conn   string
	Counts map[db.TableRef]db.RowCount // read-only; see db.Manager.RowCounts
	// Stale reports counts for a connection that is no longer the active
	// one, or a counting a later connect canceled: nothing landed, and a UI
	// draws nothing.
	Stale bool
	Notes []Note
}

// CatalogLoaded lands the catalog read a connect left running
// (Connected.Load): the levels of the sidebar, for the connection the
// workspace is already on. The workspace keeps them too (Databases,
// Schemas, CatalogSchema, Catalog), so a UI may redraw from either.
//
// Whatever was read is installed, even when the tables were not: a
// database whose table list failed or was canceled still has its schema
// picker, and a pick from it (PickSchema) or a Refresh is how the user
// tries again. Notes say what could not be read.
type CatalogLoaded struct {
	Conn      string
	Catalog   *model.Result // the tables; nil when their read failed or was canceled
	Databases []db.DatabaseInfo
	Schemas   []db.SchemaInfo
	Schema    string // the schema Catalog is of; "" for every schema's
	// Canceled reports a read the user stopped (Workspace.Cancel), rather
	// than one that failed: a UI says so in its status bar, and offers no
	// explanation of an error.
	Canceled bool
	// Stale reports a read superseded by a later connect, refresh or
	// disconnect: nothing landed, and a UI draws nothing.
	Stale  bool
	Notes  []Note
	Status string // for a status bar; "" leaves it
	// Counts is the row counting of the tables, as on Connected.
	Counts Job
	// Routines reads the listed schema's routines, as on Connected.
	Routines Job
}

// SchemaLoaded lands a schema pick (Workspace.PickSchema): the tables of
// another schema of the active database, in place of the ones the sidebar
// listed.
type SchemaLoaded struct {
	Conn    string
	Schema  string        // the schema loaded; "" for every schema
	Catalog *model.Result // its tables; nil when the load failed, and the old list stays
	// Stale reports a load superseded by a later pick or connect: nothing
	// landed, and a UI draws nothing.
	Stale bool
	Notes []Note
	// Counts is the row counting of the new tables, as on Connected.
	Counts Job
	// Routines reads the new schema's routines, as on Connected.
	Routines Job
}

// RoutinesLoaded lands the stored functions and procedures of the schema
// the sidebar lists (Workspace.Routines keeps them too, so a UI redrawing
// its list later finds them there).
type RoutinesLoaded struct {
	Conn     string
	Schema   string          // the schema they are of; "" for every schema
	Routines []model.Routine // read-only; nil when the read failed (Notes say why)
	// Stale reports a read a later connect, schema pick or switch-off has
	// superseded: nothing landed, and a UI draws nothing.
	Stale bool
	Notes []Note
}

// RoutineDDL lands a routine's definition (Workspace.RoutineDDL), as its
// server renders it, for a UI to show.
type RoutineDDL struct {
	Conn    string
	Routine model.Routine
	DDL     string // the CREATE statement, ending in ";"; "" when Err is set
	Err     error
	Notes   []Note
}

// ViewDDL lands a view's definition (Workspace.ViewDDL), as
// db.Manager.ViewDDL renders it, for a UI to show beside a routine's.
type ViewDDL struct {
	Conn  string
	View  db.TableRef
	DDL   string // the CREATE statement, ending in ";"; "" when Err is set
	Err   error
	Notes []Note
}

func (*ViewDDL) event()         {}
func (*SchemaLoaded) event()    {}
func (*CatalogLoaded) event()   {}
func (*RoutinesLoaded) event()  {}
func (*RoutineDDL) event()      {}
func (*Connected) event()       {}
func (*RowCounts) event()       {}
func (*RunDone) event()         {}
func (*ExplainDone) event()     {}
func (*ScriptShow) event()      {}
func (*ScriptPrint) event()     {}
func (*SessionReleased) event() {}

// ---------------------------------------------------------------------------
// Shared wording
// ---------------------------------------------------------------------------

// ResultStatus is the status-bar summary of a result. shown is how many
// rows the UI displays (max_display_rows can cap it below len(r.Rows)).
func ResultStatus(r *model.Result, maxRows, shown int) string {
	verb := fmt.Sprintf("%d rows", len(r.Rows))
	if r.IsExec {
		verb = fmt.Sprintf("%d affected", r.Affected)
	}
	s := fmt.Sprintf("%s in %s", verb, r.Duration.Round(10*time.Microsecond))
	if r.Truncated {
		s += fmt.Sprintf(" (truncated at %d)", maxRows)
	}
	if shown < len(r.Rows) {
		s += fmt.Sprintf(" (showing %d)", shown)
	}
	return s
}

// Preview is a one-line, length-capped echo of a statement for the log.
func Preview(stmt string) string {
	s := strings.Join(strings.Fields(stmt), " ")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:59]) + "…"
	}
	return s
}
