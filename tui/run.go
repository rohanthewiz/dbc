package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/sdb"
	"github.com/rohanthewiz/dbc/sqlsplit"
)

// Running statements and scripts. The rules are the tview UI's, carried over
// unchanged because they were each learned the hard way:
//
//   - ONE RUN AT A TIME. A second Ctrl+R while one is in flight is refused
//     in words, not queued: a queued DELETE behind a slow SELECT is a
//     surprise.
//   - A PINNED SESSION per active connection, so BEGIN/COMMIT, SET and temp
//     tables carry across runs the way they do in psql. Switching
//     connections closes the old session, deliberately rolling back
//     whatever it left open. A pinned connection that has gone bad (a
//     server idle timeout) is replaced once, transparently — unless the
//     session held state (a BEGIN, a SET), in which case the run fails
//     with db.ErrSessionLost rather than carry on without it, or the
//     statement may already have reached the server, in which case its
//     error stands rather than risk running it twice (db.Session.Classify).
//   - CANCEL REACHES THE SERVER. Every run has a context; Ctrl+K, Ctrl+C
//     and the Stop button cancel it, and the drivers turn that into a
//     server-side cancel.

// Messages the run machinery sends to Update.
type (
	connectMsg struct {
		gen    int // the connGen it was started under
		name   string
		err    error
		tables *model.Result // the catalog, for the sidebar; nil if it failed
	}
	runDoneMsg struct {
		gen     int
		conn    string
		tag     string
		stmts   []string
		res     *model.Result
		err     error
		elapsed time.Duration
		script  bool
	}
	tickMsg        struct{ gen int }
	scriptShowMsg  struct{ res *model.Result }
	scriptPrintMsg struct{ text string }
	// sessionReleasedMsg reports that switching connections closed the
	// session pinned to the previous one, and whether it held state.
	sessionReleasedMsg struct {
		conn     string
		stateful bool
	}
)

// tickEvery is how often the status bar's elapsed time refreshes while a run
// is in flight — often enough that a slow query looks alive, not hung.
const tickEvery = 150 * time.Millisecond

// connectCmd opens the connection and fetches its catalog for the sidebar.
// The catalog goes through the pool, not the pinned session: it is the app's
// query, and must not land inside a transaction the user has open.
//
// The connect runs under its own context, so Ctrl+K or Ctrl+C can abandon
// it — without that, only connect_timeout bounds it, and with
// connect_timeout = "0" an unreachable host would hold "connecting…" until
// the OS gave up on the TCP connect, over a minute later. Starting a connect
// cancels any still in flight: the user has changed their mind, and the older
// one must not land after the newer one and switch the connection back.
//
//	pick A ──► connGen=1, dial A ───────────── (canceled) ──► connectMsg{gen 1} dropped
//	pick B ──► connGen=2, cancel A, dial B ─► connectMsg{gen 2} installed
func (m *Model) connectCmd(name string) tea.Cmd {
	if name == "" {
		return nil
	}
	driver := ""
	if cc, ok := m.cfg.ConnByName(name); ok {
		driver = cc.Driver
	}
	if m.connCancel != nil {
		m.connCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.connGen++
	gen := m.connGen
	m.connCancel, m.connName = cancel, name
	mgr := m.mgr
	return func() tea.Msg {
		defer cancel() // releases the context; connected may already have
		if _, err := mgr.DBContext(ctx, name); err != nil {
			return connectMsg{gen: gen, name: name, err: err}
		}
		var tables *model.Result
		if q, err := db.TablesQuery(driver); err == nil {
			tctx, tcancel := context.WithTimeout(ctx, 10*time.Second)
			tables, _ = mgr.RunContext(tctx, name, q)
			tcancel()
		}
		return connectMsg{gen: gen, name: name, tables: tables}
	}
}

// cancelConnect abandons the connect in flight. It reports false when there
// is none.
func (m *Model) cancelConnect() bool {
	if m.connCancel == nil {
		return false
	}
	m.connCancel()
	m.logf(logWarn, "canceling connect to %s…", m.connName)
	return true
}

// setActive switches to a connection.
func (m *Model) setActive(name string) tea.Cmd {
	if name == m.active && m.tableRes != nil {
		return nil
	}
	m.logf(logInfo, "connecting to %s…", name)
	return m.connectCmd(name)
}

// connected installs a connect's outcome.
func (m *Model) connected(msg connectMsg) tea.Cmd {
	if msg.gen != m.connGen {
		return nil // superseded by a later connect, which canceled this one
	}
	m.connCancel = nil
	if errors.Is(msg.err, db.ErrCanceled) {
		m.logf(logWarn, "connect to %s canceled", msg.name)
		m.setStatus("connect canceled")
		return nil
	}
	if msg.err != nil {
		m.logf(logErr, "connect failed: %s", serr.StringFromErr(msg.err))
		return nil
	}
	changed := msg.name != m.active
	m.active = msg.name
	m.tableRes = msg.tables
	m.tableIdx = nil
	if msg.tables != nil {
		m.tableIdx = db.NewTableIndex(db.TableRefs(msg.tables.Rows))
	}
	m.refreshConns()
	m.refreshTables()
	m.catsAfterTransition()
	if changed {
		m.logf(logOk, "connected to %s", msg.name)
		m.setStatus("connected")
		return m.releaseSessionCmd(msg.name)
	}
	return nil
}

// releaseSessionCmd closes the session pinned to any connection other than
// keep, so switching connections ends the old session — rolling back what it
// left open — then and there, rather than holding its transaction and locks
// open until the next run happens to replace it.
//
// It runs as a command, off the Update goroutine, because it takes sessMu,
// which a run in flight holds for as long as its statement takes. It is issued
// only after m.active has moved to keep, so a run started in the meantime is
// already on keep and its session is left alone.
func (m *Model) releaseSessionCmd(keep string) tea.Cmd {
	return func() tea.Msg {
		m.sessMu.Lock()
		defer m.sessMu.Unlock()
		if m.sess == nil || m.sessFor == keep {
			return nil
		}
		msg := sessionReleasedMsg{conn: m.sessFor, stateful: m.sess.Stateful()}
		m.dropSessionLocked()
		return msg
	}
}

// sessionReleased tells the user when a released session took state with it.
// A session that only ever ran queries goes quietly: nothing was lost.
func (m *Model) sessionReleased(msg sessionReleasedMsg) tea.Cmd {
	if msg.stateful {
		m.logf(logWarn, "left %s: its session was closed — any open transaction was rolled back, SET values and temp tables are gone",
			msg.conn)
	}
	return nil
}

// runOnSession executes one statement on the session pinned to conn,
// opening or replacing it as needed. Called from a command goroutine.
func (m *Model) runOnSession(ctx context.Context, conn, stmt string) (*model.Result, error) {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	for retried := false; ; retried = true {
		if m.sess == nil || m.sessFor != conn {
			m.dropSessionLocked()
			sess, err := m.mgr.Session(ctx, conn)
			if err != nil {
				return nil, err
			}
			m.sess, m.sessFor = sess, conn
		}
		res, err := m.sess.Run(ctx, stmt)
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
			return nil, db.SessionLost(conn, err)
		}
		return res, err
	}
}

func (m *Model) dropSessionLocked() {
	if m.sess != nil {
		_ = m.sess.Close()
		m.sess, m.sessFor = nil, ""
	}
}

func (m *Model) dropSession() {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	m.dropSessionLocked()
}

// stmtsToRun picks what Ctrl+R executes: the selection's statements when
// there is a selection, otherwise the statement under the caret. tag names
// the run for the status bar and log.
func (m *Model) stmtsToRun() (stmts []string, tag string) {
	if sel, _, _ := m.editor.Selection(); strings.TrimSpace(sel) != "" {
		parts := sqlsplit.Split(sel)
		for _, p := range parts {
			stmts = append(stmts, p.Text)
		}
		switch len(stmts) {
		case 0:
			return nil, ""
		case 1:
			return stmts, "selection"
		}
		return stmts, fmt.Sprintf("selection (%d statements)", len(stmts))
	}
	all := sqlsplit.Split(m.editor.Text())
	if len(all) == 0 {
		return nil, ""
	}
	i := sqlsplit.IndexAt(all, m.editor.Caret())
	if len(all) == 1 {
		return []string{all[i].Text}, "query"
	}
	return []string{all[i].Text}, fmt.Sprintf("statement %d/%d", i+1, len(all))
}

// currentStmtRange is the byte range of the statement under the caret, for
// the editor's gutter marker.
func (m *Model) currentStmtRange() [2]int {
	all := sqlsplit.Split(m.editor.Text())
	if len(all) < 2 {
		return [2]int{} // one statement: marking it says nothing
	}
	i := sqlsplit.IndexAt(all, m.editor.Caret())
	return [2]int{all[i].Start, all[i].End}
}

// runQuery is Ctrl+R.
func (m *Model) runQuery() tea.Cmd {
	stmts, tag := m.stmtsToRun()
	if len(stmts) == 0 {
		m.log(logWarn, "nothing to run — type a query first")
		return nil
	}
	// recorded before the run: the query worth recalling is very often the
	// one that just failed
	for _, s := range stmts {
		m.record(s)
	}
	return m.run(stmts, tag)
}

// record adds a statement to the history; a write failure is reported once.
func (m *Model) record(stmt string) {
	err := m.hist.Add(m.active, stmt, time.Now())
	if err == nil || m.histWarned {
		return
	}
	m.histWarned = true
	m.logf(logWarn, "query history is not being saved: %s", serr.StringFromErr(err))
}

// listTables is Ctrl+T: the active driver's catalog query, through the same
// path Ctrl+R uses, so it lands in the grid and exports like any result.
func (m *Model) listTables() tea.Cmd {
	cc, ok := m.cfg.ConnByName(m.active)
	if !ok {
		m.log(logWarn, "no active connection — pick one in the sidebar")
		return nil
	}
	q, err := db.TablesQuery(cc.Driver)
	if err != nil {
		m.log(logErr, serr.StringFromErr(err))
		return nil
	}
	return m.run([]string{q}, "list tables")
}

// beginRun claims the run slot. It reports false (and says why) when a run
// is already in flight.
func (m *Model) beginRun(tag string) (context.Context, bool) {
	if m.busy {
		m.logf(logWarn, "busy — %s is still running (Ctrl+K stops it)", m.runTag)
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.busy, m.cancel, m.runTag, m.runAt = true, cancel, tag, time.Now()
	m.runGen++
	m.setStatus(m.runningStatus())
	m.catsAfterTransition()
	return ctx, true
}

// endRun releases the slot and returns how long the run took.
func (m *Model) endRun() time.Duration {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.busy = false
	m.catsAfterTransition()
	return time.Since(m.runAt)
}

// tickCmd schedules the next elapsed-time refresh for run gen.
func tickCmd(gen int) tea.Cmd {
	return tea.Tick(tickEvery, func(time.Time) tea.Msg { return tickMsg{gen: gen} })
}

// tick refreshes the running status and re-arms itself until the run ends.
func (m *Model) tick(msg tickMsg) tea.Cmd {
	if !m.busy || msg.gen != m.runGen {
		return nil
	}
	m.setStatus(m.runningStatus())
	return tickCmd(msg.gen)
}

func (m *Model) runningStatus() string {
	return fmt.Sprintf("%s %s", m.runTag, time.Since(m.runAt).Round(100*time.Millisecond))
}

// run executes statements in order on the pinned session, stopping at the
// first failure, and publishes the last result.
func (m *Model) run(stmts []string, tag string) tea.Cmd {
	if m.active == "" {
		m.log(logWarn, "no active connection — pick one in the sidebar")
		return nil
	}
	ctx, ok := m.beginRun(tag)
	if !ok {
		return nil
	}
	conn, gen := m.active, m.runGen
	m.logf(logInfo, "running %s on %s — %s", tag, conn, preview(strings.Join(stmts, "; ")))
	work := func() tea.Msg {
		var res *model.Result
		var err error
		for i, stmt := range stmts {
			res, err = m.runOnSession(ctx, conn, stmt)
			if err != nil {
				if len(stmts) > 1 {
					err = serr.Wrap(err, "statement", fmt.Sprintf("%d/%d", i+1, len(stmts)))
				}
				break
			}
		}
		return runDoneMsg{gen: gen, conn: conn, tag: tag, stmts: stmts, res: res, err: err}
	}
	return tea.Batch(work, tickCmd(gen))
}

// runDone lands a run's outcome.
func (m *Model) runDone(msg runDoneMsg) tea.Cmd {
	if msg.gen != m.runGen {
		return nil // a straggler from a run that was already written off
	}
	elapsed := m.endRun()
	if len(msg.stmts) > 0 {
		m.lastStmt = msg.stmts[len(msg.stmts)-1]
	}
	if msg.err != nil {
		m.reportRunErr(msg.conn, msg.tag, msg.err, elapsed)
		return nil
	}
	if msg.script {
		m.lastErr = ""
		m.logf(logOk, "%s completed in %s", msg.tag, elapsed.Round(time.Millisecond))
		if m.lastRes != nil {
			m.setStatus(resultStatus(m.lastRes, m.cfg.MaxRows, m.grid.Rows()))
		}
		return nil
	}
	m.lastErr = ""
	m.showResult(msg.res)
	if len(msg.stmts) > 1 {
		m.logf(logOk, "%d statements completed — showing the last result", len(msg.stmts))
	}
	return nil
}

// showResult puts a result in the grid and describes it in the status bar.
func (m *Model) showResult(r *model.Result) {
	if r == nil {
		return
	}
	m.lastRes = r
	m.grid.SetResult(r, m.cfg.MaxDisplayRows)
	if m.grid.Rows() < len(r.Rows) {
		m.logf(logWarn, "showing the first %d of %d rows — the rest are fetched, and go into an export (max_display_rows)",
			m.grid.Rows(), len(r.Rows))
	}
	m.setStatus(resultStatus(r, m.cfg.MaxRows, m.grid.Rows()))
}

// resultStatus is the status-bar summary of a result.
func resultStatus(r *model.Result, maxRows, shown int) string {
	verb := fmt.Sprintf("%d rows", len(r.Rows))
	if r.IsExec {
		verb = fmt.Sprintf("%d affected", r.Affected)
	}
	s := fmt.Sprintf("%s in %s", verb, r.Duration.Round(10*time.Microsecond))
	if r.Truncated {
		s += fmt.Sprintf(" (truncated at %d)", maxRows)
	}
	if shown < len(r.Rows) {
		s += fmt.Sprintf(" (showing %d)", shown)
	}
	return s
}

// reportRunErr writes a failed or canceled run to the log and status bar,
// and remembers a real failure for the assistant to explain.
func (m *Model) reportRunErr(conn, tag string, err error, elapsed time.Duration) {
	if errors.Is(err, db.ErrCanceled) {
		m.logf(logWarn, "%s stopped after %s", tag, elapsed.Round(time.Millisecond))
		m.setStatus(fmt.Sprintf("stopped after %s", elapsed.Round(time.Millisecond)))
		// Stopping a statement can cost the connection (the driver may
		// close it to abort the statement). When the session held state,
		// that is worth more than "stopped" alone.
		if errors.Is(err, db.ErrSessionLost) {
			m.log(logWarn, "the session was lost with it — its open transaction, SET values and temp tables are gone")
		}
		return
	}
	m.lastErr = serr.StringFromErr(err)
	m.log(logErr, m.lastErr)
	m.setStatus(fmt.Sprintf("error after %s — ✦ ask the assistant why", elapsed.Round(time.Millisecond)))
	_ = conn
}

// cancelRun is Ctrl+K and the Stop button.
func (m *Model) cancelRun() tea.Cmd {
	if !m.busy || m.cancel == nil {
		// no run, but a connect may be dialing — Ctrl+K stops that too
		if !m.cancelConnect() {
			m.log(logWarn, "nothing is running")
		}
		return nil
	}
	m.cancel()
	m.logf(logWarn, "stopping %s…", m.runTag)
	m.setStatus("stopping " + m.runTag + "…")
	return nil
}

// runScript runs a Go script. Its s.Show and s.Print callbacks fire from the
// script's goroutine mid-run, so they reach Update through m.send rather
// than as the command's return value.
func (m *Model) runScript(path string) tea.Cmd {
	tag := "script " + filepath.Base(path)
	ctx, ok := m.beginRun(tag)
	if !ok {
		return nil
	}
	// captured here: the command runs on another goroutine and must not
	// read the model
	gen, send, conn := m.runGen, m.send, m.active
	m.logf(logInfo, "running %s", tag)
	s := sdb.New(m.mgr,
		func(r *model.Result) { send(scriptShowMsg{res: r}) },
		func(msg string) { send(scriptPrintMsg{text: msg}) },
	).WithContext(ctx)
	work := func() tea.Msg {
		err := script.Run(path, s)
		return runDoneMsg{gen: gen, conn: conn, tag: tag, err: err, script: true}
	}
	return tea.Batch(work, tickCmd(gen))
}

// preview is a one-line, length-capped echo of a statement for the log.
func preview(stmt string) string {
	s := strings.Join(strings.Fields(stmt), " ")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:59]) + "…"
	}
	return s
}
