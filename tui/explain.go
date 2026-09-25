package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/model"
)

// Explaining from the TUI. An explain is a RUN like any other: it takes the
// run slot (one at a time), shows its elapsed time, stops on Ctrl+K, and goes
// through the pinned session — the plan worth seeing is the one the next
// Ctrl+R would get, under the session's search_path, SETs and open
// transaction. What differs is where the outcome lands: the results pane's
// Plan tab, beside the Results tab rather than instead of it, so the rows
// from the last run stay one click away while the plan is studied.
//
//	Ctrl+X ─► explainQuery ─► beginRun ─► onSession(Session.Explain) ─► explainDoneMsg
//	                                                                          │
//	            results pane: [ Results ] [ ◈ Plan ]  ◄── planv.set, resTab = tabPlan
//
// A plan also arrives the other way: a user who types EXPLAIN themselves and
// runs it gets a grid of plan text, which explain.Detect recognizes, so the
// same view opens on it (see maybePlan).

// resultsTab is which view the results pane shows.
type resultsTab int

const (
	tabResults resultsTab = iota
	tabPlan
)

// explainDoneMsg lands an explain's outcome.
type explainDoneMsg struct {
	gen  int
	conn string
	tag  string
	stmt string
	plan *explain.Plan
	err  error
}

// explainQuery is Ctrl+X (estimate) and Alt+X (analyze): explain the
// statement under the caret, or the selected one.
func (m *Model) explainQuery(analyze bool) tea.Cmd {
	stmts, where := m.stmtsToRun()
	switch {
	case len(stmts) == 0:
		m.log(logWarn, "nothing to explain — type a query first")
		return nil
	case len(stmts) > 1:
		m.logf(logWarn, "the selection holds %d statements — select one to explain, or put the caret in it", len(stmts))
		return nil
	}
	return m.explainStmt(stmts[0], where, analyze)
}

// explainStmt explains one statement on the active connection.
func (m *Model) explainStmt(stmt, where string, analyze bool) tea.Cmd {
	if m.active == "" {
		m.log(logWarn, "no active connection — pick one in the sidebar")
		return nil
	}
	tag := "explain"
	if analyze {
		tag = "explain analyze"
	}
	if where != "" && where != "query" {
		tag += " (" + where + ")"
	}
	ctx, ok := m.beginRun(tag)
	if !ok {
		return nil
	}
	conn, gen := m.active, m.runGen
	m.logf(logInfo, "%s on %s — %s", tag, conn, preview(stmt))
	if analyze {
		m.explainAnalyzeWarning(stmt)
	}
	work := func() tea.Msg {
		var p *explain.Plan
		err := m.onSession(ctx, conn, func(s *db.Session) (err error) {
			p, err = s.Explain(ctx, stmt, db.ExplainOptions{Analyze: analyze})
			return err
		})
		return explainDoneMsg{gen: gen, conn: conn, tag: tag, stmt: stmt, plan: p, err: err}
	}
	return tea.Batch(work, tickCmd(gen))
}

// explainAnalyzeWarning says, before it happens, that an analyze runs the
// statement — the one thing about EXPLAIN ANALYZE a user must not learn
// from its side effects.
func (m *Model) explainAnalyzeWarning(stmt string) {
	inner, _, _ := explain.Strip(stmt)
	cc, _ := m.cfg.ConnByName(m.active)
	if db.IsRead(inner) {
		m.log(logMuted, "analyze runs the statement to time it — Ctrl+K stops it")
		return
	}
	if explain.Engine(cc.Driver) == explain.Postgres {
		m.log(logWarn, "analyzing a write: it runs inside a transaction dbc rolls back, so no rows change")
	}
}

// explainDone lands an explain.
func (m *Model) explainDone(msg explainDoneMsg) tea.Cmd {
	if msg.gen != m.runGen {
		return nil
	}
	elapsed := m.endRun()
	if msg.err != nil {
		// remembered like a failed run's, so "✦ ask why" carries the error
		// with the statement that caused it
		m.lastStmt = msg.stmt
		m.reportRunErr(msg.conn, msg.tag, msg.err, elapsed)
		return nil
	}
	// A plan is not a result, so lastStmt stays the last RUN statement —
	// otherwise the assistant would be sent the grid's rows as this
	// statement's. Only an error this same statement left behind is cleared.
	if msg.stmt == m.lastStmt {
		m.lastErr = ""
	}
	m.showPlan(msg.plan)
	m.logPlan(msg.plan, elapsed)
	return nil
}

// showPlan installs a plan and turns the results pane to it.
func (m *Model) showPlan(p *explain.Plan) {
	m.planv.set(p)
	m.resTab = tabPlan
	m.focus = focusGrid
	m.setStatus(planStatus(p))
}

// planStatus is the status-bar summary of a plan.
func planStatus(p *explain.Plan) string {
	s := strings.TrimPrefix(p.Headline(), "Plan · ")
	crit, warn := countSev(p)
	if crit+warn > 0 {
		s += fmt.Sprintf(" · %s", plural(crit+warn, "finding"))
	}
	return "plan · " + s
}

func countSev(p *explain.Plan) (crit, warn int) {
	for _, in := range p.Insights {
		switch in.Severity {
		case explain.SevCrit:
			crit++
		case explain.SevWarn:
			warn++
		}
	}
	return crit, warn
}

// logPlan writes the plan's gist to the log: its size and cost, and the
// headline finding — so the log alone tells the story of a tuning session.
func (m *Model) logPlan(p *explain.Plan, elapsed time.Duration) {
	m.logf(logOk, "explained in %s — %s", explain.FmtMs(float64(elapsed.Microseconds())/1000),
		strings.TrimPrefix(p.Headline(), "Plan · "))
	if cmp, _ := m.planv.comparison(); cmp != "" {
		m.logf(logAccent, "vs the last plan of this statement: %s", cmp)
	}
	for _, in := range p.Insights {
		if in.Severity == explain.SevInfo {
			continue
		}
		kind := logWarn
		if in.Severity == explain.SevCrit {
			kind = logErr
		}
		m.log(kind, in.Severity.Glyph()+" "+in.Title)
	}
}

// maybePlan recognizes a result that is itself a plan — the output of an
// EXPLAIN the user ran — and opens the Plan tab on it. The raw output stays
// in the Results tab, one keypress (p) away.
func (m *Model) maybePlan(r *model.Result) {
	if r == nil {
		return
	}
	cc, _ := m.cfg.ConnByName(r.Conn)
	p, ok := explain.Detect(r, cc.Driver)
	if !ok {
		return
	}
	m.showPlan(p)
	m.log(logAccent, "that result is a query plan — shown in the ◈ Plan tab (p switches back to the raw rows)")
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
	if p.Conn != "" && p.Conn != m.active {
		m.logf(logWarn, "this plan is from %s and %s is active — explaining on %s", p.Conn, m.active, m.active)
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

// planForChat is the plan text the assistant gets with a question — only
// when the plan is of the statement being asked about, so a question about
// a new query is not answered from an old one's plan.
func (m *Model) planForChat(query string) string {
	p := m.planv.plan
	if p == nil || p.Statement == "" {
		return ""
	}
	norm := func(s string) string {
		s, _, _ = explain.Strip(strings.TrimSuffix(strings.TrimSpace(s), ";"))
		return strings.ToLower(strings.Join(strings.Fields(s), " "))
	}
	if norm(p.Statement) != norm(query) {
		return ""
	}
	if v := m.planv; v.chatFor != p {
		v.chatText, v.chatFor = p.Text(explain.TextOptions{Width: 110, Insights: true}), p
	}
	return m.planv.chatText
}

// ---------------------------------------------------------------------------
// Session plumbing
// ---------------------------------------------------------------------------

// onSession runs f on the session pinned to conn, opening or replacing it as
// needed, under the same rules as a statement run — see runOnSession, which
// is this with f = Session.Run. Called from a command goroutine.
func (m *Model) onSession(ctx context.Context, conn string, f func(*db.Session) error) error {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	for retried := false; ; retried = true {
		if m.sess == nil || m.sessFor != conn {
			m.dropSessionLocked()
			sess, err := m.mgr.Session(ctx, conn)
			if err != nil {
				return err
			}
			m.sess, m.sessFor = sess, conn
		}
		err := f(m.sess)
		// db.Session.Classify holds the rule; see the Fault constants.
		// In short: retry only what never reached the server on a session
		// that held nothing; fail loudly when a transaction or setting died
		// with the connection; otherwise just stop using the dead session.
		switch m.sess.Classify(err) {
		case db.FaultRetry:
			m.dropSessionLocked()
			if !retried {
				continue
			}
		case db.FaultDrop:
			m.dropSessionLocked()
		case db.FaultLost:
			m.dropSessionLocked()
			return db.SessionLost(conn, err)
		}
		return err
	}
}
