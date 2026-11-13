package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strconv"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/sqlsplit"
)

// A statement's kind is read off its verb where the verb says, and left to
// its result (kindMaybe) where it does not — on Postgres and MySQL only;
// SQLite and bytdb keep to Query and Exec.
func TestKindOf(t *testing.T) {
	cases := []struct {
		stmt      string
		pg, mysql stmtKind
	}{
		// the verb says: rows
		{"SELECT 1", kindRows, kindRows},
		{"SHOW TABLES", kindRows, kindRows},
		{"EXPLAIN ANALYZE SELECT 1", kindRows, kindRows},
		{"INSERT INTO t VALUES (1) RETURNING id", kindRows, kindRows},
		{"WITH x AS (SELECT 1) SELECT * FROM x", kindRows, kindRows},
		// the verb says: no rows
		{"INSERT INTO t VALUES (1)", kindExec, kindExec},
		{"WITH x AS (SELECT 1) DELETE FROM t", kindExec, kindExec},
		{"CREATE TABLE t (id int)", kindExec, kindExec},
		{"BEGIN", kindExec, kindExec},
		{"SET search_path = a", kindExec, kindExec},
		{"DO $$ BEGIN PERFORM 1; END $$", kindExec, kindExec},
		{"-- a note\nCOMMIT", kindExec, kindExec},
		// the verb does not say
		{"CALL monthly_report()", kindMaybe, kindMaybe},
		{"call p(1)", kindMaybe, kindMaybe},
		{"EXECUTE q", kindMaybe, kindMaybe},
		{"FETCH 10 FROM cur", kindMaybe, kindMaybe},
		{"CHECK TABLE t", kindMaybe, kindMaybe},
		{"ANALYZE TABLE t", kindMaybe, kindMaybe},
		{"OPTIMIZE TABLE t", kindMaybe, kindMaybe},
		{"HANDLER t READ FIRST", kindMaybe, kindMaybe},
		// a parenthesized SELECT is read through its paren (sqlsplit.Verbs)
		{"(SELECT 1) UNION (SELECT 2)", kindRows, kindRows},
		// several statements in one text keep the simple protocol's Exec
		{"CALL a(); CALL b()", kindExec, kindExec},
		// … but a semicolon in a string is not a second statement
		{"CALL a('x; y')", kindMaybe, kindMaybe},
	}
	for _, c := range cases {
		if got := kindOf(c.stmt, sqlsplit.Postgres, "pgx"); got != c.pg {
			t.Errorf("kindOf(%q) on Postgres = %d, want %d", c.stmt, got, c.pg)
		}
		if got := kindOf(c.stmt, sqlsplit.MySQL, "mysql"); got != c.mysql {
			t.Errorf("kindOf(%q) on MySQL = %d, want %d", c.stmt, got, c.mysql)
		}
		// the engines with no kindMaybe: what is not a Query is an Exec
		want := kindExec
		if c.pg == kindRows {
			want = kindRows
		}
		for _, drv := range []string{"sqlite", "bytdb", ""} {
			if got := kindOf(c.stmt, sqlsplit.DialectOf(drv), drv); got != want {
				t.Errorf("kindOf(%q) on %q = %d, want %d", c.stmt, drv, got, want)
			}
		}
	}
}

// setsDriver is a driver whose every query returns as many result sets as
// its DSN says, to exercise readSets without a MySQL server: set k (from 1)
// has the one column "c<k>" and the two rows k*10 and k*10+1.
type setsDriver struct{}

func init() { sql.Register("dbc-sets", setsDriver{}) }

func (setsDriver) Open(dsn string) (driver.Conn, error) {
	n, err := strconv.Atoi(dsn)
	return &setsConn{sets: n}, err
}

type setsConn struct{ sets int }

func (c *setsConn) Prepare(string) (driver.Stmt, error) { return &setsStmt{c}, nil }
func (c *setsConn) Close() error                        { return nil }
func (c *setsConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

type setsStmt struct{ c *setsConn }

func (s *setsStmt) Close() error                               { return nil }
func (s *setsStmt) NumInput() int                              { return -1 }
func (s *setsStmt) Exec([]driver.Value) (driver.Result, error) { return nil, driver.ErrSkip }
func (s *setsStmt) Query([]driver.Value) (driver.Rows, error) {
	return &setsRows{sets: s.c.sets, set: 1}, nil
}

// setsRows walks the sets; it is a driver.RowsNextResultSet.
type setsRows struct{ sets, set, row int }

func (r *setsRows) Columns() []string { return []string{fmt.Sprintf("c%d", r.set)} }
func (r *setsRows) Close() error      { return nil }
func (r *setsRows) Next(dest []driver.Value) error {
	if r.row >= 2 {
		return io.EOF
	}
	dest[0] = int64(r.set*10 + r.row)
	r.row++
	return nil
}
func (r *setsRows) HasNextResultSet() bool { return r.set < r.sets }
func (r *setsRows) NextResultSet() error {
	if !r.HasNextResultSet() {
		return io.EOF
	}
	r.set, r.row = r.set+1, 0
	return nil
}

// readSetsOf runs a query on a setsDriver database of n sets through
// readSets, with max_rows as given.
func readSetsOf(t *testing.T, n, maxRows int) []string {
	t.Helper()
	dbh, err := sql.Open("dbc-sets", strconv.Itoa(n))
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()
	rows, err := dbh.Query("CALL p()")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	m := NewManager(&config.Config{MaxRows: maxRows})
	res, err := m.readSets(context.Background(), rows, "c", "CALL p()", nil)
	if err != nil {
		t.Fatalf("readSets: %v", err)
	}
	// one line per set: its column, its rows, and whether it was cut
	var got []string
	for _, set := range res.Sets() {
		if set.Conn != "c" || set.Query != "CALL p()" {
			t.Errorf("set carries conn %q, query %q", set.Conn, set.Query)
		}
		got = append(got, fmt.Sprint(set.Columns, set.Rows, set.Truncated))
	}
	if res.MoreCut {
		got = append(got, "cut")
	}
	return got
}

// Every result set a statement returns is read: the first into the Result,
// the rest into More, each cut at max_rows on its own. Past maxMoreSets
// the rest are dropped and the Result says so.
func TestReadSets(t *testing.T) {
	if got := fmt.Sprint(readSetsOf(t, 1, 100)); got != "[[c1] [[10] [11]] false]" {
		t.Errorf("one set: %s", got)
	}
	if got := fmt.Sprint(readSetsOf(t, 3, 100)); got != "[[c1] [[10] [11]] false [c2] [[20] [21]] false [c3] [[30] [31]] false]" {
		t.Errorf("three sets: %s", got)
	}
	// the cap is each set's own, and a cut set does not end the reading
	if got := fmt.Sprint(readSetsOf(t, 2, 1)); got != "[[c1] [[10]] true [c2] [[20]] true]" {
		t.Errorf("two sets at max_rows 1: %s", got)
	}
	// exactly the cap is not a cut
	got := readSetsOf(t, 1+maxMoreSets, 100)
	if len(got) != 1+maxMoreSets {
		t.Errorf("%d sets: kept %d lines, want every set and no cut", 1+maxMoreSets, len(got))
	}
	got = readSetsOf(t, 5+maxMoreSets, 100)
	if len(got) != 2+maxMoreSets || got[len(got)-1] != "cut" {
		t.Errorf("%d sets: kept %d lines ending %q, want the first %d sets and the cut", 5+maxMoreSets, len(got), got[len(got)-1], 1+maxMoreSets)
	}
}
