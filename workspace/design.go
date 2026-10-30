package workspace

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/designer"
	"github.com/rohanthewiz/dbc/erd"
	"github.com/rohanthewiz/dbc/explain"
)

// The query designer's reads (package designer judges a design against a
// Catalog; dbc web's design tab draws it). Like Diagram they run on the
// POOL and outside the run slot: designing must not wait for a query the
// user is running, nor read the catalog from inside their transaction.
//
// THE CATALOG IS CACHED per connection, because the page asks for an
// analysis on every edit (debounced), and each would otherwise cost the
// three catalog queries of a diagram plus the row estimates:
//
//	DesignCatalog(tables) ──fresh cache?──yes──► + indexes of tables not yet read
//	                          │ no (or fresh asked: ⟳, or after a DDL run)
//	                          ▼
//	        readDiagram (whole, or scoped on a big catalog) + RowEstimates
//
// Indexes are read per table, as the design adds them: a whole catalog's
// indexes are as many rows as its columns, and the design needs a handful.
//
// designTTL bounds how stale a cached catalog may get. It is short enough
// that an index created in another tool shows within a minute, and the
// page asks for a fresh read after any statement it ran that may have
// changed the schema (db.ChangesRows says a CREATE INDEX does).
const designTTL = time.Minute

// DesignPlanTimeout bounds the designer's background EXPLAIN. It is a
// planning, not a run — milliseconds normally — so a server that takes
// longer is busy, and the plan is simply not drawn this time.
const DesignPlanTimeout = 8 * time.Second

// designCache is one connection's catalog for the designer.
type designCache struct {
	at       time.Time
	schema   *erd.Schema
	rows     map[designer.TableKey]int64
	indexes  map[designer.TableKey][]designer.Index
	idxRead  map[designer.TableKey]bool
	idxKnown bool // the engine lists indexes (false on bytdb, or after a failed read)
	idxTried bool // the listing was tried at all
}

// designState is the Workspace's cache, under its own lock: a catalog read
// takes seconds on a big database, and w.mu must never be held across one.
type designState struct {
	mu    sync.Mutex
	cache map[string]*designCache
}

// ForgetDesignCatalog drops the cached catalog of the active connection, so
// the next analysis reads it afresh (the page's ⟳, or a DDL it just ran).
func (w *Workspace) ForgetDesignCatalog() {
	conn := w.Active()
	w.design.mu.Lock()
	delete(w.design.cache, conn)
	w.design.mu.Unlock()
}

// DesignCatalog returns what the designer knows of the active connection,
// with the indexes of tables read (the design's). It blocks for catalog
// round trips when the cache is cold, so a UI calls it off its event loop.
func (w *Workspace) DesignCatalog(ctx context.Context, tables []designer.TableKey) (*designer.Catalog, error) {
	conn := w.Active()
	if conn == "" {
		return nil, refuse(NoConnection, Warn, "no active connection — pick one in the sidebar")
	}
	cc, _ := w.cfg.ConnByName(conn)

	w.design.mu.Lock()
	c := w.design.cache[conn]
	if c != nil && time.Since(c.at) > designTTL {
		c = nil
	}
	w.design.mu.Unlock()

	if c == nil {
		s, err := w.readDiagram(ctx, conn, erd.Selection{Views: true})
		if err != nil {
			return nil, err
		}
		c = &designCache{at: time.Now(), schema: s, rows: map[designer.TableKey]int64{},
			indexes: map[designer.TableKey][]designer.Index{}, idxRead: map[designer.TableKey]bool{}}
		// estimates are a help, not a need: a server that will not give
		// them leaves the size rules quiet (evidence or silence)
		ectx, cancel := context.WithTimeout(ctx, DiagramTimeout)
		if est, err := w.mgr.RowEstimates(ectx, conn); err == nil {
			for t, n := range est {
				c.rows[designer.TableKey{Schema: t.Schema, Name: t.Name}] = n
			}
		}
		cancel()
		w.design.mu.Lock()
		if w.design.cache == nil {
			w.design.cache = map[string]*designCache{}
		}
		w.design.cache[conn] = c
		w.design.mu.Unlock()
	}

	// the indexes of the tables not read yet, in one query
	w.design.mu.Lock()
	var want []db.TableRef
	for _, t := range tables {
		if !c.idxRead[t] {
			want = append(want, db.TableRef{Schema: t.Schema, Name: t.Name})
		}
	}
	tried := c.idxTried
	w.design.mu.Unlock()
	if len(want) > 0 && (!tried || c.idxKnown) {
		ictx, cancel := context.WithTimeout(ctx, DiagramTimeout)
		idx, err := w.mgr.Indexes(ictx, conn, want)
		cancel()
		w.design.mu.Lock()
		c.idxTried = true
		switch {
		case err == nil:
			c.idxKnown = true
			for _, t := range want {
				c.idxRead[designer.TableKey{Schema: t.Schema, Name: t.Name}] = true
			}
			for _, x := range idx {
				k := designer.TableKey{Schema: x.Schema, Name: x.Table}
				c.indexes[k] = append(c.indexes[k], designer.Index{Name: x.Name, Cols: x.Cols,
					Unique: x.Unique, Primary: x.Primary, Partial: x.Partial, Method: x.Method, Order: x.Order})
			}
		case errors.Is(err, db.ErrNoIndexes):
			c.idxKnown = false
		default:
			// a failed read (a timeout, a privilege) leaves what was read
			// before standing, but says nothing about these tables
			c.idxKnown = c.idxKnown && len(c.idxRead) > 0
		}
		w.design.mu.Unlock()
	}

	w.design.mu.Lock()
	defer w.design.mu.Unlock()
	cat := &designer.Catalog{Driver: cc.Driver, Schema: c.schema, Rows: c.rows,
		Indexes: c.indexes, IndexesKnown: c.idxKnown}
	// a table whose indexes could not be read must not be judged as having
	// none: with any of the asked tables unread, the index rules stay quiet
	for _, t := range tables {
		if !c.idxRead[t] {
			cat.IndexesKnown = false
		}
	}
	return cat, nil
}

// DesignPlan asks the active connection's engine how it would run a
// designed statement — estimates only: EXPLAIN without ANALYZE plans the
// statement and runs nothing. stmt must be one the designer wrote (a
// SELECT); the caller writes it from the design rather than taking SQL
// from a request, since an "EXPLAIN ANALYZE …" handed to Explain would be
// run.
func (w *Workspace) DesignPlan(ctx context.Context, stmt string) (*explain.Plan, error) {
	conn := w.Active()
	if conn == "" {
		return nil, refuse(NoConnection, Warn, "no active connection")
	}
	pctx, cancel := context.WithTimeout(ctx, DesignPlanTimeout)
	defer cancel()
	return w.mgr.Explain(pctx, conn, stmt, db.ExplainOptions{})
}
