package web

import (
	"fmt"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/workspace"
)

// Result tabs: the results bar's strip of the tab's connection's results.
//
// The workspace keeps a result set per connection a query tab has been on
// (workspace/results.go), each a strip of result tabs: a run replaces the
// result in the tab it started on unless that one is pinned, in which case
// it opens a new tab, up to result_tabs of them. The server's part is to
// describe the active connection's set wherever the page may need to redraw
// it, and to carry the strip's clicks back:
//
//	GET  …/ws/:id            ─┐
//	"run" / "result" / "conn" ─┼─► resultTabsState {resultTabs, resultTab, resultTabMax}
//	POST …/ws/:id/result-tab  ─┘
//	       {id, op: show | pin | unpin | close | close-unpinned | share | unshare}
//
//	POST …/ws/:id/rerun {id}  ─► a run of the tab's own statement, back into
//	                            that tab (pinned or not); its outcome is a
//	                            "run" event like any run's
//
// RERUN is its own endpoint, not a result-tab op: the ops change the strip
// and answer with it at once, while a rerun is a RUN — it takes the run
// slot, ticks, can be stopped, and lands later on the stream — so it is
// answered as /run is, with the run's tag.
//
// SHARING: one result tab per connection's set may be shared with the
// assistant (workspace.ShareResultTab) — its result then goes with every
// question on that connection, whatever the caret is on. Only a connection
// with ai_rows = true may share (canShare says so, for the page to offer
// it or say why not); unsharing always works.
//
// The rows are never in it — a strip of ten results would be ten results
// on the wire. Each tab carries its result's seq (the number the grid's
// pages carry), which is what the page keys a tab's grid view on: picking
// a tab and fetching …/result lands on that seq, and the page restores the
// sort, hidden columns and scroll it kept for it.

// resultTabRef is one result tab as the strip draws it.
type resultTabRef struct {
	ID       int    `json:"id"`
	Seq      int    `json:"seq"`
	Title    string `json:"title"`
	Stmt     string `json:"stmt,omitempty"` // the whole statement, for the tooltip
	Pinned   bool   `json:"pinned"`
	Rows     int    `json:"rows"`               // the result's rows (fetched, not shown)
	Exec     bool   `json:"exec"`               // a statement that returns no rows…
	Affected int64  `json:"affected,omitempty"` // …and how many it changed
	Shows    int    `json:"shows,omitempty"`    // a script's s.Show results it holds
	Shared   bool   `json:"shared,omitempty"`   // shared with the assistant
	Writes   bool   `json:"writes,omitempty"`   // rerunning it may change the database: the page asks first
}

// resultTabsState is the active connection's result set: its tabs in strip
// order, the one on screen (its id; 0 when the set is empty), and the cap
// (result_tabs), for the strip's "3 / 10". Embedded, its fields sit flat
// in the events and answers that carry it.
type resultTabsState struct {
	ResultTabs   []resultTabRef `json:"resultTabs"`
	ResultTab    int            `json:"resultTab"`
	ResultTabMax int            `json:"resultTabMax"`
	// CanShare: the connection has ai_rows = true, so a tab may be shared
	// with the assistant. Read live — the connection form can change it.
	CanShare bool `json:"canShare"`
}

// resultTabsOf describes ws's active connection's result set.
func resultTabsOf(ws *workspace.Workspace) resultTabsState {
	tabs, cur := ws.ResultTabs()
	out := resultTabsState{ResultTabs: make([]resultTabRef, len(tabs)), ResultTabMax: ws.ResultTabLimit(),
		CanShare: ws.CanShareResults()}
	for i, t := range tabs {
		ref := resultTabRef{ID: t.ID, Seq: t.Seq, Title: t.Title, Stmt: t.Stmt, Pinned: t.Pinned, Shows: t.Shows,
			Shared: t.Shared, Writes: t.Writes}
		if r := t.Result; r != nil {
			ref.Rows, ref.Exec, ref.Affected = len(r.Rows), r.IsExec, r.Affected
		}
		out.ResultTabs[i] = ref
		if i == cur {
			out.ResultTab = t.ID
		}
	}
	return out
}

// resultTabReq is a click on the strip (or its menu, or a key).
type resultTabReq struct {
	ID int    `json:"id"`
	Op string `json:"op"` // show, pin, unpin, close, close-unpinned, share, unshare
}

// resultTabOut answers it: the set as it now is, and what the page needs
// to redraw the pane under it — whether there is a result to fetch, the
// script switcher of the tab now on screen, whether the connection has a
// plan, and the status bar's summary of the result now on screen ("" with
// none, which leaves the status as it was).
type resultTabOut struct {
	resultTabsState
	HasResult bool        `json:"hasResult"`
	HasPlan   bool        `json:"hasPlan"`
	Sets      *resultSets `json:"sets,omitempty"`
	Status    string      `json:"status"`
}

// handleResultTab is POST /api/v1/ws/:id/result-tab {id, op}. Each op is
// the workspace's (ShowResultTab and the rest), so the TUI's strip and
// this one keep the same rules; a tab gone meanwhile (closed in another
// request, dropped by a run past the cap) is the workspace's refusal, a
// 400 the page reports and then redraws from the set it is sent.
func (s *Server) handleResultTab(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req resultTabReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	switch req.Op {
	case "show":
		err = t.ws.ShowResultTab(req.ID)
	case "pin", "unpin":
		err = t.ws.PinResultTab(req.ID, req.Op == "pin")
	case "close":
		err = t.ws.CloseResultTab(req.ID)
	case "close-unpinned":
		t.ws.CloseUnpinnedResultTabs()
	case "share", "unshare":
		if err = t.ws.ShareResultTab(req.ID, req.Op == "share"); err == nil {
			t.shareNote(req.ID, req.Op == "share")
		}
	default:
		err = badRequest("no result tab op %q (show, pin, unpin, close, close-unpinned, share, unshare)", req.Op)
	}
	if err != nil {
		if r, isRefusal := asRefusal(err); isRefusal {
			t.notes([]workspace.Note{r.Note}) // the why ("set ai_rows = true …") belongs in the log too
		}
		return fail(ctx, err)
	}
	return ok(ctx, s.resultTabOut(t))
}

// rerunReq names the result tab to rerun.
type rerunReq struct {
	ID int `json:"id"`
}

// handleRerun is POST /api/v1/ws/:id/rerun {id}: result tab id's statement
// run again into that tab (workspace.RerunResultTab) — the r key, and the
// strip's "↻ Rerun its query". Refusals are a run's (busy → 409) or the
// tab's (gone, a script's → 400), logged as /run logs its own. Whether a
// write should be asked about is the page's to ask (resultTabRef.Writes);
// by the time this is called the user has said yes.
func (s *Server) handleRerun(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req rerunReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	st, err := t.ws.RerunResultTab(req.ID)
	if err != nil {
		if r, isRefusal := asRefusal(err); isRefusal {
			t.notes([]workspace.Note{r.Note})
		}
		return fail(ctx, err)
	}
	s.launch(t, st)
	return ok(ctx, map[string]any{"tag": st.Tag})
}

// shareNote logs a share or unshare in the connection's log, naming the
// tab as the strip numbers it — sharing changes what every later question
// sends, so it is said where the user will look back for it.
func (t *tab) shareNote(id int, shared bool) {
	tabs, _ := t.ws.ResultTabs()
	n := 0
	for i, r := range tabs {
		if r.ID == id {
			n = i + 1
		}
	}
	conn := t.ws.Active()
	text := fmt.Sprintf("stopped sharing result %d", n)
	if shared {
		text = fmt.Sprintf("shared result %d with the assistant — it goes with every question on %s until you stop sharing", n, conn)
	}
	t.logOn(conn, "ok", text)
}

// resultTabOut is the answer to a strip click, built from the workspace as
// it is now.
func (s *Server) resultTabOut(t *tab) resultTabOut {
	out := resultTabOut{resultTabsState: resultTabsOf(t.ws), Sets: scriptSets(t.ws),
		HasPlan: t.planState().plan != nil}
	if r := t.ws.LastResult(); r != nil {
		out.HasResult = true
		out.Status = workspace.ResultStatus(r, s.cfg.MaxRows, s.shown(r))
	}
	return out
}

// visit records that the tab's workspace may hold results (or a plan) for
// conn.
func (t *tab) visit(conn string) {
	if conn == "" {
		return
	}
	t.viewMu.Lock()
	defer t.viewMu.Unlock()
	if t.visited == nil {
		t.visited = map[string]bool{}
	}
	t.visited[conn] = true
}

// moveResults follows a connection edited in the browser: every query
// tab's results and plans on connection from — and on its other databases,
// "<from>/analytics" — move to to's names, or are dropped when to is ""
// (the connection was removed). Without it, a renamed connection's results
// would sit under a name no tab can be on again, alive for the tab's life
// and never shown; a removed one's likewise.
//
// A tab is never ON from at this point (connedit refuses to rename or
// remove a connection a tab is on), so this moves results kept for a
// connection the tab visited before, which it finds again on going back.
func (s *Server) moveResults(from, to string) {
	s.hub.mu.Lock()
	all := make([]*tab, 0, len(s.hub.tabs))
	for _, t := range s.hub.tabs {
		all = append(all, t)
	}
	s.hub.mu.Unlock()
	for _, t := range all {
		t.viewMu.Lock()
		var names []string
		for c := range t.visited {
			names = append(names, c)
		}
		t.viewMu.Unlock()
		for _, c := range names {
			nc := ""
			switch {
			case c == from:
				nc = to
			case derivedFrom(s.cfg, c, from):
				if to != "" {
					nc = config.DerivedName(to, c[len(from)+len(config.DatabaseSep):])
				}
			default:
				continue
			}
			// the workspace's lock, then viewMu: never one under the other
			if nc == "" {
				t.ws.DropResults(c)
			} else {
				t.ws.RenameResults(c, nc)
			}
			t.renamePlans(c, nc)
			t.viewMu.Lock()
			delete(t.visited, c)
			if nc != "" {
				t.visited[nc] = true
			}
			t.viewMu.Unlock()
		}
	}
}
