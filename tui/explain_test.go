package tui

import (
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/explain"
)

// explainModel is the test model with an orders table big enough (20,000
// rows) that a full scan of it is a finding, sized for the plan's detail
// panel to sit beside the tree.
func explainModel(t *testing.T) *Model {
	t.Helper()
	m := newTestModel(t)
	for _, s := range []string{
		"CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INT, total REAL, status TEXT)",
		`WITH RECURSIVE g(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM g WHERE n < 20000)
		 INSERT INTO orders SELECT n, 1+n%500, n%300, CASE n%4 WHEN 0 THEN 'paid' ELSE 'new' END FROM g`,
	} {
		if _, err := m.mgr.Run(m.ws.Active(), s); err != nil {
			t.Fatal(err)
		}
	}
	drive(t, m, tea.WindowSizeMsg{Width: 150, Height: 44})
	m.editor.SetText("SELECT o.id, c.name FROM orders o JOIN cats c ON c.id = o.user_id WHERE o.total > 250")
	return m
}

// Ctrl+X explains the statement under the caret into the Plan tab, and the
// log carries the headline finding.
func TestExplainOpensPlanTab(t *testing.T) {
	m := explainModel(t)
	key(t, m, "ctrl+x")
	if m.resTab != tabPlan || m.planv.plan == nil || m.focus != focusGrid {
		t.Fatalf("tab=%v plan=%v focus=%v\n%s", m.resTab, m.planv.plan != nil, m.focus, logText(m))
	}
	c := frame(m)
	for _, want := range []string{"◈ Plan", "SCAN · orders o", "Insights ▲1", "table 20k"} {
		findText(t, c, want)
	}
	if !strings.Contains(logText(m), "▲ Full scan of orders (20,000 rows)") {
		t.Errorf("log:\n%s", logText(m))
	}
	if !strings.HasPrefix(m.status, "plan · sqlite") {
		t.Errorf("status = %q", m.status)
	}
	// the plan opens on the step with the serious finding
	if n := m.planv.node(); n.Relation != "orders" {
		t.Errorf("selected %s, want the scan of orders", n.Title())
	}
}

// The results pane keeps both: p flips between them, the tabs click, and a
// new run's result takes the pane back to the grid.
func TestPlanAndResultsTabs(t *testing.T) {
	m := explainModel(t)
	key(t, m, "ctrl+r")
	key(t, m, "ctrl+x")
	if m.resTab != tabPlan {
		t.Fatal("explain should show the plan")
	}
	key(t, m, "p")
	if m.resTab != tabResults {
		t.Fatal("p should show the results")
	}
	findText(t, frame(m), "│ id") // the grid is drawn
	key(t, m, "p")
	if m.resTab != tabPlan {
		t.Fatal("p should show the plan again")
	}
	r := m.lay.tabResults
	click(t, m, r.X+1, r.Y)
	if m.resTab != tabResults {
		t.Fatal("clicking the Results tab")
	}
	r = m.lay.tabPlan
	click(t, m, r.X+1, r.Y)
	if m.resTab != tabPlan {
		t.Fatal("clicking the Plan tab")
	}
	key(t, m, "ctrl+r")
	if m.resTab != tabResults {
		t.Error("a new result should take the pane back to the grid")
	}
}

func TestExplainToolbarButton(t *testing.T) {
	m := explainModel(t)
	x, y := findText(t, frame(m), "◈ Explain")
	click(t, m, x+1, y)
	if m.planv.plan == nil {
		t.Fatalf("the toolbar button should explain:\n%s", logText(m))
	}
}

func TestExplainRefusesSeveralStatements(t *testing.T) {
	m := explainModel(t)
	m.editor.SetText("SELECT 1; SELECT 2")
	m.editor.SelectAll()
	key(t, m, "ctrl+x")
	if m.planv.plan != nil || !strings.Contains(logText(m), "select one to explain") {
		t.Errorf("log:\n%s", logText(m))
	}
}

// Alt+X on a write, on SQLite: the plan is shown, the write is not run.
func TestExplainAnalyzeWriteDoesNotRun(t *testing.T) {
	m := explainModel(t)
	m.editor.SetText("UPDATE orders SET total = -1")
	key(t, m, "alt+x")
	p := m.planv.plan
	if p == nil || p.Measured || len(p.Notes) == 0 || !strings.Contains(p.Notes[0], "not analyzed") {
		t.Fatalf("plan=%v\n%s", p, logText(m))
	}
	res, err := m.mgr.Run(m.ws.Active(), "SELECT count(*) FROM orders WHERE total = -1")
	if err != nil || res.Rows[0][0] != "0" {
		t.Fatalf("the UPDATE ran: %v %v", res, err)
	}
	findText(t, frame(m), "ⓘ not analyzed")
}

// Alt+X on a read measures it; explaining it again shows the before/after.
func TestReexplainComparesWithThePreviousPlan(t *testing.T) {
	m := explainModel(t)
	key(t, m, "alt+x")
	first := m.planv.plan
	if first == nil || !first.Measured {
		t.Fatalf("a read should be measured:\n%s", logText(m))
	}
	key(t, m, "a") // the plan's own key: explain it again, analyzed
	if m.planv.prev != first {
		t.Fatal("the previous plan of the same statement should be kept for comparison")
	}
	if cmp, _ := m.planv.comparison(); cmp == "" {
		t.Error("two measured runs should compare")
	}
	if !strings.Contains(logText(m), "vs the last plan of this statement") {
		t.Errorf("log:\n%s", logText(m))
	}
}

// The tree folds and selects by mouse, and the keys walk it.
func TestPlanTreeMouseAndKeys(t *testing.T) {
	m := explainModel(t)
	key(t, m, "ctrl+x")
	v := m.planv
	before := len(v.visible())
	var loop planRowHit
	for _, r := range v.rows {
		if v.plan.Node(r.id).Op == "Nested loop" {
			loop = r
		}
	}
	if loop.r.Empty() {
		t.Fatalf("no nested loop row drawn:\n%s", frame(m).Text())
	}
	click(t, m, loop.twisty.X, loop.twisty.Y)
	if len(v.visible()) != before-2 || !v.collapsed[loop.id] {
		t.Fatalf("folding the loop should hide its two steps: %d → %d", before, len(v.visible()))
	}
	findText(t, frame(m), "+2")
	key(t, m, "right") // unfold
	if len(v.visible()) != before {
		t.Error("right unfolds")
	}
	key(t, m, "right") // then descends
	if v.plan.Node(v.cur).Parent().ID != loop.id {
		t.Errorf("right on an unfolded step selects its first child, got %s", v.node().Title())
	}
	key(t, m, "left")
	if v.cur != loop.id {
		t.Error("left on a leaf climbs to the parent")
	}
	click(t, m, loop.r.X+30, loop.r.Y+2)
	if v.plan.Node(v.cur).Parent().ID != loop.id {
		t.Error("clicking a row selects it")
	}
}

func TestPlanFlameAndInsightViews(t *testing.T) {
	m := explainModel(t)
	key(t, m, "ctrl+x")
	key(t, m, "2")
	c := frame(m)
	findText(t, c, "sized by shape")
	if len(m.planv.flame) == 0 {
		t.Fatal("no flame blocks drawn")
	}
	// double-clicking a block zooms the graph into it
	b := m.planv.flame[1]
	drive(t, m, tea.MouseClickMsg{X: b.x0, Y: b.y, Button: tea.MouseLeft})
	drive(t, m, tea.MouseClickMsg{X: b.x0, Y: b.y, Button: tea.MouseLeft})
	if m.planv.flameRoot != b.id {
		t.Errorf("flame root = %d, want %d", m.planv.flameRoot, b.id)
	}
	key(t, m, "esc")
	if m.planv.flameRoot != 0 {
		t.Error("esc zooms back out")
	}

	key(t, m, "3")
	findText(t, frame(m), "go to step")
	key(t, m, "enter")
	if m.planv.mode != planTree || m.planv.node().Relation != "orders" {
		t.Errorf("enter on a finding goes to its step: mode=%v at %s", m.planv.mode, m.planv.node().Title())
	}
}

// A finding's suggested statement goes into the editor, after the buffer,
// and is not run.
func TestInsightSQLInsertsWithoutRunning(t *testing.T) {
	m := explainModel(t)
	b, err := os.ReadFile("../explain/testdata/pg_parallel_seqscan.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := explain.ParsePostgresJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	m.showPlan(p)
	key(t, m, "3")
	var hit insightHit
	for _, h := range m.planv.ins {
		if !h.insertSQL.Empty() {
			hit = h
		}
	}
	if hit.insertSQL.Empty() {
		t.Fatalf("no insert chip:\n%s", frame(m).Text())
	}
	ran := m.ws.LastResult()
	click(t, m, hit.insertSQL.X+1, hit.insertSQL.Y)
	text := m.editor.Text()
	if !strings.HasSuffix(text, "CREATE INDEX idx_orders_user_id ON orders (user_id);") ||
		!strings.HasPrefix(text, "SELECT o.id") || m.focus != focusEditor {
		t.Fatalf("editor:\n%s", text)
	}
	if m.ws.LastResult() != ran || m.ws.Busy() {
		t.Error("inserting must not run anything")
	}

	key(t, m, "ctrl+x") // back to a plan to use the copy chip
	m.showPlan(p)
	key(t, m, "3")
	for _, h := range m.planv.ins {
		if !h.copySQL.Empty() {
			click(t, m, h.copySQL.X+1, h.copySQL.Y)
		}
	}
	if got := lastClip(t).Text; got != "CREATE INDEX idx_orders_user_id ON orders (user_id);" {
		t.Errorf("copied %q", got)
	}
}

func TestPlanCopyAndBrowser(t *testing.T) {
	m := explainModel(t)
	dir := t.TempDir()
	prev := planDir
	planDir = func() string { return dir }
	t.Cleanup(func() { planDir = prev })

	key(t, m, "ctrl+x")
	key(t, m, "y")
	if got := lastClip(t).Text; !strings.Contains(got, "Plan · sqlite") || !strings.Contains(got, "Insights") {
		t.Errorf("copied:\n%s", got)
	}
	key(t, m, "b")
	if len(openLog) != 1 || !strings.HasPrefix(openLog[0], "file://"+dir) {
		t.Fatalf("opened %v", openLog)
	}
	page, err := os.ReadFile(strings.TrimPrefix(openLog[0], "file://"))
	if err != nil || !strings.Contains(string(page), `id="plan-data"`) {
		t.Errorf("page: %v", err)
	}
}

// Save as PDF / JPEG from the plan menu: the file lands beside the pages
// `b` writes and is opened; "copy as Mermaid" copies the chart.
func TestPlanFilesFromTheMenu(t *testing.T) {
	m := explainModel(t)
	dir := t.TempDir()
	prev := planDir
	planDir = func() string { return dir }
	t.Cleanup(func() { planDir = prev })

	key(t, m, "ctrl+x")
	for ext, magic := range map[string]string{"pdf": "%PDF-1.4", "jpg": "\xff\xd8\xff"} {
		openLog = nil
		m.planFile(ext)
		if len(openLog) != 1 || !strings.HasPrefix(openLog[0], "file://"+dir) || !strings.HasSuffix(openLog[0], "."+ext) {
			t.Fatalf("%s: opened %v\n%s", ext, openLog, logText(m))
		}
		b, err := os.ReadFile(strings.TrimPrefix(openLog[0], "file://"))
		if err != nil || !strings.HasPrefix(string(b), magic) {
			t.Errorf("%s: %v, starts %.8q", ext, err, b)
		}
	}
}

// A user who types EXPLAIN and runs it gets the plan view on its output; the
// raw rows stay in the Results tab.
func TestUserExplainResultShowsAsPlan(t *testing.T) {
	m := explainModel(t)
	m.editor.SetText("EXPLAIN QUERY PLAN SELECT * FROM orders WHERE total > 5")
	key(t, m, "ctrl+r")
	if m.resTab != tabPlan || m.planv.plan == nil || m.planv.plan.Statement != "SELECT * FROM orders WHERE total > 5" {
		t.Fatalf("tab=%v\n%s", m.resTab, logText(m))
	}
	if m.ws.LastResult() == nil || m.ws.LastResult().Columns[3] != "detail" {
		t.Error("the raw EXPLAIN rows should still be the result")
	}
	// an ordinary query does not
	m.editor.SetText("SELECT * FROM orders LIMIT 3")
	key(t, m, "ctrl+r")
	if m.resTab != tabResults {
		t.Error("an ordinary result is not a plan")
	}
}

// The assistant gets the plan with a question about the explained statement,
// and not with a question about a different one.
func TestPlanGoesToTheAssistantForItsStatement(t *testing.T) {
	m := explainModel(t)
	key(t, m, "ctrl+x")
	ctx, _ := m.chatContext("why is this slow?")
	if !strings.Contains(ctx.Plan, "SCAN · orders o") {
		t.Fatalf("plan context = %q", ctx.Plan)
	}
	m.editor.SetText("SELECT * FROM cats")
	if ctx, _ = m.chatContext("and this?"); ctx.Plan != "" {
		t.Error("a plan must not go with a question about another statement")
	}
}

// z gives the results pane the column, and gives it back.
func TestResultsZoom(t *testing.T) {
	m := explainModel(t)
	key(t, m, "ctrl+x")
	h := m.lay.results.H
	key(t, m, "z")
	frame(m)
	if !m.resZoom || m.lay.results.H <= h || !m.lay.logR.Empty() {
		t.Fatalf("zoom: %v %d → %d", m.resZoom, h, m.lay.results.H)
	}
	key(t, m, "z")
	frame(m)
	if m.lay.results.H != h {
		t.Error("z again restores the layout")
	}
}

// Right-click on a step opens the plan menu about that step.
func TestPlanContextMenu(t *testing.T) {
	m := explainModel(t)
	key(t, m, "ctrl+x")
	row := m.planv.rows[0]
	rightClick(t, m, row.r.X+20, row.r.Y)
	if m.menu == nil || m.planv.cur != row.id {
		t.Fatal("no menu, or the clicked step was not selected")
	}
	findText(t, frame(m), "↗ Open in browser (interactive)")
}
