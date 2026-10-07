package workspace

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/rohanthewiz/dbc/ai"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
)

// MaxSchemaTables caps how many tables' columns go with one question. A
// query joining more than this is rare; a question whose words happen to
// name a dozen tables is not asking about all of them.
const MaxSchemaTables = 8

// GridView is how a UI's grid shows the result it holds — presentation, so
// it stays with each UI, but the assistant's data rule needs it: rows go in
// the order the user sees them, and hidden columns stay out.
type GridView struct {
	Result   *model.Result // the result the grid holds
	Hidden   []int         // result columns hidden in the grid, ascending
	SortCol  int           // the result column sorted on; -1 for the result's own order
	SortDesc bool
	Order    []int // display row → result row; read, never kept
}

// ChatContext gathers what the next question may carry — see package ai for
// what actually goes, which is decided by the connection's ai_rows.
//
// Which statement is "this query": the one under the editor's caret, since
// that is what the user is looking at. When it is the statement that last
// ran, its result (or error) comes along; when the editor is empty, the last
// run's statement stands in. A result goes only from the tab on screen, and
// only when that tab holds the statement's result (attachTabLocked) — which
// after a run of several statements may be an earlier statement's tab.
//
// question is the question being (or about to be) asked; its words, like
// the statement's, pick which tables' schema goes along. The Tables come
// back named but without columns — the caller looks those up (they cost a
// catalog query) — and refs is the same tables as the catalog knows them,
// for that lookup.
//
// A result tab the user SHARED (ShareResultTab) changes which result goes:
// the shared one, whatever the caret is on, framed as shared (ai.Context
// Shared) — explicit beats implied. The statement under the caret still
// goes as "this query", with its error when it is the one that just
// failed; the shared result's own statement goes along to say what the
// rows are of, and names tables for the schema lookup too.
//
// views are the UI's grids: the one on screen and, when the shared tab is
// not on screen, that tab's (its parked grid, or the page's snapshot of
// it). Each is matched to a result by GridView.Result, so passing one that
// matches nothing is harmless. A shared result with no matching view goes
// unsorted and with nothing hidden — a UI that kept a view for it must
// pass it, or columns the user hid would reach the model.
//
// It is cheap enough to call every frame, as the TUI's context chip does.
func (w *Workspace) ChatContext(question string, ed Editor, views ...GridView) (ctx ai.Context, refs []db.TableRef) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx = w.chatBaseLocked()
	cur := ""
	if stmts, _ := Pick(ed); len(stmts) > 0 {
		cur = stmts[len(stmts)-1]
	}
	last := w.lastLocked()
	if cur == "" {
		cur = last.lastStmt
	}
	ctx.Query = cur
	ctx.Plan = w.planForChatLocked(cur)
	sh, at := w.activeSetLocked().sharedTab()
	if sh != nil {
		refs = w.mentionedLocked(&ctx, cur+"\n"+sh.stmt, question)
		if cur != "" && cur == last.lastStmt {
			ctx.Err = last.lastErr
		}
		attachShared(&ctx, sh, at, cur, views)
		return ctx, refs
	}
	refs = w.mentionedLocked(&ctx, cur, question)
	switch {
	case cur == "":
	case cur == last.lastStmt:
		w.attachLastRunLocked(&ctx, views, cur)
	default:
		// not the statement that last ran, but its result may be the one
		// on screen all the same: a run of several statements leaves a tab
		// per statement (results.go), and clicking back to one with the
		// caret on its statement is asking about that result. No error
		// goes: the last error is the last statement's.
		w.attachTabLocked(&ctx, views, cur)
	}
	return ctx, refs
}

// attachShared puts a shared result tab's result into ctx — sh at position
// at in its strip, so it is named "result 2" as the UIs number it — with
// whichever of views is that result's grid. query is the SQL in question:
// the shared result's statement goes along only when it differs.
func attachShared(ctx *ai.Context, sh *resultTab, at int, query string, views []GridView) {
	ctx.Shared, ctx.SharedLabel = true, fmt.Sprintf("result %d", at+1)
	if sh.stmt != query {
		ctx.SharedFrom = sh.stmt
	}
	r := sh.res
	if r == nil || r.IsExec {
		return
	}
	ctx.Columns, ctx.Rows, ctx.Truncated = r.Columns, r.Rows, r.Truncated
	applyView(ctx, r, views)
}

// ScriptChatContext is ChatContext for a script tab: the editor holds a Go
// script (name, its file name; source, the editor's text), not SQL.
//
// What differs, and why:
//   - The whole source is "the query" (ctx.Script marks it as Go): a
//     script is one program, and Pick's statement split would cut it at
//     the first semicolon into something neither SQL nor Go.
//   - No plan: plans are of statements the editor explained, and a
//     script tab explains nothing.
//   - Tables are still looked for, in the source and the question — the
//     SQL a script runs is in its string literals, and a table named
//     there is one the model should see the columns of.
//   - The last run's error and results go only when that run was THIS
//     script: its s.Show results are what "why is this row here?" is
//     about, while another script's (or a statement's) would be answered
//     as if this one produced them.
//
// The sdb API summary is not set here: it goes once per conversation,
// which only the assistant (the caller) knows — see ai.Context.ScriptAPI.
func (w *Workspace) ScriptChatContext(question, name, source string, views ...GridView) (ctx ai.Context, refs []db.TableRef) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx = w.chatBaseLocked()
	ctx.Script, ctx.Query = name, source
	// the connections a script can name, the tab's own (if any) first, as
	// a new script's template orders them
	for _, n := range w.cfg.ConnOrder(w.active) {
		cc, _ := w.cfg.ConnByName(n)
		ctx.ScriptConns = append(ctx.ScriptConns, n+" ("+cc.Driver+")")
	}
	// a shared result tab goes as it does from a query tab (ChatContext):
	// in place of this script's own last run, which is then not asked about
	if sh, at := w.activeSetLocked().sharedTab(); sh != nil {
		refs = w.mentionedLocked(&ctx, source+"\n"+sh.stmt, question)
		attachShared(&ctx, sh, at, "", views)
		return ctx, refs
	}
	refs = w.mentionedLocked(&ctx, source, question)
	if name == "" || name != w.lastLocked().lastScript {
		return ctx, refs
	}
	// "": a script's tab holds no statement (resultTab.stmt)
	w.attachLastRunLocked(&ctx, views, "")
	return ctx, refs
}

// lastLocked is the active connection's result set for reading its last
// run — an empty one before anything ran there, so callers need no nil
// check. It is never stored: a run lands in a set made by setLocked.
func (w *Workspace) lastLocked() *resultSet {
	if s := w.activeSetLocked(); s != nil {
		return s
	}
	return &resultSet{cur: -1}
}

// chatBaseLocked is what every question carries whatever it is about: the
// connection, its dialect, and its ai_rows rule.
func (w *Workspace) chatBaseLocked() ai.Context {
	cc, _ := w.cfg.ConnByName(w.active)
	return ai.Context{Conn: w.active, Driver: cc.Driver, SendRows: cc.AIRows, MaxRows: w.cfg.AIContextRows}
}

// mentionedLocked names, in ctx.Tables, the catalog's tables that text or
// question mention (at most MaxSchemaTables), and returns them as refs for
// the caller's column lookup.
func (w *Workspace) mentionedLocked(ctx *ai.Context, text, question string) (refs []db.TableRef) {
	if w.tableIdx == nil {
		return nil
	}
	refs = w.tableIdx.Mentioned(text, question)
	refs = refs[:min(len(refs), MaxSchemaTables)]
	for _, r := range refs {
		ctx.Tables = append(ctx.Tables, ai.Table{Name: w.tableIdx.Display(r), View: r.View})
	}
	return refs
}

// attachLastRunLocked puts the last run's outcome into ctx: its error, or
// else the result on screen as the grid shows it (view), when that is
// stmt's (attachTabLocked). The caller has decided the last run is the one
// the question is about.
func (w *Workspace) attachLastRunLocked(ctx *ai.Context, views []GridView, stmt string) {
	if e := w.lastLocked().lastErr; e != "" {
		ctx.Err = e
		return
	}
	w.attachTabLocked(ctx, views, stmt)
}

// attachTabLocked puts the result on screen into ctx, as the grid shows it
// (view) — only when its tab holds stmt's result. The tab on screen is
// not always the last statement's: a run of several statements gives a
// tab to each one that returned rows and none to a write, so after
// "SELECT …; UPDATE …" the SELECT's rows are on screen while the UPDATE
// is the last statement — and the user can click back to an older tab.
// Rows sent as another statement's would be answered about wrongly.
func (w *Workspace) attachTabLocked(ctx *ai.Context, views []GridView, stmt string) {
	t := w.curLocked()
	if t == nil || t.stmt != stmt {
		return
	}
	if r := t.res; r != nil && !r.IsExec {
		ctx.Columns, ctx.Rows, ctx.Truncated = r.Columns, r.Rows, r.Truncated
		applyView(ctx, r, views)
	}
}

// applyView carries the grid's view of r into ctx. Columns hidden in the
// grid are left out, as copies and exports leave them out — see package
// ai's HIDDEN COLUMNS for why. Only a view of this very result is used
// (GridView.Result == r), so Hidden is indexed by its columns.
//
// A header sort goes too: rows are sent in the grid's order and the model
// is told so. Only the prefix of the order that could be sent is copied —
// this runs every frame for the chip — and it IS copied, because a grid
// re-sorts its order in place and a submit's context waits for its schema
// lookup before it is built into a prompt.
func applyView(ctx *ai.Context, r *model.Result, views []GridView) {
	i := slices.IndexFunc(views, func(v GridView) bool { return v.Result == r })
	if i < 0 {
		return
	}
	view := views[i]
	ctx.Hidden = view.Hidden
	if view.SortCol >= 0 && view.SortCol < len(r.Columns) {
		ctx.Order = slices.Clone(view.Order[:min(len(view.Order), max(ctx.MaxRows, 0))])
		ctx.SortedBy, ctx.SortDesc = r.Columns[view.SortCol], view.SortDesc
	}
}

// SchemaLookupTimeout bounds the catalog query a question's tables cost at
// send time. Past it the question goes without columns rather than waiting
// on a busy database: the model answering a little less precisely beats the
// user watching a spinner for a lookup they never asked for.
const SchemaLookupTimeout = 3 * time.Second

// LookupColumns fetches the columns of the tables a question involves (the
// refs ChatContext returned) on the active connection, under
// SchemaLookupTimeout. It blocks for a catalog round trip, so a UI calls it
// off its event loop; the result goes to AttachColumns.
//
// It runs on the POOL, not the pinned session: the session may be mid-run,
// and a question about a slow query must not wait for that query to end.
func (w *Workspace) LookupColumns(refs []db.TableRef) ([][]db.Column, error) {
	w.mu.Lock()
	conn := w.active
	w.mu.Unlock()
	c, cancel := context.WithTimeout(context.Background(), SchemaLookupTimeout)
	defer cancel()
	return w.mgr.Columns(c, conn, refs)
}

// AttachColumns puts a lookup's columns into the context a question was
// asked with. Only the tables the catalog could describe are kept, so the
// transcript's "sent: schema of …" names exactly the tables whose columns
// went. A failed lookup drops the schema entirely and returns the line the
// transcript shows about it: the question still goes, and the user is told
// why the model may be guessing column names rather than reading dbc's.
//
// Both UIs call this, so the rule for what a failed or partial lookup sends
// cannot differ between the terminal and the browser.
func AttachColumns(ctx ai.Context, cols [][]db.Column, err error) (ai.Context, string) {
	if err != nil {
		ctx.Tables = nil
		return ctx, "schema lookup failed, sending without it: " + err.Error()
	}
	var tables []ai.Table
	for i, t := range ctx.Tables {
		if i < len(cols) && len(cols[i]) > 0 {
			for _, c := range cols[i] {
				t.Columns = append(t.Columns, ai.Column{Name: c.Name, Type: c.Type})
			}
			tables = append(tables, t)
		}
	}
	ctx.Tables = tables
	return ctx, ""
}
