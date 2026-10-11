package web

import (
	"context"

	"github.com/rohanthewiz/rweb"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/sqlcomplete"
)

// The editor's completions: Monaco's suggest widget asks here as the user
// types (editor.js registers the provider), and the answer is the same
// sqlcomplete runs for the TUI's popup — one set of rules for both UIs.
//
//	Monaco ──POST /api/v1/ws/:id/complete {buffer, caret}──► handleComplete
//	                                                     │
//	           cache cold? LoadCompletions (blocks this  │
//	           request only, not the page) then answer   ▼
//	       ◄── {from, to, items[], incomplete, note} ── workspace.Complete
//
// Offsets travel in UTF-16 units, as the page counts them; the conversion is
// here, as for the statement marker (handleStmt).
//
// A pipeline tab is on no connection — each node names its own — so the
// inspector's small editors ask by connection instead (N-192, N-176):
//
//	sql field ──POST /api/v1/conn-complete {conn, buffer, caret}──► the same answer,
//	                                              workspace.ConnCompletions (one cache per name)
//	table field ──GET /api/v1/conn-tables?conn=──► {tables: […]}, from that cache
//
// The name travels in the body or the query, not the path: a
// "<conn>/<database>" name holds a slash.

type completeReq struct {
	Buffer string `json:"buffer"`
	Caret  int    `json:"caret"`
}

// completeItem is sqlcomplete.Item with its cursor in UTF-16 units.
type completeItem struct {
	sqlcomplete.Item
	Cursor int `json:"cursor"` // UTF-16 units into Insert; -1 for after it
}

// handleComplete answers a completion request. The first request on a
// connection waits for its schema to load; a load that fails is reported in
// note (the page logs it once) and the answer is the dialect's vocabulary.
func (s *Server) handleComplete(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req completeReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Buffer) > maxBuffer {
		return ok(ctx, map[string]any{"items": []completeItem{}})
	}
	caret := byteOffset(req.Buffer, req.Caret)
	res, ready := t.ws.Complete(req.Buffer, caret)
	note := ""
	if !ready {
		if lerr := t.ws.LoadCompletions(context.Background()); lerr != nil {
			note = "completion is without the schema: " + serr.StringFromErr(lerr)
		}
		res, _ = t.ws.Complete(req.Buffer, caret)
	}
	return ok(ctx, completeAnswer(req.Buffer, res, note))
}

// completeAnswer is a completion's answer in the page's terms: offsets in
// UTF-16 units into buffer.
func completeAnswer(buffer string, res sqlcomplete.Result, note string) map[string]any {
	items := make([]completeItem, len(res.Items))
	for i, it := range res.Items {
		cur := -1
		if it.Cursor >= 0 && it.Cursor <= len(it.Insert) {
			cur = utf16Len(it.Insert[:it.Cursor])
		}
		items[i] = completeItem{Item: it, Cursor: cur}
	}
	return map[string]any{
		"from":       utf16Len(buffer[:res.From]),
		"to":         utf16Len(buffer[:res.To]),
		"items":      items,
		"incomplete": res.Incomplete,
		"note":       note,
	}
}

// handleConnComplete is POST /api/v1/conn-complete: handleComplete for an
// editor on no workspace, against the connection the request names — a
// pipeline node's sql field and its node's conn. The first ask on a
// connection waits for its schema (ConnCompletions reads it on the pool);
// a read that fails is the note, and the answer the dialect's vocabulary.
// An unknown connection is the same: a node's conn may be half typed.
func (s *Server) handleConnComplete(ctx rweb.Context) error {
	var req struct {
		Conn string `json:"conn"`
		completeReq
	}
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Buffer) > maxBuffer {
		return ok(ctx, map[string]any{"items": []completeItem{}})
	}
	res, err := s.connCompl.Complete(context.Background(), req.Conn, req.Buffer, byteOffset(req.Buffer, req.Caret))
	note := ""
	if err != nil {
		note = "completion on " + req.Conn + " is without the schema: " + serr.StringFromErr(err)
	}
	return ok(ctx, completeAnswer(req.Buffer, res, note))
}

// handleConnTables is GET /api/v1/conn-tables?conn=: the connection's
// tables and views (ConnCompletions.Tables), for a pipeline node's table
// field to offer. A read that fails — a server that is down, a conn half
// typed — is no tables and the reason, not an error status: the field
// stays a plain line, and nothing went wrong in dbc.
func (s *Server) handleConnTables(ctx rweb.Context) error {
	conn := ctx.Request().QueryParam("conn")
	tables, err := s.connCompl.Tables(context.Background(), conn)
	out := map[string]any{"tables": tables}
	if err != nil {
		out["error"] = serr.StringFromErr(err)
	}
	return ok(ctx, out)
}
