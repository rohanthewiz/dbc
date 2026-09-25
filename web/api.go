package web

import (
	"time"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/workspace"
)

// The JSON API. Every handler returns through ok or fail (respond.go), so
// the envelope and the status mapping live in one place. Handlers that start
// work answer as soon as the workspace has accepted or refused it; what the
// work does next arrives on the tab's SSE stream.

// ---------------------------------------------------------------------------
// Server-wide
// ---------------------------------------------------------------------------

// handleHealth is public (see publicPath), so it says only that dbc web is
// up and where it keeps its state — nothing about connections or tabs'
// contents.
func (s *Server) handleHealth(ctx rweb.Context) error {
	return ok(ctx, map[string]any{
		"status":     "ok",
		"state_file": s.store.Path(),
		"persistent": s.store.Persistent(),
		"workspaces": s.hub.count(),
	})
}

// connInfo is what the browser learns about a connection: never its DSN.
// Saved marks one added in the browser (conns.go), the only kind it may
// edit or remove. AIRows is there for the edit form's checkbox; it is no
// secret, the file's own setting being in plain sight in the file.
type connInfo struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
	Saved  bool   `json:"saved,omitempty"`
	AIRows bool   `json:"ai_rows,omitempty"`
}

func (s *Server) handleConns(ctx rweb.Context) error {
	return ok(ctx, s.connList())
}

// ---------------------------------------------------------------------------
// Tabs and layout (the store)
// ---------------------------------------------------------------------------

func (s *Server) handleTabs(ctx rweb.Context) error {
	tabs, err := s.store.Tabs()
	if err != nil {
		return fail(ctx, err)
	}
	if tabs == nil {
		tabs = []Tab{}
	}
	return ok(ctx, tabs)
}

// maxBuffer caps a saved editor buffer. A buffer is typed or pasted SQL;
// anything this size is a mistake (a pasted dump), and it would be written
// on every keystroke pause.
const maxBuffer = 4 << 20

// handleSaveTab is PUT /api/v1/tabs/:id?win=<window>. The window is the
// claim check (claims.go): a tab another live window holds is refused with
// a 409, so two browser tabs cannot overwrite each other's text.
func (s *Server) handleSaveTab(ctx rweb.Context) error {
	var t Tab
	if err := decode(ctx, &t); err != nil {
		return fail(ctx, err)
	}
	t.ID = ctx.Request().PathParam("id")
	if err := s.saveTab(ctx.Request().QueryParam("win"), t); err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, nil)
}

// saveTab checks, claims for window winID and writes one saved tab — a PUT,
// or the last save riding on a window's release.
func (s *Server) saveTab(winID string, t Tab) error {
	if t.ID == "" || len(t.ID) > 64 {
		return badRequest("a tab id is 1 to 64 characters")
	}
	if len(t.Buffer) > maxBuffer {
		return badRequest("the editor holds %d MB; tabs save up to %d MB", len(t.Buffer)>>20, maxBuffer>>20)
	}
	if err := s.hub.claimOne(winID, t.ID); err != nil {
		return err
	}
	t.Updated = time.Now().UTC()
	return s.store.SaveTab(t)
}

// handleDeleteTab forgets a saved query tab — the page closed it. With
// ?win=, a tab another live window holds is refused: the page closing is
// only closing its stale copy, and the other window's tab stays saved.
func (s *Server) handleDeleteTab(ctx rweb.Context) error {
	id := ctx.Request().PathParam("id")
	if err := s.hub.claimOne(ctx.Request().QueryParam("win"), id); err != nil {
		return fail(ctx, err)
	}
	if err := s.store.DeleteTab(id); err != nil {
		return fail(ctx, err)
	}
	s.hub.unclaim(id)
	return ok(ctx, nil)
}

func (s *Server) handleLayout(ctx rweb.Context) error {
	l, err := s.store.Layout()
	if err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, l)
}

func (s *Server) handleSaveLayout(ctx rweb.Context) error {
	var l map[string]string
	if err := decode(ctx, &l); err != nil {
		return fail(ctx, err)
	}
	if len(l) > 64 {
		return fail(ctx, badRequest("too many layout keys"))
	}
	// "tabs" (the strip's order) and "plans" list saved-tab keys, and a
	// window lists only its own: keep the ones other windows hold (see
	// mergeTabKeys). Only when another window holds any — the usual single
	// window writes its values as they are, with no read first.
	_, hasTabs := l["tabs"]
	_, hasPlans := l["plans"]
	if hasTabs || hasPlans {
		if elsewhere := s.hub.heldElsewhere(ctx.Request().QueryParam("win")); len(elsewhere) > 0 {
			old, err := s.store.Layout()
			if err != nil {
				return fail(ctx, err)
			}
			for _, k := range []string{"tabs", "plans"} {
				if v, given := l[k]; given {
					l[k] = mergeTabKeys(v, old[k], elsewhere)
				}
			}
		}
	}
	if err := s.store.SetLayout(l); err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, nil)
}

// ---------------------------------------------------------------------------
// Workspaces
// ---------------------------------------------------------------------------

// wsState is a workspace as the page needs it to draw itself — after an
// open, and after a reload reattaches to a workspace that already exists.
type wsState struct {
	ID         string   `json:"id"`
	Win        string   `json:"win"` // the window (browser tab) it belongs to
	Active     string   `json:"active"`
	Connected  bool     `json:"connected"` // its catalog has loaded
	Connecting string   `json:"connecting,omitempty"`
	Busy       bool     `json:"busy"`
	Status     string   `json:"status,omitempty"` // the run in flight
	HasResult  bool     `json:"hasResult"`
	HasPlan    bool     `json:"hasPlan"`  // the Plan tab has something to show
	Stateful   bool     `json:"stateful"` // the "session state" badge; see runEvent
	Tables     []tabRef `json:"tables"`
	Warnings   []string `json:"warnings,omitempty"`
}

func (s *Server) state(t *tab) wsState {
	st := wsState{
		ID: t.id, Win: t.win.id, Active: t.ws.Active(), Connected: t.ws.Catalog() != nil,
		Busy: t.ws.Busy(), Status: t.ws.RunningStatus(),
		HasResult: t.ws.LastResult() != nil, HasPlan: t.planState().plan != nil, Tables: tables(t.ws),
	}
	if name, ok := t.ws.Connecting(); ok {
		st.Connecting = name
	}
	// Session waits on the session lock, which a statement in flight holds
	// for as long as it runs; mid-run the badge is left as the page has it,
	// and the run's own event brings it up to date
	if !st.Busy {
		_, st.Stateful = t.ws.Session()
	}
	return st
}

type openReq struct {
	Win string `json:"win"` // the window to open a query tab in; "" opens a new window
}

// handleOpen makes a workspace: a query tab in the window the body names,
// or the first query tab of a new window (a new browser tab). A new
// window's response carries the config's load warnings (a demo that could
// not be opened, say), for the page's log — the terminal printed them too,
// but the user is looking here.
func (s *Server) handleOpen(ctx rweb.Context) error {
	var req openReq
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	t, err := s.hub.open(req.Win)
	if err != nil {
		return fail(ctx, err)
	}
	st := s.state(t)
	if req.Win == "" {
		st.Warnings = s.cfg.Warnings
	}
	return ok(ctx, st)
}

// handleClose closes a query tab: its run stopped and its session released
// — an open transaction rolled back, as switching connections does. The
// page asks first when the tab's session may hold state.
func (s *Server) handleClose(ctx rweb.Context) error {
	t, err := s.hub.close(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, map[string]any{"closed": t.id})
}

// winState is a window as a reloading page needs it: which query tabs it
// still holds, so each can reattach to its workspace.
type winState struct {
	ID   string   `json:"id"`
	Tabs []string `json:"tabs"`
}

func (s *Server) handleWindow(ctx rweb.Context) error {
	w, err := s.hub.window(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	st := winState{ID: w.id, Tabs: []string{}}
	for _, t := range s.hub.tabsOf(w) {
		st.Tabs = append(st.Tabs, t.id)
	}
	return ok(ctx, st)
}

// handleWindowEvents attaches the page's EventSource to its window's
// stream. The SSE hub registers the client and unregisters it when the
// connection drops, which is what the idle reaper counts.
func (s *Server) handleWindowEvents(ctx rweb.Context) error {
	w, err := s.hub.window(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	ctx.Response().SetHeader("Cache-Control", "no-store")
	return w.sse.Handler(s.rw)(ctx)
}

func (s *Server) handleState(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, s.state(t))
}

// handleEvents attaches an EventSource to a query tab's WINDOW stream —
// the same stream handleWindowEvents serves, reached by a query tab's id
// for a client that has only that (a script, the tests). Its events are
// every query tab's in the window, each tagged with its ws.
func (s *Server) handleEvents(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	ctx.Response().SetHeader("Cache-Control", "no-store")
	return t.win.sse.Handler(s.rw)(ctx)
}

// shown is how many of a result's rows the page displays.
func (s *Server) shown(r *model.Result) int {
	if n := s.cfg.MaxDisplayRows; n > 0 && n < len(r.Rows) {
		return n
	}
	return len(r.Rows)
}

type connectReq struct {
	Name string `json:"name"`
}

// handleConnect switches the tab's connection. It answers at once; the
// connect's outcome arrives on the stream as "conn".
func (s *Server) handleConnect(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req connectReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if _, known := s.cfg.ConnByName(req.Name); !known {
		return fail(ctx, badRequest("unknown connection %q", req.Name))
	}
	st := t.ws.Switch(req.Name)
	if st.Job == nil {
		// already on it, catalog loaded: nothing to do, but the page may
		// be reattaching and want its sidebar — send the state it has
		t.send("conn", connEvent{Active: t.ws.Active(), Tables: tables(t.ws)})
		return ok(ctx, map[string]any{"connecting": false})
	}
	t.send("connecting", map[string]string{"name": req.Name})
	s.launch(t, st)
	return ok(ctx, map[string]any{"connecting": true})
}

// runReq is the editor at the moment of Run: the buffer, the caret and the
// selection. The workspace picks the statement from them, so "the statement
// under the caret" means the same thing here as in the TUI.
//
// Caret is in UTF-16 code units — JavaScript's string offsets — and is
// converted to the byte offset the workspace expects (see byteOffset).
type runReq struct {
	Buffer    string `json:"buffer"`
	Caret     int    `json:"caret"`
	Selection string `json:"selection"`
	All       bool   `json:"all"`
}

// handleRun is Ctrl+Enter / Ctrl+Shift+Enter. Busy → 409 with the TUI's
// words ("busy — statement 2/4 is still running (Ctrl+K stops it)"); no
// connection or nothing to run → 400. Accepted → 200 with the run's tag;
// the notes, ticks and outcome go to the stream.
func (s *Server) handleRun(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req runReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	ed := workspace.Editor{Text: req.Buffer, Caret: byteOffset(req.Buffer, req.Caret), Selection: req.Selection}
	st, err := t.ws.RunEditor(ed, req.All)
	if err != nil {
		t.notes(st.Notes) // a refused run is still recorded in history, with its notes
		if r, isRefusal := asRefusal(err); isRefusal {
			t.notes([]workspace.Note{r.Note})
		}
		return fail(ctx, err)
	}
	s.launch(t, st)
	return ok(ctx, map[string]any{"tag": st.Tag})
}

// handleCancel is Ctrl+K: the run in flight, else the connect in flight.
func (s *Server) handleCancel(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	n, status := t.ws.Cancel()
	t.notes([]workspace.Note{n})
	return ok(ctx, map[string]any{"note": n.Text, "status": status})
}

// byteOffset turns a UTF-16 offset into s (a JavaScript string index) into
// a byte offset. SQL is mostly ASCII, where the two agree; a single é or
// emoji before the caret would otherwise pick the wrong statement.
func byteOffset(s string, utf16 int) int {
	if utf16 <= 0 {
		return 0
	}
	units := 0
	for i, r := range s {
		if units >= utf16 {
			return i
		}
		units++
		if r >= 0x10000 { // outside the BMP: a surrogate pair in UTF-16
			units++
		}
	}
	return len(s)
}

func asRefusal(err error) (*workspace.Refusal, bool) {
	r, ok := err.(*workspace.Refusal)
	return r, ok
}
