package workspace

import (
	"context"
	"time"

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
// changed: a connect or a schema pick lands (setCatalogLocked), the
// connection is left, or a run that may have changed the schema lands — a
// statement starting with a DDL verb, or any script. A load that was in
// flight when the cache was dropped lands under its old generation and is
// never answered from.
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
	err     error         // what the load failed with
	at      time.Time     // when the load ended
	loading chan struct{} // closed when the load in flight ends; nil when none is
}

// ddlVerbs start a statement that may change what the catalog holds.
var ddlVerbs = map[string]bool{
	"create": true, "alter": true, "drop": true, "rename": true, "comment": true, "attach": true, "detach": true,
}

// dropCompletionsLocked forgets the cached schema. The caller holds mu.
func (w *Workspace) dropCompletionsLocked() { w.complGen++ }

// dropCompletionsAfterRunLocked forgets the cached schema after a run that
// may have changed it: a script (whatever it ran), or any statement whose
// verb is DDL — failed runs included, since the statements before the
// failing one did run. The caller holds mu.
func (w *Workspace) dropCompletionsAfterRunLocked(ev *RunDone) {
	if ev.Script {
		w.dropCompletionsLocked()
		return
	}
	for _, s := range ev.Stmts {
		if ddlVerbs[sqlsplit.FirstKeyword(s)] {
			w.dropCompletionsLocked()
			return
		}
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
	ready = true
	if conn != "" {
		c := w.compl
		switch {
		case c.conn != conn || c.gen != w.complGen || c.loading != nil:
			ready = false
		case c.err != nil && time.Since(c.at) > complRetry:
			ready = false // time to try again
		default:
			sc = c.schema
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
		Schema: sc, Driver: driver, Focus: focus, Buffer: buffer, Caret: caret,
	}), true
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
	conn, gen := w.active, w.complGen
	if conn == "" {
		w.mu.Unlock()
		return nil
	}
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
		case c.err == nil || time.Since(c.at) <= complRetry:
			w.mu.Unlock()
			return nil // cached, or failed recently: nothing to do
		}
	}
	done := make(chan struct{})
	w.compl = complState{conn: conn, gen: gen, loading: done}
	w.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, CompletionTimeout)
	defer cancel()
	sc, err := w.mgr.Schema(ctx, conn)

	w.mu.Lock()
	// a newer load (after the cache was dropped and asked for again) owns
	// the state now; this one's outcome is for a schema that is gone
	if w.compl.loading == done {
		w.compl = complState{conn: conn, gen: gen, schema: sc, err: err, at: time.Now()}
	}
	w.mu.Unlock()
	close(done)
	return err
}
