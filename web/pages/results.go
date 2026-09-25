package pages

import (
	"fmt"
	"strconv"

	"github.com/rohanthewiz/element"

	"github.com/rohanthewiz/dbc/model"
)

// ResultTable renders a result as a plain HTML table, the first displayCap
// rows of it (0 = all) — the Phase 2 grid. A statement that returns no rows
// (an INSERT, a CREATE) renders as its count of rows affected instead.
//
// Cells are classed rather than styled (the CSP allows no style attribute):
// "num" right-aligns a number, "null" mutes a NULL so it cannot be mistaken
// for the string "NULL". Both are read from Raw, the typed values, because
// Rows has already turned both into text.
func ResultTable(r *model.Result, displayCap int) string {
	b := element.AcquireBuilder()
	defer element.ReleaseBuilder(b)
	if r.IsExec {
		b.DivClass("exec").F("%d rows affected", r.Affected)
		return b.String()
	}
	n := len(r.Rows)
	if displayCap > 0 && displayCap < n {
		n = displayCap
	}
	b.Table("class", "grid").R(
		b.THead().R(
			b.Tr().R(
				b.ThClass("rownum").T("#"),
				element.ForEach(r.Columns, func(c string) { b.Th().T(c) }),
			),
		),
		b.TBody().R(
			b.Wrap(func() {
				for i := 0; i < n; i++ {
					b.Tr().R(
						b.TdClass("rownum").T(strconv.Itoa(i+1)),
						b.Wrap(func() {
							for j, v := range r.Rows[i] {
								b.TdClass(cellClass(r, i, j)).T(v)
							}
						}),
					)
				}
			}),
		),
	)
	if n < len(r.Rows) {
		b.PClass("more").T(fmt.Sprintf("showing the first %d of %d rows (max_display_rows)", n, len(r.Rows)))
	}
	return b.String()
}

func cellClass(r *model.Result, row, col int) string {
	if row >= len(r.Raw) || col >= len(r.Raw[row]) {
		return ""
	}
	switch r.Raw[row][col].(type) {
	case nil:
		return "null"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return "num"
	}
	return ""
}
