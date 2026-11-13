package db

import (
	"context"
	"testing"

	"github.com/rohanthewiz/dbc/sqlsplit"
)

// effectOf finds what a read's verb hides (N-272): a setting, a lock or a
// variable a SELECT leaves on the session, a table SELECT … INTO makes, a
// PRAGMA setter, and the statement an EXPLAIN ANALYZE runs — and nothing
// in a read that only mentions such things.
func TestEffectOf(t *testing.T) {
	pg, my, lite := sqlsplit.Postgres, sqlsplit.MySQL, sqlsplit.SQLite
	cases := []struct {
		d    sqlsplit.Dialect
		stmt string
		want effect
	}{
		// plain reads
		{pg, "SELECT 1", effectNone},
		{pg, "-- note\nSELECT * FROM t WHERE id IN (1, 2)", effectNone},
		{pg, "SELECT current_setting('search_path')", effectNone},
		{pg, "SELECT nextval('s')", effectNone}, // left out on purpose: see sessionFuncs
		{pg, "SELECT 'set_config(x)' AS s", effectNone},
		{pg, "SELECT f(a := 1)", effectNone}, // a named argument on Postgres
		{pg, "SHOW search_path", effectNone},
		{pg, "VALUES (1), (2)", effectNone},
		{pg, "EXPLAIN SELECT pg_advisory_lock(1)", effectNone}, // planned, not run
		{pg, "EXPLAIN DELETE FROM t", effectNone},
		{my, "SELECT @v", effectNone},
		{my, "SELECT RELEASE_LOCK('job')", effectNone},
		{my, "DESCRIBE INSERT INTO t VALUES (1)", effectNone},
		{lite, "PRAGMA foreign_keys", effectNone},
		{lite, "PRAGMA table_info(t)", effectNone},
		{lite, "PRAGMA main.index_list('t')", effectNone},
		{lite, "PRAGMA integrity_check(10)", effectNone},

		// reads that leave state on the session
		{pg, "SELECT set_config('search_path', 'x', false)", effectSession},
		{pg, "select PG_CATALOG.SET_CONFIG('a.b', '1', false)", effectSession},
		{pg, "SELECT pg_advisory_lock(42)", effectSession},
		{pg, "SELECT pg_try_advisory_lock_shared(1, 2)", effectSession},
		{pg, "SELECT * FROM jobs WHERE pg_try_advisory_lock(id) LIMIT 1", effectSession},
		{pg, "WITH l AS (SELECT pg_advisory_lock(1)) SELECT * FROM l", effectSession},
		{pg, "SELECT setseed(0.5)", effectSession},
		{pg, "SELECT dblink_connect('c', 'dbname=x')", effectSession},
		{my, "SELECT GET_LOCK('job', 0)", effectSession},
		{my, "SELECT @v := 1", effectSession},
		{my, "SELECT id, @n := @n + 1 FROM t", effectSession},
		{lite, "PRAGMA foreign_keys = ON", effectSession},
		{lite, "PRAGMA main.cache_size=-2000", effectSession},
		{lite, "PRAGMA foreign_keys(1)", effectSession},
		{pg, "EXPLAIN ANALYZE SELECT set_config('a.b', '1', false)", effectSession},

		// reads that may do anything
		{pg, "SELECT * INTO t2 FROM t", effectAny},
		{pg, "SELECT 1 AS n INTO TEMP t2", effectAny},
		{pg, "WITH x AS (SELECT 1) SELECT * INTO t2 FROM x", effectAny},
		{my, "SELECT id INTO @v FROM t LIMIT 1", effectAny},
		{my, "SELECT * FROM t INTO OUTFILE '/tmp/t'", effectAny},
		{pg, "EXPLAIN ANALYZE DELETE FROM t", effectAny},
		{pg, "EXPLAIN (ANALYZE, BUFFERS) UPDATE t SET a = 1", effectAny},
		{my, "# note\nEXPLAIN ANALYZE DELETE t FROM t JOIN u USING (id)", effectAny},
		{pg, "WITH d AS (DELETE FROM t RETURNING *) SELECT count(*) FROM d", effectAny},
		{pg, "UPDATE t SET a = 1", effectAny},
		{pg, "SET search_path = x", effectAny},
		{pg, "BEGIN", effectAny},
	}
	for _, c := range cases {
		if got := effectOf(c.stmt, c.d); got != c.want {
			t.Errorf("%s: effectOf(%q) = %d, want %d", c.d, c.stmt, got, c.want)
		}
	}

	// := is MySQL's assignment only: the other readings leave it alone
	if got := effectOf("SELECT @v := 1", sqlsplit.Generic); got != effectNone {
		t.Errorf("Generic: effectOf(SELECT @v := 1) = %d, want none", got)
	}
}

// A PRAGMA that sets something makes the session stateful; one that reads,
// with or without an argument, does not.
func TestSessionStatefulPragma(t *testing.T) {
	mgr := fileSQLiteMgr(t)
	ctx := context.Background()
	if _, err := mgr.Run("f", "CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	sess, err := mgr.Session(ctx, "f")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()

	for _, q := range []string{"PRAGMA foreign_keys", "PRAGMA table_info(t)"} {
		if _, err = sess.Run(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if sess.Stateful() {
			t.Fatalf("stateful after %s", q)
		}
	}
	if _, err = sess.Run(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("pragma set: %v", err)
	}
	if !sess.Stateful() {
		t.Error("not stateful after PRAGMA foreign_keys = ON")
	}
}
