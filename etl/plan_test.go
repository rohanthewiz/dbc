package etl

import (
	"context"
	"strings"
	"testing"
	"time"
)

// PlanCopy says what Copy would do and writes nothing (N-297): the source
// query and its rows, the CREATE for a missing table with Create, the
// truncate, and the problem when the table is missing without Create.
func TestPlanCopy(t *testing.T) {
	ctx := context.Background()
	src, dst := fileConn(t, SQLite, "src.db"), fileConn(t, SQLite, "dst.db")
	mustExec(t, src, "CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT)")
	mustExec(t, src, "INSERT INTO cats VALUES (1, 'a'), (2, 'b'), (3, 'c')")
	mustExec(t, dst, "CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT)")
	mustExec(t, dst, "INSERT INTO cats VALUES (9, 'kept')")

	p, err := PlanCopy(ctx, src, "cats", dst, CopyOptions{Where: "id > 1", Truncate: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Rows != 2 || !p.DestThere || p.Truncate != `DELETE FROM "cats"` || p.Problem != "" || p.Direct ||
		strings.Join(p.Columns, ",") != "id,name" {
		t.Errorf("plan = %+v", p)
	}
	if got := dump(t, dst, "SELECT count(*) FROM cats")[0][0]; got != "1" {
		t.Errorf("the dry run changed the destination: %s rows", got)
	}

	p, err = PlanCopy(ctx, src, "cats", dst, CopyOptions{To: "fresh", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.DestThere || !strings.HasPrefix(p.Create, `CREATE TABLE IF NOT EXISTS "fresh"`) || p.Truncate != "" {
		t.Errorf("create plan = %+v", p)
	}
	if _, err := dst.DB.Exec(`SELECT 1 FROM "fresh"`); err == nil {
		t.Error("the dry run created the table")
	}

	p, err = PlanCopy(ctx, src, "cats", dst, CopyOptions{To: "fresh"})
	if err != nil || !strings.Contains(p.Problem, "does not exist") {
		t.Errorf("missing without Create: %+v %v", p, err)
	}
	if !strings.Contains(p.String(), "problem:") || !strings.Contains(p.String(), "nothing was written") {
		t.Errorf("String:\n%s", p.String())
	}

	// a Query that writes, which `dbc copy --query` can now pass (N-336): the
	// plan runs nothing of it — not a count, and off Postgres not a describe,
	// which as a subquery is a syntax error there — and says so
	move := CopyOptions{Query: "DELETE FROM cats WHERE id = 3 RETURNING *", To: "moved", Create: true}
	if p, err = PlanCopy(ctx, src, "", dst, move); err != nil {
		t.Fatalf("a write's plan: %v", err)
	}
	if !p.Writes || p.Rows != -1 || p.ColumnsNote == "" || len(p.Columns) != 0 || p.Create != "" || p.Problem != "" {
		t.Errorf("a write's plan = %+v", p)
	}
	for _, want := range []string{"moves:", "only running it shows", "counting its rows would run it",
		"would be created from the columns the query returns"} {
		if !strings.Contains(p.String(), want) {
			t.Errorf("String has no %q:\n%s", want, p.String())
		}
	}
	if got := dump(t, src, "SELECT count(*) FROM cats")[0][0]; got != "3" {
		t.Errorf("the dry run ran the write: %s rows left", got)
	}
	move.Create = false
	if p, err = PlanCopy(ctx, src, "", dst, move); err != nil || !strings.Contains(p.Problem, "does not exist") {
		t.Errorf("a write into a missing table without Create: %+v %v", p, err)
	}
}

// TestCountCopy: the copy's source rows, counted as the dry run counts
// them, for `dbc copy`'s progress percentage (N-336). A table with a
// Where, a Query, and a Query that writes, which is never counted, as
// counting it would run it.
func TestCountCopy(t *testing.T) {
	ctx := context.Background()
	for _, e := range []Engine{SQLite, Bytdb} {
		t.Run(e.String(), func(t *testing.T) {
			src := fileConn(t, e, "src.db")
			mustExec(t, src, "CREATE TABLE cats (id INT PRIMARY KEY, name TEXT)",
				"INSERT INTO cats VALUES (1, 'a'), (2, 'b'), (3, 'c')")
			for _, c := range []struct {
				name string
				opt  CopyOptions
				want int64
			}{
				{"table", CopyOptions{}, 3},
				{"where", CopyOptions{Where: "id > 1"}, 2},
				{"columns", CopyOptions{Columns: []string{"name"}}, 3},
				{"query", CopyOptions{Query: "SELECT name FROM cats WHERE id < 3", To: "x"}, 2},
			} {
				table := "cats"
				if c.opt.Query != "" {
					table = ""
				}
				if n, err := CountCopy(ctx, src, table, c.opt); err != nil || n != c.want {
					t.Errorf("%s: %d, %v; want %d", c.name, n, err, c.want)
				}
			}
			if e == Bytdb {
				return // bytdb has no RETURNING to move rows with
			}
			n, err := CountCopy(ctx, src, "", CopyOptions{Query: "DELETE FROM cats RETURNING *", To: "x"})
			if err == nil || n != -1 || !strings.Contains(err.Error(), "would run it") {
				t.Errorf("a write: %d, %v", n, err)
			}
			if got := dump(t, src, "SELECT count(*) FROM cats")[0][0]; got != "3" {
				t.Errorf("counting the write ran it: %s rows left", got)
			}
		})
	}
}

// TestCopyStatsRate: the summary carries the rate from a tenth of a second
// on, and leaves it out below, where it is the clock's grain.
func TestCopyStatsRate(t *testing.T) {
	st := CopyStats{From: "a:t", To: "b:t", Rows: 500000, Duration: 2 * time.Second, Direct: true}
	if got, want := st.String(), "copied 500000 rows a:t → b:t in 2s at 250000 rows/s (direct COPY)"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	st = CopyStats{From: "a:t", To: "b:t", Rows: 8, Duration: 4 * time.Millisecond, Skipped: 2}
	if got, want := st.String(), "copied 8 rows a:t → b:t in 4ms (row by row), 2 skipped"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	for _, c := range []struct {
		rows int64
		took time.Duration
		want string
	}{
		{0, time.Second, ""},
		{10, 99 * time.Millisecond, ""},
		{10, 100 * time.Millisecond, "100 rows/s"},
		{3, 2 * time.Second, "2 rows/s"},
	} {
		if got := RateText(c.rows, c.took); got != c.want {
			t.Errorf("RateText(%d, %s) = %q, want %q", c.rows, c.took, got, c.want)
		}
	}
}
