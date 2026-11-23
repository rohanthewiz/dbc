package db

import (
	"testing"

	"github.com/rohanthewiz/dbc/sqlsplit"
)

// locksRows finds a row-locking clause at any depth and a transaction-level
// advisory lock call, and nothing in strings, comments or names; everything
// it finds keeps a cut read inside a transaction reading to its end.
func TestLocksRows(t *testing.T) {
	cases := []struct {
		stmt string
		want bool
	}{
		{"SELECT * FROM t FOR UPDATE", true},
		{"select * from t for no key update skip locked", true},
		{"SELECT * FROM t FOR SHARE OF t NOWAIT", true},
		{"SELECT * FROM t FOR KEY SHARE", true},
		{"SELECT * FROM (SELECT * FROM t FOR UPDATE) s", true},
		{"SELECT id FROM jobs WHERE pg_try_advisory_xact_lock(id)", true},
		{"SELECT pg_catalog.pg_advisory_xact_lock_shared(1) FROM t", true},
		// not locking
		{"SELECT * FROM t", false},
		{"SELECT substring(s FROM 1 FOR 3) FROM t", false},
		{"SELECT 'for update' FROM t", false},
		{"SELECT 1 -- for update", false},
		{`SELECT "for", update_at FROM t`, false},
		{"SELECT pg_advisory_unlock_all()", false},
	}
	for _, c := range cases {
		if got := locksRows(c.stmt, sqlsplit.Postgres.Words(c.stmt)); got != c.want {
			t.Errorf("locksRows(%q) = %v, want %v", c.stmt, got, c.want)
		}
	}
}

// readBound reads a LIMIT or FETCH FIRST at the statement's own level only,
// and gives no bound it cannot read as a plain number.
func TestReadBound(t *testing.T) {
	cases := []struct {
		stmt string
		n    int64
		ok   bool
	}{
		{"SELECT * FROM t LIMIT 10", 10, true},
		{"select * from t order by a limit 500 offset 9000", 500, true},
		{"SELECT * FROM t OFFSET 5 FETCH FIRST 20 ROWS ONLY", 20, true},
		{"SELECT * FROM t FETCH NEXT 1 ROW ONLY", 1, true},
		{"SELECT * FROM t FETCH FIRST ROW ONLY", 1, true},
		{"WITH x AS (SELECT * FROM t) SELECT * FROM x LIMIT 7", 7, true},
		{"(SELECT 1) UNION ALL (SELECT 2) LIMIT 3", 3, true},
		// no bound at the statement's own level
		{"SELECT * FROM t", 0, false},
		{"SELECT * FROM (SELECT * FROM t LIMIT 5) s", 0, false},
		{"WITH x AS (SELECT * FROM t LIMIT 5) SELECT * FROM x", 0, false},
		{"SELECT * FROM t LIMIT ALL", 0, false},
		{"SELECT * FROM t LIMIT $1", 0, false},
		{"SELECT * FROM t LIMIT (10)", 0, false},
		{"SELECT 'limit 5' FROM t", 0, false},
	}
	for _, c := range cases {
		n, ok := readBound(sqlsplit.Postgres.Words(c.stmt))
		if n != c.n || ok != c.ok {
			t.Errorf("readBound(%q) = %d, %v; want %d, %v", c.stmt, n, ok, c.n, c.ok)
		}
	}
}
