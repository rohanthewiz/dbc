// Package sdb is the API surface handed to Go scripts. A script is a plain
// Go file (package main) defining:
//
//	func Run(s *sdb.S) error
//
// It can query any configured connection, loop with parameters, push results
// into the results view, and export in any supported format.
package sdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/serr"
)

// Result is the tabular result type scripts receive from Query.
type Result = model.Result

// Plan is an explained statement: its steps (Root, Nodes), what it cost or
// took, and dbc's findings about it (Insights). See package explain.
type Plan = explain.Plan

// PlanText shapes Plan.Text: width, the metric bars are sized by, color,
// and whether the findings are appended.
type PlanText = explain.TextOptions

// S is the session handle passed to a script's Run function.
type S struct {
	mgr   *db.Manager
	show  func(*model.Result)
	print func(string)
	ctx   context.Context
	// open tracks the run's Readers and Writers for Release (etl.go).
	open openSet
	// ddl is the DDL log switch (LogDDL). It is set by the host before Run
	// is called and never cleared, so it needs no lock: a script's own
	// goroutines all start after the write.
	ddl bool
	// catalog is the connections the session ran a catalog-changing
	// statement on (sqlsplit.ChangesCatalog), for CatalogChanged. It is
	// kept whether or not the DDL log is on, and under its own lock: a
	// script may run statements from goroutines of its own.
	catMu   sync.Mutex
	catalog map[string]bool
	// paths is where script and pipeline names resolve (WithPaths).
	paths Paths
}

// New builds a script session. show receives results pushed via Show;
// print receives Print output (results table / log pane in the TUI,
// stdout in headless mode).
func New(mgr *db.Manager, show func(*model.Result), print func(string)) *S {
	return &S{mgr: mgr, show: show, print: print, ctx: context.Background()}
}

// WithContext attaches a cancellation context to the session and returns it.
// The host — not the script — sets this: when the user stops a run, the query
// in flight is aborted and every later Query or Exec fails immediately, so a
// looping script unwinds through its own error handling.
func (s *S) WithContext(ctx context.Context) *S {
	if ctx != nil {
		s.ctx = ctx
	}
	return s
}

// LogDDL turns on the DDL log for the rest of the session: every data
// definition statement it runs (sqlsplit.IsDDL: CREATE, ALTER, DROP,
// TRUNCATE, RENAME, COMMENT, GRANT, REVOKE) is written to the script output,
// as Print does, the moment before it runs:
//
//	DDL warehouse: CREATE INDEX orders_user_idx ON orders (user_id)
//
// That covers a script's Query and Exec and what Copy and Writer run to
// ready a destination (a Create's CREATE TABLE, Truncate, a Writer's
// Setup). Statements a script runs through DB's raw handle are out of its
// sight.
//
// Logging before the statement, as Postgres's log_statement = 'ddl' does,
// rather than after it, puts a long CREATE INDEX or an ALTER stuck behind a
// lock in the log while it is still running. A Query or Exec that then
// fails adds a "DDL <conn> failed" line, since a script may carry on past
// the error; a failed Copy or Writer stops with an error naming the
// statement anyway.
//
// script.Run turns this on for every script, whichever UI runs it. The
// session's other host, dbc copy, leaves it off. There is deliberately no
// way to turn it off again, so a script cannot opt out of its own log.
func (s *S) LogDDL() *S {
	s.ddl = true
	return s
}

// logDDL writes the DDL log line for each DDL statement in stmt when the
// log is on (LogDDL), and reports whether it wrote any. stmt is split
// first: one Exec can carry several statements (a script's migration
// pasted in whole), and a CREATE after an INSERT is still a CREATE. Each
// line is the statement as written, only trimmed — a multi-line CREATE
// TABLE keeps its shape, and both UIs' logs draw that as one entry.
//
// It is also where the session notes a catalog change on conn
// (CatalogChanged), log or no log: both doors a statement goes through —
// run, and the etl Trace of a Copy's or Writer's setup — come here. It is
// noted before the statement runs, so a failed one counts too: costing a
// sidebar one needless relist, where the other way round (a multi-statement
// Exec that failed after its CREATE) would leave it stale.
func (s *S) logDDL(conn, stmt string) bool {
	logged := false
	for _, st := range sqlsplit.Split(stmt) {
		if sqlsplit.ChangesCatalog(st.Text) {
			s.noteCatalog(conn)
		}
		if s.ddl && sqlsplit.IsDDL(st.Text) {
			s.Print("DDL %s: %s", conn, st.Text)
			logged = true
		}
	}
	return logged
}

// noteCatalog records that the session ran a catalog-changing statement on
// conn.
func (s *S) noteCatalog(conn string) {
	s.catMu.Lock()
	defer s.catMu.Unlock()
	if s.catalog == nil {
		s.catalog = map[string]bool{}
	}
	s.catalog[conn] = true
}

// CatalogChanged reports whether the session has run a statement that may
// change conn's catalog (sqlsplit.ChangesCatalog: CREATE, ALTER, DROP,
// RENAME, COMMENT, ATTACH, DETACH) — through Query, Exec, or what Copy and
// Writer run to ready a destination. The host asks it once the script has
// returned, to decide whether the sidebar should list the tables again.
// Statements run through DB's raw handle are out of its sight, as they are
// out of the DDL log's.
func (s *S) CatalogChanged(conn string) bool {
	s.catMu.Lock()
	defer s.catMu.Unlock()
	return s.catalog[conn]
}

// Ctx returns the session's context. Long-running scripts can select on
// s.Ctx().Done() (or check s.Canceled()) to bail out early.
func (s *S) Ctx() context.Context {
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// Canceled reports whether the user has stopped this run.
func (s *S) Canceled() bool {
	return s.Ctx().Err() != nil
}

// IsCanceled reports whether an error from Query or Exec is the user stopping
// the run, as opposed to a genuine failure — so a script can unwind quietly:
//
//	if err != nil {
//		if sdb.IsCanceled(err) {
//			return nil
//		}
//		return err
//	}
//
// Errors from Copy, Reader and Writer carry context.Canceled instead: the
// etl package does not depend on db, so both are recognised here.
func IsCanceled(err error) bool {
	return errors.Is(err, db.ErrCanceled) || errors.Is(err, context.Canceled)
}

// Conns returns the configured connection names.
func (s *S) Conns() []string {
	return s.mgr.Names()
}

// Query runs a statement with parameters on the named connection and
// returns the result set. Use the placeholder style of the target driver
// ($1 for postgres/bytdb, ? for mysql/sqlite).
//
// Server notices the statement raises (Postgres RAISE NOTICE and its kin)
// are written to the script output, as Print does, before Query returns —
// whether or not the statement failed.
func (s *S) Query(conn, query string, args ...any) (*Result, error) {
	return s.run(conn, query, args...)
}

// Exec runs a non-query statement and returns the rows affected. Its server
// notices are printed as Query's are.
func (s *S) Exec(conn, stmt string, args ...any) (int64, error) {
	r, err := s.run(conn, stmt, args...)
	if err != nil {
		return 0, err
	}
	return r.Affected, nil
}

// run is Query and Exec: the statement through db.Manager.RunNotices, its
// notices printed one per line ("NOTICE: users : 42"). They go to the
// script output rather than back to the script so that Query's signature
// stays as scripts know it, and so they read in the log next to the
// script's own Print lines, in the order things happened.
//
// DDL is logged around the statement (LogDDL):
//
//	DDL a: DROP TABLE staging          before it runs
//	NOTICE: …                          what the server said while it ran
//	DDL a failed: no such table: …     only when it failed
func (s *S) run(conn, stmt string, args ...any) (*Result, error) {
	ddl := s.logDDL(conn, stmt)
	r, notices, err := s.mgr.RunNotices(s.Ctx(), conn, stmt, args...)
	for _, n := range notices {
		s.Print("%s", n.String())
	}
	if err != nil && ddl {
		s.Print("DDL %s failed: %v", conn, err)
	}
	return r, err
}

// Explain describes how conn's database runs stmt, as the TUI's Ctrl+X and
// `dbc explain` do. analyze runs the statement to measure it: on Postgres a
// write is analyzed inside a transaction that is rolled back; on the other
// engines a write is explained but not run. It runs on its own session, not
// one the script shares with Query.
//
//	p, err := s.Explain("pg", "SELECT * FROM orders WHERE user_id = 42", true)
//	s.Print("%s", p.Text(sdb.PlanText{Insights: true}))
//	for _, in := range p.Insights { if in.Severity != "info" { … } }
func (s *S) Explain(conn, stmt string, analyze bool) (*Plan, error) {
	return s.mgr.Explain(s.Ctx(), conn, stmt, db.ExplainOptions{Analyze: analyze})
}

// DB exposes the raw *database/sql.DB for a connection — the escape hatch
// for transactions, prepared statements, or anything else database/sql can do.
func (s *S) DB(conn string) (*sql.DB, error) {
	return s.mgr.DB(conn)
}

// Show pushes a result to the results view (TUI) or prints it (headless).
func (s *S) Show(r *Result) {
	if r == nil || s.show == nil {
		return
	}
	s.show(r)
}

// Print writes a Printf-style message to the script output.
func (s *S) Print(format string, args ...any) {
	if s.print == nil {
		return
	}
	if len(args) == 0 {
		s.print(format)
		return
	}
	s.print(fmt.Sprintf(format, args...))
}

// Export renders a result in the named format (csv|tsv|markdown|html|json|text)
// and writes it to path — or to the system clipboard when path is empty.
func (s *S) Export(r *Result, format, path string) error {
	f, err := export.ParseFormat(format)
	if err != nil {
		return err
	}
	if strings.TrimSpace(path) == "" {
		return export.ToClipboard(r, f)
	}
	if err = export.ToFile(r, f, path); err != nil {
		return serr.Wrap(err, "format", format)
	}
	return nil
}
