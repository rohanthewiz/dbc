package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/stmtcut"
)

// Live tests for N-276: a plain read cut at max_rows is stopped on the
// server instead of streamed to its end by rows.Close. Opt-in like the
// other live tests (live_test.go).
//
// Each big read streams ~10M rows of ~200 bytes, which took 6–11 s to drain
// on a local Postgres 17 / MySQL 8.4 before the fix (a 3M-row one measured
// 1.85 s and 3.4 s); a cut one returns in a fraction of a second. The
// bound the tests hold is cutBudget, far from both.

const cutBudget = 2 * time.Second

// cutEngine is one engine's statements for the cut tests.
type cutEngine struct {
	env, driver string
	// big is a plain read of ~10M rows, slow to stream whole; medium one of
	// ~300k, quick to drain (the case where a stop must not happen). Both
	// carry the literal dbc_cut_marker, which running looks for.
	big, medium string
	// running counts the other sessions still executing a marked read.
	running string
	// set leaves a session setting that get reads back as "kept".
	set, get string
	// id is the session's server id.
	id string
	// table makes dbc_cut_t (one int column n).
	table, drop string
}

func cutEngines() []cutEngine {
	digits := "(SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 " +
		"UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9)"
	myRows := func(tables int) string {
		q := "SELECT 'dbc_cut_marker' AS m, a0.n, REPEAT('x', 200) AS pad FROM " + digits + " a0"
		for i := 1; i < tables; i++ {
			q += " CROSS JOIN " + digits + " a" + string(rune('0'+i))
		}
		return q
	}
	return []cutEngine{
		{
			env: "DBC_LIVE_PG_DSN", driver: "postgres",
			// generate_series in the select list streams (ProjectSet); in
			// FROM it would be materialized before the first row went out.
			big:     "SELECT 'dbc_cut_marker' AS m, generate_series(1, 10000000) AS g, repeat('x', 200) AS pad",
			medium:  "SELECT 'dbc_cut_marker' AS m, generate_series(1, 300000) AS g, repeat('x', 200) AS pad",
			running: "SELECT count(*) FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND state = 'active' AND query LIKE '%dbc_cut_marker%'",
			set:     "SET application_name = 'kept'",
			get:     "SHOW application_name",
			id:      "SELECT pg_backend_pid()",
			table:   "CREATE TABLE dbc_cut_t (n int)",
			drop:    "DROP TABLE IF EXISTS dbc_cut_t",
		},
		{
			env: "DBC_LIVE_MYSQL_DSN", driver: "mysql",
			big:     myRows(7), // 10^7 rows
			medium:  myRows(5) + " CROSS JOIN (SELECT 0 UNION ALL SELECT 1 UNION ALL SELECT 2) b",
			running: "SELECT COUNT(*) FROM information_schema.processlist WHERE id <> CONNECTION_ID() AND command = 'Query' AND info LIKE '%dbc_cut_marker%'",
			set:     "SET @dbc_cut = 'kept'",
			get:     "SELECT @dbc_cut",
			id:      "SELECT CONNECTION_ID()",
			table:   "CREATE TABLE dbc_cut_t (n int) ENGINE=InnoDB",
			drop:    "DROP TABLE IF EXISTS dbc_cut_t",
		},
	}
}

// one runs q on the session and returns its single cell.
func one(t *testing.T, s *Session, q string) string {
	t.Helper()
	res, err := s.Run(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		t.Fatalf("%s: want one cell, got %v", q, res.Rows)
	}
	return res.Rows[0][0]
}

// cutRun runs q through run (pooled when s is nil) and checks it came back
// cut at max_rows within cutBudget.
func cutRun(t *testing.T, mgr *Manager, s *Session, q string) {
	t.Helper()
	start := time.Now()
	var err error
	var rows int
	var truncated bool
	if s == nil {
		res, rerr := mgr.Run("live", q)
		err = rerr
		if res != nil {
			rows, truncated = len(res.Rows), res.Truncated
		}
	} else {
		res, rerr := s.Run(context.Background(), q)
		err = rerr
		if res != nil {
			rows, truncated = len(res.Rows), res.Truncated
		}
	}
	took := time.Since(start)
	if err != nil {
		t.Fatalf("cut read: %v", err)
	}
	if rows != 1000 || !truncated {
		t.Fatalf("cut read: %d rows, truncated=%v; want 1000, true", rows, truncated)
	}
	if took > cutBudget {
		t.Fatalf("cut read took %s: the rest of the result was streamed, not stopped", took)
	}
	t.Logf("cut read returned in %s", took)
}

// TestLiveCut: a pooled run and a pinned session's run of a big plain read
// both come back at once, nothing is left running on the server, and the
// session keeps its connection and its state.
func TestLiveCut(t *testing.T) {
	for _, e := range cutEngines() {
		t.Run(e.driver, func(t *testing.T) {
			mgr := liveMgr(t, e.env, e.driver)
			ctx := context.Background()

			t.Run("pool", func(t *testing.T) {
				cutRun(t, mgr, nil, e.big)
				s, err := mgr.Session(ctx, "live")
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				// The stop has landed by the time the run returns (a KILL
				// is waited for; a cancel's error is read off the wire).
				if n := one(t, s, e.running); n != "0" {
					t.Errorf("%s sessions still running the cut read", n)
				}
			})

			t.Run("session keeps its state", func(t *testing.T) {
				s, err := mgr.Session(ctx, "live")
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if _, err = s.Run(ctx, e.set); err != nil {
					t.Fatal(err)
				}
				id := one(t, s, e.id)
				cutRun(t, mgr, s, e.big)
				if got := one(t, s, e.id); got != id {
					t.Errorf("server id %s after the cut, was %s: the session's connection was replaced", got, id)
				}
				if got := one(t, s, e.get); got != "kept" {
					t.Errorf("setting after the cut = %q, want kept", got)
				}
				if s.Classify(nil) != FaultNone {
					t.Error("session classified as faulty")
				}
			})
		})
	}
}

// TestLiveCutInTransaction: a cut inside the user's open transaction leaves
// it open with its work. On Postgres a stop is a cancel, which would abort
// the transaction, so the read is not stopped there at all — Grace 0 makes
// a wrong stop certain to happen if it were allowed. On MySQL the stop is a
// KILL QUERY, which ends the statement only.
func TestLiveCutInTransaction(t *testing.T) {
	old := stmtcut.Grace
	stmtcut.Grace = 0
	t.Cleanup(func() { stmtcut.Grace = old })
	for _, e := range cutEngines() {
		t.Run(e.driver, func(t *testing.T) {
			mgr := liveMgr(t, e.env, e.driver)
			ctx := context.Background()
			liveExec(t, mgr, e.drop, e.table)
			t.Cleanup(func() { _, _ = mgr.Run("live", e.drop) })

			s, err := mgr.Session(ctx, "live")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for _, q := range []string{"BEGIN", "INSERT INTO dbc_cut_t VALUES (1)"} {
				if _, err = s.Run(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			q := e.medium
			if e.driver == "mysql" {
				q = e.big // stopped, so it may be big
			}
			cutRun(t, mgr, s, q)
			// 25P02 here on Postgres if the cut had aborted the transaction
			if got := one(t, s, "SELECT count(*) FROM dbc_cut_t"); got != "1" {
				t.Fatalf("rows in the transaction after the cut = %s, want 1", got)
			}
			if _, err = s.Run(ctx, "ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			// the transaction was still the one that held the insert
			if got := one(t, s, "SELECT count(*) FROM dbc_cut_t"); got != "0" {
				t.Fatalf("rows after ROLLBACK = %s, want 0", got)
			}
		})
	}
}

// TestLiveCutOnlyPlainReads: only a plain read (isRead) is stopped once cut
// at max_rows. A read that leaves state behind row by row is read to its
// end, so the state is what the whole statement leaves — Grace 0 makes a
// wrong stop certain:
//
//	Postgres  set_config(…, false) per row: the setting ends at the last
//	          row's value; a cancel would abort the implicit transaction
//	          and revert it
//	MySQL     @n := @n + 1 per row: @n ends at the row count; a KILL QUERY
//	          would leave it part way
//
// And a DELETE … RETURNING cut at max_rows deletes every row it matched.
// (Postgres would not stop that one even if asked: it runs the delete to
// the end before sending a row, and does not act on a cancel while it
// sends them — so the check stands for the rule, not as its proof.)
func TestLiveCutOnlyPlainReads(t *testing.T) {
	old := stmtcut.Grace
	stmtcut.Grace = 0
	t.Cleanup(func() { stmtcut.Grace = old })
	ctx := context.Background()

	t.Run("postgres", func(t *testing.T) {
		mgr := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
		s, err := mgr.Session(ctx, "live")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		cutRun(t, mgr, s, "SELECT set_config('dbc.cut', g::text, false), repeat('x', 200) FROM generate_series(1, 300000) g")
		if got := one(t, s, "SELECT current_setting('dbc.cut')"); got != "300000" {
			t.Errorf("setting after the cut read = %s, want 300000: the read was stopped part way", got)
		}

		liveExec(t, mgr, "DROP TABLE IF EXISTS dbc_cut_w",
			"CREATE TABLE dbc_cut_w AS SELECT g AS n, repeat('x', 200) AS pad FROM generate_series(1, 200000) g")
		t.Cleanup(func() { _, _ = mgr.Run("live", "DROP TABLE IF EXISTS dbc_cut_w") })
		cutRun(t, mgr, nil, "DELETE FROM dbc_cut_w RETURNING *")
		if got := one(t, s, "SELECT count(*) FROM dbc_cut_w"); got != "0" {
			t.Fatalf("%s rows left: the DELETE was stopped part way", got)
		}
	})

	t.Run("mysql", func(t *testing.T) {
		mgr := liveMgr(t, "DBC_LIVE_MYSQL_DSN", "mysql")
		s, err := mgr.Session(ctx, "live")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err = s.Run(ctx, "SET @dbc_n = 0"); err != nil {
			t.Fatal(err)
		}
		var medium string
		for _, e := range cutEngines() {
			if e.driver == "mysql" {
				medium = e.medium
			}
		}
		// the medium read with a counter in front: 300k rows
		cutRun(t, mgr, s, strings.Replace(medium, "SELECT ", "SELECT @dbc_n := @dbc_n + 1 AS c, ", 1))
		if got := one(t, s, "SELECT @dbc_n"); got != "300000" {
			t.Errorf("@dbc_n after the cut read = %s, want 300000: the read was stopped part way", got)
		}
	})
}

// TestLiveCutMySQLLateKill: a KILL QUERY that reaches a connection after its
// statement ended does not stop the connection's next one. This is the
// race stmtcut.Point.Close leaves open by design (the timer can fire just
// as the rest arrives); MySQL 8 clears the kill when the next command is
// dispatched.
func TestLiveCutMySQLLateKill(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_MYSQL_DSN", "mysql")
	ctx := context.Background()
	s, err := mgr.Session(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Run(ctx, "SET @dbc_cut = 'kept'"); err != nil {
		t.Fatal(err)
	}
	var kc *mysqlKillConn
	_ = s.conn.Raw(func(dc any) error {
		kc, _ = dc.(*mysqlKillConn)
		return nil
	})
	if kc == nil || kc.id == "" {
		t.Fatal("the session's connection is not a mysqlKillConn with an id")
	}
	kc.k.killQuery(kc.id) // the connection is idle
	if got := one(t, s, "SELECT @dbc_cut"); got != "kept" {
		t.Fatalf("after a late KILL QUERY: %q", got)
	}
	if got := one(t, s, "SELECT 1"); got != "1" {
		t.Fatalf("second statement after a late KILL QUERY: %q", got)
	}
}

// TestLiveCutCatalogRead: limitedRows, the reader behind the catalog and
// schema reads, stops a query past its bound too.
func TestLiveCutCatalogRead(t *testing.T) {
	for _, e := range cutEngines() {
		t.Run(e.driver, func(t *testing.T) {
			mgr := liveMgr(t, e.env, e.driver)
			ctx := context.Background()
			dbh, err := mgr.DBContext(ctx, "live")
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			_, rows, truncated, err := limitedRows(ctx, dbh, e.big, 10)
			took := time.Since(start)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 10 || !truncated {
				t.Fatalf("%d rows, truncated=%v", len(rows), truncated)
			}
			if took > cutBudget {
				t.Fatalf("limitedRows took %s: the rest was streamed", took)
			}
			t.Logf("limitedRows returned in %s", took)
			assertNotRunning(t, dbh, e.running)
		})
	}
}

// assertNotRunning checks on a fresh pooled connection that no session is
// still executing a marked read.
func assertNotRunning(t *testing.T, dbh *sql.DB, q string) {
	t.Helper()
	var n string
	if err := dbh.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(n) != "0" {
		t.Errorf("%s sessions still running the cut read", n)
	}
}
