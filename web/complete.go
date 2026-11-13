package web

import (
	"context"
	"time"

	"github.com/rohanthewiz/rweb"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/sqlcomplete"
	"github.com/rohanthewiz/dbc/workspace"
)

// The editor's completions: Monaco's suggest widget asks here as the user
// types (editor.js registers the provider), and the answer is the same
// sqlcomplete runs for the TUI's popup — one set of rules for both UIs.
//
//	Monaco ──POST /api/v1/ws/:id/complete {buffer, caret}──► handleComplete
//	                                                     │
//	           cache cold? start LoadCompletions, wait   │
//	           up to completeWait for it, then answer    ▼
//	       ◄── {from, to, items[], incomplete, note} ── workspace.Complete
//
// A COLD CACHE IS WAITED FOR BRIEFLY, NOT WHOLLY (N-275). A schema read can
// take a long time on a slow catalog: up to CompletionTimeout for each of
// its reads. Every request waiting on it holds one of the six connections
// the browser opens to this server, and the event streams already hold some
// of those. A few completions that each waited out the read would leave
// Run, Ctrl+K and saves queued behind them for a minute or more. So a
// request waits at most completeWait, which is long enough for a quick
// catalog to answer in full. After that it answers with the dialect's
// vocabulary, marked incomplete. The read keeps running on a goroutine of
// its own and fills the cache, and Monaco asks again on the next keystroke
// because of the incomplete mark. The page also aborts a request Monaco no
// longer wants (editor.js), which frees its connection at once.
//
//	t=0     ask ──► cache cold ──► start the read ──┐
//	t≤0.5s  ◄── vocabulary, incomplete              │ (still reading)
//	t=4s    keystroke: ask ──► still reading ───────┤ ◄── vocabulary again
//	t=9s                                     read lands in the cache
//	t=10s   keystroke: ask ──► cached ──► the schema's tables and columns
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

// completeWait is how long a completion request waits for a schema read it
// found missing before answering without it (see A COLD CACHE above). It is
// a variable so that tests can shorten it.
var completeWait = 500 * time.Millisecond

// The schema reads a completion request starts. They are variables so that
// tests can swap in a read that is slow on purpose.
var (
	loadCompletions = func(ws *workspace.Workspace) error {
		return ws.LoadCompletions(context.Background())
	}
	loadConnCompletions = func(c *workspace.ConnCompletions, conn string) error {
		return c.Load(context.Background(), conn)
	}
)

// awaitLoad runs load on a goroutine of its own and waits up to
// completeWait for it. landed is false when the load is still running.
// The load is not cancelled then: it finishes on its own, fills the cache,
// and a later request is answered from that.
//
// A request that arrives while a read is already in flight also starts a
// goroutine here. Both LoadCompletions and ConnCompletions.Load join the
// read in flight instead of starting a second one, so that goroutine only
// waits, and it ends when the read does.
func awaitLoad(t *tab, load func() error) (landed bool, err error) {
	done := make(chan error, 1) // buffered: no one may be listening by the time it lands
	goSafe(t, "a completion's schema read", func() { done <- load() })
	timer := time.NewTimer(completeWait)
	defer timer.Stop()
	select {
	case err = <-done:
		return true, err
	case <-timer.C:
		return false, nil
	}
}

// handleComplete answers a completion request. A cold cache is waited for
// up to completeWait; past that the answer is the dialect's vocabulary,
// marked incomplete, and the read goes on. A read that failed, whether
// during this request or since, is reported in note on every answer while
// the failure is remembered, and the page logs it once.
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
	if !ready {
		if landed, _ := awaitLoad(t, func() error { return loadCompletions(t.ws) }); landed {
			// from the cache now; if a drop raced the read, this is the
			// incomplete vocabulary again and the next keystroke asks anew
			res, _ = t.ws.Complete(req.Buffer, caret)
		}
	}
	note := ""
	if lerr := t.ws.CompletionErr(); lerr != nil {
		note = "completion is without the schema: " + serr.StringFromErr(lerr)
	}
	// a query tab's list has the user's snippets too (N-228), by the
	// TUI's rule; a pipeline field (handleConnComplete) is not where a
	// snippet goes, so that list has none
	res = sqlcomplete.AddSnippets(res, req.Buffer, s.complSnippets())
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
// pipeline node's sql field and its node's conn. A missing schema is waited
// for as handleComplete waits (completeWait, then the vocabulary marked
// incomplete while ConnCompletions reads it on the pool). A read that
// failed is the note, and the answer the dialect's vocabulary. An unknown
// connection is the same: a node's conn may be half typed.
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
	caret := byteOffset(req.Buffer, req.Caret)
	res, ready, err := s.connCompl.Ask(req.Conn, req.Buffer, caret)
	if !ready {
		if landed, _ := awaitLoad(nil, func() error { return loadConnCompletions(s.connCompl, req.Conn) }); landed {
			res, _, err = s.connCompl.Ask(req.Conn, req.Buffer, caret)
		}
	}
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
