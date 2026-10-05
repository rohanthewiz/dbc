package web

import (
	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/sqlcomplete"
)

// The editor's go to definition, find usages and rename: Monaco's providers
// (editor.js) ask here, and sqlcomplete's resolver answers — the alias or
// CTE under the caret, where it is declared, and every place it is used in
// the caret's statement.
//
//	F12 / Shift+F12 ──POST /api/v1/ws/:id/symbol {buffer, caret}──► sqlcomplete.Resolve
//	                ◄── {kind, name, at, def, uses[], fixed}
//
//	F2 ──POST …/symbol first (where the rename box opens, or why not)
//	   ──POST /api/v1/ws/:id/rename {buffer, caret, name}──► workspace.Rename
//	   ◄── {edits[]}, or a 400 whose error says why there is none
//
// Offsets travel in UTF-16 units, as for completion (handleComplete).
// Neither request needs the connection's schema: the resolver reads only
// the statement, so these never wait on a catalog load.

type symbolReq struct {
	Buffer string `json:"buffer"`
	Caret  int    `json:"caret"`
	Name   string `json:"name"` // the new name, for a rename
}

// utf16Counter converts byte offsets into s to UTF-16 units. It walks on
// from the offset it was last asked for when the next is past it: a
// symbol's uses come in buffer order, so a long buffer is read once rather
// than once per use.
type utf16Counter struct {
	s         string
	at, units int
}

func (c *utf16Counter) of(off int) int {
	if off < c.at {
		c.at, c.units = 0, 0
	}
	c.units += utf16Len(c.s[c.at:off])
	c.at = off
	return c.units
}

func (c *utf16Counter) span(s sqlcomplete.Span) sqlcomplete.Span {
	return sqlcomplete.Span{From: c.of(s.From), To: c.of(s.To)}
}

// handleSymbol answers what the name under the caret is. A caret on
// nothing the resolver knows is a symbol with no kind, not an error: the
// editor just has nothing to jump to.
func (s *Server) handleSymbol(ctx rweb.Context) error {
	if _, err := s.hub.get(ctx.Request().PathParam("id")); err != nil {
		return fail(ctx, err)
	}
	var req symbolReq
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Buffer) > maxBuffer {
		return ok(ctx, sqlcomplete.Symbol{Uses: []sqlcomplete.Span{}})
	}
	sym := sqlcomplete.Resolve(req.Buffer, byteOffset(req.Buffer, req.Caret))
	c := &utf16Counter{s: req.Buffer}
	uses := make([]sqlcomplete.Span, len(sym.Uses))
	for i, u := range sym.Uses {
		uses[i] = c.span(u)
	}
	sym.At, sym.Def, sym.Uses = c.span(sym.At), c.span(sym.Def), uses
	return ok(ctx, sym)
}

// handleRename answers the edits that rename the symbol under the caret.
// A refusal — nothing renamable there, a table, a name already taken — is
// a 400 with the resolver's sentence, which Monaco shows by the rename box.
func (s *Server) handleRename(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req symbolReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Buffer) > maxBuffer {
		return fail(ctx, badRequest("the editor's text is too long to rename in"))
	}
	edits, err := t.ws.Rename(req.Buffer, byteOffset(req.Buffer, req.Caret), req.Name)
	if err != nil {
		return fail(ctx, badRequest("%s", err.Error()))
	}
	c := &utf16Counter{s: req.Buffer}
	for i, e := range edits {
		edits[i].From, edits[i].To = c.of(e.From), c.of(e.To)
	}
	return ok(ctx, map[string]any{"edits": edits})
}
