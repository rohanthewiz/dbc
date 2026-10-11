package web

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/connedit"
	"github.com/rohanthewiz/dbc/db"
)

// Connections added in the browser.
//
// The config file stays the file's: dbc web never writes it back (the TOML
// encoder would lose the user's comments and layout). A connection added
// here is kept in the saved-connections file (~/.config/dbc/connections.toml,
// see config.SavedStore) and merged into the running config — at startup,
// by every dbc command (config.LoadSaved), and at once when it is added — so
// the workspaces, the manager and every handler find it through ConnByName
// like any other, and the TUI and headless runs list it too.
//
//	                 ┌── config file ([[connection]]) ──┐
//	startup:  cfg ◄──┤                                  ├── file's names win a clash
//	(any dbc)        └── connections.toml ──────────────┘
//
//	GET    /api/v1/conns/:name/parts ► a saved connection's DSN as fields, for
//	                             the edit form: db.SplitDSN, password withheld
//	POST   /api/v1/conns/test ─► db.Probe: open, ping, close; nothing saved
//	POST   /api/v1/conns ──────► cfg.AddConn, saved.Add, "conns" to every window
//	PUT    /api/v1/conns/:name ► an edit (rename, driver, DSN, TLS, ai_rows); all but an
//	                             ai_rows change refused while a query tab is on it;
//	                             cfg.ReplaceConn, saved.Update, store.RetagTabs,
//	                             mgr.Drop, "conns"
//	DELETE /api/v1/conns/:name ► refused while a query tab is on it; else
//	                             cfg.RemoveConn, mgr.Drop, saved.Delete, "conns"
//
// A running dbc web reads the file only at startup (and at each change it
// makes, under the lock): a connection another dbc web adds meanwhile shows
// up here at the next start. The TUI writes the file too, through the same
// connedit.Editor (tui/connform.go), so a connection it adds is listed here
// at the next start as well.
//
// The validation, the DSN-from-fields, the keep-what-was-left-empty rules
// and the config-then-file order are connedit's, shared with the TUI; what
// is left here is HTTP — decoding, the status of each refusal (connErr) —
// and what only the browser keeps: query tabs, tab groups and schema picks
// in the store, moved on a rename.
//
// A DSN never goes back to the browser: the list says name, driver, ai_rows,
// the TLS settings (file paths, not secrets) and whether it was added here — enough to draw the sidebar, fill the edit
// form and offer Edit and Remove. So the edit form cannot show the DSN it
// would change; it sends an empty one for "leave it as it is" and the
// server takes the stored one (keepDSN).
//
// FIELDS. The form can also send the DSN as parts (host, port, user,
// password, database, options — or a file), which the server writes into a
// DSN with db.BuildDSN before anything else sees the form: from there on a
// form of fields and a form with a typed DSN are the same request. Editing
// in fields, the form gets the stored DSN taken apart (handleConnParts) —
// every part but the password, which, like the DSN, never goes back. Left
// empty it is kept (keep_password), taken again from the stored DSN.
//
//	browser                       server
//	parts ──────────────────────► partsDSN: keep_password? stored one
//	                               BuildDSN ─► f.DSN ─► check, probe, save

// moveStoreConns moves the connections an older dbc web kept in its store
// (web.bytdb's conns table) to the saved-connections file, where every dbc
// reads them, and merges the moved ones into the running config — the
// caller merged the file before they were in it.
//
//	for each row in web.bytdb:
//	  file already has the name ─► drop the row (moved by an earlier run
//	                               that stopped before the delete)
//	  saved.Add ok ──────────────► merge into cfg, drop the row
//	  saved.Add fails ───────────► keep the row for the next start, and
//	                               merge it into cfg anyway, as before
//
// Only a dbc web holding web.bytdb gets here with rows: a second one's
// store is memory-only and empty, so two cannot move one row twice. It runs
// on every start and costs one empty query once the table is empty.
func (s *Server) moveStoreConns() []string {
	rows, err := s.store.Conns()
	if err != nil {
		return []string{fmt.Sprintf("connections added in an earlier dbc web could not be read: %v", err)}
	}
	if len(rows) == 0 {
		return nil
	}
	known := func(driver string) bool {
		_, err := db.Driver(driver)
		return err == nil
	}
	var warns []string
	for _, row := range rows {
		sc := config.SavedConn{Name: row.Name, Driver: row.Driver, DSN: row.DSN,
			AIRows: row.AIRows, Added: row.Added}
		switch err = s.saved.Add(sc); {
		case errors.Is(err, config.ErrConnExists):
			// the file's entry is the newer word; it was merged at startup
		case err != nil:
			warns = append(warns, fmt.Sprintf(
				"saved connection %q could not be moved to %s, so only dbc web lists it: %v",
				row.Name, s.saved.Path(), err))
			warns = append(warns, s.cfg.MergeSaved([]config.SavedConn{sc}, known)...)
			continue
		default:
			warns = append(warns, s.cfg.MergeSaved([]config.SavedConn{sc}, known)...)
		}
		if err = s.store.DeleteConn(row.Name); err != nil {
			// harmless: the next start finds it in the file and drops it then
			s.opt.Logf("warning: saved connection %q moved, but not removed from the store: %v", row.Name, err)
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
		out[i] = connInfo{Name: c.Name, Driver: c.Driver, Saved: c.Web, AIRows: c.AIRows, TLSOpts: c.TLSOpts}
	}
	return map[string]any{"conns": out, "default": s.defaultConn()}
}

// connForm is the add- or edit-connection form, for a test or a save, as
// the page sends it: connedit.Form's fields, plus From on a test.
type connForm struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
	DSN    string `json:"dsn"`
	AIRows bool   `json:"ai_rows"`
	// TLS as typed: ${VAR}s unexpanded and a relative path relative, as the
	// saved file keeps them (config.SavedConn); connedit resolves them.
	config.TLSOpts
	// From is, on a test from the edit form, the connection being edited:
	// an empty DSN then means its stored one (connedit.Editor.FormDSN). An
	// edit's save names the connection in its path instead.
	From string `json:"from,omitempty"`

	// Parts, when set, is the DSN as fields; it replaces DSN.
	Parts *db.DSNParts `json:"parts,omitempty"`
	// KeepPassword: editing in fields, the password field was left empty
	// over a stored password the form was not given — keep that one.
	KeepPassword bool `json:"keep_password,omitempty"`
}

// form is f as connedit takes it. connForm keeps its own fields (rather
// than embedding connedit.Form) so a request's JSON, and the tests that
// build one, stay exactly as they were before the logic moved out.
func (f connForm) form() connedit.Form {
	return connedit.Form{Name: f.Name, Driver: f.Driver, DSN: f.DSN, AIRows: f.AIRows,
		TLSOpts: f.TLSOpts, Parts: f.Parts, KeepPassword: f.KeepPassword}
}

// connEditor is the shared add/edit/remove logic over this server's config,
// saved-connections file and pools.
func (s *Server) connEditor() *connedit.Editor {
	return &connedit.Editor{Cfg: s.cfg, Saved: s.saved, Mgr: s.mgr, App: "dbc web"}
}

// connErr turns connedit's refusals into this package's request errors, so
// fail answers them with the status they always had: an Invalid form is a
// 400, a missing connection a 404, a clash or a race a 409. Anything else
// (the file could not be written) passes through as a 500.
func connErr(err error) error {
	var ce *connedit.Error
	if !errors.As(err, &ce) {
		return err
	}
	switch ce.Kind {
	case connedit.NotFound:
		return notFound("%s", ce.Msg)
	case connedit.Conflict:
		return conflict("%s", ce.Msg)
	}
	return badRequest("%s", ce.Msg)
}

// handleConnParts is GET /api/v1/conns/:name/parts: a saved connection's
// DSN as the form's fields. A password goes back only when it is a ${VAR}
// reference, which says where the secret is rather than being it; any other
// is withheld and reported as has_password, so the form can say "unchanged".
// A DSN the fields cannot hold (db.ErrNotFields) answers parts: null and
// why, and the form opens on the DSN text instead.
//
// Either way it carries the TLS settings as stored — ${VAR}s and relative
// paths as typed — which the form puts in place of the resolved ones the
// sidebar has, so saving an edit does not quietly pin them to today's
// expansion.
func (s *Server) handleConnParts(ctx rweb.Context) error {
	name, err := url.PathUnescape(ctx.Request().PathParam("name"))
	if err != nil {
		return fail(ctx, badRequest("bad connection name in the path"))
	}
	sc, found, err := s.saved.Get(name)
	if err != nil {
		return fail(ctx, err)
	}
	if !found {
		return fail(ctx, notFound("no saved connection named %q", name))
	}
	p, err := db.SplitDSN(sc.Driver, sc.DSN)
	if err != nil {
		return ok(ctx, map[string]any{"parts": nil, "reason": userMsg(err), "tls": sc.TLSOpts})
	}
	hasPassword := p.Password != "" && !db.IsEnvRef(p.Password)
	if hasPassword {
		p.Password = ""
	}
	return ok(ctx, map[string]any{"parts": p, "has_password": hasPassword, "tls": sc.TLSOpts})
}

// handleConnTest is POST /api/v1/conns/test: does this DSN connect? A form
// that cannot work is a 400; a connect that fails is a 200 with ok: false
// and the driver's words, because the test itself worked — it is the answer.
//
// Bounded by connect_timeout alone (connedit.TestTimeoutCap when that is
// "no limit"): rweb gives a handler no context that ends when the client
// goes away, so a closed dialog leaves the dial to finish (or time out) on
// its own; its answer is then just not read.
func (s *Server) handleConnTest(ctx rweb.Context) error {
	var f connForm
	if err := decode(ctx, &f); err != nil {
		return fail(ctx, err)
	}
	res, err := s.connEditor().Test(context.Background(), f.form(), f.From)
	if err != nil {
		return fail(ctx, connErr(err))
	}
	out := map[string]any{"ok": res.OK, "warnings": res.Warnings}
	if res.ProbeErr != nil {
		out["error"] = res.ProbeMsg()
		return ok(ctx, out)
	}
	out["ms"] = res.Took.Milliseconds()
	if res.Note != "" {
		out["note"] = res.Note
	}
	return ok(ctx, out)
}

// handleConnAdd is POST /api/v1/conns: add a connection and keep it
// (connedit.Editor.Add: config first, file second, undone on a failure).
// It does not connect — the page switches its tab to it right after, where
// the connect's outcome shows the way every connect's does.
func (s *Server) handleConnAdd(ctx rweb.Context) error {
	var f connForm
	if err := decode(ctx, &f); err != nil {
		return fail(ctx, err)
	}
	warns, err := s.connEditor().Add(f.form())
	if err != nil {
		return fail(ctx, connErr(err))
	}
	list := s.connList()
	s.hub.broadcast("conns", list)
	list["warnings"] = warns
	return ok(ctx, list)
}

// inUseRefusal is the in-use rule connedit asks about an edit or a removal:
// a query tab in any window on the connection (or connecting to it) holds a
// session — perhaps a transaction — under its old name and DSN, so it is
// refused until the user switches those tabs away. forEdit adds that the
// assistant's row access alone may still change.
func (s *Server) inUseRefusal(forEdit bool) func(string) error {
	return func(name string) error {
		n := s.hub.tabsOn(name)
		if n == 0 {
			return nil
		}
		if forEdit {
			return conflict("%s on %q — switch %s to another connection first "+
				"(the assistant's row access alone can change while it is in use)",
				tabsAre(n), name, itThem(n))
		}
		return conflict("%s on %q — switch %s to another connection first",
			tabsAre(n), name, itThem(n))
	}
}

// handleConnEdit is PUT /api/v1/conns/:name: change a connection added in
// the browser — its name, driver, DSN, TLS or ai_rows — in place. The body
// is the add form's; an empty DSN keeps the stored one. See
// connedit.Editor.Edit for the order of the changes and the in-use rule,
// which here is the hub's tab count across every window.
//
// On a rename, the saved query tabs on the old name are moved to the new
// one in the store (RetagTabs), with its schema picks and tab groups. The
// check-then-change gap is the one handleConnDelete describes, with the
// same outcome: an error on a tab that raced into it.
//
// A rename is broadcast with the "conns" event as renamed {from, to}, so
// every window can move the query tabs it has on the old name (ones not yet
// shown, which have no workspace and so do not count as "on" it) to the
// new — the store's saved tabs are moved by RetagTabs.
func (s *Server) handleConnEdit(ctx rweb.Context) error {
	name, err := url.PathUnescape(ctx.Request().PathParam("name"))
	if err != nil {
		return fail(ctx, badRequest("bad connection name in the path"))
	}
	var f connForm
	if err = decode(ctx, &f); err != nil {
		return fail(ctx, err)
	}
	res, err := s.connEditor().Edit(name, f.form(), s.inUseRefusal(true))
	if err != nil {
		return fail(ctx, connErr(err))
	}
	to := strings.TrimSpace(f.Name) // as connedit saved it
	warns := res.Warnings
	if res.Renamed {
		// A saved tab's conn is what it reconnects to when shown again; left
		// on a name that no longer exists it would fall back to whatever the
		// window has active. A failure here is only that, so it is a warning
		// rather than an undo of an edit the file already holds.
		err = s.store.RetagTabs(name, to)
		// tabs saved on one of its other databases ("<old>/analytics")
		// follow it to "<new>/analytics"
		if tabsSaved, lerr := s.store.Tabs(); err == nil && lerr == nil {
			for _, st := range tabsSaved {
				if derivedFrom(s.cfg, st.Conn, name) && err == nil {
					err = s.store.RetagTabs(st.Conn, config.DerivedName(to, st.Conn[len(name)+len(config.DatabaseSep):]))
				}
			}
		}
		if err != nil {
			warns = append(warns, fmt.Sprintf(
				"saved query tabs on %q were not moved to %q, and will open on the window's connection: %v",
				name, to, err))
		}
		if err = s.moveSchemaPicks(name, to); err != nil {
			warns = append(warns, fmt.Sprintf(
				"saved schema picks on %q were not moved to %q, and it will open on its default schema: %v",
				name, to, err))
		}
		if err = s.moveGroupConns(name, to); err != nil {
			warns = append(warns, fmt.Sprintf(
				"saved tab groups on %q were not moved to %q, and will not gather its tabs: %v",
				name, to, err))
		}
		// the results query tabs keep for it (in memory, so nothing can fail)
		s.moveResults(name, to)
	}
	list := s.connList()
	if res.Renamed {
		list["renamed"] = map[string]string{"from": name, "to": to}
	}
	s.connCompl.Drop("") // what a pipeline field completed against may be gone
	s.hub.broadcast("conns", list)
	list["warnings"] = warns
	return ok(ctx, list)
}

// handleConnDelete is DELETE /api/v1/conns/:name: forget a connection added
// in the browser. The config file's connections are refused — they are the
// file's to change. So is one a query tab (in any window) is on or
// connecting to: removing it would pull the pool out from under that tab's
// session and whatever transaction it holds. The user switches those tabs
// first; the refusal says how many there are. See connedit.Editor.Delete.
func (s *Server) handleConnDelete(ctx rweb.Context) error {
	name, err := url.PathUnescape(ctx.Request().PathParam("name"))
	if err != nil {
		return fail(ctx, badRequest("bad connection name in the path"))
	}
	removed, err := s.connEditor().Delete(name, s.inUseRefusal(false))
	if removed {
		s.moveResults(name, "") // its results go with it
		// gone for this run even when the file failed: every window is
		// told, and the failure (it comes back at the next start) is the
		// answer
		list := s.connList()
		s.connCompl.Drop("") // what a pipeline field completed against may be gone
		s.hub.broadcast("conns", list)
		if err == nil {
			return ok(ctx, list)
		}
	}
	return fail(ctx, connErr(err))
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
//
// A tab on one of the connection's other databases ("<name>/analytics",
// config.DatabaseSep) counts: that pool was opened with the connection's
// host and credentials, and goes when it is edited (db.Manager.Drop).
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
		on := func(c string) bool { return c == name || derivedFrom(t.ws.Config(), c, name) }
		if on(t.ws.Active()) || (busy && on(connecting)) {
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

// schemaPickKey is the layout key prefix the page saves a connection's
// schema pick under: "tableSchema.<conn>" (app.js PICK_KEY). The value is
// the page's to read; the server only moves it on a rename.
const schemaPickKey = "tableSchema."

// moveSchemaPicks moves the saved schema picks of connection from — its own
// and those of its other databases ("<from>/analytics") — to to's names.
//
// The page moves the picks its window holds (connRenamed), but a window
// loads the layout only at boot, so a pick saved by a window that has since
// closed, or that never connected to that database, is in no window to be
// moved. The server reads the store's layout instead, which has all of
// them. Both moves landing is harmless: the page writes the same value
// under the same new key, and blanks an old key this has deleted.
func (s *Server) moveSchemaPicks(from, to string) error {
	saved, err := s.store.Layout()
	if err != nil {
		return err
	}
	moves := map[string]string{}
	for k := range saved {
		conn, isPick := strings.CutPrefix(k, schemaPickKey)
		switch {
		case !isPick:
		case conn == from:
			moves[k] = schemaPickKey + to
		case derivedFrom(s.cfg, conn, from):
			moves[k] = schemaPickKey + config.DerivedName(to, conn[len(from)+len(config.DatabaseSep):])
		}
	}
	if len(moves) == 0 {
		return nil
	}
	return s.store.MoveLayout(moves)
}

// derivedFrom reports whether conn is base's connection onto another of its
// server's databases: "<base>/<database>", and not a configured connection
// that happens to be named so. It reads the name rather than asking
// ConnByName, which cannot resolve a derived name once its base is renamed
// or removed — the cases it is asked about.
func derivedFrom(cfg *config.Config, conn, base string) bool {
	if !strings.HasPrefix(conn, base+config.DatabaseSep) || len(conn) == len(base)+len(config.DatabaseSep) {
		return false
	}
	return !slices.ContainsFunc(cfg.Conns(), func(c config.Connection) bool { return c.Name == conn })
}
