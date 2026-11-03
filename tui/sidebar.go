package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/workspace"
)

// The sidebar: connections on top, the active connection's tables below.
//
// One click on a connection connects to it — in a mouse-first UI a
// connection list is a switcher, and "select, then press Enter" is a
// keyboard idiom. The tables list is a catalog: one click selects, a
// double-click previews the rows, and right-click offers the rest.

// refreshConns rebuilds the connections list, marking the active one — or,
// while the sidebar is on another of its server's databases
// ("<conn>/<database>"), the configured connection it is derived from —
// with ●, and those other query tabs are on with ○.
func (m *Model) refreshConns() {
	items := make([]listItem, 0, len(m.cfg.Conns()))
	on := m.baseOf(m.ws.Active())
	if len(m.tabs) > 0 && m.active().unconnected {
		// on nothing: its workspace's active is only the config's default
		// (queryTab.unconnected), which a ● would claim it is on — while
		// its first connect is dialing, and after one that failed
		on = ""
	}
	others := map[string]bool{}
	for i := range m.tabs {
		if target, _ := m.tabTarget(i); i != m.curTab && target != "" {
			others[m.baseOf(target)] = true
		}
	}
	for i, c := range m.cfg.Conns() {
		// as dbc web's sidebar: a muted ✎ marks one added from inside dbc
		// (the connection form edits it), and · tls one that dials TLS
		sub := c.Driver
		if c.Web {
			sub = "✎ " + sub
		}
		if c.TLS != "" && c.TLS != config.TLSDisable {
			sub += " · tls"
		}
		it := listItem{label: c.Name, sub: sub, data: c.Name}
		switch {
		case on != "" && c.Name == on:
			it.mark = "●"
			m.conns.cur = i
		case others[c.Name]:
			// another query tab is on it: dbc web's dimmer bar, as a
			// hollow mark (tabs.go)
			it.mark = "○"
		}
		items = append(items, it)
	}
	m.conns.set(items)
}

// refreshTables rebuilds the tables list from the active connection's
// catalog, with the cursor back at the top: a new catalog is a new list.
//
// A filter typed over the old list goes with it: its text was a fragment
// of the old catalog's names. A line still being typed in stays open,
// empty, so the find can go on in the new list.
func (m *Model) refreshTables() {
	m.tables.cur = 0
	if f := m.tfind; f != nil {
		f.input.SetText("")
		if !f.editing {
			m.tfind = nil
		}
	}
	m.fillTables()
	m.cursorToFirstName()
}

// cursorToFirstName puts the tables cursor on the list's first name — a
// table, view or routine — rather than the Tables header above it, so
// Enter on a fresh or just-narrowed list takes its first table, as it did
// before the sections. A list of headers alone leaves it where it is.
func (m *Model) cursorToFirstName() {
	for i, it := range m.tables.items {
		if !it.head && !isSectionHeader(it) {
			m.tables.cur = i
			return
		}
	}
}

// relistTables rebuilds the tables list from a re-read of the same
// connection's catalog (a Refresh), keeping the cursor on the table it was
// on. By name, not by row: a table created or dropped above it shifts the
// rows, and the cursor would otherwise land on a neighbour. With that table
// gone, the cursor stays at the row number it had (set clamps it).
func (m *Model) relistTables() {
	was := ""
	if it, ok := m.tables.current(); ok {
		was = itemKey(it) // a table's name, or a routine's signature (routines.go)
	}
	m.fillTables()
	if was == "" {
		// nothing was under the cursor — the list was empty, as before the
		// first connect lands — so start on the first table, not a header
		m.cursorToFirstName()
	}
	for i, it := range m.tables.items {
		if was != "" && itemKey(it) == was {
			m.tables.cur = i
			break
		}
	}
	m.tables.ensureVisible()
}

// fillTables (re)draws the tables list's rows from the active connection's
// catalog: table_schema · table_name · table_type, per db.TablesQuery, and
// the row counts once they land. It leaves the cursor where it is, so the
// counts filling in seconds after a connect do not yank the selection.
//
// Schema is shown only when the connection has more than one, since
// "public." in front of every name says nothing.
//
// A count — only while the row counts are on (toggleRowCounts) — goes in
// the row's right-aligned muted slot ("cats      1,234")
// rather than after the name: a long name is truncated from its end, and
// the count is what would be cut. Views have no count (see db/rowcount.go);
// they are listed in a section of their own, so need no "view" mark.
func (m *Model) fillTables() {
	r := m.ws.Catalog()
	if r == nil || len(r.Columns) < 2 {
		m.setTableRows(nil)
		return
	}
	// three sections — tables, views, routines — each under its header
	// (sections.go)
	m.setTableRows(m.catalogItems(r)) // through the pane's filter (tablefind.go)
}

// toggleRowCounts is the tables list's # key (and its menu's "Show row
// counts"): the TUI's counterpart of the web sidebar's "rows" box. Counts
// start off, since each is an exact count(*), a scan of the table (see
// db/rowcount.go). On, the counting runs as a Job and lands as a
// *workspace.RowCounts like a connect's; off, the numbers go at once.
func (m *Model) toggleRowCounts() tea.Cmd {
	on := !m.ws.RowCountsShown()
	j := m.ws.ShowRowCounts(on)
	m.fillTables() // off: the numbers go now; on: the list stays as it is until they land
	switch {
	case !on:
		m.setStatus("row counts off")
	case j != nil:
		m.setStatus("counting rows…")
	default:
		m.setStatus("row counts on — they show once tables are listed")
	}
	return m.tag(job(j))
}

// rowCountsLanded fills the counts into the tables list. Counts are exact
// (db/rowcount.go); RowCount.Short would mark an estimated one "~", should
// one ever come.
func (m *Model) rowCountsLanded(ev *workspace.RowCounts) tea.Cmd {
	if ev.Stale {
		return nil
	}
	m.notes(ev.Notes)
	m.fillTables()
	return nil
}

// listKey drives a sidebar list from the keyboard; Enter calls pick.
func (m *Model) listKey(l *list, k tea.KeyPressMsg, pick func() tea.Cmd) tea.Cmd {
	if _, picked := l.key(k); picked {
		return pick()
	}
	return nil
}

// connPicked connects to the connection under the cursor.
func (m *Model) connPicked() tea.Cmd {
	it, ok := m.conns.current()
	if !ok {
		return nil
	}
	return m.setActive(it.data.(string))
}

// tablePicked previews the table under the cursor. The statement goes into
// the history like a typed one — it is what the user would have typed — but
// NOT into the editor, whose contents are the user's.
//
// On a routine the pick is its DDL instead, and on a section's header
// the pick folds it (sections.go). A view previews as a table does: it is
// queried as one, and its DDL is the menu's.
func (m *Model) tablePicked() tea.Cmd {
	if s, ok := m.currentSection(); ok {
		return m.toggleSection(s)
	}
	if _, ok := m.currentRoutine(); ok {
		return m.routinePicked()
	}
	name, ok := m.currentTable()
	if !ok {
		return nil
	}
	stmt := fmt.Sprintf("SELECT * FROM %s LIMIT 100", name)
	m.focus = focusGrid
	return m.startRun(m.ws.RunStmts([]string{stmt}, "preview "+name)) // records it, then runs
}

// tableColumns lists the information_schema.columns rows of the table under
// the cursor in the grid, where the grid's copies take them out as TSV,
// Markdown or HTML. Recorded in the history like a preview; see
// workspace.ShowColumns.
func (m *Model) tableColumns() tea.Cmd {
	if m.tableOnly("Show columns") {
		return nil
	}
	name, ok := m.currentTable()
	if !ok {
		return nil
	}
	m.focus = focusGrid
	return m.startRun(m.ws.ShowColumns(name))
}
