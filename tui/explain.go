package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/workspace"
)

// Explaining from the TUI. An explain is a RUN like any other: it takes the
// run slot (one at a time), shows its elapsed time, stops on Ctrl+K, and goes
// through the pinned session — the plan worth seeing is the one the next
// Ctrl+R would get, under the session's search_path, SETs and open
// transaction. What differs is where the outcome lands: the results pane's
// Plan tab, beside the Results tab rather than instead of it, so the rows
// from the last run stay one click away while the plan is studied.
//
//	Ctrl+X ─► explainQuery ─► ws.ExplainEditor ─► Job: Session.Explain ─► *workspace.ExplainDone
//	                                                                          │
//	            results pane: [ Results ] [ ◈ Plan ]  ◄── planv.set, resTab = tabPlan
//
// A plan also arrives the other way: a user who types EXPLAIN themselves and
// runs it gets a grid of plan text, which the workspace recognizes (with
// explain.Detect) and hands over as RunDone.Plan, so the same view opens on
// it (see runDone).

// resultsTab is which view the results pane shows.
type resultsTab int

const (
	tabResults resultsTab = iota
	tabPlan
)

// explainQuery is Ctrl+X (estimate) and Alt+X (analyze): explain the
// statement under the caret, or the selected one.
func (m *Model) explainQuery(analyze bool) tea.Cmd {
	return m.startRun(m.ws.ExplainEditor(m.editorState(), analyze))
}

// explainStmt explains one statement on the active connection. The
// workspace takes the run slot and warns before an analyze runs a write.
func (m *Model) explainStmt(stmt, where string, analyze bool) tea.Cmd {
	return m.startRun(m.ws.Explain(stmt, where, analyze))
}

// explainDone draws an explain, which the workspace has already landed —
// the last error cleared or set, the plan remembered for the assistant.
func (m *Model) explainDone(ev *workspace.ExplainDone) tea.Cmd {
	if ev.Stale {
		return nil
	}
	m.catsAfterTransition()
	if ev.Err != nil {
		m.notes(ev.Notes)
		m.setStatus(ev.Status)
		return nil
	}
	m.showPlan(ev.Plan)
	m.logPlan(ev.Plan, ev.Elapsed)
	m.notes(ev.Notes)
	return nil
}

// showPlan installs a plan and turns the results pane to it.
func (m *Model) showPlan(p *explain.Plan) {
	m.planv.set(p)
	m.resTab = tabPlan
	m.focus = focusGrid
	m.setStatus(planStatus(p))
}

// planStatus is the status-bar summary of a plan — workspace.PlanStatus,
// the words dbc web uses too.
func planStatus(p *explain.Plan) string { return workspace.PlanStatus(p) }

// countSev counts the plan's findings worth acting on.
func countSev(p *explain.Plan) (crit, warn int) { return p.Findings() }

// logPlan writes the plan's gist to the log: its size and cost, the
// comparison with the last plan of the statement, and the findings — so the
// log alone tells the story of a tuning session. The words are
// workspace.PlanNotes, shared with dbc web.
func (m *Model) logPlan(p *explain.Plan, elapsed time.Duration) {
	m.notes(workspace.PlanNotes(p, m.planv.prev, elapsed))
}

// ---------------------------------------------------------------------------
// Keys
// ---------------------------------------------------------------------------

// planKey handles keys while the results pane shows the plan: the ones that
// start work here, the view's own in planView.key.
func (m *Model) planKey(k tea.KeyPressMsg) tea.Cmd {
	v := m.planv
	switch k.String() {
	case "p":
		m.resTab = tabResults
		return nil
	case "a":
		return m.reexplain(true)
	case "e":
		return m.reexplain(false)
	case "b":
		return m.planInBrowser()
	case "y":
		return m.copyPlanText()
	case "Y":
		return m.copyPlanRaw()
	case "c", "menu":
		m.openPlanMenu(v.area.X+2, v.area.Y+3)
		return nil
	}
	v.key(k)
	return nil
}

// reexplain explains the plan's statement again — after adding an index, say,
// which is the moment the before/after comparison exists for.
func (m *Model) reexplain(analyze bool) tea.Cmd {
	p := m.planv.plan
	if p == nil || p.Statement == "" {
		return m.explainQuery(analyze)
	}
	if active := m.ws.Active(); p.Conn != "" && p.Conn != active {
		m.logf(logWarn, "this plan is from %s and %s is active — explaining on %s", p.Conn, active, active)
	}
	return m.explainStmt(p.Statement, "", analyze)
}

// ---------------------------------------------------------------------------
// Mouse
// ---------------------------------------------------------------------------

// planClick handles a left press in the plan view.
func (m *Model) planClick(x, y, n int) tea.Cmd {
	switch m.planv.click(x, y, n) {
	case chipAnalyze:
		return m.reexplain(true)
	case chipBrowser:
		return m.planInBrowser()
	case chipCopy:
		return m.copyPlanText()
	case chipResults:
		m.resTab = tabResults
	case chipCopySQL:
		return m.insightSQL(false)
	case chipInsertSQL:
		return m.insightSQL(true)
	}
	return nil
}

// resultsTabAt reports which tab of the results pane's title is at (x, y).
func (m *Model) resultsTabAt(x, y int) (resultsTab, bool) {
	switch {
	case m.lay.tabResults.Contains(x, y):
		return tabResults, true
	case m.lay.tabPlan.Contains(x, y):
		return tabPlan, true
	}
	return 0, false
}

// openPlanMenu is the plan view's context menu.
func (m *Model) openPlanMenu(x, y int) {
	v := m.planv
	p := v.plan
	if p == nil {
		return
	}
	n := v.node()
	analyzeWhy := ""
	if p.Statement == "" {
		analyzeWhy = "this plan came from a result — explain the statement itself with ^X"
	}
	items := []menuItem{
		{label: "◈ Explain again", key: "e", why: analyzeWhy, act: func(m *Model) tea.Cmd { return m.reexplain(false) }},
		{label: "▶ Explain analyze (runs it)", key: "a", why: analyzeWhy, act: func(m *Model) tea.Cmd { return m.reexplain(true) }},
		heading("view"),
		{label: "Tree", key: "1", act: func(m *Model) tea.Cmd { m.planv.mode = planTree; return nil }},
		{label: "Flame graph", key: "2", act: func(m *Model) tea.Cmd { m.planv.mode = planFlame; return nil }},
		{label: "Insights", key: "3", act: func(m *Model) tea.Cmd { m.planv.mode = planInsights; return nil }},
	}
	for _, mt := range p.Metrics() {
		label := "  size by " + mt.Label()
		if mt == v.metric {
			label = "● size by " + mt.Label()
		}
		items = append(items, menuItem{label: label, key: "m", act: func(m *Model) tea.Cmd { m.planv.metric = mt; return nil }})
	}
	items = append(items, heading("this step"),
		menuItem{label: "Copy step details", act: func(m *Model) tea.Cmd {
			return m.copyString(stepText(p, n), "the step's details")
		}},
		menuItem{label: "Zoom the flame graph here", act: func(m *Model) tea.Cmd {
			m.planv.flameRoot, m.planv.mode = n.ID, planFlame
			return nil
		}},
		menuItem{label: "✦ Ask the assistant about this step", key: "^A", act: func(m *Model) tea.Cmd {
			return m.askAbout(fmt.Sprintf("In this plan, what is the %q step doing, and is it a problem?", n.Title()))
		}},
		heading("plan"),
		menuItem{label: "Copy plan as text", key: "y", act: func(m *Model) tea.Cmd { return m.copyPlanText() }},
		menuItem{label: "Copy the engine's own output", key: "Y", act: func(m *Model) tea.Cmd { return m.copyPlanRaw() }},
		menuItem{label: "↗ Open in browser (interactive)", key: "b", act: func(m *Model) tea.Cmd { return m.planInBrowser() }},
		menuItem{label: "✦ Ask the assistant about this plan", key: "^A", act: func(m *Model) tea.Cmd {
			return m.askAbout("Explain this query plan: where does the time go, and what would you change?")
		}},
		heading(""),
		menuItem{label: "▦ Back to the results", key: "p", act: func(m *Model) tea.Cmd { m.resTab = tabResults; return nil }},
	)
	m.openMenu(x, y, items)
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

// copyPlanText copies the plan as the text tree, findings included — the
// form that pastes readably into a ticket or a chat.
func (m *Model) copyPlanText() tea.Cmd {
	p := m.planv.plan
	if p == nil {
		return nil
	}
	text := p.Text(explain.TextOptions{Width: 120, Metric: m.planv.metric, Insights: true})
	if p.Statement != "" {
		text = p.Statement + "\n\n" + text
	}
	return m.copyString(text, "the plan")
}

// copyPlanRaw copies what the engine itself printed — what a DBA, a forum or
// explain.depesz.com expects.
func (m *Model) copyPlanRaw() tea.Cmd {
	if p := m.planv.plan; p != nil {
		return m.copyString(p.Raw, "the engine's plan output")
	}
	return nil
}

// insightSQL copies the selected finding's suggested statement, or puts it
// in the editor. Never runs it: a CREATE INDEX on a production table is a
// decision, and the editor is where it gets read first.
func (m *Model) insightSQL(insert bool) tea.Cmd {
	v := m.planv
	if v.plan == nil || v.insCur < 0 || v.insCur >= len(v.plan.Insights) {
		return nil
	}
	sql := v.plan.Insights[v.insCur].SQL
	if sql == "" {
		m.log(logWarn, "this finding has no statement to offer")
		return nil
	}
	if !insert {
		return m.copyString(sql, "the suggested statement")
	}
	text := m.editor.Text()
	if strings.TrimSpace(text) != "" {
		// on a line of its own after the buffer, so it neither lands
		// inside the statement being tuned nor runs with it by accident
		m.editor.move(m.editor.posAt(len(text)), false)
		sep := "\n\n"
		if strings.HasSuffix(text, "\n") {
			sep = "\n"
		}
		if !strings.HasSuffix(strings.TrimSpace(text), ";") {
			sep = ";" + sep
		}
		m.editor.Insert(sep)
	}
	m.editor.Insert(sql)
	m.focus = focusEditor
	m.drag.follow = true
	m.log(logOk, "inserted at the end of the editor — review it, then Ctrl+R runs it; ^X re-explains the query to compare")
	return nil
}

// planDir is where plans opened in the browser are written. A var so tests
// write into their own temp dir.
var planDir = explain.PlanDir

// planInBrowser writes the plan as an interactive page and opens it. The
// file stays, and its path is logged, so it can be sent to someone.
func (m *Model) planInBrowser() tea.Cmd {
	p := m.planv.plan
	if p == nil {
		return nil
	}
	path, err := p.WriteHTML(planDir())
	if err != nil {
		m.logf(logErr, "could not save the plan: %s", serr.StringFromErr(err))
		return nil
	}
	openURL("file://" + path)
	m.logf(logOk, "opened the plan in your browser — saved as %s", path)
	return nil
}

// OpenURL opens a page in the user's browser, best effort — the headless
// `dbc explain --open` shares the TUI's opener.
func OpenURL(u string) { openURL(u) }

// stepText is one step's details as plain text, for "copy step details".
func stepText(p *explain.Plan, n *explain.Node) string {
	var b strings.Builder
	b.WriteString(n.Title() + "\n")
	rows, factor := explain.RowsCell(n, p.Analyzed)
	if rows != "" {
		b.WriteString("rows: " + strings.TrimSpace(rows+" "+factor) + "\n")
	}
	if n.HasActual {
		fmt.Fprintf(&b, "time: %s total, %s self\n", explain.FmtMs(n.TotalMs), explain.FmtMs(n.SelfMs))
	}
	if n.HasCost {
		fmt.Fprintf(&b, "cost: %s..%s\n", explain.FmtCost(n.StartupCost), explain.FmtCost(n.TotalCost))
	}
	for _, pr := range n.Props {
		b.WriteString(pr.Key + ": " + pr.Value + "\n")
	}
	return b.String()
}
