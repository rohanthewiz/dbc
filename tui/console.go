package tui

import (
	"errors"
	"io/fs"
	"os"
	"slices"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/userdata"
)

// SQL consoles: the editor holds one running SQL file of the database the
// active connection is on. Consoles are namespaced host ─► database ─► name
// (userdata.ConsoleDB), a database may have several, and a connect that
// lands on another database swaps the editor to one of that database's.
//
//	start ──► openConsole(active)        editor = active database's console
//	connect lands on B ──► switchConsole(B):
//	        same database as the editor's? ─► nothing (a reconnect, or a
//	                                          second name for one database)
//	        else ─► save the editor's console, open B's — the one last open
//	                on B this run, else B's first
//	⌥N ──► newConsole       save, open a fresh console of the same database
//	⌥C ──► nextConsole      save, open the database's next console (wraps)
//	quit ──► saveConsole                 editor ─► its console
//
// Every swap parks the console left — caret, selection, scroll and undo
// (editor.View) — and a console reopened this run is restored from it
// (editor.Restore) instead of opening at the top with no history. Should
// its file have changed meanwhile, the new text comes in as one undoable
// edit over the parked one, as dbc web loads a console saved elsewhere.
//
// The swap is on the connect LANDING (connected), not on the pick: a
// connect that fails or is canceled leaves the connection where it was, and
// the console must stay with it. Text typed while a connect dials belongs to
// the console in the editor, and is saved there before the swap.
//
// What is a "database" is db.ConsoleTarget's call: host and port plus the
// database name for the servers, the file for SQLite and bytdb.
//
// dbc web edits the same files. A save writes only when the editor's text
// differs from what was loaded (consoleText), so a console the TUI merely
// had open does not overwrite what a browser tab wrote to it meanwhile.

// consoleOf is the console database of the named connection, with the file
// the first consoles layout kept it in (for ListConsoles to adopt). ok is
// false when consoles are off (Options.NoPersist, no home directory) or the
// name is unknown.
func (m *Model) consoleOf(name string) (d userdata.ConsoleDB, flat string, ok bool) {
	if m.consoleDir == "" {
		return d, "", false
	}
	cc, found := m.cfg.ConnByName(name)
	if !found {
		return d, "", false
	}
	t := db.ConsoleTarget(cc)
	return userdata.ConsoleDBOf(t.Host, t.Database), userdata.FlatConsoleFile(m.consoleDir, t.Host, t.Database), true
}

// openConsole makes the first console of name's database the editor's and
// returns its text, for New to put in the editor. legacy is the single
// buffer.sql every dbc kept before consoles: on the first start with
// consoles (no consoles directory yet), a console with no file is seeded
// from it, so an upgrade does not lose the scratchpad. The directory is made
// then, so the seeding happens once, not on every start whose first console
// is new. With no console to open (no connection at all), the legacy buffer
// is still the editor's.
func (m *Model) openConsole(name, legacy string) string {
	d, flat, ok := m.consoleOf(name)
	if !ok {
		m.console = ""
		return userdata.LoadBuffer(legacy)
	}
	cname := userdata.DefaultConsole
	if names := userdata.ListConsoles(m.consoleDir, d, flat); len(names) > 0 {
		cname = names[0]
	}
	m.setConsole(d, cname)
	text, exists := userdata.LoadConsole(m.console)
	if exists {
		m.consoleText = text
		return text
	}
	if _, err := os.Stat(m.consoleDir); errors.Is(err, fs.ErrNotExist) {
		_ = os.MkdirAll(m.consoleDir, 0o755)
		return userdata.LoadBuffer(legacy) // differs from consoleText (""), so it is saved
	}
	return ""
}

// setConsole records which console the editor holds, before its text is
// loaded: the path and the database's last-open name.
func (m *Model) setConsole(d userdata.ConsoleDB, name string) {
	m.consoleDB, m.consoleName = d, name
	m.console = userdata.ConsolePath(m.consoleDir, d, name)
	m.consoleText = ""
	if m.lastConsole == nil {
		m.lastConsole = map[userdata.ConsoleDB]string{}
	}
	m.lastConsole[d] = name
}

// saveConsole writes the editor to its console — or, with no console (no
// connection to key one by), to the legacy single buffer, as before. Text
// unchanged since it was loaded is not written (see the package comment).
func (m *Model) saveConsole() error {
	text := m.editor.Text()
	if m.console != "" {
		if text == m.consoleText {
			return nil
		}
		if err := userdata.SaveConsole(m.console, text); err != nil {
			return err
		}
		m.consoleText = text
		return nil
	}
	if m.consoleDir == "" {
		return nil // persistence off
	}
	return userdata.SaveBuffer(userdata.BufferFile(), text)
}

// openInEditor saves the editor's console and swaps in console name of d. A
// console that cannot be saved is not swapped out: the editor keeps it and
// says why, rather than drop what was typed. It reports whether the swap
// happened.
func (m *Model) openInEditor(d userdata.ConsoleDB, name string) bool {
	if err := m.saveConsole(); err != nil {
		m.logf(logWarn, "kept the editor's SQL: could not save its console: %s", serr.StringFromErr(err))
		return false
	}
	// park the console being left with its caret, scroll and undo, so a
	// switch back finds it as it was (keyed by file: one per console)
	if m.console != "" {
		if m.consoleViews == nil {
			m.consoleViews = map[string]editorView{}
		}
		m.consoleViews[m.console] = m.editor.View()
	}
	m.setConsole(d, name)
	text, _ := userdata.LoadConsole(m.console)
	m.consoleText = text
	m.compl = nil // a popup over the old text would complete into the new
	if v, ok := m.consoleViews[m.console]; ok {
		// a file changed since it was left (dbc web, an editor) comes in
		// as one undoable edit over the parked text — see Restore. It is
		// then exactly the file's text, so it is not written back unless
		// the user edits or undoes.
		delete(m.consoleViews, m.console)
		m.editor.Restore(v, text)
	} else {
		m.editor.SetText(text)
	}
	m.logf(logMuted, "console: %s · %s · %s", d.Host, d.Database, name)
	return true
}

// switchConsole swaps the editor to name's database's console after a
// connect landed on it: the console last open there this run, else its
// first. Another connection onto the database already in the editor
// changes nothing.
func (m *Model) switchConsole(name string) {
	d, flat, ok := m.consoleOf(name)
	if !ok || (m.console != "" && d == m.consoleDB) {
		return
	}
	cname := m.lastConsole[d]
	if cname == "" {
		cname = userdata.DefaultConsole
		if names := userdata.ListConsoles(m.consoleDir, d, flat); len(names) > 0 {
			cname = names[0]
		}
	}
	m.openInEditor(d, cname)
}

// consoleNames is the editor's database's consoles: those with a file, and
// the one in the editor even before it has one (a new, still empty console).
func (m *Model) consoleNames() []string {
	names := userdata.ListConsoles(m.consoleDir, m.consoleDB, "")
	if m.consoleName != "" && !slices.Contains(names, m.consoleName) {
		names = append(names, m.consoleName)
	}
	return names
}

// newConsole (⌥N) opens a fresh console of the editor's database. It gets
// no file until something is typed in it.
func (m *Model) newConsole() tea.Cmd {
	if m.console == "" {
		m.log(logWarn, "no console to add to: connect to a database first")
		return nil
	}
	// the current console is saved before the listing, so an empty one
	// that is about to get its file counts as taken
	if err := m.saveConsole(); err != nil {
		m.logf(logWarn, "kept the editor's SQL: could not save its console: %s", serr.StringFromErr(err))
		return nil
	}
	m.openInEditor(m.consoleDB, userdata.NextConsoleName(m.consoleNames()))
	return nil
}

// nextConsole (⌥C) opens the database's next console, wrapping round.
func (m *Model) nextConsole() tea.Cmd {
	if m.console == "" {
		m.log(logWarn, "no consoles: connect to a database first")
		return nil
	}
	names := m.consoleNames()
	if len(names) < 2 {
		m.log(logInfo, "this database has one console — ⌥N adds another")
		return nil
	}
	i := slices.Index(names, m.consoleName)
	m.openInEditor(m.consoleDB, names[(i+1)%len(names)])
	return nil
}

// consoleMenuItems are the editor menu's console rows: every console of
// the database, the one in the editor marked, and a new one.
func (m *Model) consoleMenuItems() []menuItem {
	if m.console == "" {
		return nil
	}
	items := []menuItem{heading("consoles · " + m.consoleDB.Host + " · " + m.consoleDB.Database)}
	for _, n := range m.consoleNames() {
		label, why := "  "+n, ""
		if n == m.consoleName {
			label, why = "● "+n, "it is the one in the editor"
		}
		items = append(items, menuItem{label: label, why: why, act: func(m *Model) tea.Cmd {
			m.openInEditor(m.consoleDB, n)
			return nil
		}})
	}
	return append(items,
		menuItem{label: "+ New console", key: "⌥N", act: func(m *Model) tea.Cmd { return m.newConsole() }})
}
