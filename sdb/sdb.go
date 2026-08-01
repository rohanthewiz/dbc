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

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/serr"
)

// Result is the tabular result type scripts receive from Query.
type Result = model.Result

// S is the session handle passed to a script's Run function.
type S struct {
	mgr   *db.Manager
	show  func(*model.Result)
	print func(string)
	ctx   context.Context
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
func IsCanceled(err error) bool {
	return errors.Is(err, db.ErrCanceled)
}

// Conns returns the configured connection names.
func (s *S) Conns() []string {
	return s.mgr.Names()
}

// Query runs a statement with parameters on the named connection and
// returns the result set. Use the placeholder style of the target driver
// ($1 for postgres/bytdb, ? for mysql/sqlite).
func (s *S) Query(conn, query string, args ...any) (*Result, error) {
	return s.mgr.RunContext(s.Ctx(), conn, query, args...)
}

// Exec runs a non-query statement and returns the rows affected.
func (s *S) Exec(conn, stmt string, args ...any) (int64, error) {
	r, err := s.mgr.RunContext(s.Ctx(), conn, stmt, args...)
	if err != nil {
		return 0, err
	}
	return r.Affected, nil
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
