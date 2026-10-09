package tui

import (
	tea "charm.land/bubbletea/v2"
)

// The references list — the terminal's take on Monaco's Shift+F12 peek: one
// row per use of the symbol, "line · the line's text" with the use itself
// underlined, opened by Shift+F12 (showUsages) once the uses are marked.
//
//	Shift+F12 ──► marks + log (as before) ──► usesModal, cursor on the use
//	                                            under the caret
//	  ↑ ↓ PgUp PgDn Home End, Shift+F12 (next, wraps), wheel, hover
//	      └─► the editor follows: the use is selected and scrolled to (preview)
//	  Enter / click ──► caret at the start of that use, list closed
//	  Esc / ✕ ───────► caret, selection and scroll put back as they were
//
// PREVIEW IN THE EDITOR ITSELF. Monaco's peek embeds a second editor; a
// terminal has no room for one, so the list sits over the results pane
// (place) and the real editor above it shows the use the cursor is on. The
// use is shown as a selection, not just a caret, because the editor's caret
// is not drawn while a modal holds the keyboard. Enter collapses that
// selection to a caret at the use's start — the old stepping's landing
// spot — so a key typed next does not replace the name. Esc is "never
// mind": it restores the exact view (caretView) from before the list.
//
// The marks stay either way: closing the list is not clearing them (Esc in
// the editor still does that, as does any edit).

// usesRows is how many uses the list shows before it scrolls.
const usesRows = 10

// useRow is one use as a list row: its mark and where it sits in the
// editor's lines (rune columns, which is what drawing walks).
type useRow struct {
	mk   editorMark
	row  int
	a, b int // rune columns [a, b) of the use on that line
}

// caretView is the part of the editor a preview disturbs, so Esc can put
// it back exactly: caret, selection, column goal and scroll. It is the
// light cousin of editorView (editor.go), which also carries the text and
// the undo stacks across a console swap — far more than a peek moves.
type caretView struct {
	cur, anc  pos
	sel       bool
	goal      int
	top, left int
}

func (e *editor) saveCaret() caretView {
	return caretView{e.cur, e.anc, e.sel, e.goal, e.top, e.left}
}

func (e *editor) restoreCaret(v caretView) {
	e.cur, e.anc, e.sel, e.goal, e.top, e.left = v.cur, v.anc, v.sel, v.goal, v.top, v.left
}

// usesModal is the references list. lst is used for its cursor, scroll,
// hover and hit-testing only; the rows are drawn here, since a list row
// cannot underline part of its label.
type usesModal struct {
	modalBase
	what string // "alias o", for the title
	uses []useRow
	lst  *list
	ver  int       // the editor version the uses were found at
	back caretView // where Esc returns to
}

// openUses opens the references list on marks (in buffer order), its
// cursor on mark cur.
func (m *Model) openUses(what string, marks []editorMark, cur int) {
	e := m.editor
	md := &usesModal{what: what, lst: newList(), ver: e.version, back: e.saveCaret()}
	items := make([]listItem, len(marks))
	for i, mk := range marks {
		a, b := e.posAt(mk.from), e.posAt(mk.to)
		md.uses = append(md.uses, useRow{mk: mk, row: a.row, a: a.col, b: b.col})
		items[i] = listItem{data: i}
	}
	md.lst.set(items)
	md.lst.cur = cur
	m.compl = nil
	m.openModal(md)
	md.preview(m)
}

func (u *usesModal) title() string {
	return "Uses of " + u.what + " · " + itoa(len(u.uses)) + " · ↑↓/⇧F12 step · Enter go · Esc back"
}

func (u *usesModal) size(w, h int) (int, int) {
	return min(max(60, w*2/3), 100), min(len(u.uses), usesRows) + 2
}

// place puts the list over the top of the results pane, as wide as the
// editor and right under it, so the editor stays in view to preview the
// use the cursor is on. A results pane too short to hold a few rows (the
// editor zoomed, a tiny terminal) leaves it centered, as other modals are.
func (u *usesModal) place(m *Model) (Rect, bool) {
	ed, res := m.lay.editor, m.lay.results
	if res.H < 5 || ed.H < 3 || ed.W < 30 {
		return Rect{}, false
	}
	_, h := u.size(m.w, m.h)
	return Rect{ed.X, res.Y, ed.W, min(h, res.H)}, true
}

// next is the index after i, wrapping round: Shift+F12 in the list.
func (u *usesModal) next(i int) int { return (i + 1) % len(u.uses) }

// preview shows the use under the cursor in the editor: selected, and
// scrolled to on the next draw.
func (u *usesModal) preview(m *Model) {
	if u.lst.cur < 0 || u.lst.cur >= len(u.uses) {
		return
	}
	mk := u.uses[u.lst.cur].mk
	m.editor.selectSpan(mk.from, mk.to)
	m.drag.follow = true
}

// stale reports whether the editor's text changed under the list, which
// leaves its offsets pointing at other text. Nothing should edit while a
// modal holds the keyboard and paste; this keeps an async change from
// sending the caret somewhere meaningless if one ever does.
func (u *usesModal) stale(m *Model) bool { return m.editor.version != u.ver }

// goTo closes the list with the caret at the start of the use under the
// cursor.
func (u *usesModal) goTo(m *Model) (tea.Cmd, bool) {
	if u.stale(m) {
		return nil, true
	}
	mk := u.uses[u.lst.cur].mk
	m.editor.move(m.editor.posAt(mk.from), false)
	m.editor.goal = m.editor.dispCol(m.editor.cur)
	m.focus = focusEditor
	m.drag.follow = true
	return nil, true
}

// cancel closes the list with the editor as it was before it opened.
func (u *usesModal) cancel(m *Model) (tea.Cmd, bool) {
	m.editor.restoreCaret(u.back)
	m.drag.follow = false // the restored scroll is the view; do not chase the caret
	return nil, true
}

func (u *usesModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	if u.stale(m) {
		return nil, true
	}
	switch k.String() {
	case "esc":
		return u.cancel(m)
	case "shift+f12", "f24":
		u.lst.cur = u.next(u.lst.cur)
		u.lst.ensureVisible()
		u.preview(m)
		return nil, false
	}
	used, picked := u.lst.key(k)
	if picked {
		return u.goTo(m)
	}
	if used {
		u.preview(m)
	}
	return nil, false
}

func (u *usesModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	if m.modalClose().Contains(x, y) {
		return u.cancel(m)
	}
	if i := u.lst.indexAt(x, y); i >= 0 {
		u.lst.cur = i
		return u.goTo(m)
	}
	return nil, false
}

func (u *usesModal) hover(m *Model, x, y int)     { u.lst.hover = u.lst.indexAt(x, y) }
func (u *usesModal) wheel(m *Model, x, y, dy int) { u.lst.scroll(dy) }

// draw paints one row per use:
//
//	12  WHERE o.total > 10 AND o.id IN (…)      declared
//	──  ────────────────────────────────────    ────────
//	line the line, leading blanks trimmed,      the declaration
//	no.  the use underlined, syntax-coloured   only
//
// A line too long for the row is shown from a few cells before the use,
// behind an ellipsis, so the use itself is never cut off the right edge.
func (u *usesModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	s.Fill(bg)
	l := u.lst
	l.view = s.Rect()
	l.top = max(0, min(l.top, max(len(u.uses)-s.H(), 0)))
	l.ensureVisible()

	e := m.editor
	var hl [][]Style
	if e.hlVer == e.version {
		hl = e.hl // the editor's syntax colours, when they are current
	}
	w := s.W()
	if len(u.uses) > s.H() {
		w--
		drawVBar(s.Sub(Rect{w, 0, 1, s.H()}), m.st, l.top, s.H(), len(u.uses))
	}
	gw := width(itoa(u.uses[len(u.uses)-1].row + 1)) // rows are in buffer order

	for y := 0; y < s.H(); y++ {
		i := l.top + y
		if i >= len(u.uses) {
			break
		}
		ur := u.uses[i]
		rs := bg
		switch {
		case i == l.cur:
			rs = m.st.sel
		case i == l.hover:
			rs = bg.WithBg(m.st.hover.Bg)
		}
		row := s.Sub(Rect{0, y, w, 1})
		row.Fill(rs)
		// muted, not the editor's lineNo: that one is drawn to recede, and
		// here the number is half of what a row says
		row.PutRight(1+gw, 0, itoa(ur.row+1), rs.WithFg(m.st.muted.Fg))
		subW := 0
		if ur.mk.def {
			subW = width("declared") + 2
			row.PutRight(w-1, 0, "declared", rs.WithFg(m.st.muted.Fg))
		}
		x0 := gw + 3
		text := row.Sub(Rect{x0, 0, max(w-x0-subW-1, 0), 1})
		if ur.row >= len(e.lines) || ur.b > len(e.lines[ur.row]) {
			continue // the text moved under the list (stale): the next key closes it
		}
		u.drawLine(text, e.lines[ur.row], hlRow(hl, ur.row), ur, rs, i == l.cur, m.st)
	}
	return nil
}

// hlRow is row's syntax styles, or nil.
func hlRow(hl [][]Style, row int) []Style {
	if row < len(hl) {
		return hl[row]
	}
	return nil
}

// drawLine draws one line of the editor into s with the use [ur.a, ur.b)
// marked. On the cursor row the row's own colours win over the syntax
// colours: the selection background would leave some of them unreadable.
func (u *usesModal) drawLine(s Surface, line []rune, hl []Style, ur useRow, rs Style, cur bool, st styles) {
	start := 0
	for start < ur.a && (line[start] == ' ' || line[start] == '\t') {
		start++
	}
	// keep the use in view: if the text from start through the use's end
	// is wider than the row, begin a few runes before the use instead
	const lead = 8
	x := 0
	if dispWidth(line[start:ur.b]) > s.W() {
		start = max(start, ur.a-lead)
		x = s.Put(0, 0, "…", rs.WithFg(st.muted.Fg))
	}
	for i := start; i < len(line) && x < s.W(); i++ {
		cs := rs
		if !cur && i < len(hl) {
			cs = hl[i].WithBg(rs.Bg)
		}
		if i >= ur.a && i < ur.b {
			cs = cs.Underline().Bold()
		}
		r := line[i]
		if r == '\t' {
			r = ' '
		}
		x = s.Put(x, 0, string(r), cs)
	}
}

// dispWidth is the display width of runes.
func dispWidth(rs []rune) int {
	n := 0
	for _, r := range rs {
		n += runeWidth(r)
	}
	return n
}
