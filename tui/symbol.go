package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/sqlcomplete"
)

// Go to definition, usages and rename in the editor — dbc web's F12,
// Shift+F12 and F2 (web/symbol.go, editor.js), answered by the same
// resolver (sqlcomplete.Resolve and Rename) over the editor's text.
//
//	F12 / Ctrl+click ──► Resolve(text, caret) ──► select the declaration
//	Shift+F12 ─────────► Resolve ──► mark every use (editor.marks)
//	   again, on the same symbol ──► caret to the next use (wraps)
//	F2 ────────────────► Resolve: refusal? ─► the log says why
//	                        └─► rename prompt (renameModal), name filled in
//	                              Enter ─► ws.Rename ─► one undoable edit
//
// The resolver reads only the statement: no schema, no connection round
// trip, so all three answer synchronously inside Update, unlike completion.
// It works in byte offsets into Text(), which is what editor.offset and
// posAt convert to and from — the same currency the completion popup and
// the statement marker already use, so nothing here deals in UTF-16 the
// way the browser side must.
//
// MARKS, NOT SELECTIONS. The uses are shown as highlights the editor draws
// over the syntax colours (editor.marks), not as a selection: the editor
// has one selection, and a typed key would replace it. A mark set belongs
// to the edit version it was made at (marksVer), so any edit — typing, an
// undo, a paste, a console swap — makes it stale and it simply stops being
// drawn; no edit path has to remember to clear it. Esc clears it outright.
//
// Stepping through the uses needs no extra state either: the marks are the
// uses in buffer order, so "next" is the first one starting after the
// caret, wrapping to the first.

// editorMark is a highlighted byte span of the editor's text: a use of the
// symbol whose usages are shown, def marking its declaration.
type editorMark struct {
	from, to int
	def      bool
}

// setMarks shows marks as of the current edit version.
func (e *editor) setMarks(marks []editorMark) {
	e.marks, e.marksVer = marks, e.version
}

// liveMarks is the mark set while it still describes the text: nil once
// any edit has happened since it was set.
func (e *editor) liveMarks() []editorMark {
	if e.marksVer != e.version {
		return nil
	}
	return e.marks
}

// clearMarks drops the marks, and reports whether any were showing — Esc
// is spent on them only then.
func (e *editor) clearMarks() bool {
	had := len(e.liveMarks()) > 0
	e.marks = nil
	return had
}

// selectSpan selects the byte range [from, to), caret at its end, and asks
// the next draw to scroll it into view.
func (e *editor) selectSpan(from, to int) {
	e.anc, e.cur = e.posAt(from), e.posAt(to)
	e.sel = from < to
	e.goal = e.dispCol(e.cur)
}

// ApplyEdits replaces the edits' byte ranges with their text as ONE undo
// step — a rename's edits are one change to the user, and undoing it a use
// at a time would leave a query that names a table both ways. The edits
// must not overlap; they may come in any order. The caret keeps its place
// in the text around it: shifted by the edits before it, and at the start
// of an edit it sat inside.
func (e *editor) ApplyEdits(edits []sqlcomplete.Edit) {
	if len(edits) == 0 {
		return
	}
	edits = slices.Clone(edits)
	slices.SortFunc(edits, func(a, b sqlcomplete.Edit) int { return a.From - b.From })

	text, caret := e.Text(), e.Caret()
	var b strings.Builder
	at, newCaret := 0, caret
	for _, ed := range edits {
		b.WriteString(text[at:ed.From])
		b.WriteString(ed.Text)
		switch {
		case caret >= ed.To:
			newCaret += len(ed.Text) - (ed.To - ed.From)
		case caret > ed.From:
			// inside the renamed name: keep the offset into it, up to the
			// new name's length, so a caret mid-word stays mid-word
			newCaret = b.Len() - len(ed.Text) + min(caret-ed.From, len(ed.Text))
		}
		at = ed.To
	}
	b.WriteString(text[at:])

	e.lastKind = editOther // its own step, never merged into typing
	e.checkpoint(editOther)
	e.lastKind = editOther
	e.lines = splitLines(normalize(b.String(), e.single))
	e.cur, e.sel = e.posAt(newCaret), false
	e.goal = e.dispCol(e.cur)
}

// symbolKey handles the editor's symbol keys, reporting whether k was one.
// Shift+F12 also arrives as F24 from terminals that encode shifted
// function keys xterm's old way (F13–F24).
func (m *Model) symbolKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "f12":
		return m.gotoDefinition(), true
	case "shift+f12", "f24":
		return m.showUsages(), true
	case "f2":
		return m.startRename(), true
	case "esc":
		return nil, m.editor.clearMarks()
	}
	return nil, false
}

// symbolAtCaret resolves the name under the editor's caret.
func (m *Model) symbolAtCaret() sqlcomplete.Symbol {
	return sqlcomplete.Resolve(m.editor.Text(), m.editor.Caret())
}

// symbolWhat names a symbol for the log: "alias o", "CTE totals".
func symbolWhat(s sqlcomplete.Symbol) string {
	switch s.Kind {
	case sqlcomplete.SymCTE:
		return "CTE " + s.Name
	case sqlcomplete.SymTable:
		return "table " + s.Name
	}
	return "alias " + s.Name
}

// notOnSymbol is the one sentence for a caret on nothing resolvable — the
// same words dbc web's rename box uses.
const notOnSymbol = "works on a table alias or a CTE name"

// gotoDefinition (F12, Ctrl+click) selects where the name under the caret
// is declared. Selecting it, rather than only moving the caret there, is
// what makes the jump visible in a terminal, where the caret is one cell;
// the next arrow key drops the selection as usual.
func (m *Model) gotoDefinition() tea.Cmd {
	sym := m.symbolAtCaret()
	if sym.Kind == "" {
		m.log(logWarn, "no definition here — F12 "+notOnSymbol)
		return nil
	}
	m.editor.selectSpan(sym.Def.From, sym.Def.To)
	m.drag.follow = true
	m.compl = nil
	if sym.Def == sym.At {
		m.logf(logInfo, "%s is declared here", symbolWhat(sym))
	} else {
		m.logf(logInfo, "%s is declared on line %d", symbolWhat(sym), m.editor.cur.row+1)
	}
	return nil
}

// showUsages (Shift+F12) marks every use of the name under the caret. Asked
// again on the same symbol, while its marks still show, it moves the caret
// to the next use instead — the terminal's stand-in for Monaco's list of
// references, which has no room here.
func (m *Model) showUsages() tea.Cmd {
	sym := m.symbolAtCaret()
	if sym.Kind == "" {
		m.log(logWarn, "no uses to show here — Shift+F12 "+notOnSymbol)
		return nil
	}
	marks := make([]editorMark, len(sym.Uses))
	for i, u := range sym.Uses {
		marks[i] = editorMark{from: u.From, to: u.To, def: u == sym.Def}
	}
	if live := m.editor.liveMarks(); slices.Equal(live, marks) {
		m.nextUse(marks)
		return nil
	}
	m.editor.setMarks(marks)
	lines := make([]string, 0, len(marks))
	for _, mk := range marks {
		n := itoa(m.editor.posAt(mk.from).row + 1)
		if len(lines) == 0 || lines[len(lines)-1] != n {
			lines = append(lines, n)
		}
	}
	m.logf(logInfo, "%s: %s in this statement (line %s) — Shift+F12 again steps through them, Esc clears",
		symbolWhat(sym), plural(len(marks), "use"), strings.Join(lines, ", "))
	return nil
}

// nextUse puts the caret on the first mark that starts after it, wrapping
// round to the first.
func (m *Model) nextUse(marks []editorMark) {
	caret := m.editor.Caret()
	next := marks[0]
	for _, mk := range marks {
		if mk.from > caret {
			next = mk
			break
		}
	}
	m.editor.move(m.editor.posAt(next.from), false)
	m.editor.goal = m.editor.dispCol(m.editor.cur)
	m.drag.follow = true
	m.compl = nil
}

// startRename (F2) opens the rename prompt on the name under the caret, or
// says why there is none: nothing resolvable, or a table, whose name is the
// database's and not the query's to change.
func (m *Model) startRename() tea.Cmd {
	sym := m.symbolAtCaret()
	switch {
	case sym.Kind == "":
		m.log(logWarn, "nothing to rename here — rename "+notOnSymbol)
		return nil
	case sym.Fixed != "":
		m.log(logWarn, sym.Fixed)
		return nil
	}
	md := &renameModal{sym: sym, caret: m.editor.Caret(), field: newEditor(true), hoverBtn: -1}
	md.field.SetText(sym.Name)
	md.field.SelectAll() // typing replaces the old name; an arrow key keeps it to edit
	m.compl = nil
	m.openModal(md)
	return nil
}

// ---------------------------------------------------------------------------
// The rename prompt
// ---------------------------------------------------------------------------

// renameModal asks for the new name. The edits are worked out when Enter is
// pressed, not when it opens, from the editor's text then — which is the
// text it opened on, since a modal holds the keyboard and paste. A refusal
// (the name is taken in the query, it is empty) keeps the prompt open with
// the reason under the field, so the name can be fixed rather than retyped.
type renameModal struct {
	modalBase
	sym      sqlcomplete.Symbol
	caret    int // the editor's caret when F2 was pressed: what to rename
	field    *editor
	errText  string
	fieldR   Rect
	okBtn    Rect
	hoverBtn int
}

func (r *renameModal) title() string            { return "Rename " + symbolWhat(r.sym) }
func (r *renameModal) size(w, h int) (int, int) { return 60, 8 }

func (r *renameModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	s.Put(1, 0, "New name", onBg(m.st.muted, bg))
	field := s.Sub(Rect{1, 1, s.W() - 2, 1})
	r.fieldR = field.Rect()
	cx, cy, ok := r.field.Draw(field, m.st, m.st.raised, [2]int{}, true)
	if r.errText != "" {
		s.Put(1, 3, truncate(r.errText, s.W()-2), onBg(m.st.err, bg))
	} else {
		s.Put(1, 3, "every use in the statement is renamed; quoted if the name needs it", onBg(m.st.muted, bg).Italic())
	}
	y := s.H() - 1
	r.okBtn = chip(s, 1, y, " ✎ Rename ", pick(r.hoverBtn == 0, m.st.buttonHover, m.st.buttonHot))
	s.PutRight(s.W()-1, y, "Enter renames · Esc cancels", onBg(m.st.muted, bg))
	if ok {
		return &caret{cx, cy}
	}
	return nil
}

func (r *renameModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc":
		return nil, true
	case "enter":
		return nil, r.apply(m)
	}
	before := r.field.Text()
	r.field.HandleKey(k)
	if r.field.Text() != before {
		r.errText = ""
	}
	return nil, false
}

func (r *renameModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	switch {
	case m.modalClose().Contains(x, y):
		return nil, true
	case r.okBtn.Contains(x, y):
		return nil, r.apply(m)
	case r.fieldR.Contains(x, y):
		r.field.Click(x, y, clicks, shift)
	}
	return nil, false
}

func (r *renameModal) hover(m *Model, x, y int) {
	r.hoverBtn = -1
	if r.okBtn.Contains(x, y) {
		r.hoverBtn = 0
	}
}

func (r *renameModal) paste(m *Model, s string) { r.field.Insert(s); r.errText = "" }

// apply renames, reporting whether the prompt is done. The quoting is the
// resolver's, by the active connection's driver (workspace.Rename), as in
// dbc web: "Ord" on Postgres, `order` on MySQL.
func (r *renameModal) apply(m *Model) bool {
	name := strings.TrimSpace(r.field.Text())
	if name == r.sym.Name {
		return true // nothing to change: closing is the answer
	}
	edits, err := m.ws.Rename(m.editor.Text(), r.caret, name)
	if err != nil {
		r.errText = err.Error()
		m.logf(logWarn, "rename: %s", err.Error())
		return false
	}
	m.editor.ApplyEdits(edits)
	m.focus = focusEditor
	m.drag.follow = true
	m.logf(logOk, "renamed %s to %s — %s · ^Z undoes it", symbolWhat(r.sym), edits[0].Text, plural(len(edits), "place"))
	return true
}

// symbolMenuItems are the editor menu's rows for the name under the caret
// (the right-click has already put the caret where it was clicked). Each is
// offered with the reason it would do nothing, as the rest of that menu is.
func (m *Model) symbolMenuItems() []menuItem {
	sym := m.symbolAtCaret()
	why, renameWhy := "", ""
	if sym.Kind == "" {
		why = "not on a table alias or a CTE name"
		renameWhy = why
	} else if sym.Fixed != "" {
		renameWhy = fmt.Sprintf("%s is a table — give it an alias and rename that", sym.Name)
	}
	return []menuItem{
		heading(""),
		{label: "Go to definition", key: "F12", why: why, act: func(m *Model) tea.Cmd { return m.gotoDefinition() }},
		{label: "Show usages", key: "⇧F12", why: why, act: func(m *Model) tea.Cmd { return m.showUsages() }},
		{label: "Rename…", key: "F2", why: renameWhy, act: func(m *Model) tea.Cmd { return m.startRename() }},
	}
}
