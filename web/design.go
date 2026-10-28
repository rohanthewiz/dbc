package web

import (
	"context"
	"errors"
	"sort"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/designer"
)

// The query designer in the browser: a design tab (designer.js) edits a
// designer.Design on a canvas, and every edit asks the server to judge it.
// The rules, the SQL and the story are all the designer package's, so the
// page draws and never decides:
//
//	GET  …/design/tables              the palette: every table, its size
//	POST …/design {design, fresh}     ─► designer.Analysis (SQL, findings,
//	                                     story, cards, edges, related)
//	POST …/design/add {design, schema, name, x, y}
//	                                  ─► {analysis, notes}: the table added
//	                                     and joined by its foreign keys
//	POST …/design/plan {design}       ─► designer.PlanView: the engine's
//	                                     EXPLAIN (estimates only), painted
//	                                     onto the tables
//
// Every call reads the tab's ACTIVE connection's catalog (cached in the
// workspace, workspace/design.go), on the pool and outside the run slot:
// designing never waits for a running query, and a query never waits for
// the designer.
//
// The plan is explained from the DESIGN, never from SQL the request
// carries: the statement is the one the designer writes (always a SELECT),
// so the route cannot be used to EXPLAIN ANALYZE — that is, to run —
// anything else.

type designReq struct {
	Design designer.Design `json:"design"`
	// Fresh drops the cached catalog first: the page's ⟳, and the first
	// analysis after a statement that may have changed the schema.
	Fresh bool `json:"fresh"`
}

type designAddReq struct {
	Design designer.Design `json:"design"`
	Schema string          `json:"schema"`
	Name   string          `json:"name"`
	X      float64         `json:"x"`
	Y      float64         `json:"y"`
}

// designKeys are the design's tables as catalog keys, for their indexes.
func designKeys(d designer.Design) []designer.TableKey {
	out := make([]designer.TableKey, 0, len(d.Tables))
	for _, t := range d.Tables {
		out = append(out, designer.TableKey{Schema: t.Schema, Name: t.Name})
	}
	return out
}

// catalogFacts says what the catalog could tell, so the page can say why a
// rule is quiet ("indexes are not listed on this engine").
type catalogFacts struct {
	Indexes   bool `json:"indexes"`
	Estimates bool `json:"estimates"`
}

func facts(c *designer.Catalog) catalogFacts {
	return catalogFacts{Indexes: c.IndexesKnown, Estimates: len(c.Rows) > 0}
}

// handleDesign analyzes a design.
func (s *Server) handleDesign(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req designReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if req.Fresh {
		t.ws.ForgetDesignCatalog()
	}
	cat, err := t.ws.DesignCatalog(context.Background(), designKeys(req.Design))
	if err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, map[string]any{"analysis": designer.Analyze(req.Design, cat), "catalog": facts(cat)})
}

// handleDesignAdd puts a table on the canvas, joined.
func (s *Server) handleDesignAdd(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req designAddReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	keys := append(designKeys(req.Design), designer.TableKey{Schema: req.Schema, Name: req.Name})
	cat, err := t.ws.DesignCatalog(context.Background(), keys)
	if err != nil {
		return fail(ctx, err)
	}
	d, notes, err := designer.AddTable(req.Design, cat, req.Schema, req.Name, req.X, req.Y)
	if errors.Is(err, designer.ErrNoSuchTable) {
		return fail(ctx, badRequest("no table %s on %s", req.Name, t.ws.Active()))
	}
	if err != nil {
		return fail(ctx, err)
	}
	if notes == nil {
		notes = []string{}
	}
	return ok(ctx, map[string]any{"analysis": designer.Analyze(d, cat), "catalog": facts(cat), "notes": notes})
}

// handleDesignPlan explains the design's statement. A design with errors
// has no statement worth planning, and is refused in words.
func (s *Server) handleDesignPlan(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req designReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	cat, err := t.ws.DesignCatalog(context.Background(), designKeys(req.Design))
	if err != nil {
		return fail(ctx, err)
	}
	a := designer.Analyze(req.Design, cat)
	if !a.Valid {
		return fail(ctx, conflict("the design has errors to fix before it can be planned"))
	}
	p, err := t.ws.DesignPlan(context.Background(), a.SQL)
	if err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, designer.PaintPlan(p, a.Design))
}

// paletteTable is one table of the palette.
type paletteTable struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Label  string `json:"label"`
	View   bool   `json:"view,omitempty"`
	Rows   int64  `json:"rows"`
	Known  bool   `json:"known"` // Rows is an estimate the engine gave
	Cols   int    `json:"cols"`
	Keys   int    `json:"keys"` // foreign keys in or out: how connected it is
}

// handleDesignTables lists the palette: every table and view of the
// connection's catalog, by label.
func (s *Server) handleDesignTables(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.Request().QueryParam("fresh") == "1" {
		t.ws.ForgetDesignCatalog()
	}
	cat, err := t.ws.DesignCatalog(context.Background(), nil)
	if err != nil {
		return fail(ctx, err)
	}
	keys := map[string]int{}
	for _, r := range cat.Schema.Rels {
		keys[r.Child.Label]++
		if r.Parent != r.Child {
			keys[r.Parent.Label]++
		}
	}
	out := make([]paletteTable, 0, len(cat.Schema.Tables))
	for _, et := range cat.Schema.Tables {
		n, known := cat.Rows[designer.TableKey{Schema: et.Schema, Name: et.Name}]
		out = append(out, paletteTable{Schema: et.Schema, Name: et.Name, Label: et.Label, View: et.View,
			Rows: n, Known: known, Cols: len(et.Cols), Keys: keys[et.Label]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return ok(ctx, map[string]any{"conn": t.ws.Active(), "driver": cat.Driver, "tables": out, "catalog": facts(cat)})
}
