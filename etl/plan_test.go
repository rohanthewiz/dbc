package etl

import (
	"context"
	"strings"
	"testing"
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
