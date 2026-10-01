package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/db"
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
// ("<conn>/<database>"), the configured connection it is derived from.
func (m *Model) refreshConns() {
	items := make([]listItem, 0, len(m.cfg.Connections))
	on := m.baseOf(m.ws.Active())
	for i, c := range m.cfg.Connections {
		it := listItem{label: c.Name, sub: c.Driver, data: c.Name}
		if on != "" && c.Name == on {
			it.mark = "●"
			m.conns.cur = i
		}
		items = append(items, it)
	}
	m.conns.set(items)
}

// refreshTables rebuilds the tables list from the active connection's
// catalog, with the cursor back at the top: a new catalog is a new list.
func (m *Model) refreshTables() {
	m.tables.cur = 0
	m.fillTables()
}

// fillTables (re)draws the tables list's rows from the active connection's
// catalog: table_schema · table_name · table_type, per db.TablesQuery, and
// the row counts once they land. It leaves the cursor where it is, so the
// counts filling in seconds after a connect do not yank the selection.
//
// Schema is shown only when the connection has more than one, since
// "public." in front of every name says nothing.
//
// A count goes in the row's right-aligned muted slot ("cats      1,234")
// rather than after the name: a long name is truncated from its end, and
// the count is what would be cut. Views have no count (see db/rowcount.go)
// and keep "view" there instead.
func (m *Model) fillTables() {
	r := m.ws.Catalog()
	if r == nil || len(r.Columns) < 2 {
		m.tables.set(nil)
		return
	}
	counts := m.ws.RowCounts()
	// The index's Display decides qualification, as the web's list and the
	// assistant do: by the database's schemas, not just the listed ones,
	// since a list of one schema of many still needs schema.name to work
	// off the search_path.
	idx := m.ws.TableIndex()
	items := make([]listItem, 0, len(r.Rows))
	for _, ref := range db.TableRefs(r.Rows) {
		qualified := idx.Display(ref)
		it := listItem{label: qualified, data: qualified}
		if ref.View {
			it.sub, it.muted = "view", true
		} else if c, ok := counts[ref]; ok {
			it.sub = c.Short()
		}
		items = append(items, it)
	}
	m.tables.set(items)
}

// rowCountsLanded fills the counts into the tables list. An estimated count
// is marked "~" by RowCount.Short, which is all the TUI has room to say; the
// web page's tooltip says the rest.
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
func (m *Model) tablePicked() tea.Cmd {
	it, ok := m.tables.current()
	if !ok {
		return nil
	}
	stmt := fmt.Sprintf("SELECT * FROM %s LIMIT 100", it.data.(string))
	m.focus = focusGrid
	return m.startRun(m.ws.RunStmts([]string{stmt}, "preview "+it.label)) // records it, then runs
}

// tableColumns lists the information_schema.columns rows of the table under
// the cursor in the grid, where the grid's copies take them out as TSV,
// Markdown or HTML. Recorded in the history like a preview; see
// workspace.ShowColumns.
func (m *Model) tableColumns() tea.Cmd {
	it, ok := m.tables.current()
	if !ok {
		return nil
	}
	m.focus = focusGrid
	return m.startRun(m.ws.ShowColumns(it.data.(string)))
}
