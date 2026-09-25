package workspace

import (
	"strings"

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
	if ev.Err != nil {
		// remembered like a failed run's, so "✦ ask why" carries the error
		// with the statement that caused it
		w.lastStmt = ev.Stmt
		w.failedLocked(ev.Err, ev.Tag, ev.Elapsed, &ev.Notes, &ev.Status)
		return
	}
	// A plan is not a result, so lastStmt stays the last RUN statement —
	// otherwise the assistant would be sent the last result's rows as this
	// statement's. Only an error this same statement left behind is cleared.
	if ev.Stmt == w.lastStmt {
		w.lastErr = ""
	}
	w.plan = ev.Plan
}

// planForChatLocked is the plan text the assistant gets with a question —
// only when the plan is of the statement being asked about, so a question
// about a new query is not answered from an old one's plan.
func (w *Workspace) planForChatLocked(query string) string {
	p := w.plan
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
	if w.planChatFor != p {
		w.planChat, w.planChatFor = p.Text(explain.TextOptions{Width: 110, Insights: true}), p
	}
	return w.planChat
}
