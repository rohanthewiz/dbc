package workspace

import (
	"cmp"
	"reflect"
	"slices"
	"strings"

	"github.com/rohanthewiz/dbc/model"
)

// A grid's VIEW of a result — its sort order and which columns it shows —
// is presentation, so each UI keeps its own (the TUI in its grid, the web
// page in the browser, sent along with every request that needs it). What
// is shared is the arithmetic, so that "sorted by age desc, name hidden"
// puts the same rows in the same order in a copy from either UI:
//
//	result rows ──SortRows──► order (display row → result row)
//	                              │
//	result cols ──(hidden out)──► cols (display col → result col)
//	                              ▼
//	                Project(r, order[r0:r1+1], cols[c0:c1+1]) ─► a Result
//	                in display order, Raw carried along, for copy and export

// SortRows fills order with the display order of r's rows sorted by result
// column col (ascending, or descending with desc). col < 0 means the
// result's own order. numeric says which columns compare as numbers — pass
// export.NumericColumns(r); it is a parameter because a UI computes it once
// per result, and this may run on every header click.
//
// NULLs sort LAST in both directions: "biggest first" should not open on a
// screen of nothing. A numeric column compares numbers, so 10 sorts after
// 9; anything else compares its text case-insensitively. The sort is
// stable, so rows that tie keep the result's order.
func SortRows(order []int, r *model.Result, col int, desc bool, numeric []bool) {
	for i := range order {
		order[i] = i
	}
	if r == nil || col < 0 || col >= len(r.Columns) {
		return
	}
	num := col < len(numeric) && numeric[col]
	raw := func(ri int) any {
		if ri < len(r.Raw) && col < len(r.Raw[ri]) {
			return r.Raw[ri][col]
		}
		return nil
	}
	slices.SortStableFunc(order, func(a, b int) int {
		ra, rb := raw(a), raw(b)
		switch {
		case ra == nil && rb == nil:
			return 0
		case ra == nil:
			return 1
		case rb == nil:
			return -1
		}
		var c int
		if num {
			c = cmp.Compare(toFloat(ra), toFloat(rb))
		} else {
			c = cmp.Compare(strings.ToLower(r.Rows[a][col]), strings.ToLower(r.Rows[b][col]))
		}
		if desc {
			c = -c
		}
		return c
	})
}

// toFloat converts a Go number to float64 for comparison. Only called on
// columns export.NumericColumns vouched for.
func toFloat(v any) float64 {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	}
	return 0
}

// Project builds a Result holding the given result rows (in the order
// given — a slice of a display order) and result columns (likewise), with
// Raw carried along — so a copied selection exports exactly like a whole
// result: NULLs stay NULLs, numbers stay right-aligned in HTML.
//
// Only the listed columns are taken, so a column the grid hides is left out
// by not listing it: a copy takes what the user sees. That is what makes
// hiding useful for sharing — hide the noisy columns, then copy the table
// for Teams.
func Project(src *model.Result, rows, cols []int) *model.Result {
	out := &model.Result{Conn: src.Conn, Query: src.Query, Duration: src.Duration}
	for _, rc := range cols {
		out.Columns = append(out.Columns, src.Columns[rc])
	}
	for _, ri := range rows {
		if ri < 0 || ri >= len(src.Rows) {
			continue
		}
		vals := make([]string, len(cols))
		for i, rc := range cols {
			vals[i] = src.Rows[ri][rc]
		}
		out.Rows = append(out.Rows, vals)
		if ri < len(src.Raw) {
			raws := make([]any, len(cols))
			for i, rc := range cols {
				if rc < len(src.Raw[ri]) {
					raws[i] = src.Raw[ri][rc]
				}
			}
			out.Raw = append(out.Raw, raws)
		}
	}
	return out
}

// WidestNumeric is the widest display value of numeric column c over EVERY
// row of r — for a column export.NumericColumns vouched for.
//
// Auto-sizing measures only a grid's first few hundred rows (a text column's
// width costs a grapheme walk per value, and a huge result should not pay
// that on arrival), which is fine for text, whose width varies without
// pattern, and wrong for numbers, which grow: an id column sized by rows
// 1–500 shows "10…" at row 1,000. A number's display text is plain ASCII,
// so its width is len() — no decoding — and scanning every row is a length
// read per cell. NULLs count as their "NULL" text, which is how both grids
// draw them.
//
// The widest text is measured rather than derived from the column's
// min/max: a float's text is not monotonic in its value (0.123456789 is
// wider than 1000), and a length scan is as cheap as a comparison.
func WidestNumeric(r *model.Result, c int) int {
	w := 0
	for _, row := range r.Rows {
		if c < len(row) {
			w = max(w, len(row[c]))
		}
	}
	return w
}
