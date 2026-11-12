package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// The Tables pane's type-to-find: / opens a filter line over the list, and
// what is typed there narrows it — the TUI's counterpart of dbc web's table
// box under the db/schema pickers. The pane's letters are already keys
// (j k g G c e d s f #), so finding needs a door of its own; / is the one
// less, vim and the browser's find use.
//
//	┌ Tables · 3 of 40 ─────┐
//	│ ◫ sales · 40        ▾ │   the navigator's rows, as before
//	│ ⌕ ord▏                │   the filter line, while open or holding text
//	│ orders                │
//	│ order_items           │   rows whose name holds "ord", any case
//	│ backorders            │
//
// While the line is being typed in, the pane's letters are text: ↑↓ and
// PgUp/PgDn still move the cursor, Enter previews the row under it (a
// routine's DDL, with the routines listed) and leaves the filter applied,
// and Esc clears it. Off the line, / goes back into it and Esc clears it.
// A clear keeps the cursor on the row it was on, so a find followed by Esc
// is a jump: the table found, in the whole list around it.
//
// The filter sits between the catalog and the list rather than in the
// list: fillTables and fillRoutines hand it every row (setTableRows) and
// it keeps them, so a keystroke re-narrows without re-reading the catalog,
// and the row counts landing seconds after a connect fill in under it.
// The filter matches anywhere in the name, as the pickers' (pickModal)
// does: a table is remembered by a fragment as often as by its start.
//
// It is per tab, as the list is (park/load in tabs.go), and nil until the
// first /, so a tab that never finds pays nothing for it. A new catalog —
// a connect, a schema or database pick, a disconnect — is a new list, and
// the text goes with it (refreshTables); a re-read of the same one keeps it.

// tableFind is the filter's state.
type tableFind struct {
	input   *editor
	editing bool       // keys go to the line, not the list
	all     []listItem // every row, before the filter
}

// matches is the rows of all whose label holds the filter's text,
// case-insensitively; all of them with no text.
//
// The sections' headers (sections.go) are not matched by their words: a
// header stays while its section has a match below it, so each match is
// still seen under the section it is in, and goes with a section that has
// none — a find that matches nothing leaves an empty list, which says
// "no match". A section's note ("no views") is not a match either.
//
// A folded section is searched too: its rows are in all, marked folded
// (sectionItems), and listed only while they match. Its header then
// "peeks" — a hollow ▿ in place of ▸ — so the matches do not read as
// sitting under a closed fold, and the workspace's fold is left alone:
// Esc puts the ▸ back, and Enter on the header opens it for good.
//
//	find "legac", Tables folded       Esc
//	▿ Tables          34              ▸ Tables          34
//	  legacy_orders                   ▾ Views            3
//	                                    …
//
// Folded Routines are the exception: nothing is read while they are
// folded, so there are no rows of theirs to search.
func (f *tableFind) matches() []listItem {
	q := strings.ToLower(strings.TrimSpace(f.input.Text()))
	if q == "" {
		return listedRows(f.all)
	}
	out := make([]listItem, 0, len(f.all))
	var header *listItem // the section heading the rows now scanned, until its first match
	for i, it := range f.all {
		switch {
		case isSectionHeader(it):
			header = &f.all[i]
		case it.head:
			// a note between the rows: never a match
		case strings.Contains(strings.ToLower(it.label), q):
			if header != nil {
				h := *header // a copy: the peek mark is this find's, not all's
				if it.folded {
					h.mark = "▿"
				}
				out = append(out, h)
				header = nil
			}
			out = append(out, it)
		}
	}
	return out
}

// listedRows is items as the pane lists them with no find: the rows of
// folded sections left out. It returns items itself when none is folded
// (the usual case), so an unfiltered fill allocates nothing.
func listedRows(items []listItem) []listItem {
	n := 0
	for _, it := range items {
		if it.folded {
			n++
		}
	}
	if n == 0 {
		return items
	}
	out := make([]listItem, 0, len(items)-n)
	for _, it := range items {
		if !it.folded {
			out = append(out, it)
		}
	}
	return out
}

// isSectionHeader reports whether a row is a section's header.
func isSectionHeader(it listItem) bool {
	_, ok := it.data.(sectionRow)
	return ok
}

// countNames is how many rows of items name something — a table, view or
// routine — leaving out the sections' headers and notes. A folded
// section's rows count only with folded true: they are not listed, but
// they are what a find searches, so its "3 of 40" counts them.
func countNames(items []listItem, folded bool) int {
	n := 0
	for _, it := range items {
		if !it.head && !isSectionHeader(it) && (folded || !it.folded) {
			n++
		}
	}
	return n
}

// active reports whether the filter line is up: being typed in, or holding
// text that narrows the list.
func (f *tableFind) active() bool {
	return f != nil && (f.editing || f.input.Text() != "")
}

// findEditing reports whether keys go to the Tables pane's filter line.
func (m *Model) findEditing() bool {
	return m.tfind != nil && m.tfind.editing && m.focus == focusTables
}

// setTableRows puts the catalog's rows in the Tables pane, through the
// filter when there is one. Every fill of the list goes through here.
// Without a filter the folded sections' rows are dropped (listedRows);
// with one they are kept in its all, for it to search.
func (m *Model) setTableRows(items []listItem) {
	if f := m.tfind; f != nil {
		f.all = items
		items = f.matches()
	} else {
		items = listedRows(items)
	}
	m.tables.set(items)
}

// tableRowsTotal is the number of names the pane would list unfiltered:
// the open sections' tables, views and routines, not their headers.
func (m *Model) tableRowsTotal() int {
	if m.tfind != nil {
		return countNames(m.tfind.all, false)
	}
	return countNames(m.tables.items, false)
}

// openTableFind is the Tables pane's /: into the filter line, made on the
// first use. The list on screen has lost its folded sections' rows
// (setTableRows), so the filter's all is filled from the catalog afresh —
// an in-memory rebuild, no read — rather than taken from the screen; with
// no text yet it lists the same rows, so the cursor stays put.
func (m *Model) openTableFind() {
	if m.tfind == nil {
		m.tfind = &tableFind{input: newEditor(true), all: m.tables.items}
		m.fillTables()
	}
	m.tfind.editing = true
	m.focus = focusTables
}

// refilterTables re-narrows the list after the filter's text changed. The
// cursor goes to the first match: what was under it may be gone, and the
// first match is what Enter is expected to take.
func (m *Model) refilterTables() {
	m.tables.cur, m.tables.top = 0, 0
	m.tables.set(m.tfind.matches())
	m.cursorToFirstName() // past the header the first match is under
}

// clearTableFind drops the filter: the whole list back, the cursor kept on
// the row it was on.
func (m *Model) clearTableFind() {
	if m.tfind == nil {
		return
	}
	was := ""
	if it, ok := m.tables.current(); ok {
		was = itemKey(it)
	}
	all := m.tfind.all
	m.tfind = nil
	m.tables.set(listedRows(all)) // the folds back: a peeking section's rows go
	m.cursorToKey(was)
}

// cursorToKey puts the tables list's cursor on the row named key (itemKey),
// if one is listed, and scrolls it into view.
func (m *Model) cursorToKey(key string) {
	if key == "" {
		return
	}
	for i, it := range m.tables.items {
		if itemKey(it) == key {
			m.tables.cur = i
			break
		}
	}
	m.tables.ensureVisible()
}

// tableFindKey handles a key while the filter line is being typed in.
func (m *Model) tableFindKey(k tea.KeyPressMsg) tea.Cmd {
	f := m.tfind
	switch k.String() {
	case "esc":
		m.clearTableFind()
		return nil
	case "enter":
		f.editing = false
		if f.input.Text() == "" {
			m.clearTableFind() // nothing typed: no filter to keep
		}
		if _, ok := m.tables.current(); !ok {
			return nil // no match: nothing to preview
		}
		return m.tablePicked()
	case "up", "down", "pgup", "pgdown", "ctrl+n":
		m.tables.key(k)
		return nil
	}
	before := f.input.Text()
	f.input.HandleKey(k)
	if f.input.Text() != before {
		m.refilterTables()
	}
	return nil
}

// tableFindPaste puts pasted text in the filter line.
func (m *Model) tableFindPaste(s string) {
	// a name pasted from elsewhere may carry the line's end with it
	m.tfind.input.Insert(strings.TrimSpace(strings.ReplaceAll(s, "\n", " ")))
	m.refilterTables()
}

// drawTableFind draws the filter line on row y of the Tables pane's body,
// and returns the caret while it is being typed in.
func (m *Model) drawTableFind(s Surface, y int) (Rect, *caret) {
	r := s.Sub(Rect{0, y, s.W(), 1})
	st := m.st.raised
	r.Fill(st)
	r.Put(1, 0, "⌕", st.WithFg(m.st.accent.Fg))
	// every section is searched, folded or not (matches): tables, views,
	// and routines while they are open (folded, they are not read)
	m.tfind.input.placeholder = "find by name"
	cx, cy, ok := m.tfind.input.Draw(r.Sub(Rect{3, 0, max(r.W()-4, 1), 1}), m.st, st, [2]int{}, true)
	if ok && m.findEditing() {
		return r.Rect(), &caret{cx, cy}
	}
	return r.Rect(), nil
}

// tablesCounted reports whether the Tables pane's title carries a count:
// while the pane lists names, or while a find searches some. The second
// is not implied by the first — with every section folded nothing is
// listed, yet a find under the folds (matches) has names to count.
func (m *Model) tablesCounted() bool {
	if m.tfind.active() && m.tfind.input.Text() != "" {
		return countNames(m.tfind.all, true) > 0
	}
	return m.tableRowsTotal() > 0
}

// tablesTitleCount is the Tables pane title's count of the names listed
// (headers left out): "40", or "3 of 40" while the filter narrows the
// list.
//
// Both sides count folded sections' rows (countNames' folded): the matches
// may be peeking from one, and "of" is every name the find searched, so it
// never reads "3 of 1" with the tables folded.
func (m *Model) tablesTitleCount() string {
	n := countNames(m.tables.items, true)
	if m.tfind.active() && m.tfind.input.Text() != "" {
		return fmt.Sprintf("%d of %d", n, countNames(m.tfind.all, true))
	}
	return fmt.Sprint(n)
}
