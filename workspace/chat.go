package workspace

import (
	"context"
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
// run's statement stands in.
//
// question is the question being (or about to be) asked; its words, like
// the statement's, pick which tables' schema goes along. The Tables come
// back named but without columns — the caller looks those up (they cost a
// catalog query) — and refs is the same tables as the catalog knows them,
// for that lookup.
//
// It is cheap enough to call every frame, as the TUI's context chip does.
func (w *Workspace) ChatContext(question string, ed Editor, view GridView) (ctx ai.Context, refs []db.TableRef) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cc, _ := w.cfg.ConnByName(w.active)
	ctx = ai.Context{Conn: w.active, Driver: cc.Driver, SendRows: cc.AIRows, MaxRows: w.cfg.AIContextRows}
	cur := ""
	if stmts, _ := Pick(ed); len(stmts) > 0 {
		cur = stmts[len(stmts)-1]
	}
	if cur == "" {
		cur = w.lastStmt
	}
	ctx.Query = cur
	ctx.Plan = w.planForChatLocked(cur)
	if w.tableIdx != nil {
		refs = w.tableIdx.Mentioned(cur, question)
		refs = refs[:min(len(refs), MaxSchemaTables)]
		for _, r := range refs {
			ctx.Tables = append(ctx.Tables, ai.Table{Name: w.tableIdx.Display(r), View: r.View})
		}
	}
	if cur == "" || cur != w.lastStmt {
		return ctx, refs
	}
	if w.lastErr != "" {
		ctx.Err = w.lastErr
		return ctx, refs
	}
	if r := w.lastRes; r != nil && !r.IsExec {
		ctx.Columns, ctx.Rows, ctx.Truncated = r.Columns, r.Rows, r.Truncated
		// Columns hidden in the grid are left out, as copies and exports
		// leave them out — see package ai's HIDDEN COLUMNS for why. The
		// guard makes sure the view describes this very result, so Hidden
		// is indexed by its columns.
		//
		// A header sort goes too: rows are sent in the grid's order and the
		// model is told so. Only the prefix of the order that could be sent
		// is copied — this runs every frame for the chip — and it IS copied,
		// because a grid re-sorts its order in place and a submit's context
		// waits for its schema lookup before it is built into a prompt.
		if view.Result == r {
			ctx.Hidden = view.Hidden
			if view.SortCol >= 0 && view.SortCol < len(r.Columns) {
				ctx.Order = slices.Clone(view.Order[:min(len(view.Order), max(ctx.MaxRows, 0))])
				ctx.SortedBy, ctx.SortDesc = r.Columns[view.SortCol], view.SortDesc
			}
		}
	}
	return ctx, refs
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
