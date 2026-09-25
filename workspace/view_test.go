package workspace

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
)

// pets is a small result with a numeric column, a text column, and a NULL.
func pets() *model.Result {
	return &model.Result{
		Columns: []string{"id", "name", "age"},
		Rows:    [][]string{{"1", "Whiskers", "3"}, {"2", "luna", "NULL"}, {"10", "Bella", "12"}},
		Raw:     [][]any{{int64(1), "Whiskers", int64(3)}, {int64(2), "luna", nil}, {int64(10), "Bella", int64(12)}},
	}
}

func sortedNames(r *model.Result, col int, desc bool) string {
	order := make([]int, len(r.Rows))
	SortRows(order, r, col, desc, export.NumericColumns(r))
	var out []string
	for _, ri := range order {
		out = append(out, r.Rows[ri][1])
	}
	return strings.Join(out, ",")
}

// The rules the TUI's grid and dbc web share: numbers as numbers, text
// case-insensitively, NULLs last both ways, -1 for the result's order.
func TestSortRows(t *testing.T) {
	r := pets()
	for _, tc := range []struct {
		col  int
		desc bool
		want string
	}{
		{-1, false, "Whiskers,luna,Bella"},
		{0, false, "Whiskers,luna,Bella"}, // 10 after 2, not between 1 and 2
		{0, true, "Bella,luna,Whiskers"},
		{1, false, "Bella,luna,Whiskers"},
		{2, false, "Whiskers,Bella,luna"},
		{2, true, "Bella,Whiskers,luna"},
		{9, false, "Whiskers,luna,Bella"}, // out of range: result order
	} {
		if got := sortedNames(r, tc.col, tc.desc); got != tc.want {
			t.Errorf("sort col %d desc %v = %s, want %s", tc.col, tc.desc, got, tc.want)
		}
	}
}

// A projection is a real Result in the given order, Raw carried along.
func TestProject(t *testing.T) {
	p := Project(pets(), []int{2, 1}, []int{2, 1})
	if strings.Join(p.Columns, ",") != "age,name" {
		t.Errorf("columns = %v", p.Columns)
	}
	if p.Rows[0][1] != "Bella" || p.Rows[1][0] != "NULL" || p.Raw[1][0] != nil || p.Raw[0][0] != int64(12) {
		t.Errorf("rows = %v raw = %v", p.Rows, p.Raw)
	}
	if e := Project(pets(), nil, []int{0}); len(e.Rows) != 0 || len(e.Columns) != 1 {
		t.Errorf("no rows = %+v", e)
	}
}

// A multi-statement run reports which statement it is on.
func TestRunProgress(t *testing.T) {
	w := newTestWorkspace(t)
	st, err := w.RunStmts([]string{"SELECT 1", "SELECT 2", "SELECT 3"}, "all 3 statements")
	if err != nil {
		t.Fatal(err)
	}
	if step, steps := w.RunProgress(); step != 0 || steps != 3 {
		t.Errorf("before the job: %d/%d", step, steps)
	}
	w.stepTo(st.Gen, 2)
	if s := w.RunningStatus(); !strings.HasPrefix(s, "all 3 statements · 2/3 ") {
		t.Errorf("status = %q", s)
	}
	w.stepTo(st.Gen-1, 3) // a straggler's step is not this run's
	if step, _ := w.RunProgress(); step != 2 {
		t.Errorf("a stale step moved progress to %d", step)
	}
	st.Job()
	if _, steps := w.RunProgress(); steps != 0 {
		t.Error("idle reports no progress")
	}
	one, _ := w.RunStmts([]string{"SELECT 1"}, "query")
	if s := w.RunningStatus(); strings.Contains(s, "1/1") {
		t.Errorf("one statement shows no count: %q", s)
	}
	one.Job()
}
