package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/config"
)

// A log per connection.
//
// The log pane shows the messages of the connection the query tab on screen
// is on — its runs, its notices, its connects — not one stream for
// everything, so a switch to another connection does not bury what was being
// read under the other's chatter, and coming back finds it where it was
// (each pane keeps its own scroll). The logs are the window's, not a query
// tab's: two tabs on one connection share its log, as they share the
// connection in the Connections list.
//
// ROUTING. A line goes to the log of the connection on screen WHEN IT IS
// WRITTEN. That is what every m.log call already means — "tell the user
// about what they are looking at" — and it needs no connection threaded
// through the hundreds of calls. Events from a background query tab are
// replayed when the tab comes on screen (tabs.go), so their lines land in
// that tab's connection's log. The exceptions name their connection: a
// script's s.Print (ScriptPrint.Conn), which may arrive after the tab
// switched away.
//
// THE HOLDING LOG. A tab on no connection (disconnected, or opened while
// none was active) shows the "" log. Its lines are about to be orphaned:
// once the tab connects, nobody would look there again. So when the shown
// log moves from "" to a connection, the "" lines are moved to the end of
// that connection's log:
//
//	"" ─ "disconnected from pg" ─ "connect failed: …" ─┐  connect lands on lite
//	lite ─ … older lines … ─────────────────────────────┴─► … ─ disconnected… ─ connect failed…
//
// On the log's top border, right-aligned: "⧉ copy" puts the shown log on
// the clipboard, "✕ clear" empties it (y and x with the log focused).

// logKey is the connection whose log is on screen: the query tab's own, or
// "" when it is on none. An unconnected tab's workspace still reports the
// config's default as active (see queryTab.unconnected), so that is asked
// first.
func (m *Model) logKey() string {
	if m.ws == nil || len(m.tabs) > 0 && m.active().unconnected {
		return "" // before New has built the first workspace, too
	}
	return m.ws.Active()
}

// syncLog points m.logp at the shown connection's log, making it on first
// use, and folds the holding log into it when the tab has just left "no
// connection".
func (m *Model) syncLog() {
	key := m.logKey()
	if m.logp != nil && key == m.logFor {
		return
	}
	if m.logs == nil {
		m.logs = map[string]*logPane{}
	}
	lp := m.logPaneOf(key)
	if hold := m.logs[""]; key != "" && hold != nil && len(hold.lines) > 0 {
		lp.lines = append(lp.lines, hold.lines...)
		lp.trim()
		hold.lines = nil
		lp.follow = true // what was just moved in is the newest: show it
	}
	m.logp, m.logFor = lp, key
}

// logPaneOf is conn's log, made on first use.
func (m *Model) logPaneOf(conn string) *logPane {
	if m.logs == nil {
		m.logs = map[string]*logPane{}
	}
	lp := m.logs[conn]
	if lp == nil {
		lp = newLogPane()
		m.logs[conn] = lp
	}
	return lp
}

// logTo writes a line to conn's log — the shown one when conn is the
// connection on screen, which may be the holding log's ("" for an
// unconnected tab whose workspace still names its default).
func (m *Model) logTo(conn string, kind logKind, text string) {
	if conn == m.ws.Active() {
		m.log(kind, text)
		return
	}
	m.logPaneOf(conn).add(kind, text)
}

// logTitle is the log pane's title: the connection whose log it is.
func (m *Model) logTitle() string {
	if m.logFor == "" {
		return "Log"
	}
	return "Log · " + m.logFor
}

// logName says whose log it is, for the copy's "copied …" line.
func (m *Model) logName() string {
	if m.logFor == "" {
		return "the log"
	}
	return m.logFor + "'s log"
}

// copyLog is ⧉ copy and y in the log: the shown log, as text.
func (m *Model) copyLog() tea.Cmd {
	m.syncLog()
	return m.copyString(m.logp.Text(), m.logName())
}

// clearLog is ✕ clear and x in the log: the shown log only — the others
// are other connections' business.
func (m *Model) clearLog() tea.Cmd {
	m.syncLog()
	m.logp.clear()
	m.setStatus("cleared " + m.logName())
	return nil
}

// logControls are the log title's two controls, in the order drawn from
// the right edge inwards.
const (
	logCopyLabel  = " ⧉ copy "
	logClearLabel = " ✕ clear "
)

// drawLogControls draws ⧉ copy and ✕ clear at the right end of the log
// pane r's top border, and records where for clicks. They sit on the
// splitter row, so mouseClick asks them before it asks the splitters.
func (m *Model) drawLogControls(c *Canvas, r Rect) {
	m.lay.logCopy, m.lay.logClear = Rect{}, Rect{}
	need := width(logCopyLabel) + width(logClearLabel) + 1
	if r.W < width(m.logTitle())+need+8 {
		return // a narrow log keeps its title; the right-click menu has both
	}
	st := onBg(m.st.muted, m.st.base)
	s := c.Sub(Rect{r.X, r.Y, r.W, 1})
	clearX := s.PutRight(r.W-2, 0, logClearLabel, st)
	m.lay.logClear = Rect{r.X + clearX, r.Y, width(logClearLabel), 1}
	copyX := s.PutRight(clearX-1, 0, logCopyLabel, st)
	m.lay.logCopy = Rect{r.X + copyX, r.Y, width(logCopyLabel), 1}
}

// logControlAt runs the log control at (x, y), if any.
func (m *Model) logControlAt(x, y int) (tea.Cmd, bool) {
	switch {
	case m.lay.logCopy.Contains(x, y):
		m.focus = focusLog
		return m.copyLog(), true
	case m.lay.logClear.Contains(x, y):
		m.focus = focusLog
		return m.clearLog(), true
	}
	return nil, false
}

// logKeyPress handles the log pane's keys: y copies, x clears, the rest
// scroll.
func (m *Model) logKeyPress(k tea.KeyPressMsg) tea.Cmd {
	m.syncLog()
	switch k.String() {
	case "y":
		return m.copyLog()
	case "x":
		return m.clearLog()
	}
	m.logp.key(k)
	return nil
}

// renameConnViews moves what the TUI keeps per connection from a renamed
// connection to its new name: its log, and in every query tab its result
// sets (the workspace's) with their parked grids and plan view. rename is
// connRenamed's mapping, which also carries "<old>/<db>" names over.
//
// The names to try come from the TUI's own per-connection maps: a
// connection with results in some tab was on screen in that tab at some
// point, and so has a log here, a plan view or parked grids there.
func (m *Model) renameConnViews(rename func(string) (string, bool)) {
	names := map[string]bool{}
	for n := range m.logs {
		names[n] = true
	}
	m.eachTabViews(func(_ *queryTab, ws workspaceRenamer, plans map[string]connPane, grids map[string]map[int]*grid) {
		for n := range plans {
			names[n] = true
		}
		for n := range grids {
			names[n] = true
		}
	})
	for from := range names {
		to, ok := rename(from)
		if !ok || from == "" {
			continue
		}
		if lp, ok := m.logs[from]; ok {
			delete(m.logs, from)
			m.logs[to] = lp
		}
		m.eachTabViews(func(_ *queryTab, ws workspaceRenamer, plans map[string]connPane, grids map[string]map[int]*grid) {
			ws.RenameResults(from, to)
			if p, ok := plans[from]; ok {
				delete(plans, from)
				plans[to] = p
			}
			if g, ok := grids[from]; ok {
				delete(grids, from)
				grids[to] = g
			}
		})
	}
}

// dropConnViews forgets what the TUI keeps for a removed connection and the
// "<name>/<db>" connections derived from it: their logs, and in every query
// tab their result sets, parked grids and plan views.
func (m *Model) dropConnViews(name string) {
	gone := func(n string) bool { return n == name || strings.HasPrefix(n, name+config.DatabaseSep) }
	for n := range m.logs {
		if gone(n) {
			delete(m.logs, n)
		}
	}
	m.eachTabViews(func(_ *queryTab, ws workspaceRenamer, plans map[string]connPane, grids map[string]map[int]*grid) {
		names := map[string]bool{name: true}
		for n := range plans {
			names[n] = true
		}
		for n := range grids {
			names[n] = true
		}
		for n := range names {
			if gone(n) {
				ws.DropResults(n)
				delete(plans, n)
				delete(grids, n)
			}
		}
	})
}

// workspaceRenamer is the part of a workspace a rename or removal touches.
type workspaceRenamer interface {
	RenameResults(from, to string)
	DropResults(conn string)
}

// eachTabViews calls f with every query tab's workspace and per-connection
// views — the Model's live fields for the tab on screen, the parked ones
// for the rest. A parked tab's maps are made if missing, so f may add to
// them.
func (m *Model) eachTabViews(f func(t *queryTab, ws workspaceRenamer, plans map[string]connPane, grids map[string]map[int]*grid)) {
	for i, t := range m.tabs {
		if i == m.curTab {
			if m.plans == nil {
				m.plans = map[string]connPane{}
			}
			if m.rgrids == nil {
				m.rgrids = map[string]map[int]*grid{}
			}
			f(t, m.ws, m.plans, m.rgrids)
			continue
		}
		if t.ws == nil {
			continue
		}
		if t.plans == nil {
			t.plans = map[string]connPane{}
		}
		if t.rgrids == nil {
			t.rgrids = map[string]map[int]*grid{}
		}
		f(t, t.ws, t.plans, t.rgrids)
	}
}
