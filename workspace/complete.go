package workspace

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/erd"
	"github.com/rohanthewiz/dbc/sqlcomplete"
	"github.com/rohanthewiz/dbc/sqlsplit"
)

// Completion: the editor's suggestions, from the active connection's schema.
//
// The suggestions themselves are sqlcomplete's (pure, tested without a
// database); what lives here is the schema they are drawn from, read once
// per connection and kept, because a completion is asked for on nearly
// every keystroke and a catalog read takes from milliseconds to seconds.
//
//	UI asks Complete(buffer, caret)
//	   │
//	   ├─ cache holds the active connection's schema ──► answer at once
//	   │
//	   └─ it does not ──► ready=false; the UI runs LoadCompletions off its
//	                      event loop (a tea.Cmd, a handler goroutine) and
//	                      asks again when it returns
//
// THE CACHE IS DROPPED (complGen bumped) whenever what it describes may have
// changed: a connect lands (landConnect — a reconnect to the same connection
// included), the connection is left (Disconnect), or a run that may have
// changed the schema lands — a statement starting with a DDL verb, or any
// script. A load that was in flight when the cache was dropped lands under
// its old generation and is never answered from.
//
// A SCHEMA PICK KEEPS IT. Manager.Schema reads every user schema of the
// database, whichever one the sidebar shows, so a pick changes nothing the
// cache holds — only which schema's tables rank first, and Complete reads
// that (w.schema, the request's Focus) on every ask. Dropping it would make
// each pick on a big Postgres catalog re-read the whole of it for the next
// suggestion. (A database pick is a connect to a derived connection, a
// different cache key, and reads afresh.) The exception is a scoped cache,
// below, that does not hold the picked schema.
//
// A CATALOG TOO BIG TO READ WHOLE (db.ErrCatalogTooBig: some query past
// 250,000 rows) is read SCOPED instead, on Postgres: the sidebar's schema
// and the search path's (the default public when the path is unknown) —
// the schemas a statement most likely names. Completion then knows those
// schemas' tables and columns, and not the others'; a schema.table in one
// of the others completes as nothing, as it did when the load failed. The
// connection is remembered as too big (complScoped), so later loads go
// straight to the scoped read rather than paying for the failing whole one
// first; a diagram that met the overflow (Diagram) marks it the same way.
// A pick of a schema the scoped cache does not hold reads again.
//
//	load ──► whole (Manager.Schema) ──ok──────────────► cache, scope nil
//	           │ ErrCatalogTooBig
//	           ▼
//	         complScoped[conn] = true
//	           ▼
//	         scoped (Manager.SchemaIn: focus + path) ──► cache, scope set
//
// A FAILED LOAD is remembered for complRetry. A catalog too big to read (the
// ERD's 250,000-row bound) or a server that is down would otherwise be asked
// again on every keystroke — and a big catalog's failing read is itself
// expensive, so the wait is minutes, not seconds. Meanwhile completion
// answers with the dialect's vocabulary alone, and the UI is told why once.
// A reconnect drops the remembered failure with the rest of the cache.
//
// Like Diagram and LookupColumns, the load runs on the POOL, never the
// pinned session: it must not wait for the user's long query, nor read the
// schema from inside their open transaction.

// CompletionTimeout bounds reading a schema for completion. The ERD's bound:
// the same three catalog queries.
const CompletionTimeout = DiagramTimeout

// complRetry is how long a failed load is remembered before it is tried
// again.
const complRetry = 5 * time.Minute

// complState is the completion cache. It is guarded by Workspace.mu.
type complState struct {
	conn    string
	gen     int           // the complGen it was loaded for
	schema  *erd.Schema   // nil while loading, or when the load failed
	path    []string      // the search path bare names resolve through; nil: the dialect's default
	scope   []string      // the schemas a scoped load read; nil: every schema
	err     error         // what the load failed with
	at      time.Time     // when the load ended
	loading chan struct{} // closed when the load in flight ends; nil when none is
}

// covers reports whether the cache holds the schema the sidebar shows:
// always, unless it is a scoped load of other schemas.
func (c complState) covers(focus string) bool {
	return c.scope == nil || focus == "" || slices.Contains(c.scope, focus)
}

// complScope is the schemas a scoped load reads: the sidebar's, then the
// search path's, or the dialect's default (public) when the path is
// unknown — what sqlcomplete assumes then too. The order is only for
// reading the cache in a debugger; the queries take the list as a set.
func complScope(focus string, path []string) []string {
	if path == nil {
		path = []string{"public"}
	}
	scope := make([]string, 0, len(path)+1)
	if focus != "" {
		scope = append(scope, focus)
	}
	for _, s := range path {
		if !slices.Contains(scope, s) {
			scope = append(scope, s)
		}
	}
	return scope
}

// dropCompletionsLocked forgets the cached schema. The caller holds mu.
func (w *Workspace) dropCompletionsLocked() { w.complGen++ }

// dropCompletionsAfterRunLocked forgets the cached schema after a run that
// may have changed it: a script (whatever it ran), or any statement that
// changes the catalog (sqlsplit.ChangesCatalog) — failed runs included,
// since the statements before the failing one did run. The caller holds mu.
//
// A script drops the cache whatever it ran, even though sdb.S now knows
// whether it ran DDL (CatalogChanged), which the sidebar's relist goes by
// (landRun): a drop costs nothing until the next completion asks, and a
// script can reach the database through DB's raw handle, out of S's sight.
func (w *Workspace) dropCompletionsAfterRunLocked(ev *RunDone) {
	if ev.Script || slices.ContainsFunc(ev.Stmts, sqlsplit.ChangesCatalog) {
		w.dropCompletionsLocked()
	}
}

// Complete suggests what to type at caret (a byte offset into buffer), from
// the active connection's cached schema. It never blocks on the database:
// ready is false when the schema is not cached yet, and the caller then runs
// LoadCompletions off its event loop and asks again. With no connection, or
// after a failed load, it is ready with the dialect's vocabulary alone.
func (w *Workspace) Complete(buffer string, caret int) (res sqlcomplete.Result, ready bool) {
	w.mu.Lock()
	conn, focus := w.active, w.schema
	var sc *erd.Schema
	var path []string
	ready = true
	if conn != "" {
		c := w.compl
		switch {
		case c.conn != conn || c.gen != w.complGen || c.loading != nil:
			ready = false
		case !c.covers(focus):
			ready = false // a scoped load of other schemas: read this one
		case c.err != nil && time.Since(c.at) > complRetry:
			ready = false // time to try again
		default:
			sc, path = c.schema, c.path
		}
	}
	w.mu.Unlock()
	if !ready {
		return sqlcomplete.Result{From: caret, To: caret}, false
	}
	driver := ""
	if cc, ok := w.cfg.ConnByName(conn); ok {
		driver = cc.Driver
	}
	return sqlcomplete.Complete(sqlcomplete.Request{
		Schema: sc, Driver: driver, Focus: focus, SearchPath: path, Buffer: buffer, Caret: caret,
	}), true
}

// Rename renames the alias or CTE under caret in buffer to name
// (sqlcomplete.Rename), quoting it as the active connection's dialect needs.
// It needs no schema, so unlike Complete it never waits on a load. The
// error is a sentence for the user: why there is nothing to rename.
func (w *Workspace) Rename(buffer string, caret int, name string) ([]sqlcomplete.Edit, error) {
	driver := ""
	if cc, ok := w.cfg.ConnByName(w.Active()); ok {
		driver = cc.Driver
	}
	return sqlcomplete.Rename(buffer, caret, name, driver)
}

// LoadCompletions reads the active connection's schema into the completion
// cache, unless it is cached already. It blocks for the catalog round trips
// (bounded by CompletionTimeout and ctx), so a UI calls it off its event
// loop. Callers that arrive while a load is in flight wait for that one
// rather than starting their own.
//
// The error is the load's, for the UI to report once; completion goes on
// with the vocabulary alone either way.
func (w *Workspace) LoadCompletions(ctx context.Context) error {
	w.mu.Lock()
	conn, gen, focus := w.active, w.complGen, w.schema
	if conn == "" {
		w.mu.Unlock()
		return nil
	}
	scoped := w.complScoped[conn]
	c := w.compl
	if c.conn == conn && c.gen == gen {
		switch {
		case c.loading != nil:
			wait := c.loading
			w.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return ctx.Err()
			}
			w.mu.Lock()
			err := w.compl.err
			w.mu.Unlock()
			return err
		case !c.covers(focus):
			// a scoped load of other schemas: read again, for this one
		case c.err == nil || time.Since(c.at) <= complRetry:
			w.mu.Unlock()
			return nil // cached, or failed recently: nothing to do
		}
	}
	done := make(chan struct{})
	w.compl = complState{conn: conn, gen: gen, loading: done}
	w.mu.Unlock()

	sc, path, scope, err := w.readCompletions(ctx, conn, focus, scoped)

	w.mu.Lock()
	// a newer load (after the cache was dropped and asked for again) owns
	// the state now; this one's outcome is for a schema that is gone
	if w.compl.loading == done {
		w.compl = complState{conn: conn, gen: gen, schema: sc, path: path, scope: scope, err: err, at: time.Now()}
	}
	w.mu.Unlock()
	close(done)
	return err
}

// readCompletions reads what the completion cache holds: the schema, whole
// or scoped (see A CATALOG TOO BIG above), and the search path. scope is
// the schemas a scoped read covered, nil after a whole read; on an error
// the schema and scope are nil.
//
// The path is read first, being cheap and what a scoped read is scoped by.
// Failing it is no reason to go without the schema: the dialect's default
// (public) is then assumed, as sqlcomplete does.
//
// Each read of the schema gets CompletionTimeout of its own, so a whole
// read that ran long before it overflowed does not leave the scoped one
// none.
func (w *Workspace) readCompletions(ctx context.Context, conn, focus string, scoped bool) (*erd.Schema, []string, []string, error) {
	pctx, pcancel := context.WithTimeout(ctx, CompletionTimeout)
	path, _ := w.mgr.SearchPath(pctx, conn)
	pcancel()

	if !scoped {
		sctx, scancel := context.WithTimeout(ctx, CompletionTimeout)
		sc, err := w.mgr.Schema(sctx, conn)
		scancel()
		if !errors.Is(err, db.ErrCatalogTooBig) || !w.navigable(conn) {
			if err != nil {
				return nil, path, nil, err
			}
			return sc, path, nil, nil
		}
		w.mu.Lock()
		if w.complScoped == nil {
			w.complScoped = map[string]bool{}
		}
		w.complScoped[conn] = true
		w.mu.Unlock()
	}

	scope := complScope(focus, path)
	sctx, scancel := context.WithTimeout(ctx, CompletionTimeout)
	defer scancel()
	sc, err := w.mgr.SchemaIn(sctx, conn, scope)
	if err != nil {
		return nil, path, nil, err
	}
	return sc, path, scope, nil
}

// navigable reports whether conn's driver lists its tables by schema
// (db.Navigable), the only kind a scoped read narrows.
func (w *Workspace) navigable(conn string) bool {
	cc, ok := w.cfg.ConnByName(conn)
	return ok && db.Navigable(cc.Driver)
}
