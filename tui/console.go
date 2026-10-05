package tui

import (
	"errors"
	"io/fs"
	"os"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/userdata"
)

// SQL consoles: the editor holds the running SQL file of the database the
// active connection is on, one per (host, database), and a connect that
// lands on another database swaps it for that one's.
//
//	start ──► openConsole(active)        editor = console(active's target)
//	connect lands on B ──► switchConsole(B):
//	        same target as the editor's? ─► nothing (a reconnect, or a
//	                                        second name for one database)
//	        else ─► save the editor to its console, load B's
//	quit ──► saveConsole                 editor ─► its console
//
// The swap is on the connect LANDING (connected), not on the pick: a
// connect that fails or is canceled leaves the connection where it was, and
// the console must stay with it. Text typed while a connect dials belongs to
// the console in the editor, and is saved there before the swap.
//
// What is a "database" is db.ConsoleTarget's call: host and port plus the
// database name for the servers, the file for SQLite and bytdb.

// consoleOf is the console file of the named connection: "" when consoles
// are off (Options.NoPersist, no home directory) or the name is unknown.
func (m *Model) consoleOf(name string) string {
	if m.consoleDir == "" {
		return ""
	}
	cc, ok := m.cfg.ConnByName(name)
	if !ok {
		return ""
	}
	t := db.ConsoleTarget(cc)
	return userdata.ConsoleFile(m.consoleDir, t.Host, t.Database)
}

// openConsole makes name's console the editor's and returns its text, for
// New to put in the editor. legacy is the single buffer.sql every dbc kept
// before consoles: on the first start with consoles (no consoles directory
// yet), a console with no file is seeded from it, so an upgrade does not
// lose the scratchpad. The directory is made then, so the seeding happens
// once, not on every start whose first console is new. With no console to
// open (no connection at all), the legacy buffer is still the editor's.
func (m *Model) openConsole(name, legacy string) string {
	m.console = m.consoleOf(name)
	if m.console == "" {
		return userdata.LoadBuffer(legacy)
	}
	text, exists := userdata.LoadConsole(m.console)
	if exists {
		return text
	}
	if _, err := os.Stat(m.consoleDir); errors.Is(err, fs.ErrNotExist) {
		_ = os.MkdirAll(m.consoleDir, 0o755)
		return userdata.LoadBuffer(legacy)
	}
	return ""
}

// saveConsole writes the editor to its console — or, with no console (no
// connection to key one by), to the legacy single buffer, as before.
func (m *Model) saveConsole() error {
	if m.console != "" {
		return userdata.SaveConsole(m.console, m.editor.Text())
	}
	if m.consoleDir == "" {
		return nil // persistence off
	}
	return userdata.SaveBuffer(userdata.BufferFile(), m.editor.Text())
}

// switchConsole swaps the editor to name's console after a connect landed
// on it. A console that cannot be saved is not swapped out: the editor keeps
// it — on the new connection — and says why, rather than drop what was
// typed.
func (m *Model) switchConsole(name string) {
	next := m.consoleOf(name)
	if next == "" || next == m.console {
		return
	}
	if m.console != "" {
		if err := userdata.SaveConsole(m.console, m.editor.Text()); err != nil {
			m.logf(logWarn, "kept the editor's SQL: could not save its console: %s", serr.StringFromErr(err))
			return
		}
	}
	text, _ := userdata.LoadConsole(next)
	m.compl = nil // a popup over the old text would complete into the new
	m.editor.SetText(text)
	m.console = next
	if cc, ok := m.cfg.ConnByName(name); ok {
		t := db.ConsoleTarget(cc)
		where := t.Database
		if t.Host != "" {
			where = t.Host + " · " + t.Database
		}
		m.logf(logMuted, "console: %s", where)
	}
}
