package tui

import (
	"fmt"
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/theme"
)

// layout is where everything is on the current frame. It is computed once
// per frame from the window size and the user's splitter positions, and it
// is the ONLY source of geometry: drawing reads it to know where to paint,
// and mouse handling reads the same value to know what was clicked.
//
//	row 0      ┌ toolbar: dbc │ ▶ Run ■ Stop ⧉ Copy ▾ …            ● conn ▾ ┐
//	           ├──────────┬───────────────────────────────┬─────────────────┤
//	           │ Conns    │ Query (editor)                │ Assistant       │
//	           │          ├───────── splitEd ─────────────┤ (when open)     │
//	           ├ Tables   │ Results (grid)                │                 │
//	           │          ├───────── splitLog ────────────┤                 │
//	           │          │ Log                           │                 │
//	           ├─splitSide┴───────────────────────────────┴─splitChat───────┤
//	row h-1    └ status: conn │ 8 rows in 1.2ms │ hints                     ┘
//
// The splitters are the pane borders themselves: grabbing the line between
// two panes and dragging it resizes them, which is what a border invites.
type layout struct {
	toolbar Rect
	buttons []button

	conns, tables Rect // sidebar boxes (zero when the sidebar is hidden)
	editor        Rect
	results       Rect
	logR          Rect
	chat          Rect // zero when the assistant is closed
	status        Rect

	splitSide, splitChat, splitEd, splitLog Rect
}

// button is one clickable toolbar chip.
type button struct {
	id      btnID
	r       Rect
	label   string
	enabled bool
	hot     bool // drawn lit: the primary action
}

type btnID int

const (
	btnNone btnID = iota
	btnRun
	btnStop
	btnCopy
	btnExport
	btnHistory
	btnScripts
	btnTables
	btnAssistant
	btnConn
)

// buttonSpec is a toolbar button in three widths; the widest tier that fits
// the terminal is used for all of them, so the bar never wraps and never
// changes a button's position except on resize.
type buttonSpec struct {
	id               btnID
	full, mid, short string
}

var toolbarSpecs = []buttonSpec{
	{btnRun, "▶ Run ^R", "▶ Run", "▶"},
	{btnStop, "■ Stop ^K", "■ Stop", "■"},
	{btnCopy, "⧉ Copy ▾", "⧉ Copy", "⧉"},
	{btnExport, "⤓ Export ^E", "⤓ Export", "⤓"},
	{btnHistory, "⌕ History ^P", "⌕ History", "⌕"},
	{btnScripts, "ƒ Scripts ^O", "ƒ Scripts", "ƒ"},
	{btnTables, "▦ Tables ^T", "▦ Tables", "▦"},
	{btnAssistant, "✦ Assistant ^A", "✦ Ask", "✦"},
}

// Size limits for the user-adjustable panes.
const (
	minSide      = 16
	minChat      = 30
	minCenter    = 36
	minLog       = 3
	minPaneH     = 4
	sidebarWidth = 72 // below this terminal width the sidebar is hidden
)

// computeLayout lays the frame out for the current size and splitters.
func (m *Model) computeLayout() layout {
	var l layout
	w, h := m.w, m.h
	l.toolbar = Rect{0, 0, w, 1}
	l.status = Rect{0, h - 1, w, 1}
	mainY, mainH := 1, max(h-2, 0)

	x0, x1 := 0, w
	if w >= sidebarWidth {
		m.sideW = max(minSide, min(m.sideW, w/3))
		connH := max(3, min(len(m.conns.items)+2, mainH/2))
		l.conns = Rect{0, mainY, m.sideW, connH}
		l.tables = Rect{0, mainY + connH, m.sideW, mainH - connH}
		l.splitSide = Rect{m.sideW - 1, mainY, 1, mainH}
		x0 = m.sideW
	}
	if m.chat.open {
		if m.chatW == 0 {
			m.chatW = max(minChat, w*36/100)
		}
		m.chatW = max(minChat, min(m.chatW, w-x0-minCenter))
		if m.chatW >= minChat {
			l.chat = Rect{w - m.chatW, mainY, m.chatW, mainH}
			l.splitChat = Rect{w - m.chatW, mainY, 1, mainH}
			x1 = w - m.chatW
		}
	}
	cw := x1 - x0

	logH := m.logH
	if mainH < 16 {
		logH = 0 // a short terminal gives the log's rows to the grid
	}
	logH = max(0, min(logH, mainH-2*minPaneH))
	if logH > 0 {
		logH = max(logH, minLog)
		l.logR = Rect{x0, mainY + mainH - logH, cw, logH}
		l.splitLog = Rect{x0, l.logR.Y, cw, 1}
	}
	above := mainH - logH
	edH := int(m.edFrac * float64(above))
	edH = max(minPaneH, min(edH, above-minPaneH))
	l.editor = Rect{x0, mainY, cw, edH}
	l.results = Rect{x0, mainY + edH, cw, above - edH}
	l.splitEd = Rect{x0, mainY + edH - 1, cw, 1}

	l.buttons = m.layoutButtons(w)
	return l
}

// layoutButtons places the toolbar chips after the "dbc" badge, choosing the
// widest tier that leaves room for the connection chip on the right.
func (m *Model) layoutButtons(w int) []button {
	connLabel := "● " + m.active + " ▾"
	connW := width(connLabel) + 2
	avail := w - 6 - connW // " dbc " badge and a gap
	tiers := []func(buttonSpec) string{
		func(s buttonSpec) string { return s.full },
		func(s buttonSpec) string { return s.mid },
		func(s buttonSpec) string { return s.short },
	}
	pick := tiers[len(tiers)-1]
	for _, t := range tiers {
		total := 0
		for _, s := range toolbarSpecs {
			total += width(t(s)) + 3 // padding and gap
		}
		if total <= avail {
			pick = t
			break
		}
	}
	var out []button
	x := 6
	for _, s := range toolbarSpecs {
		label := " " + pick(s) + " "
		bw := width(label)
		if x+bw > w-connW {
			break
		}
		b := button{id: s.id, r: Rect{x, 0, bw, 1}, label: label, enabled: true}
		switch s.id {
		case btnRun:
			b.enabled, b.hot = !m.busy, true
		case btnStop:
			b.enabled = m.busy
		case btnCopy, btnExport:
			b.enabled = m.lastRes != nil
		case btnAssistant:
			b.hot = m.chat.open
		}
		out = append(out, b)
		x += bw + 1
	}
	out = append(out, button{id: btnConn, r: Rect{w - connW, 0, connW, 1},
		label: " " + connLabel + " ", enabled: true})
	return out
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

// View renders the frame.
func (m *Model) View() tea.View {
	c, cur := m.render()
	v := tea.NewView(c.String())
	v.AltScreen = true
	// All motion, not just drags: hover feedback on buttons, grid cells and
	// menu rows is how a mouse user discovers what is clickable.
	v.MouseMode = tea.MouseModeAllMotion
	v.ReportFocus = true
	v.WindowTitle = m.windowTitle()
	v.BackgroundColor = rgb(m.st.pal.Bg)
	v.ForegroundColor = rgb(m.st.pal.Fg)
	if cur != nil {
		v.Cursor = tea.NewCursor(cur.x, cur.y)
		v.Cursor.Shape = tea.CursorBar
	}
	return v
}

// caret is where the terminal cursor goes, when a text field has focus.
type caret struct{ x, y int }

// rgb converts a palette hex color for the View's terminal colors.
func rgb(h string) color.Color {
	r, g, b, ok := theme.ParseHex(h)
	if !ok {
		return nil
	}
	return color.RGBA{R: r, G: g, B: b, A: 0xff}
}

// windowTitle names the connection and what is running, so a terminal tab
// (or the cats sidebar) says what this pane is doing without looking at it.
// The running tag leads: read from a tab bar, the question a title answers
// is "is this still going?", and the connection is its context.
func (m *Model) windowTitle() string {
	conn, tag := titleSafe(m.active), ""
	if m.busy {
		tag = titleSafe(m.runTag)
	}
	switch {
	case tag != "" && conn != "":
		return tag + " · " + conn + " — dbc"
	case tag != "":
		return tag + " — dbc"
	case conn != "":
		return conn + " — dbc"
	}
	return "dbc"
}

// render draws the whole frame onto a fresh canvas.
func (m *Model) render() (*Canvas, *caret) {
	c := NewCanvas(m.w, m.h, m.st.base)
	if m.w < 20 || m.h < 8 {
		c.Sub(Rect{0, 0, m.w, m.h}).Put(0, 0, "dbc: window too small", m.st.warn)
		return c, nil
	}
	m.lay = m.computeLayout()
	l := m.lay
	var cur *caret

	m.drawToolbar(c.Sub(l.toolbar))
	if !l.conns.Empty() {
		m.drawPane(c, l.conns, "Connections", focusConns, m.st.panel, func(s Surface) {
			m.conns.draw(s, m.st, m.st.panel, m.focus == focusConns, "no connections")
		})
		title := "Tables"
		if n := len(m.tables.items); n > 0 {
			title = fmt.Sprintf("Tables · %d", n)
		}
		m.drawPane(c, l.tables, title, focusTables, m.st.panel, func(s Surface) {
			m.tables.draw(s, m.st, m.st.panel, m.focus == focusTables, "(none yet)")
		})
	}

	m.drawPane(c, l.editor, m.editorTitle(), focusEditor, m.st.base, func(s Surface) {
		x, y, ok := m.editor.Draw(s, m.st, m.st.base, m.currentStmtRange(), m.drag.follow)
		m.drag.follow = false
		if ok && m.focus == focusEditor && m.modal == nil && m.menu == nil {
			cur = &caret{x, y}
		}
	})
	m.drawPane(c, l.results, m.resultsTitle(), focusGrid, m.st.base, func(s Surface) {
		m.grid.Draw(s, m.st, m.focus == focusGrid, "run a query with Ctrl+R or ▶ Run — results appear here")
	})
	if !l.logR.Empty() {
		m.drawPane(c, l.logR, "Log", focusLog, m.st.base, func(s Surface) {
			m.logp.draw(s, m.st, m.st.base)
		})
	}
	if !l.chat.Empty() {
		if p := m.drawChat(c, l.chat); p != nil && m.focus == focusChat && m.modal == nil && m.menu == nil {
			cur = p
		}
	}
	m.drawStatus(c.Sub(l.status))
	m.drawSplitterHover(c)

	if m.modal != nil {
		if p := m.drawModal(c); p != nil && m.menu == nil {
			cur = p
		}
	}
	if m.menu != nil {
		m.menu.draw(c, m.st)
	}
	return c, cur
}

// drawPane draws a bordered pane with the focus cue, then its content.
func (m *Model) drawPane(c *Canvas, r Rect, title string, f focusID, bg Style, body func(Surface)) {
	s := c.Sub(r)
	s.Fill(bg)
	border, ts := onBg(m.st.border, bg), onBg(m.st.title, bg)
	if m.focus == f {
		border, ts = onBg(m.st.borderFocus, bg), onBg(m.st.titleFocus, bg)
	}
	s.Box(border, title, ts)
	body(c.Sub(r.Inset(1)))
}

func (m *Model) editorTitle() string {
	if stmts, tag := m.stmtsToRun(); len(stmts) > 0 && tag != "query" {
		return "Query · ^R runs " + tag
	}
	return "Query"
}

func (m *Model) resultsTitle() string {
	r := m.lastRes
	if r == nil {
		return "Results"
	}
	if r.IsExec {
		return fmt.Sprintf("Results · %d affected · %s", r.Affected, r.Duration.Round(10_000))
	}
	t := fmt.Sprintf("Results · %d rows · %s", len(r.Rows), r.Duration.Round(10_000))
	if r.Truncated {
		t += " · truncated"
	}
	return t + " · right-click to copy"
}

// drawToolbar draws the top bar: the badge, the buttons, the connection chip.
func (m *Model) drawToolbar(s Surface) {
	s.Fill(m.st.raised)
	s.Put(0, 0, " dbc ", m.st.buttonHover)
	for _, b := range m.lay.buttons {
		st := m.st.button
		switch {
		case !b.enabled:
			st = m.st.button.WithFg(m.st.muted.Fg).Dim()
		case m.hover.btn == b.id:
			st = m.st.buttonHover
		case b.id == btnStop:
			st = m.st.buttonStop
		case b.hot:
			st = m.st.buttonHot
		}
		s.Put(b.r.X, 0, b.label, st)
	}
}

// drawStatus draws the bottom bar: connection, status, and key hints that
// shrink rather than wrap.
func (m *Model) drawStatus(s Surface) {
	bar := m.st.raised.WithFg(m.st.muted.Fg)
	s.Fill(bar)
	x := s.Put(0, 0, " ● ", bar.WithFg(m.st.accent.Fg))
	x = s.Put(x, 0, m.active, bar.WithFg(m.st.accent.Fg).Bold())
	x = s.Put(x, 0, " │ ", bar)
	st := bar.WithFg(m.st.base.Fg)
	switch {
	case m.busy:
		st = bar.WithFg(m.st.warn.Fg)
	case strings.HasPrefix(m.status, "error"):
		st = bar.WithFg(m.st.err.Fg)
	}
	x = s.Put(x, 0, m.status, st)

	hints := []string{"^R run", "^K stop", "^A ask", "^E export", "^P history", "^Q quit"}
	for len(hints) > 0 {
		h := strings.Join(hints, "  ") + " "
		if s.W()-x-3 >= width(h) {
			s.PutRight(s.W(), 0, h, bar)
			break
		}
		hints = hints[:len(hints)-1]
	}
}

// drawSplitterHover lights the border under the mouse when it can be dragged,
// which is the only way a user would guess that it can.
func (m *Model) drawSplitterHover(c *Canvas) {
	var r Rect
	switch {
	case m.drag.kind >= dragSplitSide && m.drag.kind <= dragSplitLog:
		r = m.splitRect(m.drag.kind)
	case m.hover.split != dragNone:
		r = m.splitRect(m.hover.split)
	default:
		return
	}
	c.Sub(r).Restyle(func(s Style) Style { return s.WithFg(m.st.accent.Fg).Bold() })
}

func (m *Model) splitRect(k dragKind) Rect {
	switch k {
	case dragSplitSide:
		return m.lay.splitSide
	case dragSplitChat:
		return m.lay.splitChat
	case dragSplitEd:
		return m.lay.splitEd
	case dragSplitLog:
		return m.lay.splitLog
	}
	return Rect{}
}
