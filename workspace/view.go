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
//
// COST (N-282). Each row's key — its lower-cased text, or its number as a
// float64 — is computed once, before the sort, not twice per comparison:
// the comparison used to lower-case both texts (an allocation each, for any
// text with a capital) and reflect on both numbers, about 40 times a row.
// At 1,000,000 rows of mixed-case text that was 6.1 s and 53M allocations
// on the TUI's event loop; with the keys made first it is a few hundred
// milliseconds. The sort is then pdqsort (slices.SortFunc), not a stable
// merge sort, with the row's own index as the last tie-break: a total order
// has one sorted arrangement, so the result is the stable sort's exactly,
// for less work.
func SortRows(order []int, r *model.Result, col int, desc bool, numeric []bool) {
	for i := range order {
		order[i] = i
	}
	if r == nil || col < 0 || col >= len(r.Columns) {
		return
	}
	n := len(order)
	// null[ri]: the row has no value here (SQL NULL, or a row too short
	// to reach col), and sorts last both ways
	null := make([]bool, n)
	for ri := range n {
		null[ri] = ri >= len(r.Raw) || col >= len(r.Raw[ri]) || r.Raw[ri][col] == nil
	}
	// nulls orders a pair when either is NULL (ok): NULLs last, two NULLs
	// in the result's order
	nulls := func(a, b int) (c int, ok bool) {
		switch na, nb := null[a], null[b]; {
		case na && nb:
			return a - b, true
		case na:
			return 1, true
		case nb:
			return -1, true
		}
		return 0, false
	}
	// keyed turns c, a's key against b's ascending, into the pair's order:
	// reversed for desc, and a tie broken by the result's order
	keyed := func(a, b, c int) int {
		if desc {
			c = -c
		}
		if c == 0 {
			return a - b
		}
		return c
	}
	if col < len(numeric) && numeric[col] {
		keys := make([]float64, n)
		for ri := range n {
			if !null[ri] {
				keys[ri] = toFloat(r.Raw[ri][col])
			}
		}
		slices.SortFunc(order, func(a, b int) int {
			if c, ok := nulls(a, b); ok {
				return c
			}
			return keyed(a, b, cmp.Compare(keys[a], keys[b]))
		})
		return
	}
	keys := make([]string, n)
	for ri := range n {
		if !null[ri] && ri < len(r.Rows) && col < len(r.Rows[ri]) {
			keys[ri] = strings.ToLower(r.Rows[ri][col])
		}
	}
	slices.SortFunc(order, func(a, b int) int {
		if c, ok := nulls(a, b); ok {
			return c
		}
		return keyed(a, b, strings.Compare(keys[a], keys[b]))
	})
}

// toFloat converts a Go number to float64 for comparison. Only called on
// columns export.NumericColumns vouched for. The types the drivers hand
// back are switched on first; reflect is for the rest.
func toFloat(v any) float64 {
	switch t := v.(type) {
	case int64:
		return float64(t)
	case float64:
		return t
	case int32:
		return float64(t)
	case float32:
		return float64(t)
	case int:
		return float64(t)
	}
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
