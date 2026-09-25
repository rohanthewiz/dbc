package workspace

import (
	"fmt"
	"strings"
	"time"

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
type Connected struct {
	Name    string
	Catalog *model.Result // the connection's tables; nil if the catalog query failed
	Err     error         // the connect's failure (db.ErrCanceled when stopped); nil on success
	Changed bool          // the active connection moved to Name
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
}

// RunDone lands a run of statements, or of a script.
type RunDone struct {
	Tag     string
	Conn    string
	Stmts   []string      // what ran; nil for a script
	Result  *model.Result // the last statement's result; nil on failure and for a script
	Plan    *explain.Plan // Result is itself a query plan (an EXPLAIN the user typed)
	Err     error         // the failure, or db.ErrCanceled when stopped
	Script  bool
	Elapsed time.Duration
	Stale   bool // a straggler from a run already written off: draw nothing
	Notes   []Note
	// Status is for the status bar when the run failed or stopped. On
	// success it is "": the summary depends on how many rows the UI shows
	// (see ResultStatus).
	Status string
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

// ScriptShow is a script's s.Show, mid-run: a result to put in the grid.
type ScriptShow struct{ Result *model.Result }

// ScriptPrint is a script's s.Print, mid-run: a line for the log.
type ScriptPrint struct{ Text string }

// SessionReleased reports that switching connections closed the session
// pinned to the previous one.
type SessionReleased struct {
	Conn     string
	Stateful bool   // it held state, now gone
	Notes    []Note // a warning when it did
}

func (*Connected) event()       {}
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
