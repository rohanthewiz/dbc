package workspace

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/erd"
	"github.com/rohanthewiz/dbc/sqlcomplete"
)

// ConnCompletions is completion against a connection given by name rather
// than a workspace's active one: a pipeline tab is on no connection, and
// each of its nodes names its own (`conn`). dbc web keeps one for the
// pipeline inspector, whose sql fields complete against their node's
// connection (N-192) and whose table fields offer that connection's tables
// (Tables, N-176).
//
// The reads are a workspace's (complete.go): the schema read on the pool,
// whole or — a catalog too big to read whole — scoped to the search path,
// and the routines beside it; a failed read remembered for complRetry so
// a server that is down is not asked on every keystroke. What differs is
// when an entry is dropped. A workspace drops its cache when one of its own
// runs may have changed the schema; nothing runs here, so an entry is read
// again once it is ConnComplTTL old — a table a pipeline run just made is
// offered within the minute — and Drop forgets a connection at once (its
// settings edited, say).
//
//	Complete(conn, buffer, caret) / Tables(conn)
//	   │
//	   ├─ entry fresh (read < ConnComplTTL ago, or failed < complRetry ago) ─► answer
//	   ├─ a read in flight ─► wait for it
//	   └─ else ─► read (readCompletions, readRoutines), then answer
//
// It blocks for a read, so a caller asks from a goroutine of its own (an
// HTTP handler's), never an event loop.
type ConnCompletions struct {
	cfg *config.Config
	mgr *db.Manager

	mu      sync.Mutex
	entries map[string]*complState // by connection name
	scoped  map[string]bool        // connections whose whole read overflowed
}

// ConnComplTTL is how long a ConnCompletions entry is answered from before
// it is read again.
const ConnComplTTL = time.Minute

// NewConnCompletions makes an empty cache over mgr's connections.
func NewConnCompletions(cfg *config.Config, mgr *db.Manager) *ConnCompletions {
	return &ConnCompletions{cfg: cfg, mgr: mgr, entries: map[string]*complState{}, scoped: map[string]bool{}}
}

// Drop forgets conn's entry ("" forgets every one), so the next ask reads
// again.
func (c *ConnCompletions) Drop(conn string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if conn == "" {
		clear(c.entries)
		return
	}
	delete(c.entries, conn)
}

// Complete suggests what to type at caret (a byte offset into buffer) on
// conn, reading its schema first unless cached. The error is the read's,
// for the editor to note; the suggestions then are the dialect's
// vocabulary alone, as a workspace's are after a failed load.
func (c *ConnCompletions) Complete(ctx context.Context, conn, buffer string, caret int) (sqlcomplete.Result, error) {
	st, err := c.load(ctx, conn)
	driver := ""
	if cc, ok := c.cfg.ConnByName(conn); ok {
		driver = cc.Driver
	}
	return sqlcomplete.Complete(sqlcomplete.Request{
		Schema: st.schema, Routines: st.routines, Driver: driver, SearchPath: st.path, Buffer: buffer, Caret: caret,
	}), err
}

// Tables is conn's tables and views as a node's table field takes them:
// each by its label (erd.Table.Label — bare when the connection has one
// schema, schema.name when it has several: the sidebar's rule), tables
// first, then views, each sorted. A scoped read lists the scope's only.
func (c *ConnCompletions) Tables(ctx context.Context, conn string) ([]string, error) {
	st, err := c.load(ctx, conn)
	if err != nil || st.schema == nil {
		return []string{}, err
	}
	var tables, views []string
	for _, t := range st.schema.Tables {
		if t.View {
			views = append(views, t.Label)
		} else {
			tables = append(tables, t.Label)
		}
	}
	slices.Sort(tables)
	slices.Sort(views)
	return append(tables, views...), nil
}

// load is conn's entry, read when it is missing or stale; a caller that
// arrives while a read is in flight waits for that one. An unknown
// connection is an error without a read.
func (c *ConnCompletions) load(ctx context.Context, conn string) (complState, error) {
	if _, ok := c.cfg.ConnByName(conn); !ok || conn == "" {
		return complState{}, &unknownConnError{conn}
	}
	c.mu.Lock()
	for {
		e := c.entries[conn]
		switch {
		case e == nil:
		case e.loading != nil:
			wait := e.loading
			c.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return complState{}, ctx.Err()
			}
			c.mu.Lock()
			continue // the read landed (or was dropped): look again
		case e.err != nil && time.Since(e.at) <= complRetry,
			e.err == nil && time.Since(e.at) <= ConnComplTTL:
			st := *e
			c.mu.Unlock()
			return st, st.err
		}
		break
	}
	done := make(chan struct{})
	mine := &complState{conn: conn, loading: done}
	c.entries[conn] = mine
	scoped := c.scoped[conn]
	c.mu.Unlock()

	sc, path, scope, tooBig, err := readCompletions(ctx, c.mgr, navigable(c.cfg, conn), conn, "", scoped)
	st := complState{conn: conn, schema: sc, path: path, scope: scope, err: err, at: time.Now()}
	if err == nil {
		st.routines = readRoutines(ctx, c.mgr, conn, scope)
	}

	c.mu.Lock()
	if tooBig {
		c.scoped[conn] = true
	}
	// a Drop while reading took the entry away: the read still answers
	// this caller, but is not kept
	if c.entries[conn] == mine {
		c.entries[conn] = &st
	}
	c.mu.Unlock()
	close(done)
	return st, err
}

// unknownConnError is an ask about a connection the config does not have.
type unknownConnError struct{ conn string }

func (e *unknownConnError) Error() string { return "no connection named " + e.conn }

// schemaOf is the cached schema of conn, nil when none is cached: for
// tests.
func (c *ConnCompletions) schemaOf(conn string) *erd.Schema {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[conn]; e != nil {
		return e.schema
	}
	return nil
}
