package sdb

import (
	"testing"

	"github.com/rohanthewiz/dbc/model"
)

// TestShowEvensRows is N-258: a result a script builds by hand may have a
// row shorter (or longer) than its columns, and every consumer indexes a
// row by column — the TUI's grid panicked on one, on its event loop. Show
// hands on a copy with every row as wide as the columns; the script's own
// result is left as it was.
func TestShowEvensRows(t *testing.T) {
	var got *model.Result
	s := New(nil, func(r *model.Result) { got = r }, nil)
	r := &model.Result{
		Columns: []string{"a", "b", "c"},
		Rows:    [][]string{{"1"}, {"1", "2", "3", "4"}, {"x", "y", "z"}},
		Raw:     [][]any{{int64(1)}},
	}
	s.Show(r)
	if got == nil || got == r {
		t.Fatalf("Show handed on %p (the script's own result is %p)", got, r)
	}
	for i, row := range got.Rows {
		if len(row) != 3 {
			t.Errorf("row %d has %d values", i, len(row))
		}
	}
	if got.Rows[0][0] != "1" || got.Rows[0][2] != "" || got.Rows[1][2] != "3" {
		t.Errorf("rows: %q", got.Rows)
	}
	if len(got.Raw[0]) != 3 || got.Raw[0][0] != int64(1) || got.Raw[0][1] != nil {
		t.Errorf("raw: %v", got.Raw)
	}
	if len(r.Rows[0]) != 1 || len(r.Rows[1]) != 4 {
		t.Error("Show changed the script's result")
	}
	// an even result goes through as it is
	even := &model.Result{Columns: []string{"a"}, Rows: [][]string{{"1"}}}
	s.Show(even)
	if got != even {
		t.Error("an even result was copied")
	}
}
