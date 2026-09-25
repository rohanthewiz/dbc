package web

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rohanthewiz/rweb"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/userdata"
	"github.com/rohanthewiz/dbc/workspace"
)

// The results grid's server half.
//
// The grid in the page is VIRTUALIZED: it draws only the rows in view and
// fetches them a page at a time, so a 50,000-row result never crosses the
// wire at once. Its VIEW of the result — the sort, which columns are hidden,
// the cursor and range — lives in the page (it is presentation, as the TUI
// keeps it in its grid) and travels with each request that depends on it:
//
//	GET  …/result?from=&n=&sort=&desc=   a page of display rows (sorted here)
//	POST …/copy   {seq, sort, desc, cols, rows, format}  → {text, html, what}
//	GET  …/export?format=&sort=&desc=&cols=&seq=         → a download
//
// Sorting happens HERE, with workspace.SortRows — the TUI's comparison — so a
// copy of "sorted by age desc" holds the same rows in the same order from
// either UI. Hiding is simpler: the page just leaves hidden columns out of
// cols, and workspace.Project takes what is listed.
//
// Every response carries the result's SEQ, a per-tab number that moves when
// the result does. A copy or export names the seq it was made against, and
// one that no longer matches is refused (409): a rerun landing between the
// click and the request must not put a different result on the clipboard.

// gridPage bounds one page of rows. The page asks for a screenful plus
// some; this caps a hand-made request.
const gridPage = 1000

// Auto-sizing mirrors the TUI's grid (tui/grid.go): measure the header and
// the first widthSample values (every value, for a numeric column — see
// workspace.WidestNumeric), and cap a column at maxColWidth characters
// so one long TEXT value cannot push every other column off screen — the
// full value is a double-click away, and a double-click on the header
// border fits past the cap.
const (
	maxColWidth = 40
	widthSample = 500
)

// resultView is a tab's cache of its last result as the grid sees it. The
// order is re-sorted only when the sort changes, not on every page fetch —
// sorting 50,000 rows per scroll step would be the slow part of scrolling.
// The column widths are cached for the same reason: a numeric column is
// measured from every row, which is cheap once and wasteful per page.
type resultView struct {
	res     *model.Result
	seq     int
	numeric []bool
	widths  []int // auto widths, capped (colWidths)
	content []int // content widths, uncapped
	sortCol int
	desc    bool
	order   []int
}

// view returns the tab's current result with its display order for the
// given sort, (re)building the cache when the result or the sort moved. A
// nil view means there is no result.
func (t *tab) view(sortCol int, desc bool) *resultView {
	r := t.ws.LastResult()
	t.viewMu.Lock()
	defer t.viewMu.Unlock()
	if r == nil {
		return nil
	}
	v := &t.rv
	if v.res != r {
		t.rvSeq++
		num := export.NumericColumns(r)
		*v = resultView{res: r, seq: t.rvSeq, numeric: num, sortCol: -2}
		v.widths, v.content = colWidths(r, num)
	}
	if sortCol < -1 || sortCol >= len(r.Columns) {
		sortCol = -1
	}
	if sortCol == -1 {
		desc = false
	}
	if v.order == nil || v.sortCol != sortCol || v.desc != desc {
		// a NEW slice per sort, never a re-sort in place: an order, once
		// published, is immutable, so a request still reading the old one
		// (a page fetch racing a header click) needs no lock and no copy
		order := make([]int, len(r.Rows))
		workspace.SortRows(order, r, sortCol, desc, v.numeric)
		v.order, v.sortCol, v.desc = order, sortCol, desc
	}
	out := *v
	return &out
}

// resultPage is one page of the grid, plus everything the page needs to
// draw the header when it is the first page of a new result.
type resultPage struct {
	Seq      int    `json:"seq"`
	Conn     string `json:"conn"`
	Exec     bool   `json:"exec"`               // a statement that returns no rows
	Affected int64  `json:"affected,omitempty"` // … and how many it changed
	Status   string `json:"status"`             // the status bar's summary

	Columns []string `json:"columns"`
	Numeric []bool   `json:"numeric"` // right-align these
	Widths  []int    `json:"widths"`  // auto widths, in characters, capped
	Content []int    `json:"content"` // content widths, uncapped: "fit"

	Total int  `json:"total"` // display rows: the result's rows, max_display_rows applied
	Rows  int  `json:"rows"`  // the result's rows
	Sort  int  `json:"sort"`  // the result column sorted on, -1 for none
	Desc  bool `json:"desc"`

	From  int         `json:"from"`
	Cells [][]*string `json:"cells"` // display rows from From; nil is SQL NULL
}

// handleResult is a page of the last result's display rows.
func (s *Server) handleResult(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	q := ctx.Request()
	v := t.view(intParam(q.QueryParam("sort"), -1), q.QueryParam("desc") == "1")
	if v == nil {
		return ok(ctx, nil)
	}
	r := v.res
	total := s.shown(r)
	from := min(max(intParam(q.QueryParam("from"), 0), 0), total)
	n := min(max(intParam(q.QueryParam("n"), 200), 0), gridPage)
	to := min(from+n, total)

	pg := resultPage{
		Seq: v.seq, Conn: r.Conn, Exec: r.IsExec, Affected: r.Affected,
		Status:  workspace.ResultStatus(r, s.cfg.MaxRows, total),
		Columns: r.Columns, Numeric: v.numeric,
		Total: total, Rows: len(r.Rows), Sort: v.sortCol, Desc: v.desc,
		From: from, Cells: make([][]*string, 0, to-from),
	}
	pg.Widths, pg.Content = v.widths, v.content
	for d := from; d < to; d++ {
		ri := v.order[d]
		row := make([]*string, len(r.Columns))
		for c := range r.Columns {
			if !isNull(r, ri, c) {
				row[c] = &r.Rows[ri][c]
			}
		}
		pg.Cells = append(pg.Cells, row)
	}
	return ok(ctx, pg)
}

// isNull reads a real NULL from Raw: Rows has already turned it into the
// text "NULL", which a string column could also hold.
func isNull(r *model.Result, row, col int) bool {
	return row < len(r.Raw) && col < len(r.Raw[row]) && r.Raw[row][col] == nil
}

// colWidths measures each column as the TUI's grid does: the header plus
// room for the sort arrow, and the first widthSample values with newlines
// flattened to one character — or every value of a numeric column (numeric
// is export.NumericColumns(r)), since numbers grow down a result. auto is
// capped at maxColWidth, content not.
func colWidths(r *model.Result, numeric []bool) (auto, content []int) {
	auto, content = make([]int, len(r.Columns)), make([]int, len(r.Columns))
	n := min(len(r.Rows), widthSample)
	for c, name := range r.Columns {
		w := utf8.RuneCountInString(name) + 2
		if c < len(numeric) && numeric[c] {
			w = max(w, workspace.WidestNumeric(r, c))
		} else {
			for i := 0; i < n; i++ {
				w = max(w, utf8.RuneCountInString(r.Rows[i][c]))
			}
		}
		content[c], auto[c] = w, min(w, maxColWidth)
	}
	return auto, content
}

// ---------------------------------------------------------------------------
// Copy and export
// ---------------------------------------------------------------------------

// viewReq names a piece of the grid: the result (by seq), the sort that
// orders it, the result columns to take in display order (the visible ones
// for a whole copy, a range's for a range), and the display rows (inclusive;
// nil for every row).
type viewReq struct {
	Seq    int     `json:"seq"`
	Sort   int     `json:"sort"`
	Desc   bool    `json:"desc"`
	Cols   []int   `json:"cols"`
	Rows   *[2]int `json:"rows"`
	Row    bool    `json:"row"`    // the range is the cursor's whole row ("row 3")
	Hidden int     `json:"hidden"` // columns hidden in the grid, for the words
	Format string  `json:"format"` // "plain" (cells, tab-separated) or an export format
}

// slice resolves a viewReq to the Result it names and the words for the
// log — the TUI's words (grid.Selected), so the two logs read alike:
// "the result (8 rows, 1 column hidden)", "name of row 3", "2×3 cells".
func (t *tab) slice(req viewReq) (*model.Result, string, error) {
	v := t.view(req.Sort, req.Desc)
	if v == nil {
		return nil, "", badRequest("nothing to copy yet — run a query first")
	}
	if req.Seq != v.seq {
		return nil, "", &reqError{status: http.StatusConflict,
			msg: "the result changed since this view was drawn — try again"}
	}
	if len(req.Cols) == 0 {
		return nil, "", badRequest("no columns to copy")
	}
	for _, c := range req.Cols {
		if c < 0 || c >= len(v.res.Columns) {
			return nil, "", badRequest("no column %d in this result", c)
		}
	}
	if req.Rows == nil {
		what := fmt.Sprintf("the result (%d rows)", len(v.order))
		if req.Hidden > 0 {
			// said out loud, so a shared table missing a column is never a
			// surprise to the person who hid it last week
			what = fmt.Sprintf("the result (%d rows, %s hidden)", len(v.order), plural(req.Hidden, "column"))
		}
		return workspace.Project(v.res, v.order, req.Cols), what, nil
	}
	r0, r1 := req.Rows[0], req.Rows[1]
	if r0 < 0 || r1 < r0 || r1 >= len(v.order) {
		return nil, "", badRequest("rows %d–%d are not in this result", r0, r1)
	}
	out := workspace.Project(v.res, v.order[r0:r1+1], req.Cols)
	rows, cols := r1-r0+1, len(req.Cols)
	switch {
	case req.Row:
		return out, fmt.Sprintf("row %d", r0+1), nil
	case rows == 1 && cols == 1:
		return out, fmt.Sprintf("%s of row %d", v.res.Columns[req.Cols[0]], r0+1), nil
	}
	return out, fmt.Sprintf("%d×%d cells", rows, cols), nil
}

// formatNames are how a copy's format reads in the log — the TUI's words.
var formatNames = map[export.Format]string{
	export.HTML: "a table", export.Markdown: "Markdown", export.CSV: "CSV",
	export.TSV: "TSV", export.JSON: "JSON", export.Text: "an aligned text table",
}

// copyOut is what the page puts on the clipboard: HTML carries a rich
// flavor as well, which the Clipboard API writes as text/html — a real
// table in Teams, Outlook or a doc, even over SSH, where the TUI can only
// send text.
type copyOut struct {
	Text string `json:"text"`
	HTML string `json:"html,omitempty"`
	What string `json:"what"`
}

// handleCopy renders a piece of the grid for the clipboard. The page does
// the write — the browser owns the clipboard — and logs how it landed.
func (s *Server) handleCopy(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req viewReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	r, what, err := t.slice(req)
	if err != nil {
		return fail(ctx, err)
	}
	if req.Format == "" || req.Format == "plain" {
		return ok(ctx, copyOut{Text: export.PlainCells(r), What: what})
	}
	f, err := export.ParseFormat(req.Format)
	if err != nil {
		return fail(ctx, badRequest("%s", serr.StringFromErr(err)))
	}
	c, err := export.ClipContent(r, f)
	if err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, copyOut{Text: c.Text, HTML: c.HTML, What: what + " as " + formatNames[f]})
}

// exportTypes are a download's extension and media type, by format.
var exportTypes = map[export.Format][2]string{
	export.CSV:      {"csv", "text/csv; charset=utf-8"},
	export.TSV:      {"tsv", "text/tab-separated-values; charset=utf-8"},
	export.Markdown: {"md", "text/markdown; charset=utf-8"},
	export.HTML:     {"html", "text/html; charset=utf-8"},
	export.JSON:     {"json", "application/json; charset=utf-8"},
	export.Text:     {"txt", "text/plain; charset=utf-8"},
}

// handleExport is ⤓ Export: the whole result, in display order with hidden
// columns left out — the same view a whole-result copy takes, as the TUI's
// export dialog does — as a file download. A GET, so the page can hand it to
// a plain link and let the browser save it.
func (s *Server) handleExport(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	q := ctx.Request()
	f, err := export.ParseFormat(q.QueryParam("format"))
	if err != nil {
		return fail(ctx, badRequest("%s", serr.StringFromErr(err)))
	}
	req := viewReq{
		Seq: intParam(q.QueryParam("seq"), 0), Sort: intParam(q.QueryParam("sort"), -1),
		Desc: q.QueryParam("desc") == "1", Cols: intList(q.QueryParam("cols")),
	}
	r, _, err := t.slice(req)
	if err != nil {
		return fail(ctx, err)
	}
	out, err := export.Render(r, f)
	if err != nil {
		return fail(ctx, err)
	}
	typ := exportTypes[f]
	name := fmt.Sprintf("%s-%s.%s", fileSafe(r.Conn), time.Now().Format("20060102-150405"), typ[0])
	h := ctx.Response()
	h.SetHeader("Content-Type", typ[1])
	h.SetHeader("Content-Disposition", `attachment; filename="`+name+`"`)
	h.SetHeader("Cache-Control", "no-store")
	return ctx.WriteString(out)
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// fileSafe makes a connection name usable in a download's file name.
func fileSafe(s string) string {
	s = strings.Trim(unsafeFileChars.ReplaceAllString(s, "-"), "-.")
	if s == "" {
		return "result"
	}
	return s
}

// ---------------------------------------------------------------------------
// History, table preview, and the statement under the caret
// ---------------------------------------------------------------------------

// historyMax bounds one history listing; the file keeps 500.
const historyMax = 200

// handleHistory is Ctrl+P's list: newest first, filtered by q the way the
// TUI's history filters (the SQL or the connection name holds it). The
// history is shared — the TUI's file — so a query run in either is here.
func (s *Server) handleHistory(ctx rweb.Context) error {
	es := userdata.MatchHistory(s.opt.History.Recent(), ctx.Request().QueryParam("q"))
	if len(es) > historyMax {
		es = es[:historyMax]
	}
	if es == nil {
		es = []userdata.Entry{}
	}
	return ok(ctx, es)
}

type previewReq struct {
	Name string `json:"name"`
}

// handlePreview is a double-click on a table: its first 100 rows, run the
// TUI's way (tui/sidebar.go) — recorded in the history, since it is what the
// user would have typed, but not put in the editor, whose text is theirs.
// The name must be one the sidebar listed: this is not a way to run SQL.
func (s *Server) handlePreview(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req previewReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if !slices.ContainsFunc(tables(t.ws), func(r tabRef) bool { return r.QName == req.Name }) {
		return fail(ctx, badRequest("no table %q on %s", req.Name, t.ws.Active()))
	}
	stmt := fmt.Sprintf("SELECT * FROM %s LIMIT 100", req.Name)
	st, err := t.ws.RunStmts([]string{stmt}, "preview "+req.Name)
	if err != nil {
		t.notes(st.Notes)
		if r, isRefusal := asRefusal(err); isRefusal {
			t.notes([]workspace.Note{r.Note})
		}
		return fail(ctx, err)
	}
	s.launch(t, st)
	return ok(ctx, map[string]any{"tag": st.Tag})
}

type stmtReq struct {
	Buffer string `json:"buffer"`
	Caret  int    `json:"caret"`
}

// handleStmt is the statement under the caret, for the editor's marker —
// in UTF-16 units, as the page counts. It asks the same splitter a run
// uses, so the marked statement is the one Ctrl+Enter runs: a JavaScript
// copy of the splitter would sooner or later disagree about a quote or a
// dollar-quoted body. {0, 0} when the buffer holds fewer than two
// statements, since marking the only one says nothing.
func (s *Server) handleStmt(ctx rweb.Context) error {
	var req stmtReq
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Buffer) > maxBuffer {
		return ok(ctx, [2]int{})
	}
	r := workspace.StmtRange(req.Buffer, byteOffset(req.Buffer, req.Caret))
	return ok(ctx, [2]int{utf16Len(req.Buffer[:r[0]]), utf16Len(req.Buffer[:r[1]])})
}

// utf16Len is the length of s in UTF-16 code units — byteOffset's inverse.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x10000 {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Small parsers
// ---------------------------------------------------------------------------

// intParam parses a query parameter, def when it is missing or not a number.
func intParam(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// intList parses "0,2,5"; anything that is not a number is dropped (and a
// column list that ends up empty is refused by slice).
func intList(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
