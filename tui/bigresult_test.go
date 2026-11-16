package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/workspace"
)

// N-282: with max_rows raised, a sort, a copy and an export of the whole
// result used to run inside Update and freeze the screen. Now a sort past
// asyncSortRows, and every copy's and export's projection and rendering,
// run as commands.

// bigNames is n rows of (id, name), the names a scramble with mixed case
// so a sort by name has work to do.
func bigNames(n int) *model.Result {
	r := &model.Result{Columns: []string{"id", "name"}}
	for i := range n {
		k := (i*7919 + 13) % n
		name := "Name " + strconv.Itoa(k)
		if k%2 == 0 {
			name = strings.ToLower(name)
		}
		r.Rows = append(r.Rows, []string{strconv.Itoa(i), name})
		r.Raw = append(r.Raw, []any{int64(i), name})
	}
	return r
}

// A sort of a result past asyncSortRows does not happen in the click: the
// rows stay, the grid says it is sorting, and a command holds the work.
// Its answer lands only while it is still the sort wanted.
func TestBigSortRunsOffTheLoop(t *testing.T) {
	r := bigNames(asyncSortRows + 1)
	g := newGrid()
	g.SetResult(r, 0)

	g.Sort(1)
	if !g.sorting || g.sortCmd == nil {
		t.Fatalf("sorting %v, command %v: the sort ran in the click", g.sorting, g.sortCmd != nil)
	}
	if g.order[0] != 0 || g.order[len(g.order)-1] != len(g.order)-1 {
		t.Fatal("the rows moved before the sort's answer came")
	}
	msg := g.takeSort()().(sortDoneMsg)
	g.sorted(msg)
	want := make([]int, len(r.Rows))
	workspace.SortRows(want, r, 1, false, export.NumericColumns(r))
	if g.sorting || !slices.Equal(g.order, want) {
		t.Fatalf("after the answer: sorting %v, order the sort's %v", g.sorting, slices.Equal(g.order, want))
	}

	// asked again before the answer came: the old answer is dropped
	g.Sort(1) // desc, off the loop
	stale := g.takeSort()
	g.Sort(1) // back to the result's order, at once
	g.sorted(stale().(sortDoneMsg))
	if g.sorting || g.order[0] != 0 || g.order[1] != 1 {
		t.Errorf("a superseded sort's answer landed: first rows %v", g.order[:2])
	}

	// a new result before the answer: dropped too
	g.Sort(1)
	stale = g.takeSort()
	g.SetResult(bigNames(asyncSortRows+1), 0)
	g.sorted(stale().(sortDoneMsg))
	if g.sorting || g.order[0] != 0 {
		t.Errorf("an answer for the old result landed on the new one")
	}

	// a small result sorts in the click, as it always did
	g.SetResult(bigNames(100), 0)
	g.Sort(1)
	if g.sorting || g.sortCmd != nil {
		t.Errorf("a 100-row sort went off the loop")
	}
}

// Through the program: a header click on a big result returns the sort as
// Update's command, the strip says "sorting by …" until it lands, and then
// "sorted by …" over the sorted rows.
func TestBigSortThroughUpdate(t *testing.T) {
	m := newTestModelCfg(t, func(c *config.Config) { c.MaxRows = 100_000 })
	n := asyncSortRows + 10
	runSQL(t, m, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < `+
		strconv.Itoa(n)+`) SELECT i AS id, CASE WHEN i % 2 = 0 THEN 'b' || i ELSE 'A' || i END AS label FROM n`)
	if m.grid.res == nil || len(m.grid.res.Rows) != n {
		t.Fatalf("the result is not the %d rows asked for", n)
	}
	frame(m)
	x, _ := findText(t, frame(m), "label")
	_, cmd := m.Update(tea.MouseClickMsg{X: x + 1, Y: m.grid.head.Y, Button: tea.MouseLeft})
	m.render()
	if cmd == nil || !m.grid.sorting {
		t.Fatalf("the click sorted in Update (command %v, sorting %v)", cmd != nil, m.grid.sorting)
	}
	findText(t, frame(m), "sorting by label asc…")
	drive(t, m, nil, cmd)
	if m.grid.sorting {
		t.Fatal("the sort's answer did not land")
	}
	findText(t, frame(m), "sorted by label asc")
	// 'A1' < 'A10' < … < 'b10' …, case-insensitively: A-names first
	if v, _, _ := m.grid.value(0, 1); v != "A1" {
		t.Errorf("first row after the sort: %q, want A1", v)
	}
}

// An export to a file writes through a command, and the file is whole;
// a path that cannot be written keeps the dialog open, as it did.
func TestExportRunsAsACommand(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	key(t, m, "ctrl+e")
	md, ok := m.modal.(*exportModal)
	if !ok {
		t.Fatalf("modal = %T", m.modal)
	}

	bad := filepath.Join(t.TempDir(), "no", "such", "dir.csv")
	md.path.SetText(bad)
	cmd, closed := md.run(m, false)
	if cmd != nil || closed || !strings.Contains(logText(m), "export failed") {
		t.Errorf("an unwritable path: command %v, closed %v; log %s", cmd != nil, closed, logText(m))
	}

	path := filepath.Join(t.TempDir(), "out.csv")
	md.path.SetText(path)
	cmd, closed = md.run(m, false)
	if cmd == nil || !closed {
		t.Fatalf("command %v, closed %v", cmd != nil, closed)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the file was written in Update, before its command ran")
	}
	drive(t, m, nil, cmd)
	b, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(b), "id,name,breed,age,adopted\n") {
		t.Fatalf("file %q, %v", b, err)
	}
	if !strings.Contains(logText(m), "exported the result") {
		t.Errorf("log: %s", logText(m))
	}
}
