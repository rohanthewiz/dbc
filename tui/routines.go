package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/workspace"
)

// The Tables pane's Routines section: the stored functions and procedures
// of the schema it lists, and each one's DDL. It is the last of the pane's
// three sections (sections.go), folded as a tab starts; f, or Enter or a
// click on its header, unfolds it — and only then are the routines read:
//
//	│ ▾ Tables          34 │   f   │ ▾ Tables          34 │
//	│  orders              │  ───► │  orders              │
//	│ ▸ Views            3 │  ◄─── │ ▸ Views            3 │
//	│ ▸ Routines           │       │ ▾ Routines         2 │
//	                               │  order_total      fn │
//
// The fold is the tab's workspace's routines switch
// (Workspace.ShowRoutines), as the row counts' is, so each tab keeps its
// own, and while it is open every connect, refresh and schema pick reads
// the routines again (a *RoutinesLoaded, after the tables have drawn).
// Enter or a double-click on a routine reads its definition
// (Workspace.RoutineDDL) into a viewer (ddlModal), from which it can be
// copied or put in the editor.
//
// The rows are m.tables's own, a routine's holding a model.Routine where a
// table's hold its name: one pane, one cursor and one set of mouse paths,
// with the handful of table-only actions (columns, diagram) refused in
// words on a routine rather than misread.

// routinesShown reports whether the Tables pane's Routines section is open.
func (m *Model) routinesShown() bool { return m.ws.RoutinesShown() }

// toggleRoutines is the Tables pane's f key (and its menu's "Show
// routines" / "Fold routines", and Enter on the section's header): it
// unfolds or folds the Routines section. Unfolding reads them as a Job,
// landing as a *workspace.RoutinesLoaded; until then the section says
// "loading routines…". The cursor goes to the section's header either
// way, so the routines are in sight below it and a second f or Enter
// folds them back.
func (m *Model) toggleRoutines() tea.Cmd {
	cc, ok := m.cfg.ConnByName(m.ws.Active())
	on := !m.routinesShown()
	if on && ok && !db.HasRoutines(cc.Driver) {
		m.logf(logWarn, "%s stores no functions or procedures", cc.Driver)
		return nil
	}
	j := m.ws.ShowRoutines(on)
	m.fillTables()
	m.cursorToKey(itemKey(listItem{data: sectionRow(workspace.SectionRoutines)}))
	if on {
		m.setStatus("routines — Enter shows one's DDL, f folds them")
	} else {
		m.setStatus("routines folded")
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
// table or view by its name, a routine by its oid (Postgres) and
// signature, so an overload keeps the cursor rather than its neighbour,
// and a section's header by its section.
func itemKey(it listItem) string {
	switch d := it.data.(type) {
	case string:
		return d
	case sectionRow:
		return "\x00section " + string(d)
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

// currentTable is the table or view under the Tables pane's cursor: ""
// with none, or on a routine or a section's header.
func (m *Model) currentTable() (string, bool) {
	it, ok := m.tables.current()
	if !ok {
		return "", false
	}
	name, ok := it.data.(string)
	return name, ok
}

// tableOnly refuses a table's action (columns, diagram) on a routine's
// row, saying so rather than doing nothing. It reports whether it did.
func (m *Model) tableOnly(what string) bool {
	if _, ok := m.currentRoutine(); !ok {
		return false
	}
	m.logf(logWarn, "%s is for tables and views, not routines", what)
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

// openRoutineMenu is the Tables pane's context menu on a routine's row.
// onRow says the click landed on one.
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
		{label: "Fold routines", key: "f", act: func(m *Model) tea.Cmd { return m.toggleRoutines() }},
		{label: "Find by name…", key: "/", act: func(m *Model) tea.Cmd { m.openTableFind(); return nil }},
	}
	m.openMenu(x, y, append(items, m.navMenuItems()...))
}

// ---------------------------------------------------------------------------
// The DDL viewer
// ---------------------------------------------------------------------------

// ddlModal shows a routine's or a view's definition in full, colored as the editor
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
	// qname names what is shown, in the title and the log's lines; head is
	// the line above the text: a routine's signature, or "view <qname>"
	qname    string
	head     string
	ddl      string // as the server rendered it: what is copied and inserted
	shown    string // the same with tabs expanded, which the canvas would draw as dots
	top      int
	view     Rect
	copyBtn  Rect
	insBtn   Rect
	hoverBtn int // the button under the mouse: 0 copy, 1 insert, -1 none
}

// newDDLModal is the viewer for a routine: its heading the routine's
// signature, "function total(o integer) → numeric".
func newDDLModal(r model.Routine, ddl string) *ddlModal {
	head := string(r.Kind)
	if r.Args != "" || r.Kind != model.RoutineProcedure {
		head += " " + r.Name + "(" + r.Args + ")"
	}
	if r.Result != "" {
		head += " → " + r.Result
	}
	return &ddlModal{qname: r.QName(), head: head, ddl: ddl, shown: strings.ReplaceAll(ddl, "\t", "    "), hoverBtn: -1}
}

// newViewDDLModal is the viewer for a view, the same one a routine's
// definition opens in.
func newViewDDLModal(v db.TableRef, ddl string) *ddlModal {
	qname := v.Name
	if v.Schema != "" {
		qname = v.Schema + "." + v.Name
	}
	return &ddlModal{qname: qname, head: "view " + qname, ddl: ddl, shown: strings.ReplaceAll(ddl, "\t", "    "), hoverBtn: -1}
}

func (d *ddlModal) title() string { return "DDL · " + d.qname }

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
	s.Put(1, 0, truncate(d.head, s.W()-2), onBg(m.st.accent, bg).Bold())

	body := s.Sub(Rect{1, 2, s.W() - 2, s.H() - 4})
	body.Fill(m.st.chatCode)
	d.view = body.Rect()
	text := body.Sub(Rect{1, 0, body.W() - 2, body.H()}) // a cell of padding each side
	kinds := codeKinds("sql", d.shown, m.ws.Dialect())
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
	return m.copyString(d.ddl, "the DDL of "+d.qname)
}

// insert puts the definition in the editor at the caret and closes the
// viewer, leaving the keyboard in the editor to work on it.
func (d *ddlModal) insert(m *Model) {
	m.editor.Insert(d.ddl)
	m.focus = focusEditor
	m.drag.follow = true
	m.logf(logOk, "inserted the DDL of %s", d.qname)
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
