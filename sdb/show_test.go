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

// TestShowEverySet is N-317: a statement that returned several result sets
// (a MySQL CALL whose procedure runs several SELECTs) has the first in the
// Result and the rest in More, and s.Show(r) used to show the first alone.
// Each set now reaches the host as a result of its own, in order — r first,
// itself when it is even, so a host still finds MoreCut on it — and each is
// evened on its own. A set taken from More, which has none of its own, is
// shown alone.
func TestShowEverySet(t *testing.T) {
	var got []*model.Result
	s := New(nil, func(r *model.Result) { got = append(got, r) }, nil)
	two := &model.Result{Columns: []string{"b", "c"}, Rows: [][]string{{"1"}}}
	three := &model.Result{Columns: []string{"d"}, Rows: [][]string{{"x"}}}
	r := &model.Result{
		Columns: []string{"a"}, Rows: [][]string{{"1"}},
		More: []*model.Result{two, three}, MoreCut: true,
	}
	s.Show(r)
	if len(got) != 3 {
		t.Fatalf("Show handed on %d results, want 3", len(got))
	}
	if got[0] != r || !got[0].MoreCut {
		t.Error("the first set is not the script's own result, More and MoreCut and all")
	}
	if got[1] == two || len(got[1].Rows[0]) != 2 || got[1].Columns[1] != "c" {
		t.Errorf("the second set was not evened on its own: %+v", got[1])
	}
	if got[2] != three {
		t.Errorf("the third set: %+v", got[2])
	}

	got = nil
	s.Show(r.More[1])
	if len(got) != 1 || got[0] != three {
		t.Errorf("a set from More shown alone gave %d results", len(got))
	}
	// the copy Show's doc gives for the first set alone
	got = nil
	one := *r
	one.More = nil
	s.Show(&one)
	if len(got) != 1 || got[0].Columns[0] != "a" {
		t.Errorf("the first set alone gave %d results", len(got))
	}
}
