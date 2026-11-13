package etl

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// Live tests for N-276 on the Reader: one closed or aborted before its last
// row stops its query on the server, instead of reading every remaining row
// first — what a script that breaks out of its loop, a pipeline Preview
// (Abort) and rows.limit (Close) do. Opt-in: DBC_LIVE_PG_DSN and/or
// DBC_LIVE_MYSQL_DSN.
//
// The pools are dbc's own (db.Manager), as sdb.ETLConn hands them to every
// Reader in dbc: MySQL's stop is a KILL QUERY that only dbc's connector
// knows how to send.

// readerCutBudget is far below the 6–11 s the big reads take to stream
// whole on a local server, and far above a stopped one.
const readerCutBudget = 2 * time.Second

type readerCutEngine struct {
	env, driver string
	engine      Engine
	big         string // ~10M rows, slow to stream whole
}

func readerCutEngines() []readerCutEngine {
	digits := "(SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 " +
		"UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9)"
	my := "SELECT a0.n, REPEAT('x', 200) AS pad FROM " + digits + " a0"
	for i := 1; i < 7; i++ {
		my += " CROSS JOIN " + digits + " a" + string(rune('0'+i))
	}
	return []readerCutEngine{
		{"DBC_LIVE_PG_DSN", "postgres", Postgres,
			"SELECT generate_series(1, 10000000) AS g, repeat('x', 200) AS pad"},
		{"DBC_LIVE_MYSQL_DSN", "mysql", MySQL, my},
	}
}

// managerConn is an etl Conn on a db.Manager pool for env's DSN, or a skip.
func managerConn(t *testing.T, env, driver string, e Engine) Conn {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skipf("set %s to run against a live %s", env, driver)
	}
	mgr := db.NewManager(&config.Config{MaxRows: 1000,
		Connections: []config.Connection{{Name: "live", Driver: driver, DSN: dsn}}})
	t.Cleanup(mgr.Close)
	dbh, err := mgr.DBContext(context.Background(), "live")
	if err != nil {
		t.Fatal(err)
	}
	return Conn{Name: "live", DB: dbh, Engine: e}
}

// read50 opens a read of q and takes 50 rows of it.
func read50(t *testing.T, c Conn, q string, opt ReadOptions) *Reader {
	t.Helper()
	rd, err := ReadWith(context.Background(), c, q, opt)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		if !rd.Next() {
			t.Fatalf("row %d: %v", i+1, rd.Err())
		}
	}
	return rd
}

// timed runs f and fails when it takes longer than readerCutBudget.
func timed(t *testing.T, what string, f func()) {
	t.Helper()
	start := time.Now()
	f()
	if took := time.Since(start); took > readerCutBudget {
		t.Fatalf("%s took %s: the rest of the result was streamed, not stopped", what, took)
	} else {
		t.Logf("%s in %s", what, took)
	}
}

func TestLiveReaderCut(t *testing.T) {
	for _, e := range readerCutEngines() {
		t.Run(e.driver, func(t *testing.T) {
			c := managerConn(t, e.env, e.driver, e.engine)

			// a script's early break, rows.limit (a held read's Close)
			for _, hold := range []bool{false, true} {
				rd := read50(t, c, e.big, ReadOptions{Hold: hold})
				timed(t, "Close after 50 rows", func() {
					if err := rd.Close(); err != nil {
						t.Fatalf("Close: %v", err)
					}
				})
			}
			// a Preview's end, a failed load
			rd := read50(t, c, e.big, ReadOptions{Hold: true})
			timed(t, "Abort after 50 rows", rd.Abort)

			// the pool's connections came through it whole
			var one int
			if err := c.DB.QueryRow("SELECT 1").Scan(&one); err != nil || one != 1 {
				t.Fatalf("SELECT 1 after the stops: %d, %v", one, err)
			}
		})
	}
}

// A read that writes, closed early, keeps Close's promise: its work
// completes and commits. Postgres: a DELETE … RETURNING that moved 200k rows
// while the caller took 50 of them deleted all 200k, as before.
func TestLiveReaderCutKeepsAWrite(t *testing.T) {
	c := managerConn(t, "DBC_LIVE_PG_DSN", "postgres", Postgres)
	mustExec(t, c, "DROP TABLE IF EXISTS etl_cut_w",
		"CREATE TABLE etl_cut_w AS SELECT g AS n, repeat('x', 200) AS pad FROM generate_series(1, 200000) g")
	t.Cleanup(func() { _, _ = c.DB.Exec("DROP TABLE IF EXISTS etl_cut_w") })
	rd := read50(t, c, "DELETE FROM etl_cut_w RETURNING *", ReadOptions{})
	if err := rd.Close(); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := c.DB.QueryRow("SELECT count(*) FROM etl_cut_w").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d rows left: the early Close did not complete the delete", left)
	}
}

// A plain read stopped early leaves nothing behind on Postgres: its
// transaction is rolled back on a live connection, so the next statement
// on the pool gets that very connection, not inside an aborted transaction
// and without the read's pinned output settings (pgPinOutput sets
// extra_float_digits to 3 for the read's transaction only).
func TestLiveReaderCutLeavesNoTransaction(t *testing.T) {
	c := managerConn(t, "DBC_LIVE_PG_DSN", "postgres", Postgres)
	c.DB.SetMaxOpenConns(1) // the next statement gets the very connection
	var pid, def string
	if err := c.DB.QueryRow("SELECT pg_backend_pid()::text, current_setting('extra_float_digits')").Scan(&pid, &def); err != nil {
		t.Fatal(err)
	}
	if def == "3" {
		t.Skip("the server's extra_float_digits is already 3: the pin would not show")
	}
	rd := read50(t, c, readerCutEngines()[0].big, ReadOptions{})
	timed(t, "Close after 50 rows", func() {
		if err := rd.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	var pid2, digits string
	if err := c.DB.QueryRow("SELECT pg_backend_pid()::text, current_setting('extra_float_digits')").Scan(&pid2, &digits); err != nil {
		t.Fatalf("the next statement on the pool: %v", err)
	}
	if pid2 != pid {
		t.Errorf("backend %s after the stop, was %s: the connection was dropped", pid2, pid)
	}
	if digits != def {
		t.Errorf("extra_float_digits = %s after the read, want the session's %s", digits, def)
	}
	assertNoOpenTx(t, c)
}
