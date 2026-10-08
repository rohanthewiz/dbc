package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/workspace"
)

// The results pane per connection, and its result tabs.
//
// The workspace keeps a RESULT SET per connection a query tab has been on
// (workspace/results.go): a strip of result tabs, the plan, the last
// statement and error. Switching the tab's connection shows the new
// connection's set; switching back shows the old one as it was left. A run
// replaces the result in the current result tab unless that tab is pinned,
// when it opens a new one; result_tabs caps the strip.
//
// The workspace owns WHICH result is on screen. What the TUI owns is how
// each one is LOOKED AT — a grid's sort, hidden columns, widths, cursor and
// scroll, and a plan view's mode, fold and selection — and that view has to
// survive a trip to another result tab or connection and back, or keeping
// the results would not be worth much. So the Model keeps parked widgets,
// the same way the query-tab swap does (tabs.go): the one on screen IS
// m.grid / m.planv, the rest wait in maps.
//
//	m.grid  ◄── the active connection's current result tab (gridConn, gridFor)
//	m.rgrids[conn][id]  ◄── every other result tab's grid, parked
//	m.planv, m.resTab  ◄── the active connection's plan view (planFor)
//	m.plans[conn]  ◄── every other connection's, parked
//
// syncResults reconciles them with the workspace after anything that can
// move the current result: a connect landing, a run or a script's show, an
// explain, a disconnect, a result tab shown, pinned or closed, a query tab
// coming on screen. It is idempotent and cheap when nothing moved, so the
// rule is simply "call it after any of those" rather than working out each
// time which part changed.
//
// The result tabs are drawn on the results pane's BOTTOM border, which was
// unused — the top border already carries the title, the Results │ ◈ Plan
// tabs and a script's "Result 1 · 2 · 3" switcher, and a spreadsheet's
// sheet tabs sit at the bottom too:
//
//	╰─ 1 select * from cats · 2⚑ orders ↻ · 3✦ columns cats ── { } switch · r rerun · P pin · x close ─╯
//	   └────── a chip ─────┘  └ current ┘│ └ shared with the assistant ┘
//	                                     └ a click reruns the current tab's query (r)
//
// A tab SHARED with the assistant (s, or its menu) sends its result with
// every question on the connection, whichever tab is on screen; the
// workspace keeps it like a pinned one. Its view (hidden columns, sort)
// goes along from its parked grid — see chatContext.
//
// Where the labels do not fit, their titles are cut; where even bare numbers
// do not, a compact "‹ 2/7 ›" steps instead, as the script switcher does.

// connPane is what the results pane keeps per connection besides its result
// tabs: the plan view, and whether the pane was on the grid or the plan.
type connPane struct {
	planv  *planView
	resTab resultsTab
}

// rtabChip is one clickable part of the result-tab strip, as drawn: a click
// shows result tab id. arrow marks the compact form's ‹ and ›, which step
// rather than name a tab (a right-click on one opens no menu).
type rtabChip struct {
	id    int
	r     Rect
	arrow bool
}

// syncResults makes the results pane show what the workspace says is
// current: the active connection's plan view, and its current result tab's
// grid. See the file comment for why it is one idempotent function.
func (m *Model) syncResults() {
	conn := m.ws.Active()
	if conn != m.planFor {
		if m.plans == nil {
			m.plans = map[string]connPane{}
		}
		m.plans[m.planFor] = connPane{planv: m.planv, resTab: m.resTab}
		p, ok := m.plans[conn]
		delete(m.plans, conn)
		if !ok {
			// a connection this tab has no view of yet: a fresh one, on
			// the plan the workspace already holds for it (if any)
			p = connPane{planv: newPlanView(), resTab: tabResults}
			if pl := m.ws.Plan(); pl != nil {
				p.planv.set(pl)
			}
		}
		m.planv, m.resTab, m.planFor = p.planv, p.resTab, conn
	}

	tabs, cur := m.ws.ResultTabs()
	id, res, seq, rerunOf := 0, (*model.Result)(nil), 0, 0
	if cur >= 0 {
		id, res, seq, rerunOf = tabs[cur].ID, tabs[cur].Result, tabs[cur].Seq, tabs[cur].RerunOf
	}
	if conn != m.gridConn || id != m.gridFor {
		m.parkGrid()
		g := m.takeGrid(conn, id)
		if g == nil {
			g = m.seedGrid()
		}
		m.grid, m.gridConn, m.gridFor = g, conn, id
	}
	// self-heal: a run replaced the result in this very tab (same id), or
	// a script's switcher moved within it. SetResult keeps the hidden
	// columns and widths when the columns are the same, as a rerun's
	// always are; a different query starts fresh.
	//
	// A RERUN of the result this grid was showing (r: its RerunOf is the
	// grid's seq) keeps the sort too — SetRerun. Matched on the seq rather
	// than "the result is a rerun's": a grid parked while its tab was
	// refilled more than once (an edited run, then a rerun of THAT) holds
	// a different question's result, whose sort means nothing here.
	if m.grid.res != res {
		if rerunOf != 0 && rerunOf == m.grid.seq {
			m.grid.SetRerun(res, m.cfg.MaxDisplayRows)
		} else {
			m.grid.SetResult(res, m.cfg.MaxDisplayRows)
		}
	}
	m.grid.seq = seq
	m.pruneGrids(conn, tabs)
}

// parkGrid puts m.grid away under the result tab it shows. A grid showing
// no tab (an empty set) is not kept: there is nothing to come back to.
func (m *Model) parkGrid() {
	if m.gridFor == 0 {
		return
	}
	if m.rgrids == nil {
		m.rgrids = map[string]map[int]*grid{}
	}
	if m.rgrids[m.gridConn] == nil {
		m.rgrids[m.gridConn] = map[int]*grid{}
	}
	m.rgrids[m.gridConn][m.gridFor] = m.grid
}

// takeGrid takes result tab id's parked grid on conn out of the parking,
// or returns nil when it has none (a tab never looked at, or no tab).
func (m *Model) takeGrid(conn string, id int) *grid {
	if id == 0 {
		return nil
	}
	g := m.rgrids[conn][id]
	delete(m.rgrids[conn], id)
	return g
}

// seedGrid makes the grid for a result tab that has none yet — usually a
// run's new tab, because the one it would have replaced is pinned. It
// starts from the grid on screen's layout: the turn (transposed or not)
// belongs to the pane, not to one result (see grid.SetResult), and hidden
// columns and hand-set widths carry over to a result with the very same
// columns, as a rerun's do. SetResult, called by syncResults, decides
// which of that applies.
func (m *Model) seedGrid() *grid {
	old := m.grid
	g := newGrid()
	g.flip = old.flip
	if old.res != nil {
		g.res = old.res
		g.hidden, g.userW = slices.Clone(old.hidden), slices.Clone(old.userW)
		g.recW, g.namesW = old.recW, old.namesW
	}
	return g
}

// pruneGrids drops parked grids of conn's result tabs that are gone —
// closed, or dropped off the front to keep to result_tabs. Only the active
// connection is pruned: it is the only set the workspace lets us list, and
// the only one whose tabs can have changed since its grids were parked
// (a run landing on a connection left mid-run aside, which the next visit
// prunes).
func (m *Model) pruneGrids(conn string, tabs []workspace.ResultTab) {
	for id := range m.rgrids[conn] {
		if !slices.ContainsFunc(tabs, func(t workspace.ResultTab) bool { return t.ID == id }) {
			delete(m.rgrids[conn], id)
		}
	}
}

// planViewFor is conn's plan view: m.planv when conn is the one on screen,
// else its parked one, made when it has none. An explain or an EXPLAIN-run
// that lands after the tab switched away must go to its own connection's
// view, never to the one on screen.
func (m *Model) planViewFor(conn string) *planView {
	if conn == m.planFor {
		return m.planv
	}
	if m.plans == nil {
		m.plans = map[string]connPane{}
	}
	p, ok := m.plans[conn]
	if !ok {
		p = connPane{planv: newPlanView(), resTab: tabResults}
		m.plans[conn] = p
	}
	return p.planv
}

// showParkedPlan turns a parked connection's pane to its plan, for when it
// comes back on screen — what an explain landing on screen does at once.
func (m *Model) showParkedPlan(conn string) {
	if p, ok := m.plans[conn]; ok {
		p.resTab = tabPlan
		m.plans[conn] = p
	}
}

// ---------------------------------------------------------------------------
// Commands on the result tabs
// ---------------------------------------------------------------------------

// curResultTab is the active connection's current result tab, its 1-based
// position in the strip, and how many tabs there are; ok false with none.
func (m *Model) curResultTab() (t workspace.ResultTab, pos, n int, ok bool) {
	tabs, cur := m.ws.ResultTabs()
	if cur < 0 {
		return workspace.ResultTab{}, 0, len(tabs), false
	}
	return tabs[cur], cur + 1, len(tabs), true
}

// noResultTab is said when a result-tab command finds none.
const noResultTab = "no result tab — run a query first"

// showResultTab puts result tab id on screen: a strip click, { and }. The
// pane turns to the grid, since a tab is picked to see its rows.
func (m *Model) showResultTab(id int) tea.Cmd {
	if err := m.ws.ShowResultTab(id); err != nil {
		m.refused(err)
		return nil
	}
	m.resTab = tabResults
	m.syncResults()
	m.focus = focusGrid
	if r := m.ws.LastResult(); r != nil {
		m.setStatus(resultStatus(r, m.cfg.MaxRows, m.grid.Rows()))
	}
	return nil
}

// stepResultTab is { (d = -1) and } (d = +1): the previous or next result
// tab. It stops at either end rather than wrapping, as [ and ] do.
func (m *Model) stepResultTab(d int) tea.Cmd {
	tabs, cur := m.ws.ResultTabs()
	if len(tabs) < 2 {
		m.log(logWarn, "one result tab or none — { and } switch between the result tabs of this connection (pin one with P to keep it)")
		return nil
	}
	i := cur + d
	if i < 0 || i >= len(tabs) {
		return nil
	}
	return m.showResultTab(tabs[i].ID)
}

// togglePin is P: pin the current result tab, or unpin it.
func (m *Model) togglePin() tea.Cmd {
	t, pos, _, ok := m.curResultTab()
	if !ok {
		m.log(logWarn, noResultTab)
		return nil
	}
	return m.pinResultTab(t.ID, pos, !t.Pinned)
}

// pinResultTab pins or unpins result tab id (at strip position pos), and
// says what that changes.
func (m *Model) pinResultTab(id, pos int, pin bool) tea.Cmd {
	if err := m.ws.PinResultTab(id, pin); err != nil {
		m.refused(err)
		return nil
	}
	if pin {
		m.logf(logInfo, "pinned result %d — the next run opens a new tab (r reruns this one in place, P unpins)", pos)
	} else {
		m.logf(logInfo, "unpinned result %d — a run on it replaces its result", pos)
	}
	return nil
}

// rerunCurResultTab is r: run the current result tab's statement again,
// into that tab — pinned or not (workspace.RerunResultTab). A statement
// that may write asks first, in a menu at the grid's cursor: the key sits
// among the grid's own, and an INSERT run twice by a stray keypress is not
// undone by a second one.
func (m *Model) rerunCurResultTab() tea.Cmd {
	// below the grid's cursor; on the plan view, where the grid is not
	// drawn, at the pane's top left as the plan's own menu opens
	x, y := m.grid.cursorScreen()
	if m.resTab == tabPlan && m.planv.plan != nil {
		x, y = m.planv.area.X+2, m.planv.area.Y+2
	}
	return m.rerunCurResultTabAt(x, y+1)
}

// rerunCurResultTabAt is rerunCurResultTab with the confirm for a write
// opened at (x, y): r puts it at the grid's cursor, a click on the strip's
// ↻ where the click was, so the question comes up under the pointer that
// asked for it.
func (m *Model) rerunCurResultTabAt(x, y int) tea.Cmd {
	t, pos, _, ok := m.curResultTab()
	if !ok {
		m.log(logWarn, noResultTab)
		return nil
	}
	if t.Writes {
		m.openMenu(x, y, []menuItem{
			heading(fmt.Sprintf("result %d's statement may change the database", pos)),
			{label: "Keep the result as it is", act: func(m *Model) tea.Cmd { return nil }},
			{label: "↻ Run it again — " + truncate(t.Title, rtabMaxTitle),
				act: func(m *Model) tea.Cmd { return m.rerunResultTab(t.ID) }},
		})
		return nil
	}
	return m.rerunResultTab(t.ID)
}

// rerunResultTab reruns result tab id's statement as a run: the running
// status, the ticker, and its RunDone landing in that tab (syncResults
// then keeps the tab's grid — its hidden columns, widths and sort — when
// the columns come back the same, as a refresh's do). A refusal (busy, a
// script's tab) goes to the log.
func (m *Model) rerunResultTab(id int) tea.Cmd {
	return m.startRun(m.ws.RerunResultTab(id))
}

// closeCurResultTab is x: close the current result tab.
func (m *Model) closeCurResultTab() tea.Cmd {
	t, pos, _, ok := m.curResultTab()
	if !ok {
		m.log(logWarn, noResultTab)
		return nil
	}
	return m.closeResultTab(t.ID, pos)
}

// closeResultTab closes result tab id (at strip position pos), pinned or
// not: closing is asked for by name, so a pin does not stand in its way.
func (m *Model) closeResultTab(id, pos int) tea.Cmd {
	if err := m.ws.CloseResultTab(id); err != nil {
		m.refused(err)
		return nil
	}
	m.syncResults()
	m.logf(logInfo, "closed result %d", pos)
	return nil
}

// closeUnpinnedResultTabs closes every unpinned result tab of the
// connection.
func (m *Model) closeUnpinnedResultTabs() tea.Cmd {
	n := m.ws.CloseUnpinnedResultTabs()
	m.syncResults()
	m.logf(logInfo, "closed %s", plural(n, "unpinned result tab"))
	return nil
}

// toggleShare is s: share the current result tab with the assistant, or
// stop sharing it.
func (m *Model) toggleShare() tea.Cmd {
	t, pos, _, ok := m.curResultTab()
	if !ok {
		m.log(logWarn, noResultTab)
		return nil
	}
	return m.shareResultTab(t.ID, pos, !t.Shared)
}

// shareResultTab shares result tab id (at strip position pos) with the
// assistant, or stops. The workspace refuses a share on a connection
// without ai_rows, in words naming the setting; that goes to the log.
func (m *Model) shareResultTab(id, pos int, share bool) tea.Cmd {
	if err := m.ws.ShareResultTab(id, share); err != nil {
		m.refused(err)
		return nil
	}
	if share {
		m.logf(logInfo, "shared result %d with the assistant — it goes with every question on %s until you stop sharing (S)",
			pos, m.ws.Active())
	} else {
		m.logf(logInfo, "stopped sharing result %d with the assistant", pos)
	}
	return nil
}

// sharedParkedGrid is the parked grid of the result tab shared with the
// assistant, when that tab is not the one on screen and has been looked at
// (a grid that was never drawn holds no view the user set); nil otherwise.
// It must still be on the tab's result: a grid parked before a rerun is
// not that result's view.
func (m *Model) sharedParkedGrid() *grid {
	tabs, _ := m.ws.ResultTabs()
	for _, t := range tabs {
		if !t.Shared || t.ID == m.gridFor {
			continue
		}
		if g := m.rgrids[m.ws.Active()][t.ID]; g != nil && g.res == t.Result {
			return g
		}
	}
	return nil
}

// resultTabKey handles the result-tab keys, shared by the grid and the plan
// view: { } step, P pins, r reruns, S (or s) shares with the assistant, x
// closes.
// used is false for any other key. S is the documented key because it is
// dbc web's too, where s is already the grid's sort; s is kept here, where
// it is free, as the shorter way to the same toggle.
func (m *Model) resultTabKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "{":
		return m.stepResultTab(-1), true
	case "}":
		return m.stepResultTab(1), true
	case "P":
		return m.togglePin(), true
	case "r":
		return m.rerunCurResultTab(), true
	case "S", "s":
		return m.toggleShare(), true
	case "x":
		return m.closeCurResultTab(), true
	}
	return nil, false
}

// resultTabItems are the result-tab rows of a menu (the strip's, and the
// grid's), acting on the current tab: rerun, pin, share with the
// assistant, close, close the unpinned ones; none when there is no tab.
func (m *Model) resultTabItems() []menuItem {
	t, pos, n, ok := m.curResultTab()
	if !ok {
		return nil
	}
	pin := menuItem{label: "Pin this result", key: "P",
		act: func(m *Model) tea.Cmd { return m.pinResultTab(t.ID, pos, true) }}
	if t.Pinned {
		pin = menuItem{label: "Unpin this result", key: "P",
			act: func(m *Model) tea.Cmd { return m.pinResultTab(t.ID, pos, false) }}
	}
	// sharing needs the connection's ai_rows; without it the row says so
	// rather than leading to a refusal (unsharing always works)
	share := menuItem{label: "✦ Share with the assistant", key: "S",
		act: func(m *Model) tea.Cmd { return m.shareResultTab(t.ID, pos, true) }}
	if !m.ws.CanShareResults() {
		share.why = "set ai_rows = true on " + m.ws.Active() + " to share its results"
	}
	if t.Shared {
		share = menuItem{label: "✦ Stop sharing with the assistant", key: "S",
			act: func(m *Model) tea.Cmd { return m.shareResultTab(t.ID, pos, false) }}
	}
	// rerun goes through r's own path, so a write is asked about from the
	// menu too: a menu row is one misclick from its neighbours (dbc web's
	// row does the same). The menu acts on the current tab, which is t.
	rerun := menuItem{label: "↻ Rerun its query", key: "r",
		act: func(m *Model) tea.Cmd { return m.rerunCurResultTab() }}
	switch {
	case t.Stmt == "":
		rerun.why = "a script's results — run the script again to refresh them"
	case t.Writes:
		rerun.label = "↻ Rerun its query — it writes again"
	}
	items := []menuItem{rerun, pin, share,
		{label: "Close this result tab", key: "x", act: func(m *Model) tea.Cmd { return m.closeResultTab(t.ID, pos) }}}
	tabs, _ := m.ws.ResultTabs()
	if unpinned := countUnpinned(tabs); unpinned > 0 && n > 1 {
		items = append(items, menuItem{label: fmt.Sprintf("Close unpinned result tabs (%d)", unpinned),
			act: func(m *Model) tea.Cmd { return m.closeUnpinnedResultTabs() }})
	}
	return items
}

// countUnpinned counts the tabs "Close unpinned" would close: not pinned,
// and not shared with the assistant (which keeps a tab as a pin does).
func countUnpinned(tabs []workspace.ResultTab) int {
	n := 0
	for _, t := range tabs {
		if !t.Pinned && !t.Shared {
			n++
		}
	}
	return n
}

// openResultTabMenu is a strip label's right-click menu. The tab clicked
// comes on screen first, so the menu acts on the tab the user sees — the
// grammar of the query tabs' menu (openTabMenu).
func (m *Model) openResultTabMenu(id, x, y int) tea.Cmd {
	cmd := m.showResultTab(id)
	_, pos, n, ok := m.curResultTab()
	if !ok {
		return cmd
	}
	conn := m.ws.Active()
	head := fmt.Sprintf("result %d of %d · at most %d on %s (result_tabs)", pos, n, m.ws.ResultTabLimit(), conn)
	m.openMenu(x, y, append([]menuItem{heading(head)}, m.resultTabItems()...))
	return cmd
}

// ---------------------------------------------------------------------------
// The strip
// ---------------------------------------------------------------------------

// rtabPart is one run of text in the strip before it is placed: a chip
// (to > 0, the result tab it shows), the current tab (on), the current
// tab's ↻ (rerun), or plain text.
type rtabPart struct {
	text  string
	to    int  // the result tab a click shows; 0 for text that is not a chip
	on    bool // the tab on screen
	arrow bool // the compact form's ‹ or ›
	rerun bool // the ↻ after the current tab: a click reruns its query
}

// rtabRerun is the ↻ drawn after the current tab's label. Only the current
// tab gets one: a click on any other label shows it first (and then its ↻
// is there), and one on every label would cost the titles two cells each.
// It follows the label's own trailing space, so it reads " 2 orders ↻ ".
const rtabRerun = "↻ "

// rtabMaxTitle bounds one label's title: a long statement preview would
// otherwise take the whole border.
const rtabMaxTitle = 24

// rtabHint is drawn after the labels when there is room for it, so the keys
// are discoverable without the help dialog; rtabShareHint where the
// connection may share results with the assistant (ai_rows).
const (
	rtabHint      = " { } switch · r rerun · P pin · x close "
	rtabShareHint = " { } switch · r rerun · P pin · S share · x close "
)

// resultTabParts lays out the strip for a border w cells wide (the corners
// and one ─ either side excluded by the caller): the labels with titles cut
// to the widest length that fits, else bare numbers, else the compact form,
// else nothing.
func resultTabParts(tabs []workspace.ResultTab, cur, w int) []rtabPart {
	if len(tabs) == 0 || w <= 0 {
		return nil
	}
	label := func(i int, tw int) string {
		t := tabs[i]
		s := " " + itoa(i+1) + tabMarks(t)
		if tw > 0 {
			title := strings.Join(strings.Fields(t.Title), " ")
			if title != "" {
				s += " " + truncate(title, tw)
			}
		}
		return s + " "
	}
	// a script's tab has no statement to rerun (RerunResultTab refuses
	// it), so it gets no ↻ rather than one that only logs a refusal
	rerun := cur >= 0 && cur < len(tabs) && tabs[cur].Stmt != ""
	build := func(tw int) []rtabPart {
		var out []rtabPart
		for i, t := range tabs {
			if i > 0 {
				out = append(out, rtabPart{text: "·"})
			}
			out = append(out, rtabPart{text: label(i, tw), to: t.ID, on: i == cur})
			if i == cur && rerun {
				out = append(out, rtabPart{text: rtabRerun, rerun: true})
			}
		}
		return out
	}
	// the widest title length that fits, from the cap down to a few cells
	// (below that a title reads as noise, and numbers alone are clearer)
	for tw := rtabMaxTitle; tw >= 4; tw-- {
		if p := build(tw); rtabWidth(p) <= w {
			return p
		}
	}
	if p := build(0); rtabWidth(p) <= w {
		return p
	}
	// compact: ‹ and › step one either way; at an end the arrow is drawn
	// but is no chip, so a click there does nothing rather than wrap
	prev, next := 0, 0
	if cur > 0 {
		prev = tabs[cur-1].ID
	}
	if cur >= 0 && cur+1 < len(tabs) {
		next = tabs[cur+1].ID
	}
	on := ""
	if cur >= 0 {
		on = itoa(cur+1) + "/" + itoa(len(tabs)) + tabMarks(tabs[cur])
	}
	p := []rtabPart{{text: " ‹ ", to: prev, arrow: true}, {text: on, on: true}, {text: " › ", to: next, arrow: true}}
	// the ↻ between the count and the ›, " ‹ 2/7 ↻ › ", when it fits; the
	// steps matter more than it does (r still reruns), so it is the first
	// thing dropped
	if rerun {
		withRerun := slices.Insert(slices.Clone(p), 2, rtabPart{text: " ↻", rerun: true})
		if rtabWidth(withRerun) <= w {
			return withRerun
		}
	}
	if rtabWidth(p) <= w {
		return p
	}
	return nil
}

// tabMarks are a result tab's marks after its number: ⚑ pinned, ✦ shared
// with the assistant.
func tabMarks(t workspace.ResultTab) string {
	s := ""
	if t.Pinned {
		s += "⚑"
	}
	if t.Shared {
		s += "✦"
	}
	return s
}

// rtabWidth is a strip's width in cells.
func rtabWidth(parts []rtabPart) int {
	n := 0
	for _, p := range parts {
		n += width(p.text)
	}
	return n
}

// drawResultTabs draws the strip on the results pane r's bottom border and
// records its chips for clicks. Nothing is drawn with no result tab: an
// empty pane's border stays plain, so a user who never ran anything sees
// no change.
func (m *Model) drawResultTabs(c *Canvas, r Rect) {
	m.lay.rtabChips = m.lay.rtabChips[:0]
	m.lay.rtabRerun = Rect{}
	if r.W < 12 || r.H < 3 {
		return
	}
	tabs, cur := m.ws.ResultTabs()
	// the border from x=2 to W-3: a ─ kept either side of the strip, as the
	// title keeps one after the ╭
	parts := resultTabParts(tabs, cur, r.W-4)
	if len(parts) == 0 {
		return
	}
	bg := m.st.base
	off, on := onBg(m.st.muted, bg), onBg(m.st.title, bg).Bold().Underline()
	if m.focus == focusGrid {
		on = onBg(m.st.titleFocus, bg).Bold().Underline()
	}
	y := r.Y + r.H - 1
	s := c.Sub(Rect{r.X, y, r.W, 1})
	x := 2
	for _, p := range parts {
		st := off
		switch {
		case p.on:
			st = on
		case p.arrow && p.to == 0:
			st = off.Dim() // the dead end of the compact form
		case p.rerun:
			// the current tab's colour, so it reads as that tab's, but not
			// underlined: it is a control beside the label, not part of it
			st = onBg(m.st.title, bg).Bold()
			if m.focus == focusGrid {
				st = onBg(m.st.titleFocus, bg).Bold()
			}
		}
		start := x
		x = s.Put(x, 0, p.text, st)
		if p.to > 0 && !p.on {
			m.lay.rtabChips = append(m.lay.rtabChips, rtabChip{id: p.to, r: Rect{r.X + start, y, x - start, 1}, arrow: p.arrow})
		}
		if p.rerun {
			// the glyph's own cell, not the space beside it: a click just
			// past the ↻ only focuses the results. Cells, not bytes, before it.
			gx := start + width(p.text[:strings.Index(p.text, "↻")])
			m.lay.rtabRerun = Rect{r.X + gx, y, width("↻"), 1}
		}
	}
	// the hint ends one ─ short of the ╯, with at least one ─ before it
	hint := rtabHint
	if m.ws.CanShareResults() {
		hint = rtabShareHint
	}
	if x+width(hint)+1 <= r.W-2 {
		s.PutRight(r.W-2, 0, hint, off.Italic().Dim())
	}
}

// resultTabRerunAt reports whether (x, y) is on the current tab's ↻.
func (m *Model) resultTabRerunAt(x, y int) bool {
	return m.lay.rtabRerun.Contains(x, y)
}

// resultTabAt reports the strip chip at (x, y): the result tab it shows,
// and whether it is a step arrow rather than a tab's label.
func (m *Model) resultTabAt(x, y int) (id int, arrow, ok bool) {
	for _, ch := range m.lay.rtabChips {
		if ch.r.Contains(x, y) {
			return ch.id, ch.arrow, true
		}
	}
	return 0, false, false
}

// resultTabLabelAt is resultTabAt for a right-click, which also opens on
// the current tab's own label (not a chip, since a click on it does
// nothing) — the strip's menu is reached from any label.
func (m *Model) resultTabLabelAt(x, y int) (int, bool) {
	if id, arrow, ok := m.resultTabAt(x, y); ok && !arrow {
		return id, true
	}
	r := m.lay.results
	if r.Empty() || y != r.Y+r.H-1 || x < r.X+2 || x >= r.X+r.W-2 {
		return 0, false
	}
	t, _, _, ok := m.curResultTab()
	if !ok {
		return 0, false
	}
	// between the chips: the current label sits where no chip is, so a
	// click in the strip that hit nothing else is on it — or on a · or the
	// hint, which may as well open the same menu
	return t.ID, true
}
