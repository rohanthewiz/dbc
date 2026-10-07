package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/userdata"
)

// ⌥T opens a second tab on the same connection, with a workspace of its own:
// a BEGIN in one leaves the other's session untouched. The strip shows on
// the editor's border only once there are two tabs, and ⌥1 / ⌥2 switch.
func TestTabsHaveTheirOwnSessions(t *testing.T) {
	m := newTestModel(t)
	if strings.Contains(frame(m).Line(m.lay.editor.Y), "Query 1") {
		t.Fatal("a strip with one tab: the border should keep its plain title")
	}
	first := m.ws
	m.editor.SetText("BEGIN")
	key(t, m, "ctrl+r")
	if _, stateful := m.ws.Session(); !stateful {
		t.Fatal("BEGIN did not make the session stateful")
	}

	key(t, m, "alt+t")
	if len(m.tabs) != 2 || m.curTab != 1 || m.ws == first {
		t.Fatalf("tabs %d, cur %d, same workspace %v", len(m.tabs), m.curTab, m.ws == first)
	}
	if m.ws.Active() != first.Active() {
		t.Errorf("new tab on %q, want the first tab's %q", m.ws.Active(), first.Active())
	}
	if _, stateful := m.ws.Session(); stateful {
		t.Error("the new tab inherited the first tab's transaction")
	}
	line := frame(m).Line(m.lay.editor.Y)
	if !strings.Contains(line, "Query 1") || !strings.Contains(line, "Query 2") || !strings.Contains(line, "◆") {
		t.Errorf("strip = %q, want both tabs and the first one's ◆", line)
	}

	m.editor.SetText("SELECT 2 AS two")
	key(t, m, "ctrl+r")
	key(t, m, "alt+1")
	if m.ws != first || m.editor.Text() != "BEGIN" {
		t.Fatalf("⌥1: workspace switched %v, editor %q", m.ws == first, m.editor.Text())
	}
	key(t, m, "alt+2")
	if m.editor.Text() != "SELECT 2 AS two" || m.grid.colName(0) != "two" {
		t.Fatalf("⌥2: editor %q, grid column %q", m.editor.Text(), m.grid.colName(0))
	}
}

// A run left going in a tab that is then switched away from lands in that
// tab, not the one on screen: its tab is marked done, and its result is
// drawn when the tab comes back.
func TestTabBackgroundRunLandsInItsTab(t *testing.T) {
	m := newTestModel(t)
	m.editor.SetText("SELECT 41 + 1 AS answer")
	run := m.runQuery() // started, not yet landed

	key(t, m, "alt+t")
	m.editor.SetText("SELECT 'other' AS here")
	key(t, m, "ctrl+r")
	drive(t, m, nil, run) // tab 1's run lands while tab 2 is on screen

	if m.grid.colName(0) != "here" {
		t.Fatalf("tab 2's grid shows %q — tab 1's result was drawn into it", m.grid.colName(0))
	}
	if !m.tabs[0].done || len(m.tabs[0].pending) == 0 {
		t.Fatalf("tab 1: done %v, pending %d", m.tabs[0].done, len(m.tabs[0].pending))
	}
	if line := frame(m).Line(m.lay.editor.Y); !strings.Contains(line, "Query 1 •") {
		t.Errorf("strip = %q, want tab 1 marked •", line)
	}

	key(t, m, "alt+1")
	if m.grid.colName(0) != "answer" {
		t.Fatalf("back on tab 1, grid column %q, want answer", m.grid.colName(0))
	}
	if v, _, _ := m.grid.value(0, 0); v != "42" {
		t.Errorf("answer = %q", v)
	}
	if m.tabs[0].done || len(m.tabs[0].pending) != 0 {
		t.Error("looking at the tab should clear its mark and its queue")
	}
}

// A DDL run that lands while its tab is in the background starts its
// relist at once rather than on the user's return — until it lands, the
// tab's workspace is mid-connect, refusing a Refresh — and the tab comes
// back to the new table listed.
func TestTabBackgroundDDLRelistsAtOnce(t *testing.T) {
	m := newTestModel(t)
	m.editor.SetText("CREATE TABLE aaa_background (id INTEGER)")
	run := m.runQuery()

	key(t, m, "alt+t")
	drive(t, m, nil, run)
	if _, connecting := m.tabs[0].ws.Connecting(); connecting {
		t.Fatal("tab 1's relist waits for the user's return: its workspace is still mid-connect")
	}

	key(t, m, "alt+1")
	if len(m.tables.items) == 0 || m.tables.items[0].data.(string) != "aaa_background" {
		t.Fatalf("back on tab 1: first table %+v, want aaa_background", m.tables.items[0])
	}
}

// ⌥W closes the tab on screen, asking first when its session may hold a
// transaction; the last tab stays.
func TestTabClose(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "alt+w")
	if len(m.tabs) != 1 || !strings.Contains(logText(m), "the last tab stays") {
		t.Fatalf("closed the last tab: %d left", len(m.tabs))
	}

	key(t, m, "alt+t")
	m.editor.SetText("BEGIN")
	key(t, m, "ctrl+r")
	key(t, m, "alt+w")
	if m.menu == nil || len(m.tabs) != 2 {
		t.Fatalf("a stateful tab closed without asking (menu %v, tabs %d)", m.menu != nil, len(m.tabs))
	}
	key(t, m, "esc")
	if len(m.tabs) != 2 {
		t.Fatal("Esc closed the tab")
	}
	key(t, m, "alt+w")
	for i, it := range m.menu.items {
		if strings.HasPrefix(it.label, "Close the tab") {
			drive(t, m, nil, m.menuPick(i))
		}
	}
	if len(m.tabs) != 1 || m.curTab != 0 {
		t.Fatalf("after the confirmed close: %d tabs, cur %d", len(m.tabs), m.curTab)
	}
}

// The strip is mouse-driven too: a click switches, "+" opens a tab, a
// double-click renames, and the right-click menu has close and rename.
func TestTabStripMouse(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "alt+t")
	frame(m)
	x, y := findText(t, frame(m), "Query 1")
	click(t, m, x+1, y)
	if m.curTab != 0 {
		t.Fatalf("click on Query 1: cur %d", m.curTab)
	}
	px, py := findText(t, frame(m), " + ")
	click(t, m, px+1, py)
	if len(m.tabs) != 3 {
		t.Fatalf("+ made %d tabs", len(m.tabs))
	}

	x, y = findText(t, frame(m), "Query 2")
	drive(t, m, tea.MouseClickMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	drive(t, m, tea.MouseClickMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	p, ok := m.modal.(*promptModal)
	if !ok || m.curTab != 1 {
		t.Fatalf("double-click: modal %T, cur %d", m.modal, m.curTab)
	}
	p.field.SetText("wip")
	key(t, m, "enter")
	if m.active().title != "wip" {
		t.Fatalf("renamed to %q", m.active().title)
	}

	x, y = findText(t, frame(m), "wip")
	rightClick(t, m, x+1, y)
	if m.menu == nil {
		t.Fatal("no tab menu")
	}
	labels := ""
	for _, it := range m.menu.items {
		labels += it.label + "|"
	}
	for _, want := range []string{"Rename…", "Close tab", "New query tab"} {
		if !strings.Contains(labels, want) {
			t.Errorf("menu lacks %q: %s", want, labels)
		}
	}
}

// Two tabs on one database get two consoles, never one file written by
// both: the new tab takes a console no other tab shows, ⌥C skips the
// consoles other tabs show, and the console menu sends you to the tab that
// has one rather than opening it twice.
func TestTabsKeepConsolesApart(t *testing.T) {
	m, _ := consoleModel(t)
	first := m.consoleName
	m.editor.SetText("SELECT 'one'")

	key(t, m, "alt+t")
	if m.consoleName == first || m.consoleDB != m.tabs[0].consoleDB {
		t.Fatalf("new tab on console %q of %v; tab 1 has %q", m.consoleName, m.consoleDB, first)
	}
	if text, _ := userdata.LoadConsole(m.tabs[0].console); text != "SELECT 'one'" {
		t.Errorf("tab 1's console was not saved when the tab opened: %q", text)
	}
	m.editor.SetText("SELECT 'two'")
	key(t, m, "alt+c")
	if m.consoleName == first {
		t.Fatal("⌥C moved onto the console tab 1 shows")
	}

	var goTo func(*Model) tea.Cmd
	for _, it := range m.consoleMenuItems() {
		if strings.Contains(it.label, first) && strings.Contains(it.label, "· in Query 1") {
			goTo = it.act
		}
	}
	if goTo == nil {
		t.Fatal("the console menu does not name the tab showing " + first)
	}
	drive(t, m, nil, goTo(m))
	if m.curTab != 0 || m.editor.Text() != "SELECT 'one'" {
		t.Fatalf("the menu row went to tab %d, editor %q", m.curTab, m.editor.Text())
	}
}

// Tabs survive a restart: titles, connections and consoles are saved with
// the layout, the active tab comes back on screen, and a tab that was not
// on screen connects only when it is first looked at.
func TestTabsRestore(t *testing.T) {
	m, dir := consoleModel(t)
	drive(t, m, nil, m.setActive("a"))
	m.editor.SetText("SELECT 'on a'")
	key(t, m, "alt+t")
	m.active().title = "wip"
	drive(t, m, nil, m.setActive("b"))
	m.editor.SetText("SELECT 'on b'")
	saved, active := m.savedTabs(), m.curTab
	m.forEachTab(func() {
		if err := m.saveConsole(); err != nil {
			t.Fatal(err)
		}
	})

	m2, _ := consoleModel(t)
	m2.cfg.Connections = m.cfg.Connections
	m2.consoleDir = dir
	m2.restoreTabs(saved, active)
	if len(m2.tabs) != 2 || m2.curTab != 1 || m2.active().title != "wip" {
		t.Fatalf("restored %d tabs, cur %d, title %q", len(m2.tabs), m2.curTab, m2.active().title)
	}
	if m2.editor.Text() != "SELECT 'on b'" || m2.active().lazy != "b" {
		t.Fatalf("active tab: editor %q, connects to %q", m2.editor.Text(), m2.active().lazy)
	}
	drive(t, m2, nil, m2.Init())
	if m2.ws.Active() != "b" {
		t.Fatalf("Init connected the active tab to %q, want b", m2.ws.Active())
	}
	if m2.tabs[0].ws.Active() == "a" {
		t.Fatal("the background tab connected before it was looked at")
	}
	key(t, m2, "alt+1")
	if m2.ws.Active() != "a" || m2.editor.Text() != "SELECT 'on a'" {
		t.Fatalf("tab 1 on %q with %q", m2.ws.Active(), m2.editor.Text())
	}
}

// The connections list marks the tab's connection ● and those other tabs
// are on ○, and a connection another tab is on cannot be removed or have
// what it dials changed — dbc web's "a query tab is on it".
func TestTabsInConnectionsList(t *testing.T) {
	m, _ := consoleModel(t)
	key(t, m, "alt+t")
	drive(t, m, nil, m.setActive("a"))
	marks := map[string]string{}
	for _, it := range m.conns.items {
		marks[it.label] = it.mark
	}
	if marks["a"] != "●" || marks[m.tabs[0].ws.Active()] != "○" {
		t.Fatalf("marks = %v", marks)
	}
	err := m.connInUse(false)(m.tabs[0].ws.Active())
	if err == nil || !strings.Contains(err.Error(), "tab Query 1 is on") {
		t.Fatalf("in-use for tab 1's connection: %v", err)
	}
	if err := m.connInUse(false)("b"); err != nil {
		t.Fatalf("b is on no tab, but: %v", err)
	}
}

// Ctrl+C on an idle tab does not quit over a query still running in
// another tab: it says where, and Ctrl+Q is the way out anyway.
func TestInterruptKeepsABackgroundRun(t *testing.T) {
	m := newTestModel(t)
	m.editor.SetText("SELECT 1")
	run := m.runQuery() // tab 1 busy until run is driven
	key(t, m, "alt+t")
	key(t, m, "ctrl+c")
	if m.quit || !strings.Contains(logText(m), "Query 1 is still running") {
		t.Fatalf("quit %v; log:\n%s", m.quit, logText(m))
	}
	drive(t, m, nil, run)
	m.interrupt() // not through drive: tea.Quit would re-fire forever there
	if !m.quit {
		t.Fatal("Ctrl+C with nothing running did not quit")
	}
}

// Closing a tab brings its neighbour on screen as a switch would: a run
// that landed there in the background is drawn and its mark cleared,
// rather than left queued to replay stale on a later visit.
func TestTabCloseArrivesAtTheNeighbour(t *testing.T) {
	m := newTestModel(t)
	m.editor.SetText("SELECT 41 + 1 AS answer")
	run := m.runQuery()
	key(t, m, "alt+t")
	drive(t, m, nil, run) // lands in tab 1, in the background
	if len(m.tabs[0].pending) == 0 {
		t.Fatal("tab 1's run was not queued")
	}
	key(t, m, "alt+w")
	if len(m.tabs) != 1 || m.grid.colName(0) != "answer" {
		t.Fatalf("after the close: %d tabs, grid column %q", len(m.tabs), m.grid.colName(0))
	}
	if m.active().done || len(m.active().pending) != 0 {
		t.Error("the neighbour kept its mark or its queue")
	}
}

// A tab is on the connection it will dial (a restored one's) or on none
// (one that never connected) — never on the default its fresh workspace
// reports: the save keeps a lazy tab's connection, and the default is not
// held by a tab that is not on it. A tab on <conn>/<db> holds <conn>'s
// pool too, which a disconnect would close with it.
func TestTabConnectionsAreTheirOwn(t *testing.T) {
	m, _ := consoleModel(t)
	def := m.ws.Active()
	m.restoreTabs([]userdata.LayoutTab{{Title: "one", Conn: "a"}, {Title: "two", Conn: "b"}}, 1)
	if saved := m.savedTabs(); saved[0].Conn != "a" {
		t.Fatalf("a lazy tab re-saved on %q, want a", saved[0].Conn)
	}
	drive(t, m, nil, m.Init()) // tab two connects to b
	if m.tabOn(def) {
		t.Errorf("%s counted as in use, though no tab is on it", def)
	}

	key(t, m, "x") // focus is the editor: x types; disconnect through the call
	m.editor.SetText("")
	drive(t, m, nil, m.disconnectNow())
	key(t, m, "alt+t") // a tab opened with no connection to follow
	if target, _ := m.tabTarget(m.curTab); target != "" {
		t.Errorf("an unconnected tab reads as on %q", target)
	}

	// configured only, never dialed: the derived name resolves by config
	m.cfg.Connections = append(m.cfg.Connections,
		config.Connection{Name: "pg", Driver: "postgres", DSN: "postgres://localhost/x"})
	m.tabs[0].lazy = "pg/foo"
	if !m.tabOn("pg") {
		t.Error("a tab on pg/foo does not hold pg's pool")
	}
}

// A quick A → B → A leaves one elapsed-time chain for A, not two: the
// earlier visit's tick is dropped.
func TestTabTickChainsDoNotPileUp(t *testing.T) {
	m := newTestModel(t)
	old := tickMsg{gen: 1, tab: m.active().key, seq: m.active().tickSeq}
	key(t, m, "alt+t")
	key(t, m, "alt+1")
	if cmd := m.tick(old); cmd != nil {
		t.Fatal("a tick from an earlier visit re-armed itself")
	}
}
