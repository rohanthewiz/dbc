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
	items := make([]completeItem, len(res.Items))
	for i, it := range res.Items {
		cur := -1
		if it.Cursor >= 0 && it.Cursor <= len(it.Insert) {
			cur = utf16Len(it.Insert[:it.Cursor])
		}
		items[i] = completeItem{Item: it, Cursor: cur}
	}
	return ok(ctx, map[string]any{
		"from":       utf16Len(req.Buffer[:res.From]),
		"to":         utf16Len(req.Buffer[:res.To]),
		"items":      items,
		"incomplete": res.Incomplete,
		"note":       note,
	})
}
