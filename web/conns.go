package web

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rohanthewiz/rweb"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// Connections added in the browser.
//
// The config file stays the file's: dbc web never writes it back (the TOML
// encoder would lose the user's comments and layout). A connection added
// here is kept in the store (web.bytdb, see SavedConn) and merged into the
// running config — at startup, and at once when it is added — so the
// workspaces, the manager and every handler find it through ConnByName like
// any other. The TUI and headless runs read only the file, so they do not
// see these.
//
//	                 ┌── config file ([[connection]]) ──┐
//	startup:  cfg ◄──┤                                  ├── file's names win a clash
//	                 └── store (conns table) ───────────┘
//
//	POST   /api/v1/conns/test ─► db.Probe: open, ping, close; nothing saved
//	POST   /api/v1/conns ──────► cfg.AddConn, store.SaveConn, "conns" to every window
//	DELETE /api/v1/conns/:name ► refused while a query tab is on it; else
//	                             cfg.RemoveConn, mgr.Drop, store.DeleteConn, "conns"
//
// A DSN never goes back to the browser: the list says name, driver and
// whether it was added here — enough to draw the sidebar and offer Remove.

// maxConnName bounds a connection name. It is shown in the sidebar, the
// topbar and every history row: a name that long is a paste gone wrong.
const maxConnName = 64

// mergeSavedConns adds the store's connections to the config. One whose name
// the config file has since taken is skipped — the file is the user's
// deliberate word, the store only what was once typed in a form — and so is
// one whose driver this build does not know. Both stay in the store, and the
// returned warnings say why they are missing.
func (s *Server) mergeSavedConns() []string {
	saved, err := s.store.Conns()
	if err != nil {
		return []string{fmt.Sprintf("connections added in the browser could not be read: %v", err)}
	}
	var warns []string
	for _, sc := range saved {
		if _, err = db.Driver(sc.Driver); err != nil {
			warns = append(warns, fmt.Sprintf("saved connection %q skipped: unknown driver %q", sc.Name, sc.Driver))
			continue
		}
		dsn, w := config.ExpandDSN(sc.Name, sc.DSN)
		warns = append(warns, w...)
		if err = s.cfg.AddConn(config.Connection{Name: sc.Name, Driver: sc.Driver, DSN: dsn,
			AIRows: sc.AIRows, Web: true}); err != nil {
			warns = append(warns, fmt.Sprintf(
				"saved connection %q skipped: the config file now defines a connection by that name", sc.Name))
		}
	}
	return warns
}

// connList is the sidebar's view of every connection, for GET /api/v1/conns
// and the "conns" event.
func (s *Server) connList() map[string]any {
	conns := s.cfg.Conns()
	out := make([]connInfo, len(conns))
	for i, c := range conns {
		out[i] = connInfo{Name: c.Name, Driver: c.Driver, Saved: c.Web}
	}
	return map[string]any{"conns": out, "default": s.defaultConn()}
}

// connForm is the add-connection form, for a test or a save.
type connForm struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
	DSN    string `json:"dsn"`
	AIRows bool   `json:"ai_rows"`
}

// check trims the form and turns away what could never work. needName: a
// test may be run before a name is typed; a save may not.
func (f *connForm) check(needName bool) error {
	f.Name, f.Driver, f.DSN = strings.TrimSpace(f.Name), strings.TrimSpace(f.Driver), strings.TrimSpace(f.DSN)
	if needName || f.Name != "" {
		switch {
		case f.Name == "":
			return badRequest("name the connection")
		case utf8.RuneCountInString(f.Name) > maxConnName:
			return badRequest("a connection name is at most %d characters", maxConnName)
		case strings.ContainsFunc(f.Name, unicode.IsControl):
			return badRequest("a connection name cannot hold control characters")
		}
	}
	if _, err := db.Driver(f.Driver); err != nil {
		return badRequest("pick a driver: postgres, mysql, sqlite or bytdb")
	}
	if f.DSN == "" {
		return badRequest("the DSN is empty")
	}
	return nil
}

// testTimeoutCap bounds a test when connect_timeout is 0 ("no limit"): a
// real connect may then wait on the OS, but a button press should answer
// while the user is still looking at it.
const testTimeoutCap = 30 * time.Second

// handleConnTest is POST /api/v1/conns/test: does this DSN connect? A form
// that cannot work is a 400; a connect that fails is a 200 with ok: false
// and the driver's words, because the test itself worked — it is the answer.
func (s *Server) handleConnTest(ctx rweb.Context) error {
	var f connForm
	if err := decode(ctx, &f); err != nil {
		return fail(ctx, err)
	}
	if err := f.check(false); err != nil {
		return fail(ctx, err)
	}
	name := f.Name
	if name == "" {
		name = "(new connection)"
	}
	dsn, warns := config.ExpandDSN(name, f.DSN)
	timeout := s.cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = testTimeoutCap
	}
	// Bounded by the timeout alone: rweb gives a handler no context that
	// ends when the client goes away, so a closed dialog leaves the dial to
	// finish (or time out) on its own; its answer is then just not read.
	res, err := db.Probe(context.Background(), config.Connection{Name: name, Driver: f.Driver, DSN: dsn}, timeout)
	out := map[string]any{"ok": res.OK, "warnings": warns}
	if err != nil {
		out["error"] = userMsg(err)
		return ok(ctx, out)
	}
	out["ms"] = res.Took.Milliseconds()
	if res.Note != "" {
		out["note"] = res.Note
	}
	return ok(ctx, out)
}

// handleConnAdd is POST /api/v1/conns: add a connection and keep it.
//
// The config is changed first, the store second: AddConn is where a
// duplicate name is caught atomically (two windows saving the same name
// race there, not in the store), and a store that then fails to write is
// undone by RemoveConn, so the two never disagree about what exists. It
// does not connect — the page switches its tab to it right after, where
// the connect's outcome shows the way every connect's does.
func (s *Server) handleConnAdd(ctx rweb.Context) error {
	var f connForm
	if err := decode(ctx, &f); err != nil {
		return fail(ctx, err)
	}
	if err := f.check(true); err != nil {
		return fail(ctx, err)
	}
	dsn, warns := config.ExpandDSN(f.Name, f.DSN)
	cn := config.Connection{Name: f.Name, Driver: f.Driver, DSN: dsn, AIRows: f.AIRows, Web: true}
	if err := s.cfg.AddConn(cn); err != nil {
		if errors.Is(err, config.ErrConnExists) {
			return fail(ctx, conflict("a connection named %q already exists", f.Name))
		}
		return fail(ctx, err)
	}
	// the DSN as typed, ${VAR}s and all: see SavedConn
	if err := s.store.SaveConn(SavedConn{Name: f.Name, Driver: f.Driver, DSN: f.DSN, AIRows: f.AIRows}); err != nil {
		s.cfg.RemoveConn(f.Name)
		return fail(ctx, serr.Wrap(err, "op", "add conn"))
	}
	if !s.store.Persistent() {
		warns = append(warns, "the store is not being saved this session (another dbc web holds it), "+
			"so this connection lasts only until dbc web stops")
	}
	list := s.connList()
	s.hub.broadcast("conns", list)
	list["warnings"] = warns
	return ok(ctx, list)
}

// handleConnDelete is DELETE /api/v1/conns/:name: forget a connection added
// in the browser. The config file's connections are refused — they are the
// file's to change. So is one a query tab (in any window) is on or
// connecting to: removing it would pull the pool out from under that tab's
// session and whatever transaction it holds. The user switches those tabs
// first; the refusal says how many there are.
//
// The check and the removal are not one atomic step: a tab that picks the
// connection in between gets a connect that fails ("unknown connection"),
// or a pool Drop then closes. Either shows as an error on that tab and
// breaks nothing else, which for a race a person would have to win by
// clicking in two windows within microseconds is enough.
func (s *Server) handleConnDelete(ctx rweb.Context) error {
	name, err := url.PathUnescape(ctx.Request().PathParam("name"))
	if err != nil {
		return fail(ctx, badRequest("bad connection name in the path"))
	}
	cn, known := s.cfg.ConnByName(name)
	if !known {
		return fail(ctx, notFound("no connection named %q", name))
	}
	if !cn.Web {
		where := "the config file"
		if s.cfg.Path != "" {
			where = s.cfg.Path
		}
		if s.cfg.Demo {
			return fail(ctx, badRequest("%q is a built-in demo connection", name))
		}
		return fail(ctx, badRequest("%q is defined in %s — edit the file to remove it", name, where))
	}
	if n := s.hub.tabsOn(name); n > 0 {
		return fail(ctx, conflict("%s on %q — switch %s to another connection first",
			tabsAre(n), name, itThem(n)))
	}
	s.cfg.RemoveConn(name)
	s.mgr.Drop(name)
	if err = s.store.DeleteConn(name); err != nil {
		// gone for this run, back at the next start: say so rather than
		// pretend, and leave it removed now — putting it back would only
		// hand the page a connection it just asked to be rid of
		list := s.connList()
		s.hub.broadcast("conns", list)
		return fail(ctx, serr.Wrap(err, "op", "delete conn"))
	}
	list := s.connList()
	s.hub.broadcast("conns", list)
	return ok(ctx, list)
}

// tabsAre is "1 query tab is" / "3 query tabs are".
func tabsAre(n int) string {
	if n == 1 {
		return "1 query tab is"
	}
	return fmt.Sprintf("%d query tabs are", n)
}

// itThem is the pronoun for n query tabs.
func itThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// tabsOn counts the query tabs, in every window, on the named connection or
// connecting to it. The tabs are gathered under hub.mu and asked outside
// it, so hub.mu is never held while taking a workspace's lock.
func (h *hub) tabsOn(name string) int {
	h.mu.Lock()
	all := make([]*tab, 0, len(h.tabs))
	for _, t := range h.tabs {
		all = append(all, t)
	}
	h.mu.Unlock()
	n := 0
	for _, t := range all {
		connecting, busy := t.ws.Connecting()
		if t.ws.Active() == name || (busy && connecting == name) {
			n++
		}
	}
	return n
}

// broadcast sends a window-level event (no query tab) to every window — a
// change every open page must draw, such as the connection list.
func (h *hub) broadcast(typ string, data any) {
	h.mu.Lock()
	wins := make([]*window, 0, len(h.wins))
	for _, w := range h.wins {
		wins = append(wins, w)
	}
	h.mu.Unlock()
	for _, w := range wins {
		w.send(typ, "", data)
	}
}
