package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// The sidebar: connections on top, the active connection's tables below.
//
// One click on a connection connects to it — in a mouse-first UI a
// connection list is a switcher, and "select, then press Enter" is a
// keyboard idiom. The tables list is a catalog: one click selects, a
// double-click previews the rows, and right-click offers the rest.

// refreshConns rebuilds the connections list, marking the active one.
func (m *Model) refreshConns() {
	items := make([]listItem, 0, len(m.cfg.Connections))
	for i, c := range m.cfg.Connections {
		it := listItem{label: c.Name, sub: c.Driver, data: c.Name}
		if c.Name == m.ws.Active() {
			it.mark = "●"
			m.conns.cur = i
		}
		items = append(items, it)
	}
	m.conns.set(items)
}

// refreshTables rebuilds the tables list from the active connection's
// catalog: table_schema · table_name · table_type, per db.TablesQuery.
// Schema is shown only when the connection has more than one, since
// "public." in front of every name says nothing.
func (m *Model) refreshTables() {
	r := m.ws.Catalog()
	if r == nil || len(r.Columns) < 2 {
		m.tables.set(nil)
		return
	}
	schemas := map[string]bool{}
	for _, row := range r.Rows {
		schemas[row[0]] = true
	}
	items := make([]listItem, 0, len(r.Rows))
	for _, row := range r.Rows {
		name := row[1]
		qualified := name
		if len(schemas) > 1 && row[0] != "" {
			qualified = row[0] + "." + name
		}
		it := listItem{label: qualified, data: qualified}
		if len(row) > 2 && strings.Contains(strings.ToUpper(row[2]), "VIEW") {
			it.sub, it.muted = "view", true
		}
		items = append(items, it)
	}
	m.tables.cur = 0
	m.tables.set(items)
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
