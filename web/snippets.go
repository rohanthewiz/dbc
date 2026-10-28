package web

import (
	"errors"
	"strings"

	"github.com/rohanthewiz/rweb"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/snippet"
	"github.com/rohanthewiz/dbc/userdata"
)

// Live snippets in dbc web: the store is the TUI's (userdata.Snippets, the
// same snippets.bytdb), and the expansion is package snippet's, so a
// snippet inserts the same text in both. The page does the rest with
// Monaco's own snippet engine (editor.js): it asks for the expansion in
// Monaco's syntax and inserts it, and Monaco steps the stops and mirrors
// them.
//
//	GET    /api/v1/snippets               the list, with what the page needs to
//	                                      decide Tab (names) and draw the picker
//	POST   /api/v1/snippets               a new one              {name, desc, body}
//	PUT    /api/v1/snippets/:name         save over :name (rename) {name, desc, body}
//	DELETE /api/v1/snippets/:name
//	POST   /api/v1/snippets/:name/expand  {selection, selPrefix, indent} → {snippet, text}
//
// Each change is broadcast as "snippets", so every window's list — and
// with it which words its Tab expands — follows it.

// snippetRow is one snippet as the page lists it.
type snippetRow struct {
	userdata.Snippet
	// Selection marks a $SELECTION$ snippet (the picker's ⇲)
	Selection bool `json:"selection"`
	// Err is why the body does not parse ("" when it does): a snippet saved
	// by hand, or by a dbc with other rules, is listed so it can be fixed,
	// but cannot be inserted
	Err string `json:"err,omitempty"`
}

// snippetSave is the body of a POST or PUT.
type snippetSave struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
	Body string `json:"body"`
}

// snippetExpandReq says where the snippet goes; see snippet.Context.
type snippetExpandReq struct {
	Selection string `json:"selection"`
	SelPrefix string `json:"selPrefix"`
	Indent    string `json:"indent"`
}

type snippetExpandResp struct {
	Snippet string `json:"snippet"` // Monaco's snippet syntax (Expansion.Monaco)
	Text    string `json:"text"`    // the plain text, for the page without Monaco
	Stops   int    `json:"stops"`
}

// maxSnippetBody bounds a body: a snippet is typed into an editor, and the
// store keeps the whole list in memory.
const maxSnippetBody = 64 << 10

// handleSnippets is GET /api/v1/snippets.
func (s *Server) handleSnippets(ctx rweb.Context) error {
	list, err := s.snips.List()
	if err != nil {
		return fail(ctx, s.snippetErr(err, ""))
	}
	rows := make([]snippetRow, 0, len(list))
	for _, sn := range list {
		r := snippetRow{Snippet: sn}
		if tpl, err := snippet.Parse(sn.Body); err != nil {
			r.Err = err.Error()
		} else {
			r.Selection = tpl.UsesSelection()
		}
		rows = append(rows, r)
	}
	return ok(ctx, map[string]any{"snippets": rows})
}

// handleSnippetNew is POST /api/v1/snippets.
func (s *Server) handleSnippetNew(ctx rweb.Context) error { return s.saveSnippet(ctx, "") }

// handleSnippetSave is PUT /api/v1/snippets/:name.
func (s *Server) handleSnippetSave(ctx rweb.Context) error {
	return s.saveSnippet(ctx, ctx.Request().PathParam("name"))
}

// saveSnippet checks the request before the store does, so a bad name or a
// template that does not parse is a 400 with the reason — the form shows
// it under its fields — rather than the store's error as a 500.
func (s *Server) saveSnippet(ctx rweb.Context, old string) error {
	var req snippetSave
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	req.Name = strings.TrimSpace(req.Name)
	if len(req.Body) > maxSnippetBody {
		return fail(ctx, badRequest("the body is %d KB; a snippet holds up to %d KB", len(req.Body)>>10, maxSnippetBody>>10))
	}
	if err := snippet.ValidName(req.Name); err != nil {
		return fail(ctx, badRequest("%v", err))
	}
	if strings.TrimSpace(req.Body) == "" {
		return fail(ctx, badRequest("the snippet's body is empty"))
	}
	if _, err := snippet.Parse(req.Body); err != nil {
		return fail(ctx, badRequest("%v", err))
	}
	if err := s.snips.Save(old, userdata.Snippet{Name: req.Name, Desc: req.Desc, Body: req.Body}); err != nil {
		return fail(ctx, s.snippetErr(err, req.Name))
	}
	s.hub.broadcast("snippets", map[string]string{"op": "saved", "name": req.Name, "old": old})
	return ok(ctx, map[string]string{"name": req.Name})
}

// handleSnippetDelete is DELETE /api/v1/snippets/:name.
func (s *Server) handleSnippetDelete(ctx rweb.Context) error {
	name := ctx.Request().PathParam("name")
	if err := s.snips.Delete(name); err != nil {
		return fail(ctx, s.snippetErr(err, name))
	}
	s.hub.broadcast("snippets", map[string]string{"op": "deleted", "name": name})
	return ok(ctx, nil)
}

// handleSnippetExpand is POST /api/v1/snippets/:name/expand.
func (s *Server) handleSnippetExpand(ctx rweb.Context) error {
	name := ctx.Request().PathParam("name")
	var req snippetExpandReq
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Selection) > maxBuffer {
		return fail(ctx, badRequest("the selection is too large to wrap in a snippet"))
	}
	sn, found, err := s.snips.Get(name)
	if err != nil {
		return fail(ctx, s.snippetErr(err, name))
	}
	if !found {
		return fail(ctx, notFound("no snippet %s", name))
	}
	tpl, err := snippet.Parse(sn.Body)
	if err != nil {
		return fail(ctx, badRequest("snippet %s does not parse: %v — fix it in the snippets dialog (Alt+S)", name, err))
	}
	x := snippet.Expand(tpl, snippet.Context{Selection: req.Selection, SelPrefix: req.SelPrefix, Indent: req.Indent})
	return ok(ctx, snippetExpandResp{Snippet: x.Monaco(), Text: x.Text, Stops: len(x.Stops)})
}

// snippetErr maps the store's errors onto statuses: a missing snippet is a
// 404, a name taken or a file the TUI holds a 409, anything else fail's 500.
func (s *Server) snippetErr(err error, name string) error {
	switch {
	case errors.Is(err, userdata.ErrNoSnippet):
		return notFound("no snippet %s — deleted in another window?", name)
	case errors.Is(err, userdata.ErrSnippetExists):
		return conflict("a snippet called %s already exists", name)
	case errors.Is(err, userdata.ErrSnippetsBusy):
		return conflict("%v", userdata.ErrSnippetsBusy)
	}
	return serr.Wrap(err, "op", "snippets")
}
