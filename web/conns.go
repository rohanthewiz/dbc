package web

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
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
// up here at the next start. The TUI only reads the file.
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

// maxConnName bounds a connection name. It is shown in the sidebar, the
// topbar and every history row: a name that long is a paste gone wrong.
const maxConnName = 64

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

// connForm is the add- or edit-connection form, for a test or a save.
type connForm struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
	DSN    string `json:"dsn"`
	AIRows bool   `json:"ai_rows"`
	// TLS as typed: ${VAR}s unexpanded and a relative path relative, as the
	// saved file keeps them (config.SavedConn); tlsFor resolves them.
	config.TLSOpts
	// From is, on a test from the edit form, the connection being edited:
	// an empty DSN then means its stored one (keepDSN). An edit's save names
	// the connection in its path instead.
	From string `json:"from,omitempty"`

	// Parts, when set, is the DSN as fields; it replaces DSN (partsDSN).
	Parts *db.DSNParts `json:"parts,omitempty"`
	// KeepPassword: editing in fields, the password field was left empty
	// over a stored password the form was not given — keep that one.
	KeepPassword bool `json:"keep_password,omitempty"`
}

// formDSN settles f.DSN, whichever way the form gave it: built from fields,
// or kept from the stored connection from ("" when adding).
func (s *Server) formDSN(f *connForm, from string) error {
	if f.Parts != nil {
		return s.partsDSN(f, from)
	}
	if from == "" {
		return nil
	}
	return s.keepDSN(f, from)
}

// partsDSN writes f.Parts into f.DSN. With KeepPassword, the password is the
// one in from's stored DSN — taken apart with the driver it was stored
// under, so switching drivers (a postgres server moving to a mysql one, say)
// still keeps it. BuildDSN's refusals name the field to fix; they go back as
// a 400, which the form shows beside its buttons.
func (s *Server) partsDSN(f *connForm, from string) error {
	p := *f.Parts
	if f.KeepPassword && p.Password == "" && from != "" {
		sc, found, err := s.saved.Get(from)
		if err != nil {
			return err
		}
		if !found {
			return notFound("no saved connection named %q", from)
		}
		old, err := db.SplitDSN(sc.Driver, sc.DSN)
		if err != nil {
			return badRequest("%q's stored DSN cannot be taken apart to keep its password — type the password, or edit the DSN as text", from)
		}
		p.Password = old.Password
	}
	dsn, err := db.BuildDSN(strings.TrimSpace(f.Driver), p)
	if err != nil {
		return badRequest("%s", userMsg(err))
	}
	f.DSN = dsn
	return nil
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

// keepDSN fills an empty DSN in f with the one stored for the saved
// connection from — the edit form's "leave the DSN unchanged", which exists
// because the browser never has the DSN to send back. It is the DSN as
// typed, ${VAR}s and all, so a kept DSN expands as it always did.
//
// A kept DSN goes only with its own driver: a postgres URL handed to the
// mysql driver cannot work, so changing the driver asks for a DSN to match.
// A DSN that is typed is left alone for check to judge as any other.
func (s *Server) keepDSN(f *connForm, from string) error {
	if strings.TrimSpace(f.DSN) != "" {
		return nil
	}
	sc, found, err := s.saved.Get(from)
	if err != nil {
		return err
	}
	if !found {
		return notFound("no saved connection named %q", from)
	}
	if drv := strings.TrimSpace(f.Driver); drv != sc.Driver {
		return badRequest("%q's DSN is a %s one — type a %s DSN to change the driver", from, sc.Driver, drv)
	}
	f.DSN = sc.DSN
	return nil
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
	if err := f.TLSOpts.Check(); err != nil {
		return badRequest("%s", userMsg(err))
	}
	// The form hides the TLS fields for the embedded engines, so this is a
	// stale form or a hand-made request; refused here rather than saved and
	// then failing every connect (db.tlsOpen refuses it too).
	if drv, _ := db.Driver(f.Driver); f.TLSOpts.Set() && drv != "pgx" && drv != "mysql" {
		return badRequest("TLS settings are for postgres and mysql — %s opens a local file", f.Driver)
	}
	return nil
}

// tlsFor is f's TLS settings as a connect uses them: paths resolved the way
// config.MergeSaved resolves a saved entry's, so what is tested here, what
// runs now and what the next start merges are the same files.
func (f *connForm) tlsFor(name string) (config.TLSOpts, []string) {
	return config.ExpandTLS(name, f.TLSOpts, config.SavedDir())
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
	if err := s.formDSN(&f, f.From); err != nil {
		return fail(ctx, err)
	}
	if err := f.check(false); err != nil {
		return fail(ctx, err)
	}
	name := f.Name
	if name == "" {
		name = "(new connection)"
	}
	dsn, warns := config.ExpandDSN(name, f.Driver, f.DSN)
	tlsOpts, tw := f.tlsFor(name)
	warns = append(warns, tw...)
	timeout := s.cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = testTimeoutCap
	}
	// Bounded by the timeout alone: rweb gives a handler no context that
	// ends when the client goes away, so a closed dialog leaves the dial to
	// finish (or time out) on its own; its answer is then just not read.
	res, err := db.Probe(context.Background(), config.Connection{Name: name, Driver: f.Driver, DSN: dsn, TLSOpts: tlsOpts}, timeout)
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
// The config is changed first, the file second: AddConn is where a
// duplicate name is caught atomically (two windows saving the same name
// race there, not in the file), and a file that then fails to write is
// undone by RemoveConn, so the two never disagree about what exists. The
// file can refuse a name the config allowed: another dbc web added it
// since this one started. It
// does not connect — the page switches its tab to it right after, where
// the connect's outcome shows the way every connect's does.
func (s *Server) handleConnAdd(ctx rweb.Context) error {
	var f connForm
	if err := decode(ctx, &f); err != nil {
		return fail(ctx, err)
	}
	if err := s.formDSN(&f, ""); err != nil {
		return fail(ctx, err)
	}
	if err := f.check(true); err != nil {
		return fail(ctx, err)
	}
	dsn, warns := config.ExpandDSN(f.Name, f.Driver, f.DSN)
	tlsOpts, tw := f.tlsFor(f.Name)
	warns = append(warns, tw...)
	cn := config.Connection{Name: f.Name, Driver: f.Driver, DSN: dsn, TLSOpts: tlsOpts, AIRows: f.AIRows, Web: true}
	if err := s.cfg.AddConn(cn); err != nil {
		if errors.Is(err, config.ErrConnExists) {
			return fail(ctx, conflict("a connection named %q already exists", f.Name))
		}
		return fail(ctx, err)
	}
	// the DSN as typed, ${VAR}s and all: see config.SavedConn
	if err := s.saved.Add(config.SavedConn{Name: f.Name, Driver: f.Driver, DSN: f.DSN,
		TLSOpts: f.TLSOpts, AIRows: f.AIRows}); err != nil {
		s.cfg.RemoveConn(f.Name)
		if errors.Is(err, config.ErrConnExists) {
			return fail(ctx, conflict("a connection named %q was added by another dbc web — "+
				"restart this one to see it", f.Name))
		}
		return fail(ctx, serr.Wrap(err, "op", "add conn"))
	}
	if w := s.unsavedConnWarning("connection"); w != "" {
		warns = append(warns, w)
	}
	list := s.connList()
	s.hub.broadcast("conns", list)
	list["warnings"] = warns
	return ok(ctx, list)
}

// unsavedConnWarning is the note for a connection change that no file
// keeps — there is no home directory to keep one in: it holds only until
// dbc web stops. what is the thing that lasts that long. (A second dbc web,
// whose store is memory-only, still writes the saved-connections file: that
// has no long-held lock to lose to the first.)
func (s *Server) unsavedConnWarning(what string) string {
	if s.saved.Persistent() {
		return ""
	}
	return "connections are not being saved (no home directory to keep them in), " +
		"so this " + what + " lasts only until dbc web stops"
}

// handleConnEdit is PUT /api/v1/conns/:name: change a connection added in
// the browser — its name, driver, DSN, TLS or ai_rows — in place. The body is the
// add form's; an empty DSN keeps the stored one (keepDSN).
//
// Anything but ai_rows changes what the connection connects to, or what
// it is called, and a query tab on it holds a session (and perhaps a
// transaction) opened under the old name and DSN. So, as for a removal,
// those edits are refused while any tab is on it or connecting to it. An
// ai_rows change alone goes through regardless: it is read afresh from the
// config each time the assistant is asked (workspace/chat.go), so the
// tab's next question simply follows it.
//
// The order mirrors handleConnAdd: config first, where a clashing new name
// or a removal by another window is caught atomically (ReplaceConn), then
// the file, undone in the config if it fails. On a rename, the saved query
// tabs on the old name are moved to the new one in the store (RetagTabs).
// The pool opened under the old name and DSN is dropped last, so the next
// connect opens a fresh one from the new entry. The check-then-change gap is the one handleConnDelete
// describes, with the same outcome: an error on a tab that raced into it.
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
	cur, known := s.cfg.ConnByName(name)
	if !known {
		return fail(ctx, notFound("no connection named %q", name))
	}
	if !cur.Web {
		return fail(ctx, s.fileConnRefusal(name, "change"))
	}
	if err = s.formDSN(&f, name); err != nil {
		return fail(ctx, err)
	}
	if err = f.check(true); err != nil {
		return fail(ctx, err)
	}
	dsn, warns := config.ExpandDSN(f.Name, f.Driver, f.DSN)
	tlsOpts, tw := f.tlsFor(f.Name)
	warns = append(warns, tw...)
	next := config.Connection{Name: f.Name, Driver: f.Driver, DSN: dsn, TLSOpts: tlsOpts, AIRows: f.AIRows, Web: true}

	// Compared expanded, as the pool would see it: a DSN retyped to the same
	// text, or kept, is no change; a ${VAR} whose value moved since startup
	// is one, and the pool should pick it up. TLS settings are part of how
	// the pool dials, so a change to them reconnects too.
	renamed := f.Name != name
	reconnects := renamed || f.Driver != cur.Driver || dsn != cur.DSN || tlsOpts != cur.TLSOpts
	if reconnects {
		if n := s.hub.tabsOn(name); n > 0 {
			return fail(ctx, conflict("%s on %q — switch %s to another connection first "+
				"(the assistant's row access alone can change while it is in use)",
				tabsAre(n), name, itThem(n)))
		}
	}

	if err = s.cfg.ReplaceConn(name, next); err != nil {
		switch {
		case errors.Is(err, config.ErrConnExists):
			return fail(ctx, conflict("a connection named %q already exists", f.Name))
		case errors.Is(err, config.ErrConnNotFound):
			return fail(ctx, notFound("no connection named %q", name))
		}
		return fail(ctx, err)
	}
	// the DSN as typed (or as stored, when kept): see config.SavedConn
	if err = s.saved.Update(name, config.SavedConn{Name: f.Name, Driver: f.Driver, DSN: f.DSN,
		TLSOpts: f.TLSOpts, AIRows: f.AIRows}); err != nil {
		// Put the config back as it was. That fails only if another window
		// added a connection under the old name in the moment since it was
		// given up; the edit then stands for this run and the file keeps
		// the old entry, which the next start skips as a clash and says so.
		_ = s.cfg.ReplaceConn(f.Name, cur)
		switch {
		case errors.Is(err, config.ErrConnExists):
			return fail(ctx, conflict("a connection named %q was added by another dbc web — "+
				"restart this one to see it", f.Name))
		case errors.Is(err, config.ErrConnNotFound):
			return fail(ctx, notFound("%q is no longer in %s — removed by another dbc web, "+
				"or by hand", name, s.saved.Path()))
		}
		return fail(ctx, serr.Wrap(err, "op", "edit conn"))
	}
	if renamed {
		// A saved tab's conn is what it reconnects to when shown again; left
		// on a name that no longer exists it would fall back to whatever the
		// window has active. A failure here is only that, so it is a warning
		// rather than an undo of an edit the file already holds.
		err = s.store.RetagTabs(name, f.Name)
		// tabs saved on one of its other databases ("<old>/analytics")
		// follow it to "<new>/analytics"
		if tabsSaved, lerr := s.store.Tabs(); err == nil && lerr == nil {
			for _, st := range tabsSaved {
				if derivedFrom(s.cfg, st.Conn, name) && err == nil {
					err = s.store.RetagTabs(st.Conn, config.DerivedName(f.Name, st.Conn[len(name)+len(config.DatabaseSep):]))
				}
			}
		}
		if err != nil {
			warns = append(warns, fmt.Sprintf(
				"saved query tabs on %q were not moved to %q, and will open on the window's connection: %v",
				name, f.Name, err))
		}
		if err = s.moveSchemaPicks(name, f.Name); err != nil {
			warns = append(warns, fmt.Sprintf(
				"saved schema picks on %q were not moved to %q, and it will open on its default schema: %v",
				name, f.Name, err))
		}
	}
	if reconnects {
		s.mgr.Drop(name)
	}
	if w := s.unsavedConnWarning("change"); w != "" {
		warns = append(warns, w)
	}
	list := s.connList()
	if renamed {
		list["renamed"] = map[string]string{"from": name, "to": f.Name}
	}
	s.hub.broadcast("conns", list)
	list["warnings"] = warns
	return ok(ctx, list)
}

// fileConnRefusal is the answer to changing or removing a connection that
// is not the browser's to change: the config file's, or a built-in demo.
// verb is what was asked ("change", "remove").
func (s *Server) fileConnRefusal(name, verb string) error {
	if s.cfg.Demo {
		return badRequest("%q is a built-in demo connection", name)
	}
	where := "the config file"
	if s.cfg.Path != "" {
		where = s.cfg.Path
	}
	return badRequest("%q is defined in %s — edit the file to %s it", name, where, verb)
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
		return fail(ctx, s.fileConnRefusal(name, "remove"))
	}
	if n := s.hub.tabsOn(name); n > 0 {
		return fail(ctx, conflict("%s on %q — switch %s to another connection first",
			tabsAre(n), name, itThem(n)))
	}
	s.cfg.RemoveConn(name)
	s.mgr.Drop(name)
	if err = s.saved.Delete(name); err != nil {
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
