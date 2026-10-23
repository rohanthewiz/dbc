package workspace

import (
	"strings"

	"github.com/rohanthewiz/dbc/ai"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/explain"
)

// Explaining. An explain is a RUN like any other: it takes the run slot (one
// at a time), stops on Cancel, and goes through the pinned session — the
// plan worth seeing is the one the next run would get, under the session's
// search_path, SETs and open transaction. What differs is the outcome: a
// plan, which a UI shows beside the last result rather than instead of it.
//
// A plan also arrives the other way: a run whose result is itself a plan
// (an EXPLAIN the user typed) — see landRun.

// ExplainEditor is Ctrl+X (estimate) and Alt+X (analyze): explain the
// statement under the caret, or the selected one.
func (w *Workspace) ExplainEditor(ed Editor, analyze bool) (Start, error) {
	stmts, where := Pick(ed)
	switch {
	case len(stmts) == 0:
		return Start{}, refuse(Nothing, Warn, "nothing to explain — type a query first")
	case len(stmts) > 1:
		return Start{}, refuse(Invalid, Warn,
			"the selection holds %d statements — select one to explain, or put the caret in it", len(stmts))
	}
	return w.Explain(stmts[0], where, analyze)
}

// Explain explains one statement on the active connection. where is how
// the statement was picked ("statement 2/4", "selection"), folded into the
// tag; "" or "query" adds nothing. The Job's event is an *ExplainDone.
func (w *Workspace) Explain(stmt, where string, analyze bool) (Start, error) {
	tag := "explain"
	if analyze {
		tag = "explain analyze"
	}
	if where != "" && where != "query" {
		tag += " (" + where + ")"
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active == "" {
		return Start{}, refuse(NoConnection, Warn, "no active connection — pick one in the sidebar")
	}
	ctx, err := w.beginRunLocked(tag)
	if err != nil {
		return Start{}, err
	}
	conn, gen := w.active, w.runGen
	notes := []Note{notef(Info, "%s on %s — %s", tag, conn, Preview(stmt))}
	if analyze {
		if n, ok := w.analyzeWarning(conn, stmt); ok {
			notes = append(notes, n)
		}
	}
	job := func() Event {
		var p *explain.Plan
		err := w.onSession(ctx, conn, func(s *db.Session) (err error) {
			p, err = s.Explain(ctx, stmt, db.ExplainOptions{Analyze: analyze})
			return err
		})
		ev := &ExplainDone{Tag: tag, Conn: conn, Stmt: stmt, Plan: p, Err: err}
		w.landExplain(ev, gen)
		return ev
	}
	return Start{Tag: tag, Gen: gen, Job: job, Notes: notes}, nil
}

// analyzeWarning says, before it happens, that an analyze runs the
// statement — the one thing about EXPLAIN ANALYZE a user must not learn
// from its side effects.
func (w *Workspace) analyzeWarning(conn, stmt string) (Note, bool) {
	inner, _, _ := explain.Strip(stmt)
	cc, _ := w.cfg.ConnByName(conn)
	if db.IsRead(inner) {
		return Note{Muted, "analyze runs the statement to time it — Ctrl+K stops it"}, true
	}
	if explain.Engine(cc.Driver) == explain.Postgres {
		return Note{Warn, "analyzing a write: it runs inside a transaction dbc rolls back, so no rows change"}, true
	}
	return Note{}, false
}

// landExplain installs an explain's outcome.
func (w *Workspace) landExplain(ev *ExplainDone, gen int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if gen != w.runGen {
		ev.Stale = true
		return
	}
	ev.Elapsed = w.endRunLocked()
	set := w.setLocked(ev.Conn) // the explain's connection's, as a run's
	if ev.Err != nil {
		// remembered like a failed run's, so "✦ ask why" carries the error
		// with the statement that caused it
		set.lastStmt, set.lastScript, set.lastScriptPath = ev.Stmt, "", ""
		w.failedLocked(set, ev.Err, ev.Tag, ev.Elapsed, &ev.Notes, &ev.Status)
		return
	}
	// A plan is not a result, so lastStmt stays the last RUN statement —
	// otherwise the assistant would be sent the last result's rows as this
	// statement's. Only an error this same statement left behind is cleared.
	if ev.Stmt == set.lastStmt {
		set.lastErr = ""
	}
	set.plan, set.planStmt = ev.Plan, ev.Stmt
}

// SharePlan shares the active connection's plan with the assistant, or
// stops sharing it — ShareResultTab's counterpart for the plan pane. By
// default a plan goes with a question only when the statement under the
// caret is the one it explains (planForChatLocked); a shared plan goes
// with every question asked on the connection, framed as shared and with
// its own statement (ai.Context PlanShared, PlanFrom), so moving the caret
// to write the fix — or asking from a script tab — keeps it in view.
//
// Unlike a result, a plan needs no ai_rows: it is the database's
// strategy (table and index names, estimates, timings), not its rows,
// and goes on every connection already. Sharing needs a plan; unsharing
// always works.
func (w *Workspace) SharePlan(share bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.activeSetLocked()
	if !share {
		if s != nil {
			s.planShared = false
		}
		return nil
	}
	if s == nil || s.plan == nil {
		return refuse(Nothing, Warn, "no plan to send — Ctrl+X explains the statement under the caret")
	}
	s.planShared = true
	return nil
}

// PlanShared reports whether the active connection's plan is shared with
// the assistant (SharePlan) — for a UI to mark the plan and offer to stop.
func (w *Workspace) PlanShared() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.activeSetLocked()
	return s != nil && s.planShared && s.plan != nil
}

// planForChatLocked puts the plan the assistant gets with a question into
// ctx. A shared plan (SharePlan) always goes, with its statement when that
// is not query; otherwise the plan goes only when it is of query, the
// statement being asked about, so a question about a new query is not
// answered from an old one's plan.
//
// query is the SQL in question ("" for none). A script or pipeline tab
// passes "", since its source is not a statement: only a shared plan goes
// from there, and always with its statement.
func (w *Workspace) planForChatLocked(ctx *ai.Context, query string) {
	s := w.activeSetLocked()
	if s == nil || s.plan == nil {
		return
	}
	p := s.plan
	// the statement the plan is of: an explain's own, or the EXPLAIN the
	// user typed when the plan was detected in its result
	stmt := p.Statement
	if stmt == "" {
		stmt = s.planStmt
	}
	norm := func(s string) string {
		s, _, _ = explain.Strip(strings.TrimSuffix(strings.TrimSpace(s), ";"))
		return strings.ToLower(strings.Join(strings.Fields(s), " "))
	}
	same := stmt != "" && query != "" && norm(stmt) == norm(query)
	if !same && !s.planShared {
		return
	}
	if w.planChatFor != p {
		w.planChat, w.planChatFor = p.Text(explain.TextOptions{Width: 110, Insights: true}), p
	}
	ctx.Plan, ctx.PlanShared = w.planChat, s.planShared
	if !same {
		ctx.PlanFrom = stmt
	}
}
