package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// One table whose count(*) outlasts the one counting statement's share
// of the budget no longer leaves every table without a number (N-283):
// the counting falls back to table by table, where only that table runs
// out its time. The slow table is one another session holds an ACCESS
// EXCLUSIVE lock on, so its count(*) waits. Opt-in: DBC_LIVE_PG_DSN.
func TestLiveRowCountsOneSlowTable(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
	liveExec(t, mgr,
		`DROP SCHEMA IF EXISTS dbc_live_rc CASCADE`,
		`CREATE SCHEMA dbc_live_rc`,
		`CREATE TABLE dbc_live_rc.slow (id int)`,
		`CREATE TABLE dbc_live_rc.quick (id int)`,
		`INSERT INTO dbc_live_rc.quick SELECT generate_series(1, 42)`,
	)
	t.Cleanup(func() { liveExec(t, mgr, `DROP SCHEMA IF EXISTS dbc_live_rc CASCADE`) })

	// shorter clocks for the test: the statement's share and one count's
	oldShare, oldTimeout := pgStatementShare, countTimeout
	pgStatementShare, countTimeout = 500*time.Millisecond, time.Second
	t.Cleanup(func() { pgStatementShare, countTimeout = oldShare, oldTimeout })

	// another session holds the slow table
	locker, err := sql.Open("pgx", liveDSN(t, "DBC_LIVE_PG_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	ctx := context.Background()
	conn, err := locker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, s := range []string{`BEGIN`, `LOCK TABLE dbc_live_rc.slow IN ACCESS EXCLUSIVE MODE`} {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	defer conn.ExecContext(ctx, `ROLLBACK`) //nolint:errcheck

	tables := []TableRef{{Schema: "dbc_live_rc", Name: "slow"}, {Schema: "dbc_live_rc", Name: "quick"}}
	start := time.Now()
	counts, err := mgr.RowCounts(ctx, "live", tables)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := counts[tables[1]]; !ok || c.N != 42 {
		t.Errorf("the quick table: %v (%v), want 42 — one slow table blanked the rest", c, ok)
	}
	if _, ok := counts[tables[0]]; ok {
		t.Errorf("the locked table has a count: %v", counts[tables[0]])
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %s: the fallback should be bounded by the share and one count's timeout", took)
	}
}
