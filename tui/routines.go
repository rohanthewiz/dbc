package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/workspace"
)

// The Tables pane's other list: the stored functions and procedures of the
// schema it lists, and each one's DDL. f flips the pane between the two;
// the database and schema rows above stay, and pick for both:
//
//	┌ Tables · 40 ──────────┐   f   ┌ Routines · 3 ─────────┐
//	│ ◫ sales · 40        ▾ │  ───► │ ◫ sales · 40        ▾ │
//	│ orders                │  ◄─── │ order_total        fn │
//	│ …                     │       │ archive          proc │
//
// The switch is the tab's workspace's (Workspace.ShowRoutines), as the row
// counts' is, so each tab keeps its own, and while it is on every connect,
// refresh and schema pick reads the routines again (a *RoutinesLoaded,
// after the tables have drawn). Enter or a double-click on a routine reads
// its definition (Workspace.RoutineDDL) into a viewer (ddlModal), from
// which it can be copied or put in the editor.
//
// The list is m.tables itself, its rows holding a model.Routine where a
// table's hold its name: one pane, one cursor and one set of mouse paths,
// with the handful of table-only actions (preview, columns, diagram)
// refused in words on a routine rather than misread.

// routinesShown reports whether the Tables pane lists routines.
func (m *Model) routinesShown() bool { return m.ws.RoutinesShown() }

// toggleRoutines is the Tables pane's f key (and its menu's "Show
// routines" / "Show tables"). On, the routines are read as a Job and land
// as a *workspace.RoutinesLoaded; until then the list says "loading…".
func (m *Model) toggleRoutines() tea.Cmd {
	cc, ok := m.cfg.ConnByName(m.ws.Active())
	on := !m.routinesShown()
	if on && ok && !db.HasRoutines(cc.Driver) {
		m.logf(logWarn, "%s stores no functions or procedures", cc.Driver)
		return nil
	}
	j := m.ws.ShowRoutines(on)
	m.tables.cur, m.tables.top = 0, 0 // the other list: start at its top
	m.fillTables()
	if on {
		m.setStatus("routines — Enter shows one's DDL, f goes back to the tables")
	} else {
		m.setStatus("tables")
	}
	return m.tag(job(j))
}

// noRoutinesWhy is why the menu's "Show routines" is dim: no connection,
// or a driver that stores none; "" when it is live.
func (m *Model) noRoutinesWhy() string {
	cc, ok := m.cfg.ConnByName(m.ws.Active())
	switch {
	case !ok:
		return "connect first"
	case !db.HasRoutines(cc.Driver):
		return cc.Driver + " stores no functions or procedures"
	}
	return ""
}

// routineKind is a routine's kind as the list's muted right-hand slot
// shows it: short, since the sidebar is narrow.
func routineKind(k model.RoutineKind) string {
	switch k {
	case model.RoutineFunction:
		return "fn"
	case model.RoutineProcedure:
		return "proc"
	case model.RoutineAggregate:
		return "agg"
	case model.RoutineWindow:
		return "window"
	case model.RoutineTrigger:
		return "trigger"
	}
	return string(k)
}

// routineItems are the list rows for routines.
//
// Names are qualified by schema on the tables' rule — when the database
// has several schemas (the routines of one of them still need the prefix
// to be called from off the search_path) — or when the routines listed
// span several, as every schema's do.
//
// Postgres overloads share a name; each is its own row, told apart by its
// argument list in the row's muted desc. A name with no overloads shows
// none, since the arguments of every routine would crowd the narrow pane
// and its name is what one scans for.
func routineItems(rs []model.Routine, manySchemas bool) []listItem {
	if !manySchemas && len(rs) > 0 {
		for _, r := range rs[1:] {
			if r.Schema != rs[0].Schema {
				manySchemas = true
				break
			}
		}
	}
	label := func(r model.Routine) string {
		if manySchemas {
			return r.QName()
		}
		return r.Name
	}
	seen := map[string]int{}
	for _, r := range rs {
		seen[r.QName()]++
	}
	items := make([]listItem, 0, len(rs))
	for _, r := range rs {
		it := listItem{label: label(r), sub: routineKind(r.Kind), data: r}
		if seen[r.QName()] > 1 {
			it.desc = "(" + r.Args + ")"
		}
		items = append(items, it)
	}
	return items
}

// fillRoutines is fillTables for the routines list.
func (m *Model) fillRoutines() {
	m.setTableRows(routineItems(m.ws.Routines(), len(m.ws.Schemas()) > 1))
}

// routinesLanded draws the routines a Routines job read, keeping the
// cursor on the routine it was on (relistTables), since a relist after a
// run's CREATE FUNCTION lands this way too.
func (m *Model) routinesLanded(ev *workspace.RoutinesLoaded) tea.Cmd {
	if ev.Stale {
		return nil
	}
	m.notes(ev.Notes)
	if m.routinesShown() {
		m.relistTables()
	}
	return nil
}

// itemKey names a Tables pane row across a re-read, for relistTables: a
// table by its name, a routine by its oid (Postgres) and signature, so an
// overload keeps the cursor rather than its neighbour.
func itemKey(it listItem) string {
	switch d := it.data.(type) {
	case string:
		return d
	case model.Routine:
		return "\x00routine " + d.ID + " " + d.QName() + "(" + d.Args + ")"
	}
	return ""
}

// currentRoutine is the routine under the Tables pane's cursor.
func (m *Model) currentRoutine() (model.Routine, bool) {
	it, ok := m.tables.current()
	if !ok {
		return model.Routine{}, false
	}
	r, ok := it.data.(model.Routine)
	return r, ok
}

// currentTable is the table under the Tables pane's cursor: "" with none,
// or with the routines listed.
func (m *Model) currentTable() (string, bool) {
	it, ok := m.tables.current()
	if !ok {
		return "", false
	}
	name, ok := it.data.(string)
	return name, ok
}

// tableOnly refuses a table's action (preview, columns, diagram) while
// the pane lists routines, saying how to get the tables back. It reports
// whether it did.
func (m *Model) tableOnly(what string) bool {
	if !m.routinesShown() {
		return false
	}
	m.logf(logWarn, "%s is for tables — f lists them again", what)
	return true
}

// routinePicked reads the definition of the routine under the cursor; it
// opens in a viewer when it lands (ddlLanded).
func (m *Model) routinePicked() tea.Cmd {
	r, ok := m.currentRoutine()
	if !ok {
		return nil
	}
	st, err := m.ws.RoutineDDL(r)
	if err != nil {
		m.refused(err)
		return nil
	}
	m.notes(st.Notes)
	return m.tag(job(st.Job))
}

// ddlLanded shows a routine's definition, or leaves the log's note saying
// why there is none.
func (m *Model) ddlLanded(ev *workspace.RoutineDDL) tea.Cmd {
	m.notes(ev.Notes)
	if ev.Err != nil {
		return nil
	}
	m.openModal(newDDLModal(ev.Routine, ev.DDL))
	return nil
}

// openRoutineMenu is the Tables pane's context menu while it lists
// routines. onRow says the click landed on one.
func (m *Model) openRoutineMenu(x, y int, onRow bool) {
	r, ok := m.currentRoutine()
	noRoutine := ""
	if !ok || !onRow {
		noRoutine = "right-click a routine for this"
	}
	name := r.Name
	if len(m.ws.Schemas()) > 1 {
		name = r.QName()
	}
	items := []menuItem{
		{label: "Show DDL", key: "Enter", why: noRoutine, act: func(m *Model) tea.Cmd { return m.routinePicked() }},
		{label: "Insert name at the caret", why: noRoutine, act: func(m *Model) tea.Cmd {
			m.editor.Insert(name)
			m.focus = focusEditor
			return nil
		}},
		{label: "Copy name", why: noRoutine, act: func(m *Model) tea.Cmd { return m.copyString(name, "the routine name") }},
		{label: "Show tables", key: "f", act: func(m *Model) tea.Cmd { return m.toggleRoutines() }},
		{label: "Find a routine…", key: "/", act: func(m *Model) tea.Cmd { m.openTableFind(); return nil }},
	}
	m.openMenu(x, y, append(items, m.navMenuItems()...))
}

// ---------------------------------------------------------------------------
// The DDL viewer
// ---------------------------------------------------------------------------

// ddlModal shows a routine's definition in full, colored as the editor
// colors SQL, with the two things one does with it: copy it, or put it in
// the editor (at the caret, as the history's recall does — the editor's
// contents are the user's, so nothing there is replaced).
//
// Long lines wrap rather than run off the dialog: a function body is
// read, not scanned. The wrap is a hard cut at the dialog's width,
// counted in runes, so a chunk's bytes map straight back onto the
// highlighter's per-byte kinds (codeKinds); a word-wrap would move text
// between lines and the colors with it.
type ddlModal struct {
	modalBase
	routine  model.Routine
	ddl      string // as the server rendered it: what is copied and inserted
	shown    string // the same with tabs expanded, which the canvas would draw as dots
	top      int
	view     Rect
	copyBtn  Rect
	insBtn   Rect
	hoverBtn int // the button under the mouse: 0 copy, 1 insert, -1 none
}

func newDDLModal(r model.Routine, ddl string) *ddlModal {
	return &ddlModal{routine: r, ddl: ddl, shown: strings.ReplaceAll(ddl, "\t", "    "), hoverBtn: -1}
}

func (d *ddlModal) title() string { return "DDL · " + d.routine.QName() }

// size fits the dialog to the definition, as the cell inspector does: up
// to the widest line, within most of the screen, and scrolling past that.
func (d *ddlModal) size(w, h int) (int, int) {
	lines := strings.Split(d.shown, "\n")
	widest := 0
	for _, l := range lines {
		widest = max(widest, width(l))
	}
	mw := max(60, min(widest+4, w*4/5))
	return mw, max(10, min(len(lines)+6, h*4/5))
}

// ddlLine is one drawn row: a run of the definition's bytes, [from, to).
type ddlLine struct{ from, to int }

// lines cuts the definition into rows at most w runes wide.
func (d *ddlModal) lines(w int) []ddlLine {
	var out []ddlLine
	at := 0
	for _, l := range strings.Split(d.shown, "\n") {
		start, n := 0, 0
		for i := range l {
			if n == w {
				out = append(out, ddlLine{at + start, at + i})
				start, n = i, 0
			}
			n++
		}
		out = append(out, ddlLine{at + start, at + len(l)})
		at += len(l) + 1 // the newline
	}
	return out
}

func (d *ddlModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	head := string(d.routine.Kind)
	if d.routine.Args != "" || d.routine.Kind != model.RoutineProcedure {
		head += " " + d.routine.Name + "(" + d.routine.Args + ")"
	}
	if d.routine.Result != "" {
		head += " → " + d.routine.Result
	}
	s.Put(1, 0, truncate(head, s.W()-2), onBg(m.st.accent, bg).Bold())

	body := s.Sub(Rect{1, 2, s.W() - 2, s.H() - 4})
	body.Fill(m.st.chatCode)
	d.view = body.Rect()
	text := body.Sub(Rect{1, 0, body.W() - 2, body.H()}) // a cell of padding each side
	kinds := codeKinds("sql", d.shown)
	rows := d.lines(text.W())
	d.top = max(0, min(d.top, len(rows)-text.H()))
	for y := 0; y < text.H() && d.top+y < len(rows); y++ {
		r := rows[d.top+y]
		x := 0
		for _, sg := range codeSegs(m.st, d.shown[r.from:r.to], kinds, r.from) {
			x = text.Put(x, y, sg.text, sg.st)
		}
	}
	if len(rows) > text.H() {
		drawVBar(s.Sub(Rect{s.W() - 1, 2, 1, s.H() - 4}), m.st, d.top, text.H(), len(rows))
	}

	y := s.H() - 1
	d.copyBtn = chip(s, 1, y, " ⧉ Copy ", pick(d.hoverBtn == 0, m.st.buttonHover, m.st.buttonHot))
	d.insBtn = chip(s, d.copyBtn.X-s.Rect().X+d.copyBtn.W+2, y, " ↳ Insert into editor ", pick(d.hoverBtn == 1, m.st.buttonHover, m.st.button))
	s.PutRight(s.W()-1, y, "y copies · i inserts · Esc closes", onBg(m.st.muted, bg))
	return nil
}

func (d *ddlModal) copy(m *Model) tea.Cmd {
	return m.copyString(d.ddl, "the DDL of "+d.routine.QName())
}

// insert puts the definition in the editor at the caret and closes the
// viewer, leaving the keyboard in the editor to work on it.
func (d *ddlModal) insert(m *Model) {
	m.editor.Insert(d.ddl)
	m.focus = focusEditor
	m.drag.follow = true
	m.logf(logOk, "inserted the DDL of %s", d.routine.QName())
}

func (d *ddlModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc", "enter", "q":
		return nil, true
	case "y":
		return d.copy(m), false
	case "i":
		d.insert(m)
		return nil, true
	case "up", "k":
		d.top = max(d.top-1, 0)
	case "down", "j":
		d.top++
	case "pgup":
		d.top = max(d.top-d.view.H, 0)
	case "pgdown", "space":
		d.top += d.view.H
	case "home", "g":
		d.top = 0
	case "end", "G":
		d.top = len(d.ddl) // draw clamps it to the last page
	}
	return nil, false
}

func (d *ddlModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	switch {
	case m.modalClose().Contains(x, y):
		return nil, true
	case d.copyBtn.Contains(x, y):
		return d.copy(m), false
	case d.insBtn.Contains(x, y):
		d.insert(m)
		return nil, true
	}
	return nil, false
}

func (d *ddlModal) hover(m *Model, x, y int) {
	d.hoverBtn = -1
	if d.copyBtn.Contains(x, y) {
		d.hoverBtn = 0
	} else if d.insBtn.Contains(x, y) {
		d.hoverBtn = 1
	}
}

func (d *ddlModal) wheel(m *Model, x, y, dy int) { d.top = max(d.top+dy, 0) }

// routinesTitle is the Tables pane's title while it lists routines:
// "Routines · 3", or "Routines" until they land.
func (m *Model) routinesTitle() string {
	if rs := m.ws.Routines(); rs != nil {
		return "Routines · " + m.tablesTitleCount() // "3 of 12" while filtered
	}
	return "Routines"
}
