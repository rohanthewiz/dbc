package export

import (
	"fmt"
	"strings"

	"github.com/rohanthewiz/dbc/model"
)

// A result turned on its side — psql's \x, a "record view".
//
// A wide result read one row at a time is easier vertical: each column a
// line, its name on the left, so a 40-column row fits a chat message and a
// reader never scrolls sideways to match a value with its header.
//
//	  id │ name │ email            column │ row 1 │ row 2
//	  ───┼──────┼──────     ⇒      ───────┼───────┼──────
//	   1 │ ann  │ a@x               id    │ 1     │ 2
//	   2 │ bob  │ b@x               name  │ ann   │ bob
//	                                email │ a@x   │ b@x
//
// The transposed result is an ordinary *model.Result, so every format
// (CSV, Markdown, JSON, the HTML page and the paste-ready fragment) renders
// it unchanged; only the two HTML renderings look at Transposed, to draw
// column 0 as row headers.

// TransposeNameCol heads the column of names in a transposed result.
const TransposeNameCol = "column"

// Transpose returns r with rows and columns swapped. Row i of the result is
// r's column i: its name, then that column's value in each of r's rows.
// Those rows become columns headed "row N", N counting from first — the
// display row the first of them was, so a transposed range copied from the
// middle of a grid keeps the numbers the grid showed. A single row is
// headed "value" instead: the common case is one record, and "column |
// value" reads better pasted into a message than "column | row 17".
//
// Raw is transposed with Rows, so a real NULL stays NULL (drawn as such by
// the HTML renderings) and JSON keeps each value's type.
func Transpose(r *model.Result, first int) *model.Result {
	out := &model.Result{
		Conn: r.Conn, Query: r.Query, Duration: r.Duration, Truncated: r.Truncated,
		Transposed: true,
	}
	out.Columns = make([]string, 0, len(r.Rows)+1)
	out.Columns = append(out.Columns, TransposeNameCol)
	if len(r.Rows) == 1 {
		out.Columns = append(out.Columns, "value")
	} else {
		for i := range r.Rows {
			out.Columns = append(out.Columns, fmt.Sprintf("row %d", first+i))
		}
	}

	out.Rows = make([][]string, len(r.Columns))
	out.Raw = make([][]any, len(r.Columns))
	for c, name := range r.Columns {
		row := make([]string, 0, len(r.Rows)+1)
		raw := make([]any, 0, len(r.Rows)+1)
		row = append(row, name)
		raw = append(raw, name)
		for ri := range r.Rows {
			v := ""
			if c < len(r.Rows[ri]) {
				v = r.Rows[ri][c]
			}
			row = append(row, v)
			// a short Raw row (a hand-built result) reads as a value, not a
			// NULL: absent evidence of NULL, the display string stands
			var rv any = v
			if ri < len(r.Raw) && c < len(r.Raw[ri]) {
				rv = r.Raw[ri][c]
			}
			raw = append(raw, rv)
		}
		out.Rows[c], out.Raw[c] = row, raw
	}
	return out
}

// PlainCellsTransposed is PlainCells for a grid on its side: one line per
// column of r, its values tab-separated across r's rows, and no names — the
// plain copy carries values only, in the orientation the grid shows them.
// One value alone is copied as it is, as PlainCells does.
func PlainCellsTransposed(r *model.Result) string {
	if len(r.Rows) == 1 && len(r.Columns) == 1 {
		return r.Rows[0][0]
	}
	lines := make([]string, len(r.Columns))
	vals := make([]string, len(r.Rows))
	for c := range r.Columns {
		for ri, row := range r.Rows {
			vals[ri] = ""
			if c < len(row) {
				vals[ri] = row[c]
			}
		}
		lines[c] = strings.Join(vals, "\t")
	}
	return strings.Join(lines, "\n")
}
