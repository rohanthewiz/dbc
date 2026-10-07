package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/workspace"
)

// These cover resulttabs.go and logs.go: the results pane and the log
// follow the connection, the result tabs' strip, keys and menu, and the
// log's copy and clear.

// withOther adds an empty in-memory SQLite connection named "other".
func withOther(t *testing.T, m *Model) {
	t.Helper()
	m.cfg.Connections = append(m.cfg.Connections, config.Connection{
		Name: "other", Driver: "sqlite",
		DSN: fmt.Sprintf("file:tuitest%d?mode=memory&cache=shared", dbSeq.Add(1)),
	})
	m.refreshConns()
}

// runSQL puts sql in the editor and runs it (Ctrl+R).
func runSQL(t *testing.T, m *Model, sql string) {
	t.Helper()
	m.editor.SetText(sql)
	key(t, m, "ctrl+r")
	if e := m.ws.LastErr(); e != "" {
		t.Fatalf("%s: %s", sql, e)
	}
}

// switchConn connects the tab to name and checks it landed.
func switchConn(t *testing.T, m *Model, name string) {
	t.Helper()
	drive(t, m, nil, m.setActive(name))
	if m.ws.Active() != name {
		t.Fatalf("switch to %s did not land; log: %s", name, logText(m))
	}
}

// stripRow is the results pane's bottom border, where the strip is drawn.
func stripRow(m *Model) int { return m.lay.results.Y + m.lay.results.H - 1 }

// curID is the current result tab's id (0 for none).
func curID(m *Model) int {
	t, _, _, ok := m.curResultTab()
	if !ok {
		return 0
	}
	return t.ID
}

// The results pane shows the connection's own results: nothing on a
// connection that has run nothing, the other's back — with the grid's view
// as it was left (a sort) — on the way back. The plan follows too.
func TestResultsFollowTheConnection(t *testing.T) {
	m := newTestModel(t)
	withOther(t, m)
	key(t, m, "ctrl+r") // the cats query
	cats := m.ws.LastResult()
	if cats == nil || m.grid.res != cats {
		t.Fatalf("no result on demo; log: %s", logText(m))
	}
	m.grid.Sort(1)
	key(t, m, "ctrl+x") // a plan on demo
	if m.planv.plan == nil || m.resTab != tabPlan {
		t.Fatalf("no plan on demo; log: %s", logText(m))
	}

	switchConn(t, m, "other")
	if m.ws.LastResult() != nil || m.grid.res != nil || m.grid.Rows() != 0 {
		t.Errorf("other shows demo's result: %v", m.grid.res != nil)
	}
	if m.planv.plan != nil || m.resTab != tabResults {
		t.Errorf("other shows demo's plan (%v) or is on the plan tab (%v)", m.planv.plan != nil, m.resTab == tabPlan)
	}
	if len(m.lay.rtabChips) != 0 || strings.Contains(frame(m).Line(stripRow(m)), " 1 ") {
		t.Errorf("an empty set drew a strip: %q", frame(m).Line(stripRow(m)))
	}
	runSQL(t, m, "SELECT 2 AS two")
	if m.grid.res == nil || m.grid.res.Columns[0] != "two" {
		t.Fatalf("other's run: %+v", m.grid.res)
	}

	switchConn(t, m, config.DemoSQLite)
	if m.grid.res != cats || m.grid.sortCol != 1 {
		t.Errorf("back on demo: result %v, sort col %d — the view was not kept", m.grid.res == cats, m.grid.sortCol)
	}
	if m.planv.plan == nil || m.resTab != tabPlan {
		t.Errorf("back on demo: plan %v, on the plan tab %v", m.planv.plan != nil, m.resTab == tabPlan)
	}
	switchConn(t, m, "other")
	if m.grid.res == nil || m.grid.res.Columns[0] != "two" {
		t.Error("back on other: its result is gone")
	}
}

// A run replaces the current result tab; a pinned one makes the next run
// open a second tab, and the strip on the bottom border shows both. A click
// on a label shows that tab, { and } step, and x closes.
func TestResultTabsStripAndKeys(t *testing.T) {
	m := newTestModel(t)
	runSQL(t, m, "SELECT 1 AS a")
	runSQL(t, m, "SELECT 2 AS a")
	if tabs, _ := m.ws.ResultTabs(); len(tabs) != 1 {
		t.Fatalf("a rerun opened a tab: %d", len(tabs))
	}
	_, y := findText(t, frame(m), " 1 SELECT 2 AS a ")
	if y != stripRow(m) {
		t.Errorf("the strip is on row %d, want the bottom border %d", y, stripRow(m))
	}

	m.focus = focusGrid
	key(t, m, "P")
	if !strings.Contains(logText(m), "pinned result 1") {
		t.Errorf("P said nothing; log: %s", logText(m))
	}
	first := curID(m)
	runSQL(t, m, "SELECT 3 AS a")
	tabs, cur := m.ws.ResultTabs()
	if len(tabs) != 2 || cur != 1 || !tabs[0].Pinned {
		t.Fatalf("after a pin: %+v, cur %d", tabs, cur)
	}
	line := frame(m).Line(stripRow(m))
	if !strings.Contains(line, " 1⚑ SELECT 2 AS a ") || !strings.Contains(line, " 2 SELECT 3 AS a ") {
		t.Errorf("strip = %q", line)
	}
	if len(m.lay.rtabChips) != 1 {
		t.Errorf("chips = %d, want 1 (the current tab is not one)", len(m.lay.rtabChips))
	}

	x, y := findText(t, frame(m), " 1⚑")
	click(t, m, x+1, y)
	if curID(m) != first || m.grid.res.Rows[0][0] != "2" {
		t.Fatalf("a click on 1 shows %d (%v)", curID(m), m.grid.res.Rows)
	}
	m.focus = focusGrid
	key(t, m, "{") // at the first: stays
	if curID(m) != first {
		t.Error("{ on the first moved")
	}
	key(t, m, "}")
	if m.grid.res.Rows[0][0] != "3" {
		t.Errorf("} did not show the second: %v", m.grid.res.Rows)
	}
	key(t, m, "{")
	key(t, m, "x")
	if tabs, _ := m.ws.ResultTabs(); len(tabs) != 1 || curID(m) == first {
		t.Errorf("x left %d tabs, current %d", len(tabs), curID(m))
	}
	if !strings.Contains(logText(m), "closed result 1") || m.grid.res.Rows[0][0] != "3" {
		t.Errorf("after x: grid %v; log: %s", m.grid.res.Rows, logText(m))
	}

	// the plan view answers the same keys
	key(t, m, "ctrl+x")
	if m.resTab != tabPlan {
		t.Fatal("no plan")
	}
	m.focus = focusGrid
	key(t, m, "P")
	if tabs, _ := m.ws.ResultTabs(); !tabs[0].Pinned {
		t.Error("P in the plan view did not pin")
	}
}

// A strip label's right-click menu, and the grid menu's result-tab rows.
func TestResultTabMenus(t *testing.T) {
	m := newTestModel(t)
	runSQL(t, m, "SELECT 1 AS a")
	m.focus = focusGrid
	key(t, m, "P")
	runSQL(t, m, "SELECT 2 AS a")

	x, y := findText(t, frame(m), " 1⚑")
	rightClick(t, m, x+1, y)
	if m.menu == nil {
		t.Fatal("no menu")
	}
	labels := menuLabels(m)
	if !strings.Contains(labels, "result 1 of 2 · at most 10 on demo-sqlite") ||
		!strings.Contains(labels, "Unpin this result") || !strings.Contains(labels, "Close unpinned result tabs (1)") {
		t.Errorf("menu = %s", labels)
	}
	if curID(m) == 0 || m.grid.res.Rows[0][0] != "1" {
		t.Error("the right-clicked tab did not come on screen")
	}
	pickMenu(t, m, "Close unpinned result tabs (1)")
	if tabs, _ := m.ws.ResultTabs(); len(tabs) != 1 || !tabs[0].Pinned {
		t.Errorf("after close unpinned: %+v", tabs)
	}

	m.openGridMenu(m.lay.results.X+4, m.lay.results.Y+3)
	if l := menuLabels(m); !strings.Contains(l, "Unpin this result") || !strings.Contains(l, "Close this result tab") {
		t.Errorf("grid menu = %s", l)
	}
	m.menu = nil
}

// menuLabels is the open menu's rows, one per line.
func menuLabels(m *Model) string {
	var b strings.Builder
	for _, it := range m.menu.items {
		b.WriteString(it.label + "\n")
	}
	return b.String()
}

// Every tab pinned at result_tabs: the run is refused, in words, and costs
// nothing.
func TestResultTabCapRefusedInTheLog(t *testing.T) {
	m := newTestModel(t)
	m.cfg.ResultTabs = 2
	for _, q := range []string{"SELECT 1", "SELECT 2"} {
		runSQL(t, m, q)
		m.focus = focusGrid
		key(t, m, "P")
	}
	m.editor.SetText("SELECT 3")
	key(t, m, "ctrl+r")
	if !strings.Contains(logText(m), "every result tab on demo-sqlite is pinned") || m.ws.Busy() {
		t.Errorf("busy %v; log: %s", m.ws.Busy(), logText(m))
	}
}

// A run that lands after the tab switched away goes to its own
// connection's tabs, not the grid on screen; its log lines go to its own
// connection's log, and the log on screen says where both went.
func TestRunLandingOffScreen(t *testing.T) {
	m := newTestModel(t)
	withOther(t, m)
	st, err := m.ws.RunStmts([]string{"SELECT 7 AS seven"}, "query")
	if err != nil {
		t.Fatal(err)
	}
	switchConn(t, m, "other")
	drive(t, m, st.Job())
	if m.grid.res != nil {
		t.Error("demo's result was drawn on other")
	}
	if log := logText(m); !strings.Contains(log, "query on demo-sqlite finished — its result and log lines are demo-sqlite's") ||
		strings.Contains(log, "completed on demo-sqlite") {
		t.Errorf("log on other: %s", log)
	}
	switchConn(t, m, config.DemoSQLite)
	if m.grid.res == nil || m.grid.res.Columns[0] != "seven" {
		t.Errorf("back on demo: %+v", m.grid.res)
	}
	if log := logText(m); !strings.Contains(log, "query completed on demo-sqlite") {
		t.Errorf("demo's log lacks its run's line: %s", log)
	}
}

// An explain landing off screen goes to its own connection's plan view,
// which comes back turned to it.
func TestExplainLandingOffScreen(t *testing.T) {
	m := newTestModel(t)
	withOther(t, m)
	st, err := m.ws.Explain("SELECT * FROM cats", "", false)
	if err != nil {
		t.Fatal(err)
	}
	switchConn(t, m, "other")
	drive(t, m, st.Job())
	if m.planv.plan != nil || m.resTab == tabPlan {
		t.Error("demo's plan was drawn on other")
	}
	if log := logText(m); !strings.Contains(log, "explain on demo-sqlite finished — its plan and log lines are demo-sqlite's") {
		t.Errorf("log on other: %s", log)
	}
	switchConn(t, m, config.DemoSQLite)
	if m.planv.plan == nil || m.resTab != tabPlan {
		t.Errorf("back on demo: plan %v, on it %v", m.planv.plan != nil, m.resTab == tabPlan)
	}
}

// Each query tab keeps its own result sets — and its own parked grids —
// across a switch to another query tab and back.
func TestResultTabsSwapWithTheQueryTab(t *testing.T) {
	m := newTestModel(t)
	runSQL(t, m, "SELECT 1 AS a")
	m.focus = focusGrid
	key(t, m, "P")
	runSQL(t, m, "SELECT 2 AS a")
	m.grid.Sort(0)
	key(t, m, "alt+t")
	if tabs, _ := m.ws.ResultTabs(); len(tabs) != 0 || m.grid.res != nil {
		t.Fatalf("a new query tab has results: %d", len(tabs))
	}
	key(t, m, "alt+1")
	if tabs, _ := m.ws.ResultTabs(); len(tabs) != 2 || m.grid.res.Rows[0][0] != "2" || m.grid.sortCol != 0 {
		t.Errorf("back on tab 1: %d tabs, grid %v, sort %d", len(tabs), m.grid.res.Rows, m.grid.sortCol)
	}
}

// A connection renamed: its results and log follow it. Removed: they go.
func TestRenamedConnectionKeepsItsViews(t *testing.T) {
	m := newTestModel(t)
	withOther(t, m)
	switchConn(t, m, "other")
	runSQL(t, m, "SELECT 5 AS five")
	m.log(logInfo, "said on other")
	switchConn(t, m, config.DemoSQLite)

	m.cfg.Connections[1].Name = "renamed"
	m.connRenamed("other", "renamed")
	switchConn(t, m, "renamed")
	if m.grid.res == nil || m.grid.res.Columns[0] != "five" {
		t.Errorf("the result did not follow the rename: %+v", m.grid.res)
	}
	if !strings.Contains(logText(m), "said on other") || m.logTitle() != "Log · renamed" {
		t.Errorf("the log did not follow (%s): %s", m.logTitle(), logText(m))
	}
	switchConn(t, m, config.DemoSQLite)
	m.dropConnViews("renamed")
	if _, ok := m.logs["renamed"]; ok {
		t.Error("a removed connection's log stayed")
	}
	switchConn(t, m, "renamed")
	if m.ws.LastResult() != nil {
		t.Error("a removed connection's results stayed")
	}
}

// The log is the connection's: a line said on one is not in the other's,
// and comes back with it. Lines said on no connection (the holding log)
// move into the next connection's log rather than being stranded.
func TestLogPerConnection(t *testing.T) {
	m := newTestModel(t)
	withOther(t, m)
	m.log(logInfo, "hello demo")
	if !strings.Contains(frame(m).Text(), "Log · demo-sqlite") {
		t.Error("the log's title does not name the connection")
	}
	switchConn(t, m, "other")
	if strings.Contains(logText(m), "hello demo") {
		t.Errorf("demo's line in other's log: %s", logText(m))
	}
	if !strings.Contains(frame(m).Text(), "Log · other") {
		t.Error("the title did not follow")
	}
	switchConn(t, m, config.DemoSQLite)
	if !strings.Contains(logText(m), "hello demo") {
		t.Errorf("demo's log lost its line: %s", logText(m))
	}

	drive(t, m, nil, m.disconnectNow())
	if m.logFor != "" || !strings.Contains(logText(m), "disconnected from demo-sqlite") {
		t.Fatalf("disconnected: log %q: %s", m.logFor, logText(m))
	}
	if line := frame(m).Line(m.lay.logR.Y); !strings.Contains(line, "─ Log ─") {
		t.Error("the holding log should be titled plain Log")
	}
	m.log(logWarn, "said while on nothing")
	switchConn(t, m, "other")
	got := logText(m)
	if !strings.Contains(got, "said while on nothing") || !strings.Contains(got, "disconnected from demo-sqlite") {
		t.Errorf("the holding log was not folded into other's: %s", got)
	}
	if hold := m.logs[""]; hold != nil && len(hold.lines) > 0 {
		t.Errorf("the holding log kept %d lines", len(hold.lines))
	}
}

// A script's s.Print goes to the log of the connection it ran on, even
// when another is on screen.
func TestScriptPrintGoesToItsConnection(t *testing.T) {
	m := newTestModel(t)
	withOther(t, m)
	switchConn(t, m, "other")
	drive(t, m, &workspace.ScriptPrint{Text: "printed on demo", Conn: config.DemoSQLite})
	if strings.Contains(logText(m), "printed on demo") {
		t.Error("demo's print in other's log")
	}
	switchConn(t, m, config.DemoSQLite)
	if !strings.Contains(logText(m), "printed on demo") {
		t.Errorf("demo's log: %s", logText(m))
	}
}

// ⧉ copy and ✕ clear on the log's title, and y / x in the log.
func TestLogCopyAndClear(t *testing.T) {
	m := newTestModel(t)
	m.log(logInfo, "a line to copy")
	x, y := findText(t, frame(m), "⧉ copy")
	if y != m.lay.logR.Y {
		t.Fatalf("⧉ copy on row %d, want the log's top border %d", y, m.lay.logR.Y)
	}
	click(t, m, x, y)
	if c := lastClip(t); !strings.Contains(c.Text, "a line to copy") {
		t.Errorf("copied %q", c.Text)
	}
	if m.drag.kind != dragNone {
		t.Error("the click started a splitter drag")
	}
	if !strings.Contains(logText(m), "demo-sqlite's log") {
		t.Errorf("the copy did not say what it copied: %s", logText(m))
	}

	x, y = findText(t, frame(m), "✕ clear")
	click(t, m, x, y)
	if n := len(m.logp.lines); n != 0 {
		t.Errorf("clear left %d lines", n)
	}
	if m.focus != focusLog {
		t.Error("a control's click should focus the log")
	}

	m.log(logInfo, "another")
	clipLog = nil
	key(t, m, "y")
	if c := lastClip(t); !strings.Contains(c.Text, "another") {
		t.Errorf("y copied %q", c.Text)
	}
	key(t, m, "x")
	if n := len(m.logp.lines); n != 0 {
		t.Errorf("x left %d lines", n)
	}
}

// Sharing a result tab with the assistant needs the connection's ai_rows:
// without it, s is refused in words naming the setting and the menu row is
// disabled with the reason.
func TestShareRefusedWithoutAIRows(t *testing.T) {
	m := newTestModel(t)
	runSQL(t, m, "SELECT 1 AS a")
	m.focus = focusGrid
	key(t, m, "s")
	if tabs, _ := m.ws.ResultTabs(); tabs[0].Shared {
		t.Fatal("shared on a connection without ai_rows")
	}
	if !strings.Contains(logText(m), "ai_rows = true") {
		t.Errorf("the refusal did not name ai_rows: %s", logText(m))
	}
	m.openGridMenu(m.lay.results.X+4, m.lay.results.Y+3)
	found := false
	for _, it := range m.menu.items {
		if it.label == "✦ Share with the assistant" {
			found = true
			if !strings.Contains(it.why, "ai_rows = true") {
				t.Errorf("the share row is not disabled with the reason: why %q", it.why)
			}
		}
	}
	if !found {
		t.Errorf("no share row:\n%s", menuLabels(m))
	}
	m.menu = nil
}

// With ai_rows on: S shares (✦ on the strip; s too), the shared tab is kept like a
// pinned one, and while another tab is on screen the assistant's context
// is the shared result — through its parked grid's view, so the column
// hidden there stays out. s again stops sharing.
func TestShareResultTab(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Connections[0].AIRows = true
	runSQL(t, m, "SELECT id, name, breed FROM cats")
	m.grid.Hide(1, 1) // name
	m.focus = focusGrid
	key(t, m, "S") // the documented key (dbc web's too); s is the TUI's alias, below
	tabs, _ := m.ws.ResultTabs()
	if !tabs[0].Shared {
		t.Fatalf("S did not share; log: %s", logText(m))
	}
	if !strings.Contains(logText(m), "shared result 1 with the assistant") {
		t.Errorf("log: %s", logText(m))
	}
	if line := frame(m).Line(stripRow(m)); !strings.Contains(line, " 1✦ ") || !strings.Contains(line, "S share") {
		t.Errorf("strip = %q", line)
	}

	runSQL(t, m, "SELECT 2 AS two") // a new tab: the shared one is kept
	if tabs, cur := m.ws.ResultTabs(); len(tabs) != 2 || cur != 1 {
		t.Fatalf("after a run: %d tabs, cur %d", len(tabs), cur)
	}
	ctx, _ := m.chatContext("what is in it?")
	if !ctx.Shared || ctx.SharedLabel != "result 1" {
		t.Fatalf("context: shared %v label %q", ctx.Shared, ctx.SharedLabel)
	}
	if len(ctx.Columns) != 3 || len(ctx.Hidden) != 1 || ctx.Hidden[0] != 1 {
		t.Errorf("the shared result's view did not go along: columns %v hidden %v", ctx.Columns, ctx.Hidden)
	}

	key(t, m, "{")
	key(t, m, "s")
	if tabs, _ := m.ws.ResultTabs(); tabs[0].Shared {
		t.Error("s did not stop sharing")
	}
	if !strings.Contains(logText(m), "stopped sharing result 1") {
		t.Errorf("log: %s", logText(m))
	}
	if ctx, _ := m.chatContext("q"); ctx.Shared {
		t.Error("an unshared tab still goes as shared")
	}
}
