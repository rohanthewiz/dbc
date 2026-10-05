package web

import (
	"errors"
	"strings"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/userdata"
)

// Consoles: a query tab's text is one of its database's SQL consoles — the
// running .sql files the TUI keeps too (userdata, console.go), namespaced
// host ─► database ─► name — not a buffer of the tab's own.
//
//	tab ── on connection ──► database (host, database) ──► consoles
//	 └──── console name ───────────────────────────────────┘ (one of them)
//
// The server says which database a tab is on with every sidebar it sends
// (sideState.Console, from db.ConsoleTarget): the page swaps the tab's
// console when that changes, choosing a console of the new database that no
// other query tab of its window shows, or a new one — so two tabs on one
// database get two consoles, never one file written by both. The page picks
// the name; a console gets its file on the first save with text in it.
//
// THE FILES ARE SHARED: with the TUI, with other windows, with any editor.
// So a save says which revision it was made from (base: userdata.ConsoleRev
// of the text the page last loaded or saved), and a file that has moved on
// since is not overwritten:
//
//	PUT {text, base}  ── file's rev == base ──► written; {rev}; other
//	                                            windows told ("console")
//	                  ── file's rev != base ──► NOT written; {conflict,
//	                                            text, rev} of the file
//
// The page then loads the file's text as an edit it can undo (Ctrl+Z gets
// its own back, to save again, now from the current revision). consoleMu
// makes each read-compare-write one step against the other requests of this
// process; a writer outside it (the TUI) is caught by the next save's
// compare.
//
// Consoles are off — Options.ConsolesDir "" — in a memory-only dbc web
// (tests, no home directory): sideState.Console stays nil and every tab
// keeps its own buffer, as before consoles.

// consoleRef is the database a sidebar's connection is on, as a set of
// consoles: its directory levels, words for the user, and the consoles it
// has files for.
type consoleRef struct {
	userdata.ConsoleDB
	Label string   `json:"label"` // "db.example.com:5432 · app"
	Names []string `json:"names"` // ListConsoles' order
}

// consoleFor is the consoleRef of connection name, nil when consoles are
// off or the name is unknown. Listing the consoles also moves in a console
// the first consoles layout kept flat (userdata.ListConsoles).
func (s *Server) consoleFor(name string) *consoleRef {
	if s.opt.ConsolesDir == "" {
		return nil
	}
	cc, ok := s.cfg.ConnByName(name)
	if !ok {
		return nil
	}
	return consoleRefOf(s.opt.ConsolesDir, cc)
}

func consoleRefOf(root string, cc config.Connection) *consoleRef {
	t := db.ConsoleTarget(cc)
	d := userdata.ConsoleDBOf(t.Host, t.Database)
	label := t.Database
	if t.Host != "" {
		label = t.Host + " · " + t.Database
	}
	names := userdata.ListConsoles(root, d, userdata.FlatConsoleFile(root, t.Host, t.Database))
	if names == nil {
		names = []string{}
	}
	return &consoleRef{ConsoleDB: d, Label: label, Names: names}
}

// consolePath is the console the request's path names, checked: each level
// must be a name ConsoleDBOf could have made, so nothing from the browser
// reaches the filesystem as anything but one file in the consoles tree.
func (s *Server) consolePath(ctx rweb.Context) (userdata.ConsoleDB, string, string, error) {
	req := ctx.Request()
	d := userdata.ConsoleDB{Host: req.PathParam("host"), Database: req.PathParam("db")}
	name := req.PathParam("name")
	if s.opt.ConsolesDir == "" {
		return d, name, "", notFound("consoles are off in this dbc web (no config directory)")
	}
	path := userdata.ConsolePath(s.opt.ConsolesDir, d, name)
	if path == "" {
		return d, name, "", badRequest("not a console: %s/%s/%s", d.Host, d.Database, name)
	}
	return d, name, path, nil
}

// consoleText is a console as the page loads it.
type consoleText struct {
	Text string `json:"text"`
	Rev  string `json:"rev"` // "" when it has no file yet
}

// handleConsole is GET /api/v1/consoles/:host/:db/:name. A console with no
// file is empty, at revision "".
func (s *Server) handleConsole(ctx rweb.Context) error {
	_, _, path, err := s.consolePath(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	text, exists := userdata.LoadConsole(path)
	return ok(ctx, consoleText{Text: text, Rev: userdata.ConsoleRev(text, exists)})
}

// consoleSave is a PUT's body, and the console riding on a window's release.
type consoleSave struct {
	Text string `json:"text"`
	Base string `json:"base"` // the revision the text was edited from
}

// consoleSaved is a PUT's answer: the new revision, or — Conflict — the
// file's text and revision, which were not overwritten.
type consoleSaved struct {
	Rev      string `json:"rev"`
	Conflict bool   `json:"conflict,omitempty"`
	Text     string `json:"text,omitempty"`
}

// consoleEvent is the window-level "console" event: a console's new text,
// for the other windows showing it. Win is the window that saved it, which
// already has it.
type consoleEvent struct {
	userdata.ConsoleDB
	Name string `json:"name"`
	Text string `json:"text"`
	Rev  string `json:"rev"`
	Win  string `json:"win"`
}

// handleSaveConsole is PUT /api/v1/consoles/:host/:db/:name?win=<window>.
func (s *Server) handleSaveConsole(ctx rweb.Context) error {
	d, name, path, err := s.consolePath(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req consoleSave
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	out, err := s.saveConsole(ctx.Request().QueryParam("win"), d, name, path, req)
	if err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, out)
}

// saveConsole is the compare-and-write (see the comment at the top).
// Writing the text the file already holds is no conflict, whatever the base:
// two windows that typed the same thing agree.
func (s *Server) saveConsole(winID string, d userdata.ConsoleDB, name, path string, req consoleSave) (consoleSaved, error) {
	if len(req.Text) > maxBuffer {
		return consoleSaved{}, badRequest("the editor holds %d MB; consoles save up to %d MB", len(req.Text)>>20, maxBuffer>>20)
	}
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	cur, exists := userdata.LoadConsole(path)
	rev := userdata.ConsoleRev(cur, exists)
	if exists && cur == req.Text {
		return consoleSaved{Rev: rev}, nil
	}
	if rev != req.Base {
		return consoleSaved{Rev: rev, Conflict: true, Text: cur}, nil
	}
	if err := userdata.SaveConsole(path, req.Text); err != nil {
		return consoleSaved{}, err
	}
	text, exists := userdata.LoadConsole(path) // an empty never-saved console stays fileless
	rev = userdata.ConsoleRev(text, exists)
	s.hub.broadcast("console", consoleEvent{ConsoleDB: d, Name: name, Text: text, Rev: rev, Win: winID})
	return consoleSaved{Rev: rev}, nil
}

// consolesEvent is the window-level "consoles" event: a database's set of
// consoles changed (one renamed or deleted), with its list as it now is.
type consolesEvent struct {
	userdata.ConsoleDB
	Names   []string `json:"names"`
	Renamed *renamed `json:"renamed,omitempty"`
	Deleted string   `json:"deleted,omitempty"`
}

type renamed struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// handleRenameConsole is POST /api/v1/consoles/:host/:db/:name/rename {to}.
// Every window hears of it, so a tab showing the console follows it.
func (s *Server) handleRenameConsole(ctx rweb.Context) error {
	d, name, _, err := s.consolePath(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req struct {
		To string `json:"to"`
	}
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	req.To = strings.TrimSpace(req.To)
	if !userdata.ValidConsoleName(req.To) {
		return fail(ctx, badRequest("a console name is up to 64 letters, digits, '.', '-' or '_', not starting with '.'"))
	}
	s.consoleMu.Lock()
	err = userdata.RenameConsole(s.opt.ConsolesDir, d, name, req.To)
	s.consoleMu.Unlock()
	if errors.Is(err, userdata.ErrConsoleExists) {
		return fail(ctx, conflict("%s already has a console named %s", d.Database, req.To))
	}
	if err != nil {
		return fail(ctx, err)
	}
	s.hub.broadcast("consoles", consolesEvent{ConsoleDB: d, Names: s.listConsoles(d),
		Renamed: &renamed{From: name, To: req.To}})
	return ok(ctx, nil)
}

// handleDeleteConsole is DELETE /api/v1/consoles/:host/:db/:name. The page
// asks first; every window hears of it, so a tab showing the console moves
// to another.
func (s *Server) handleDeleteConsole(ctx rweb.Context) error {
	d, name, _, err := s.consolePath(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	s.consoleMu.Lock()
	err = userdata.DeleteConsole(s.opt.ConsolesDir, d, name)
	s.consoleMu.Unlock()
	if err != nil {
		return fail(ctx, err)
	}
	s.hub.broadcast("consoles", consolesEvent{ConsoleDB: d, Names: s.listConsoles(d), Deleted: name})
	return ok(ctx, nil)
}

// handleConsoles is GET /api/v1/consoles/:host/:db: the database's consoles,
// for the picker to list fresh.
func (s *Server) handleConsoles(ctx rweb.Context) error {
	d := userdata.ConsoleDB{Host: ctx.Request().PathParam("host"), Database: ctx.Request().PathParam("db")}
	if s.opt.ConsolesDir == "" || !d.Valid() {
		return fail(ctx, badRequest("not a console database: %s/%s", d.Host, d.Database))
	}
	return ok(ctx, map[string]any{"names": s.listConsoles(d)})
}

func (s *Server) listConsoles(d userdata.ConsoleDB) []string {
	names := userdata.ListConsoles(s.opt.ConsolesDir, d, "")
	if names == nil {
		names = []string{}
	}
	return names
}

// consoleMarker is the layout key that records tabs' buffers were moved
// into consoles (moveTabBuffers), so it is done once.
const consoleMarker = "consoles.moved"

// moveTabBuffers gives every saved tab that predates consoles a console of
// its own, holding its buffer — once, at the first start with consoles. A
// tab on a connection gets its database's first free console name (one
// whose file holds that very text is reused instead, so a TUI that already
// moved a scratchpad in is not duplicated); a tab with no connection, or an
// empty buffer, is left to the page, which gives it one when it connects.
// No file is overwritten, and the tabs' buffers stay in the store.
func (s *Server) moveTabBuffers() []string {
	if s.opt.ConsolesDir == "" {
		return nil
	}
	l, err := s.store.Layout()
	if err != nil || l[consoleMarker] != "" {
		return nil
	}
	tabs, err := s.store.Tabs()
	if err != nil {
		return []string{"could not read saved tabs to move them into consoles: " + err.Error()}
	}
	var warns []string
	for _, t := range tabs {
		if t.Console != "" || t.Conn == "" || strings.TrimSpace(t.Buffer) == "" {
			continue
		}
		ref := s.consoleFor(t.Conn)
		if ref == nil {
			continue
		}
		name := ""
		for _, n := range ref.Names {
			if text, _ := userdata.LoadConsole(userdata.ConsolePath(s.opt.ConsolesDir, ref.ConsoleDB, n)); text == t.Buffer {
				name = n
				break
			}
		}
		if name == "" {
			name = userdata.NextConsoleName(ref.Names)
			if err = userdata.SaveConsole(userdata.ConsolePath(s.opt.ConsolesDir, ref.ConsoleDB, name), t.Buffer); err != nil {
				warns = append(warns, "could not move tab "+t.Title+" into a console: "+err.Error())
				continue
			}
		}
		t.Console = name
		if err = s.store.SaveTab(t); err != nil {
			warns = append(warns, "could not record tab "+t.Title+"'s console: "+err.Error())
		}
	}
	_ = s.store.SetLayout(map[string]string{consoleMarker: "1"})
	return warns
}
