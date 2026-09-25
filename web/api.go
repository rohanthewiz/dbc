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
type connInfo struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
}

func (s *Server) handleConns(ctx rweb.Context) error {
	out := make([]connInfo, len(s.cfg.Connections))
	for i, c := range s.cfg.Connections {
		out[i] = connInfo{Name: c.Name, Driver: c.Driver}
	}
	return ok(ctx, map[string]any{"conns": out, "default": s.defaultConn()})
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

func (s *Server) handleSaveTab(ctx rweb.Context) error {
	var t Tab
	if err := decode(ctx, &t); err != nil {
		return fail(ctx, err)
	}
	t.ID = ctx.Request().PathParam("id")
	if t.ID == "" || len(t.ID) > 64 {
		return fail(ctx, badRequest("a tab id is 1 to 64 characters"))
	}
	if len(t.Buffer) > maxBuffer {
		return fail(ctx, badRequest("the editor holds %d MB; tabs save up to %d MB", len(t.Buffer)>>20, maxBuffer>>20))
	}
	t.Updated = time.Now().UTC()
	if err := s.store.SaveTab(t); err != nil {
		return fail(ctx, err)
	}
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
		ID: t.id, Active: t.ws.Active(), Connected: t.ws.Catalog() != nil,
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

// handleOpen makes a workspace for a new browser tab. The config's load
// warnings (a demo that could not be opened, say) ride along, for the
// page's log — the terminal printed them too, but the user is looking here.
func (s *Server) handleOpen(ctx rweb.Context) error {
	t := s.hub.open()
	st := s.state(t)
	st.Warnings = s.cfg.Warnings
	return ok(ctx, st)
}

func (s *Server) handleState(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, s.state(t))
}

// handleEvents attaches the page's EventSource to the tab's stream. The
// SSE hub registers the client and unregisters it when the connection
// drops, which is what the idle reaper counts.
func (s *Server) handleEvents(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	ctx.Response().SetHeader("Cache-Control", "no-store")
	return t.sse.Handler(s.rw)(ctx)
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
