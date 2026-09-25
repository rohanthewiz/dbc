package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// Mouse handling. The order of questions a click is asked is the design:
//
//  1. Is a menu open? A click inside picks a row; outside, it closes the
//     menu and is swallowed — except a right-click, which then opens the
//     menu for where it landed, as right-clicking elsewhere does everywhere.
//  2. Is a modal open? It gets clicks inside it; clicks outside do nothing
//     (a modal is dismissed on purpose, with Esc or its ✕).
//  3. A splitter under a left press starts a resize drag.
//  4. Otherwise the pane under the pointer takes the keyboard focus —
//     keyboard follows mouse — and the pane's widget handles the click.
//
// Every gesture that continues (a text selection, a range selection, a
// scrollbar or splitter drag) records its kind in m.drag on the press, and
// motion is routed by that kind alone, not by where the pointer now is — so
// dragging a selection past the edge of its pane keeps selecting.
//
// A new press always ends any stale gesture first: a release can be lost
// (the pointer let go outside the terminal window), and a drag that never
// ended would otherwise hijack the next unrelated click.

// dragKind is the gesture in progress.
type dragKind int

const (
	dragNone dragKind = iota
	dragSplitSide
	dragSplitChat
	dragSplitEd
	dragSplitLog
	dragEditor
	dragGrid
	dragGridBar
	dragGridCol // a header border: resizing a column
	dragChatInput
	dragModal
)

type dragState struct {
	kind dragKind

	// follow asks the next draw to scroll the editor to its caret. It is set
	// by typing and clicking, and deliberately not by the wheel: scrolling
	// away to read something must not snap back on the next frame.
	follow bool
}

// clickState counts clicks for double- and triple-click detection.
type clickState struct {
	x, y   int
	at     time.Time
	n      int
	button tea.MouseButton
}

// multiClickWindow is how quickly a second click must follow to count as a
// double-click — the common desktop default.
const multiClickWindow = 400 * time.Millisecond

// hoverState is what the mouse is over, for highlight feedback.
type hoverState struct {
	btn   btnID
	split dragKind
}

// now is time.Now, as a var so click-counting tests control the clock.
var now = time.Now

// countClick records a press and returns 1, 2 or 3 for single, double and
// triple clicks. A press must land on the same cell to extend a run.
func (m *Model) countClick(msg tea.MouseClickMsg) int {
	t := now()
	c := &m.click
	if c.button == msg.Button && c.x == msg.X && c.y == msg.Y && t.Sub(c.at) < multiClickWindow && c.n < 3 {
		c.n++
	} else {
		c.n = 1
	}
	c.x, c.y, c.at, c.button = msg.X, msg.Y, t, msg.Button
	return c.n
}

func (m *Model) mouseClick(msg tea.MouseClickMsg) tea.Cmd {
	m.drag.kind = dragNone // a new press ends any gesture whose release was lost
	n := m.countClick(msg)
	x, y := msg.X, msg.Y
	shift := msg.Mod&tea.ModShift != 0

	if m.menu != nil {
		if m.menu.rect.Contains(x, y) {
			if msg.Button == tea.MouseLeft {
				return m.menuPick(m.menu.itemAt(y))
			}
			return nil
		}
		m.menu = nil
		if msg.Button != tea.MouseRight {
			return nil
		}
	}
	if m.modal != nil {
		r := m.modalRect()
		if !r.Contains(x, y) {
			return nil
		}
		if msg.Button == tea.MouseRight {
			return m.modal.rightClick(m, x, y)
		}
		if msg.Button == tea.MouseLeft {
			m.drag.kind = dragModal
			cmd, closed := m.modal.click(m, x, y, n, shift)
			if closed {
				m.modal = nil
				m.drag.kind = dragNone
			}
			return cmd
		}
		return nil
	}

	l := m.lay
	if msg.Button == tea.MouseRight {
		return m.rightClick(x, y)
	}
	if msg.Button != tea.MouseLeft {
		return nil
	}

	if l.toolbar.Contains(x, y) {
		for _, b := range l.buttons {
			if b.r.Contains(x, y) {
				return m.pressButton(b)
			}
		}
		return nil
	}
	if k := m.splitterAt(x, y); k != dragNone {
		m.drag.kind = k
		return nil
	}
	if t, ok := m.resultsTabAt(x, y); ok {
		m.resTab, m.focus = t, focusGrid
		return nil
	}

	switch {
	case l.editor.Contains(x, y):
		m.focus = focusEditor
		if m.editor.view.Contains(x, y) || x >= m.editor.view.X {
			m.editor.Click(x, y, n, shift)
			m.drag.kind = dragEditor
		}
	case l.results.Contains(x, y):
		m.focus = focusGrid
		if m.resTab == tabPlan && m.planv.plan != nil {
			return m.planClick(x, y, n)
		}
		return m.gridClick(x, y, n, shift)
	case l.conns.Contains(x, y):
		m.focus = focusConns
		if i := m.conns.indexAt(x, y); i >= 0 {
			m.conns.cur = i
			return m.connPicked()
		}
	case l.tables.Contains(x, y):
		m.focus = focusTables
		if i := m.tables.indexAt(x, y); i >= 0 {
			m.tables.cur = i
			if n == 2 {
				return m.tablePicked()
			}
		}
	case l.logR.Contains(x, y):
		m.focus = focusLog
	case l.chat.Contains(x, y):
		m.focus = focusChat
		return m.chatClick(x, y, n, shift)
	}
	return nil
}

// gridClick handles a left press in the results pane.
func (m *Model) gridClick(x, y, n int, shift bool) tea.Cmd {
	g := m.grid
	h := g.hitAt(x, y)
	switch h.kind {
	case hitHeader:
		g.Sort(h.col)
	case hitBorder:
		// Double-click fits the column to its content, as in a spreadsheet.
		// The first click of the pair already started a drag, but with no
		// motion in between it changed nothing, so fitting on the second
		// press needs nothing undone.
		if n == 2 {
			g.Fit(h.col)
			return nil
		}
		g.startResize(h.col)
		m.drag.kind = dragGridCol
	case hitRowNum:
		// a click on a row number selects the whole row, as in a spreadsheet
		g.moveTo(h.row, 0, shift)
		if !shift {
			g.anc, g.sel = cell2{h.row, 0}, true
		}
		g.cur = cell2{h.row, g.Cols() - 1}
		g.ensureVisible()
		m.drag.kind = dragGrid
	case hitCell:
		if n == 2 && !shift {
			g.moveTo(h.row, h.col, false)
			m.openInspect()
			return nil
		}
		g.moveTo(h.row, h.col, shift)
		m.drag.kind = dragGrid
	case hitVBar:
		g.vbarJump(y)
		m.drag.kind = dragGridBar
	}
	return nil
}

// splitterAt returns the splitter under (x, y), if any.
func (m *Model) splitterAt(x, y int) dragKind {
	l := m.lay
	switch {
	case l.splitSide.Contains(x, y):
		return dragSplitSide
	case l.splitChat.Contains(x, y):
		return dragSplitChat
	case l.splitEd.Contains(x, y):
		return dragSplitEd
	case l.splitLog.Contains(x, y):
		return dragSplitLog
	}
	return dragNone
}

func (m *Model) mouseRelease(tea.MouseReleaseMsg) tea.Cmd {
	m.drag.kind = dragNone
	return nil
}

func (m *Model) mouseMotion(msg tea.MouseMotionMsg) tea.Cmd {
	x, y := msg.X, msg.Y
	// Only a held left button continues a drag. Some terminals keep sending
	// motion with no button after a release they swallowed; treating that as
	// a drag would select text the user is merely pointing at.
	if m.drag.kind != dragNone && msg.Button == tea.MouseLeft {
		m.dragTo(x, y)
		return nil
	}
	if m.drag.kind != dragNone && msg.Button == tea.MouseNone {
		m.drag.kind = dragNone
	}

	// hover feedback
	m.hover = hoverState{}
	m.grid.hover, m.grid.hoverH, m.grid.hoverB = cell2{-1, -1}, -1, -1
	m.conns.hover, m.tables.hover = -1, -1
	if m.menu != nil {
		if i := m.menu.itemAt(y); m.menu.rect.Contains(x, y) && i >= 0 {
			m.menu.cur = i
		}
		return nil
	}
	if m.modal != nil {
		m.modal.hover(m, x, y)
		return nil
	}
	if m.lay.toolbar.Contains(x, y) {
		for _, b := range m.lay.buttons {
			if b.r.Contains(x, y) && b.enabled {
				m.hover.btn = b.id
			}
		}
		return nil
	}
	m.hover.split = m.splitterAt(x, y)
	if m.resTab == tabPlan && m.planv.plan != nil {
		m.planv.hover(x, y)
	} else {
		switch h := m.grid.hitAt(x, y); h.kind {
		case hitCell:
			m.grid.hover = cell2{h.row, h.col}
		case hitHeader:
			m.grid.hoverH = h.col
		case hitBorder:
			m.grid.hoverB = h.col
		}
	}
	m.conns.hover = m.conns.indexAt(x, y)
	m.tables.hover = m.tables.indexAt(x, y)
	m.chatHover(x, y)
	return nil
}

// dragTo continues the gesture in progress.
func (m *Model) dragTo(x, y int) {
	switch m.drag.kind {
	case dragSplitSide:
		m.sideW = max(minSide, x+1)
	case dragSplitChat:
		m.chatW = max(minChat, m.w-x)
	case dragSplitEd:
		// the editor's share of the space above the log
		top := m.lay.editor.Y
		above := m.lay.editor.H + m.lay.results.H
		if above > 0 {
			m.edFrac = max(0.1, min(float64(y-top+1)/float64(above), 0.9))
		}
	case dragSplitLog:
		m.logH = max(minLog, m.lay.status.Y-y)
	case dragEditor:
		m.editor.Drag(x, y)
	case dragGrid:
		m.grid.dragTo(x, y)
	case dragGridBar:
		m.grid.vbarJump(y)
	case dragGridCol:
		m.grid.resizeTo(x)
	case dragChatInput:
		m.chat.input.Drag(x, y)
	case dragModal:
		if m.modal != nil {
			m.modal.drag(m, x, y)
		}
	}
}

// mouseWheel scrolls whatever is under the pointer — not necessarily the
// focused pane, which is how every desktop app behaves and what a mouse user
// expects. Shift+wheel, or a horizontal wheel, scrolls sideways.
func (m *Model) mouseWheel(msg tea.MouseWheelMsg) tea.Cmd {
	x, y := msg.X, msg.Y
	dy, dx := 0, 0
	switch msg.Button {
	case tea.MouseWheelUp:
		dy = -3
	case tea.MouseWheelDown:
		dy = 3
	case tea.MouseWheelLeft:
		dx = -1
	case tea.MouseWheelRight:
		dx = 1
	}
	if msg.Mod&tea.ModShift != 0 && dy != 0 {
		dx, dy = sign(dy), 0
	}
	if m.menu != nil {
		return nil
	}
	if m.modal != nil {
		if m.modalRect().Contains(x, y) {
			m.modal.wheel(m, x, y, dy)
		}
		return nil
	}
	l := m.lay
	switch {
	case l.editor.Contains(x, y):
		m.editor.Scroll(dy, dx*4)
	case l.results.Contains(x, y):
		if m.resTab == tabPlan && m.planv.plan != nil {
			m.planv.wheel(x, y, dy)
			return nil
		}
		m.grid.Scroll(dy, dx)
	case l.logR.Contains(x, y):
		m.logp.scroll(dy)
	case l.conns.Contains(x, y):
		m.conns.scroll(dy)
	case l.tables.Contains(x, y):
		m.tables.scroll(dy)
	case l.chat.Contains(x, y):
		m.chat.scroll(dy)
	}
	return nil
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// pressButton runs a toolbar button.
func (m *Model) pressButton(b button) tea.Cmd {
	if !b.enabled {
		return nil
	}
	switch b.id {
	case btnRun:
		return m.runQuery()
	case btnStop:
		return m.cancelRun()
	case btnExplain:
		return m.explainQuery(false)
	case btnCopy:
		m.openCopyMenu(b.r.X, b.r.Y+1)
	case btnExport:
		m.openExport()
	case btnHistory:
		m.openHistory()
	case btnScripts:
		m.openScripts()
	case btnTables:
		return m.listTables()
	case btnAssistant:
		return m.toggleChat()
	case btnConn:
		m.openConnMenu(b.r.X, b.r.Y+1)
	}
	return nil
}

// rightClick opens the context menu for what is under (x, y). The click
// first moves the cursor onto the thing clicked — unless it lands inside the
// current selection — so the menu and the keyboard agree on its target.
func (m *Model) rightClick(x, y int) tea.Cmd {
	l := m.lay
	switch {
	case l.editor.Contains(x, y):
		m.focus = focusEditor
		if _, a, b := m.editor.Selection(); a == b {
			m.editor.Click(x, y, 1, false)
		}
		m.openEditorMenu(x, y)
	case l.results.Contains(x, y) && m.resTab == tabPlan && m.planv.plan != nil:
		m.focus = focusGrid
		m.planv.selectAt(x, y) // the menu acts on the step clicked, as the grid's does on its cell
		m.openPlanMenu(x, y)
	case l.results.Contains(x, y):
		m.focus = focusGrid
		g := m.grid
		switch h := g.hitAt(x, y); {
		case h.kind == hitCell && !g.inSel(h.row, h.col):
			g.moveTo(h.row, h.col, false)
		case h.kind == hitHeader || h.kind == hitBorder:
			// a header's menu acts on that column ("Hide name"), so the
			// cursor moves onto it — unless it is a column of the range,
			// whose menu then hides the range's columns. The cursor is set
			// directly rather than with moveTo, which does nothing on a
			// zero-row result, where hiding a column still makes sense.
			_, c0, _, c1 := g.bounds()
			if !g.sel || h.col < c0 || h.col > c1 {
				g.cur.col, g.sel = h.col, false
			}
		}
		m.openGridMenu(x, y)
	case l.tables.Contains(x, y):
		m.focus = focusTables
		if i := m.tables.indexAt(x, y); i >= 0 {
			m.tables.cur = i
			m.openTableMenu(x, y)
		}
	case l.conns.Contains(x, y):
		m.focus = focusConns
		m.openConnMenu(x, y)
	case l.logR.Contains(x, y):
		m.focus = focusLog
		m.openLogMenu(x, y)
	case l.chat.Contains(x, y):
		m.focus = focusChat
		m.chatRightClick(x, y)
	}
	return nil
}
