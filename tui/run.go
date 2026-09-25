package tui

import (
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

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

// tickMsg refreshes the status bar's elapsed time while run gen is in flight.
type tickMsg struct{ gen int }

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
func (m *Model) note(n workspace.Note) {
	kind := logInfo
	switch n.Level {
	case workspace.Ok:
		kind = logOk
	case workspace.Warn:
		kind = logWarn
	case workspace.Err:
		kind = logErr
	case workspace.Accent:
		kind = logAccent
	case workspace.Muted:
		kind = logMuted
	}
	m.log(kind, n.Text)
}

func (m *Model) notes(ns []workspace.Note) {
	for _, n := range ns {
		m.note(n)
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
	return tea.Batch(job(st.Job), tickCmd(st.Gen))
}

// ---------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------

// connectCmd opens the connection and fetches its catalog for the sidebar.
// A newer connect supersedes one still dialing — see workspace.Connect.
func (m *Model) connectCmd(name string) tea.Cmd {
	return job(m.ws.Connect(name).Job)
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

// setActive switches to a connection.
func (m *Model) setActive(name string) tea.Cmd {
	st := m.ws.Switch(name)
	m.notes(st.Notes)
	return job(st.Job)
}

// connected draws a connect's outcome. On a switch, the session pinned to
// the old connection is released by the event's Release job — separately,
// because it waits for any statement still running there.
func (m *Model) connected(ev *workspace.Connected) tea.Cmd {
	if ev.Stale {
		return nil // superseded by a later connect, which canceled this one
	}
	m.notes(ev.Notes)
	if ev.Status != "" {
		m.setStatus(ev.Status)
	}
	if ev.Err != nil {
		return nil
	}
	m.refreshConns()
	m.refreshTables()
	m.catsAfterTransition()
	return job(ev.Release)
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

// tickCmd schedules the next elapsed-time refresh for run gen.
func tickCmd(gen int) tea.Cmd {
	return tea.Tick(tickEvery, func(time.Time) tea.Msg { return tickMsg{gen: gen} })
}

// tick refreshes the running status and re-arms itself until the run ends.
func (m *Model) tick(msg tickMsg) tea.Cmd {
	status, ok := m.ws.Ticking(msg.gen)
	if !ok {
		return nil
	}
	m.setStatus(status)
	return tickCmd(msg.gen)
}

// runDone draws a run's outcome, which the workspace has already landed.
func (m *Model) runDone(ev *workspace.RunDone) tea.Cmd {
	if ev.Stale {
		return nil // a straggler from a run that was already written off
	}
	m.catsAfterTransition()
	if ev.Err != nil {
		m.notes(ev.Notes)
		m.setStatus(ev.Status)
		return nil
	}
	if ev.Script {
		m.notes(ev.Notes)
		if r := m.ws.LastResult(); r != nil {
			m.setStatus(resultStatus(r, m.cfg.MaxRows, m.grid.Rows()))
		}
		return nil
	}
	m.showResult(ev.Result)
	if ev.Plan != nil {
		// The raw output stays in the Results tab, one keypress (p) away.
		m.showPlan(ev.Plan)
		m.log(logAccent, "that result is a query plan — shown in the ◈ Plan tab (p switches back to the raw rows)")
	}
	m.notes(ev.Notes)
	return nil
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
	m.grid.SetResult(r, m.cfg.MaxDisplayRows)
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
