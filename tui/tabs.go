package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/userdata"
	"github.com/rohanthewiz/dbc/workspace"
)

// Query tabs: several workspaces in one TUI, as dbc web's query tabs are.
// Each tab has its own pinned session (a BEGIN in one does not leak into
// another), its own console, result, plan and Tables list; the
// connections list, the log, the assistant and the layout are shared.
//
// THE SWAP. The Model was built around one workspace and one set of
// widgets (m.ws, m.editor, m.grid, …), and every handler reads those
// fields. Rather than thread a tab through every handler, the tab on
// screen IS those fields: switching parks the Model's per-tab fields in
// the tab being left and loads the next tab's into them. Everything that
// draws or handles keys keeps working on "the" workspace, unchanged.
//
//	m.ws, m.editor, m.grid, m.planv, m.tables, console…  ◄── the active tab
//	m.tabs[i]  (parked: its own copies of those fields)  ◄── every other tab
//
// EVENTS FROM THE BACKGROUND. A tab left running keeps running: its Job
// still lands in its own workspace. But the message that draws it (a
// *workspace.RunDone, say) must not be drawn into the tab on screen. So
// every command that comes from a workspace is TAGGED with the tab that
// started it (tabMsg); route unwraps a message for the active tab, and
// queues one for a background tab in that tab's pending list:
//
//	job ──► tabMsg{key, ev} ──► route ─┬─ key is the active tab ─► route(ev)
//	                                   └─ background ─► t.pending += ev,
//	                                                    mark ● running / • done
//	activate(t) ──► load t's fields ──► route(each pending ev), in order
//
// Replaying on activation draws a background run exactly as it would have
// been drawn live, through the same handlers. Two things cannot wait for
// the user to come back: a switch's Release (the old session's rollback,
// and a derived pool's close) runs at once, and the elapsed-time ticker is
// simply dropped and re-armed on activation.
//
// Tabs persist with the layout (userdata.Layout.Tabs): title, connection
// and console. On a restart only the active tab connects; the others are
// lazy and connect on their first activation, so a TUI with ten saved tabs
// does not dial ten connections to draw its first frame.

// queryTab is one query tab. While it is the active tab its fields are
// stale: the live values are the Model's (see park / load).
type queryTab struct {
	key   int // stable identity for tagging events (indices shift on close)
	title string

	ws     *workspace.Workspace
	editor *editor
	grid   *grid
	planv  *planView
	tables *list

	resTab        resultsTab
	schemaLoading string
	status        string
	complSt       complState

	console, consoleName, consoleText string
	consoleDB                         userdata.ConsoleDB

	// gen is the run generation last started on this tab, so its
	// elapsed-time ticker can be re-armed when the tab comes back.
	gen int
	// pending holds the events that landed while the tab was in the
	// background, replayed in order when it is activated.
	pending []tea.Msg
	// done / failed mark a background run that finished (• in the strip,
	// in the error color when it failed), until the tab is looked at.
	done, failed bool
	// lazy is the connection a restored tab opens on its first activation
	// ("" once connected, or for a tab made this run).
	lazy string
}

// tabMsg is a message from a command a tab started, tagged with that tab.
type tabMsg struct {
	key int
	msg tea.Msg
}

// maxTabs bounds the strip: each tab holds a workspace (and, once used, a
// pinned connection), and Alt+1…9 reach only nine anyway.
const maxTabs = 9

// initTabs makes the first tab out of the widgets New built.
func (m *Model) initTabs() {
	m.nextTabKey = 1
	t := &queryTab{key: m.nextTabKey, title: "Query 1"}
	m.tabs = []*queryTab{t}
	m.curTab = 0
}

// active is the tab on screen.
func (m *Model) active() *queryTab { return m.tabs[m.curTab] }

// tag wraps cmd so its message comes back tagged with the active tab — see
// the package comment above. A nil command stays nil.
func (m *Model) tag(cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	key := m.active().key
	return func() tea.Msg {
		msg := cmd()
		if msg == nil {
			return nil
		}
		return tabMsg{key: key, msg: msg}
	}
}

// newWorkspace builds tab key's workspace. hist is the one history every
// tab records into, so a statement run in any tab is recallable from all
// of them, as in dbc web. The sink carries a script's s.Show / s.Print,
// which fire mid-run from the script's goroutine, to Update through
// m.send — tagged, like every other event, with the tab. It reads m.send
// when it fires, not now: Run installs Program.Send after New returns.
//
// No WholeCatalog: the Tables pane has the web's database and schema
// pickers (navigator.go), so a Postgres connection opens on its default
// schema, as in the web, and "all schemas" is a pick away rather than a
// catalog read of every schema on every connect.
func (m *Model) newWorkspace(key int, hist *userdata.History) *workspace.Workspace {
	return workspace.New(m.cfg, m.mgr, hist, workspace.Options{
		Sink: func(e workspace.Event) { m.send(tabMsg{key: key, msg: e}) },
	})
}

// tabByKey finds a tab by its key; nil when it was closed meanwhile.
func (m *Model) tabByKey(key int) *queryTab {
	for _, t := range m.tabs {
		if t.key == key {
			return t
		}
	}
	return nil
}

// routeTab delivers a tagged message: to the handlers when its tab is on
// screen, to the tab's pending list when it is not, and nowhere when the
// tab was closed (its workspace is gone; there is nothing to draw).
func (m *Model) routeTab(tm tabMsg) tea.Cmd {
	if tm.key == m.active().key {
		return m.route(tm.msg)
	}
	t := m.tabByKey(tm.key)
	if t == nil {
		return nil
	}
	var cmd tea.Cmd
	switch ev := tm.msg.(type) {
	case *workspace.Connected:
		// the session left behind is released now, not when the user
		// comes back: it may hold a transaction, and rolling that back
		// should not wait on a tab switch. The replay then has no
		// release left to do.
		if !ev.Stale && ev.Err == nil {
			cmd = m.tagFor(t, m.releaseThenCloseOn(t.ws, ev))
			ev.Release, ev.Left = nil, ""
			m.refreshConns() // its ○ in the connections list moved
		}
	case *workspace.RunDone:
		if !ev.Stale {
			t.done, t.failed = true, ev.Err != nil
		}
	case *workspace.ExplainDone:
		if !ev.Stale {
			t.done, t.failed = true, ev.Err != nil
		}
	}
	t.pending = append(t.pending, tm.msg)
	m.catsAfterTransition() // "working" may have ended with this tab's run
	return cmd
}

// tagFor is tag for a tab that may not be the active one.
func (m *Model) tagFor(t *queryTab, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	key := t.key
	return func() tea.Msg {
		if msg := cmd(); msg != nil {
			return tabMsg{key: key, msg: msg}
		}
		return nil
	}
}

// park copies the Model's per-tab fields into t (the tab being left).
func (m *Model) park(t *queryTab) {
	t.ws, t.editor, t.grid, t.planv, t.tables = m.ws, m.editor, m.grid, m.planv, m.tables
	t.resTab, t.schemaLoading, t.status, t.complSt = m.resTab, m.schemaLoading, m.status, m.complSt
	t.console, t.consoleName, t.consoleText, t.consoleDB = m.console, m.consoleName, m.consoleText, m.consoleDB
}

// load copies t's fields into the Model (the tab coming on screen).
func (m *Model) load(t *queryTab) {
	m.ws, m.editor, m.grid, m.planv, m.tables = t.ws, t.editor, t.grid, t.planv, t.tables
	m.resTab, m.schemaLoading, m.status, m.complSt = t.resTab, t.schemaLoading, t.status, t.complSt
	m.console, m.consoleName, m.consoleText, m.consoleDB = t.console, t.consoleName, t.consoleText, t.consoleDB
	m.compl = nil // a popup belongs to the editor it opened over
}

// activate puts tab i on screen: the current tab is parked, i's fields are
// loaded, and what landed while i was in the background is replayed.
func (m *Model) activate(i int) tea.Cmd {
	if i < 0 || i >= len(m.tabs) || i == m.curTab {
		return nil
	}
	m.park(m.active())
	m.curTab = i
	t := m.active()
	m.load(t)
	m.menu = nil
	t.done, t.failed = false, false
	m.refreshConns() // the ● follows the tab's connection

	var cmds []tea.Cmd
	pending := t.pending
	t.pending = nil
	for _, msg := range pending {
		cmds = append(cmds, m.route(msg))
	}
	if t.lazy != "" {
		// a restored tab's first look: connect it now (see restoreTabs)
		name := t.lazy
		t.lazy = ""
		cmds = append(cmds, m.connectCmd(name))
	}
	if m.ws.Busy() {
		m.setStatus(m.ws.RunningStatus())
		cmds = append(cmds, m.tickCmd(t.gen))
	}
	if m.focus == focusChat && !m.chat.open {
		m.focus = focusEditor
	}
	return tea.Batch(cmds...)
}

// newTab (⌥T) opens a tab on the active tab's connection, with a console of
// that database no other tab shows — or a fresh one — so two tabs on one
// database never write one file. It connects at once: a tab is for
// working in, and its session is its own.
func (m *Model) newTab() tea.Cmd {
	if len(m.tabs) >= maxTabs {
		m.logf(logWarn, "%d tabs is the most — close one first (⌥W)", maxTabs)
		return nil
	}
	conn := m.ws.Active()
	if err := m.saveConsole(); err != nil {
		m.logf(logWarn, "could not save the console: %s", err.Error())
	}
	m.park(m.active())

	m.nextTabKey++
	t := &queryTab{key: m.nextTabKey, title: m.nextTabTitle()}
	t.ws = m.newWorkspace(t.key, m.ws.History())
	t.editor = newEditor(false)
	t.editor.sqlMode = true
	t.editor.placeholder = m.editor.placeholder
	t.grid, t.planv, t.tables = newGrid(), newPlanView(), newList()
	t.status = "ready"
	m.tabs = append(m.tabs, t)
	m.curTab = len(m.tabs) - 1
	m.load(t)
	m.focus = focusEditor

	// the console first, so the connect's landing (switchConsole) finds
	// the editor already on this database and swaps nothing
	m.openTabConsole(conn)
	m.logf(logInfo, "%s — its own session; ⌥1…⌥9 switch, ⌥W closes", t.title)
	if conn == "" {
		return nil
	}
	return m.connectCmd(conn)
}

// nextTabTitle is "Query N" for the lowest N no tab is titled with.
func (m *Model) nextTabTitle() string {
	for n := 1; ; n++ {
		title := fmt.Sprintf("Query %d", n)
		taken := false
		for _, t := range m.tabs {
			if t.title == title {
				taken = true
				break
			}
		}
		if !taken {
			return title
		}
	}
}

// closeTab (⌥W) closes tab i. A session that may hold a transaction is
// asked about first, since closing rolls it back; the last tab stays (its
// editor can be cleared instead), as in dbc web.
func (m *Model) closeTab(i int, confirmed bool) tea.Cmd {
	if len(m.tabs) < 2 {
		m.log(logInfo, "the last tab stays — clear its editor instead")
		return nil
	}
	if i < 0 || i >= len(m.tabs) {
		return nil
	}
	if i != m.curTab {
		// work on it as the active tab, so its fields are the Model's; the
		// replay's follow-up commands go with the tab
		m.activate(i)
	}
	t := m.active()
	if conn, stateful := m.ws.Session(); stateful && !confirmed {
		x, y := m.lay.editor.X+2, m.lay.editor.Y+1
		m.openMenu(x, y, []menuItem{
			heading(fmt.Sprintf("close %s? its session on %s may hold a transaction — closing rolls it back", t.title, conn)),
			{label: "Close the tab and roll back", act: func(m *Model) tea.Cmd { return m.closeTab(m.curTab, true) }},
			{label: "Keep it", act: func(*Model) tea.Cmd { return nil }},
		})
		return nil
	}
	if err := m.saveConsole(); err != nil {
		m.logf(logWarn, "closing %s: could not save its console: %s", t.title, err.Error())
	}
	ws, left := m.ws, m.ws.Active()
	m.tabs = append(m.tabs[:m.curTab], m.tabs[m.curTab+1:]...)
	m.curTab = min(m.curTab, len(m.tabs)-1)
	m.load(m.active())
	m.refreshConns()
	m.logf(logInfo, "closed %s", t.title)

	// the workspace is let go off the UI goroutine: Close waits for a
	// statement still running to return from its cancel. A derived
	// <conn>/<database> pool no other tab is on is closed with it, as a
	// switch away from one does (releaseThenClose).
	closeDerived := left != "" && ws.Derived(left) && !m.tabOn(left)
	mgr := m.mgr
	return func() tea.Msg {
		ws.Close()
		if closeDerived {
			mgr.Disconnect(left)
		}
		return nil
	}
}

// tabOn reports whether any open tab is on (or connecting to) connection
// name.
func (m *Model) tabOn(name string) bool {
	for i, t := range m.tabs {
		ws := t.ws
		if i == m.curTab {
			ws = m.ws
		}
		if ws == nil {
			continue
		}
		if ws.Active() == name {
			return true
		}
		if c, ok := ws.Connecting(); ok && c == name {
			return true
		}
	}
	return false
}

// tabWorkspaces is every tab's workspace, the active one's live value.
func (m *Model) tabWorkspaces() []*workspace.Workspace {
	out := make([]*workspace.Workspace, 0, len(m.tabs))
	for i, t := range m.tabs {
		if i == m.curTab {
			out = append(out, m.ws)
		} else if t.ws != nil {
			out = append(out, t.ws)
		}
	}
	return out
}

// anyBusy reports whether a run is in flight in any tab — what "working"
// means to the cats host, and what Ctrl+C would stop before quitting.
func (m *Model) anyBusy() bool {
	for _, ws := range m.tabWorkspaces() {
		if ws.Busy() {
			return true
		}
	}
	return false
}

// forEachTab runs f with each tab in turn loaded into the Model, then puts
// the active one back — for the few things done to every tab (saving
// their consoles at quit). It replays nothing.
func (m *Model) forEachTab(f func()) {
	cur := m.curTab
	m.park(m.active())
	for i, t := range m.tabs {
		if t.ws == nil {
			continue
		}
		m.curTab = i
		m.load(t)
		f()
		m.park(t)
	}
	m.curTab = cur
	m.load(m.active())
}

// consoleShownElsewhere reports which other tab shows console name of
// database d, if any.
func (m *Model) consoleShownElsewhere(d userdata.ConsoleDB, name string) *queryTab {
	for i, t := range m.tabs {
		if i == m.curTab || t.console == "" {
			continue
		}
		if t.consoleDB == d && t.consoleName == name {
			return t
		}
	}
	return nil
}

// openTabConsole puts a console of conn's database in the active tab's
// (empty) editor: the first one no other tab shows, else a new one.
func (m *Model) openTabConsole(conn string) {
	d, flat, ok := m.consoleOf(conn)
	if !ok {
		return
	}
	names := userdata.ListConsoles(m.consoleDir, d, flat)
	name := ""
	for _, n := range names {
		if m.consoleShownElsewhere(d, n) == nil {
			name = n
			break
		}
	}
	if name == "" {
		name = userdata.NextConsoleName(append(names, m.otherTabConsoles(d)...))
	}
	m.setConsole(d, name)
	text, _ := userdata.LoadConsole(m.console)
	m.consoleText = text
	m.editor.SetText(text)
}

// otherTabConsoles is the consoles of database d the other tabs show, some
// of which may have no file yet.
func (m *Model) otherTabConsoles(d userdata.ConsoleDB) []string {
	var out []string
	for i, t := range m.tabs {
		if i != m.curTab && t.console != "" && t.consoleDB == d {
			out = append(out, t.consoleName)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The strip
// ---------------------------------------------------------------------------

// tabChip is one clickable tab in the strip, as drawn.
type tabChip struct {
	r   Rect
	idx int // the tab's index; -1 for the "+" chip
}

// tabLabel is a tab's chip text: its title, the console for the active
// tab, and its marks — ● running, • finished in the background, ◆ its
// session may hold a transaction.
func (m *Model) tabLabel(i int) string {
	t := m.tabs[i]
	ws := t.ws
	label := t.title
	if i == m.curTab {
		ws = m.ws
		if m.console != "" {
			label += " · " + m.consoleName
		}
	}
	if ws != nil {
		if ws.Busy() {
			label += " ●"
		} else if t.done {
			label += " •"
		}
		if _, stateful := ws.Session(); stateful {
			label += " ◆"
		}
	}
	return label
}

// drawTabs draws the strip on the editor's top border when there is more
// than one tab — with one, the border keeps its plain title, so a user who
// never opens a tab sees no change.
func (m *Model) drawTabs(c *Canvas) {
	m.lay.tabChips = m.lay.tabChips[:0]
	r := m.lay.editor
	if len(m.tabs) < 2 || r.W < 20 {
		return
	}
	s := c.Sub(Rect{r.X + 1, r.Y, r.W - 2, 1})
	border := onBg(m.st.border, m.st.base)
	if m.focus == focusEditor {
		border = onBg(m.st.borderFocus, m.st.base)
	}
	s.Put(0, 0, strings.Repeat("─", s.W()), border)
	// each chip gets an equal share when they do not all fit
	plus := " + "
	avail := s.W() - width(plus) - 1
	share := max(6, avail/len(m.tabs)-1)
	x := 1
	for i := range m.tabs {
		label := " " + truncate(m.tabLabel(i), share-2) + " "
		st := onBg(m.st.title, m.st.base)
		switch {
		case i == m.curTab && m.focus == focusEditor:
			st = m.st.sel
		case i == m.curTab:
			st = m.st.buttonHover
		case m.tabs[i].failed:
			st = onBg(m.st.err, m.st.base)
		}
		if x+width(label) > s.W()-width(plus) {
			break
		}
		cr := chip(s, x, 0, label, st)
		m.lay.tabChips = append(m.lay.tabChips, tabChip{r: cr, idx: i})
		x += cr.W + 1
	}
	if len(m.tabs) < maxTabs {
		cr := chip(s, x, 0, plus, onBg(m.st.muted, m.st.base))
		m.lay.tabChips = append(m.lay.tabChips, tabChip{r: cr, idx: -1})
	}
}

// tabChipAt is the chip at (x, y): its tab index, -1 for "+", ok false off
// every chip.
func (m *Model) tabChipAt(x, y int) (int, bool) {
	for _, c := range m.lay.tabChips {
		if c.r.Contains(x, y) {
			return c.idx, true
		}
	}
	return 0, false
}

// tabClick handles a left press on a chip: switch, or a double-click
// renames, or "+" opens a tab.
func (m *Model) tabClick(idx, clicks int) tea.Cmd {
	m.focus = focusEditor
	if idx < 0 {
		return m.newTab()
	}
	cmd := m.activate(idx)
	if clicks >= 2 {
		m.renameTab()
	}
	return cmd
}

// openTabMenu is a chip's right-click menu. Right-clicking a tab that is
// not on screen brings it on screen first, so the menu acts on the tab the
// user sees — the grammar of every other right-click here.
func (m *Model) openTabMenu(idx, x, y int) tea.Cmd {
	if idx < 0 {
		return nil
	}
	cmd := m.activate(idx)
	closeWhy := ""
	if len(m.tabs) < 2 {
		closeWhy = "the last tab stays — clear its editor instead"
	}
	items := []menuItem{
		heading("tab · " + m.active().title),
		{label: "Rename…", act: func(m *Model) tea.Cmd { m.renameTab(); return nil }},
		{label: "Close tab", key: "⌥W", why: closeWhy, act: func(m *Model) tea.Cmd { return m.closeTab(m.curTab, false) }},
		{label: "New query tab", key: "⌥T", act: func(m *Model) tea.Cmd { return m.newTab() }},
	}
	items = append(items, m.consoleMenuItems()...)
	m.openMenu(x, y, items)
	return cmd
}

// maxTabTitle bounds a tab's title, which shares the editor's top border
// with every other tab's.
const maxTabTitle = 24

// renameTab asks for a new title for the tab on screen (a double-click on
// its chip, or its menu), as dbc web's tab rename does.
func (m *Model) renameTab() {
	t := m.active()
	m.openPrompt("Rename "+t.title, fmt.Sprintf("up to %d characters", maxTabTitle), "Rename", t.title,
		func(m *Model, text string) error {
			text = strings.TrimSpace(text)
			switch {
			case text == "":
				return fmt.Errorf("a tab needs a name")
			case len([]rune(text)) > maxTabTitle:
				return fmt.Errorf("at most %d characters", maxTabTitle)
			}
			m.active().title = text
			return nil
		})
}

// ---------------------------------------------------------------------------
// Saving and restoring
// ---------------------------------------------------------------------------

// savedTabs is the tabs as the layout file keeps them.
func (m *Model) savedTabs() []userdata.LayoutTab {
	out := make([]userdata.LayoutTab, len(m.tabs))
	for i, t := range m.tabs {
		ws, console := t.ws, t.consoleName
		if i == m.curTab {
			ws, console = m.ws, m.consoleName
		}
		lt := userdata.LayoutTab{Title: t.title, Console: console, Conn: t.lazy}
		if ws != nil && ws.Active() != "" {
			lt.Conn = ws.Active()
		}
		out[i] = lt
	}
	return out
}

// restoreTabs rebuilds the tabs the last run saved. The first tab New built
// becomes saved[0] (its console swapped for the saved one); the others are
// made parked. None connects here: each keeps its connection in lazy, the
// active one's connected by Init and the rest on their first activation —
// so a restart dials one connection, not one per tab.
//
// A saved connection the config no longer has falls back to the default,
// and a console that would be a second tab's is replaced by a free one.
func (m *Model) restoreTabs(saved []userdata.LayoutTab, active int) {
	if len(saved) == 0 {
		return
	}
	if len(saved) > maxTabs {
		saved = saved[:maxTabs]
	}
	def := m.ws.Active()
	for i, lt := range saved {
		conn := lt.Conn
		if _, ok := m.cfg.ConnByName(conn); !ok {
			conn = def
		}
		if i > 0 {
			m.park(m.active())
			m.nextTabKey++
			t := &queryTab{key: m.nextTabKey}
			t.ws = m.newWorkspace(t.key, m.ws.History())
			t.editor = newEditor(false)
			t.editor.sqlMode = true
			t.editor.placeholder = m.editor.placeholder
			t.grid, t.planv, t.tables = newGrid(), newPlanView(), newList()
			t.status = "ready"
			m.tabs = append(m.tabs, t)
			m.curTab = len(m.tabs) - 1
			m.load(t)
		}
		t := m.active()
		t.title = lt.Title
		if t.title == "" {
			t.title = m.nextTabTitle()
		}
		t.lazy = conn
		m.restoreTabConsole(conn, lt.Console)
	}
	m.park(m.active())
	m.curTab = max(0, min(active, len(m.tabs)-1))
	m.load(m.active())
	m.refreshConns()
}

// restoreTabConsole puts console name of conn's database in the active
// tab's editor — or, when that console is another tab's or has no name, a
// free one (freeConsole). With consoles off it leaves the editor as is.
func (m *Model) restoreTabConsole(conn, name string) {
	d, flat, ok := m.consoleOf(conn)
	if !ok {
		return
	}
	if m.console != "" && m.consoleDB == d && m.consoleName == name {
		return // the first tab, already on it (New opened it)
	}
	if !userdata.ValidConsoleName(name) || m.consoleShownElsewhere(d, name) != nil {
		name = m.freeConsole(d, userdata.ListConsoles(m.consoleDir, d, flat))
	}
	if m.console != "" {
		// the first tab's console as New opened it: saved before the swap
		// (the legacy buffer may have seeded it)
		if err := m.saveConsole(); err != nil {
			m.logf(logWarn, "could not save the console: %s", err.Error())
		}
	}
	m.setConsole(d, name)
	text, _ := userdata.LoadConsole(m.console)
	m.consoleText = text
	m.editor.SetText(text)
}
