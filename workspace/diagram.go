package workspace

import (
	"context"
	"strings"
	"time"

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
	ctx, cancel := context.WithTimeout(ctx, DiagramTimeout)
	defer cancel()
	s, err := w.mgr.Schema(ctx, conn)
	if err != nil {
		return nil, err
	}
	out, missing := s.Select(sel)
	if len(missing) > 0 {
		return nil, refuse(Invalid, Warn, "no table %s on %s", strings.Join(missing, ", "), conn)
	}
	return out, nil
}
