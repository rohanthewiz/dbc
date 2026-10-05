package workspace

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/erd"
)

// DiagramTimeout bounds reading a schema for a diagram: three catalog
// queries, which take milliseconds on a normal database and seconds on a
// catalog of tens of thousands of tables. Longer than SchemaLookupTimeout,
// because here the user asked for the diagram and is waiting for it, while
// the assistant's lookup is a side errand that must not hold up a question.
const DiagramTimeout = 30 * time.Second

// Diagram reads the active connection's schema and narrows it to sel — the
// entity-relationship diagram both UIs draw (the TUI saves and opens it,
// dbc web shows it and serves its files). It blocks for the catalog round
// trips, so a UI calls it off its event loop.
//
// Like LookupColumns it runs on the POOL, not the pinned session: a diagram
// must not wait for a long query the user is running, nor read the schema
// from inside their open transaction.
//
// It does not take the run slot either — reading the catalog is not a run,
// and a diagram while a query runs is a reasonable thing to want.
//
// A table in sel that is not in the schema is refused (Invalid) rather than
// dropped quietly: a diagram "around orders" that silently shows the whole
// database would be a confusing answer.
//
// A diagram of everything (sel naming no tables) is of the schema the
// sidebar lists, when it lists one (CatalogSchema): on a big database "all"
// means what the user is looking at, not every table the server holds.
//
// A CATALOG TOO BIG TO READ WHOLE (db.ErrCatalogTooBig) is read SCOPED
// instead, on Postgres, as completion does (complete.go): Manager.SchemaIn
// over the schemas the selection can be about (diagramScope). The
// connection is then remembered as too big (complScoped, shared with
// completion — both read the same Manager.Schema, so one finding it
// overflow is the other's answer too), and later diagrams and completion
// loads skip the failing whole read.
//
//	whole (Manager.Schema) ──ok────────────────────────────► Select
//	  │ ErrCatalogTooBig            (or complScoped[conn] already)
//	  ▼
//	complScoped[conn] = true
//	  ▼
//	scoped (Manager.SchemaIn: diagramScope) ──► Select
//
// What a scoped diagram misses: a neighbour (sel.Depth) in a schema outside
// the scope, since SchemaIn leaves out keys to tables it did not read; and
// a bare name that is unique in the scope but not in the database resolves
// rather than being refused as ambiguous. Both are the price of drawing
// anything at all where the whole read cannot.
func (w *Workspace) Diagram(ctx context.Context, sel erd.Selection) (*erd.Schema, error) {
	w.mu.Lock()
	conn := w.active
	if sel.Schema == "" {
		sel.Schema = w.schema
	}
	w.mu.Unlock()
	if conn == "" {
		return nil, refuse(NoConnection, Warn, "no active connection — pick one in the sidebar")
	}
	s, err := w.readDiagram(ctx, conn, sel)
	if err != nil {
		return nil, err
	}
	out, missing := s.Select(sel)
	if len(missing) > 0 {
		return nil, refuse(Invalid, Warn, "no table %s on %s", strings.Join(missing, ", "), conn)
	}
	return out, nil
}

// readDiagram reads the schema a diagram of sel is drawn from: whole, or
// scoped when the whole catalog is too big (see Diagram).
//
// Each read gets DiagramTimeout of its own, so a whole read that ran long
// before it overflowed does not leave the scoped one none.
func (w *Workspace) readDiagram(ctx context.Context, conn string, sel erd.Selection) (*erd.Schema, error) {
	w.mu.Lock()
	scoped := w.complScoped[conn]
	w.mu.Unlock()

	if !scoped {
		sctx, scancel := context.WithTimeout(ctx, DiagramTimeout)
		s, err := w.mgr.Schema(sctx, conn)
		scancel()
		if !errors.Is(err, db.ErrCatalogTooBig) || !w.navigable(conn) {
			return s, err
		}
		w.mu.Lock()
		if w.complScoped == nil {
			w.complScoped = map[string]bool{}
		}
		w.complScoped[conn] = true
		w.mu.Unlock()
	}

	// The search path is read only when the scope may need it: a diagram
	// of one schema's everything needs that schema alone, and adding the
	// path's (public, often the biggest) would only risk overflowing again
	// for tables Select then drops.
	var path []string
	if len(sel.Tables) > 0 || sel.Schema == "" {
		pctx, pcancel := context.WithTimeout(ctx, DiagramTimeout)
		path, _ = w.mgr.SearchPath(pctx, conn)
		pcancel()
	}
	sctx, scancel := context.WithTimeout(ctx, DiagramTimeout)
	defer scancel()
	return w.mgr.SchemaIn(sctx, conn, diagramScope(sel, path))
}

// diagramScope is the schemas a scoped diagram of sel reads:
//
//   - everything in a schema (no Tables, Schema set): that schema alone,
//     the one Select keeps;
//   - around named tables: the sidebar's schema and the search path's
//     (complScope — where a bare name most likely lives), plus the schema
//     of each name spelled schema.table. Such a schema is added as spelled
//     and lower-cased too, since Find matches names case-insensitively
//     while the catalog query matches schema names exactly, and an
//     unquoted Postgres name is stored lower-case;
//   - neither: the search path's, the schemas a session sees unqualified.
//
// path is the search path, nil when unknown (complScope then assumes
// public); it is unused in the first case.
func diagramScope(sel erd.Selection, path []string) []string {
	if len(sel.Tables) == 0 && sel.Schema != "" {
		return []string{sel.Schema}
	}
	scope := complScope(sel.Schema, path)
	for _, n := range sel.Tables {
		sch, _, ok := strings.Cut(n, ".")
		if !ok || sch == "" {
			continue
		}
		for _, s := range []string{sch, strings.ToLower(sch)} {
			if !slices.Contains(scope, s) {
				scope = append(scope, s)
			}
		}
	}
	return scope
}
