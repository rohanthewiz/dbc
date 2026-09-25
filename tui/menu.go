package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// menu is a floating list of actions: every right-click menu, the ⧉ Copy
// dropdown, and the connection picker.
//
// Following cats-todo's grammar for context menus:
//   - The click that opens a menu first moves the cursor onto what was
//     clicked, so the menu and the keyboard act on the same thing.
//   - Rows that cannot run right now stay VISIBLE, drawn dim, and picking one
//     says why in the log instead of silently doing nothing — a silent no-op
//     reads exactly like a click the terminal dropped.
//   - Separators are headings, not just lines, when a group needs a name
//     ("copy result as").
type menu struct {
	items []menuItem
	cur   int
	rect  Rect // as last drawn; items occupy rows rect.Y+1 …
	x, y  int  // where it was opened
}

// menuItem is one row.
type menuItem struct {
	label string
	key   string               // key hint, drawn right-aligned
	act   func(*Model) tea.Cmd // nil for a heading
	why   string               // non-empty: disabled, and this says why
	head  bool                 // a heading/separator row, not pickable
}

func heading(label string) menuItem { return menuItem{label: label, head: true} }

// openMenu shows items at (x, y), with the cursor on the first pickable row.
func (m *Model) openMenu(x, y int, items []menuItem) {
	mn := &menu{items: items, x: x, y: y, cur: -1}
	mn.step(1)
	m.menu = mn
}

// step moves the cursor to the next pickable row in direction d.
func (mn *menu) step(d int) {
	n := len(mn.items)
	for i := 1; i <= n; i++ {
		j := ((mn.cur+d*i)%n + n) % n
		if !mn.items[j].head {
			mn.cur = j
			return
		}
	}
}

// itemAt returns the item index drawn at screen row y, or -1.
func (mn *menu) itemAt(y int) int {
	i := y - mn.rect.Y - 1
	if i < 0 || i >= len(mn.items) || mn.items[i].head {
		return -1
	}
	return i
}

// menuKey handles keys while a menu is open. Anything it does not know
// closes the menu, the way Esc would, so a stray key never leaves a menu
// floating over what the user went back to.
func (m *Model) menuKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "up", "k", "shift+tab":
		m.menu.step(-1)
	case "down", "j", "tab":
		m.menu.step(1)
	case "enter", "space":
		return m.menuPick(m.menu.cur)
	default:
		m.menu = nil
	}
	return nil
}

// menuPick runs item i and closes the menu.
func (m *Model) menuPick(i int) tea.Cmd {
	mn := m.menu
	m.menu = nil
	if mn == nil || i < 0 || i >= len(mn.items) {
		return nil
	}
	it := mn.items[i]
	if it.head || it.act == nil {
		return nil
	}
	if it.why != "" {
		m.log(logWarn, it.why)
		return nil
	}
	return it.act(m)
}

// draw paints the menu, placed below-right of where it was opened and
// flipped up or left when that would run off the screen.
func (mn *menu) draw(c *Canvas, st styles) {
	w := 0
	for _, it := range mn.items {
		iw := width(it.label) + 4
		if it.key != "" {
			iw += width(it.key) + 3
		}
		w = max(w, iw)
	}
	w = min(w, c.W)
	h := min(len(mn.items)+2, c.H)
	x, y := mn.x, mn.y
	if x+w > c.W {
		x = max(c.W-w, 0)
	}
	if y+h > c.H {
		y = max(mn.y-h+1, 0)
	}
	mn.rect = Rect{x, y, w, h}

	s := c.Sub(mn.rect)
	bg := st.raised
	s.Fill(bg)
	s.Box(onBg(st.borderFocus, bg), "", bg)
	for i, it := range mn.items {
		row := s.Sub(Rect{1, i + 1, w - 2, 1})
		switch {
		case it.head:
			label := " " + it.label + " "
			if it.label == "" {
				label = ""
			}
			rule := strings.Repeat("─", max(row.W(), 0))
			row.Put(0, 0, rule, onBg(st.border, bg))
			row.Put(1, 0, label, onBg(st.muted, bg).Italic())
		default:
			rs := bg
			if i == mn.cur {
				rs = st.buttonHover
			}
			if it.why != "" {
				rs = rs.WithFg(st.muted.Fg).Dim()
			}
			row.Fill(rs)
			row.Put(1, 0, it.label, rs)
			if it.key != "" {
				ks := rs
				if i != mn.cur {
					ks = rs.WithFg(st.muted.Fg)
				}
				row.PutRight(row.W()-1, 0, it.key, ks)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The menus
// ---------------------------------------------------------------------------

// noResult is the reason copy/export rows are dim before anything has run.
const noResult = "nothing to copy yet — run a query first"

// copyItems lists the formats a copy can take, for a scope that is either
// the selection or the whole result.
func copyItems(whole bool, why string) []menuItem {
	mk := func(label string, f copyFormat) menuItem {
		return menuItem{label: label, why: why, act: func(m *Model) tea.Cmd { return m.copyGrid(f, whole) }}
	}
	return []menuItem{
		mk("Table for Teams / Outlook / Docs (HTML)", copyHTML),
		mk("Markdown table", copyMarkdown),
		mk("CSV", copyCSV),
		mk("TSV (pastes into a spreadsheet)", copyTSV),
		mk("JSON", copyJSON),
	}
}

// openGridMenu is the results pane's context menu.
func (m *Model) openGridMenu(x, y int) {
	why := ""
	if m.ws.LastResult() == nil {
		why = noResult
	}
	g := m.grid
	var items []menuItem
	if g.sel {
		r0, c0, r1, c1 := g.bounds()
		items = append(items,
			menuItem{label: fmt.Sprintf("Copy %d×%d cells", r1-r0+1, c1-c0+1), key: "y", why: why,
				act: func(m *Model) tea.Cmd { return m.copyGrid(copyText, false) }},
			heading("copy selection as"))
		items = append(items, copyItems(false, why)...)
	} else {
		items = append(items,
			menuItem{label: "Copy value", key: "y", why: why,
				act: func(m *Model) tea.Cmd { return m.copyGrid(copyText, false) }},
			menuItem{label: "Copy row", key: "Y", why: why,
				act: func(m *Model) tea.Cmd { r, w := m.grid.RowResult(); return m.copyResult(r, w, copyText) }},
			menuItem{label: "Inspect value", key: "Enter", why: why,
				act: func(m *Model) tea.Cmd { m.openInspect(); return nil }})
	}
	items = append(items, heading("copy whole result as"))
	items = append(items, copyItems(true, why)...)
	items = append(items,
		heading(""),
		menuItem{label: "Sort by this column", why: why,
			act: func(m *Model) tea.Cmd { m.grid.Sort(m.grid.cur.col); return nil }},
	)
	items = append(items, columnItems(g, why)...)
	if m.planv.plan != nil {
		items = append(items, heading(""),
			menuItem{label: "◈ Show the plan", key: "p", act: func(m *Model) tea.Cmd { m.resTab = tabPlan; return nil }})
	}
	items = append(items,
		heading(""),
		menuItem{label: "Export to file…", key: "^E", why: why,
			act: func(m *Model) tea.Cmd { m.openExport(); return nil }},
		menuItem{label: "✦ Ask the assistant about this result", key: "^A", why: why,
			act: func(m *Model) tea.Cmd { return m.askAbout("Explain this result — anything notable in it?") }},
	)
	m.openMenu(x, y, items)
}

// maxShowItems bounds the per-column "Show …" rows, so hiding forty columns
// of a wide table does not grow a menu taller than the screen; "Show all"
// is always there for the rest.
const maxShowItems = 8

// columnItems are the grid menu's column rows: hide the cursor's column (or
// the range's columns), fit its width, and bring hidden ones back — each by
// name, since after hiding several the user remembers names, not positions.
func columnItems(g *grid, why string) []menuItem {
	_, c0, _, c1 := g.bounds()
	hide := "Hide column " + g.colName(g.cur.col)
	if g.sel && c1 > c0 {
		hide = "Hide " + plural(c1-c0+1, "column")
	}
	hideWhy := why
	if hideWhy == "" && c1-c0+1 >= g.Cols() {
		hideWhy = "can't hide every column — at least one has to stay"
	}
	items := []menuItem{
		{label: hide, key: "-", why: hideWhy,
			act: func(m *Model) tea.Cmd { m.hideColumns(); return nil }},
		{label: "Fit column to its content", key: "=", why: why,
			act: func(m *Model) tea.Cmd { m.grid.Fit(m.grid.cur.col); return nil }},
	}
	hidden := g.HiddenCols()
	if len(hidden) == 0 {
		return items
	}
	items = append(items, heading("hidden columns"))
	for i, rc := range hidden {
		if i == maxShowItems {
			items = append(items, menuItem{label: fmt.Sprintf("  … and %d more", len(hidden)-i), why: "use Show all"})
			break
		}
		items = append(items, menuItem{label: "Show " + g.res.Columns[rc],
			act: func(m *Model) tea.Cmd { m.grid.Show(rc); return nil }})
	}
	return append(items, menuItem{label: "Show all columns", key: "+",
		act: func(m *Model) tea.Cmd { m.showAllColumns(); return nil }})
}

// openCopyMenu is the ⧉ Copy toolbar dropdown: the whole result, by format.
func (m *Model) openCopyMenu(x, y int) {
	why := ""
	if m.ws.LastResult() == nil {
		why = noResult
	}
	items := []menuItem{heading("copy result as")}
	items = append(items, copyItems(!m.grid.sel, why)...)
	if m.grid.sel {
		items[0] = heading("copy selection as")
		items = append(items, heading("copy whole result as"))
		items = append(items, copyItems(true, why)...)
	}
	m.openMenu(x, y, items)
}

// openEditorMenu is the editor's context menu.
func (m *Model) openEditorMenu(x, y int) {
	sel, _, _ := m.editor.Selection()
	noSel := ""
	if sel == "" {
		noSel = "nothing is selected"
	}
	stmts, tag := m.stmtsToRun()
	runWhy := ""
	if len(stmts) == 0 {
		runWhy = "nothing to run — type a query first"
	}
	// Run all is offered only when it differs from Run: with one statement
	// in the buffer, or a selection already covering all of them, the two
	// rows would do the same thing.
	allStmts, allLabel := m.allStmts()
	allWhy := ""
	switch {
	case len(allStmts) == 0:
		allWhy, allLabel = "nothing to run — type a query first", "all"
	case len(allStmts) == 1:
		allWhy, allLabel = "the buffer holds one statement — ^R runs it", "all"
	case slices.Equal(allStmts, stmts):
		allWhy = "the selection already covers every statement"
	}
	m.openMenu(x, y, []menuItem{
		{label: "▶ Run " + orDefault(tag, "statement"), key: "^R", why: runWhy,
			act: func(m *Model) tea.Cmd { return m.runQuery() }},
		{label: "▶ Run " + allLabel, key: "^⇧R", why: allWhy,
			act: func(m *Model) tea.Cmd { return m.runAll() }},
		{label: "◈ Explain " + orDefault(tag, "statement"), key: "^X", why: runWhy,
			act: func(m *Model) tea.Cmd { return m.explainQuery(false) }},
		{label: "◈ Explain analyze (runs it, timed)", key: "⌥X", why: runWhy,
			act: func(m *Model) tea.Cmd { return m.explainQuery(true) }},
		heading(""),
		{label: "Copy", why: noSel, act: func(m *Model) tea.Cmd {
			s, _, _ := m.editor.Selection()
			return m.copyString(s, "the selection")
		}},
		{label: "Cut", why: noSel, act: func(m *Model) tea.Cmd {
			s, _, _ := m.editor.Selection()
			m.editor.Backspace()
			return m.copyString(s, "the selection")
		}},
		{label: "Select all", key: "⌥A", act: func(m *Model) tea.Cmd { m.editor.SelectAll(); return nil }},
		{label: "Undo", key: "^Z", act: func(m *Model) tea.Cmd { m.editor.Undo(); return nil }},
		heading(""),
		{label: "History…", key: "^P", act: func(m *Model) tea.Cmd { m.openHistory(); return nil }},
		{label: "✦ Ask the assistant about this query", key: "^A", why: runWhy,
			act: func(m *Model) tea.Cmd { return m.askAbout("Explain this query.") }},
	})
}

// openConnMenu lists the connections; picking one connects.
func (m *Model) openConnMenu(x, y int) {
	items := []menuItem{heading("connect to")}
	for _, c := range m.cfg.Connections {
		name := c.Name
		label := "  " + name
		if name == m.ws.Active() {
			label = "● " + name
		}
		items = append(items, menuItem{label: label, key: c.Driver,
			act: func(m *Model) tea.Cmd { return m.setActive(name) }})
	}
	m.openMenu(x, y, items)
}

// openTableMenu is the tables list's context menu.
func (m *Model) openTableMenu(x, y int) {
	it, ok := m.tables.current()
	if !ok {
		return
	}
	name := it.data.(string)
	m.openMenu(x, y, []menuItem{
		{label: "Preview rows", key: "2×click", act: func(m *Model) tea.Cmd { return m.tablePicked() }},
		{label: "Insert name at the caret", act: func(m *Model) tea.Cmd {
			m.editor.Insert(name)
			m.focus = focusEditor
			return nil
		}},
		{label: "Copy name", act: func(m *Model) tea.Cmd { return m.copyString(name, "the table name") }},
	})
}

// openLogMenu is the log's context menu.
func (m *Model) openLogMenu(x, y int) {
	m.openMenu(x, y, []menuItem{
		{label: "Copy log", act: func(m *Model) tea.Cmd { return m.copyString(m.logp.Text(), "the log") }},
		{label: "Clear log", act: func(m *Model) tea.Cmd { m.logp.lines = nil; return nil }},
	})
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
