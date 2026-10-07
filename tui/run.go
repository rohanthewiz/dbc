package tui

import (
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/workspace"
)

// Running statements and scripts. The rules — ONE RUN AT A TIME, A PINNED
// SESSION per active connection, CANCEL REACHES THE SERVER — live in package
// workspace, which the browser UI shares; see workspace/run.go for them.
// What is left here is the TUI's half: turning keys into workspace calls,
// running each call's Job as a tea.Cmd, and drawing the event it lands.
//
//	Ctrl+R ─► runQuery ─► ws.RunEditor ─► startRun ─┬─ log Start.Notes
//	                                                └─ tea.Batch(job(Start.Job), tickCmd)
//	                                                        │
//	Update ◄── *workspace.RunDone (already landed in ws) ◄──┘
//	  └─► runDone: grid, plan tab, log, status bar

// tickMsg refreshes the status bar's elapsed time while run gen is in flight
// on tab tab. A tick for a tab not on screen is dropped; activating the tab
// re-arms it (tabs.go).
type tickMsg struct{ gen, tab, seq int }

// tickEvery is how often the status bar's elapsed time refreshes while a run
// is in flight — often enough that a slow query looks alive, not hung.
const tickEvery = 150 * time.Millisecond

// job runs a workspace Job as a command. Its event — already landed in the
// workspace — comes back to Update as the message, routed on its concrete
// type (*workspace.RunDone, *workspace.Connected, …).
func job(j workspace.Job) tea.Cmd {
	if j == nil {
		return nil
	}
	return func() tea.Msg {
		if ev := j(); ev != nil {
			return ev
		}
		return nil // an untyped nil, so Bubble Tea sees no message
	}
}

// editorState is the editor as the workspace needs it to pick statements.
func (m *Model) editorState() workspace.Editor {
	sel, _, _ := m.editor.Selection()
	return workspace.Editor{Text: m.editor.Text(), Caret: m.editor.Caret(), Selection: sel}
}

// note writes one of the workspace's notes to the log.
func (m *Model) note(n workspace.Note) { m.log(noteKind(n.Level), n.Text) }

// noteKind is the log color of a workspace note's level.
func noteKind(l workspace.Level) logKind {
	switch l {
	case workspace.Ok:
		return logOk
	case workspace.Warn:
		return logWarn
	case workspace.Err:
		return logErr
	case workspace.Accent:
		return logAccent
	case workspace.Muted:
		return logMuted
	}
	return logInfo
}

func (m *Model) notes(ns []workspace.Note) {
	for _, n := range ns {
		m.note(n)
	}
}

// notesTo writes notes to conn's log (logs.go) — the shown one when conn
// is on screen, else that connection's, where they wait for the tab to
// come back to it.
func (m *Model) notesTo(conn string, ns []workspace.Note) {
	for _, n := range ns {
		m.logTo(conn, noteKind(n.Level), n.Text)
	}
}

// refused logs why the workspace declined a request.
func (m *Model) refused(err error) {
	var r *workspace.Refusal
	if errors.As(err, &r) {
		m.note(r.Note)
		return
	}
	m.log(logErr, serr.StringFromErr(err))
}

// startRun lands what a run-slot request (a run, an explain, a script)
// returned: its notes in the log, then — unless refused — the running status
// and the Job and its elapsed-time ticker as commands.
func (m *Model) startRun(st workspace.Start, err error) tea.Cmd {
	m.notes(st.Notes)
	if err != nil {
		m.refused(err)
		return nil
	}
	if st.Job == nil {
		return nil
	}
	m.setStatus(m.ws.RunningStatus())
	m.catsAfterTransition()
	m.active().gen = st.Gen
	return tea.Batch(m.tag(job(st.Job)), m.tickCmd(st.Gen))
}

// ---------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------

// connectCmd opens the connection and fetches its catalog for the sidebar.
// A newer connect supersedes one still dialing — see workspace.Connect.
// It opens on the schema last picked on the connection (schemaPicks, which
// persist across runs), so the first connect after a restart lands where
// the user left off; a pick the database no longer has falls back to the
// default schema (workspace.SchemaPick).
func (m *Model) connectCmd(name string) tea.Cmd {
	return m.tag(job(m.ws.ConnectPick(name, m.schemaPicks[name]).Job))
}

// cancelConnect abandons the connect in flight. It reports false when there
// is none.
func (m *Model) cancelConnect() bool {
	n, ok := m.ws.CancelConnect()
	if ok {
		m.note(n)
	}
	return ok
}

// setActive switches to a connection, opening its sidebar on the schema last
// picked there (navigator.go), or the default.
func (m *Model) setActive(name string) tea.Cmd {
	st := m.ws.SwitchPick(name, m.schemaPicks[name])
	m.notes(st.Notes)
	return m.tag(job(st.Job))
}

// refreshCatalog is r in the Connections pane and the connections menu's
// Refresh: workspace.Refresh re-reads the active connection's databases,
// schemas and tables, keeping its session and its schema pick. It lands as
// a connect back to the same connection does (connected), with a
// "refreshed …" line saying what the list now holds.
func (m *Model) refreshCatalog() tea.Cmd {
	st, err := m.ws.Refresh()
	if err != nil {
		m.refused(err)
		return nil
	}
	m.notes(st.Notes)
	m.setStatus("refreshing " + m.ws.Active() + "…")
	return m.tag(job(st.Job))
}

// connected draws a connect's outcome. On a switch, the session pinned to
// the old connection is released by the event's Release job — separately,
// because it waits for any statement still running there. The tables' row
// counts come from the Counts job, after the list has drawn.
func (m *Model) connected(ev *workspace.Connected) tea.Cmd {
	if ev.Stale {
		return nil // superseded by a later connect, which canceled this one
	}
	m.schemaLoading = "" // a connect supersedes a pick in flight
	m.notes(ev.Notes)
	if ev.Status != "" {
		m.setStatus(ev.Status)
	}
	if ev.Err != nil {
		return nil
	}
	m.active().unconnected = false
	m.syncResults()          // the results pane follows the connection (resulttabs.go)
	m.switchConsole(ev.Name) // the editor follows the database (console.go)
	m.refreshConns()
	if ev.Changed {
		m.refreshTables()
	} else {
		// the same connection's list read again — a Refresh, or a connect
		// back after its catalog failed — so the cursor stays on its table
		m.relistTables()
	}
	m.catsAfterTransition()
	return tea.Batch(m.tag(m.releaseThenClose(ev)), m.tag(job(ev.Counts)))
}

// sessionReleased tells the user when a released session took state with
// it. A session that only ever ran queries goes quietly: nothing was lost.
func (m *Model) sessionReleased(ev *workspace.SessionReleased) tea.Cmd {
	m.notes(ev.Notes)
	return nil
}

// ---------------------------------------------------------------------------
// Picking statements
// ---------------------------------------------------------------------------

// stmtsToRun is what Ctrl+R executes: see workspace.Pick.
func (m *Model) stmtsToRun() (stmts []string, tag string) {
	return workspace.Pick(m.editorState())
}

// currentStmtRange is the byte range of the statement under the caret, for
// the editor's gutter marker.
func (m *Model) currentStmtRange() [2]int {
	return workspace.StmtRange(m.editor.Text(), m.editor.Caret())
}

// allStmts is what Ctrl+Shift+R executes: see workspace.PickAll.
func (m *Model) allStmts() (stmts []string, tag string) {
	return workspace.PickAll(m.editor.Text())
}

// ---------------------------------------------------------------------------
// Running
// ---------------------------------------------------------------------------

// runQuery is Ctrl+R.
func (m *Model) runQuery() tea.Cmd {
	return m.startRun(m.ws.RunEditor(m.editorState(), false))
}

// runAll is Ctrl+Shift+R (Alt+R where the terminal cannot tell Ctrl+Shift+R
// from Ctrl+R). It is a second door onto the path a multi-statement
// selection already takes — in order, on the pinned session, stopping at the
// first failure, last result shown — so Ctrl+R keeps its one-statement
// default and a scratchpad of unrelated queries is never run by accident.
func (m *Model) runAll() tea.Cmd {
	return m.startRun(m.ws.RunEditor(m.editorState(), true))
}

// listTables is Ctrl+T: the active driver's catalog query, through the same
// path Ctrl+R uses, so it lands in the grid and exports like any result.
func (m *Model) listTables() tea.Cmd {
	return m.startRun(m.ws.ListTables())
}

// runScript runs a Go script. Its s.Show and s.Print reach Update through
// the workspace's sink (m.send) as they happen.
func (m *Model) runScript(path string) tea.Cmd {
	return m.startRun(m.ws.RunScript(path))
}

// tickCmd schedules the next elapsed-time refresh for run gen of the tab
// on screen.
func (m *Model) tickCmd(gen int) tea.Cmd {
	tab, seq := m.active().key, m.active().tickSeq
	return tea.Tick(tickEvery, func(time.Time) tea.Msg { return tickMsg{gen: gen, tab: tab, seq: seq} })
}

// tick refreshes the running status and re-arms itself until the run ends,
// or until its tab leaves the screen (its status is not the one drawn).
func (m *Model) tick(msg tickMsg) tea.Cmd {
	if msg.tab != m.active().key || msg.seq != m.active().tickSeq {
		return nil // another tab's, or an earlier visit's chain (tabs.go arrive)
	}
	status, ok := m.ws.Ticking(msg.gen)
	if !ok {
		return nil
	}
	m.setStatus(status)
	return m.tickCmd(msg.gen)
}

// runDone draws a run's outcome, which the workspace has already landed.
// A run that may have changed rows comes with a recount of the sidebar's
// row counts (RunDone.Counts), and one that may have changed the catalog
// with a re-read of the sidebar's list instead (RunDone.Relist, which
// counts the list it lands) — either started whatever the outcome. A run
// that landed while its tab was in the background had its relist started
// then (routeTab), and comes here with Relist nil.
func (m *Model) runDone(ev *workspace.RunDone) tea.Cmd {
	if ev.Stale {
		return nil // a straggler from a run that was already written off
	}
	m.catsAfterTransition()
	recount := tea.Batch(m.tag(job(ev.Counts)), m.tag(job(ev.Relist)))
	// The run landed in its own connection's result set (workspace
	// results.go), and its lines belong in that connection's log. When the
	// tab has switched away meanwhile, neither is on screen: nothing is
	// drawn, the notes go to the run's connection's log, and one line in
	// the log on screen says where they went — so a run finishing out of
	// sight is neither silent nor mixed into another connection's log.
	onScreen := m.landedHere(ev.Conn, ev.Tag, ev.Err, "result")
	if ev.Err != nil {
		if onScreen && ev.Result != nil {
			// the statements before the failure landed their results
			// (workspace landRun): show them, the status still the error's
			m.showResult(ev.Result)
		}
		m.notesTo(ev.Conn, ev.Notes)
		if onScreen {
			m.setStatus(ev.Status)
		}
		return recount
	}
	if ev.Script {
		m.notesTo(ev.Conn, ev.Notes)
		if r := m.ws.LastResult(); r != nil && onScreen {
			m.syncResults()
			m.setStatus(resultStatus(r, m.cfg.MaxRows, m.grid.Rows()))
		}
		return recount
	}
	if onScreen {
		m.showResult(ev.Result)
	}
	if ev.Plan != nil {
		// The raw output stays in the Results tab, one keypress (p) away.
		m.showPlanOn(ev.Conn, ev.Plan)
		m.logTo(ev.Conn, logAccent, "that result is a query plan — shown in the ◈ Plan tab (p switches back to the raw rows)")
	}
	m.notesTo(ev.Conn, ev.Notes)
	return recount
}

// landedHere reports whether a run (or explain) on conn landed on the
// connection on screen. When it did not — the tab switched away while it
// ran — the log on screen gets one line saying what became of it and where
// its lines went (conn's log, by notesTo), so a run ending out of sight is
// neither silent nor mixed into another connection's log. what is what it
// left there: "result" or "plan".
func (m *Model) landedHere(conn, tag string, err error, what string) bool {
	if conn == m.ws.Active() {
		return true
	}
	verb := "finished"
	switch {
	case errors.Is(err, db.ErrCanceled):
		verb = "was stopped"
	case err != nil:
		verb, what = "failed", "error"
	}
	m.logf(logMuted, "%s on %s %s — its %s and log lines are %s's (switch back to %s to see them)",
		tag, conn, verb, what, conn, conn)
	// the status bar still shows the run's last elapsed-time tick, which
	// would read as still running
	m.setStatus(fmt.Sprintf("%s on %s %s", tag, conn, verb))
	return false
}

// showResult puts a result in the grid and describes it in the status bar.
// A new result turns the results pane back to its grid: what just ran is
// what the user wants to see, even if a plan was on screen. (The workspace
// has already made it the last result.)
func (m *Model) showResult(r *model.Result) {
	if r == nil {
		return
	}
	m.resTab = tabResults
	// the workspace has put r in the current result tab; syncResults brings
	// that tab's grid on screen (resulttabs.go) and gives it r
	m.syncResults()
	if m.grid.Rows() < len(r.Rows) {
		m.logf(logWarn, "showing the first %d of %d rows — the rest are fetched, and go into an export (max_display_rows)",
			m.grid.Rows(), len(r.Rows))
	}
	m.setStatus(resultStatus(r, m.cfg.MaxRows, m.grid.Rows()))
}

// resultStatus is the status-bar summary of a result.
func resultStatus(r *model.Result, maxRows, shown int) string {
	return workspace.ResultStatus(r, maxRows, shown)
}

// cancelRun is Ctrl+K and the Stop button: the run in flight, or failing
// that the connect in flight.
func (m *Model) cancelRun() tea.Cmd {
	n, status := m.ws.Cancel()
	m.note(n)
	if status != "" {
		m.setStatus(status)
	}
	return nil
}

// preview is a one-line, length-capped echo of a statement for the log.
func preview(stmt string) string { return workspace.Preview(stmt) }
