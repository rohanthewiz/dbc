package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/workspace"
)

// The Tables pane lists the catalog in three headed, foldable sections —
// tables, views, routines — rather than tables alone with views mixed in
// and routines behind a switch:
//
//	┌ Tables ───────────────┐
//	│ ◫ sales · 40        ▾ │   the navigator's rows, as before
//	│ ▾ Tables          34  │   a section's header: Enter, a click, folds it
//	│  customers      1.2k  │
//	│  orders          88k  │
//	│ ▸ Views            3  │   folded: its rows are not listed
//	│ ▾ Routines         2  │   f folds/unfolds it; opening it reads them
//	│  order_total      fn  │
//	│  archive        proc  │
//
// The headers are rows of the list, holding a sectionRow, so the cursor,
// the mouse and the filter treat them as the rows they head: the cursor
// lands on one and Enter folds it, a single click folds it (a disclosure
// triangle is clicked once), and the filter keeps a header while its
// section has a match. A section's note ("no views", "loading routines…")
// is a head row, which the cursor steps over.
//
// THE COST OF A FOLD (workspace/sections.go). Tables and views are the one
// catalog read a connect makes, so their folds read nothing. Routines are
// read only while their section is open: as a tab starts it is folded, and
// its header has no count, since none has been read to count.

// sectionRow is a section header's data in the list.
type sectionRow workspace.Section

// sectionItems are a section's rows: its header, then — while it is open —
// its rows, or a note when it has none. count is the header's number ("" for
// none known); empty is the note.
func sectionItems(s workspace.Section, title, count string, open bool, rows []listItem, empty string) []listItem {
	mark := "▸"
	if open {
		mark = "▾"
	}
	items := []listItem{{mark: mark, label: title, sub: count, data: sectionRow(s)}}
	if !open {
		return items
	}
	if len(rows) == 0 {
		return append(items, listItem{label: "  " + empty, head: true})
	}
	// a blank mark puts each name in the column its header's title starts
	// in (the list draws a mark, then a space), so the rows read as the
	// section's; the label itself stays the bare name the filter and the
	// actions use
	for _, it := range rows {
		if it.mark == "" {
			it.mark = " "
		}
		items = append(items, it)
	}
	return items
}

// catalogItems are the pane's rows: the three sections from the catalog
// (tables and views, split by their type) and the routines once read.
//
// Routines get a section only on a driver that stores them
// (db.HasRoutines); SQLite and bytdb show Tables and Views alone.
func (m *Model) catalogItems(r *model.Result) []listItem {
	counts := m.ws.RowCounts()
	// The index's Display decides qualification, as the web's list and the
	// assistant do: by the database's schemas, not just the listed ones,
	// since a list of one schema of many still needs schema.name to work
	// off the search_path.
	idx := m.ws.TableIndex()
	var tables, views []listItem
	for _, ref := range db.TableRefs(r.Rows) {
		qualified := idx.Display(ref)
		it := listItem{label: qualified, data: qualified}
		if ref.View {
			views = append(views, it)
			continue
		}
		if c, ok := counts[ref]; ok {
			it.sub = c.Short()
		}
		tables = append(tables, it)
	}
	ws := m.ws
	items := sectionItems(workspace.SectionTables, "Tables", fmt.Sprint(len(tables)),
		ws.SectionOpen(workspace.SectionTables), tables, "no tables")
	items = append(items, sectionItems(workspace.SectionViews, "Views", fmt.Sprint(len(views)),
		ws.SectionOpen(workspace.SectionViews), views, "no views")...)
	if cc, ok := m.cfg.ConnByName(ws.Active()); ok && db.HasRoutines(cc.Driver) {
		rs, count, empty := ws.Routines(), "", "no routines"
		switch {
		case rs != nil:
			count = fmt.Sprint(len(rs))
		case ws.RoutinesShown():
			empty = "loading routines…" // open, the read on its way
		}
		items = append(items, sectionItems(workspace.SectionRoutines, "Routines", count,
			ws.RoutinesShown(), routineItems(rs, len(ws.Schemas()) > 1), empty)...)
	}
	return items
}

// currentSection is the section whose header is under the cursor.
func (m *Model) currentSection() (workspace.Section, bool) {
	it, ok := m.tables.current()
	if !ok {
		return "", false
	}
	s, ok := it.data.(sectionRow)
	return workspace.Section(s), ok
}

// toggleSection folds or unfolds a section, leaving the cursor on its
// header so a second Enter folds it back. Opening Routines starts their
// read (toggleRoutines); the other two fold at once.
func (m *Model) toggleSection(s workspace.Section) tea.Cmd {
	if s == workspace.SectionRoutines {
		return m.toggleRoutines()
	}
	open := !m.ws.SectionOpen(s)
	m.ws.OpenSection(s, open)
	m.fillTables()
	m.cursorToKey(itemKey(listItem{data: sectionRow(s)}))
	word := map[workspace.Section]string{workspace.SectionTables: "tables", workspace.SectionViews: "views"}[s]
	if open {
		m.setStatus(word + " shown")
	} else {
		m.setStatus(word + " folded")
	}
	return nil
}

// sectionLabel is a menu row's words for a section's fold: what picking it
// does.
func (m *Model) sectionLabel(s workspace.Section, name string) string {
	if m.ws.SectionOpen(s) {
		return "Fold " + name
	}
	return "Show " + name
}

// currentView is the view under the cursor, as the catalog names it; ok
// false on a table, a routine or a header.
func (m *Model) currentView() (db.TableRef, bool) {
	name, ok := m.currentTable()
	if !ok {
		return db.TableRef{}, false
	}
	ref, ok := m.ws.TableIndex().Lookup(name)
	if !ok || !ref.View {
		return db.TableRef{}, false
	}
	return ref, true
}

// viewDDLPicked reads the definition of the view under the cursor; it
// opens in the DDL viewer when it lands (viewDDLLanded).
func (m *Model) viewDDLPicked() tea.Cmd {
	v, ok := m.currentView()
	if !ok {
		m.logf(logWarn, "Show DDL is for a view or a routine — move onto one")
		return nil
	}
	st, err := m.ws.ViewDDL(v)
	if err != nil {
		m.refused(err)
		return nil
	}
	m.notes(st.Notes)
	return m.tag(job(st.Job))
}

// viewDDLLanded shows a view's definition, or leaves the log's note saying
// why there is none.
func (m *Model) viewDDLLanded(ev *workspace.ViewDDL) tea.Cmd {
	m.notes(ev.Notes)
	if ev.Err != nil {
		return nil
	}
	m.openModal(newViewDDLModal(ev.View, ev.DDL))
	return nil
}
